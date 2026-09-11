package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- helpers ----------

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := defaultConfig()
	c.Source.URL = "http://source.invalid/data/aircraft.json"
	c.Ntfy.URL = "https://ntfy.invalid"
	c.Ntfy.Topic = "test-topic"
	c.DB.BaseURL = "https://db.invalid/plane-alert-db"
	c.DB.Files = []string{"plane-alert-db.csv"}
	c.CacheDir = t.TempDir()
	return c
}

// requiredEnv is the environment LoadConfig needs before it will start: none of these
// have a default any more. ntfy.topic is left out so tests can supply it either way.
func requiredEnv() []string {
	return []string{
		"SKY_SOURCE_URL=http://source.invalid/data/aircraft.json",
		"SKY_NTFY_URL=https://ntfy.invalid",
		"SKY_DB_BASE_URL=https://db.invalid/plane-alert-db",
		"SKY_DB_FILES=plane-alert-db.csv",
		"SKY_CACHE_DIR=/tmp/sky-notify-test",
	}
}

func mustParse(t *testing.T, csv string) []Plane {
	t.Helper()
	rows, _, err := parseCSV([]byte(csv))
	if err != nil {
		t.Fatalf("parseCSV: %v", err)
	}
	return rows
}

const sampleCSV = `$ICAO,$Registration,$Operator,$Type,$ICAO Type,#CMPG,$Tag 1,$#Tag 2,$#Tag 3,Category,$#Link
ADEB2F,N12345,US Air Force,C-17,C17,Mil,Cargo,Heavy,,Zoomies,https://example.com/a
000004,FAC1282,Colombian Aerospace Force,CASA C-295 M,C295,Mil,Cargo,,,Other Air Forces,ftp://example.com/bad
`

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

// ---------- hex normalization ----------

// A '~' prefix marks a non-ICAO (TIS-B) address. Stripping it would forge a
// legitimate-looking ICAO key and alert on the wrong airframe.
func TestNonICAOAddressDoesNotMatchDatabase(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	listed := true
	alerts.Rules = []Rule{{Name: "listed", Listed: &listed}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))

	if a := Evaluate(Aircraft{Hex: "adeb2f"}, db, alerts, nil); a == nil {
		t.Fatal("plain hex should match the database")
	}
	if a := Evaluate(Aircraft{Hex: "~adeb2f"}, db, alerts, nil); a != nil {
		t.Fatalf("tilde-prefixed address must not match the database, got %+v", a)
	}
	if a := Evaluate(Aircraft{Hex: "~ADEB2F"}, db, alerts, nil); a != nil {
		t.Fatal("uppercase tilde-prefixed address must not match either")
	}
}

func dbWith(t *testing.T, cfg *Config, rows []Plane) *DB {
	t.Helper()
	db := NewDB(cfg, http.DefaultClient)
	db.commit(&snapshot{
		BaseURL: cfg.DB.BaseURL,
		Files:   map[string]*fileEntry{cfg.DB.Files[0]: {Rows: rows}},
	}, time.Now())
	return db
}

// ---------- alt_baro ----------

func TestAltitudeUnmarshal(t *testing.T) {
	for _, tc := range []struct {
		in      string
		present bool
		ground  bool
		feet    int
	}{
		{`{"alt_baro":31000}`, true, false, 31000},
		{`{"alt_baro":"ground"}`, true, true, 0},
		{`{"alt_baro":null}`, false, false, 0},
		{`{}`, false, false, 0},
		{`{"alt_baro":"weird"}`, false, false, 0},
	} {
		var ac Aircraft
		if err := json.Unmarshal([]byte(tc.in), &ac); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		got := ac.AltBaro
		if got.Present != tc.present || got.Ground != tc.ground || got.Feet != tc.feet {
			t.Errorf("%s: got %+v, want present=%v ground=%v feet=%d", tc.in, got, tc.present, tc.ground, tc.feet)
		}
	}
}

// ---------- rules ----------

func TestFiltersFailClosedOnMissingData(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	lat, lon := 51.5, -0.12
	limit := 50.0
	alerts.Lat, alerts.Lon = &lat, &lon
	alerts.Rules = []Rule{{Name: "near", MaxDistanceNM: &limit}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))

	// An explicitly configured distance filter must not admit everything positionless.
	if a := Evaluate(Aircraft{Hex: "adeb2f"}, db, alerts, nil); a != nil {
		t.Error("positionless aircraft must be suppressed when a distance filter is on")
	}
	near, farLon := 51.6, 10.0
	if a := Evaluate(Aircraft{Hex: "adeb2f", Lat: &near, Lon: &lon}, db, alerts, nil); a == nil {
		t.Error("nearby aircraft should pass the distance filter")
	}
	if a := Evaluate(Aircraft{Hex: "adeb2f", Lat: &near, Lon: &farLon}, db, alerts, nil); a != nil {
		t.Error("distant aircraft should be suppressed")
	}
}

