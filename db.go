package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxCSVBytes = 64 << 20

// Plane is one row of plane-alert-db.
type Plane struct {
	ICAO     string   `json:"icao"`
	Reg      string   `json:"reg,omitempty"`
	Operator string   `json:"operator,omitempty"`
	Type     string   `json:"type,omitempty"`
	ICAOType string   `json:"icao_type,omitempty"`
	CMPG     string   `json:"cmpg,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Category string   `json:"category,omitempty"`
	Link     string   `json:"link,omitempty"`
}

type fileEntry struct {
	ETag string  `json:"etag,omitempty"`
	Rows []Plane `json:"rows"`
}

// snapshot is the on-disk cache. Rows are kept per file rather than pre-merged so a
// 304 can actually be honoured ("reuse that file's rows" is unanswerable once rows
// have been flattened and duplicates shadowed), and base_url is recorded because an
// ETag identifies a resource URL, not a filename.
type snapshot struct {
	BaseURL string                `json:"base_url"`
	Files   map[string]*fileEntry `json:"files"`
}

type DB struct {
	cfg    *Config
	client *http.Client
	path   string

	mu      sync.RWMutex
	snap    *snapshot
	merged  map[string]*Plane
	refresh time.Time
	dupes   int
	// pendingShrink holds the content hash of a drastic row reduction awaiting
	// confirmation by a second identical refresh. In memory only: a restart
	// conservatively re-requires confirmation.
	pendingShrink map[string]string
}

func NewDB(cfg *Config, client *http.Client) *DB {
	return &DB{
		cfg:           cfg,
		client:        client,
		path:          filepath.Join(cfg.CacheDir, "db.json"),
		pendingShrink: map[string]string{},
	}
}

func (d *DB) Lookup(icao string) (*Plane, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.merged[icao]
	return p, ok
}

func (d *DB) Stats() (rows int, refreshed time.Time) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.merged), d.refresh
}

// Load reads the cached snapshot from disk. It reports whether the cache covers every
// configured file for the configured base URL — a partial cache is not a usable start.
func (d *DB) Load() bool {
	b, err := os.ReadFile(d.path)
	if err != nil {
		return false
	}
	var s snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		slog.Warn("cache unreadable, ignoring", "path", d.path, "err", err)
		return false
	}
	if s.BaseURL != d.cfg.DB.BaseURL {
		slog.Info("cached db base_url differs from config, discarding cache", "cached", s.BaseURL, "configured", d.cfg.DB.BaseURL)
		return false
	}
	for _, f := range d.cfg.DB.Files {
		e, ok := s.Files[f]
		if !ok || len(e.Rows) == 0 {
			slog.Info("cached db does not cover all configured files", "missing", f)
			return false
		}
	}
	d.commit(&s, time.Time{})
	rows, _ := d.Stats()
	slog.Info("loaded db from cache", "path", d.path, "rows", rows)
	return true
}

// commit derives the merged map and installs the snapshot. First file to define an
// ICAO wins, so db.files order is the precedence order.
func (d *DB) commit(s *snapshot, refreshed time.Time) {
	merged := make(map[string]*Plane, 20000)
	dupes := 0
	for _, f := range d.cfg.DB.Files {
		e, ok := s.Files[f]
		if !ok {
			continue
		}
		for i := range e.Rows {
			p := &e.Rows[i]
			if _, exists := merged[p.ICAO]; exists {
				dupes++
				continue
			}
			merged[p.ICAO] = p
		}
	}
	d.mu.Lock()
	d.snap, d.merged, d.dupes = s, merged, dupes
	if !refreshed.IsZero() {
		d.refresh = refreshed
	}
	d.mu.Unlock()
}

// Refresh runs one validate-then-commit cycle. Nothing is installed — in memory or on
// disk — unless every configured file yields usable rows.
func (d *DB) Refresh(ctx context.Context) error {
	d.mu.RLock()
	cur := d.snap
	d.mu.RUnlock()
	if cur != nil && cur.BaseURL != d.cfg.DB.BaseURL {
		cur = nil // base URL changed: cached ETags belong to a different resource
	}

	next := &snapshot{BaseURL: d.cfg.DB.BaseURL, Files: map[string]*fileEntry{}}

	for _, f := range d.cfg.DB.Files {
		var curETag string
		var curRows []Plane
		if cur != nil {
			if e, ok := cur.Files[f]; ok {
				curETag, curRows = e.ETag, e.Rows
			}
		}

		body, etag, notModified, err := d.fetch(ctx, f, curETag)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if notModified {
			if len(curRows) == 0 {
				return fmt.Errorf("%s: server returned 304 but no cached rows are loaded", f)
			}
			next.Files[f] = &fileEntry{ETag: curETag, Rows: curRows}
			continue
		}

		rows, skipped, err := parseCSV(body)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if len(rows) == 0 {
			return fmt.Errorf("%s: no usable rows", f)
		}
		if skipped > 0 {
			slog.Warn("skipped unusable csv rows", "file", f, "skipped", skipped, "kept", len(rows))
		}

		// A drastic shrink needs two consecutive refreshes to agree. Rejecting it
		// outright would freeze the cache forever after a legitimate upstream cleanup;
		// accepting it blindly lets one bad response install itself and then be
		// preserved by every subsequent 304.
		if len(curRows) > 0 && len(rows)*2 < len(curRows) {
			sum := sha256.Sum256(body)
			h := hex.EncodeToString(sum[:])
			d.mu.Lock()
			confirmed := d.pendingShrink[f] == h
			if !confirmed {
				d.pendingShrink[f] = h
			}
			d.mu.Unlock()
			if !confirmed {
				slog.Warn("db shrank drastically, holding for confirmation",
					"file", f, "was", len(curRows), "now", len(rows))
				next.Files[f] = &fileEntry{ETag: curETag, Rows: curRows}
				continue
			}
			slog.Warn("db shrink confirmed by a second refresh, accepting",
				"file", f, "was", len(curRows), "now", len(rows))
		}
		d.mu.Lock()
		delete(d.pendingShrink, f)
		d.mu.Unlock()

		next.Files[f] = &fileEntry{ETag: etag, Rows: rows}
	}

	blob, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := writeFileDurable(d.path, blob); err != nil {
		return fmt.Errorf("persist cache: %w", err)
	}
	d.commit(next, time.Now())
	rows, _ := d.Stats()
	slog.Info("db refreshed", "rows", rows, "duplicates_shadowed", d.dupes)
	return nil
}

func (d *DB) fetch(ctx context.Context, file, etag string) (body []byte, newETag string, notModified bool, err error) {
	// JoinPath rather than concatenation: a base URL carrying a query or fragment would
	// otherwise produce a wrong path, silently fetching the same bad data for every file.
	base, err := url.Parse(d.cfg.DB.BaseURL)
	if err != nil {
		return nil, "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath(file).String(), nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, etag, true, nil
	case http.StatusOK:
	default:
		return nil, "", false, fmt.Errorf("unexpected status %s", resp.Status)
	}

	b, err := readLimited(resp.Body, maxCSVBytes)
	if err != nil {
		return nil, "", false, err
	}
	return b, resp.Header.Get("ETag"), false, nil
}

// readLimited reads at most limit bytes and reports an error if the source had more,
// so a truncated document is never mistaken for a complete short one.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d byte limit", limit)
	}
	return b, nil
}

// parseCSV reads a plane-alert-db CSV. The upstream data is ragged (plane-alert-pia.csv
// declares 14 columns and most rows carry 2) and its headers carry planefence's $ and #
// tweet-formatting prefixes, which are not part of the field name.
func parseCSV(b []byte) (rows []Plane, skipped int, err error) {
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = true

	head, err := r.Read()
	if err != nil {
		return nil, 0, fmt.Errorf("read header: %w", err)
	}
	// Map by header name, not index, so an upstream column insertion does not
	// silently shift every field.
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(strings.TrimLeft(h, "$#"))] = i
	}
	if _, ok := col["ICAO"]; !ok {
		return nil, 0, errors.New("missing ICAO column")
	}
	at := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return "" // short row: zero-fill
		}
		return strings.TrimSpace(rec[i])
	}

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			skipped++
			continue
		}
		icao := normalizeHex(at(rec, "ICAO"))
		if !validICAO(icao) {
			skipped++
			continue
		}
		p := Plane{
			ICAO:     icao,
			Reg:      at(rec, "Registration"),
			Operator: at(rec, "Operator"),
			Type:     at(rec, "Type"),
			ICAOType: at(rec, "ICAO Type"),
			CMPG:     at(rec, "CMPG"),
			Category: at(rec, "Category"),
			Link:     at(rec, "Link"),
		}
		for _, t := range []string{at(rec, "Tag 1"), at(rec, "Tag 2"), at(rec, "Tag 3")} {
			if t != "" {
				p.Tags = append(p.Tags, t)
			}
		}
		rows = append(rows, p)
	}
	return rows, skipped, nil
}

func validICAO(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// writeFileDurable writes atomically and durably: write, sync the file, close, rename,
// then sync the parent directory. Syncing the directory before the rename would not
// make the rename durable.
func writeFileDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	df, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer df.Close()
	return df.Sync()
}
