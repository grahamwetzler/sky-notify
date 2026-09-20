package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Duration is a time.Duration that unmarshals from a Go duration string ("15s", "24h")
// rather than from an integer count of nanoseconds.
type Duration time.Duration

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

type Config struct {
	Source struct {
		URL    string   `yaml:"url" json:"url"`
		MaxAge Duration `yaml:"max_age" json:"max_age"`
	} `yaml:"source" json:"source"`

	Ntfy struct {
		URL   string `yaml:"url" json:"url"`
		Topic string `yaml:"topic" json:"topic"`
		// Token and Password are pointers for the same three-way reason AI.Key is: GET
		// /api/config never echoes them, so a PUT/import that omits one must mean "keep
		// the stored value", not "clear it" — only an explicit empty string means that.
		Token    *string `yaml:"token,omitempty" json:"token,omitempty"`
		User     string  `yaml:"user" json:"user"`
		Password *string `yaml:"password,omitempty" json:"password,omitempty"`
	} `yaml:"ntfy" json:"ntfy"`

	DB struct {
		Files   []string `yaml:"files" json:"files"`
		BaseURL string   `yaml:"base_url" json:"base_url"`
	} `yaml:"db" json:"db"`

	// Map is the snapshot attached to each notification. Startup-only, like the rest of
	// Config: the renderer's tile client is built once.
	Map struct {
		// Enabled is a pointer so "map.enabled: false" in the file can be told apart
		// from the key being absent, which is what makes the default true.
		Enabled  *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
		TilesURL string `yaml:"tiles_url" json:"tiles_url"`
	} `yaml:"map" json:"map"`

	CacheDir   string `yaml:"cache_dir" json:"cache_dir"`
	Tar1090URL string `yaml:"tar1090_url" json:"tar1090_url"`
	Listen     string `yaml:"listen" json:"listen"`
	// RouteAPIURL is the flight-route lookup asked about a callsign before a rule's
	// research question goes to the model, and only then. Empty asks nobody.
	RouteAPIURL string `yaml:"route_api_url" json:"route_api_url"`
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
	// AI is the provider that answers a rule with research: true. Hot-reloaded with the
	// rest of this file, so a model can be swapped without a restart.
	//
	// Key is write-only to the browser: GET /api/alerts blanks it and reports only
	// whether one is set, and a PUT that sends none keeps the one on disk. It is the one
	// value in this file that is a credential, and it is handled like a password field.
	AI struct {
		// URL is a base, like ntfy.url: "/chat/completions" is appended. Every
		// OpenAI-compatible provider is reachable that way.
		URL string `yaml:"url,omitempty" json:"url"`
		// Key is a pointer so the three things a save can mean stay distinguishable:
		// absent is "keep the stored one" (the page was never sent it), an empty string
		// is "clear it", and a value replaces it.
		Key     *string  `yaml:"key,omitempty" json:"key,omitempty"`
		Model   string   `yaml:"model,omitempty" json:"model"`
		Timeout Duration `yaml:"timeout" json:"timeout"`
	} `yaml:"ai" json:"ai"`

	Cooldown Duration `yaml:"cooldown" json:"cooldown"`
	Lat      *float64 `yaml:"lat,omitempty" json:"lat"`
	Lon      *float64 `yaml:"lon,omitempty" json:"lon"`
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
	// Unlike the other endpoints, this one is not a statement about someone else's
	// deployment: OpenFreeMap is a free public service with no key and no account, so a
	// default here misconfigures nothing. Point it at a self-hosted planet to change that.
	c.Map.TilesURL = "https://tiles.openfreemap.org/planet"
	// Same reasoning, and the same service tar1090 defaults to: a public lookup with no
	// key and no account. Nothing reaches it unless a rule asks for research.
	c.RouteAPIURL = defaultRouteAPIURL
	return c
}

func defaultAlerts() *Alerts {
	a := &Alerts{}
	a.Source.PollInterval = Duration(15 * time.Second)
	a.Ntfy.Priority = 3
	a.DB.RefreshInterval = Duration(24 * time.Hour)
	a.Cooldown = Duration(24 * time.Hour)
	// Only how long we will wait for a provider. URL, key and model stay empty — unset
	// means research is off, and guessing a provider is the same mistake as guessing an
	// ntfy server.
	a.AI.Timeout = Duration(20 * time.Second)
	a.LogLevel = "info"
	return a
}

// envDBPathVar names the settings database. It is bound separately from the table below
// (it is read before the store is opened) but must still count as a known name.
const envDBPathVar = "SKY_CONFIG_DB"
const defaultDBPath = "/config/config.db"

type envBinding struct {
	name  string
	key   string
	apply func(*Config, string) error
}