// Go's zero value would make an altitude-less aircraft read as being on the ground and
// sail backwards through a min_altitude gate.
func TestAltitudeFilterFailsClosedAndTreatsGroundAsZero(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	minimum := 1000
	alerts.Rules = []Rule{{Name: "airborne", MinAltitudeFt: &minimum}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))

	if a := Evaluate(Aircraft{Hex: "adeb2f"}, db, alerts, nil); a != nil {
		t.Error("missing alt_baro must be suppressed when an altitude filter is on")
	}
	ground := Aircraft{Hex: "adeb2f", AltBaro: Altitude{Present: true, Ground: true}}
	if a := Evaluate(ground, db, alerts, nil); a != nil {
		t.Error(`"ground" must read as 0 ft and fail a 1000ft floor`)
	}
	airborne := Aircraft{Hex: "adeb2f", AltBaro: Altitude{Present: true, Feet: 31000}}
	if a := Evaluate(airborne, db, alerts, nil); a == nil {
		t.Error("airborne aircraft should pass the floor")
	}
}

func TestSilenceIsTheDefault(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	for _, ac := range []Aircraft{{Hex: "adeb2f"}, {Hex: "ffffff"}, {Hex: "ffffff", Squawk: "7700"}} {
		if a := Evaluate(ac, db, alerts, nil); a != nil {
			t.Fatalf("empty rules must be silent, got %+v", a)
		}
	}
}

func TestEmergencyRequiresAndUsesRule(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "emergency", Squawk: []string{"7500", "7600", "7700"}}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	if a := Evaluate(Aircraft{Hex: "ffffff", Squawk: "7700"}, db, alerts, nil); a == nil || !a.Emergency || a.Trigger != "emergency" {
		t.Fatalf("squawk rule should produce framed emergency, got %+v", a)
	}
}

func intp(v int) *int { return &v }

func TestRuleMatching(t *testing.T) {
	p := &Plane{ICAO: "adeb2f", Reg: "N12345", Operator: "US Air Force", Type: "Tupolev Tu-154 B-2", ICAOType: "C17", CMPG: "Mil", Category: "Zoomies", Tags: []string{"Cargo", "Heavy"}}
	ac := Aircraft{Hex: p.ICAO}
	a := &Alert{}
	for _, tc := range []struct {
		name      string
		rules     []Rule
		wantMatch bool
	}{
		{"or within and across fields", []Rule{{CMPG: []string{"Gov", "Mil"}, ICAOType: []string{"C17"}, Priority: intp(3)}}, true},
		{"and rejects one mismatched field", []Rule{{CMPG: []string{"Mil"}, ICAOType: []string{"B52"}, Priority: intp(3)}}, false},
		{"exact not substring", []Rule{{Type: []string{"B-2 Spirit"}, Priority: intp(3)}}, false},
		{"case insensitive field", []Rule{{Operator: []string{" us air force "}, Priority: intp(3)}}, true},
		{"case insensitive tag", []Rule{{Tags: []string{" heavy "}, Priority: intp(3)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := firstMatch(tc.rules, ac, p, a)
			if (got != nil) != tc.wantMatch {
				t.Errorf("match = %v, want %v", got != nil, tc.wantMatch)
			}
		})
	}
}

func TestPriorityRulesAndSquawks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rules     []Rule
		squawk    string
		wantAlert bool
		wantPri   int
	}{
		{"first match wins", []Rule{{Name: "first", ICAO: []string{"adeb2f"}, Priority: intp(2)}, {Name: "second", ICAO: []string{"adeb2f"}, Priority: intp(4)}}, "", true, 2},
		{"rules exclude unmatched aircraft", []Rule{{Name: "other", ICAO: []string{"ffffff"}, Priority: intp(2)}}, "", false, 0},
		{"later matching rule alerts", []Rule{{Name: "other", ICAO: []string{"ffffff"}, Priority: intp(2)}, {Name: "match", ICAO: []string{"adeb2f"}, Priority: intp(4)}}, "", true, 4},
		{"first match mutes and stops", []Rule{{Name: "mute", ICAO: []string{"adeb2f"}, Priority: intp(0)}, {Name: "broad", Listed: boolp(true)}}, "", false, 0},
		{"squawk rule alerts", []Rule{{Name: "emergency", Squawk: []string{"7700"}, Priority: intp(4)}}, "7700", true, 4},
		{"no rules is silent", nil, "7700", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			alerts := defaultAlerts()
			alerts.Rules = tc.rules
			db := dbWith(t, cfg, mustParse(t, sampleCSV))
			a := Evaluate(Aircraft{Hex: "adeb2f", Squawk: tc.squawk}, db, alerts, nil)
			if (a != nil) != tc.wantAlert {
				t.Fatalf("alert = %+v, want alert %v", a, tc.wantAlert)
			}
			if a != nil && a.Priority != tc.wantPri {
				t.Errorf("priority = %d, want %d", a.Priority, tc.wantPri)
			}
		})
	}
}

