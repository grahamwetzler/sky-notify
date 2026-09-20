package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"
)

// exportDocument is the shape GET /api/config/export and -config-export produce.
type exportDocument struct {
	ExportedAt       string  `json:"exported_at"`
	SkyNotifyVersion string  `json:"sky_notify_version,omitempty"`
	Config           *Config `json:"config,omitempty"`
	Alerts           *Alerts `json:"alerts,omitempty"`
	// Set only when redact=true asked for the corresponding section to have its
	// credentials blanked, in place of echoing them.
	NtfyTokenSet    *bool `json:"ntfy_token_set,omitempty"`
	NtfyPasswordSet *bool `json:"ntfy_password_set,omitempty"`
	NtfyTopicSet    *bool `json:"ntfy_topic_set,omitempty"`
	AIKeySet        *bool `json:"ai_key_set,omitempty"`
}

// importDocument is the shape POST /api/config/import and -config-import consume. It
// accepts exactly what exportSettings produces — the same optional fields, in the same
// positions — but keeps config and alerts as raw JSON rather than decoded structs: each
// section is decoded onto its own defaults in importSettings, exactly as its PUT handler
// decodes onto defaultConfig()/defaultAlerts(), so a payload that omits a field gets the
// same default a PUT would give it rather than that field's zero value. Either section is
// optional: an alerts-only import (the common case) leaves config untouched.
type importDocument struct {
	ExportedAt       string          `json:"exported_at"`
	SkyNotifyVersion string          `json:"sky_notify_version,omitempty"`
	Config           json.RawMessage `json:"config,omitempty"`
	Alerts           json.RawMessage `json:"alerts,omitempty"`
	NtfyTokenSet     *bool           `json:"ntfy_token_set,omitempty"`
	NtfyPasswordSet  *bool           `json:"ntfy_password_set,omitempty"`
	NtfyTopicSet     *bool           `json:"ntfy_topic_set,omitempty"`
	AIKeySet         *bool           `json:"ai_key_set,omitempty"`
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
//
// Both sections are read in one GetSections call rather than two separate store.Get
// calls, so a concurrent import cannot commit between them and leave this document
// pairing a config and an alerts section that never coexisted in the database.
func exportSettings(store *SettingsStore, redact bool) (*exportDocument, error) {
	doc := &exportDocument{ExportedAt: time.Now().UTC().Format(time.RFC3339), SkyNotifyVersion: buildVersion()}

	rows, err := store.GetSections("config", "alerts")
	if err != nil {
		return nil, err
	}

	if data, ok := rows["config"]; ok {
		var cfg Config
		if err := json.Unmarshal([]byte(data), &cfg); err != nil {
			return nil, err
		}
		if redact {
			tokenSet, passwordSet, topicSet := cfg.ntfyToken() != "", cfg.ntfyPassword() != "", cfg.ntfyTopic() != ""
			doc.NtfyTokenSet, doc.NtfyPasswordSet, doc.NtfyTopicSet = &tokenSet, &passwordSet, &topicSet
			cfg.Ntfy.Token, cfg.Ntfy.Password, cfg.Ntfy.Topic = nil, nil, nil
		}
		doc.Config = &cfg
	}

	if data, ok := rows["alerts"]; ok {
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
// does — decoded onto defaults, the environment overlaid, then validated — and writes
// both in one transaction: a bad alerts section must not leave a good config section
// written from the same payload.
//
// Each section also passes the same credential-destination guard its PUT handler does
// (checkNtfyCredsStayPut, checkAIKeyStaysPut): import is another way to move ntfy.url or
// ai.url, and a credential the payload does not carry must not follow it there either.
// isJSONNull reports whether raw is the JSON literal null (whitespace aside) rather than
// an absent field. json.RawMessage cannot tell "the key was omitted" (nil, len 0) apart
// from "the key was sent as null" on its own: both must read as "section absent" here, or
// decoding null onto defaultConfig()/defaultAlerts() leaves those defaults untouched and
// an explicit `"alerts":null` silently overwrites the stored section with them.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func importSettings(store *SettingsStore, doc *importDocument, env map[string]string) error {
	writes := map[string]string{}

	if len(doc.Config) > 0 && !isJSONNull(doc.Config) {
		cfg := defaultConfig()
		dec := json.NewDecoder(bytes.NewReader(doc.Config))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		onDisk, err := loadConfigFile(store)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if err := checkNtfyCredsStayPut(onDisk, cfg, env); err != nil {
			return err
		}
		if cfg.Ntfy.Token == nil {
			cfg.Ntfy.Token = onDisk.Ntfy.Token
		} else if *cfg.Ntfy.Token == "" {
			cfg.Ntfy.Token = nil
		}
		if cfg.Ntfy.Password == nil {
			cfg.Ntfy.Password = onDisk.Ntfy.Password
		} else if *cfg.Ntfy.Password == "" {
			cfg.Ntfy.Password = nil
		}
		if cfg.Ntfy.Topic == nil {
			cfg.Ntfy.Topic = onDisk.Ntfy.Topic
		} else if *cfg.Ntfy.Topic == "" {
			cfg.Ntfy.Topic = nil
		}
		overlaid := *cfg
		if err := applyConfigEnv(&overlaid, env); err != nil {
			return err
		}
		if err := overlaid.validate(); err != nil {
			return err
		}
		blob, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		writes["config"] = string(blob)
	}

	if len(doc.Alerts) > 0 && !isJSONNull(doc.Alerts) {
		alerts := defaultAlerts()
		dec := json.NewDecoder(bytes.NewReader(doc.Alerts))
		dec.DisallowUnknownFields()
		if err := dec.Decode(alerts); err != nil {
			return fmt.Errorf("alerts: %w", err)
		}
		onDisk, err := loadAlertsFile(store)
		if err != nil {
			return fmt.Errorf("alerts: %w", err)
		}
		if err := checkAIKeyStaysPut(onDisk, alerts, env); err != nil {
			return err
		}
		switch {
		case aiKeyEndpoint(alerts, env) == "":
			alerts.AI.Key = nil
		case alerts.AI.Key == nil:
			alerts.AI.Key = onDisk.AI.Key
		case *alerts.AI.Key == "":
			alerts.AI.Key = nil
		}
		overlaid := *alerts
		if err := applyAlertEnv(&overlaid, env); err != nil {
			return err
		}
		if err := overlaid.validate(); err != nil {
			return err
		}
		blob, err := json.Marshal(alerts)
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
