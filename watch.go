package main

import (
	"context"
	"log/slog"
	"time"
)

func reloadConfig(store *SettingsStore, live *Live, environ []string, onReload func(*Alerts)) error {
	next, err := LoadAlerts(environ, store)
	if err != nil {
		return err
	}
	live.p.Store(next)
	if onReload != nil {
		onReload(next)
	}
	return nil
}

// configWatcher notices edits to the alerts document. It compares the row's version
// counter rather than watching for a change notification on the SQLite connection: a
// cheap counter poll is the right default at 5s, and there is no bind-mount/inotify
// quirk here to poll around.
type configWatcher struct {
	store   *SettingsStore
	version int64
}

// newConfigWatcher takes the baseline eagerly, so a caller that constructs it before
// starting the watch goroutine cannot miss an edit made in between.
func newConfigWatcher(store *SettingsStore) *configWatcher {
	w := &configWatcher{store: store}
	_, w.version, _, _ = store.Get("alerts")
	return w
}

// changed reports whether the stored alerts row differs from the version last seen, and
// records it. A row that is absent is implicitly version 0, so it needs no separate
// "missing" flag the way a bind-mounted file did.
func (w *configWatcher) changed() bool {
	_, version, _, _ := w.store.Get("alerts")
	if version == w.version {
		return false
	}
	w.version = version
	return true
}

// run publishes validated changes to live until ctx is done.
func (w *configWatcher) run(ctx context.Context, live *Live, environ []string, interval time.Duration, onReload func(*Alerts)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if !w.changed() {
			continue
		}
		// A half-written save parses badly; logging and keeping the running config is
		// the only acceptable outcome, and the next write fires another attempt.
		err := reloadConfig(w.store, live, environ, onReload)
		if err != nil {
			slog.Error("config reload failed, keeping running config", "err", err)
			continue
		}
		slog.Info("config reloaded", "version", w.version)
	}
}