func boolp(v bool) *bool { return &v }

func TestListedAndFeedOnlyRules(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	for _, tc := range []struct {
		listed *bool
		hex    string
		want   bool
	}{{boolp(true), "adeb2f", true}, {boolp(true), "ffffff", false}, {boolp(false), "ffffff", true}, {nil, "ffffff", true}} {
		a := defaultAlerts()
		a.Rules = []Rule{{Name: "rule", Listed: tc.listed, ICAO: []string{tc.hex}}}
		if got := Evaluate(Aircraft{Hex: tc.hex}, db, a, nil); (got != nil) != tc.want {
			t.Errorf("listed=%v hex=%s alert=%+v", tc.listed, tc.hex, got)
		}
	}
}

func TestUnlistedAircraftMatchesFeedFieldsAndLimits(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, nil)
	lat, lon, maxDistance := 32.0, -96.0, 3.0
	minAltitude, maxAltitude := 0, 2000
	alerts := defaultAlerts()
	alerts.Lat, alerts.Lon = &lat, &lon
	alerts.Rules = []Rule{{Name: "low flyer", Reg: []string{"N1"}, ICAOType: []string{"C172"}, MinAltitudeFt: &minAltitude, MaxAltitudeFt: &maxAltitude, MaxDistanceNM: &maxDistance}}
	ac := Aircraft{Hex: "ffffff", Reg: "n1", Type: "c172", Lat: &lat, Lon: &lon, AltBaro: Altitude{Present: true, Feet: 1000}}
	if got := Evaluate(ac, db, alerts, nil); got == nil || got.Plane != nil {
		t.Fatalf("feed-only rule should match unlisted aircraft, got %+v", got)
	}
}

// reg and icao_type exist in both the database and the feed and routinely disagree;
// either source satisfying the rule has to count, or a rule written from the database
// silently stops matching the moment the aircraft broadcasts something different.
func TestRegAndICAOTypeMatchFromEitherSource(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	listed := Aircraft{Hex: "adeb2f", Reg: "OTHER", Type: "OTHER"}

	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "by database reg", Reg: []string{"N12345"}}}
	if got := Evaluate(listed, db, alerts, nil); got == nil {
		t.Error("the database registration should match even when the feed disagrees")
	}
	alerts.Rules = []Rule{{Name: "by feed reg", Reg: []string{"other"}}}
	if got := Evaluate(listed, db, alerts, nil); got == nil {
		t.Error("the feed registration should match even when the database disagrees")
	}
	alerts.Rules = []Rule{{Name: "by database type", ICAOType: []string{"C17"}}}
	if got := Evaluate(listed, db, alerts, nil); got == nil {
		t.Error("the database ICAO type should match even when the feed disagrees")
	}
}

func TestPerRuleLimitsSelectLaterRule(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, nil)
	low, high, ceiling := 0, 3000, 10000
	alerts := defaultAlerts()
	alerts.Rules = []Rule{
		{Name: "low", ICAO: []string{"ffffff"}, MaxAltitudeFt: &low, Priority: intp(2)},
		{Name: "high", ICAO: []string{"ffffff"}, MinAltitudeFt: &high, MaxAltitudeFt: &ceiling, Priority: intp(4)},
	}
	got := Evaluate(Aircraft{Hex: "ffffff", AltBaro: Altitude{Present: true, Feet: 5000}}, db, alerts, nil)
	if got == nil || got.Trigger != "high" || got.Priority != 4 {
		t.Fatalf("later altitude rule should win, got %+v", got)
	}
}

func TestDistanceLimitFailsClosedThenEvaluationContinues(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, nil)
	limit := 3.0
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "near", ICAO: []string{"ffffff"}, MaxDistanceNM: &limit}, {Name: "anywhere", ICAO: []string{"ffffff"}}}
	got := Evaluate(Aircraft{Hex: "ffffff"}, db, alerts, nil)
	if got == nil || got.Trigger != "anywhere" {
		t.Fatalf("missing position should fail only the limited rule, got %+v", got)
	}
}