// envBindings is an explicit table rather than a reflection walk: it is fewer lines
// than the walker would be, and it is the one greppable place that answers "what can
// this service be configured with". key is the config path GET /api/config reports a
// locked field under, mirroring alertEnvBinding's key.
func envBindings() []envBinding {
	return []envBinding{
		{"SKY_SOURCE_URL", "source.url", func(c *Config, v string) error { c.Source.URL = v; return nil }},
		{"SKY_SOURCE_MAX_AGE", "source.max_age", func(c *Config, v string) error { return setDur(&c.Source.MaxAge, v) }},

		{"SKY_NTFY_URL", "ntfy.url", func(c *Config, v string) error { c.Ntfy.URL = v; return nil }},
		{"SKY_NTFY_TOPIC", "ntfy.topic", func(c *Config, v string) error { c.Ntfy.Topic = v; return nil }},
		{"SKY_NTFY_TOKEN", "ntfy.token", func(c *Config, v string) error { c.Ntfy.Token = &v; return nil }},
		{"SKY_NTFY_USER", "ntfy.user", func(c *Config, v string) error { c.Ntfy.User = v; return nil }},
		{"SKY_NTFY_PASSWORD", "ntfy.password", func(c *Config, v string) error { c.Ntfy.Password = &v; return nil }},

		{"SKY_DB_FILES", "db.files", func(c *Config, v string) error { c.DB.Files = splitList(v); return nil }},
		{"SKY_DB_BASE_URL", "db.base_url", func(c *Config, v string) error { c.DB.BaseURL = v; return nil }},

		{"SKY_MAP_ENABLED", "map.enabled", func(c *Config, v string) error { return setBoolPtr(&c.Map.Enabled, v) }},
		{"SKY_MAP_TILES_URL", "map.tiles_url", func(c *Config, v string) error { c.Map.TilesURL = v; return nil }},

		{"SKY_CACHE_DIR", "cache_dir", func(c *Config, v string) error { c.CacheDir = v; return nil }},
		{"SKY_TAR1090_URL", "tar1090_url", func(c *Config, v string) error { c.Tar1090URL = v; return nil }},
		{"SKY_ROUTE_API_URL", "route_api_url", func(c *Config, v string) error { c.RouteAPIURL = v; return nil }},
		{"SKY_LISTEN", "listen", func(c *Config, v string) error { c.Listen = v; return nil }},
	}
}

// The three variables the guard below reasons about by name, the config-side mirror of
// aiKeyEnv/aiURLEnv.
const (
	ntfyURLEnv      = "SKY_NTFY_URL"
	ntfyTokenEnv    = "SKY_NTFY_TOKEN"
	ntfyPasswordEnv = "SKY_NTFY_PASSWORD"
)

// ntfyEndpoint is the endpoint a document's stored ntfy credential belongs to — the
// environment's when it names one, since applyConfigEnv overwrites whatever the payload
// sent. Mirrors aiKeyEndpoint.
func ntfyEndpoint(c *Config, env map[string]string) string {
	if v := env[ntfyURLEnv]; v != "" {
		return v
	}
	return c.Ntfy.URL
}

// ntfyReach is every endpoint a document's ntfy credential can be sent to. Mirrors
// aiKeyReach for the same reason: the run posts to ntfyEndpoint, and the document's own URL
// is where the credential goes the moment SKY_NTFY_URL is taken away again.
func ntfyReach(c *Config, env map[string]string) []string {
	return []string{ntfyEndpoint(c, env), c.Ntfy.URL}
}

// checkNtfyCredsStayPut refuses a save that would send a stored ntfy token or password it
// did not supply to an endpoint it did — the config-side mirror of checkAIKeyStaysPut. A
// token and a password are mutually exclusive (validate() enforces it), so both are judged
// as one credential here: either one being carried silently to a new endpoint is the same
// disclosure.
func checkNtfyCredsStayPut(onDisk, saved *Config, env map[string]string) error {
	was := ntfyReach(onDisk, env)
	for _, now := range ntfyReach(saved, env) {
		if now == "" || slices.ContainsFunc(was, func(w string) bool {
			return w != "" && aiOrigin(w) == aiOrigin(now)
		}) {
			continue
		}
		if err := refuseNtfyCredsMove(onDisk, saved, env, now); err != nil {
			return err
		}
	}
	return nil
}

