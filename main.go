package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	maxPending     = 512
	coldStartLimit = 5 * time.Minute
	drainTimeout   = 5 * time.Second
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe this container's own /healthz and exit 0/1")
	flag.Parse()

	if *healthcheck {
		if err := probeSelf(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := LoadConfig(os.Environ())
	if err != nil {
		// slog is not configured yet; this must still be legible.
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	alerts, err := LoadAlerts(os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	live := NewLive(alerts)
	// Constructed here, not inside the goroutine below: its baseline must be the file
	// LoadAlerts just read, or an edit made while we start up is never noticed.
	watcher := newConfigWatcher(os.Environ())
	var logLevel slog.LevelVar
	logLevel.Set(parseLevel(alerts.LogLevel))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &logLevel})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return fmt.Errorf("cache dir %s: %w", cfg.CacheDir, err)
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}
	db := NewDB(cfg, httpClient)
	state := NewState(cfg.CacheDir)
	state.Load(alerts.Cooldown.Std())

	notifier, err := NewNotifier(cfg)
	if err != nil {
		return err
	}
	source := NewSource(cfg, httpClient)
	h := &health{live: live, db: db, state: state, notifier: notifier}

	// Cold start needs a complete list. Running with a partial or empty one would look
	// healthy while silently matching nothing.
	loadedFromCache := db.Load()
	if !loadedFromCache {
		if err := seedDB(ctx, db); err != nil {
			return fmt.Errorf("no usable interesting-aircraft list: %w", err)
		}
	}
	// Whether the list came from cache or a cold-start fetch, refreshLoop owns every
	// subsequent refresh. It is the only refresher, so two cycles can never overlap and
	// race each other's results or the shrink confirmation.
	refreshAtStart := loadedFromCache

	q := newQueue(maxPending)
	srv := &http.Server{Addr: cfg.Listen, Handler: h.mux(q)}

	// The notifier gets its own cancellation so shutdown can stop producing and still
	// drain what is already queued, rather than abandoning an in-flight emergency.
	notifyCtx, stopNotify := context.WithCancel(context.Background())
	defer stopNotify()

	// pollDone lets shutdown wait for the *producer* specifically: a cancelled context
	// does not mean pollLoop has returned, and an in-progress poll could still enqueue
	// alerts after we sampled an empty queue.
	pollDone := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		defer close(pollDone)
		pollLoop(ctx, live, source, db, state, q, h)
	}()
	go func() { defer wg.Done(); notifyLoop(notifyCtx, live, notifier, state, q) }()
	go func() { defer wg.Done(); refreshLoop(ctx, live, db, state, refreshAtStart) }()
	go func() {
		defer wg.Done()
		watcher.run(ctx, live, os.Environ(), configPollInterval, func(c *Alerts) {
			logLevel.Set(parseLevel(c.LogLevel))
		})
	}()

	go func() {
		slog.Info("listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	// Wait for the producer to actually exit before sampling the queue, then give the
	// notifier a bounded window to finish what is already queued.
	<-pollDone
	drainBy := time.Now().Add(drainTimeout)
	for q.depth() > 0 && time.Now().Before(drainBy) {
		time.Sleep(100 * time.Millisecond)
	}
	if d := q.depth(); d > 0 {
		slog.Warn("shutting down with alerts still queued", "pending", d)
	}
	stopNotify()

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
	wg.Wait()
	state.Flush(live.Get().Cooldown.Std())
	return nil
}

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
	h.setPollOK(len(f.Aircraft))
	tr.Update(f)

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

// queue is a bounded, key-deduplicated set of pending alerts. An entry stays present
// through delivery so the poll loop cannot re-enqueue a key that is in flight.
type queue struct {
	mu    sync.Mutex
	items map[string]*pendingAlert
	max   int
	wake  chan struct{}
}

type pendingAlert struct {
	alert    *Alert
	inflight bool
}

func newQueue(max int) *queue {
	return &queue{items: map[string]*pendingAlert{}, max: max, wake: make(chan struct{}, 1)}
}

func (q *queue) add(a *Alert) {
	key := cooldownKey(a.Hex, a.Trigger)
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, exists := q.items[key]; exists {
		return
	}
	if len(q.items) >= q.max {
		// An emergency evicts an ordinary alert rather than being dropped behind it.
		if !a.Emergency || !q.evictOrdinaryLocked() {
			slog.Warn("pending queue full, dropping alert", "icao", a.Hex, "trigger", a.Trigger)
			return
		}
	}
	q.items[key] = &pendingAlert{alert: a}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue) evictOrdinaryLocked() bool {
	for k, p := range q.items {
		if !p.inflight && !p.alert.Emergency {
			delete(q.items, k)
			return true
		}
	}
	return false
}

// take returns the next alert to deliver, preferring emergencies. Map iteration order
// is random, so without this an urgent squawk could sit behind arbitrary routine traffic.
func (q *queue) take() *Alert {
	q.mu.Lock()
	defer q.mu.Unlock()
	var fallback *pendingAlert
	for _, p := range q.items {
		if p.inflight {
			continue
		}
		if p.alert.Emergency {
			p.inflight = true
			return p.alert
		}
		if fallback == nil {
			fallback = p
		}
	}
	if fallback != nil {
		fallback.inflight = true
		return fallback.alert
	}
	return nil
}

func (q *queue) done(key string) {
	q.mu.Lock()
	delete(q.items, key)
	q.mu.Unlock()
}

func (q *queue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

type health struct {
	live     *Live
	db       *DB
	state    *State
	notifier *Notifier

	mu            sync.Mutex
	lastFreshPoll time.Time
	lastPollErr   error
	aircraft      int
}

func (h *health) setPollOK(n int) {
	h.mu.Lock()
	h.lastFreshPoll, h.lastPollErr, h.aircraft = time.Now(), nil, n
	h.mu.Unlock()
}

func (h *health) setPollErr(err error) {
	h.mu.Lock()
	h.lastPollErr = err
	h.mu.Unlock()
}

func (h *health) mux(q *queue) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		lastPoll, pollErr, aircraft := h.lastFreshPoll, h.lastPollErr, h.aircraft
		h.mu.Unlock()

		rows, refreshed := h.db.Stats()
		notifyErr := h.notifier.LastErr()
		stateErr := h.state.WriteErr()

		var reasons []string
		stale := h.live.Get().Source.PollInterval.Std() * 3
		if lastPoll.IsZero() {
			reasons = append(reasons, "no successful poll yet")
		} else if age := time.Since(lastPoll); age > stale {
			reasons = append(reasons, fmt.Sprintf("last fresh poll %s ago", age.Round(time.Second)))
		}
		// A service that cannot deliver, or cannot record what it delivered, has
		// stopped doing its job — green health there would hide it for weeks.
		if notifyErr != nil {
			reasons = append(reasons, "notification sink failing: "+notifyErr.Error())
		}
		if stateErr != nil {
			reasons = append(reasons, "state write failing: "+stateErr.Error())
		}

		body := map[string]any{
			"status":   "ok",
			"aircraft": aircraft,
			"db_rows":  rows,
			"pending":  q.depth(),
		}
		if !lastPoll.IsZero() {
			body["last_fresh_poll_age_s"] = int(time.Since(lastPoll).Seconds())
		}
		if !refreshed.IsZero() {
			body["db_refresh_age_s"] = int(time.Since(refreshed).Seconds())
		}
		if pollErr != nil {
			body["last_poll_error"] = pollErr.Error()
		}
		if notifyErr != nil {
			body["last_notify_error"] = notifyErr.Error()
		}

		status := http.StatusOK
		if len(reasons) > 0 {
			status, body["status"], body["reasons"] = http.StatusServiceUnavailable, "unhealthy", reasons
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	})
	return mux
}

// probeSelf backs the container HEALTHCHECK: the distroless image has no shell, curl or
// wget, so the binary probes itself. A listen spec like ":8080" is not a client target.
func probeSelf() error {
	// Derive the port from the effective config, not just the environment: a YAML-only
	// custom port would otherwise make every healthcheck fail against a healthy service.
	listen := defaultConfig().Listen
	if cfg, err := LoadConfig(os.Environ()); err == nil {
		listen = cfg.Listen
	} else if v := os.Getenv("SKY_LISTEN"); v != "" {
		listen = v
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("cannot derive port from listen %q: %w", listen, err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
