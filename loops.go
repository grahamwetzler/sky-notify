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

func pollLoop(ctx context.Context, live *Live, src *Source, db *DB, state *State, q *queue, h *health) {
	interval := live.Get().Source.PollInterval.Std()
	t := time.NewTicker(interval)
	defer t.Stop()
	tr := NewTracker()
	for {
		cfg := live.Get()
		poll(ctx, cfg, src, db, state, q, h, tr)
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

func poll(ctx context.Context, cfg *Alerts, src *Source, db *DB, state *State, q *queue, h *health, tr *Tracker) {
	f, err := src.Fetch(ctx, time.Now())
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("poll failed", "err", err)
		}
		h.setPollErr(err)
		return
	}
	tr.Update(f)
	h.setPollOK(f, tr)

	cooldown := cfg.Cooldown.Std()
	for _, ac := range f.Aircraft {
		a := Evaluate(ac, db, cfg, tr.get(normalizeHex(ac.Hex)))
		if a == nil {
			continue
		}
		if !state.Eligible(cooldownKey(a.Hex, a.Trigger), cooldown) {
			continue
		}
		q.add(a)
	}
}

func notifyLoop(ctx context.Context, live *Live, n *Notifier, state *State, q *queue) {
	backoff := map[string]time.Time{}
	delay := map[string]time.Duration{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-time.After(time.Second):
		}

		cooldown := live.Get().Cooldown.Std()
		for {
			a := q.take()
			if a == nil {
				break
			}
			key := cooldownKey(a.Hex, a.Trigger)

			// Belt and braces. The queue holds one entry per key and marks it in flight,
			// so nothing should advance this cooldown while the alert waits — but the
			// cost of being wrong is a duplicate notification, and this check is a map
			// lookup.
			if !state.Eligible(key, cooldown) {
				q.done(key)
				continue
			}
			if until, ok := backoff[key]; ok && time.Now().Before(until) {
				q.done(key)
				continue
			}

			err := n.Publish(ctx, a)
			if err != nil {
				// Cooldown is not advanced, so the next poll re-enqueues naturally.
				// The per-key backoff keeps a permanently broken sink to a trickle.
				d := delay[key]
				if d == 0 {
					d = 30 * time.Second
				} else if d < 30*time.Minute {
					d *= 2
				}
				delay[key] = d
				backoff[key] = time.Now().Add(d)
				slog.Warn("notification failed", "icao", a.Hex, "trigger", a.Trigger, "retry_after", d, "err", err)
				q.done(key)
				continue
			}

			delete(backoff, key)
			delete(delay, key)
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