func TestHaversine(t *testing.T) {
	// LHR to JFK is ~2990 NM.
	got := haversineNM(51.4700, -0.4543, 40.6413, -73.7781)
	if got < 2960 || got > 3020 {
		t.Errorf("LHR-JFK: got %.0f NM, want ~2990", got)
	}
	if d := haversineNM(51.5, -0.12, 51.5, -0.12); d != 0 {
		t.Errorf("identical points should be 0, got %v", d)
	}
}

// ---------- cooldown ----------

// ---------- motion ----------

func TestClosestApproach(t *testing.T) {
	lat, lon := 51.5, -0.12
	south := lat - 10.0/60 // 10 NM south
	for _, tc := range []struct {
		name    string
		heading float64
		horizon time.Duration
		wantNM  float64
		wantIn  time.Duration
	}{
		{"heading straight over", 0, 5 * time.Minute, 0, 5 * time.Minute},
		{"crossing", 90, 5 * time.Minute, 10, 0},
		{"receding", 180, 5 * time.Minute, 10, 0},
		{"clamped to horizon", 0, 2 * time.Minute, 6, 2 * time.Minute},
	} {
		nm, in := closestApproach(lat, lon, south, lon, 120, tc.heading, tc.horizon)
		if math.Abs(nm-tc.wantNM) > 0.1 || (in-tc.wantIn).Abs() > 5*time.Second {
			t.Errorf("%s: got %.2f NM in %s, want %.0f NM in %s", tc.name, nm, in, tc.wantNM, tc.wantIn)
		}
	}
}

// fly integrates 15s steps at gs knots; turns[i] is the heading change before step i.
func fly(gs float64, turns []float64) *track {
	lat, lon, hdg := 51.5, -0.12, 0.0
	tk := &track{}
	for i, turn := range turns {
		hdg = math.Mod(hdg+turn+360, 360)
		d := gs * 15 / 3600
		lat += d * math.Cos(hdg*math.Pi/180) / 60
		lon += d * math.Sin(hdg*math.Pi/180) / (60 * math.Cos(lat*math.Pi/180))
		tk.samples = append(tk.samples, sample{time.Unix(int64(i*15), 0), lat, lon, hdg})
	}
	return tk
}

