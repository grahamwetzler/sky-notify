package app

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// SettingsStore holds the two config documents — startup config and hot-reloaded
// alerts — as JSON blobs in config.db, modeled directly on History's use of SQLite.
type SettingsStore struct{ db *sql.DB }

// OpenSettings opens (or creates) the settings database at path. WAL and a busy
// timeout for the same reason History uses them: a read must not block a write, and a
// write must not fail outright if it overlaps one.
func OpenSettings(path string) (*SettingsStore, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS settings (
			section    TEXT PRIMARY KEY CHECK (section IN ('config', 'alerts')),
			data       TEXT NOT NULL,
			version    INTEGER NOT NULL DEFAULT 1,
			updated_at INTEGER NOT NULL
		);
		-- The web UI's password, one row once the operator has chosen. No row means
		-- nobody has been asked yet; an empty hash means they chose to go without.
		CREATE TABLE IF NOT EXISTS ui_auth (
			id            INTEGER PRIMARY KEY CHECK (id = 1),
			password_hash TEXT NOT NULL,
			updated_at    INTEGER NOT NULL
		);
		-- The key session cookies are signed under, made once and kept so a restart
		-- does not sign everyone out.
		CREATE TABLE IF NOT EXISTS session_secret (
			id     INTEGER PRIMARY KEY CHECK (id = 1),
			secret BLOB NOT NULL
		);
	`); err != nil {
		db.Close()
		return nil, err
	}
	return &SettingsStore{db: db}, nil
}

func (s *SettingsStore) Close() {
	if s != nil {
		s.db.Close()
	}
}

// Get returns a section's stored document and version. ok is false when the section has
// never been written — the same "file didn't exist" case loadYAML handled by reading a
// missing path, not an error.
func (s *SettingsStore) Get(section string) (data string, version int64, ok bool, err error) {
	err = s.db.QueryRow(`SELECT data, version FROM settings WHERE section = ?`, section).Scan(&data, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return data, version, true, nil
}

// upsertSectionSQL inserts a section's document or, if the section already has a row,
// updates it in place and bumps its version. RETURNING hands back the version in the same
// statement so a caller never needs a second query to learn what it now is.
const upsertSectionSQL = `
	INSERT INTO settings (section, data, version, updated_at) VALUES (?, ?, 1, ?)
	ON CONFLICT (section) DO UPDATE SET data = excluded.data, version = version + 1, updated_at = excluded.updated_at
	RETURNING version
`

// Put upserts a section's document and returns the version it now has, so a caller that
// needs to report what changed (the API handlers) does not have to read it back.
//
// The upsert and the version read are one statement, via RETURNING, rather than an Exec
// followed by a separate QueryRow: two statements would let a concurrent Put on the same
// section commit in between them, so both callers could read back the same version.
func (s *SettingsStore) Put(section, data string) (version int64, err error) {
	err = s.db.QueryRow(upsertSectionSQL, section, data, time.Now().UnixMilli()).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// GetSections returns several sections' documents in one read transaction, so a caller
// that builds one artifact from more than one row (an export) cannot observe a write that
// commits between two separate reads — a config row and an alerts row that never coexisted.
// A section with no stored row is simply absent from the result, the same "missing is
// fine" contract Get has.
func (s *SettingsStore) GetSections(sections ...string) (map[string]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	out := make(map[string]string, len(sections))
	for _, section := range sections {
		var data string
		err := tx.QueryRow(`SELECT data FROM settings WHERE section = ?`, section).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[section] = data
	}
	return out, tx.Commit()
}

// PutMany upserts several sections in one transaction, so an import that touches both
// documents cannot leave one written and the other rejected.
func (s *SettingsStore) PutMany(docs map[string]string) (versions map[string]int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UnixMilli()
	versions = make(map[string]int64, len(docs))
	for section, data := range docs {
		var v int64
		if err := tx.QueryRow(upsertSectionSQL, section, data, now).Scan(&v); err != nil {
			return nil, err
		}
		versions[section] = v
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return versions, nil
}

// UIPassword returns the stored password hash, and whether the operator has made a choice
// at all: chosen with an empty hash is "no password", which is a different thing from
// never having been asked.
func (s *SettingsStore) UIPassword() (hash string, chosen bool, err error) {
	err = s.db.QueryRow(`SELECT password_hash FROM ui_auth WHERE id = 1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// SetUIPassword records the operator's choice; an empty hash means no password.
func (s *SettingsStore) SetUIPassword(hash string) error {
	_, err := s.db.Exec(`
		INSERT INTO ui_auth (id, password_hash, updated_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET password_hash = excluded.password_hash, updated_at = excluded.updated_at
	`, hash, time.Now().UnixMilli())
	return err
}

// ClearUIPassword forgets the operator's choice, so the web UI asks again as it did on
// first run.
func (s *SettingsStore) ClearUIPassword() error {
	_, err := s.db.Exec(`DELETE FROM ui_auth`)
	return err
}

// SessionSecret returns the cookie-signing secret, creating it on first use. INSERT OR
// IGNORE then a read, so two first callers agree on one secret.
func (s *SettingsStore) SessionSecret() ([]byte, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO session_secret (id, secret) VALUES (1, ?)`, rand.Text()); err != nil {
		return nil, err
	}
	var secret []byte
	if err := s.db.QueryRow(`SELECT secret FROM session_secret WHERE id = 1`).Scan(&secret); err != nil {
		return nil, err
	}
	return secret, nil
}
