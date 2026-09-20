package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------- CSV parsing ----------

func TestParseCSVStripsHeaderPrefixesAndMapsByName(t *testing.T) {
	rows := mustParse(t, sampleCSV)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	got := rows[0]
	if got.ICAO != "adeb2f" {
		t.Errorf("ICAO: want lowercased adeb2f, got %q", got.ICAO)
	}
	if got.Operator != "US Air Force" || got.Type != "C-17" || got.Category != "Zoomies" {
		t.Errorf("field mapping wrong: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Cargo" || got.Tags[1] != "Heavy" {
		t.Errorf("tags: want [Cargo Heavy], got %v", got.Tags)
	}
}

// Upstream files are genuinely ragged: plane-alert-pia.csv declares 14 columns and
// most rows carry 2.
func TestParseCSVToleratesRaggedRows(t *testing.T) {
	in := `$ICAO,$Registration,$Operator,$Type,$ICAO Type,#CMPG,$Tag 1,$#Tag 2,$#Tag 3,Category,$#Link,#ImageLink
0000C8,N917BC
A038BC,N82123
`
	rows := mustParse(t, in)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows from ragged input, got %d", len(rows))
	}
	if rows[0].ICAO != "0000c8" || rows[0].Reg != "N917BC" || rows[0].Operator != "" {
		t.Errorf("short row not zero-filled correctly: %+v", rows[0])
	}
}

// C03121 upstream puts a real Category in the Tag 3 slot, shifting its link into
// Category while keeping the declared field count, so the ragged-row path never
// sees it. Left alone, the URL shows up as something to filter on.
func TestParseCSVMovesAShiftedLinkOutOfCategory(t *testing.T) {
	in := `$ICAO,$Registration,$Operator,$Type,$ICAO Type,#CMPG,$Tag 1,$#Tag 2,$#Tag 3,Category,$#Link
C03121,C-FSPS,Saskatoon Board of Police Commissioners,Cessna 182T Skylane,C182,Pol,Police Squad,The Cops,Police Forces,https://tc.gc.ca/ADet.aspx?id=531479,
`
	got := mustParse(t, in)[0]
	if got.Category != "" {
		t.Errorf("category: want empty, got %q", got.Category)
	}
	if got.Link != "https://tc.gc.ca/ADet.aspx?id=531479" {
		t.Errorf("link: want the shifted URL, got %q", got.Link)
	}
	if len(got.Tags) != 3 {
		t.Errorf("tags: want the three intact, got %v", got.Tags)
	}
}

func TestParseCSVRejectsMissingICAOHeader(t *testing.T) {
	if _, _, err := parseCSV([]byte("$Registration,$Operator\nN1,Someone\n")); err == nil {
		t.Fatal("want error when the ICAO column is absent")
	}
}

func TestParseCSVSkipsUnusableICAOs(t *testing.T) {
	in := "$ICAO,$Registration\nNOTHEX,N1\nabc,N2\nadeb2f,N3\n"
	rows, skipped, err := parseCSV([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || skipped != 2 {
		t.Fatalf("want 1 row / 2 skipped, got %d / %d", len(rows), skipped)
	}
}

// ---------- db refresh ----------

type csvServer struct {
	body   string
	etag   string
	status int
	hits   int
}

func (c *csvServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.hits++
		if c.status != 0 {
			w.WriteHeader(c.status)
			return
		}
		if c.etag != "" {
			if r.Header.Get("If-None-Match") == c.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", c.etag)
		}
		fmt.Fprint(w, c.body)
	}
}

func newDBServer(t *testing.T, cfg *Config, c *csvServer) *DB {
	t.Helper()
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	cfg.DB.BaseURL = srv.URL
	return NewDB(cfg, srv.Client())
}

func TestRefreshCommitsAndPersists(t *testing.T) {
	cfg := testConfig(t)
	c := &csvServer{body: sampleCSV, etag: `"v1"`}
	db := newDBServer(t, cfg, c)

	if err := db.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rows, _ := db.Stats(); rows != 2 {
		t.Fatalf("want 2 rows, got %d", rows)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, "db.json")); err != nil {
		t.Fatalf("snapshot should be persisted: %v", err)
	}

	// A second cache-hit load must reuse the file rather than refetching.
	db2 := NewDB(cfg, http.DefaultClient)
	if !db2.Load() {
		t.Fatal("Load should accept a complete snapshot")
	}
	if _, ok := db2.Lookup("adeb2f"); !ok {
		t.Error("merged map should be derived on load")
	}
}

func TestRefreshHonours304AndReusesStoredRows(t *testing.T) {
	cfg := testConfig(t)
	c := &csvServer{body: sampleCSV, etag: `"v1"`}
	db := newDBServer(t, cfg, c)
	ctx := context.Background()

	if err := db.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Refresh(ctx); err != nil {
		t.Fatalf("304 cycle should succeed: %v", err)
	}
	if rows, _ := db.Stats(); rows != 2 {
		t.Fatalf("rows should survive a 304, got %d", rows)
	}
	if c.hits != 2 {
		t.Errorf("want 2 requests, got %d", c.hits)
	}
}

func TestRefreshFailureRetainsPreviousSnapshot(t *testing.T) {
	cfg := testConfig(t)
	c := &csvServer{body: sampleCSV}
	db := newDBServer(t, cfg, c)
	ctx := context.Background()
	if err := db.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(cfg.CacheDir, "db.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []csvServer{
		{body: "$Registration\nN1\n"},            // no ICAO column
		{body: "$ICAO\nNOTHEX\n"},                // zero usable rows
		{status: http.StatusInternalServerError}, // upstream error
	} {
		*c = bad
		if err := db.Refresh(ctx); err == nil {
			t.Errorf("bad refresh should fail: %+v", bad)
		}
		if rows, _ := db.Stats(); rows != 2 {
			t.Errorf("previous snapshot must be retained, got %d rows", rows)
		}
	}
	after, _ := os.ReadFile(filepath.Join(cfg.CacheDir, "db.json"))
	if string(before) != string(after) {
		t.Error("a failed refresh must not rewrite the on-disk snapshot")
	}
}

// One bad one-row response must not install itself and then be preserved by every
// subsequent 304 — but a real upstream cleanup must not freeze the cache forever either.
func TestDrasticShrinkNeedsTwoAgreeingRefreshes(t *testing.T) {
	cfg := testConfig(t)
	big := "$ICAO,$Registration\n"
	for i := 0; i < 20; i++ {
		big += fmt.Sprintf("%06x,N%d\n", i, i)
	}
	c := &csvServer{body: big}
	db := newDBServer(t, cfg, c)
	ctx := context.Background()
	if err := db.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := db.Stats(); rows != 20 {
		t.Fatalf("want 20 rows, got %d", rows)
	}

	c.body = "$ICAO,$Registration\n000001,N1\n"
	if err := db.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := db.Stats(); rows != 20 {
		t.Fatalf("first drastic shrink must be held, got %d rows", rows)
	}
	if err := db.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, _ := db.Stats(); rows != 1 {
		t.Fatalf("a shrink confirmed by a second refresh must be accepted, got %d rows", rows)
	}
}

// An ETag identifies a resource URL, not a filename.
func TestChangedBaseURLDiscardsCachedETags(t *testing.T) {
	cfg := testConfig(t)
	c := &csvServer{body: sampleCSV, etag: `"v1"`}
	db := newDBServer(t, cfg, c)
	if err := db.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	other := &csvServer{body: "$ICAO,$Registration\n000009,N9\n", etag: `"v1"`}
	srv2 := httptest.NewServer(other.handler())
	defer srv2.Close()
	cfg.DB.BaseURL = srv2.URL

	if err := db.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Lookup("adeb2f"); ok {
		t.Error("rows from the previous base URL must not survive the switch")
	}
	if _, ok := db.Lookup("000009"); !ok {
		t.Error("rows from the new base URL should be loaded")
	}

	// A cache written for a different base URL must not be loadable either.
	db2 := NewDB(cfg, http.DefaultClient)
	cfg.DB.BaseURL = "https://example.invalid/other"
	if db2.Load() {
		t.Error("Load must reject a snapshot whose base_url differs from the config")
	}
}

// String concatenation onto a base URL carrying a query would fetch the wrong path.
func TestFetchJoinsPathOntoBaseURLWithQuery(t *testing.T) {
	cfg := testConfig(t)
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		fmt.Fprint(w, sampleCSV)
	}))
	defer srv.Close()

	cfg.DB.BaseURL = srv.URL + "/raw?token=abc"
	db := NewDB(cfg, srv.Client())
	if err := db.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/raw/plane-alert-db.csv" {
		t.Errorf("path: got %q, want /raw/plane-alert-db.csv", gotPath)
	}
	if gotQuery != "token=abc" {
		t.Errorf("query should be preserved, got %q", gotQuery)
	}
}

