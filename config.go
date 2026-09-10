package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a Go duration string ("15s", "24h")
// rather than from an integer count of nanoseconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf(`must be a duration string like "15s" or "24h"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Rule struct {
	Name     string   `yaml:"name"`
	ICAO     []string `yaml:"icao"`
	Reg      []string `yaml:"reg"`
	Operator []string `yaml:"operator"`
	Type     []string `yaml:"type"`
	ICAOType []string `yaml:"icao_type"`
	CMPG     []string `yaml:"cmpg"`
	Category []string `yaml:"category"`
	Tags     []string `yaml:"tags"`
	Priority *int     `yaml:"priority"`
}

type Config struct {
	Source struct {
		URL          string   `yaml:"url"`
		PollInterval Duration `yaml:"poll_interval"`
		MaxAge       Duration `yaml:"max_age"`
	} `yaml:"source"`

	Ntfy struct {
		URL      string `yaml:"url"`
		Topic    string `yaml:"topic"`
		Token    string `yaml:"token"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		Priority int    `yaml:"priority"`
	} `yaml:"ntfy"`

	DB struct {
		Files           []string `yaml:"files"`
		BaseURL         string   `yaml:"base_url"`
		RefreshInterval Duration `yaml:"refresh_interval"`
	} `yaml:"db"`

	CacheDir string   `yaml:"cache_dir"`
	Cooldown Duration `yaml:"cooldown"`

	Filters struct {
		MinAltitudeFt int      `yaml:"min_altitude_ft"`
		MaxAltitudeFt int      `yaml:"max_altitude_ft"`
		MaxDistanceNM float64  `yaml:"max_distance_nm"`
		Lat           *float64 `yaml:"lat"`
		Lon           *float64 `yaml:"lon"`
	} `yaml:"filters"`

	AlertOnEmergencySquawk bool           `yaml:"alert_on_emergency_squawk"`
	Rules                  []Rule         `yaml:"rules"`
	SquawkPriority         map[string]int `yaml:"squawk_priority"`
	Tar1090URL             string         `yaml:"tar1090_url"`
	LogLevel               string         `yaml:"log_level"`
	Listen                 string         `yaml:"listen"`
}

var defaultSquawkPriority = map[string]int{"7500": 5, "7600": 5, "7700": 5}

const configPollInterval = 5 * time.Second

// Live holds the config the running loops read. The pointer is swapped wholesale and
// a published Config is never mutated, so readers need no lock.
type Live struct{ p atomic.Pointer[Config] }

func NewLive(c *Config) *Live {
	l := &Live{}
	l.p.Store(c)
	return l
}

func (l *Live) Get() *Config { return l.p.Load() }

func defaultConfig() *Config {
	c := &Config{}
	c.Source.URL = "http://ultrafeeder/data/aircraft.json"
	c.Source.PollInterval = Duration(15 * time.Second)
	c.Source.MaxAge = Duration(60 * time.Second)
	c.Ntfy.URL = "https://ntfy.sh"
	c.Ntfy.Priority = 3
	c.SquawkPriority = map[string]int{}
	for squawk, priority := range defaultSquawkPriority {
		c.SquawkPriority[squawk] = priority
	}
	c.DB.Files = []string{"plane-alert-db.csv"}
	c.DB.BaseURL = "https://raw.githubusercontent.com/sdr-enthusiasts/plane-alert-db/main"
	c.DB.RefreshInterval = Duration(24 * time.Hour)
	c.CacheDir = "/data"
	c.Cooldown = Duration(24 * time.Hour)
	c.AlertOnEmergencySquawk = true
	c.LogLevel = "info"
	c.Listen = ":8080"
	return c
}

// envConfigVar names the YAML config path. It is bound separately from the table
// below (it is read before the file is parsed) but must still count as a known name.
const envConfigVar = "SKY_CONFIG"

type envBinding struct {
	name  string
	apply func(*Config, string) error
}

// envBindings is an explicit table rather than a reflection walk: it is fewer lines
// than the walker would be, and it is the one greppable place that answers "what can
// this service be configured with".
func envBindings() []envBinding {
	return []envBinding{
		{"SKY_SOURCE_URL", func(c *Config, v string) error { c.Source.URL = v; return nil }},
		{"SKY_SOURCE_POLL_INTERVAL", func(c *Config, v string) error { return setDur(&c.Source.PollInterval, v) }},
		{"SKY_SOURCE_MAX_AGE", func(c *Config, v string) error { return setDur(&c.Source.MaxAge, v) }},

		{"SKY_NTFY_URL", func(c *Config, v string) error { c.Ntfy.URL = v; return nil }},
		{"SKY_NTFY_TOPIC", func(c *Config, v string) error { c.Ntfy.Topic = v; return nil }},
		{"SKY_NTFY_TOKEN", func(c *Config, v string) error { c.Ntfy.Token = v; return nil }},
		{"SKY_NTFY_USER", func(c *Config, v string) error { c.Ntfy.User = v; return nil }},
		{"SKY_NTFY_PASSWORD", func(c *Config, v string) error { c.Ntfy.Password = v; return nil }},
		{"SKY_NTFY_PRIORITY", func(c *Config, v string) error { return setInt(&c.Ntfy.Priority, v) }},

		{"SKY_DB_FILES", func(c *Config, v string) error { c.DB.Files = splitList(v); return nil }},
		{"SKY_DB_BASE_URL", func(c *Config, v string) error { c.DB.BaseURL = v; return nil }},
		{"SKY_DB_REFRESH_INTERVAL", func(c *Config, v string) error { return setDur(&c.DB.RefreshInterval, v) }},

		{"SKY_CACHE_DIR", func(c *Config, v string) error { c.CacheDir = v; return nil }},
		{"SKY_COOLDOWN", func(c *Config, v string) error { return setDur(&c.Cooldown, v) }},

		{"SKY_FILTERS_MIN_ALTITUDE_FT", func(c *Config, v string) error { return setInt(&c.Filters.MinAltitudeFt, v) }},
		{"SKY_FILTERS_MAX_ALTITUDE_FT", func(c *Config, v string) error { return setInt(&c.Filters.MaxAltitudeFt, v) }},
		{"SKY_FILTERS_MAX_DISTANCE_NM", func(c *Config, v string) error { return setFloat(&c.Filters.MaxDistanceNM, v) }},
		{"SKY_FILTERS_LAT", func(c *Config, v string) error { return setFloatPtr(&c.Filters.Lat, v) }},
		{"SKY_FILTERS_LON", func(c *Config, v string) error { return setFloatPtr(&c.Filters.Lon, v) }},

		{"SKY_ALERT_ON_EMERGENCY_SQUAWK", func(c *Config, v string) error { return setBool(&c.AlertOnEmergencySquawk, v) }},
		{"SKY_TAR1090_URL", func(c *Config, v string) error { c.Tar1090URL = v; return nil }},
		{"SKY_LOG_LEVEL", func(c *Config, v string) error { c.LogLevel = v; return nil }},
		{"SKY_LISTEN", func(c *Config, v string) error { c.Listen = v; return nil }},
	}
}

func setDur(d *Duration, v string) error {
	p, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	*d = Duration(p)
	return nil
}

func setInt(i *int, v string) error {
	p, err := strconv.Atoi(v)
	if err != nil {
		return err
	}
	*i = p
	return nil
}

func setFloat(f *float64, v string) error {
	p, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return err
	}
	*f = p
	return nil
}

func setFloatPtr(f **float64, v string) error {
	p, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return err
	}
	*f = &p
	return nil
}

func setBool(b *bool, v string) error {
	p, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	*b = p
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadConfig applies defaults, then the YAML file (if present), then the environment.
// environ is passed in rather than read from os so tests can drive it.
func LoadConfig(environ []string) (*Config, error) {
	env := environMap(environ)

	cfg := defaultConfig()

	path := configPath(env)
	// A missing config file is not an error; a malformed one is.
	if b, err := os.ReadFile(path); err == nil {
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if cfg.SquawkPriority == nil {
		cfg.SquawkPriority = map[string]int{}
	}
	for squawk, priority := range defaultSquawkPriority {
		if _, ok := cfg.SquawkPriority[squawk]; !ok {
			cfg.SquawkPriority[squawk] = priority
		}
	}

	bindings := envBindings()
	known := map[string]bool{envConfigVar: true}
	for _, b := range bindings {
		known[b.name] = true
	}
	// SKY_ is this service's namespace: an unrecognised name there is a typo, and a
	// typo that silently selects a default is exactly the bug this catches.
	var unknown []string
	for k := range env {
		if strings.HasPrefix(k, "SKY_") && !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		msgs := make([]string, 0, len(unknown))
		for _, u := range unknown {
			msgs = append(msgs, fmt.Sprintf("%s (did you mean %s?)", u, nearestName(u, known)))
		}
		return nil, fmt.Errorf("unknown environment variable(s): %s", strings.Join(msgs, ", "))
	}

	for _, b := range bindings {
		if v, ok := env[b.name]; ok {
			if err := b.apply(cfg, v); err != nil {
				return nil, fmt.Errorf("%s: %w", b.name, err)
			}
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func configPath(env map[string]string) string {
	if path := env[envConfigVar]; path != "" {
		return path
	}
	return "/config/config.yaml"
}

func environMap(environ []string) map[string]string {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// mergeReload pins the cold fields to their running values and reports which of them
// the operator changed. Source, Notifier and DB captured those at construction, so
// publishing the new values would make the live config claim what is not in effect.
func mergeReload(running, next *Config) (*Config, []string) {
	merged := *next
	var changed []string
	check := func(key string, different bool) {
		if different {
			changed = append(changed, key)
		}
	}

	check("source.url", next.Source.URL != running.Source.URL)
	check("source.max_age", next.Source.MaxAge != running.Source.MaxAge)
	check("ntfy.url", next.Ntfy.URL != running.Ntfy.URL)
	check("ntfy.topic", next.Ntfy.Topic != running.Ntfy.Topic)
	check("ntfy.token", next.Ntfy.Token != running.Ntfy.Token)
	check("ntfy.user", next.Ntfy.User != running.Ntfy.User)
	check("ntfy.password", next.Ntfy.Password != running.Ntfy.Password)
	check("tar1090_url", next.Tar1090URL != running.Tar1090URL)
	check("db.files", !reflect.DeepEqual(next.DB.Files, running.DB.Files))
	check("db.base_url", next.DB.BaseURL != running.DB.BaseURL)
	check("cache_dir", next.CacheDir != running.CacheDir)
	check("listen", next.Listen != running.Listen)

	merged.Source.URL = running.Source.URL
	merged.Source.MaxAge = running.Source.MaxAge
	merged.Ntfy.URL = running.Ntfy.URL
	merged.Ntfy.Topic = running.Ntfy.Topic
	merged.Ntfy.Token = running.Ntfy.Token
	merged.Ntfy.User = running.Ntfy.User
	merged.Ntfy.Password = running.Ntfy.Password
	merged.Tar1090URL = running.Tar1090URL
	merged.DB.Files = running.DB.Files
	merged.DB.BaseURL = running.DB.BaseURL
	merged.CacheDir = running.CacheDir
	merged.Listen = running.Listen
	return &merged, changed
}

func reloadConfig(live *Live, environ []string, onReload func(*Config)) ([]string, error) {
	next, err := LoadConfig(environ)
	if err != nil {
		return nil, err
	}
	published, changed := mergeReload(live.Get(), next)
	live.p.Store(published)
	if onReload != nil {
		onReload(published)
	}
	return changed, nil
}

// configWatcher notices edits to the config file. It compares a digest rather than
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
	w := &configWatcher{path: configPath(environMap(environ))}
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
func (w *configWatcher) run(ctx context.Context, live *Live, environ []string, interval time.Duration, onReload func(*Config)) {
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
		changed, err := reloadConfig(live, environ, onReload)
		if err != nil {
			slog.Error("config reload failed, keeping running config", "err", err)
			continue
		}
		slog.Info("config reloaded", "path", w.path)
		if len(changed) > 0 {
			slog.Warn("config change needs a restart", "keys", changed)
		}
	}
}

var dbFileRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.csv$`)

func (c *Config) validate() error {
	if c.Ntfy.Topic == "" {
		return fmt.Errorf("ntfy.topic is required (set SKY_NTFY_TOPIC)")
	}
	if len(c.Ntfy.Topic) > 64 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(c.Ntfy.Topic) {
		return fmt.Errorf("ntfy.topic must be 1-64 chars of [A-Za-z0-9_-]")
	}
	for _, u := range []struct{ name, val string }{
		{"ntfy.url", c.Ntfy.URL},
		{"db.base_url", c.DB.BaseURL},
	} {
		if err := checkHTTPURL(u.name, u.val); err != nil {
			return err
		}
	}
	if c.Tar1090URL != "" {
		if err := checkHTTPURL("tar1090_url", c.Tar1090URL); err != nil {
			return err
		}
	}
	if c.Ntfy.Priority < 1 || c.Ntfy.Priority > 5 {
		return fmt.Errorf("ntfy.priority must be 1..5, got %d", c.Ntfy.Priority)
	}
	for squawk, priority := range c.SquawkPriority {
		if _, ok := emergencySquawks[squawk]; !ok {
			return fmt.Errorf("squawk_priority: unknown squawk %q", squawk)
		}
		if priority < 0 || priority > 5 {
			return fmt.Errorf("squawk_priority.%s must be 0..5, got %d", squawk, priority)
		}
	}
	for i, rule := range c.Rules {
		if rule.Priority == nil {
			return fmt.Errorf("rule %d (%q): priority is required", i, rule.Name)
		}
		if *rule.Priority < 0 || *rule.Priority > 5 {
			return fmt.Errorf("rule %d (%q): priority must be 0..5, got %d", i, rule.Name, *rule.Priority)
		}
		if len(rule.ICAO)+len(rule.Reg)+len(rule.Operator)+len(rule.Type)+len(rule.ICAOType)+len(rule.CMPG)+len(rule.Category)+len(rule.Tags) == 0 {
			return fmt.Errorf("rule %d (%q): at least one match field is required", i, rule.Name)
		}
	}

	// source.url is either an http(s) URL or an absolute path. Anything else — a typo'd
	// scheme especially — must fail loudly rather than degrade into a path that never exists.
	if u, err := url.Parse(c.Source.URL); err == nil && u.Scheme != "" {
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("source.url: unsupported scheme %q (want http, https, or an absolute file path)", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("source.url: %q has no host", c.Source.URL)
		}
	} else if !filepath.IsAbs(c.Source.URL) {
		return fmt.Errorf("source.url must be an http(s) URL or an absolute file path, got %q", c.Source.URL)
	}

	if c.Ntfy.Token != "" && (c.Ntfy.User != "" || c.Ntfy.Password != "") {
		return fmt.Errorf("ntfy: token and user/password are mutually exclusive")
	}
	if (c.Ntfy.User == "") != (c.Ntfy.Password == "") {
		return fmt.Errorf("ntfy: user and password must be set together")
	}

	if len(c.DB.Files) == 0 {
		return fmt.Errorf("db.files must not be empty")
	}
	seen := map[string]bool{}
	for _, f := range c.DB.Files {
		if !dbFileRe.MatchString(f) {
			return fmt.Errorf("db.files: %q must be a plain CSV filename", f)
		}
		if seen[f] {
			return fmt.Errorf("db.files: duplicate entry %q", f)
		}
		seen[f] = true
	}

	for _, d := range []struct {
		name string
		val  Duration
	}{
		{"cooldown", c.Cooldown},
		{"source.poll_interval", c.Source.PollInterval},
		{"source.max_age", c.Source.MaxAge},
		{"db.refresh_interval", c.DB.RefreshInterval},
	} {
		if d.val.Std() <= 0 {
			return fmt.Errorf("%s must be > 0", d.name)
		}
	}

	// NaN and negative values would slip past every `> 0` check and silently disable
	// the filter the operator just asked for.
	if math.IsNaN(c.Filters.MaxDistanceNM) || math.IsInf(c.Filters.MaxDistanceNM, 0) || c.Filters.MaxDistanceNM < 0 {
		return fmt.Errorf("filters.max_distance_nm must be a non-negative number")
	}
	if c.Filters.MaxDistanceNM > 0 {
		if c.Filters.Lat == nil || c.Filters.Lon == nil {
			return fmt.Errorf("filters.max_distance_nm requires filters.lat and filters.lon")
		}
	}
	// Validate coordinates whenever they are set, not only when the filter is on.
	if c.Filters.Lat != nil && !(*c.Filters.Lat >= -90 && *c.Filters.Lat <= 90) {
		return fmt.Errorf("filters.lat must be in [-90,90]")
	}
	if c.Filters.Lon != nil && !(*c.Filters.Lon >= -180 && *c.Filters.Lon <= 180) {
		return fmt.Errorf("filters.lon must be in [-180,180]")
	}
	if c.Filters.MinAltitudeFt < 0 || c.Filters.MaxAltitudeFt < 0 {
		return fmt.Errorf("filters altitudes must not be negative")
	}
	if c.Filters.MaxAltitudeFt > 0 && c.Filters.MaxAltitudeFt <= c.Filters.MinAltitudeFt {
		return fmt.Errorf("filters.max_altitude_ft must exceed filters.min_altitude_ft")
	}
	return nil
}

func checkHTTPURL(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL, got %q", name, raw)
	}
	return nil
}

// nearestName returns the known variable with the smallest edit distance, so a typo
// gets a pointer instead of a wall of valid names.
func nearestName(s string, known map[string]bool) string {
	best, bestD := "", 1<<30
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if d := editDistance(s, k); d < bestD {
			best, bestD = k, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(min(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
