package main

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
)

var dbFileRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.csv$`)

// mapEnabled reports whether notifications should carry a map. Absent means on.
func (c *Config) mapEnabled() bool { return c.Map.Enabled == nil || *c.Map.Enabled }

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
	if c.mapEnabled() {
		if err := checkHTTPURL("map.tiles_url", c.Map.TilesURL); err != nil {
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
	// Names are optional, but a cooldown key that repeats is a bug worth failing on: two
	// rules sharing one silence each other. Checking the key rather than the name covers
	// all three ways a key can repeat — two identical unnamed rules, two rules sharing a
	// name, and a rule named after the fingerprint the UI showed for an unnamed one.
	seen := map[string]bool{}
	for i, rule := range a.Rules {
		key := rule.Key()
		if seen[key] {
			return fmt.Errorf("rule %d (%s): its cooldown key %q is already taken by an earlier rule — two rules sharing one would silence each other", i, rule.label(), key)
		}
		seen[key] = true
		if rule.Priority != nil && (*rule.Priority < 0 || *rule.Priority > 5) {
			return fmt.Errorf("rule %d (%s): priority must be 0..5, got %d", i, rule.label(), *rule.Priority)
		}
		switch rule.Notify {
		case "", notifyOnSight:
		case notifyClosestPass:
			// Without a receiver there is no distance to be closest to, so the rule
			// could never decide the aircraft had passed.
			if a.Lat == nil || a.Lon == nil {
				return fmt.Errorf("rule %d (%s): notify: %s requires top-level lat and lon in alerts.yaml", i, rule.label(), notifyClosestPass)
			}
		default:
			return fmt.Errorf("rule %d (%s): notify must be %s or %s, got %q", i, rule.label(), notifyOnSight, notifyClosestPass, rule.Notify)
		}
		if rule.All != nil {
			return fmt.Errorf("rule %d (%s): all is no longer a key — a rule with no conditions already matches every aircraft, so delete it", i, rule.label())
		}
		if (rule.MinAltitudeFt != nil && *rule.MinAltitudeFt < 0) || (rule.MaxAltitudeFt != nil && *rule.MaxAltitudeFt < 0) {
			return fmt.Errorf("rule %d (%s): altitudes must not be negative", i, rule.label())
		}
		if rule.MinAltitudeFt != nil && rule.MaxAltitudeFt != nil && *rule.MaxAltitudeFt <= *rule.MinAltitudeFt {
			return fmt.Errorf("rule %d (%s): max_altitude_ft must exceed min_altitude_ft", i, rule.label())
		}
		for _, lim := range []struct {
			name string
			val  *float64
		}{{"max_distance_nm", rule.MaxDistanceNM}, {"passes_within_nm", rule.PassesWithinNM}} {
			if lim.val == nil {
				continue
			}
			if math.IsNaN(*lim.val) || math.IsInf(*lim.val, 0) || *lim.val <= 0 {
				return fmt.Errorf("rule %d (%s): %s must be a finite number > 0", i, rule.label(), lim.name)
			}
			if a.Lat == nil || a.Lon == nil {
				return fmt.Errorf("rule %d (%s): %s requires top-level lat and lon in alerts.yaml", i, rule.label(), lim.name)
			}
		}
		if rule.PassesWithin != nil {
			if rule.PassesWithinNM == nil {
				return fmt.Errorf("rule %d (%s): passes_within requires passes_within_nm", i, rule.label())
			}
			if rule.PassesWithin.Std() <= 0 {
				return fmt.Errorf("rule %d (%s): passes_within must be > 0", i, rule.label())
			}
		}
		// Samples further apart than maxSampleGap break the orbit, so slower polling would
		// leave a circling rule valid but unable ever to match. Half the gap leaves room
		// for one missed poll.
		if rule.Circling != nil && *rule.Circling && a.Source.PollInterval.Std() > maxSampleGap/2 {
			return fmt.Errorf("rule %d (%s): circling needs source.poll_interval of at most %s", i, rule.label(), maxSampleGap/2)
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
