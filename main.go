package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed ui.html
var uiFS embed.FS

const (
	maxPending     = 512
	coldStartLimit = 5 * time.Minute
	drainTimeout   = 5 * time.Second
	// How many matching aircraft a rule preview shows. The count it reports is the
	// real total; only the sample is capped.
	previewLimit = 20
	// How many sent alerts one page of a rule's history holds.
	historyPage = 20
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
	if cfg.Tar1090URL == "" {
		slog.Warn("tar1090_url unset: notifications will carry no link to your map (set SKY_TAR1090_URL)")
	}

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
	// The signal context, not notifyCtx: the notifier needs to know when Ctrl-C was
	// pressed so it can stop drawing maps, while still delivering on notifyCtx for the
	// whole drain window below.
	notifier.shutdown = ctx
	if cfg.mapEnabled() {
		tiles := newTileStore(httpClient, cfg.Map.TilesURL, cfg.CacheDir)
		go tiles.prune()
		if maps, err := newMapRenderer(tiles); err != nil {
			slog.Warn("map snapshots unavailable", "err", err)
		} else {
			notifier.maps = maps
		}
	} else {
		slog.Info("map snapshots disabled: notifications will carry no image")
	}
	// A store that will not open is a warning, not an outage: sky-notify alerts fine
	// without a record of having done so.
	hist, err := NewHistory(cfg.CacheDir)
	if err != nil {
		slog.Warn("alert history unavailable", "dir", cfg.CacheDir, "err", err)
	} else {
		defer hist.Close()
		notifier.history = hist
	}
	source := NewSource(cfg, httpClient)
	h := &health{live: live, db: db, state: state, notifier: notifier, history: hist}

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
	history  *History

	mu            sync.Mutex
	lastFreshPoll time.Time
	lastPollErr   error
	aircraft      int
	// typeOf is the ICAO type code each aircraft was last heard broadcasting. The
	// database knows the type of the aircraft it lists and nothing else; aircraft.json
	// carries one for everything overhead, which is what makes the type picker
	// answerable without the database.
	// ponytail: memory only, one entry per aircraft seen since start — a restart
	// starts the list again, as the tracker does.
	typeOf map[string]string
	// The last poll's aircraft, and which of them the tracker called circling at the
	// time. This is what a rule preview is matched against to say what would alert right
	// now. The tracker belongs to the poll loop and is not safe to read from a request,
	// so the one question a rule asks it is answered while the poll loop still holds it.
	overhead []Aircraft
	circling map[string]bool
}

// setPollOK records a good poll. tr may be nil; when it is not it must already have
// been updated with this feed, or the circling flags describe the poll before it.
func (h *health) setPollOK(f *feed, tr *Tracker) {
	h.mu.Lock()
	h.lastFreshPoll, h.lastPollErr, h.aircraft = time.Now(), nil, len(f.Aircraft)
	if h.typeOf == nil {
		h.typeOf = map[string]string{}
	}
	h.overhead = f.Aircraft
	h.circling = map[string]bool{}
	for _, ac := range f.Aircraft {
		hex, t := normalizeHex(ac.Hex), strings.TrimSpace(ac.Type)
		if hex == "" {
			continue
		}
		if t != "" {
			h.typeOf[hex] = t
		}
		if tr.get(hex).circling() {
			h.circling[hex] = true
		}
	}
	h.mu.Unlock()
}

