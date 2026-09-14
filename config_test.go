package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------- config ----------

func loadWith(t *testing.T, yaml string, env ...string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if yaml != "" {
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return LoadConfig(append(append(requiredEnv(), "SKY_CONFIG="+path), env...))
}

func loadAlertsWith(t *testing.T, yaml string, env ...string) (*Alerts, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.yaml")
	if yaml != "" {
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return LoadAlerts(append([]string{"SKY_CONFIG=" + filepath.Join(dir, "config.yaml"), "SKY_ALERTS_CONFIG=" + path}, env...))
}

func TestConfigPrecedenceEnvOverYAMLOverDefault(t *testing.T) {
	y := "ntfy:\n  topic: from-yaml\n"

	cfg, err := loadWith(t, y)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ntfy.Topic != "from-yaml" {
		t.Errorf("yaml should override defaults, got %q", cfg.Ntfy.Topic)
	}

	cfg, err = loadWith(t, y, "SKY_NTFY_TOPIC=from-env")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ntfy.Topic != "from-env" {
		t.Errorf("env should win over yaml, got %q", cfg.Ntfy.Topic)
	}
}

func TestConfigReloadHotSwapTakesEffect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.yaml")
	environ := []string{"SKY_CONFIG=" + filepath.Join(dir, "config.yaml"), "SKY_ALERTS_CONFIG=" + path, "SKY_NTFY_TOPIC=test-topic"}
	write := func(priority int) {
		t.Helper()
		yaml := fmt.Sprintf("rules:\n  - name: test\n    icao: [adeb2f]\n    priority: %d\n", priority)
		if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(1)
	alerts, err := LoadAlerts(environ)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(alerts)
	cfg := testConfig(t)
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	write(5)
	if err := reloadConfig(live, environ, nil); err != nil {
		t.Fatal(err)
	}
	if a := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, live.Get(), nil); a == nil || a.Priority != 5 {
		t.Fatalf("reloaded alert = %+v, want priority 5", a)
	}
}

func TestInvalidConfigReloadIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.yaml")
	environ := []string{"SKY_ALERTS_CONFIG=" + path}
	if err := os.WriteFile(path, []byte("ntfy:\n  priority: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAlerts(environ)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(cfg)
	if err := os.WriteFile(path, []byte("ntfy: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reloadConfig(live, environ, nil); err == nil {
		t.Fatal("invalid reload should fail")
	}
	if live.Get() != cfg {
		t.Fatal("invalid reload replaced the running config")
	}
}

func TestConfigReloadEnvironmentStillWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.yaml")
	environ := []string{"SKY_ALERTS_CONFIG=" + path, "SKY_NTFY_PRIORITY=4"}
	if err := os.WriteFile(path, []byte("ntfy:\n  priority: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAlerts(environ)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(cfg)
	if err := os.WriteFile(path, []byte("ntfy:\n  priority: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reloadConfig(live, environ, nil); err != nil {
		t.Fatal(err)
	}
	if live.Get().Ntfy.Priority != 4 {
		t.Fatalf("priority = %d, want environment value 4", live.Get().Ntfy.Priority)
	}
}

func TestWatchConfigReloadsWhenMissingFileAppearsAndChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.yaml")
	environ := []string{"SKY_ALERTS_CONFIG=" + path}
	cfg, err := LoadAlerts(environ)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(cfg)
	// Baseline taken before the file is written, so the appearance is a real change.
	watcher := newConfigWatcher(environ)
	reloaded := make(chan struct{}, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.run(ctx, live, environ, 10*time.Millisecond, func(*Alerts) {
			select {
			case reloaded <- struct{}{}:
			default:
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	wait := func() {
		t.Helper()
		select {
		case <-reloaded:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for config reload")
		}
	}
	if err := os.WriteFile(path, []byte("ntfy:\n  priority: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wait()
	if live.Get().Ntfy.Priority != 2 {
		t.Fatalf("priority = %d after file appeared, want 2", live.Get().Ntfy.Priority)
	}
	if err := os.WriteFile(path, []byte("ntfy:\n  priority: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wait()
	if live.Get().Ntfy.Priority != 4 {
		t.Fatalf("priority = %d after file changed, want 4", live.Get().Ntfy.Priority)
	}
}

func TestConfigMissingFileIsFine(t *testing.T) {
	if _, err := LoadConfig(append(requiredEnv(), "SKY_CONFIG=/nonexistent/nope.yaml", "SKY_NTFY_TOPIC=t")); err != nil {
		t.Fatalf("a missing config file should not be an error: %v", err)
	}
}

func TestAlertsMissingFileIsFine(t *testing.T) {
	if _, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=/nonexistent/alerts.yaml"}); err != nil {
		t.Fatalf("a missing alerts file should not be an error: %v", err)
	}
}

func alertsHandler(t *testing.T, path string, planes ...*Plane) http.Handler {
	t.Helper()
	t.Setenv("SKY_ALERTS_CONFIG", path)
	cfg := testConfig(t)
	db := NewDB(cfg, http.DefaultClient)
	if len(planes) > 0 {
		db.merged = map[string]*Plane{}
		for _, p := range planes {
			db.merged[p.ICAO] = p
		}
	}
	return (&server{live: NewLive(defaultAlerts()), db: db}).mux(newQueue(1))
}

func TestAlertsUIRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	h := alertsHandler(t, path)
	want := defaultAlerts()
	want.Source.PollInterval = Duration(7 * time.Second)
	want.Ntfy.Priority = 4
	want.DB.RefreshInterval = Duration(2 * time.Hour)
	want.Cooldown = Duration(30 * time.Minute)
	lat, lon := 41.9, -87.6
	want.Lat, want.Lon = &lat, &lon
	minAlt, maxAlt, maxNM := 100, 40000, 25.5
	want.Rules = []Rule{{
		Name: "cargo", ICAO: []string{"abc123"}, Reg: []string{"N1"}, Operator: []string{"Operator"},
		Type: []string{"Type"}, ICAOType: []string{"C17"}, Squawk: []string{"7700"}, CMPG: []string{"Mil"},
		Category: []string{"Other"}, Tags: []string{"Cargo"}, Listed: boolp(true), Priority: intp(2),
		MinAltitudeFt: &minAlt, MaxAltitudeFt: &maxAlt, MaxDistanceNM: &maxNM,
	}}
	want.LogLevel = "debug"
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/alerts", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	got, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestAlertsUIGetReadsSavedFileImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(`{"rules":[{"name":"saved","icao":["abc123"],"priority":2}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	var body struct {
		Alerts Alerts `json:"alerts"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Alerts.Rules) != 1 || body.Alerts.Rules[0].Name != "saved" {
		t.Fatalf("GET returned stale rules: %+v", body.Alerts.Rules)
	}
}

func TestAlertsUIRejectsInvalidWithoutWriting(t *testing.T) {
	for _, body := range []string{
		`{"rules":[{"name":"nameless"},{"name":"nameless"}]}`,
		`{"ntfy":{"priority":9}}`,
	} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "alerts.yaml")
			h := alertsHandler(t, path)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid payload wrote file: %v", err)
			}
		})
	}
}

func TestAlertsUIRejectsUnknownField(t *testing.T) {
	h := alertsHandler(t, filepath.Join(t.TempDir(), "alerts.yaml"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(`{"nope":1}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestAlertsUIRejectsOversizedBody(t *testing.T) {
	h := alertsHandler(t, filepath.Join(t.TempDir(), "alerts.yaml"))
	w := httptest.NewRecorder()
	body := `{"log_level":"` + strings.Repeat("x", 1<<20) + `"}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "request body too large") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestAlertsUIBlankCoordinatesStayNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	body := `{"lat":null,"lon":null}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path})
	if err != nil || got.Lat != nil || got.Lon != nil {
		t.Fatalf("alerts = %+v, err = %v", got, err)
	}
}

func TestAlertsUILockedReflectsEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	for _, set := range []bool{true, false} {
		t.Run(fmt.Sprint(set), func(t *testing.T) {
			if set {
				t.Setenv("SKY_COOLDOWN", "2h")
			} else {
				old, present := os.LookupEnv("SKY_COOLDOWN")
				os.Unsetenv("SKY_COOLDOWN")
				t.Cleanup(func() {
					if present {
						os.Setenv("SKY_COOLDOWN", old)
					}
				})
			}
			h := alertsHandler(t, path)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
			var body struct {
				Locked []map[string]string `json:"locked"`
			}
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range body.Locked {
				found = found || item["key"] == "cooldown"
			}
			if found != set {
				t.Fatalf("locked = %v", body.Locked)
			}
		})
	}
}

func TestAlertsUIGetPreservesFileValueUnderEnvironmentLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	if err := os.WriteFile(path, []byte("cooldown: 24h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKY_COOLDOWN", "1h")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	var body struct {
		Alerts Alerts              `json:"alerts"`
		Locked []map[string]string `json:"locked"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	locked := false
	for _, item := range body.Locked {
		locked = locked || item["key"] == "cooldown" && item["env"] == "SKY_COOLDOWN"
	}
	if body.Alerts.Cooldown.Std() != 24*time.Hour || !locked {
		t.Fatalf("response = %+v", body)
	}
}

func TestAlertsUIGetDoesNotValidateBeforeEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	// max_distance_nm fails validation without lat/lon, and here they arrive from the
	// environment — so a GET that validated the file alone would 500 on a valid setup.
	if err := os.WriteFile(path, []byte("rules:\n  - name: near\n    max_distance_nm: 25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKY_LAT", "41.9")
	t.Setenv("SKY_LON", "-87.6")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d: %s", w.Code, w.Body.String())
	}
}

func TestAlertsUIUnknownPageIsNotFound(t *testing.T) {
	h := alertsHandler(t, filepath.Join(t.TempDir(), "alerts.yaml"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDurationMarshalKeepsYAMLReadable(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour:   "24h",
		5 * time.Minute:  "5m",
		90 * time.Second: "1m30s",
		90 * time.Minute: "1h30m",
		0:                "0s",
	} {
		got, err := (Duration(d)).MarshalYAML()
		if err != nil || got != want {
			t.Errorf("%s: got %q, err %v; want %q", d, got, err, want)
		}
	}
}

// preview drives POST /api/preview and returns the decoded response.
func previewRule(t *testing.T, h http.Handler, body string) (total, database int, sample []Plane) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/preview", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("preview status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Total    int     `json:"total"`
		Database int     `json:"database"`
		Aircraft []Plane `json:"aircraft"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got.Total, got.Database, got.Aircraft
}

func TestPreviewCountsAndCapsTheSample(t *testing.T) {
	// One more aircraft than the sample cap, so total and sample must disagree.
	var planes []*Plane
	for i := 0; i <= previewLimit; i++ {
		planes = append(planes, &Plane{ICAO: fmt.Sprintf("%06x", i), CMPG: "Mil", Tags: []string{"Cargo"}})
	}
	planes = append(planes, &Plane{ICAO: "ffffff", CMPG: "Civ", Tags: []string{"Airliner"}})
	h := alertsHandler(t, filepath.Join(t.TempDir(), "alerts.yaml"), planes...)

	// A rule with no conditions selects the whole database; only the sample is capped.
	total, database, sample := previewRule(t, h, `{"name":"all","priority":3}`)
	if total != len(planes) || database != len(planes) || len(sample) != previewLimit {
		t.Fatalf("total=%d database=%d sample=%d, want %d/%d/%d", total, database, len(sample), len(planes), len(planes), previewLimit)
	}
	// The sample is sorted, so it does not reshuffle between identical requests.
	for i := 1; i < len(sample); i++ {
		if sample[i-1].ICAO >= sample[i].ICAO {
			t.Fatalf("sample not sorted at %d: %q then %q", i, sample[i-1].ICAO, sample[i].ICAO)
		}
	}

	total, _, sample = previewRule(t, h, `{"name":"civ","priority":3,"cmpg":["Civ"]}`)
	if total != 1 || len(sample) != 1 || sample[0].ICAO != "ffffff" {
		t.Fatalf("cmpg filter: total=%d sample=%+v", total, sample)
	}
}

// The preview is only worth showing if it agrees with what will actually alert, so it
// must inherit the matcher's quirks -- here, that fields are ANDed together.
func TestPreviewMatchesTheAlertPath(t *testing.T) {
	gov := &Plane{ICAO: "adfdf8", CMPG: "Gov", Category: "Head of State", Tags: []string{"Air Force One"}}
	mil := &Plane{ICAO: "af83f3", CMPG: "Mil", Category: "USAF", Tags: []string{"Gunship"}}
	h := alertsHandler(t, filepath.Join(t.TempDir(), "alerts.yaml"), gov, mil)

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"tag alone", `{"name":"x","priority":5,"tags":["Air Force One"]}`, 1},
		{"tag and matching cmpg", `{"name":"x","priority":5,"tags":["Air Force One"],"cmpg":["Gov"]}`, 1},
		// Every condition must match, so one wrong field empties the whole rule.
		{"tag and conflicting cmpg", `{"name":"x","priority":5,"tags":["Air Force One"],"cmpg":["Mil"]}`, 0},
		{"unknown value", `{"name":"x","priority":5,"tags":["Air Force Onee"]}`, 0},
		{"case insensitive", `{"name":"x","priority":5,"cmpg":["mil"]}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total, _, _ := previewRule(t, h, tc.body)
			if total != tc.want {
				t.Fatalf("total = %d, want %d", total, tc.want)
			}
			// Whatever the preview counts, the alert path must agree.
			var rule Rule
			if err := json.Unmarshal([]byte(tc.body), &rule); err != nil {
				t.Fatal(err)
			}
			live := 0
			for _, p := range []*Plane{gov, mil} {
				ac := at(Aircraft{Hex: p.ICAO, Reg: p.Reg, Type: p.ICAOType})
				if firstMatch([]Rule{rule}, ac, p, &Alert{Hex: p.ICAO}) != nil {
					live++
				}
			}
			if live != total {
				t.Fatalf("preview says %d, firstMatch says %d", total, live)
			}
		})
	}
}

func facetValues(f []FacetValue) []string {
	out := make([]string, len(f))
	for i, v := range f {
		out[i] = v.Value
	}
	return out
}

func TestFacetsNarrowByTheOtherConditions(t *testing.T) {
	db := &DB{merged: map[string]*Plane{
		"a": {ICAO: "a", CMPG: "Gov", Category: "Head of State", Tags: []string{"POTUS", "Government"}},
		"b": {ICAO: "b", CMPG: "Mil", Category: "USAF", Tags: []string{"Gunship"}},
		"c": {ICAO: "c", CMPG: "Pol", Category: "Police Forces", Tags: []string{"Helicopter"}},
	}}
	all := db.Facets(Rule{})
	if !reflect.DeepEqual(facetValues(all["tags"]), []string{"Government", "Gunship", "Helicopter", "POTUS"}) {
		t.Fatalf("unconditioned tags = %#v", all["tags"])
	}

	// Choosing a category cuts the tag list down to that category's own tags.
	got := db.Facets(Rule{Category: []string{"Head of State"}})
	if !reflect.DeepEqual(facetValues(got["tags"]), []string{"Government", "POTUS"}) {
		t.Fatalf("tags under Head of State = %#v", got["tags"])
	}
	if !reflect.DeepEqual(facetValues(got["cmpg"]), []string{"Gov"}) {
		t.Fatalf("cmpg under Head of State = %#v", got["cmpg"])
	}
	// A field is never narrowed by its own value, or a second category could never
	// be added and the first could never be swapped.
	if !reflect.DeepEqual(got["category"], all["category"]) {
		t.Fatalf("category narrowed itself: %#v", got["category"])
	}

	// Facets respect every other condition, not just the most recent one.
	got = db.Facets(Rule{Category: []string{"Head of State"}, CMPG: []string{"Mil"}})
	if len(got["tags"]) != 0 {
		t.Fatalf("impossible combination still offers tags: %#v", got["tags"])
	}
}

func TestPreviewRejectsUnknownField(t *testing.T) {
	h := alertsHandler(t, filepath.Join(t.TempDir(), "alerts.yaml"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/preview", strings.NewReader(`{"nope":1}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestVocabularyDedupesAndSorts(t *testing.T) {
	db := dbWith(t, testConfig(t), []Plane{
		{ICAO: "a", Operator: "Zulu", Type: "C-17", ICAOType: "C17", CMPG: "Mil", Category: "Other", Tags: []string{"Heavy", "Cargo"}},
		{ICAO: "b", Operator: "Alpha", Tags: []string{"Cargo", ""}},
	})
	got := db.Vocabulary()
	if !reflect.DeepEqual(facetValues(got["operator"]), []string{"Alpha", "Zulu"}) || !reflect.DeepEqual(facetValues(got["tags"]), []string{"Cargo", "Heavy"}) {
		t.Fatalf("vocabulary = %#v", got)
	}
	// Cargo is on both aircraft, Heavy on one.
	if got["tags"][0].Count != 2 || got["tags"][1].Count != 1 {
		t.Fatalf("tag counts = %#v", got["tags"])
	}
	for _, key := range []string{"tags", "operator", "type", "icao_type", "cmpg", "category"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
}

// The type picker must work on a receiver whose sky the database has never heard of,
// so the codes come off aircraft.json and are counted by aircraft, not by sighting.
func TestFeedTypesCountAircraftSeen(t *testing.T) {
	h := &server{}
	h.setPollOK(&feed{Aircraft: []Aircraft{
		{Hex: "ABC123", Type: "B738"}, {Hex: "def456", Type: "B738"},
		{Hex: "aaa111", Type: " A320 "}, {Hex: "bbb222"}, {Hex: "", Type: "C172"},
	}}, NewTracker())
	// Same aircraft again, and one that has changed what it broadcasts.
	h.setPollOK(&feed{Aircraft: []Aircraft{
		{Hex: "abc123", Type: "B738"}, {Hex: "aaa111", Type: "A321"},
	}}, NewTracker())
	want := []FacetValue{{Value: "A321", Count: 1}, {Value: "B738", Count: 2}}
	if got := h.feedTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("feedTypes = %#v, want %#v", got, want)
	}
}

func TestEmptyConfigFilesUseDefaults(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	alerts := filepath.Join(dir, "alerts.yaml")
	for _, path := range []string{config, alerts} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append(requiredEnv(), "SKY_CONFIG="+config, "SKY_ALERTS_CONFIG="+alerts, "SKY_NTFY_TOPIC=t")
	cfg, err := LoadConfig(env)
	if err != nil || cfg.Listen != defaultConfig().Listen {
		t.Fatalf("config = %+v, err = %v", cfg, err)
	}
	a, err := LoadAlerts(env)
	if err != nil || a.Cooldown != defaultAlerts().Cooldown {
		t.Fatalf("alerts = %+v, err = %v", a, err)
	}
}

func TestExampleFilesRespectConfigSplit(t *testing.T) {
	if err := loadYAML("config.example.yaml", defaultConfig(), configMisplaced, "alerts.example.yaml", "hot-reloaded", nil); err != nil {
		t.Fatal(err)
	}
	if err := loadYAML("alerts.example.yaml", defaultAlerts(), alertsMisplaced, "config.example.yaml", "startup-only", alertsRemoved); err != nil {
		t.Fatal(err)
	}
}

func TestMisplacedConfigKeysNameAlertsFile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	alerts := filepath.Join(dir, "alerts.yaml")
	if err := os.WriteFile(config, []byte("rules: []\nlat: 0\nsource:\n  poll_interval: 1s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig([]string{"SKY_CONFIG=" + config, "SKY_ALERTS_CONFIG=" + alerts})
	if err == nil || !strings.Contains(err.Error(), "move rules, lat, source.poll_interval to "+alerts+" (hot-reloaded)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMisplacedAlertKeysNameConfigFile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	alerts := filepath.Join(dir, "alerts.yaml")
	if err := os.WriteFile(alerts, []byte("source:\n  url: http://example.com\nntfy:\n  topic: wrong\nlisten: :9000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAlerts([]string{"SKY_CONFIG=" + config, "SKY_ALERTS_CONFIG=" + alerts})
	if err == nil || !strings.Contains(err.Error(), "move source.url, ntfy.topic, listen to "+config+" (startup-only)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAlertsDefaultBesideResolvedConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alerts.yaml"), []byte("cooldown: 2h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAlerts([]string{"SKY_CONFIG=" + filepath.Join(dir, "config.yaml")})
	if err != nil || a.Cooldown.Std() != 2*time.Hour {
		t.Fatalf("alerts = %+v, err = %v", a, err)
	}
}

func TestBothLoadersKnowBothEnvironmentTables(t *testing.T) {
	env := append(requiredEnv(), "SKY_CONFIG=/nonexistent/config.yaml", "SKY_ALERTS_CONFIG=/nonexistent/alerts.yaml", "SKY_NTFY_TOPIC=t", "SKY_COOLDOWN=2h")
	if _, err := LoadConfig(env); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAlerts(env); err != nil {
		t.Fatal(err)
	}
	bad := append(env, "SKY_COOLDWON=3h")
	if _, err := LoadConfig(bad); err == nil {
		t.Fatal("LoadConfig accepted unknown variable")
	}
	if _, err := LoadAlerts(bad); err == nil {
		t.Fatal("LoadAlerts accepted unknown variable")
	}
}

func TestConfigRejectsUnknownYAMLKey(t *testing.T) {
	if _, err := loadWith(t, "cooldwon: 1h\nntfy:\n  topic: t\n"); err == nil {
		t.Fatal("a typo'd yaml key must be an error, not a silent default")
	}
}

// SKY_ is this service's namespace; a typo there must not silently take a default.
func TestConfigRejectsUnknownSKYEnvVarButAcceptsSKYCONFIG(t *testing.T) {
	_, err := loadWith(t, "", "SKY_NTFY_TOPIC=t", "SKY_COOLDWON=1h")
	if err == nil {
		t.Fatal("unknown SKY_ variable must be fatal")
	}
	if !strings.Contains(err.Error(), "SKY_COOLDWON") || !strings.Contains(err.Error(), "SKY_COOLDOWN") {
		t.Errorf("error should name the typo and suggest the nearest valid name, got: %v", err)
	}
	// SKY_CONFIG is bound separately but must still count as known.
	if _, err := loadWith(t, "", "SKY_NTFY_TOPIC=t"); err != nil {
		t.Fatalf("SKY_CONFIG must be an accepted name: %v", err)
	}
	// Foreign variables outside the namespace are none of our business.
	if _, err := loadWith(t, "", "SKY_NTFY_TOPIC=t", "PATH=/usr/bin", "TZ=UTC"); err != nil {
		t.Fatalf("non-SKY_ variables must be ignored: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		env        []string
	}{
		{name: "no topic", yaml: "ntfy:\n  topic: \"\"\n"},
		{name: "no source url", env: []string{"SKY_SOURCE_URL="}},
		{name: "no ntfy url", env: []string{"SKY_NTFY_URL="}},
		{name: "no db base url", env: []string{"SKY_DB_BASE_URL="}},
		{name: "no cache dir", env: []string{"SKY_CACHE_DIR="}},
		{name: "topic with slash", yaml: "ntfy:\n  topic: a/b\n"},
		{name: "bad priority", yaml: "ntfy:\n  topic: t\n  priority: 9\n"},
		{name: "token and password", yaml: "ntfy:\n  topic: t\n  token: x\n  user: u\n  password: p\n"},
		{name: "user without password", yaml: "ntfy:\n  topic: t\n  user: u\n"},
		{name: "empty db files", env: []string{"SKY_DB_FILES="}},
		{name: "db file traversal", env: []string{"SKY_DB_FILES=../../etc/passwd.csv"}},
		{name: "db file not csv", env: []string{"SKY_DB_FILES=x.txt"}},
		{name: "duplicate db file", env: []string{"SKY_DB_FILES=a.csv,a.csv"}},
		{name: "bad source scheme", env: []string{"SKY_SOURCE_URL=htp://oops/data.json"}},
		{name: "relative source path", env: []string{"SKY_SOURCE_URL=data/aircraft.json"}},
		{name: "zero cooldown", yaml: "ntfy:\n  topic: t\ncooldown: 0s\n"},
		{name: "distance without position", yaml: "ntfy:\n  topic: t\nfilters:\n  max_distance_nm: 50\n"},
		{name: "lat out of range", yaml: "ntfy:\n  topic: t\nfilters:\n  lat: 91.0\n  lon: 0.0\n"},
		{name: "inverted altitudes", yaml: "ntfy:\n  topic: t\nfilters:\n  min_altitude_ft: 5000\n  max_altitude_ft: 1000\n"},
		{name: "bad tar1090 url", yaml: "ntfy:\n  topic: t\ntar1090_url: 'not a url'\n"},
		{name: "negative distance", yaml: "ntfy:\n  topic: t\nfilters:\n  max_distance_nm: -5\n"},
		{name: "bad squawk key", yaml: "ntfy:\n  topic: t\nsquawk_priority:\n  '1200': 3\n"},
		{name: "bad squawk priority", yaml: "ntfy:\n  topic: t\nsquawk_priority:\n  '7700': 6\n"},
		{name: "rule priority missing", yaml: "ntfy:\n  topic: t\nrules:\n  - cmpg: [Mil]\n"},
		{name: "bad rule priority", yaml: "ntfy:\n  topic: t\nrules:\n  - cmpg: [Mil]\n    priority: -1\n"},
		{name: "rule without fields", yaml: "ntfy:\n  topic: t\nrules:\n  - name: everything\n    priority: 3\n"},
	} {
		if _, err := loadWith(t, tc.yaml, tc.env...); err == nil {
			t.Errorf("%s: want a validation error", tc.name)
		}
	}
}

// NaN slips past every `> 0` check and would silently disable the filter the operator
// just asked for.
func TestNonFiniteFilterValuesRejected(t *testing.T) {
	for _, value := range []string{".nan", ".inf"} {
		yaml := "lat: 0\nlon: 0\nrules:\n  - name: distance\n    max_distance_nm: " + value + "\n"
		if _, err := loadAlertsWith(t, yaml); err == nil {
			t.Errorf("%s: want a validation error", value)
		}
	}
}

func TestMigrationErrors(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		env  []string
		want string
	}{{"filters: {}\n", nil, "filters moved onto each rule"}, {"", []string{"SKY_FILTERS_MAX_DISTANCE_NM=50"}, "now a per-rule key in alerts.yaml"}} {
		_, err := loadAlertsWith(t, tc.yaml, tc.env...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error = %v, want %q", err, tc.want)
		}
	}
}

func TestRuleValidationNamesRule(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
	}{
		{"duplicate name", "rules:\n  - name: same\n    listed: true\n  - name: same\n    listed: false\n"},
		{"the old all key", "rules:\n  - name: empty\n    all: true\n"},
		{"distance without coordinates", "rules:\n  - name: distance\n    max_distance_nm: 3\n"},
		{"inverted altitude", "rules:\n  - name: altitude\n    min_altitude_ft: 100\n    max_altitude_ft: 100\n"},
		{"bad priority", "rules:\n  - name: priority\n    listed: true\n    priority: 9\n"},
		{"overhead without coordinates", "rules:\n  - name: overhead\n    passes_within_nm: 1\n"},
		{"horizon without distance", "lat: 0\nlon: 0\nrules:\n  - name: overhead\n    listed: true\n    passes_within: 5m\n"},
		{"zero horizon", "lat: 0\nlon: 0\nrules:\n  - name: overhead\n    passes_within_nm: 1\n    passes_within: 0s\n"},
		{"backwards horizon", "lat: 0\nlon: 0\nrules:\n  - name: overhead\n    passes_within_nm: 1\n    passes_within: -1m\n"},
		{"non-finite overhead", "lat: 0\nlon: 0\nrules:\n  - name: overhead\n    passes_within_nm: .nan\n"},
		{"circling with slow polls", "source:\n  poll_interval: 45s\nrules:\n  - name: orbit\n    circling: true\n"},
	} {
		_, err := loadAlertsWith(t, tc.yaml)
		if err == nil || !strings.Contains(err.Error(), "rule ") {
			t.Errorf("%s: error = %v", tc.name, err)
		}
	}
	// Motion keys are conditions in their own right.
	if _, err := loadAlertsWith(t, "rules:\n  - name: orbit\n    circling: true\n"); err != nil {
		t.Errorf("circling alone should be a valid rule: %v", err)
	}
}

// A name is a label, not a requirement: it never reaches a notification, only the
// cooldown ledger and the logs, and a rule's conditions already say what it is.
func TestRuleNamesAreOptional(t *testing.T) {
	cfg, err := loadAlertsWith(t, "rules:\n  - listed: true\n  - cmpg: [Mil]\n")
	if err != nil {
		t.Fatalf("a rule without a name must be valid: %v", err)
	}
	a, b := cfg.Rules[0].Key(), cfg.Rules[1].Key()
	if a == "" || b == "" {
		t.Fatalf("unnamed rules need keys, got %q and %q", a, b)
	}
	if a == b {
		t.Fatalf("rules with different conditions must key apart, both %q", a)
	}

	// The key is the cooldown key, so what does and does not move it is the whole point:
	// position and priority must not, and a changed condition must.
	same, err := loadAlertsWith(t, "rules:\n  - cmpg: [Mil]\n    priority: 5\n  - listed: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := same.Rules[0].Key(); got != b {
		t.Errorf("reordering or repriotising a rule must not change its key: %q, want %q", got, b)
	}
	edited, err := loadAlertsWith(t, "rules:\n  - cmpg: [Pol]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := edited.Rules[0].Key(); got == b {
		t.Errorf("editing a rule's conditions must change its key, still %q", got)
	}

	// A named rule keys on its name, and that is what the alert carries.
	named, err := loadAlertsWith(t, "rules:\n  - name: listed\n    listed: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := named.Rules[0].Key(); got != "listed" {
		t.Errorf("a named rule keys on its name, got %q", got)
	}

	// Names and fingerprints share one cooldown namespace, so a key that repeats is
	// refused however it came to repeat: two rules that state the same conditions, or a
	// name copied from the fingerprint the UI showed for an unnamed rule.
	if _, err := loadAlertsWith(t, "rules:\n  - cmpg: [Mil]\n  - cmpg: [Mil]\n    priority: 2\n"); err == nil {
		t.Error("two unnamed rules with the same conditions share a cooldown key and must be refused")
	}
	clash := fmt.Sprintf("rules:\n  - listed: true\n  - name: %q\n    cmpg: [Mil]\n", a)
	if _, err := loadAlertsWith(t, clash); err == nil {
		t.Errorf("a name equal to an earlier rule's fingerprint %s must be refused", a)
	}

	db := dbWith(t, testConfig(t), mustParse(t, sampleCSV))
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Listed: boolp(true)}}
	alert := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, alerts, nil)
	if alert == nil {
		t.Fatal("an unnamed rule must still alert")
	}
	if alert.Trigger != alerts.Rules[0].Key() {
		t.Errorf("trigger = %q, want the rule key %q", alert.Trigger, alerts.Rules[0].Key())
	}
}

// A receiver at 0,0 is a valid receiver — the position must be optional, not zero-tested.
func TestZeroCoordinatesAreValid(t *testing.T) {
	cfg, err := loadAlertsWith(t, "lat: 0.0\nlon: 0.0\n")
	if err != nil {
		t.Fatalf("0,0 should be a valid receiver position: %v", err)
	}
	if cfg.Lat == nil || *cfg.Lat != 0 {
		t.Error("lat 0.0 should be set, not nil")
	}
}

func TestAbsoluteFilePathSourceIsAccepted(t *testing.T) {
	cfg, err := loadWith(t, "ntfy:\n  topic: t\n", "SKY_SOURCE_URL=/run/ultrafeeder/aircraft.json")
	if err != nil {
		t.Fatal(err)
	}
	if NewSource(cfg, http.DefaultClient).isHTTP {
		t.Error("an absolute path source must not be treated as HTTP")
	}
}

// notify says when a rule announces what it matched, never what it matches. So it must
// not move the fingerprint an unnamed rule is keyed by: if it did, adding it to a rule
// already in service would reset that rule's cooldown and its alert history.
func TestNotifyIsNotACondition(t *testing.T) {
	plain, err := loadAlertsWith(t, "lat: 51.5\nlon: -0.12\nrules:\n  - cmpg: [Mil]\n")
	if err != nil {
		t.Fatal(err)
	}
	held, err := loadAlertsWith(t, "lat: 51.5\nlon: -0.12\nrules:\n  - cmpg: [Mil]\n    notify: closest_pass\n")
	if err != nil {
		t.Fatalf("closest_pass with a receiver must be valid: %v", err)
	}
	if a, b := plain.Rules[0].Key(), held.Rules[0].Key(); a != b {
		t.Errorf("notify must not change the cooldown key: %q became %q", a, b)
	}

	if _, err := loadAlertsWith(t, "lat: 51.5\nlon: -0.12\nrules:\n  - notify: when_it_lands\n"); err == nil {
		t.Error("an unknown notify value must be refused")
	}
	// No receiver, so there is nothing for the aircraft to be closest to.
	if _, err := loadAlertsWith(t, "rules:\n  - cmpg: [Mil]\n    notify: closest_pass\n"); err == nil {
		t.Error("closest_pass without lat/lon must be refused")
	}
	if _, err := loadAlertsWith(t, "rules:\n  - cmpg: [Mil]\n    notify: on_sight\n"); err != nil {
		t.Errorf("on_sight needs no receiver: %v", err)
	}
}
