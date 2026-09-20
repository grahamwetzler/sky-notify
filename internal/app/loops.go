package app

import (
	"context"
	"log/slog"
	"time"
)

func seedDB(ctx context.Context, db *DB) error {
	deadline := time.Now().Add(coldStartLimit)
	backoff := 2 * time.Second
	for {
		err := db.Refresh(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return err
		}
		slog.Warn("cold start: db fetch failed, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// poller is everything one poll needs. pollLoop holds all of it for its lifetime, so
// it is a receiver rather than eight arguments repeated at the call.
type poller struct {
	src   *Source
	db    *DB
	state *State
	q     *queue
	h     *server
	tr    *Tracker
	// holds are the alerts waiting for their aircraft to pass, keyed as the cooldown
	// ledger keys them. Bounded by aircraft overhead × closest-pass rules, and nothing
	// stays past delivery or holdGiveUp.
	// ponytail: memory only, like the Tracker — a restart forgets what was in flight and
	// picks the aircraft still overhead up on the next poll.
	holds map[string]held
}

// holdGiveUp bounds a parked alert's life. Two attempts past the 30 minutes
// notifyLoop's per-key backoff climbs to: an ntfy server down for a day must not leave
// every aircraft that passed in that time parked and still being offered.
const holdGiveUp = time.Hour

// held is one alert parked at the nearest position seen for it so far, for a rule that
// notifies at the closest pass.
//
// judged is the last poll that could tell whether the aircraft had passed — present in
// the feed, and with the ground track receding needs. Not "last seen": an aircraft that
// keeps matching while broadcasting no track would refresh a last-seen stamp forever and
// never reach the fallback that exists for exactly that case. It is stamped when the
// entry is created, so one that has never once been judgeable waits out the same grace
// window rather than tripping a zero timestamp on its first sweep.
//
// fired is when the alert was first offered to the queue, zero until then. The entry
// outlives the offer, because an offer is not a delivery — the alert's own ack is what
// says it was delivered.
type held struct {
	alert  *Alert
	judged time.Time
	fired  time.Time
}

func pollLoop(ctx context.Context, live *Live, src *Source, db *DB, state *State, q *queue, h *server) {
	interval := live.Get().Source.PollInterval.Std()
	t := time.NewTicker(interval)
	defer t.Stop()
	p := &poller{src: src, db: db, state: state, q: q, h: h, tr: NewTracker(), holds: map[string]held{}}
	for {
		cfg := live.Get()
		p.poll(ctx, cfg)
		if next := cfg.Source.PollInterval.Std(); next != interval {
			interval = next
			t.Reset(interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *poller) poll(ctx context.Context, cfg *Alerts) {
	f, err := p.src.Fetch(ctx, time.Now())
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("poll failed", "err", err)
		}
		p.h.setPollErr(err)
		// A dead feed is when a parked alert most needs the sweep, not least: nothing
		// can refresh judged while it is down, so waiting for a poll that may never
		// come would hold every alert until it returns — and could abandon a fired one
		// at holdGiveUp without ever retrying it, since a failed publish leaves the
		// queue empty and only the sweep re-offers.
		p.sweepHolds(cfg, time.Now())
		return
	}
	p.tr.Update(f, windowFor(cfg.Rules))
	p.h.setPollOK(f, p.tr)

	now := time.Now()
	cooldown := cfg.Cooldown.Std()
	for _, ac := range f.Aircraft {
		a := Evaluate(ac, p.db, cfg, p.tr.get(normalizeHex(ac.Hex)))
		if a == nil {
			continue
		}
		key := cooldownKey(a.Hex, a.Trigger)
		if !p.state.Eligible(key, cooldown) {
			delete(p.holds, key)
			continue
		}
		if a.AtClosest {
			p.park(key, ac, a, now)
			continue
		}
		p.q.add(a)
	}
	// The one place a parked alert reaches the queue, so there is a single place where
	// it can be offered, retried or dropped.
	p.sweepHolds(cfg, now)
}

// park keeps the nearest sighting of an aircraft a closest-pass rule has claimed, and
// decides whether it has passed. It queues nothing: waiting records no cooldown, so
// nothing is consumed by it and the next poll re-evaluates from scratch.
func (p *poller) park(key string, ac Aircraft, a *Alert, now time.Time) {
	h, ok := p.holds[key]
	switch {
	case !ok:
		h = held{alert: a, judged: now}
	case !h.fired.IsZero():
		// Frozen. A retry must resend what was decided, not a moving target — and the
		// delivery ack rides on the alert pointer, so swapping in a nearer snapshot
		// after firing would hand the sweep an unacknowledged alert and send the pass
		// twice. Nothing is lost: the alert has gone out, and an aircraft still closing
		// cannot un-send it.
	case a.recvLat != h.alert.recvLat || a.recvLon != h.alert.recvLon:
		// The receiver has moved since this was parked. The two distances were measured
		// from different places and comparing them would keep whichever number happened
		// to be smaller, so there is nothing here to keep: start again from this sighting.
		h = held{alert: a, judged: now}
	case a.DistanceNM < h.alert.DistanceNM:
		h.alert = a
	}
	// Judgeable at all: a moving aircraft broadcasting no ground track cannot be told
	// to have passed, which is the case the grace window in sweepHolds exists for. A
	// stationary one is judgeable, and receding answers true for it.
	if ac.GS == 0 || ac.Track != nil {
		h.judged = now
		var heading float64
		if ac.Track != nil {
			heading = *ac.Track
		}
		if h.fired.IsZero() && receding(a.recvLat, a.recvLon, *ac.Lat, *ac.Lon, ac.GS, heading) {
			h.fired = now
		}
	}
	p.holds[key] = h
}

// sweepHolds offers, re-offers and retires the parked alerts. It reads the config it is
// given rather than the one the alert was built under, because alerts.yaml is re-read
// every five seconds and a hold outlives several of those.
func (p *poller) sweepHolds(cfg *Alerts, now time.Time) {
	grace := 3 * cfg.Source.PollInterval.Std()
	for key, h := range p.holds {
		switch {
		// Nothing is holding this any more: the rule is gone, muted, shadowed by an
		// exception written ahead of it, or no longer waits for the pass. Evaluate stops
		// matching the first three the moment the file is reloaded, which would leave
		// the alert parked with nothing to announce it for — and an alert for a rule the
		// operator has just excepted is exactly what a mute is written to prevent. The
		// alert carries the sighting it was built from, so a hold that is retrying is not
		// retired because the aircraft has since flown out of the rule that caught it.
		case claimedBy(cfg, h.alert) == nil:
			delete(p.holds, key)
		// Delivered, said by the alert itself. Not "no longer eligible": a cooldown
		// shorter than the poll interval is legal, and the ledger has pruned its own
		// entry by the time the next sweep looks, so it would re-offer a delivered
		// alert every poll until holdGiveUp.
		case h.alert.delivered.Load():
			delete(p.holds, key)
		case !h.fired.IsZero() && now.Sub(h.fired) >= holdGiveUp:
			delete(p.holds, key)
		case h.fired.IsZero() && now.Sub(h.judged) >= grace:
			// The fallback for the two aircraft receding can never answer for: one that
			// left the feed mid-approach, and one that stays in it broadcasting a
			// position but no ground track. Three polls of grace rather than "missing
			// from this poll", because a feed drops an aircraft and brings it back, and
			// firing on a flicker would report a pass at 40 NM that was going to be 2.
			h.fired = now
			p.holds[key] = h
			p.q.add(h.alert)
		case !h.fired.IsZero():
			// Offered every poll until the ack, not enqueued once and forgotten:
			// notifyLoop drops a queue entry whose publish failed without advancing the
			// cooldown, and q.add refuses outright at maxPending. Dropping the entry on
			// the offer would answer either by rebuilding the alert from a later poll —
			// a worse distance for the pass, which is the whole point of the hold — or,
			// for an aircraft that has left the feed, by losing it. q.add ignores a key
			// already queued or in flight, so re-offering costs a map lookup.
			p.q.add(h.alert)
		}
	}
}

// sameReceiver reports whether this alert's distances still mean what they say. A hold
// waits minutes for its aircraft while alerts.yaml is re-read every five seconds, so the
// receiver can move under it — and then park compares a distance measured from where the
// receiver was against one measured from where it is, and announces a pass from a place
// the operator has left. Retiring the hold costs nothing: the next poll parks a fresh one
// against the receiver as it is now.
func sameReceiver(cfg *Alerts, a *Alert) bool {
	if !a.HasDistance {
		return true // nothing was measured, so nothing can be measured from the wrong place
	}
	return cfg.Lat != nil && cfg.Lon != nil && *cfg.Lat == a.recvLat && *cfg.Lon == a.recvLon
}

// claimedBy re-runs the match that built this alert against the rules as they are now, on
// the sighting it was built from — which the alert carries, so a waiting alert stays
// frozen: it is not dropped because the aircraft has since climbed out of the rule that
// caught it. nil when the alert has nothing left to announce it.
//
// Asking which rule claims the aircraft rather than whether its own rule still alerts is
// what catches an exception written ahead of that rule: first match wins, so the aircraft
// now stops at the new rule, and a muted one answers nil. Both places an alert waits ask
// it — a hold on the poll side, the queue on the notify side — because an alert that has
// already moved from one to the other is exactly the one a mute must still catch.
func claimedBy(cfg *Alerts, a *Alert) *Rule {
	// matchInput, never the alert itself: a passes_within_nm rule writes its prediction
	// into whatever alert it is handed, and this one may already be in the notifier's
	// hands.
	snap := a.matchInput()
	r := firstMatch(cfg.Rules, snap.AC, snap.Plane, snap)
	if r == nil || r.priorityIn(cfg) == 0 || r.Key() != a.Trigger {
		return nil
	}
	// When it sends is not part of the key, so a rule that has changed its mind about
	// that is not the rule this alert was built under, even though it answers to the same
	// name. Both directions matter and for the same reason: the two alerts share a
	// cooldown, so whichever is let out silences the other. A first-sighting alert waiting
	// in the queue would publish immediately and swallow the pass the operator just asked
	// to wait for; a hold whose rule now sends on sight has already been overtaken by the
	// poll that queued the ordinary one.
	if (r.Notify == notifyClosestPass) != a.AtClosest {
		return nil
	}
	// And measured from the receiver that is there now. A hold retired for having moved
	// may already have put its alert in the queue, which outlives the hold — so the
	// distance has to be judged here as well, or the pass the sweep just refused to
	// announce is announced from the queue instead.
	if !sameReceiver(cfg, a) {
		return nil
	}
	return r
}

// retry is how long a failing key waits and how long it will wait next time. The two
// are always set together and cleared together, so they are one entry.
type retry struct {
	until time.Time
	delay time.Duration
}

func notifyLoop(ctx context.Context, live *Live, n *Notifier, state *State, q *queue) {
	backoff := map[string]retry{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-time.After(time.Second):
		}

		for {
			a := q.take()
			if a == nil {
				break
			}
			// Read per alert, not per drain: one publish can spend the whole 30s
			// budget retrying, and the rules are re-read every five seconds, so a drain
			// outlives several reloads. The alert about to be sent must be weighed
			// against the config as it is now, not as it was when the drain began.
			cfg := live.Get()
			cooldown := cfg.Cooldown.Std()
			key := cooldownKey(a.Hex, a.Trigger)

			// The last gate before something is sent, and the only one the queue is
			// behind: alerts.yaml is re-read every five seconds, so a rule can be
			// deleted, muted or shadowed between the poll that queued this and now. A
			// mute that still let a queued alert out would be a mute the operator cannot
			// trust — including one written as an exception ahead of the rule that
			// queued it, which leaves that rule alerting and this aircraft no longer
			// reaching it. No cooldown is recorded, so the alert returns if the rule does.
			if claimedBy(cfg, a) == nil {
				slog.Debug("dropping a queued alert no rule claims any more", "icao", a.Hex, "trigger", a.Trigger)
				q.done(key)
				continue
			}
			// Belt and braces. The queue holds one entry per key and marks it in flight,
			// so nothing should advance this cooldown while the alert waits — but the
			// cost of being wrong is a duplicate notification, and this check is a map
			// lookup.
			if !state.Eligible(key, cooldown) {
				q.done(key)
				continue
			}
			if r, ok := backoff[key]; ok && time.Now().Before(r.until) {
				q.done(key)
				continue
			}

			err := n.Publish(ctx, a)
			if err != nil {
				// Cooldown is not advanced, so the next poll re-enqueues naturally.
				// The per-key backoff keeps a permanently broken sink to a trickle.
				d := backoff[key].delay
				if d == 0 {
					d = 30 * time.Second
				} else if d < 30*time.Minute {
					d *= 2
				}
				backoff[key] = retry{until: time.Now().Add(d), delay: d}
				slog.Warn("notification failed", "icao", a.Hex, "trigger", a.Trigger, "retry_after", d, "err", err)
				q.done(key)
				continue
			}

			delete(backoff, key)
			// Before the cooldown, and independent of it: this is what the poll loop's
			// parked alerts read to know they are done.
			a.delivered.Store(true)
			state.Record(key, cooldown)
			q.done(key)
			slog.Info("notified", "icao", a.Hex, "trigger", a.Trigger, "reg", a.AC.Reg)
		}
	}
}

func refreshLoop(ctx context.Context, live *Live, db *DB, state *State, refreshAtStart bool) {
	// Done here, inside the single refresher, rather than in a parallel goroutine:
	// two overlapping cycles could race each other's results and could each satisfy
	// the other's "two consecutive refreshes agree" shrink confirmation.
	if refreshAtStart {
		if err := db.Refresh(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("startup db refresh failed, continuing with cached list", "err", err)
		}
	}

	interval := live.Get().DB.RefreshInterval.Std()
	refresh := time.NewTicker(interval)
	defer refresh.Stop()
	// Retries a previously failed state write; a no-op when the ledger is clean.
	flush := time.NewTicker(time.Minute)
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			cfg := live.Get()
			if err := db.Refresh(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("db refresh failed, continuing with cached list", "err", err)
			}
			if next := cfg.DB.RefreshInterval.Std(); next != interval {
				interval = next
				refresh.Reset(interval)
			}
		case <-flush.C:
			state.Flush(live.Get().Cooldown.Std())
		}
	}
}
