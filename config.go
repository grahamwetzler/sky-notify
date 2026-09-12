package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
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

func (d Duration) MarshalYAML() (any, error) { return durationString(d), nil }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(durationString(d)) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

func durationString(d Duration) string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "h0m0s") {
		return strings.TrimSuffix(s, "0m0s")
	}
	if strings.HasSuffix(s, "m0s") {
		return strings.TrimSuffix(s, "0s")
	}
	return s
}

type Rule struct {
	Name           string    `yaml:"name" json:"name"`
	Priority       *int      `yaml:"priority" json:"priority"`
	ICAO           []string  `yaml:"icao,omitempty" json:"icao,omitempty"`
	Reg            []string  `yaml:"reg,omitempty" json:"reg,omitempty"`
	ICAOType       []string  `yaml:"icao_type,omitempty" json:"icao_type,omitempty"`
	Squawk         []string  `yaml:"squawk,omitempty" json:"squawk,omitempty"`
	Operator       []string  `yaml:"operator,omitempty" json:"operator,omitempty"`
	Type           []string  `yaml:"type,omitempty" json:"type,omitempty"`
	CMPG           []string  `yaml:"cmpg,omitempty" json:"cmpg,omitempty"`
	Category       []string  `yaml:"category,omitempty" json:"category,omitempty"`
	Tags           []string  `yaml:"tags,omitempty" json:"tags,omitempty"`
	Listed         *bool     `yaml:"listed,omitempty" json:"listed,omitempty"`
	All            bool      `yaml:"all,omitempty" json:"all,omitempty"`
	MinAltitudeFt  *int      `yaml:"min_altitude_ft,omitempty" json:"min_altitude_ft,omitempty"`
	MaxAltitudeFt  *int      `yaml:"max_altitude_ft,omitempty" json:"max_altitude_ft,omitempty"`
	MaxDistanceNM  *float64  `yaml:"max_distance_nm,omitempty" json:"max_distance_nm,omitempty"`
	Circling       *bool     `yaml:"circling,omitempty" json:"circling,omitempty"`
	PassesWithinNM *float64  `yaml:"passes_within_nm,omitempty" json:"passes_within_nm,omitempty"`
	PassesWithin   *Duration `yaml:"passes_within,omitempty" json:"passes_within,omitempty"`
}

