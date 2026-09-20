package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