func repeat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestCirclingDetection(t *testing.T) {
	// A holding pattern turns a full circle too, but over miles: two 6 NM legs.
	var hold []float64
	for range 2 {
		hold = append(hold, repeat(8, 0)...)
		hold = append(hold, repeat(6, 30)...)
	}
	gapped := fly(60, repeat(20, 30))
	for i := 10; i < len(gapped.samples); i++ {
		gapped.samples[i].t = gapped.samples[i].t.Add(2 * time.Minute)
	}
	for _, tc := range []struct {
		name string
		tk   *track
		want bool
	}{
		{"half-mile orbit", fly(60, repeat(20, 30)), true},
		{"left-hand orbit", fly(60, repeat(20, -30)), true},
		{"straight line", fly(120, repeat(20, 0)), false},
		{"holding pattern", fly(180, hold), false},
		{"gap splits the orbit", gapped, false},
		{"no history", nil, false},
	} {
		if got := tc.tk.circling(); got != tc.want {
			t.Errorf("%s: circling = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTrackerRecordsNewPositionsAndForgets(t *testing.T) {
	lat, lon, hdg := 51.5, -0.12, 90.0
	tr := NewTracker()
	at := func(now, seenPos float64) *feed {
		return &feed{Now: now, Aircraft: []Aircraft{{Hex: "ABCDEF", Lat: &lat, Lon: &lon, Track: &hdg, SeenPos: seenPos}}}
	}
	tr.Update(at(1000, 0))
	tr.Update(at(1000, 0))
	tr.Update(at(1015, 15)) // readsb repeating the same position
	if n := len(tr.get("abcdef").samples); n != 1 {
		t.Fatalf("repeated positions must be one sample, got %d", n)
	}
	tr.Update(at(1030, 0))
	if n := len(tr.get("abcdef").samples); n != 2 {
		t.Fatalf("a new position must be recorded, got %d samples", n)
	}
	tr.Update(&feed{Now: 1030 + circleWindow.Seconds() + 1})
	if tr.get("abcdef") != nil {
		t.Fatal("a track with nothing left in the window must be forgotten")
	}
}

func TestMotionRulesFailClosedAndRecordPrediction(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, nil)
	lat, lon, within := 51.5, -0.12, 1.0
	alerts := defaultAlerts()
	alerts.Lat, alerts.Lon = &lat, &lon
	alerts.Rules = []Rule{{Name: "overhead", PassesWithinNM: &within}}

	south, north := lat-10.0/60, 0.0
	moving := Aircraft{Hex: "ffffff", Lat: &south, Lon: &lon, GS: 120}
	if a := Evaluate(moving, db, alerts, nil); a != nil {
		t.Error("a moving aircraft without a ground track must fail closed")
	}
	moving.Track = &north
	a := Evaluate(moving, db, alerts, nil)
	if a == nil || !a.HasPass || a.PassNM > 0.1 || a.PassIn < 4*time.Minute {
		t.Fatalf("inbound aircraft should match with its prediction recorded, got %+v", a)
	}

	alerts.Rules = []Rule{{Name: "orbit", Circling: boolp(true)}}
	if a := Evaluate(moving, db, alerts, nil); a != nil {
		t.Error("no history must not read as circling")
	}
	if a := Evaluate(moving, db, alerts, fly(60, repeat(20, 30))); a == nil || !a.Circling || a.HasPass {
		t.Errorf("orbiting aircraft should match the circling rule alone, got %+v", a)
	}
}

func TestCooldownStateMachine(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := NewState(dir)
	s.Now = func() time.Time { return now }
	cooldown := 24 * time.Hour

	key := cooldownKey("adeb2f", "listed")
	if !s.Eligible(key, cooldown) {
		t.Fatal("first sighting should be eligible")
	}
	s.Record(key, cooldown)
	if s.Eligible(key, cooldown) {
		t.Fatal("should be suppressed inside the cooldown window")
	}
	now = now.Add(23 * time.Hour)
	if s.Eligible(key, cooldown) {
		t.Fatal("still inside the window at 23h")
	}
	now = now.Add(2 * time.Hour)
	if !s.Eligible(key, cooldown) {
		t.Fatal("should be eligible again after the window")
	}
}

// The rule name is the cooldown key, so a routine sighting must not mute the emergency
// rule that catches the same airframe an hour later.
func TestCooldownsAreIndependentPerRule(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	alerts.Rules = []Rule{
		{Name: "emergency", Squawk: []string{"7700"}},
		{Name: "listed", Listed: boolp(true)},
	}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	s := NewState(t.TempDir())
	cooldown := alerts.Cooldown.Std()

	// A quiet pass matches the second rule; only that rule's cooldown advances.
	routine := Evaluate(Aircraft{Hex: "adeb2f"}, db, alerts, nil)
	if routine == nil || routine.Trigger != "listed" {
		t.Fatalf("want the listed rule, got %+v", routine)
	}
	s.Record(cooldownKey(routine.Hex, routine.Trigger), cooldown)

	// The same airframe squawking 7700 now matches the first rule, on its own key.
	emergency := Evaluate(Aircraft{Hex: "adeb2f", Squawk: "7700"}, db, alerts, nil)
	if emergency == nil || emergency.Trigger != "emergency" {
		t.Fatalf("want the emergency rule, got %+v", emergency)
	}
	if !s.Eligible(cooldownKey(emergency.Hex, emergency.Trigger), cooldown) {
		t.Error("a routine alert must not suppress a different rule's alert")
	}
	if s.Eligible(cooldownKey(routine.Hex, routine.Trigger), cooldown) {
		t.Error("the routine rule should still be in its own cooldown")
	}
}

// One notification, not two: first match wins, and the alert still carries the database
// metadata that makes the message worth reading.
func TestListedAircraftInEmergencyProducesOneAlert(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "emergency", Squawk: []string{"7700"}}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	s := NewState(t.TempDir())
	cooldown := alerts.Cooldown.Std()

	a := Evaluate(Aircraft{Hex: "adeb2f", Squawk: "7700"}, db, alerts, nil)
	if a == nil || !a.Emergency {
		t.Fatal("want an emergency alert")
	}
	if a.Plane == nil {
		t.Fatal("emergency alert on a listed aircraft should still carry its metadata")
	}
	key := cooldownKey(a.Hex, a.Trigger)
	s.Record(key, cooldown)
	if s.Eligible(key, cooldown) {
		t.Error("the matching rule cooldown should have been advanced")
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cooldown := 24 * time.Hour
	s1 := NewState(dir)
	s1.Record(cooldownKey("adeb2f", "listed"), cooldown)

	s2 := NewState(dir)
	s2.Load(cooldown)
	if s2.Eligible(cooldownKey("adeb2f", "listed"), cooldown) {
		t.Fatal("cooldown should survive a restart")
	}
}

func TestCorruptStateIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o644)
	s := NewState(dir)
	s.Load(24 * time.Hour)
	if !s.Eligible(cooldownKey("adeb2f", "listed"), 24*time.Hour) {
		t.Fatal("corrupt state should degrade to an empty ledger, not a failure")
	}
}

