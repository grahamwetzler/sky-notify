package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
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

func loadWith(t *testing.T, doc string, env ...string) (*Config, error) {
	t.Helper()
	store := newTestStore(t)
	if doc != "" {
		if _, err := store.Put("config", doc); err != nil {
			t.Fatal(err)
		}
	}
	return LoadConfig(append(requiredEnv(), env...), store)
}

func loadAlertsWith(t *testing.T, doc string, env ...string) (*Alerts, error) {
	t.Helper()
	store := newTestStore(t)
	if doc != "" {
		if _, err := store.Put("alerts", doc); err != nil {
			t.Fatal(err)
		}
	}
	return LoadAlerts(env, store)
}

func TestConfigJSONRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	enabled := true
	cfg.Map.Enabled = &enabled
	token := "tk_x"
	cfg.Ntfy.Token = &token
	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&got, cfg) {
		t.Fatalf("round trip mismatch\ngot:  %+v\nwant: %+v", got, cfg)
	}
}

func TestConfigPrecedenceEnvOverStoreOverDefault(t *testing.T) {
	doc := `{"ntfy":{"topic":"from-store"}}`

	cfg, err := loadWith(t, doc)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ntfy.Topic != "from-store" {
		t.Errorf("the stored document should override defaults, got %q", cfg.Ntfy.Topic)
	}

	cfg, err = loadWith(t, doc, "SKY_NTFY_TOPIC=from-env")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ntfy.Topic != "from-env" {
		t.Errorf("env should win over the stored document, got %q", cfg.Ntfy.Topic)
	}
}

func TestConfigReloadHotSwapTakesEffect(t *testing.T) {
	store := newTestStore(t)
	environ := []string{"SKY_NTFY_TOPIC=test-topic"}
	write := func(priority int) {
		t.Helper()
		doc := fmt.Sprintf(`{"rules":[{"name":"test","icao":["adeb2f"],"priority":%d}]}`, priority)
		if _, err := store.Put("alerts", doc); err != nil {
			t.Fatal(err)
		}
	}
	write(1)
	alerts, err := LoadAlerts(environ, store)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(alerts)
	cfg := testConfig(t)
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	write(5)
	if err := reloadConfig(store, live, environ, nil); err != nil {
		t.Fatal(err)
	}
	if a := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, live.Get(), nil); a == nil || a.Priority != 5 {
		t.Fatalf("reloaded alert = %+v, want priority 5", a)
	}
}

func TestInvalidConfigReloadIsIgnored(t *testing.T) {
	store := newTestStore(t)
	var environ []string
	if _, err := store.Put("alerts", `{"ntfy":{"priority":3}}`); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAlerts(environ, store)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(cfg)
	if _, err := store.Put("alerts", `{"ntfy": [`); err != nil {
		t.Fatal(err)
	}
	if err := reloadConfig(store, live, environ, nil); err == nil {
		t.Fatal("invalid reload should fail")
	}
	if live.Get() != cfg {
		t.Fatal("invalid reload replaced the running config")
	}
}

func TestConfigReloadEnvironmentStillWins(t *testing.T) {
	store := newTestStore(t)
	environ := []string{"SKY_NTFY_PRIORITY=4"}
	if _, err := store.Put("alerts", `{"ntfy":{"priority":3}}`); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAlerts(environ, store)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(cfg)
	if _, err := store.Put("alerts", `{"ntfy":{"priority":2}}`); err != nil {
		t.Fatal(err)
	}
	if err := reloadConfig(store, live, environ, nil); err != nil {
		t.Fatal(err)
	}
	if live.Get().Ntfy.Priority != 4 {
		t.Fatalf("priority = %d, want environment value 4", live.Get().Ntfy.Priority)
	}
}

func TestWatchConfigReloadsWhenSectionAppearsAndChanges(t *testing.T) {
	store := newTestStore(t)
	var environ []string
	cfg, err := LoadAlerts(environ, store)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLive(cfg)
	// Baseline taken before the row is written, so the appearance is a real change.
	watcher := newConfigWatcher(store)
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
	if _, err := store.Put("alerts", `{"ntfy":{"priority":2}}`); err != nil {
		t.Fatal(err)
	}
	wait()
	if live.Get().Ntfy.Priority != 2 {
		t.Fatalf("priority = %d after row appeared, want 2", live.Get().Ntfy.Priority)
	}
	if _, err := store.Put("alerts", `{"ntfy":{"priority":4}}`); err != nil {
		t.Fatal(err)
	}
	wait()
	if live.Get().Ntfy.Priority != 4 {
		t.Fatalf("priority = %d after row changed, want 4", live.Get().Ntfy.Priority)
	}
}

func TestConfigMissingRowIsFine(t *testing.T) {
	if _, err := LoadConfig(append(requiredEnv(), "SKY_NTFY_TOPIC=t"), newTestStore(t)); err != nil {
		t.Fatalf("a missing config row should not be an error: %v", err)
	}
}

func TestAlertsMissingRowIsFine(t *testing.T) {
	if _, err := LoadAlerts(nil, newTestStore(t)); err != nil {
		t.Fatalf("a missing alerts row should not be an error: %v", err)
	}
}

