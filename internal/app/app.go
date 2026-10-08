package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed ui.html login.html public-sans.woff2
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

// Main is the process entry point, called from cmd/sky-notify.
func Main() {
	healthcheck := flag.Bool("healthcheck", false, "probe this container's own /healthz and exit 0/1")
	configExport := flag.Bool("config-export", false, "print the settings database as one JSON document and exit")
	configImport := flag.String("config-import", "", "load a JSON document exported by -config-export into the settings database and exit")
	out := flag.String("out", "", "file to write -config-export to (default: stdout)")
	redact := flag.Bool("redact", false, "with -config-export, blank ntfy and ai credentials")
	resetPassword := flag.Bool("reset-password", false, "remove the web UI's password, so the next visit asks for a new one, and exit")
	flag.Parse()

	if *healthcheck {
		if err := probeSelf(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	if *resetPassword {
		if err := runResetPassword(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
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
	if err := decodeStrict(bytes.NewReader(blob), &doc); err != nil {
		return fmt.Errorf("%s: %w", importPath, err)
	}
	return importSettings(store, &doc, environMap(os.Environ()))
}

// runResetPassword is the way back in for an operator who has forgotten the password:
// `docker exec <container> /sky-notify -reset-password`. It forgets the stored password
// rather than setting a new one, so no password has to pass through a shell history; the
// running service notices within a few seconds and the next visit to the page asks for a
// new one, exactly as the first visit did.
func runResetPassword(out io.Writer) error {
	if os.Getenv(uiPasswordEnv) != "" {
		return fmt.Errorf("the password is set by %s; change or remove it there and recreate the container", uiPasswordEnv)
	}
	store, err := OpenSettings(dbPath(environMap(os.Environ())))
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.ClearUIPassword(); err != nil {
		return err
	}
	fmt.Fprintln(out, "The web UI password is removed. Open the web UI within a few seconds to choose a new one.")
	return nil
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
	// Built once, outside the restart loop below, so a restart neither forgets a
	// password set a moment ago nor resets the login throttle.
	a, err := newAuth(os.Getenv(uiPasswordEnv), store)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.watch(ctx.Done(), configPollInterval)

	// Saving the startup config from the web UI restarts the service in place: every
	// long-lived object below is built from it once, so the only way to apply a new one
	// is to build them again. served is the address to fall back to when a saved one
	// will not bind: the one the last run listened on, or on a fresh start the default,
	// so a bad address saved from the UI cannot leave a container with no page to fix
	// it from across a restart. An address set by SKY_LISTEN has no fallback: the
	// operator chose it outside the UI and can change it there.
	served := defaultConfig().Listen
	if _, ok := env["SKY_LISTEN"]; ok {
		served = ""
	}
	for {
		restart, addr, err := serve(ctx, store, a, served)
		if err != nil || !restart {
			return err
		}
		served = addr
		slog.Info("restarting with the saved settings")
	}
}

// serve runs the service until ctx is done, or until a config save asks for a restart,
// which it reports along with the address it served on. A config that is incomplete or
// does not work is not fatal: the web UI is served on its own, saying what is wrong, so
// it can be put right from there. The web UI is up before anything slow starts, so it
// is reachable all the way through.
func serve(ctx context.Context, store *SettingsStore, a *auth, fallback string) (restart bool, addr string, err error) {
	cfg, err := loadConfigOverlaid(os.Environ(), store)
	if err != nil {
		// Only a malformed stored document or environment variable lands here; the
		// web UI can fix neither.
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

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var restarting atomic.Bool
	h := &server{live: live, store: store, auth: a, db: &DB{}, generation: rand.Text(), restart: func() {
		restarting.Store(true)
		cancel()
	}}
	if a.state() == authOff {
		slog.Warn("the web UI has no password; set one in Settings")
	}

	// Everything the handlers read is built before the server starts, so none of it is
	// written while a request may be reading it. notRunning is why the service cannot
	// run with this config; the web UI is served alone then, saying so.
	var notRunning error
	var (
		httpClient *http.Client
		db         *DB
		state      *State
		notifier   *Notifier
		source     *Source
	)
	if notRunning = cfg.validate(); notRunning == nil {
		if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
			notRunning = fmt.Errorf("cache dir %s: %w", cfg.CacheDir, err)
		}
	}
	if notRunning == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
		notifier, notRunning = NewNotifier(cfg)
	}
	if notRunning == nil {
		if cfg.Tar1090URL == "" {
			slog.Warn("tar1090_url unset: notifications will carry no link to your map (set it in Settings)")
		}
		db = NewDB(cfg, httpClient)
		state = NewState(cfg.CacheDir)
		state.Load(alerts.Cooldown.Std())
		// The signal context, not notifyCtx: the notifier needs to know when Ctrl-C was
		// pressed so it can stop drawing maps, while still delivering on notifyCtx for
		// the whole drain window below.
		notifier.shutdown = runCtx
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
				slog.Warn("rules ask for AI research but no provider is configured (set it in Settings, or SKY_AI_URL and SKY_AI_MODEL)",
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
		source = NewSource(cfg, httpClient)
		h.db, h.state, h.notifier, h.history = db, state, notifier, hist
	}

	q := newQueue(maxPending)
	srv, addr, err := listen(cfg.Listen, h.mux(q))
	if err != nil {
		// A saved address that will not bind would leave nothing to fix it from, so the
		// page stays where the operator last reached it, saying why.
		if fallback == "" || fallback == cfg.Listen {
			return false, "", err
		}
		var ferr error
		if srv, addr, ferr = listen(fallback, h.mux(q)); ferr != nil {
			return false, "", err
		}
		notRunning = err
	}
	// waitSetup holds the web UI alone until a config save restarts it or the process
	// is stopped.
	waitSetup := func(why error) (bool, string, error) {
		slog.Warn("not running: open the web UI to finish setting up", "reason", why)
		h.setSetupErr(why)
		<-runCtx.Done()
		shutdown(srv)
		return restarting.Load(), addr, nil
	}
	if notRunning != nil {
		return waitSetup(notRunning)
	}

	// Cold start needs a complete list. Running with a partial or empty one would look
	// healthy while silently matching nothing. The page says what it is waiting for.
	loadedFromCache := db.Load()
	if !loadedFromCache {
		h.setSetupErr(errors.New("downloading the interesting-aircraft database"))
		err := seedDB(runCtx, db, func(err error) {
			h.setSetupErr(fmt.Errorf("downloading the interesting-aircraft database, still retrying: %w", err))
		})
		if err != nil {
			if runCtx.Err() != nil {
				shutdown(srv)
				return restarting.Load(), addr, nil
			}
			return waitSetup(fmt.Errorf("no usable interesting-aircraft list: %w", err))
		}
		h.setSetupErr(nil)
	}
	// Whether the list came from cache or a cold-start fetch, refreshLoop owns every
	// subsequent refresh. It is the only refresher, so two cycles can never overlap and
	// race each other's results or the shrink confirmation.
	refreshAtStart := loadedFromCache

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
		pollLoop(runCtx, live, source, db, state, q, h)
	}()
	go func() { defer wg.Done(); notifyLoop(notifyCtx, live, notifier, state, q) }()
	go func() { defer wg.Done(); refreshLoop(runCtx, live, db, state, refreshAtStart) }()
	go func() {
		defer wg.Done()
		watcher.run(runCtx, live, os.Environ(), configPollInterval, func(c *Alerts) {
			logLevel.Set(parseLevel(c.LogLevel))
		})
	}()

	<-runCtx.Done()
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

	shutdown(srv)
	wg.Wait()
	state.Flush(live.Get().Cooldown.Std())
	return restarting.Load(), addr, nil
}

// listen binds before returning, so an address already in use is an error here rather
// than a log line from a goroutine while the process carries on serving nothing.
func listen(addr string, handler http.Handler) (*http.Server, string, error) {
	if addr == "" {
		addr = defaultConfig().Listen
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("listen %s: %w", addr, err)
	}
	srv := &http.Server{Addr: addr, Handler: handler}
	slog.Info("listening", "addr", addr)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
		}
	}()
	return srv, addr, nil
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
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
		if cfg, err := loadConfigOverlaid(os.Environ(), store); err == nil && cfg.Listen != "" {
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