// coord reads one drafted coordinate. Blank is a cleared receiver, and so is anything
// unparseable: the page sends what is in the box, and a half-typed "-" is not a place.
func coord(v string) *float64 {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

// previewAlerts is the running config with the receiver the page is holding, which is
// not yet the saved one: coordinates are edited in the same session as the rule that
// needs them, and matching a distance rule against the old ones answers for the wrong
// place — or, before they are first saved, for nowhere at all. The environment still
// wins, exactly as it does at save; a coordinate it sets is not editable in the page
// either.
func (h *health) previewAlerts(q url.Values) *Alerts {
	cfg := h.live.Get()
	// Absent is not blank. Only the page sends these, and it sends both or neither, so
	// nothing here means nothing is being drafted; a blank one means the receiver has
	// been cleared on screen, and answering that from the saved pair would show
	// distances from a place this configuration no longer knows.
	if !q.Has("lat") || !q.Has("lon") {
		return cfg
	}
	draft := *cfg
	draft.Lat, draft.Lon = coord(q.Get("lat")), coord(q.Get("lon"))
	if draft.Lat == nil || draft.Lon == nil {
		// Half a pair measures nothing. Both go, or a rule with a distance would be
		// matched against a receiver at a longitude the page never gave.
		draft.Lat, draft.Lon = nil, nil
	}
	env := environMap(os.Environ())
	for _, b := range alertEnvBindings() {
		if _, set := env[b.name]; !set {
			continue
		}
		switch b.key {
		case "lat":
			draft.Lat = cfg.Lat
		case "lon":
			draft.Lon = cfg.Lon
		}
	}
	return &draft
}

// pollAge is how old the snapshot is, and whether it still describes the sky by the same
// staleness rule /healthz uses. A preview drawn from a dead feeder must say so rather
// than read as an empty sky — and the age travels with the answer, so a page holding one
// can watch it go stale without asking again.
func (h *health) pollAge() (age time.Duration, ok, fresh bool) {
	h.mu.Lock()
	last := h.lastFreshPoll
	h.mu.Unlock()
	if last.IsZero() {
		return 0, false, false
	}
	age = time.Since(last)
	return age, true, age <= h.live.Get().Source.PollInterval.Std()*3
}

// snapshot is the last poll's traffic, for matching a draft rule against what is
// overhead right now.
func (h *health) snapshot() ([]Aircraft, map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.overhead, h.circling
}

// feedTypes is every ICAO type code the receiver has heard, and how many aircraft
// broadcast each one. Offered alongside the database's own codes so a type nothing in
// the database carries is still a value you can pick rather than one you must know to
// type.
func (h *health) feedTypes() []FacetValue {
	h.mu.Lock()
	counts := map[string]int{}
	for _, t := range h.typeOf {
		counts[t]++
	}
	h.mu.Unlock()
	out := make([]FacetValue, 0, len(counts))
	for v, n := range counts {
		out = append(out, FacetValue{Value: v, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func (h *health) setPollErr(err error) {
	h.mu.Lock()
	h.lastPollErr = err
	h.mu.Unlock()
}

func (h *health) mux(q *queue) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := uiFS.ReadFile("ui.html")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		env := environMap(os.Environ())
		alerts, err := loadAlertsFile(env)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		// Each locked setting carries its value as well as its name. The page warns
		// about rules this server would reject, and the save validates the file with
		// the environment laid over it — so without the value here, a distance rule on
		// a receiver whose coordinates arrive as SKY_LAT and SKY_LON would be marked
		// invalid against a file that never mentions them.
		var locked []map[string]string
		for _, b := range alertEnvBindings() {
			if v, ok := env[b.name]; ok {
				locked = append(locked, map[string]string{"key": b.key, "env": b.name, "value": v})
			}
		}
		// The cooldown key of every rule, in order. A rule may have no name, and the
		// page has to be able to say what the ledger and the logs will call it — which
		// only this side can compute, so it is not left to the page to guess.
		keys := make([]string, len(alerts.Rules))
		for i := range alerts.Rules {
			keys[i] = alerts.Rules[i].Key()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"alerts": alerts, "path": alertsPath(env), "locked": locked,
			"rule_keys": keys, "vocabulary": h.db.Vocabulary(), "feed_types": h.feedTypes(),
		})
	})
	mux.HandleFunc("POST /api/preview", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var rule Rule
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rule); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// A negative or unparseable offset reads as the first page rather than as an
		// error: it can only come from our own page, and showing the top of the list is
		// a better answer than refusing to preview at all.
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil || offset < 0 {
			offset = 0
		}
		total, sample := h.db.Match(rule, previewLimit, offset)
		rows, _ := h.db.Stats()
		body := map[string]any{
			"total": total, "aircraft": sample, "database": rows,
			"limit": previewLimit, "offset": offset, "key": rule.Key(),
		}
		// What the rule would alert on this second, judged against the last poll rather
		// than the database: the whole rule applies here, altitude and distance and
		// flight path included. Not paged — the sky holds a few hundred aircraft, not a
		// few hundred thousand, so the sample cap is only ever a courtesy.
		overhead, circling := h.snapshot()
		liveTotal, liveSample := MatchLive(rule, overhead, circling, h.db, h.previewAlerts(r.URL.Query()), previewLimit)
		age, seen, fresh := h.pollAge()
		live := map[string]any{
			"total": liveTotal, "aircraft": liveSample, "overhead": len(overhead),
			"fresh": fresh,
		}
		if seen {
			live["age_s"] = int(age.Seconds())
		}
		body["live"] = live
		// Faceting scans the whole database once per pickable field, and the facets do
		// not change as you page through a fixed rule. Only the first page pays for it.
		if offset == 0 {
			body["facets"] = h.db.Facets(rule)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		// The rule's cooldown key, which is what deliveries were filed under. Blank is
		// an empty page, not an error: a rule the page has not saved yet has no history.
		// `after` is the cursor a previous page handed back, absent on the first.
		q := r.URL.Query()
		page, err := h.history.List(q.Get("key"), q.Get("after"), historyPage)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("PUT /api/alerts", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		alerts := defaultAlerts()
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(alerts); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		// Validate what the service will actually run: the file as written, with the
		// environment laid over it exactly as LoadAlerts does. The overlay is a copy, so
		// the file keeps the operator's own values — an override is not silently baked in.
		// Only the env bindings' scalar fields are reassigned, and none of them alias the
		// rules slice, so a shallow copy is enough to keep the payload untouched.
		overlaid := *alerts
		env := environMap(os.Environ())
		if err := applyAlertEnv(&overlaid, env); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if err := overlaid.validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		blob, err := yaml.Marshal(alerts)
		if err == nil {
			blob = append([]byte("# Written by the sky-notify web UI. Hand edits are read back on the next reload.\n"), blob...)
			err = writeFileDurable(alertsPath(env), blob)
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
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

func writeJSONError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
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