func alertsHandler(t *testing.T, store *SettingsStore, planes ...*Plane) http.Handler {
	t.Helper()
	cfg := testConfig(t)
	db := NewDB(cfg, http.DefaultClient)
	if len(planes) > 0 {
		db.merged = map[string]*Plane{}
		for _, p := range planes {
			db.merged[p.ICAO] = p
		}
	}
	return (&server{live: NewLive(defaultAlerts()), db: db, store: store}).mux(newQueue(1))
}

func TestAlertsUIRoundTrip(t *testing.T) {
	store := newTestStore(t)
	h := alertsHandler(t, store)
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
	got, err := LoadAlerts(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestAlertsUIGetReadsSavedFileImmediately(t *testing.T) {
	h := alertsHandler(t, newTestStore(t))
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
			store := newTestStore(t)
			h := alertsHandler(t, store)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if _, _, ok, err := store.Get("alerts"); ok || err != nil {
				t.Fatalf("invalid payload wrote a row: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestAlertsUIRejectsUnknownField(t *testing.T) {
	h := alertsHandler(t, newTestStore(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(`{"nope":1}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestAlertsUIRejectsOversizedBody(t *testing.T) {
	h := alertsHandler(t, newTestStore(t))
	w := httptest.NewRecorder()
	body := `{"log_level":"` + strings.Repeat("x", 1<<20) + `"}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "request body too large") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestAlertsUIBlankCoordinatesStayNil(t *testing.T) {
	store := newTestStore(t)
	h := alertsHandler(t, store)
	w := httptest.NewRecorder()
	body := `{"lat":null,"lon":null}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	got, err := LoadAlerts(nil, store)
	if err != nil || got.Lat != nil || got.Lon != nil {
		t.Fatalf("alerts = %+v, err = %v", got, err)
	}
}

func TestAlertsUILockedReflectsEnvironment(t *testing.T) {
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
			h := alertsHandler(t, newTestStore(t))
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
	store := newTestStore(t)
	if _, err := store.Put("alerts", `{"cooldown":"24h"}`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKY_COOLDOWN", "1h")
	h := alertsHandler(t, store)
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
	store := newTestStore(t)
	// max_distance_nm fails validation without lat/lon, and here they arrive from the
	// environment — so a GET that validated the row alone would 500 on a valid setup.
	if _, err := store.Put("alerts", `{"rules":[{"name":"near","max_distance_nm":25}]}`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SKY_LAT", "41.9")
	t.Setenv("SKY_LON", "-87.6")
	h := alertsHandler(t, store)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d: %s", w.Code, w.Body.String())
	}
}

func TestAlertsUIUnknownPageIsNotFound(t *testing.T) {
	h := alertsHandler(t, newTestStore(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDurationMarshalKeepsJSONReadable(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour:   "24h",
		5 * time.Minute:  "5m",
		90 * time.Second: "1m30s",
		90 * time.Minute: "1h30m",
		0:                "0s",
	} {
		blob, err := Duration(d).MarshalJSON()
		got := strings.Trim(string(blob), `"`)
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
	h := alertsHandler(t, newTestStore(t), planes...)

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
	h := alertsHandler(t, newTestStore(t), gov, mil)

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
	h := alertsHandler(t, newTestStore(t))
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

func TestEmptyConfigRowsUseDefaults(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Put("config", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("alerts", `{}`); err != nil {
		t.Fatal(err)
	}
	env := append(requiredEnv(), "SKY_NTFY_TOPIC=t")
	cfg, err := LoadConfig(env, store)
	if err != nil || cfg.Listen != defaultConfig().Listen {
		t.Fatalf("config = %+v, err = %v", cfg, err)
	}
	a, err := LoadAlerts(env, store)
	if err != nil || a.Cooldown != defaultAlerts().Cooldown {
		t.Fatalf("alerts = %+v, err = %v", a, err)
	}
}

// The two sections of the combined example document still respect the same
// startup/hot-reload split the two example files used to: neither carries a key that
// belongs to the other, and each decodes strictly under its own struct.
func TestExampleFileRespectsConfigSplit(t *testing.T) {
	blob, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config json.RawMessage `json:"config"`
		Alerts json.RawMessage `json:"alerts"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	if _, err := store.Put("config", string(doc.Config)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("alerts", string(doc.Alerts)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFile(store); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAlertsFile(store); err != nil {
		t.Fatal(err)
	}
}

func TestMisplacedConfigKeysNameAlertsSection(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Put("config", `{"rules":[],"lat":0,"source":{"poll_interval":"1s"}}`); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(nil, store)
	if err == nil || !strings.Contains(err.Error(), "move rules, lat, source.poll_interval to alerts (hot-reloaded)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMisplacedAlertKeysNameConfigSection(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Put("alerts", `{"source":{"url":"http://example.com"},"ntfy":{"topic":"wrong"},"listen":":9000"}`); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAlerts(nil, store)
	if err == nil || !strings.Contains(err.Error(), "move source.url, ntfy.topic, listen to config (startup-only)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBothLoadersKnowBothEnvironmentTables(t *testing.T) {
	store := newTestStore(t)
	env := append(requiredEnv(), "SKY_NTFY_TOPIC=t", "SKY_COOLDOWN=2h")
	if _, err := LoadConfig(env, store); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAlerts(env, store); err != nil {
		t.Fatal(err)
	}
	bad := append(env, "SKY_COOLDWON=3h")
	if _, err := LoadConfig(bad, store); err == nil {
		t.Fatal("LoadConfig accepted unknown variable")
	}
	if _, err := LoadAlerts(bad, store); err == nil {
		t.Fatal("LoadAlerts accepted unknown variable")
	}
}

func TestConfigRejectsUnknownJSONKey(t *testing.T) {
	if _, err := loadWith(t, `{"cooldwon":"1h","ntfy":{"topic":"t"}}`); err == nil {
		t.Fatal("a typo'd key must be an error, not a silent default")
	}
}

// SKY_ is this service's namespace; a typo there must not silently take a default.
func TestConfigRejectsUnknownSKYEnvVarButAcceptsKnownOnes(t *testing.T) {
	_, err := loadWith(t, "", "SKY_NTFY_TOPIC=t", "SKY_COOLDWON=1h")
	if err == nil {
		t.Fatal("unknown SKY_ variable must be fatal")
	}
	if !strings.Contains(err.Error(), "SKY_COOLDWON") || !strings.Contains(err.Error(), "SKY_COOLDOWN") {
		t.Errorf("error should name the typo and suggest the nearest valid name, got: %v", err)
	}
	// SKY_CONFIG_DB is bound separately but must still count as known.
	if _, err := loadWith(t, "", "SKY_NTFY_TOPIC=t", "SKY_CONFIG_DB=/tmp/other.db"); err != nil {
		t.Fatalf("SKY_CONFIG_DB must be an accepted name: %v", err)
	}
	// Foreign variables outside the namespace are none of our business.
	if _, err := loadWith(t, "", "SKY_NTFY_TOPIC=t", "PATH=/usr/bin", "TZ=UTC"); err != nil {
		t.Fatalf("non-SKY_ variables must be ignored: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		env       []string
	}{
		{name: "no topic", doc: `{"ntfy":{"topic":""}}`},
		{name: "no source url", env: []string{"SKY_SOURCE_URL="}},
		{name: "no ntfy url", env: []string{"SKY_NTFY_URL="}},
		{name: "no db base url", env: []string{"SKY_DB_BASE_URL="}},
		{name: "no cache dir", env: []string{"SKY_CACHE_DIR="}},
		{name: "topic with slash", doc: `{"ntfy":{"topic":"a/b"}}`},
		{name: "bad priority", doc: `{"ntfy":{"topic":"t","priority":9}}`},
		{name: "token and password", doc: `{"ntfy":{"topic":"t","token":"x","user":"u","password":"p"}}`},
		{name: "user without password", doc: `{"ntfy":{"topic":"t","user":"u"}}`},
		{name: "empty db files", env: []string{"SKY_DB_FILES="}},
		{name: "db file traversal", env: []string{"SKY_DB_FILES=../../etc/passwd.csv"}},
		{name: "db file not csv", env: []string{"SKY_DB_FILES=x.txt"}},
		{name: "duplicate db file", env: []string{"SKY_DB_FILES=a.csv,a.csv"}},
		{name: "bad source scheme", env: []string{"SKY_SOURCE_URL=htp://oops/data.json"}},
		{name: "relative source path", env: []string{"SKY_SOURCE_URL=data/aircraft.json"}},
		{name: "zero cooldown", doc: `{"ntfy":{"topic":"t"},"cooldown":"0s"}`},
		{name: "distance without position", doc: `{"ntfy":{"topic":"t"},"filters":{"max_distance_nm":50}}`},
		{name: "lat out of range", doc: `{"ntfy":{"topic":"t"},"filters":{"lat":91.0,"lon":0.0}}`},
		{name: "inverted altitudes", doc: `{"ntfy":{"topic":"t"},"filters":{"min_altitude_ft":5000,"max_altitude_ft":1000}}`},
		{name: "bad tar1090 url", doc: `{"ntfy":{"topic":"t"},"tar1090_url":"not a url"}`},
		{name: "negative distance", doc: `{"ntfy":{"topic":"t"},"filters":{"max_distance_nm":-5}}`},
		{name: "bad squawk key", doc: `{"ntfy":{"topic":"t"},"squawk_priority":{"1200":3}}`},
		{name: "bad squawk priority", doc: `{"ntfy":{"topic":"t"},"squawk_priority":{"7700":6}}`},
		{name: "rule priority missing", doc: `{"ntfy":{"topic":"t"},"rules":[{"cmpg":["Mil"]}]}`},
		{name: "bad rule priority", doc: `{"ntfy":{"topic":"t"},"rules":[{"cmpg":["Mil"],"priority":-1}]}`},
		{name: "rule without fields", doc: `{"ntfy":{"topic":"t"},"rules":[{"name":"everything","priority":3}]}`},
	} {
		if _, err := loadWith(t, tc.doc, tc.env...); err == nil {
			t.Errorf("%s: want a validation error", tc.name)
		}
	}
}

// NaN and Infinity slip past every `> 0` check and would silently disable the filter the
// operator just asked for. JSON syntax cannot carry either as a document literal, so this
// is exercised on the struct directly rather than through a loaded document.
func TestNonFiniteFilterValuesRejected(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		a := defaultAlerts()
		lat, lon, nm := 0.0, 0.0, v
		a.Lat, a.Lon = &lat, &lon
		a.Rules = []Rule{{Name: "distance", MaxDistanceNM: &nm}}
		if err := a.validate(); err == nil {
			t.Errorf("%v: want a validation error", v)
		}
	}
}

func TestMigrationErrors(t *testing.T) {
	for _, tc := range []struct {
		doc, want string
		env       []string
	}{
		{doc: `{"filters":{}}`, want: "filters moved onto each rule"},
		{env: []string{"SKY_FILTERS_MAX_DISTANCE_NM=50"}, want: "now a per-rule key in alerts"},
	} {
		_, err := loadAlertsWith(t, tc.doc, tc.env...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error = %v, want %q", err, tc.want)
		}
	}
}

func TestRuleValidationNamesRule(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
	}{
		{"duplicate name", `{"rules":[{"name":"same","listed":true},{"name":"same","listed":false}]}`},
		{"the old all key", `{"rules":[{"name":"empty","all":true}]}`},
		{"distance without coordinates", `{"rules":[{"name":"distance","max_distance_nm":3}]}`},
		{"inverted altitude", `{"rules":[{"name":"altitude","min_altitude_ft":100,"max_altitude_ft":100}]}`},
		{"bad priority", `{"rules":[{"name":"priority","listed":true,"priority":9}]}`},
		{"overhead without coordinates", `{"rules":[{"name":"overhead","passes_within_nm":1}]}`},
		{"horizon without distance", `{"lat":0,"lon":0,"rules":[{"name":"overhead","listed":true,"passes_within":"5m"}]}`},
		{"zero horizon", `{"lat":0,"lon":0,"rules":[{"name":"overhead","passes_within_nm":1,"passes_within":"0s"}]}`},
		{"backwards horizon", `{"lat":0,"lon":0,"rules":[{"name":"overhead","passes_within_nm":1,"passes_within":"-1m"}]}`},
		{"circling with slow polls", `{"source":{"poll_interval":"45s"},"rules":[{"name":"orbit","circling":true}]}`},
	} {
		_, err := loadAlertsWith(t, tc.doc)
		if err == nil || !strings.Contains(err.Error(), "rule ") {
			t.Errorf("%s: error = %v", tc.name, err)
		}
	}
	// Motion keys are conditions in their own right.
	if _, err := loadAlertsWith(t, `{"rules":[{"name":"orbit","circling":true}]}`); err != nil {
		t.Errorf("circling alone should be a valid rule: %v", err)
	}
}

// A name is a label, not a requirement: it never reaches a notification, only the
// cooldown ledger and the logs, and a rule's conditions already say what it is.
func TestRuleNamesAreOptional(t *testing.T) {
	cfg, err := loadAlertsWith(t, `{"rules":[{"listed":true},{"cmpg":["Mil"]}]}`)
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
	same, err := loadAlertsWith(t, `{"rules":[{"cmpg":["Mil"],"priority":5},{"listed":true}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := same.Rules[0].Key(); got != b {
		t.Errorf("reordering or repriotising a rule must not change its key: %q, want %q", got, b)
	}
	edited, err := loadAlertsWith(t, `{"rules":[{"cmpg":["Pol"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := edited.Rules[0].Key(); got == b {
		t.Errorf("editing a rule's conditions must change its key, still %q", got)
	}

	// A named rule keys on its name, and that is what the alert carries.
	named, err := loadAlertsWith(t, `{"rules":[{"name":"listed","listed":true}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := named.Rules[0].Key(); got != "listed" {
		t.Errorf("a named rule keys on its name, got %q", got)
	}

	// Names and fingerprints share one cooldown namespace, so a key that repeats is
	// refused however it came to repeat: two rules that state the same conditions, or a
	// name copied from the fingerprint the UI showed for an unnamed rule.
	if _, err := loadAlertsWith(t, `{"rules":[{"cmpg":["Mil"]},{"cmpg":["Mil"],"priority":2}]}`); err == nil {
		t.Error("two unnamed rules with the same conditions share a cooldown key and must be refused")
	}
	clash := fmt.Sprintf(`{"rules":[{"listed":true},{"name":%q,"cmpg":["Mil"]}]}`, a)
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
	cfg, err := loadAlertsWith(t, `{"lat":0.0,"lon":0.0}`)
	if err != nil {
		t.Fatalf("0,0 should be a valid receiver position: %v", err)
	}
	if cfg.Lat == nil || *cfg.Lat != 0 {
		t.Error("lat 0.0 should be set, not nil")
	}
}

func TestAbsoluteFilePathSourceIsAccepted(t *testing.T) {
	cfg, err := loadWith(t, `{"ntfy":{"topic":"t"}}`, "SKY_SOURCE_URL=/run/ultrafeeder/aircraft.json")
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
	plain, err := loadAlertsWith(t, `{"lat":51.5,"lon":-0.12,"rules":[{"cmpg":["Mil"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	held, err := loadAlertsWith(t, `{"lat":51.5,"lon":-0.12,"rules":[{"cmpg":["Mil"],"notify":"closest_pass"}]}`)
	if err != nil {
		t.Fatalf("closest_pass with a receiver must be valid: %v", err)
	}
	if a, b := plain.Rules[0].Key(), held.Rules[0].Key(); a != b {
		t.Errorf("notify must not change the cooldown key: %q became %q", a, b)
	}

	if _, err := loadAlertsWith(t, `{"lat":51.5,"lon":-0.12,"rules":[{"notify":"when_it_lands"}]}`); err == nil {
		t.Error("an unknown notify value must be refused")
	}
	// No receiver, so there is nothing for the aircraft to be closest to.
	if _, err := loadAlertsWith(t, `{"rules":[{"cmpg":["Mil"],"notify":"closest_pass"}]}`); err == nil {
		t.Error("closest_pass without lat/lon must be refused")
	}
	if _, err := loadAlertsWith(t, `{"rules":[{"cmpg":["Mil"],"notify":"on_sight"}]}`); err != nil {
		t.Errorf("on_sight needs no receiver: %v", err)
	}
}

func TestAIBlockLoadsFromStoreAndEnv(t *testing.T) {
	doc := `{"ai":{"url":"https://openrouter.ai/api/v1","key":"sk-or-v1-secret","model":"perplexity/sonar","timeout":"5s"}}`
	alerts, err := loadAlertsWith(t, doc)
	if err != nil {
		t.Fatalf("LoadAlerts: %v", err)
	}
	if alerts.AI.URL != "https://openrouter.ai/api/v1" || alerts.aiKey() != "sk-or-v1-secret" ||
		alerts.AI.Model != "perplexity/sonar" || alerts.AI.Timeout.Std() != 5*time.Second {
		t.Fatalf("ai = %+v", alerts.AI)
	}
	if !alerts.aiEnabled() {
		t.Error("aiEnabled = false for a configured provider")
	}

	alerts, err = loadAlertsWith(t, doc, "SKY_AI_MODEL=anthropic/claude-sonnet-4.5", "SKY_AI_TIMEOUT=9s")
	if err != nil {
		t.Fatalf("LoadAlerts with env: %v", err)
	}
	if alerts.AI.Model != "anthropic/claude-sonnet-4.5" || alerts.AI.Timeout.Std() != 9*time.Second {
		t.Fatalf("env override: ai = %+v", alerts.AI)
	}
	// Absent is off, and the default timeout does not make it look configured.
	bare, err := loadAlertsWith(t, `{"cooldown":"1h"}`)
	if err != nil {
		t.Fatalf("LoadAlerts: %v", err)
	}
	if bare.aiEnabled() {
		t.Error("aiEnabled = true with no ai block")
	}
}

// The ai block is hot-reloaded, so it belongs in the alerts section. Putting it in
// config has to say which section it belongs in rather than read as a typo.
func TestAIBlockInConfigNamesTheRightSection(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Put("config", `{"ai":{"key":"sk-or-v1-secret"}}`); err != nil {
		t.Fatal(err)
	}
	env := append(requiredEnv(), "SKY_NTFY_TOPIC=test")
	_, err := LoadConfig(env, store)
	if err == nil {
		t.Fatal("an ai block in config loaded without complaint")
	}
	if !strings.Contains(err.Error(), "ai.key") || !strings.Contains(err.Error(), "alerts") {
		t.Errorf("error should name the key and the section it belongs in: %v", err)
	}
}

// Half a block is a misconfiguration, not a way to turn research off.
func TestPartialAIBlockIsRefused(t *testing.T) {
	a := defaultAlerts()
	a.AI.URL = "https://openrouter.ai/api/v1"
	if err := a.validate(); err == nil {
		t.Fatal("a url with no model validated")
	}
	a.AI.URL, a.AI.Model = "", "perplexity/sonar"
	if err := a.validate(); err == nil {
		t.Fatal("a model with no url validated")
	}
	a.AI.URL = "not-a-url"
	if err := a.validate(); err == nil {
		t.Fatal("a non-http ai.url validated")
	}
	a.AI.URL, a.AI.Timeout = "https://openrouter.ai/api/v1", 0
	if err := a.validate(); err == nil {
		t.Fatal("a zero ai.timeout validated")
	}
}

// An empty database next to a config.yaml/alerts.yaml left over from an upgrade must not
// start silently on defaults: that combination means the deployment's real settings were
// never migrated, not that it has none.
func TestLegacyYAMLBesideEmptyStoreRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "config.db")
	store, err := OpenSettings(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := os.WriteFile(filepath.Join(dir, "alerts.yaml"), []byte("cooldown: 24h\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = checkLegacyYAML(store, dbFile)
	if err == nil {
		t.Fatal("a leftover alerts.yaml beside an empty database started without complaint")
	}
	if !strings.Contains(err.Error(), "alerts.yaml") || !strings.Contains(err.Error(), "-config-import") {
		t.Errorf("error should name the file and the way to migrate it: %v", err)
	}
}

// Once either row is written, a leftover YAML file beside the database is not the upgrade
// case any more, and must not block startup.
func TestLegacyYAMLBesidePopulatedStoreIsFine(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "config.db")
	store, err := OpenSettings(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("listen: :8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("alerts", `{"cooldown":"1h"}`); err != nil {
		t.Fatal(err)
	}

	if err := checkLegacyYAML(store, dbFile); err != nil {
		t.Fatalf("checkLegacyYAML with a populated store: %v", err)
	}
}

// No YAML file, no complaint — the ordinary fresh-deployment case.
func TestNoLegacyYAMLIsFine(t *testing.T) {
	store := newTestStore(t)
	if err := checkLegacyYAML(store, filepath.Join(t.TempDir(), "config.db")); err != nil {
		t.Fatalf("checkLegacyYAML with no legacy files: %v", err)
	}
}

// ---------- /api/config ----------

func TestConfigUIRoundTrip(t *testing.T) {
	store := newTestStore(t)
	h := alertsHandler(t, store)
	want := testConfig(t)
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	var putResp struct {
		OK              bool `json:"ok"`
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.NewDecoder(w.Body).Decode(&putResp); err != nil {
		t.Fatal(err)
	}
	if !putResp.OK || !putResp.RestartRequired {
		t.Fatalf("PUT response = %+v", putResp)
	}

	got, err := loadConfigFile(store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\ngot:  %+v\nwant: %+v", got, want)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d: %s", w.Code, w.Body.String())
	}
	var getResp struct {
		Config Config `json:"config"`
	}
	if err := json.NewDecoder(w.Body).Decode(&getResp); err != nil {
		t.Fatal(err)
	}
	if getResp.Config.Source.URL != want.Source.URL || getResp.Config.Ntfy.Topic != want.Ntfy.Topic {
		t.Fatalf("GET config = %+v", getResp.Config)
	}
}

func TestConfigUILockedReflectsEnvironment(t *testing.T) {
	t.Setenv("SKY_CACHE_DIR", "/env-cache")
	h := alertsHandler(t, newTestStore(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Locked []map[string]string `json:"locked"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range body.Locked {
		if item["key"] == "cache_dir" {
			found = true
			if item["value"] != "/env-cache" {
				t.Errorf("locked value = %q", item["value"])
			}
		}
	}
	if !found {
		t.Error("cache_dir set by the environment must be reported as locked")
	}
}

// A secret is reported set/not-set, never echoed, the same treatment ai.key gets.
func TestConfigUISecretsAreNotEchoed(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Put("config", `{"ntfy":{"token":"tk_secret"}}`); err != nil {
		t.Fatal(err)
	}
	h := alertsHandler(t, store)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tk_secret") {
		t.Fatalf("the ntfy token was served to the browser:\n%s", w.Body.String())
	}
	var body struct {
		NtfyTokenSet bool `json:"ntfy_token_set"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.NtfyTokenSet {
		t.Error("ntfy_token_set = false, want true")
	}
}

func TestConfigUIRejectsInvalidWithoutWriting(t *testing.T) {
	store := newTestStore(t)
	h := alertsHandler(t, store)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"ntfy":{"topic":"a/b"}}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if _, _, ok, err := store.Get("config"); ok || err != nil {
		t.Fatalf("invalid payload wrote a row: ok=%v err=%v", ok, err)
	}
}

func TestConfigUIRejectsUnknownField(t *testing.T) {
	h := alertsHandler(t, newTestStore(t))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/config", strings.NewReader(`{"nope":1}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// ---------- export / import ----------

func TestConfigExportImportRoundTrip(t *testing.T) {
	store := newTestStore(t)
	h := alertsHandler(t, store)

	cfgBody, err := json.Marshal(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(cfgBody)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT config status %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(`{"rules":[{"name":"exported","listed":true}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT alerts status %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/config/export", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("export status %d: %s", w.Code, w.Body.String())
	}
	exported := w.Body.Bytes()

	// Wipe the store and import the export back in.
	fresh := newTestStore(t)
	h2 := alertsHandler(t, fresh)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/config/import", bytes.NewReader(exported)))
	if w.Code != http.StatusOK {
		t.Fatalf("import status %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/config/export", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("re-export status %d: %s", w.Code, w.Body.String())
	}
	reExported := w.Body.Bytes()

	var first, second exportDocument
	if err := json.Unmarshal(exported, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reExported, &second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Config, second.Config) {
		t.Fatalf("config mismatch after round trip\ngot:  %+v\nwant: %+v", second.Config, first.Config)
	}
	if !reflect.DeepEqual(first.Alerts, second.Alerts) {
		t.Fatalf("alerts mismatch after round trip\ngot:  %+v\nwant: %+v", second.Alerts, first.Alerts)
	}
}

func TestConfigImportRejectsInvalidWithoutWriting(t *testing.T) {
	store := newTestStore(t)
	h := alertsHandler(t, store)
	// A good config paired with an invalid alerts section: the import is all-or-nothing,
	// so the good half must not land either.
	cfgBlob, err := json.Marshal(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"config":%s,"alerts":{"ntfy":{"priority":9}}}`, cfgBlob)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if _, _, ok, err := store.Get("config"); ok || err != nil {
		t.Fatalf("a rejected import must not write config either: ok=%v err=%v", ok, err)
	}
}

func TestConfigExportRedactsOnRequest(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Put("alerts", `{"ai":{"url":"https://openrouter.ai/api/v1","key":"sk-or-v1-secret","model":"perplexity/sonar"}}`); err != nil {
		t.Fatal(err)
	}
	h := alertsHandler(t, store)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/config/export?redact=true", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-or-v1-secret") {
		t.Fatalf("redacted export leaked the key:\n%s", w.Body.String())
	}
	var doc exportDocument
	if err := json.NewDecoder(w.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.AIKeySet == nil || !*doc.AIKeySet {
		t.Errorf("ai_key_set = %v, want true", doc.AIKeySet)
	}
	if doc.Alerts == nil || doc.Alerts.AI.Key != nil {
		t.Errorf("redacted export still carried the key: %+v", doc.Alerts)
	}
}

// The page never holds ntfy.token or ntfy.password, so a save that supplies neither and
// moves ntfy.url is asking for the stored credential to be posted to a host the payload
// chose — the config-side mirror of TestAIKeyDoesNotFollowTheEndpoint.
func TestNtfyCredsDoNotFollowTheEndpoint(t *testing.T) {
	stored := func(t *testing.T) *Config {
		t.Helper()
		c := testConfig(t)
		c.Ntfy.URL = "https://ntfy.example.com"
		token := "tk_secret"
		c.Ntfy.Token = &token
		return c
	}
	put := func(t *testing.T, h http.Handler, cfg *Config) *httptest.ResponseRecorder {
		t.Helper()
		blob, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(blob)))
		return w
	}

	t.Run("a move with no credential is refused", func(t *testing.T) {
		store := newTestStore(t)
		want := stored(t)
		blob, _ := json.Marshal(want)
		if _, err := store.Put("config", string(blob)); err != nil {
			t.Fatal(err)
		}
		h := alertsHandler(t, store)
		moved := stored(t)
		moved.Ntfy.URL, moved.Ntfy.Token = "https://attacker.invalid", nil
		w := put(t, h, moved)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		saved, err := loadConfigFile(store)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Ntfy.URL != "https://ntfy.example.com" || saved.ntfyToken() != "tk_secret" {
			t.Fatalf("a refused save must not touch the row: %+v", saved.Ntfy)
		}
	})

	t.Run("a move with a credential of its own is allowed", func(t *testing.T) {
		store := newTestStore(t)
		blob, _ := json.Marshal(stored(t))
		if _, err := store.Put("config", string(blob)); err != nil {
			t.Fatal(err)
		}
		h := alertsHandler(t, store)
		moved := stored(t)
		moved.Ntfy.URL = "https://ntfy.new.example.com"
		newToken := "tk_new"
		moved.Ntfy.Token = &newToken
		w := put(t, h, moved)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		saved, _ := loadConfigFile(store)
		if saved.ntfyToken() != "tk_new" {
			t.Errorf("token = %q, want the one the save supplied", saved.ntfyToken())
		}
	})

	t.Run("staying put keeps the credential", func(t *testing.T) {
		store := newTestStore(t)
		blob, _ := json.Marshal(stored(t))
		if _, err := store.Put("config", string(blob)); err != nil {
			t.Fatal(err)
		}
		h := alertsHandler(t, store)
		unrelated := stored(t)
		unrelated.CacheDir = t.TempDir()
		unrelated.Ntfy.Token = nil // the page was never shown it
		w := put(t, h, unrelated)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		saved, _ := loadConfigFile(store)
		if saved.ntfyToken() != "tk_secret" {
			t.Errorf("an omitted token wiped the stored one: %q", saved.ntfyToken())
		}
	})

	// A stored token must not be moved to a new endpoint just because the save touched
	// the *other* credential field. Touching password (even to clear it) is not evidence
	// the token was shown to the page, so the untouched, still-stored token must not be
	// allowed to silently follow ntfy.url to the new host.
	t.Run("a move is refused when only the other credential field is touched", func(t *testing.T) {
		store := newTestStore(t)
		blob, _ := json.Marshal(stored(t))
		if _, err := store.Put("config", string(blob)); err != nil {
			t.Fatal(err)
		}
		h := alertsHandler(t, store)
		moved := stored(t)
		moved.Ntfy.URL, moved.Ntfy.Token = "https://attacker.invalid", nil
		empty := ""
		moved.Ntfy.Password = &empty
		w := put(t, h, moved)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		saved, err := loadConfigFile(store)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Ntfy.URL != "https://ntfy.example.com" || saved.ntfyToken() != "tk_secret" {
			t.Fatalf("a refused save must not touch the row: %+v", saved.Ntfy)
		}
	})
}

// An empty string is the page saying "clear it", distinct from omitting the field, which
// means "leave it alone" — the config-side mirror of the ai.key clear/keep distinction.
func TestNtfyTokenClearVsKeep(t *testing.T) {
	seed := testConfig(t)
	token := "tk_secret"
	seed.Ntfy.Token = &token
	blob, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	if _, err := store.Put("config", string(blob)); err != nil {
		t.Fatal(err)
	}
	h := alertsHandler(t, store)

	// Omitted (nil): the stored token survives an unrelated edit.
	kept := testConfig(t)
	kept.Ntfy.Token = nil
	keptBlob, _ := json.Marshal(kept)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(keptBlob)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	if saved, _ := loadConfigFile(store); saved.ntfyToken() != "tk_secret" {
		t.Errorf("token = %q, want it kept", saved.ntfyToken())
	}

	// Empty string: an explicit clear.
	cleared := testConfig(t)
	empty := ""
	cleared.Ntfy.Token = &empty
	clearedBlob, _ := json.Marshal(cleared)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(clearedBlob)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	if saved, _ := loadConfigFile(store); saved.ntfyToken() != "" {
		t.Errorf("token = %q, want it cleared", saved.ntfyToken())
	}
}

// Import is another way to move ntfy.url or ai.url, and must be judged the same way PUT
// is: a credential the payload does not carry must not follow either endpoint there.
func TestImportCredsDoNotFollowTheEndpoint(t *testing.T) {
	t.Run("ai.key via alerts import", func(t *testing.T) {
		store := newTestStore(t)
		if _, err := store.Put("alerts", `{"ai":{"url":"https://openrouter.ai/api/v1","key":"sk-or-v1-secret","model":"perplexity/sonar"}}`); err != nil {
			t.Fatal(err)
		}
		h := alertsHandler(t, store)
		body := `{"alerts":{"ai":{"url":"https://attacker.invalid/v1","model":"perplexity/sonar","timeout":"20s"},"rules":[]}}`
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		saved, err := LoadAlerts(nil, store)
		if err != nil {
			t.Fatal(err)
		}
		if saved.AI.URL != "https://openrouter.ai/api/v1" || saved.aiKey() != "sk-or-v1-secret" {
			t.Fatalf("a refused import must not touch the row: %+v", saved.AI)
		}
	})

	t.Run("ntfy credential via config import", func(t *testing.T) {
		store := newTestStore(t)
		seed := testConfig(t)
		seed.Ntfy.URL = "https://ntfy.example.com"
		token := "tk_secret"
		seed.Ntfy.Token = &token
		blob, err := json.Marshal(seed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put("config", string(blob)); err != nil {
			t.Fatal(err)
		}
		h := alertsHandler(t, store)

		moved := testConfig(t)
		moved.Ntfy.URL = "https://attacker.invalid"
		movedBlob, err := json.Marshal(moved)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"config":%s}`, movedBlob)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		saved, err := loadConfigFile(store)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Ntfy.URL != "https://ntfy.example.com" || saved.ntfyToken() != "tk_secret" {
			t.Fatalf("a refused import must not touch the row: %+v", saved.Ntfy)
		}
	})
}

// An explicit `"alerts":null` must read the same as the key being omitted entirely:
// json.RawMessage cannot distinguish the two on its own, and decoding the JSON literal
// null onto defaultAlerts() leaves those defaults untouched, so treating null as
// "present" would silently overwrite the stored section — including its rules — with
// defaults. A config-only import that sends "alerts":null must update config and leave
// the stored alerts (and its rules) exactly as they were.
func TestImportNullSectionIsTreatedAsAbsent(t *testing.T) {
	store := newTestStore(t)

	seedAlerts := defaultAlerts()
	seedAlerts.Rules = []Rule{{Name: "watch", ICAO: []string{"a1b2c3"}}}
	alertsBlob, err := json.Marshal(seedAlerts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("alerts", string(alertsBlob)); err != nil {
		t.Fatal(err)
	}

	seedConfig := testConfig(t)
	configBlob, err := json.Marshal(seedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("config", string(configBlob)); err != nil {
		t.Fatal(err)
	}

	h := alertsHandler(t, store)

	updated := testConfig(t)
	updated.Listen = ":9090"
	updatedBlob, err := json.Marshal(updated)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"config":%s,"alerts":null}`, updatedBlob)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	savedConfig, err := loadConfigFile(store)
	if err != nil {
		t.Fatal(err)
	}
	if savedConfig.Listen != ":9090" {
		t.Fatalf("config section was not applied: %+v", savedConfig)
	}

	savedAlerts, err := LoadAlerts(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(savedAlerts.Rules) != 1 || savedAlerts.Rules[0].Name != "watch" {
		t.Fatalf("a null alerts section wiped the stored rules: %+v", savedAlerts.Rules)
	}
}
