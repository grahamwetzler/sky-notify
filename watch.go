package main

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"time"
)

func reloadConfig(live *Live, environ []string, onReload func(*Alerts)) error {
	next, err := LoadAlerts(environ)
	if err != nil {
		return err
	}
	live.p.Store(next)
	if onReload != nil {
		onReload(next)
	}
	return nil
}

// configWatcher notices edits to the alerts file. It compares a digest rather than
// watching with inotify: the config is a bind mount in Docker, where inotify is
// unreliable, and editors replace the file by rename. Polling by path survives both.
type configWatcher struct {
	path    string
	digest  [32]byte
	missing bool
}

// newConfigWatcher takes the baseline eagerly, so a caller that constructs it before
// starting the watch goroutine cannot miss an edit made in between.
func newConfigWatcher(environ []string) *configWatcher {
	w := &configWatcher{path: alertsPath(environMap(environ))}
	w.digest, w.missing = w.read()
	return w
}

func (w *configWatcher) read() ([32]byte, bool) {
	b, err := os.ReadFile(w.path)
	return sha256.Sum256(b), os.IsNotExist(err)
}

// changed reports whether the file differs from the last state seen, and records it.
// A file that appears or disappears counts, so an empty read is not mistaken for one.
func (w *configWatcher) changed() bool {
	digest, missing := w.read()
	if digest == w.digest && missing == w.missing {
		return false
	}
	w.digest, w.missing = digest, missing
	return true
}

// run publishes validated file changes to live until ctx is done.
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
		// A half-written file parses badly; logging and keeping the running config is
		// the only acceptable outcome, and the next write fires another attempt.
		err := reloadConfig(live, environ, onReload)
		if err != nil {
			slog.Error("config reload failed, keeping running config", "err", err)
			continue
		}
		slog.Info("config reloaded", "path", w.path)
	}
}
