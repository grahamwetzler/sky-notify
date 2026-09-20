package main

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"
)

// exportDocument is the shape GET /api/config/export and -config-export produce, and
// POST /api/config/import and -config-import consume. Either section is optional: an
// alerts-only export/import round trip is the common case, and a payload missing a
// section leaves that section alone rather than forcing the other one along.
type exportDocument struct {
	ExportedAt       string  `json:"exported_at"`
	SkyNotifyVersion string  `json:"sky_notify_version,omitempty"`
	Config           *Config `json:"config,omitempty"`
	Alerts           *Alerts `json:"alerts,omitempty"`
	// Set only when redact=true asked for the corresponding section to have its
	// credentials blanked, in place of echoing them.
	NtfyTokenSet    *bool `json:"ntfy_token_set,omitempty"`
	NtfyPasswordSet *bool `json:"ntfy_password_set,omitempty"`
	AIKeySet        *bool `json:"ai_key_set,omitempty"`
}

// buildVersion is the module's build info, "" when it is not available (a plain `go
// build` without module info embedded, as in some test binaries).
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return ""
	}
	return info.Main.Version
}

// exportSettings reads the whole database as one document. Secrets are included by
// default — this is a backup/restore document, and the database file already holds them
// in plaintext — and blanked only when redact is requested, for pasting a config shape
// into an issue without the credentials.
func exportSettings(store *SettingsStore, redact bool) (*exportDocument, error) {
	doc := &exportDocument{ExportedAt: time.Now().UTC().Format(time.RFC3339), SkyNotifyVersion: buildVersion()}

	if data, _, ok, err := store.Get("config"); err != nil {
		return nil, err
	} else if ok {
		var cfg Config
		if err := json.Unmarshal([]byte(data), &cfg); err != nil {
			return nil, err
		}
		if redact {
			tokenSet, passwordSet := cfg.Ntfy.Token != "", cfg.Ntfy.Password != ""
			doc.NtfyTokenSet, doc.NtfyPasswordSet = &tokenSet, &passwordSet
			cfg.Ntfy.Token, cfg.Ntfy.Password = "", ""
		}
		doc.Config = &cfg
	}

	if data, _, ok, err := store.Get("alerts"); err != nil {
		return nil, err
	} else if ok {
		var alerts Alerts
		if err := json.Unmarshal([]byte(data), &alerts); err != nil {
			return nil, err
		}
		if redact {
			keySet := alerts.aiKey() != ""
			doc.AIKeySet = &keySet
			alerts.AI.Key = nil
		}
		doc.Alerts = &alerts
	}

	return doc, nil
}

// importSettings validates each section present in doc exactly as its own PUT handler
// does — the environment overlaid, then validated — and writes both in one transaction:
// a bad alerts section must not leave a good config section written from the same
// payload.
func importSettings(store *SettingsStore, doc *exportDocument, env map[string]string) error {
	writes := map[string]string{}

	if doc.Config != nil {
		overlaid := *doc.Config
		if err := applyConfigEnv(&overlaid, env); err != nil {
			return err
		}
		if err := overlaid.validate(); err != nil {
			return err
		}
		blob, err := json.Marshal(doc.Config)
		if err != nil {
			return err
		}
		writes["config"] = string(blob)
	}

	if doc.Alerts != nil {
		overlaid := *doc.Alerts
		if err := applyAlertEnv(&overlaid, env); err != nil {
			return err
		}
		if err := overlaid.validate(); err != nil {
			return err
		}
		blob, err := json.Marshal(doc.Alerts)
		if err != nil {
			return err
		}
		writes["alerts"] = string(blob)
	}

	if len(writes) == 0 {
		return fmt.Errorf("import document carries neither config nor alerts")
	}
	_, err := store.PutMany(writes)
	return err
}
