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

// ---------- hex normalization ----------

// A '~' prefix marks a non-ICAO (TIS-B) address. Stripping it would forge a
// legitimate-looking ICAO key and alert on the wrong airframe.
func TestNonICAOAddressDoesNotMatchDatabase(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	listed := true
	alerts.Rules = []Rule{{Name: "listed", Listed: &listed}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))

	if a := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, alerts, nil); a == nil {
		t.Fatal("plain hex should match the database")
	}
	if a := Evaluate(at(Aircraft{Hex: "~adeb2f"}), db, alerts, nil); a != nil {
		t.Fatalf("tilde-prefixed address must not match the database, got %+v", a)
	}
	if a := Evaluate(at(Aircraft{Hex: "~ADEB2F"}), db, alerts, nil); a != nil {
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

	if a := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, alerts, nil); a != nil {
		t.Error("missing alt_baro must be suppressed when an altitude filter is on")
	}
	ground := at(Aircraft{Hex: "adeb2f", AltBaro: Altitude{Present: true, Ground: true}})
	if a := Evaluate(ground, db, alerts, nil); a != nil {
		t.Error(`"ground" must read as 0 ft and fail a 1000ft floor`)
	}
	airborne := at(Aircraft{Hex: "adeb2f", AltBaro: Altitude{Present: true, Feet: 31000}})
	if a := Evaluate(airborne, db, alerts, nil); a == nil {
		t.Error("airborne aircraft should pass the floor")
	}
}

func TestSilenceIsTheDefault(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	for _, ac := range []Aircraft{{Hex: "adeb2f"}, {Hex: "ffffff"}, {Hex: "ffffff", Squawk: "7700"}} {
		if a := Evaluate(at(ac), db, alerts, nil); a != nil {
			t.Fatalf("empty rules must be silent, got %+v", a)
		}
	}
}

// A matching aircraft with no position is held, not dropped: the next poll re-evaluates
// it, and nothing has recorded a cooldown in the meantime.
func TestAlertsWaitForPositionAndDistance(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	alerts.Lat, alerts.Lon = &testLat, &testLon
	alerts.Rules = []Rule{{Name: "listed", Listed: boolp(true)}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))

	if a := Evaluate(Aircraft{Hex: "adeb2f"}, db, alerts, nil); a != nil {
		t.Fatalf("an aircraft without a position must not alert, got %+v", a)
	}
	a := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, alerts, nil)
	if a == nil || !a.HasDistance {
		t.Fatalf("the same aircraft with a position should alert with a distance, got %+v", a)
	}
}

// The wildcard is what a rule says by saying nothing: no conditions, every aircraft.
func TestWildcardRule(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "everything interesting", Listed: boolp(true)}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))

	if a := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, alerts, nil); a == nil {
		t.Fatal("a wildcard rule should match a listed aircraft")
	}
	if a := Evaluate(at(Aircraft{Hex: "ffffff"}), db, alerts, nil); a != nil {
		t.Fatalf("listed: true still excludes unlisted aircraft, got %+v", a)
	}

	alerts.Rules = []Rule{{Name: "everything"}}
	if err := alerts.validate(); err != nil {
		t.Fatalf("a rule with no conditions is the wildcard, not an error: %v", err)
	}
	if a := Evaluate(at(Aircraft{Hex: "ffffff"}), db, alerts, nil); a == nil {
		t.Fatal("a rule with no conditions should match any aircraft")
	}
	// An empty rules list stays silent: there is no rule to reach.
	alerts.Rules = nil
	if a := Evaluate(at(Aircraft{Hex: "ffffff"}), db, alerts, nil); a != nil {
		t.Fatalf("no rules must alert on nothing, got %+v", a)
	}
	// all: true was the old spelling. It has to name its replacement, not read as a typo.
	alerts.Rules = []Rule{{Name: "old wildcard", All: boolp(true)}}
	err := alerts.validate()
	if err == nil || !strings.Contains(err.Error(), "no conditions") {
		t.Fatalf("all: true should be rejected with a migration note, got %v", err)
	}
}

