package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// 60 KB is a typical tile, so this is an absurdity limit rather than a tuning knob.
	maxTileBytes     = 4 << 20
	tileFetchTimeout = 5 * time.Second
	tileParallel     = 4
	tileCacheTTL     = 30 * 24 * time.Hour
	tileJSONTTL      = 24 * time.Hour
	// How long one failure silences the tile fetcher. The notifier is a single
	// goroutine, so a tile outage without this would add the full render budget to
	// every alert — head-of-line delay in front of the next emergency.
	tileBreaker = 5 * time.Minute
)

// tileStore fetches vector tiles and keeps them on disk.
//
// The TileJSON endpoint is not the tile URL: OpenFreeMap's template carries the planet
// build it was generated from ("…/planet/20260906_080001_pt/{z}/{x}/{y}.pbf"), and the
// unversioned path answers 200 with an empty tile rather than failing. So the template is
// read at startup, re-read daily, and its version stamp becomes part of the cache path —
// without that, a refreshed template would keep serving tiles from a planet build the
// server has already replaced.
type tileStore struct {
	client   *http.Client
	tileJSON string
	dir      string

	mu        sync.Mutex
	template  string
	version   string
	read      time.Time
	downUntil time.Time
}

func newTileStore(client *http.Client, tileJSONURL, cacheDir string) *tileStore {
	return &tileStore{client: client, tileJSON: tileJSONURL, dir: filepath.Join(cacheDir, "tiles")}
}

// placedTile is one decoded tile and where it belongs.
type placedTile struct {
	x, y   int
	layers []layer
}

// tiles returns every tile the view covers. A tile that is absent — beyond the dataset,
// or over empty ocean — is not an error: the server answers 200 with nothing, and the
// canvas simply has nothing to draw there.
func (s *tileStore) tiles(ctx context.Context, v view) ([]placedTile, error) {
	if s.down() {
		return nil, fmt.Errorf("tiles: fetcher is in its failure backoff")
	}
	template, version, err := s.urlTemplate(ctx)
	if err != nil {
		s.markDown()
		return nil, err
	}

	x0, y0, x1, y1 := v.tileRange()
	type job struct{ x, y int }
	var jobs []job
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			jobs = append(jobs, job{x, y})
		}
	}

	out := make([]placedTile, len(jobs))
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, tileParallel)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			layers, err := s.tile(ctx, template, version, v.zoom, j.x, j.y)
			out[i], errs[i] = placedTile{j.x, j.y, layers}, err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			s.markDown()
			return nil, err
		}
	}
	return out, nil
}

func (s *tileStore) tile(ctx context.Context, template, version string, z, x, y int) ([]layer, error) {
	n := 1 << z
	x = ((x % n) + n) % n // the world repeats sideways
	path := filepath.Join(s.dir, version, fmt.Sprint(z), fmt.Sprint(x), fmt.Sprint(y)+".pbf")

	if b, ok := readCachedTile(path); ok {
		layers, err := decodeTile(ctx, b)
		if err == nil {
			return layers, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		// A cached file that will not decode is a truncated or scribbled-on download,
		// not a reason to give up: throw it away and ask again.
		slog.Debug("discarding corrupt cached tile", "path", path, "err", err)
		os.Remove(path)
	}

	url := strings.NewReplacer("{z}", fmt.Sprint(z), "{x}", fmt.Sprint(x), "{y}", fmt.Sprint(y)).Replace(template)
	b, err := s.get(ctx, url, tileFetchTimeout)
	if errors.Is(err, errNotFound) {
		// Either a tile that does not exist or a template naming a planet build the
		// server has retired. Dropping the template costs one extra request and fixes
		// the second case; an absent tile is not an error, the canvas just has nothing
		// to draw there.
		s.expireTemplate()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	writeCachedTile(path, b)
	return decodeTile(ctx, b)
}

func readCachedTile(path string) ([]byte, bool) {
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) > tileCacheTTL {
		return nil, false
	}
	b, err := os.ReadFile(path)
	return b, err == nil
}

// writeCachedTile is best effort and atomic: a half-written file left by a crash would
// otherwise be served as a corrupt tile until its TTL ran out.
func writeCachedTile(path string, b []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tile-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
	}
}

// urlTemplate returns the tile URL template and the dataset version it names.
func (s *tileStore) urlTemplate(ctx context.Context) (template, version string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.template != "" && time.Since(s.read) < tileJSONTTL {
		return s.template, s.version, nil
	}
	// ponytail: the fetch happens under the lock. The notifier is the only caller, so
	// there is nothing to contend with; split it if a second caller ever appears.
	// Not while holding s.mu on the 404 path: expireTemplate takes the same mutex, and
	// a sync.Mutex is not reentrant — that self-deadlock hung the alert, every alert
	// behind it, and shutdown, with no context able to break it.
	b, err := s.get(ctx, s.tileJSON, tileFetchTimeout)
	if errors.Is(err, errNotFound) {
		return "", "", fmt.Errorf("tiles: %s has no TileJSON (404); check map.tiles_url", s.tileJSON)
	}
	if err != nil {
		return "", "", err
	}
	var doc struct {
		Tiles []string `json:"tiles"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", "", fmt.Errorf("tiles: %s: %w", s.tileJSON, err)
	}
	if len(doc.Tiles) == 0 || !strings.Contains(doc.Tiles[0], "{z}") {
		return "", "", fmt.Errorf("tiles: %s returned no usable tile template", s.tileJSON)
	}
	s.template, s.version, s.read = doc.Tiles[0], templateVersion(doc.Tiles[0]), time.Now()
	slog.Debug("tile template", "url", s.template, "version", s.version)
	return s.template, s.version, nil
}

// errNotFound is a 404, which means different things to the two callers of get and so is
// decided by them rather than inside it. An absent tile is ordinary; an absent TileJSON
// is a misconfigured endpoint.
var errNotFound = errors.New("tiles: not found")

var unsafePathSegment = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// templateVersion pulls the dataset stamp out of a tile template: the path segment just
// before {z}. A server that does not version its tiles gets a constant, which is right —
// there is then nothing for the cache key to distinguish.
func templateVersion(template string) string {
	parts := strings.Split(template, "/")
	for i, p := range parts {
		if strings.Contains(p, "{z}") && i > 0 {
			if v := unsafePathSegment.ReplaceAllString(parts[i-1], "_"); v != "" && v != "." && v != ".." {
				return v
			}
		}
	}
	return "unversioned"
}

func (s *tileStore) get(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "sky-notify")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tiles: %s returned %s", url, resp.Status)
	}
	return readLimited(resp.Body, maxTileBytes)
}

func (s *tileStore) expireTemplate() {
	s.mu.Lock()
	s.template = ""
	s.mu.Unlock()
}

func (s *tileStore) down() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.downUntil)
}

func (s *tileStore) markDown() {
	s.mu.Lock()
	s.downUntil = time.Now().Add(tileBreaker)
	s.mu.Unlock()
}

// prune drops tiles past their TTL, and with them whole directories left behind by a
// dataset version the server has replaced. Called once at startup: a fixed receiver only
// ever touches a few hundred tiles, so there is nothing here that needs watching.
//
// ponytail: unbounded between prunes; add a size cap if a moving receiver ever appears.
func (s *tileStore) prune() {
	cutoff := time.Now().Add(-tileCacheTTL)
	filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().Before(cutoff) {
			os.Remove(path)
		}
		return nil
	})
	// Second pass: a directory is only empty once its files are gone.
	for range 4 { // z / x / version / tiles
		filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && path != s.dir {
				os.Remove(path) // fails unless empty, which is the test
			}
			return nil
		})
	}
}
