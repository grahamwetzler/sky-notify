package main

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
	p.tr.Update(f)
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
	alerting := alertingKeys(cfg)
	grace := 3 * cfg.Source.PollInterval.Std()
	for key, h := range p.holds {
		switch {
		// The rule is gone, or has been muted. Evaluate stops matching either of those
		// the moment the file is reloaded, which leaves the alert parked with nothing
		// to announce it for — and an alert for a rule the operator has just deleted is
		// exactly what a mute is written to prevent.
		case !alerting[h.alert.Trigger]:
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

// alertingKeys is the cooldown key of every rule that would still send something. A
// muted rule has no key here: priority 0 means the aircraft stops at it.
func alertingKeys(cfg *Alerts) map[string]bool {
	keys := make(map[string]bool, len(cfg.Rules))
	for i := range cfg.Rules {
		if r := &cfg.Rules[i]; r.priorityIn(cfg) != 0 {
			keys[r.Key()] = true
		}
	}
	return keys
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
			alerting := alertingKeys(cfg)
			key := cooldownKey(a.Hex, a.Trigger)

			// The last gate before something is sent, and the only one the queue is
			// behind: alerts.yaml is re-read every five seconds, so a rule can be
			// deleted or muted between the poll that queued this and now. A mute that
			// still let a queued alert out would be a mute the operator cannot trust.
			// No cooldown is recorded, so the alert returns if the rule does.
			if !alerting[a.Trigger] {
				slog.Debug("dropping a queued alert whose rule no longer alerts", "icao", a.Hex, "trigger", a.Trigger)
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

			// Resolved here rather than at match time, for the same reason the mute
			// gate above is: a closest-pass alert waits minutes for its aircraft to
			// pass, and alerts.yaml is re-read every five seconds. A rule whose
			// research was switched off in that window must not still send the
			// aircraft to the provider — that is a third party, and a bill. Last,
			// because every gate above can still drop this alert unsent.
			a.ResearchPrompt = researchPromptFor(cfg, a.Trigger)

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