// If ntfy succeeds but the disk write fails, the in-memory cooldown must still advance:
// re-queueing would republish the same notification every poll.
func TestStateWriteFailureStillAdvancesInMemoryCooldown(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewState(filepath.Join(blocked, "sub")) // MkdirAll under a regular file fails
	cooldown := 24 * time.Hour
	key := cooldownKey("adeb2f", "listed")

	s.Record(key, cooldown)
	if s.Eligible(key, cooldown) {
		t.Fatal("in-memory cooldown must advance even when persistence fails")
	}
	if s.WriteErr() == nil {
		t.Fatal("the write failure should be recorded for /healthz")
	}
}

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
	if a := Evaluate(Aircraft{Hex: "adeb2f"}, db, live.Get(), nil); a == nil || a.Priority != 5 {
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
		{"missing name", "rules:\n  - listed: true\n"},
		{"duplicate name", "rules:\n  - name: same\n    listed: true\n  - name: same\n    listed: false\n"},
		{"no conditions", "rules:\n  - name: empty\n"},
		{"distance without coordinates", "rules:\n  - name: distance\n    max_distance_nm: 3\n"},
		{"inverted altitude", "rules:\n  - name: altitude\n    min_altitude_ft: 100\n    max_altitude_ft: 100\n"},
		{"bad priority", "rules:\n  - name: priority\n    listed: true\n    priority: 9\n"},
		{"overhead without coordinates", "rules:\n  - name: overhead\n    passes_within_nm: 1\n"},
		{"horizon without distance", "lat: 0\nlon: 0\nrules:\n  - name: overhead\n    listed: true\n    passes_within: 5m\n"},
		{"zero horizon", "lat: 0\nlon: 0\nrules:\n  - name: overhead\n    passes_within_nm: 1\n    passes_within: 0s\n"},
		{"non-finite overhead", "lat: 0\nlon: 0\nrules:\n  - name: overhead\n    passes_within_nm: .nan\n"},
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

// ---------- aircraft.json ----------

func feedJSON(now time.Time, body string) string {
	return fmt.Sprintf(`{"now":%d,"messages":1,"aircraft":[%s]}`, now.Unix(), body)
}

func TestSourceFreshnessGateIsTwoSided(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	for _, tc := range []struct {
		name    string
		docTime time.Time
		wantErr bool
	}{
		{"fresh", now, false},
		{"stale past", now.Add(-5 * time.Minute), true},
		// A frozen file dated in the future would otherwise stay healthy forever.
		{"stale future", now.Add(5 * time.Minute), true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, feedJSON(tc.docTime, `{"hex":"adeb2f"}`))
		}))
		cfg.Source.URL = srv.URL
		_, err := NewSource(cfg, srv.Client()).Fetch(context.Background(), now)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: got err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
		srv.Close()
	}
}

func TestSourceRejectsTruncatedDocument(t *testing.T) {
	cfg := testConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"now":1,"aircraft":[{"hex":"adeb`)
	}))
	defer srv.Close()
	cfg.Source.URL = srv.URL
	if _, err := NewSource(cfg, srv.Client()).Fetch(context.Background(), time.Now()); err == nil {
		t.Fatal("a truncated document must be an error, not a short aircraft list")
	}
}

func TestSourceToleratesBOM(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "\xef\xbb\xbf"+feedJSON(now, `{"hex":"adeb2f"}`))
	}))
	defer srv.Close()
	cfg.Source.URL = srv.URL
	f, err := NewSource(cfg, srv.Client()).Fetch(context.Background(), now)
	if err != nil {
		t.Fatalf("a BOM-prefixed document must still decode: %v", err)
	}
	if len(f.Aircraft) != 1 {
		t.Fatalf("got %d aircraft, want 1", len(f.Aircraft))
	}
}

