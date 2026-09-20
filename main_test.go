package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigCLIExportImportRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "config.db")
	seed := filepath.Join(t.TempDir(), "seed.json")
	seedDoc := `{
		"config": {
			"source": {"url": "http://feeder.lan/data/aircraft.json", "max_age": "60s"},
			"ntfy": {"url": "https://ntfy.sh", "topic": "test-topic"},
			"db": {"files": ["plane-alert-db.csv"], "base_url": "https://example.com/db"},
			"map": {"enabled": true, "tiles_url": "https://tiles.openfreemap.org/planet"},
			"cache_dir": "/data",
			"listen": ":8080"
		},
		"alerts": {
			"source": {"poll_interval": "15s"},
			"ntfy": {"priority": 3},
			"db": {"refresh_interval": "24h"},
			"ai": {"timeout": "20s"},
			"cooldown": "24h",
			"rules": [{"name": "test", "listed": true}],
			"log_level": "info"
		}
	}`
	if err := os.WriteFile(seed, []byte(seedDoc), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SKY_CONFIG_DB", dbPath)
	if err := runConfigCLI(false, seed, "", false); err != nil {
		t.Fatalf("-config-import: %v", err)
	}

	out := filepath.Join(t.TempDir(), "export.json")
	if err := runConfigCLI(true, "", out, false); err != nil {
		t.Fatalf("-config-export: %v", err)
	}
	blob, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc exportDocument
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Config == nil || doc.Config.Ntfy.Topic != "test-topic" {
		t.Fatalf("exported config = %+v", doc.Config)
	}
	if doc.Alerts == nil || len(doc.Alerts.Rules) != 1 || doc.Alerts.Rules[0].Name != "test" {
		t.Fatalf("exported alerts = %+v", doc.Alerts)
	}

	// Importing the exported document back into a fresh database round-trips cleanly.
	dbPath2 := filepath.Join(t.TempDir(), "config2.db")
	t.Setenv("SKY_CONFIG_DB", dbPath2)
	if err := runConfigCLI(false, out, "", false); err != nil {
		t.Fatalf("re-import of the export: %v", err)
	}
	store, err := OpenSettings(dbPath2)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alerts, err := LoadAlerts(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts.Rules) != 1 || alerts.Rules[0].Name != "test" {
		t.Fatalf("round-tripped alerts = %+v", alerts)
	}
}

func TestConfigCLIExportRedactsOnRequest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "config.db")
	t.Setenv("SKY_CONFIG_DB", dbPath)
	store, err := OpenSettings(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("alerts", `{"ai":{"url":"https://openrouter.ai/api/v1","key":"sk-or-v1-secret","model":"perplexity/sonar"}}`); err != nil {
		t.Fatal(err)
	}
	store.Close()

	out := filepath.Join(t.TempDir(), "export.json")
	if err := runConfigCLI(true, "", out, true); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(blob); !strings.Contains(got, `"ai_key_set": true`) || strings.Contains(got, "sk-or-v1-secret") {
		t.Fatalf("redacted export leaked the key or omitted the flag:\n%s", got)
	}
}

// An export carries plaintext credentials by default, so the file it writes must be
// owner-only — including when it overwrites a file that previously had broader
// permissions, since WriteFile alone would leave those in place.
func TestConfigCLIExportIsOwnerOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "config.db")
	t.Setenv("SKY_CONFIG_DB", dbPath)
	store, err := OpenSettings(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("alerts", `{"ai":{"key":"sk-or-v1-secret"}}`); err != nil {
		t.Fatal(err)
	}
	store.Close()

	out := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(out, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runConfigCLI(true, "", out, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("export file mode = %o, want 0600 (an existing 0644 file must be narrowed, not left as-is)", perm)
	}
}

// The export is written via a temp file renamed into place, not straight to out, so a
// reader never observes the destination at its old (possibly looser) permissions while
// the plaintext is being written. Confirm the helper leaves no temp file behind and the
// destination directory ends up holding exactly the export.
func TestConfigCLIExportLeavesNoTempFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "config.db")
	t.Setenv("SKY_CONFIG_DB", dbPath)
	store, err := OpenSettings(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("alerts", `{"ai":{"key":"sk-or-v1-secret"}}`); err != nil {
		t.Fatal(err)
	}
	store.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "export.json")
	if err := runConfigCLI(true, "", out, false); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "export.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("destination dir = %v, want only export.json (a leftover temp file would leak the export)", names)
	}
}

// An import that omits a field must get the same default a PUT would give it, not that
// field's zero value: importSettings decodes each section onto defaultAlerts()/
// defaultConfig(), exactly as its PUT handler decodes onto them.
func TestConfigCLIImportAppliesDefaultsToOmittedFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "config.db")
	t.Setenv("SKY_CONFIG_DB", dbPath)
	seed := filepath.Join(t.TempDir(), "sparse.json")
	if err := os.WriteFile(seed, []byte(`{"alerts":{"rules":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runConfigCLI(false, seed, "", false); err != nil {
		t.Fatalf("sparse import: %v", err)
	}
	store, err := OpenSettings(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alerts, err := LoadAlerts(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	if alerts.Ntfy.Priority != 3 {
		t.Errorf("ntfy.priority = %d, want the default 3, not the zero value", alerts.Ntfy.Priority)
	}
	if alerts.Cooldown.Std() != 24*time.Hour {
		t.Errorf("cooldown = %s, want the default 24h", alerts.Cooldown.Std())
	}
}

func TestConfigCLIImportRejectsInvalidWithoutWriting(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "config.db")
	t.Setenv("SKY_CONFIG_DB", dbPath)
	seed := filepath.Join(t.TempDir(), "bad.json")
	// A good config paired with an alerts section that fails validation: the import
	// must be all-or-nothing, so the good half must not land either.
	if err := os.WriteFile(seed, []byte(`{
		"config": {
			"source": {"url": "http://feeder.lan/data/aircraft.json", "max_age": "60s"},
			"ntfy": {"url": "https://ntfy.sh", "topic": "test-topic"},
			"db": {"files": ["plane-alert-db.csv"], "base_url": "https://example.com/db"},
			"cache_dir": "/data"
		},
		"alerts": {"ntfy": {"priority": 9}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runConfigCLI(false, seed, "", false); err == nil {
		t.Fatal("an invalid alerts section should refuse the whole import")
	}
	store, err := OpenSettings(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, ok, _ := store.Get("config"); ok {
		t.Fatal("a rejected import must not write the config section either")
	}
}