func TestLoadRejectsPartialCache(t *testing.T) {
	cfg := testConfig(t)
	cfg.DB.Files = []string{"a.csv", "b.csv"}
	blob, _ := json.Marshal(&snapshot{
		BaseURL: cfg.DB.BaseURL,
		Files:   map[string]*fileEntry{"a.csv": {Rows: mustParse(t, sampleCSV)}},
	})
	os.WriteFile(filepath.Join(cfg.CacheDir, "db.json"), blob, 0o644)

	if NewDB(cfg, http.DefaultClient).Load() {
		t.Fatal("a cache missing a configured file must not count as a usable start")
	}
}

func TestMergePrecedenceFirstFileWins(t *testing.T) {
	cfg := testConfig(t)
	cfg.DB.Files = []string{"first.csv", "second.csv"}
	db := NewDB(cfg, http.DefaultClient)
	db.commit(&snapshot{BaseURL: cfg.DB.BaseURL, Files: map[string]*fileEntry{
		"first.csv":  {Rows: []Plane{{ICAO: "adeb2f", Operator: "First"}}},
		"second.csv": {Rows: []Plane{{ICAO: "adeb2f", Operator: "Second"}}},
	}}, time.Now())

	p, ok := db.Lookup("adeb2f")
	if !ok || p.Operator != "First" {
		t.Fatalf("first configured file should win, got %+v", p)
	}
}