func TestReadLimitedRejectsOversize(t *testing.T) {
	if _, err := readLimited(strings.NewReader("hello"), 3); err == nil {
		t.Fatal("want an error when the source exceeds the limit")
	}
	if b, err := readLimited(strings.NewReader("hi"), 3); err != nil || string(b) != "hi" {
		t.Fatalf("under-limit read failed: %q %v", b, err)
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

// ---------- notifier ----------

type ntfyServer struct {
	statuses []int // consumed in order; the last one repeats
	retryHdr string
	bodies   []ntfyMessage
	paths    []string
	hits     int
}

func (n *ntfyServer) start(t *testing.T, cfg *Config) *Notifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m ntfyMessage
		json.NewDecoder(r.Body).Decode(&m)
		n.bodies = append(n.bodies, m)
		n.paths = append(n.paths, r.URL.Path)
		st := http.StatusOK
		if n.hits < len(n.statuses) {
			st = n.statuses[n.hits]
		} else if len(n.statuses) > 0 {
			st = n.statuses[len(n.statuses)-1]
		}
		n.hits++
		if n.retryHdr != "" {
			w.Header().Set("Retry-After", n.retryHdr)
		}
		if st >= 300 && st < 400 {
			w.Header().Set("Location", "https://elsewhere.invalid/x")
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(srv.Close)
	cfg.Ntfy.URL = srv.URL
	nt, err := NewNotifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	nt.client = srv.Client()
	nt.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return nt
}

func testAlert() *Alert {
	return &Alert{
		Hex:      "adeb2f",
		Trigger:  "listed",
		Priority: 2,
		Plane:    &Plane{ICAO: "adeb2f", Reg: "N12345", Operator: "US Air Force", Type: "C-17", Link: "ftp://evil.invalid/x"},
		AC:       Aircraft{Hex: "adeb2f", Flight: "RCH123 ", AltBaro: Altitude{Present: true, Feet: 31000}},

		DistanceNM:  12.34,
		HasDistance: true,
	}
}

func TestNotifySuccess(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{}
	n := s.start(t, cfg)
	if err := n.Publish(context.Background(), testAlert()); err != nil {
		t.Fatal(err)
	}
	if len(s.bodies) != 1 {
		t.Fatalf("want 1 publish, got %d", len(s.bodies))
	}
	// ntfy parses a JSON publish document only at the server root. POSTing it to
	// /<topic> returns 200 and delivers the raw JSON as the message text.
	if s.paths[0] != "/" {
		t.Errorf("publish must POST to the server root, got %q", s.paths[0])
	}
	m := s.bodies[0]
	if m.Topic != cfg.Ntfy.Topic || m.Title != "US Air Force C-17" {
		t.Errorf("unexpected message: %+v", m)
	}
	if m.Priority != 2 {
		t.Errorf("priority = %d, want alert priority 2", m.Priority)
	}
	if !strings.Contains(m.Message, "N12345") || !strings.Contains(m.Message, "31000 ft") ||
		!strings.Contains(m.Message, "12.3 NM") {
		t.Errorf("body missing detail: %q", m.Message)
	}
	// The hex is phone-screen noise: it identifies nothing a human reads, and the
	// click target already carries it to tar1090.
	if strings.Contains(m.Message, "adeb2f") {
		t.Errorf("body should not carry the ICAO hex: %q", m.Message)
	}
	// plane-alert-db links are untrusted: a non-http scheme must be dropped.
	if m.Click != "" {
		t.Errorf("ftp:// click target should have been dropped, got %q", m.Click)
	}
	if n.LastErr() != nil {
		t.Error("a successful publish should clear the error")
	}
}

func TestNotifyRetryClassification(t *testing.T) {
	t.Run("429 then success", func(t *testing.T) {
		cfg := testConfig(t)
		s := &ntfyServer{statuses: []int{http.StatusTooManyRequests, http.StatusOK}, retryHdr: "1"}
		n := s.start(t, cfg)
		if err := n.Publish(context.Background(), testAlert()); err != nil {
			t.Fatalf("429 should be retried: %v", err)
		}
		if s.hits != 2 {
			t.Errorf("want 2 attempts, got %d", s.hits)
		}
	})
	t.Run("403 is not retried", func(t *testing.T) {
		cfg := testConfig(t)
		s := &ntfyServer{statuses: []int{http.StatusForbidden}}
		n := s.start(t, cfg)
		if err := n.Publish(context.Background(), testAlert()); err == nil {
			t.Fatal("403 should fail")
		}
		if s.hits != 1 {
			t.Errorf("403 must not be retried, got %d attempts", s.hits)
		}
		if n.LastErr() == nil {
			t.Error("health must see a non-retryable failure")
		}
	})
	t.Run("exhausted retryable failure is still recorded", func(t *testing.T) {
		cfg := testConfig(t)
		s := &ntfyServer{statuses: []int{http.StatusBadGateway}}
		n := s.start(t, cfg)
		if err := n.Publish(context.Background(), testAlert()); err == nil {
			t.Fatal("sustained 502 should fail")
		}
		// A wedged sink starves alerts exactly as completely as a bad token.
		if n.LastErr() == nil {
			t.Error("an exhausted retryable failure must also degrade health")
		}
	})
}

// Go's default would downgrade a redirected POST to a GET and return 200, writing the
// cooldown for something that was never published.
func TestNotifyDoesNotFollowRedirects(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{statuses: []int{http.StatusFound}}
	n := s.start(t, cfg)
	if err := n.Publish(context.Background(), testAlert()); err == nil {
		t.Fatal("a redirect must be reported as a failure, not silently accepted")
	}
	if s.hits != 1 {
		t.Errorf("want 1 attempt, got %d", s.hits)
	}
}

func TestNotifyEmergencyFraming(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{}
	n := s.start(t, cfg)
	a := testAlert()
	a.Trigger, a.Emergency, a.Squawk, a.SquawkMeans = "emergency:7700", true, "7700", "general emergency"
	a.Priority = 4

	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	m := s.bodies[0]
	if m.Priority != 4 {
		t.Errorf("emergency priority should come from the alert, got %d", m.Priority)
	}
	if !strings.Contains(strings.Join(m.Tags, ","), "rotating_light") {
		t.Errorf("emergency should carry the alert tag, got %v", m.Tags)
	}
	if !strings.HasPrefix(m.Title, "SQUAWK 7700") {
		t.Errorf("title should lead with the squawk, got %q", m.Title)
	}
}

func TestClickURLPrefersTar1090(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tar1090URL = "https://tar1090.example.com/"
	s := &ntfyServer{}
	n := s.start(t, cfg)
	if err := n.Publish(context.Background(), testAlert()); err != nil {
		t.Fatal(err)
	}
	if got := s.bodies[0].Click; got != "https://tar1090.example.com/?icao=adeb2f" {
		t.Errorf("click url: got %q", got)
	}
}

func TestAuthHeaders(t *testing.T) {
	for _, tc := range []struct {
		name              string
		token, user, pass string
		check             func(*testing.T, *http.Request)
	}{
		{"bearer", "tok", "", "", func(t *testing.T, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("want bearer, got %q", r.Header.Get("Authorization"))
			}
		}},
		{"basic", "", "u", "p", func(t *testing.T, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != "u" || p != "p" {
				t.Errorf("want basic auth u/p, got %q/%q ok=%v", u, p, ok)
			}
		}},
		{"none", "", "", "", func(t *testing.T, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				t.Error("no auth expected")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Ntfy.Token, cfg.Ntfy.User, cfg.Ntfy.Password = tc.token, tc.user, tc.pass
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.check(t, r)
			}))
			defer srv.Close()
			cfg.Ntfy.URL = srv.URL
			n, err := NewNotifier(cfg)
			if err != nil {
				t.Fatal(err)
			}
			n.client = srv.Client()
			if err := n.Publish(context.Background(), testAlert()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// ---------- queue ----------

func alertFor(hex, trigger string, emergency bool) *Alert {
	return &Alert{Hex: hex, Trigger: trigger, Emergency: emergency}
}

func TestQueueDeduplicatesInFlightKey(t *testing.T) {
	q := newQueue(10)
	a := alertFor("adeb2f", "listed", false)
	q.add(a)

	got := q.take()
	if got == nil {
		t.Fatal("want an alert")
	}
	// The poll loop runs again while delivery is in flight.
	q.add(alertFor("adeb2f", "listed", false))
	if q.depth() != 1 {
		t.Fatalf("an in-flight key must not be re-enqueued, depth=%d", q.depth())
	}
	if q.take() != nil {
		t.Fatal("an in-flight entry must not be handed out twice")
	}
	q.done(cooldownKey("adeb2f", "listed"))
	if q.depth() != 0 {
		t.Fatal("done should remove the entry")
	}
}

// Map iteration order is random, so without explicit priority an urgent squawk could sit
// behind arbitrary routine traffic — or be dropped when the queue fills.
func TestQueuePrioritisesAndProtectsEmergencies(t *testing.T) {
	q := newQueue(3)
	for i := 0; i < 3; i++ {
		q.add(alertFor(fmt.Sprintf("00000%d", i), "listed", false))
	}
	if q.depth() != 3 {
		t.Fatalf("want a full queue, got %d", q.depth())
	}

	// Full queue: an ordinary alert is dropped, an emergency evicts one instead.
	q.add(alertFor("aaaaaa", "listed", false))
	if q.depth() != 3 {
		t.Errorf("ordinary alert should have been dropped, depth=%d", q.depth())
	}
	q.add(alertFor("bbbbbb", "emergency:7700", true))
	if q.depth() != 3 {
		t.Errorf("emergency should have evicted an ordinary alert, depth=%d", q.depth())
	}

	got := q.take()
	if got == nil || !got.Emergency {
		t.Fatalf("the emergency must be drained first, got %+v", got)
	}
}

func TestQueueDoesNotEvictInflightOrOtherEmergencies(t *testing.T) {
	q := newQueue(2)
	q.add(alertFor("000001", "listed", false))
	inflight := q.take() // marks 000001 in flight
	if inflight == nil {
		t.Fatal("want an alert")
	}
	q.add(alertFor("bbbbbb", "emergency:7700", true))

	// Queue is full (one in-flight, one emergency); a second emergency has nothing
	// evictable and must be dropped rather than displacing either.
	q.add(alertFor("cccccc", "emergency:7500", true))
	if q.depth() != 2 {
		t.Errorf("must not evict an in-flight or emergency entry, depth=%d", q.depth())
	}
}
