package main

import (
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

// Put upserts a section's document and returns the version it now has, so a caller that
// needs to report what changed (the API handlers) does not have to read it back.
func (s *SettingsStore) Put(section, data string) (version int64, err error) {
	_, err = s.db.Exec(`
		INSERT INTO settings (section, data, version, updated_at) VALUES (?, ?, 1, ?)
		ON CONFLICT (section) DO UPDATE SET data = excluded.data, version = version + 1, updated_at = excluded.updated_at
	`, section, data, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	if err := s.db.QueryRow(`SELECT version FROM settings WHERE section = ?`, section).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
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
		if _, err := tx.Exec(`
			INSERT INTO settings (section, data, version, updated_at) VALUES (?, ?, 1, ?)
			ON CONFLICT (section) DO UPDATE SET data = excluded.data, version = version + 1, updated_at = excluded.updated_at
		`, section, data, now); err != nil {
			return nil, err
		}
		var v int64
		if err := tx.QueryRow(`SELECT version FROM settings WHERE section = ?`, section).Scan(&v); err != nil {
			return nil, err
		}
		versions[section] = v
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return versions, nil
}
