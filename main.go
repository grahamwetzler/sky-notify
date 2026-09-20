package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	configExport := flag.Bool("config-export", false, "print the settings database as one JSON document and exit")
	configImport := flag.String("config-import", "", "load a JSON document exported by -config-export into the settings database and exit")
	out := flag.String("out", "", "file to write -config-export to (default: stdout)")
	redact := flag.Bool("redact", false, "with -config-export, blank ntfy and ai credentials")
	flag.Parse()

	if *healthcheck {
		if err := probeSelf(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	if *configExport || *configImport != "" {
		if err := runConfigCLI(*configExport, *configImport, *out, *redact); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runConfigCLI opens the settings database directly and does not start the HTTP server,
// which is what makes it usable for bootstrapping (seeding a first-boot database with no
// running server to POST to) and for scripted backup/restore.
func runConfigCLI(doExport bool, importPath, out string, redact bool) error {
	store, err := OpenSettings(dbPath(environMap(os.Environ())))
	if err != nil {
		return err
	}
	defer store.Close()

	if doExport {
		doc, err := exportSettings(store, redact)
		if err != nil {
			return err
		}
		blob, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		blob = append(blob, '\n')
		if out == "" {
			_, err = os.Stdout.Write(blob)
			return err
		}
		// An export carries plaintext credentials by default (-redact aside), so it must
		// never be readable by anyone but the owner. Writing straight to out and
		// chmod'ing after leaves a window — and an existing looser-permissioned file
		// stays that way until the chmod lands — where the plaintext is exposed, so the
		// owner-only file is written next to the destination and renamed into place
		// instead.
		return writeFileAtomic(out, blob, 0o600)
	}

	blob, err := os.ReadFile(importPath)
	if err != nil {
		return err
	}
	var doc importDocument
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("%s: %w", importPath, err)
	}
	return importSettings(store, &doc, environMap(os.Environ()))
}

// writeFileAtomic writes data to a fresh, perm-moded file in path's directory and renames
// it over path, so a reader never observes a partially written file nor one that briefly
// carries path's old (possibly looser) permissions.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func run() error {
	env := environMap(os.Environ())
	store, err := OpenSettings(dbPath(env))
	if err != nil {
		return fmt.Errorf("settings database: %w", err)
	}
	defer store.Close()

	if err := checkLegacyYAML(store, dbPath(env)); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	cfg, err := LoadConfig(os.Environ(), store)
	if err != nil {
		// slog is not configured yet; this must still be legible.
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	alerts, err := LoadAlerts(os.Environ(), store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	live := NewLive(alerts)
	// Constructed here, not inside the goroutine below: its baseline must be what
	// LoadAlerts just read, or an edit made while we start up is never noticed.
	watcher := newConfigWatcher(store)
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
	h := &server{live: live, db: db, state: state, notifier: notifier, history: hist, store: store}

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
	// Derive the port from the effective config, not just the environment: a
	// database-only custom port would otherwise make every healthcheck fail against a
	// healthy service.
	listen, loaded := defaultConfig().Listen, false
	if store, err := OpenSettings(dbPath(environMap(os.Environ()))); err == nil {
		defer store.Close()
		if cfg, err := LoadConfig(os.Environ(), store); err == nil {
			listen, loaded = cfg.Listen, true
		}
	}
	if !loaded {
		if v := os.Getenv("SKY_LISTEN"); v != "" {
			listen = v
		}
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