func TestEmergencyRequiresAndUsesRule(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "emergency", Squawk: []string{"7500", "7600", "7700"}}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	if a := Evaluate(at(Aircraft{Hex: "ffffff", Squawk: "7700"}), db, alerts, nil); a == nil || !a.Emergency || a.Trigger != "emergency" {
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
			a := Evaluate(at(Aircraft{Hex: "adeb2f", Squawk: tc.squawk}), db, alerts, nil)
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

// Every alert now waits for a position, so test aircraft carry one unless the test is
// about what happens without one.
var testLat, testLon = 51.5, -0.12

func at(ac Aircraft) Aircraft {
	ac.Lat, ac.Lon = &testLat, &testLon
	return ac
}

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
		if got := Evaluate(at(Aircraft{Hex: tc.hex}), db, a, nil); (got != nil) != tc.want {
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
	listed := at(Aircraft{Hex: "adeb2f", Reg: "OTHER", Type: "OTHER"})

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

// callsign is the one condition that is not exact: the feed carries SWA2504 and the
// thing worth asking for is SWA. A prefix, not a substring — WA must not catch
// Southwest — and a blank must not quietly widen a rule to everything overhead.
func TestCallsignMatchesOnItsStart(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	alerts := defaultAlerts()

	for _, tc := range []struct {
		name   string
		want   []string
		flight string
		match  bool
	}{
		{"airline prefix", []string{"SWA"}, "SWA2504 ", true},
		{"case and space ignored", []string{" swa "}, "swa900", true},
		{"the whole callsign", []string{"SWA2504"}, "SWA2504", true},
		{"another airline", []string{"SWA"}, "ASA100", false},
		{"not a substring", []string{"WA"}, "SWA2504", false},
		{"one of several", []string{"ASA", "SWA"}, "SWA11", true},
		{"blank matches nothing", []string{""}, "SWA2504", false},
		{"blank beside a real value", []string{"", "SWA"}, "SWA2504", true},
		{"no callsign broadcast", []string{"SWA"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alerts.Rules = []Rule{{Name: "by callsign", Callsign: tc.want}}
			got := Evaluate(at(Aircraft{Hex: "ffffff", Flight: tc.flight}), db, alerts, nil)
			if (got != nil) != tc.match {
				t.Errorf("callsign %q against flight %q: matched %v, want %v", tc.want, tc.flight, got != nil, tc.match)
			}
		})
	}
}

// A database row has no callsign, so the preview has to drop the condition rather than
// fail it closed — otherwise every callsign rule previews as zero and reads as dead.
func TestCallsignRulePreviewsAgainstTheDatabase(t *testing.T) {
	rows := mustParse(t, sampleCSV)
	rule := Rule{Callsign: []string{"SWA"}, CMPG: []string{"Mil"}}
	if !rule.matchesPlane(&rows[0]) {
		t.Error("a callsign rule should still preview the database rows its other conditions select")
	}
	rule.CMPG = []string{"Civ"}
	if rule.matchesPlane(&rows[0]) {
		t.Error("dropping the callsign must not drop the rest of the rule")
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
	got := Evaluate(at(Aircraft{Hex: "ffffff", AltBaro: Altitude{Present: true, Feet: 5000}}), db, alerts, nil)
	if got == nil || got.Trigger != "high" || got.Priority != 4 {
		t.Fatalf("later altitude rule should win, got %+v", got)
	}
}

func TestDistanceLimitFailsClosedThenEvaluationContinues(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, nil)
	limit := 3.0
	alerts := defaultAlerts()
	alerts.Lat, alerts.Lon = &testLat, &testLon
	alerts.Rules = []Rule{{Name: "near", ICAO: []string{"ffffff"}, MaxDistanceNM: &limit}, {Name: "anywhere", ICAO: []string{"ffffff"}}}
	far := 10.0
	got := Evaluate(Aircraft{Hex: "ffffff", Lat: &testLat, Lon: &far}, db, alerts, nil)
	if got == nil || got.Trigger != "anywhere" {
		t.Fatalf("an out-of-range aircraft should fail only the limited rule, got %+v", got)
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
		age     time.Duration
		horizon time.Duration
		wantNM  float64
		wantIn  time.Duration
	}{
		{"heading straight over", 0, 0, 5 * time.Minute, 0, 5 * time.Minute},
		{"crossing", 90, 0, 5 * time.Minute, 10, 0},
		{"receding", 180, 0, 5 * time.Minute, 10, 0},
		{"clamped to horizon", 0, 0, 2 * time.Minute, 6, 2 * time.Minute},
		{"old position counts from now", 0, 2 * time.Minute, 5 * time.Minute, 0, 3 * time.Minute},
		// Reported inbound 6 minutes ago: it has since passed and is 2 NM beyond.
		{"already passed since last position", 0, 6 * time.Minute, 5 * time.Minute, 2, 0},
	} {
		nm, in := closestApproach(lat, lon, south, lon, 120, tc.heading, tc.age, tc.horizon)
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
	// The same orbit moved onto the antimeridian, so its longitudes straddle ±180.
	dateline := fly(60, repeat(20, 30))
	for i := range dateline.samples {
		dateline.samples[i].lon = wrap180(dateline.samples[i].lon + 180.12)
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
		{"orbit across the antimeridian", dateline, true},
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
	routine := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, alerts, nil)
	if routine == nil || routine.Trigger != "listed" {
		t.Fatalf("want the listed rule, got %+v", routine)
	}
	s.Record(cooldownKey(routine.Hex, routine.Trigger), cooldown)

	// The same airframe squawking 7700 now matches the first rule, on its own key.
	emergency := Evaluate(at(Aircraft{Hex: "adeb2f", Squawk: "7700"}), db, alerts, nil)
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

	a := Evaluate(at(Aircraft{Hex: "adeb2f", Squawk: "7700"}), db, alerts, nil)
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