type Config struct {
	Source struct {
		URL    string   `yaml:"url"`
		MaxAge Duration `yaml:"max_age"`
	} `yaml:"source"`

	Ntfy struct {
		URL      string `yaml:"url"`
		Topic    string `yaml:"topic"`
		Token    string `yaml:"token"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
	} `yaml:"ntfy"`

	DB struct {
		Files   []string `yaml:"files"`
		BaseURL string   `yaml:"base_url"`
	} `yaml:"db"`

	CacheDir   string `yaml:"cache_dir"`
	Tar1090URL string `yaml:"tar1090_url"`
	Listen     string `yaml:"listen"`
}

type Alerts struct {
	Source struct {
		PollInterval Duration `yaml:"poll_interval" json:"poll_interval"`
	} `yaml:"source" json:"source"`
	Ntfy struct {
		Priority int `yaml:"priority" json:"priority"`
	} `yaml:"ntfy" json:"ntfy"`
	DB struct {
		RefreshInterval Duration `yaml:"refresh_interval" json:"refresh_interval"`
	} `yaml:"db" json:"db"`
	Cooldown Duration `yaml:"cooldown" json:"cooldown"`
	Lat      *float64 `yaml:"lat" json:"lat"`
	Lon      *float64 `yaml:"lon" json:"lon"`
	Rules    []Rule   `yaml:"rules" json:"rules"`
	LogLevel string   `yaml:"log_level" json:"log_level"`
}

const configPollInterval = 5 * time.Second

// Live holds the alerts the running loops read. The pointer is swapped wholesale and
// a published Alerts is never mutated, so readers need no lock.
type Live struct{ p atomic.Pointer[Alerts] }

func NewLive(c *Alerts) *Live {
	l := &Live{}
	l.p.Store(c)
	return l
}

func (l *Live) Get() *Alerts { return l.p.Load() }

// defaultConfig carries only values that are not statements about someone else's
// deployment. Where the aircraft feed lives, which ntfy server to post to, where the
// database is hosted and where the cache goes are all site facts: guessing them means
// a misconfigured instance quietly talks to the wrong host instead of refusing to start.
func defaultConfig() *Config {
	c := &Config{}
	c.Source.MaxAge = Duration(60 * time.Second)
	c.Listen = ":8080"
	return c
}

func defaultAlerts() *Alerts {
	a := &Alerts{}
	a.Source.PollInterval = Duration(15 * time.Second)
	a.Ntfy.Priority = 3
	a.DB.RefreshInterval = Duration(24 * time.Hour)
	a.Cooldown = Duration(24 * time.Hour)
	a.LogLevel = "info"
	return a
}

// envConfigVar names the YAML config path. It is bound separately from the table
// below (it is read before the file is parsed) but must still count as a known name.
const envConfigVar = "SKY_CONFIG"
const envAlertsConfigVar = "SKY_ALERTS_CONFIG"

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
		{"SKY_SOURCE_MAX_AGE", func(c *Config, v string) error { return setDur(&c.Source.MaxAge, v) }},

		{"SKY_NTFY_URL", func(c *Config, v string) error { c.Ntfy.URL = v; return nil }},
		{"SKY_NTFY_TOPIC", func(c *Config, v string) error { c.Ntfy.Topic = v; return nil }},
		{"SKY_NTFY_TOKEN", func(c *Config, v string) error { c.Ntfy.Token = v; return nil }},
		{"SKY_NTFY_USER", func(c *Config, v string) error { c.Ntfy.User = v; return nil }},
		{"SKY_NTFY_PASSWORD", func(c *Config, v string) error { c.Ntfy.Password = v; return nil }},

		{"SKY_DB_FILES", func(c *Config, v string) error { c.DB.Files = splitList(v); return nil }},
		{"SKY_DB_BASE_URL", func(c *Config, v string) error { c.DB.BaseURL = v; return nil }},

		{"SKY_CACHE_DIR", func(c *Config, v string) error { c.CacheDir = v; return nil }},
		{"SKY_TAR1090_URL", func(c *Config, v string) error { c.Tar1090URL = v; return nil }},
		{"SKY_LISTEN", func(c *Config, v string) error { c.Listen = v; return nil }},
	}
}

type alertEnvBinding struct {
	name  string
	key   string
	apply func(*Alerts, string) error
}

func alertEnvBindings() []alertEnvBinding {
	return []alertEnvBinding{
		{"SKY_SOURCE_POLL_INTERVAL", "source.poll_interval", func(a *Alerts, v string) error { return setDur(&a.Source.PollInterval, v) }},
		{"SKY_NTFY_PRIORITY", "ntfy.priority", func(a *Alerts, v string) error { return setInt(&a.Ntfy.Priority, v) }},
		{"SKY_DB_REFRESH_INTERVAL", "db.refresh_interval", func(a *Alerts, v string) error { return setDur(&a.DB.RefreshInterval, v) }},
		{"SKY_COOLDOWN", "cooldown", func(a *Alerts, v string) error { return setDur(&a.Cooldown, v) }},
		{"SKY_LAT", "lat", func(a *Alerts, v string) error { return setFloatPtr(&a.Lat, v) }},
		{"SKY_LON", "lon", func(a *Alerts, v string) error { return setFloatPtr(&a.Lon, v) }},
		{"SKY_LOG_LEVEL", "log_level", func(a *Alerts, v string) error { a.LogLevel = v; return nil }},
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

func setFloatPtr(f **float64, v string) error {
	p, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return err
	}
	*f = &p
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
	if err := loadYAML(path, cfg, configMisplaced, alertsPath(env), "hot-reloaded", nil); err != nil {
		return nil, err
	}
	if err := checkEnv(env); err != nil {
		return nil, err
	}
	for _, b := range envBindings() {
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

func LoadAlerts(environ []string) (*Alerts, error) {
	env := environMap(environ)
	alerts, err := loadAlertsFile(env)
	if err != nil {
		return nil, err
	}
	if err := checkEnv(env); err != nil {
		return nil, err
	}
	for _, b := range alertEnvBindings() {
		if v, ok := env[b.name]; ok {
			if err := b.apply(alerts, v); err != nil {
				return nil, fmt.Errorf("%s: %w", b.name, err)
			}
		}
	}
	if err := alerts.validate(); err != nil {
		return nil, err
	}
	return alerts, nil
}

func loadAlertsFile(env map[string]string) (*Alerts, error) {
	alerts := defaultAlerts()
	if err := loadYAML(alertsPath(env), alerts, alertsMisplaced, configPath(env), "startup-only", alertsRemoved); err != nil {
		return nil, err
	}
	return alerts, nil
}

// These mirror the other struct's keys; keep them in sync or migration errors fall
// back to KnownFields' unhelpful "field not found" message.
// Each list mirrors the other file's keys, and must be extended whenever a key is added
// to the other struct: a key missing here still fails, but with KnownFields' "field rules
// not found in type main.Config" instead of a message naming the file it belongs in.
var configMisplaced = []string{"rules", "cooldown", "lat", "lon", "log_level", "source.poll_interval", "ntfy.priority", "db.refresh_interval"}
var alertsMisplaced = []string{"source.url", "source.max_age", "ntfy.url", "ntfy.topic", "ntfy.token", "ntfy.user", "ntfy.password", "tar1090_url", "db.files", "db.base_url", "cache_dir", "listen"}

// A key or variable deleted by the rules-only rewrite gets an explanation of where its
// job went. Without these, KnownFields says "field filters not found in type main.Alerts"
// and nearestName offers a spelling correction for a name that is not misspelled — both
// of which read as "you typo'd" when the truth is "this setting moved".
var alertsRemoved = map[string]string{
	"filters":                   "filters moved onto each rule: min_altitude_ft, max_altitude_ft and max_distance_nm are now per-rule keys, and lat/lon are top-level in this file",
	"alert_on_emergency_squawk": `emergency squawks are now ordinary rules: write a rule with squawk: ["7500", "7600", "7700"]`,
	"squawk_priority":           "squawk priority is now the priority of the rule that matches the squawk",
}

var removedEnv = map[string]string{
	"SKY_FILTERS_MIN_ALTITUDE_FT":   "now a per-rule key in alerts.yaml",
	"SKY_FILTERS_MAX_ALTITUDE_FT":   "now a per-rule key in alerts.yaml",
	"SKY_FILTERS_MAX_DISTANCE_NM":   "now a per-rule key in alerts.yaml",
	"SKY_ALERT_ON_EMERGENCY_SQUAWK": "write a squawk rule instead",
	"SKY_FILTERS_LAT":               "use SKY_LAT instead",
	"SKY_FILTERS_LON":               "use SKY_LON instead",
}

func loadYAML(path string, dst any, misplaced []string, belongs, label string, removed map[string]string) error {
	// A missing config file is not an error; a malformed one is.
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	// The loose pass names the destination for moved keys; KnownFields alone cannot.
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	// Sorted, so a file carrying two removed keys always names the same one first.
	gone := make([]string, 0, len(removed))
	for key := range removed {
		if _, ok := raw[key]; ok {
			gone = append(gone, key)
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		return fmt.Errorf("config %s: %s: %s", path, gone[0], removed[gone[0]])
	}
	var found []string
	for _, key := range misplaced {
		parts := strings.Split(key, ".")
		_, ok := raw[parts[0]]
		if len(parts) == 2 {
			block, _ := raw[parts[0]].(map[string]any)
			_, ok = block[parts[1]]
		}
		if ok {
			found = append(found, key)
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("config %s: move %s to %s (%s) and restart", path, strings.Join(found, ", "), belongs, label)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

func checkEnv(env map[string]string) error {
	known := map[string]bool{envConfigVar: true, envAlertsConfigVar: true}
	for _, b := range envBindings() {
		known[b.name] = true
	}
	for _, b := range alertEnvBindings() {
		known[b.name] = true
	}
	var unknown []string
	for k := range env {
		if strings.HasPrefix(k, "SKY_") && !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	msgs := make([]string, 0, len(unknown))
	for _, u := range unknown {
		if message, ok := removedEnv[u]; ok {
			msgs = append(msgs, fmt.Sprintf("%s (%s)", u, message))
		} else {
			msgs = append(msgs, fmt.Sprintf("%s (did you mean %s?)", u, nearestName(u, known)))
		}
	}
	return fmt.Errorf("unknown environment variable(s): %s", strings.Join(msgs, ", "))
}

func configPath(env map[string]string) string {
	if path := env[envConfigVar]; path != "" {
		return path
	}
	return "/config/config.yaml"
}

func alertsPath(env map[string]string) string {
	if path := env[envAlertsConfigVar]; path != "" {
		return path
	}
	return filepath.Join(filepath.Dir(configPath(env)), "alerts.yaml")
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

var dbFileRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.csv$`)

func (c *Config) validate() error {
	for _, r := range []struct{ name, env, val string }{
		{"source.url", "SKY_SOURCE_URL", c.Source.URL},
		{"ntfy.url", "SKY_NTFY_URL", c.Ntfy.URL},
		{"ntfy.topic", "SKY_NTFY_TOPIC", c.Ntfy.Topic},
		{"db.base_url", "SKY_DB_BASE_URL", c.DB.BaseURL},
		{"cache_dir", "SKY_CACHE_DIR", c.CacheDir},
	} {
		if r.val == "" {
			return fmt.Errorf("%s is required (set %s)", r.name, r.env)
		}
	}
	if len(c.DB.Files) == 0 {
		return fmt.Errorf("db.files is required (set SKY_DB_FILES)")
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

	if c.Source.MaxAge.Std() <= 0 {
		return fmt.Errorf("source.max_age must be > 0")
	}
	return nil
}

func (a *Alerts) validate() error {
	if a.Ntfy.Priority < 1 || a.Ntfy.Priority > 5 {
		return fmt.Errorf("ntfy.priority must be 1..5, got %d", a.Ntfy.Priority)
	}
	seen := map[string]bool{}
	for i, rule := range a.Rules {
		if rule.Name == "" {
			return fmt.Errorf("rule %d (%q): name is required", i, rule.Name)
		}
		if seen[rule.Name] {
			return fmt.Errorf("rule %d (%q): name must be unique", i, rule.Name)
		}
		seen[rule.Name] = true
		if rule.Priority != nil && (*rule.Priority < 0 || *rule.Priority > 5) {
			return fmt.Errorf("rule %d (%q): priority must be 0..5, got %d", i, rule.Name, *rule.Priority)
		}
		if len(rule.ICAO)+len(rule.Reg)+len(rule.ICAOType)+len(rule.Squawk)+len(rule.Operator)+len(rule.Type)+len(rule.CMPG)+len(rule.Category)+len(rule.Tags) == 0 && !rule.All && rule.Listed == nil && rule.MinAltitudeFt == nil && rule.MaxAltitudeFt == nil && rule.MaxDistanceNM == nil && rule.Circling == nil && rule.PassesWithinNM == nil {
			return fmt.Errorf("rule %d (%q): at least one condition is required", i, rule.Name)
		}
		if (rule.MinAltitudeFt != nil && *rule.MinAltitudeFt < 0) || (rule.MaxAltitudeFt != nil && *rule.MaxAltitudeFt < 0) {
			return fmt.Errorf("rule %d (%q): altitude limits must not be negative", i, rule.Name)
		}
		if rule.MinAltitudeFt != nil && rule.MaxAltitudeFt != nil && *rule.MaxAltitudeFt <= *rule.MinAltitudeFt {
			return fmt.Errorf("rule %d (%q): max_altitude_ft must exceed min_altitude_ft", i, rule.Name)
		}
		for _, lim := range []struct {
			name string
			val  *float64
		}{{"max_distance_nm", rule.MaxDistanceNM}, {"passes_within_nm", rule.PassesWithinNM}} {
			if lim.val == nil {
				continue
			}
			if math.IsNaN(*lim.val) || math.IsInf(*lim.val, 0) || *lim.val <= 0 {
				return fmt.Errorf("rule %d (%q): %s must be a finite number > 0", i, rule.Name, lim.name)
			}
			if a.Lat == nil || a.Lon == nil {
				return fmt.Errorf("rule %d (%q): %s requires top-level lat and lon in alerts.yaml", i, rule.Name, lim.name)
			}
		}
		if rule.PassesWithin != nil {
			if rule.PassesWithinNM == nil {
				return fmt.Errorf("rule %d (%q): passes_within requires passes_within_nm", i, rule.Name)
			}
			if rule.PassesWithin.Std() <= 0 {
				return fmt.Errorf("rule %d (%q): passes_within must be > 0", i, rule.Name)
			}
		}
		// Samples further apart than maxSampleGap break the orbit, so slower polling would
		// leave a circling rule valid but unable ever to match. Half the gap leaves room
		// for one missed poll.
		if rule.Circling != nil && *rule.Circling && a.Source.PollInterval.Std() > maxSampleGap/2 {
			return fmt.Errorf("rule %d (%q): circling needs source.poll_interval of at most %s", i, rule.Name, maxSampleGap/2)
		}
	}
	for _, d := range []struct {
		name string
		val  Duration
	}{
		{"cooldown", a.Cooldown},
		{"source.poll_interval", a.Source.PollInterval},
		{"db.refresh_interval", a.DB.RefreshInterval},
	} {
		if d.val.Std() <= 0 {
			return fmt.Errorf("%s must be > 0", d.name)
		}
	}
	// Validate coordinates whenever they are set, not only when the filter is on.
	if a.Lat != nil && !(*a.Lat >= -90 && *a.Lat <= 90) {
		return fmt.Errorf("lat must be in [-90,90]")
	}
	if a.Lon != nil && !(*a.Lon >= -180 && *a.Lon <= 180) {
		return fmt.Errorf("lon must be in [-180,180]")
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