// refuseNtfyCredsMove says why a credential cannot follow ntfy.url to now, or nil when
// there is no credential at stake and the move costs nothing. Mirrors refuseAIKeyMove.
//
// Token and password are judged independently rather than as one unit: validate() makes
// them mutually exclusive, so at most one is ever stored, but the save could supply
// either field. Treating "the save touched either field" as proof for both would let a
// save that only clears the untouched one (e.g. `"password":""` alongside a stored
// token) carry the real, untouched credential to the new endpoint for free.
func refuseNtfyCredsMove(onDisk, saved *Config, env map[string]string, now string) error {
	if env[ntfyTokenEnv] != "" || env[ntfyPasswordEnv] != "" {
		return fmt.Errorf("ntfy.url: %s or %s holds a credential, so the endpoint it is sent to is not editable here — set %s to the endpoint you want", ntfyTokenEnv, ntfyPasswordEnv, ntfyURLEnv)
	}
	if onDisk.ntfyToken() != "" && saved.Ntfy.Token == nil {
		return fmt.Errorf("ntfy.url: the stored token was issued for %s and is never sent to this page, so it cannot follow the endpoint to %s — enter the token for the new endpoint, or clear it", aiOrigin(ntfyEndpoint(onDisk, env)), aiOrigin(now))
	}
	if onDisk.ntfyPassword() != "" && saved.Ntfy.Password == nil {
		return fmt.Errorf("ntfy.url: the stored password was issued for %s and is never sent to this page, so it cannot follow the endpoint to %s — enter the password for the new endpoint, or clear it", aiOrigin(ntfyEndpoint(onDisk, env)), aiOrigin(now))
	}
	return nil
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
		{"SKY_AI_URL", "ai.url", func(a *Alerts, v string) error { a.AI.URL = v; return nil }},
		{"SKY_AI_KEY", "ai.key", func(a *Alerts, v string) error { a.AI.Key = &v; return nil }},
		{"SKY_AI_MODEL", "ai.model", func(a *Alerts, v string) error { a.AI.Model = v; return nil }},
		{"SKY_AI_TIMEOUT", "ai.timeout", func(a *Alerts, v string) error { return setDur(&a.AI.Timeout, v) }},
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

func setBoolPtr(b **bool, v string) error {
	p, err := strconv.ParseBool(v)
	if err != nil {
		return err
	}
	*b = &p
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

// LoadConfig applies defaults, then the stored document (if present), then the
// environment. environ is passed in rather than read from os so tests can drive it.
func LoadConfig(environ []string, store *SettingsStore) (*Config, error) {
	env := environMap(environ)
	cfg, err := loadConfigFile(store)
	if err != nil {
		return nil, err
	}
	if err := checkEnv(env); err != nil {
		return nil, err
	}
	if err := applyConfigEnv(cfg, env); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyConfigEnv lays the environment over a loaded document, the config-side mirror of
// applyAlertEnv. GET /api/config and PUT /api/config validate through this too, for the
// same reason: what the service runs is the document plus the overrides.
func applyConfigEnv(c *Config, env map[string]string) error {
	for _, b := range envBindings() {
		if v, ok := env[b.name]; ok {
			if err := b.apply(c, v); err != nil {
				return fmt.Errorf("%s: %w", b.name, err)
			}
		}
	}
	return nil
}

func loadConfigFile(store *SettingsStore) (*Config, error) {
	cfg := defaultConfig()
	if err := loadJSON("config", cfg, configMisplaced, "alerts", "hot-reloaded", nil, store); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadAlerts(environ []string, store *SettingsStore) (*Alerts, error) {
	env := environMap(environ)
	alerts, err := loadAlertsFile(store)
	if err != nil {
		return nil, err
	}
	if err := checkEnv(env); err != nil {
		return nil, err
	}
	if err := applyAlertEnv(alerts, env); err != nil {
		return nil, err
	}
	if err := alerts.validate(); err != nil {
		return nil, err
	}
	return alerts, nil
}

// applyAlertEnv lays the environment over a loaded file. The UI validates through this
// too: what the service runs is the file plus the overrides, so validating the file alone
// would reject a save that is only valid because of an override, and accept one the
// watcher refuses the moment the override is applied.
func applyAlertEnv(a *Alerts, env map[string]string) error {
	for _, b := range alertEnvBindings() {
		if v, ok := env[b.name]; ok {
			if err := b.apply(a, v); err != nil {
				return fmt.Errorf("%s: %w", b.name, err)
			}
		}
	}
	return nil
}

func loadAlertsFile(store *SettingsStore) (*Alerts, error) {
	alerts := defaultAlerts()
	if err := loadJSON("alerts", alerts, alertsMisplaced, "config", "startup-only", alertsRemoved, store); err != nil {
		return nil, err
	}
	return alerts, nil
}

// These mirror the other struct's keys; keep them in sync or migration errors fall
// back to DisallowUnknownFields' unhelpful "field not found" message.
// Each list mirrors the other section's keys, and must be extended whenever a key is
// added to the other struct: a key missing here still fails, but with
// DisallowUnknownFields' "unknown field \"rules\"" instead of a message naming the
// section it belongs in.
var configMisplaced = []string{"rules", "cooldown", "lat", "lon", "log_level", "source.poll_interval", "ntfy.priority", "db.refresh_interval", "ai.url", "ai.key", "ai.model", "ai.timeout"}
var alertsMisplaced = []string{"source.url", "source.max_age", "ntfy.url", "ntfy.topic", "ntfy.token", "ntfy.user", "ntfy.password", "tar1090_url", "db.files", "db.base_url", "cache_dir", "listen"}

// A key or variable deleted by the rules-only rewrite gets an explanation of where its
// job went. Without these, DisallowUnknownFields says `unknown field "filters"` and
// nearestName offers a spelling correction for a name that is not misspelled — both of
// which read as "you typo'd" when the truth is "this setting moved".
var alertsRemoved = map[string]string{
	"filters":                   "filters moved onto each rule: min_altitude_ft, max_altitude_ft and max_distance_nm are now per-rule keys, and lat/lon are top-level in alerts",
	"alert_on_emergency_squawk": `emergency squawks are now ordinary rules: write a rule with squawk: ["7500", "7600", "7700"]`,
	"squawk_priority":           "squawk priority is now the priority of the rule that matches the squawk",
}

var removedEnv = map[string]string{
	"SKY_FILTERS_MIN_ALTITUDE_FT":   "now a per-rule key in alerts",
	"SKY_FILTERS_MAX_ALTITUDE_FT":   "now a per-rule key in alerts",
	"SKY_FILTERS_MAX_DISTANCE_NM":   "now a per-rule key in alerts",
	"SKY_ALERT_ON_EMERGENCY_SQUAWK": "write a squawk rule instead",
	"SKY_FILTERS_LAT":               "use SKY_LAT instead",
	"SKY_FILTERS_LON":               "use SKY_LON instead",
}

// loadJSON reads section's stored document from store into dst, applying the same
// removed-key and misplaced-key checks loadYAML used to apply to a file. section names
// the row this reads ("config" or "alerts"); belongs names the other one, for a
// misplaced-key error.
func loadJSON(section string, dst any, misplaced []string, belongs, label string, removed map[string]string, store *SettingsStore) error {
	// A section with no stored row is not an error; a malformed one is.
	data, _, ok, err := store.Get(section)
	if err != nil {
		return fmt.Errorf("%s: %w", section, err)
	}
	if !ok {
		return nil
	}
	b := []byte(data)
	// The loose pass names the destination for moved keys; DisallowUnknownFields alone
	// cannot.
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("%s: %w", section, err)
	}
	// Sorted, so a document carrying two removed keys always names the same one first.
	gone := make([]string, 0, len(removed))
	for key := range removed {
		if _, ok := raw[key]; ok {
			gone = append(gone, key)
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		return fmt.Errorf("%s: %s: %s", section, gone[0], removed[gone[0]])
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
		return fmt.Errorf("%s: move %s to %s (%s) and restart", section, strings.Join(found, ", "), belongs, label)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", section, err)
	}
	return nil
}

func checkEnv(env map[string]string) error {
	known := map[string]bool{envDBPathVar: true}
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

// dbPath is where the settings database lives — the one thing that has to be knowable
// before any config is loaded, since the loaded documents cannot name their own location.
func dbPath(env map[string]string) string {
	if path := env[envDBPathVar]; path != "" {
		return path
	}
	return defaultDBPath
}

// checkLegacyYAML refuses to start on an empty settings database when config.yaml or
// alerts.yaml sit beside it: that combination means a deployment upgrading from a version
// that read those files directly, not a fresh one, and loadJSON's "missing row" case would
// otherwise start it on defaults — silently disabling every rule — without saying why.
//
// A store that already has either row is not this case, whatever files remain nearby: the
// database is what gets read now, so a leftover YAML file changes nothing and is not worth
// refusing to start over.
func checkLegacyYAML(store *SettingsStore, dbFile string) error {
	dir := filepath.Dir(dbFile)
	var found []string
	for _, name := range []string{"config.yaml", "alerts.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			found = append(found, filepath.Join(dir, name))
		}
	}
	if len(found) == 0 {
		return nil
	}
	_, _, configOK, err := store.Get("config")
	if err != nil {
		return err
	}
	_, _, alertsOK, err := store.Get("alerts")
	if err != nil {
		return err
	}
	if configOK || alertsOK {
		return nil
	}
	return fmt.Errorf(
		"found %s from a version that read YAML config directly; sky-notify now reads %s and does not read these files — "+
			"convert them to the config.example.json shape and load with `sky-notify -config-import <file>.json`, then remove them",
		strings.Join(found, " and "), dbFile)
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
