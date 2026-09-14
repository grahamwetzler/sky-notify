package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed ui.html public-sans.woff2
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
	// The provider is read per alert, not captured here: it is hot-reloaded with the
	// rules that ask for it. This only says so once at startup, when nothing is set.
	notifier.live, notifier.aiClient = live, httpClient
	if !alerts.aiEnabled() {
		if asked := researchRules(alerts); len(asked) > 0 {
			slog.Warn("rules ask for AI research but no provider is configured (set ai.url and ai.model in alerts.yaml, or SKY_AI_URL and SKY_AI_MODEL)",
				"rules", strings.Join(asked, ", "))
		}
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
	h := &server{live: live, db: db, state: state, notifier: notifier, history: hist}

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
