package app

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

// receding is what decides an aircraft has passed, so the abeam case matters most: a
// dot product of zero is the moment of closest approach, and reading it as "not yet"
// would hold an alert for a pass that has already happened.
func TestReceding(t *testing.T) {
	lat, lon := 51.5, -0.12
	south := lat - 10.0/60
	for _, tc := range []struct {
		name      string
		lat, lon  float64
		gs, track float64
		want      bool
	}{
		{"inbound", south, lon, 120, 0, false},
		{"outbound", south, lon, 120, 180, true},
		{"abeam counts as passed", south, lon, 120, 90, true},
		{"stationary never gets nearer", south, lon, 0, 0, true},
		// Receiver just west of the antimeridian, aircraft just east of it: the
		// longitude difference is 0.2°, not 359.8°.
		{"eastbound across the antimeridian", lat, -179.9, 120, 90, true},
		{"westbound across the antimeridian", lat, -179.9, 120, 270, false},
	} {
		recvLon := lon
		if tc.lon < -179 || tc.lon > 179 {
			recvLon = 179.9
		}
		if got := receding(lat, recvLon, tc.lat, tc.lon, tc.gs, tc.track); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
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

// ---------- holding an alert for the closest pass ----------

// holdRig drives poller.poll over a synthetic feed, so what is under test is the whole
// path — Evaluate, the cooldown check, the hold and the sweep — rather than a
// re-implemented slice of it. The queue is read directly; no notifier is stood up.
type holdRig struct {
	t      *testing.T
	p      *poller
	alerts *Alerts
	path   string
	clock  time.Time
}

func newHoldRig(t *testing.T, rules []Rule) *holdRig {
	t.Helper()
	cfg := testConfig(t)
	dir := t.TempDir()
	cfg.Source.URL = filepath.Join(dir, "aircraft.json")
	a := defaultAlerts()
	a.Lat, a.Lon, a.Rules = &testLat, &testLon, rules
	if err := a.validate(); err != nil {
		t.Fatalf("rules under test must be valid: %v", err)
	}
	r := &holdRig{t: t, alerts: a, path: cfg.Source.URL, clock: time.Now()}
	state := NewState(dir)
	// The ledger's clock is the rig's, so a cooldown shorter than the poll interval —
	// legal, and the reason the sweep cannot ask the ledger anything — can be reached.
	state.Now = func() time.Time { return r.clock }
	r.p = &poller{src: NewSource(cfg, http.DefaultClient), db: dbWith(t, cfg, nil), state: state,
		q: newQueue(maxPending), h: &server{}, tr: NewTracker(), holds: map[string]held{}}
	return r
}

// poll writes one feed and runs one poll over it.
func (r *holdRig) poll(acs ...Aircraft) {
	r.t.Helper()
	b, err := json.Marshal(feed{Now: float64(time.Now().UnixMilli()) / 1000, Aircraft: acs})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.path, b, 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.p.poll(context.Background(), r.alerts)
}

// take empties the queue the way a failing notifyLoop does: the entry is taken and
// dropped, and no cooldown is recorded.
func (r *holdRig) take() []*Alert {
	var out []*Alert
	for {
		a := r.p.q.take()
		if a == nil {
			return out
		}
		r.p.q.done(cooldownKey(a.Hex, a.Trigger))
		out = append(out, a)
	}
}

func (r *holdRig) one() *Alert {
	r.t.Helper()
	got := r.take()
	if len(got) != 1 {
		r.t.Fatalf("want exactly one alert offered, got %d", len(got))
	}
	return got[0]
}

func (r *holdRig) none(what string) {
	r.t.Helper()
	if got := r.take(); len(got) > 0 {
		r.t.Fatalf("%s: %d alert(s) offered, want none", what, len(got))
	}
}

// deliver is what notifyLoop does after a successful publish.
func (r *holdRig) deliver(a *Alert) {
	a.delivered.Store(true)
	r.p.state.Record(cooldownKey(a.Hex, a.Trigger), r.alerts.Cooldown.Std())
}

// age back-dates both of a hold's stamps, which is how a test reaches a grace window or
// the give-up ceiling without sleeping through one.
func (r *holdRig) age(d time.Duration) {
	for k, h := range r.p.holds {
		if !h.judged.IsZero() {
			h.judged = h.judged.Add(-d)
		}
		if !h.fired.IsZero() {
			h.fired = h.fired.Add(-d)
		}
		r.p.holds[k] = h
	}
}

func (r *holdRig) holds() int { return len(r.p.holds) }

// pollFailed runs one poll against a feed the source will refuse — a dead feeder, which
// is the state the hold has to keep working through.
func (r *holdRig) pollFailed() {
	r.t.Helper()
	if err := os.WriteFile(r.path, []byte("not json"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.p.poll(context.Background(), r.alerts)
}

// flying is one aircraft nm north (negative: south) of the receiver on the given track.
func flying(nm, track float64) Aircraft {
	lat, lon, hdg := testLat+nm/60, testLon, track
	return Aircraft{Hex: "abc123", Lat: &lat, Lon: &lon, GS: 120, Track: &hdg}
}

// trackless is the aircraft receding can never answer for: moving, reporting a position,
// broadcasting no ground track.
func trackless(nm float64) Aircraft {
	ac := flying(nm, 0)
	ac.Track = nil
	return ac
}

func closestPassRule() []Rule { return []Rule{{Name: "pass", Notify: notifyClosestPass}} }

// The point of the whole mechanism: the alert that goes out is the one built at the
// nearest position, not the one built at the poll that noticed the aircraft had passed.
func TestClosestPassSendsTheNearestPosition(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	for _, nm := range []float64{-10, -5, -0.5} {
		r.poll(flying(nm, 0)) // inbound from the south
		r.none("while inbound")
		if r.holds() != 1 {
			t.Fatal("an inbound aircraft should be parked")
		}
	}
	r.poll(flying(2, 0)) // 2 NM north and still going: it has passed
	a := r.one()
	if !a.AtClosest || math.Abs(a.DistanceNM-0.5) > 0.01 {
		t.Fatalf("want the 0.5 NM snapshot, got %.2f NM (AtClosest=%v)", a.DistanceNM, a.AtClosest)
	}
}

// An aircraft that leaves the feed mid-approach never reads as receding, so the grace
// window is the only thing that will ever send its alert.
func TestClosestPassFiresForAnAircraftThatVanishes(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.poll(flying(-10, 0))
	r.poll(flying(-2, 0))
	r.none("while inbound")

	r.poll() // gone from the feed
	r.none("inside the grace window")
	r.age(time.Minute) // three polls of a 15s interval is 45s
	r.poll()
	if a := r.one(); math.Abs(a.DistanceNM-2) > 0.01 {
		t.Fatalf("want the 2 NM snapshot, got %.2f NM", a.DistanceNM)
	}
}

// An aircraft broadcasting a position but no ground track cannot be judged at all. Both
// halves matter: a last-seen stamp would hold it for as long as it kept matching, and a
// judged left zero at creation would fire it on the first sweep.
func TestClosestPassFiresForATracklessAircraft(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	for i := 0; i < 10; i++ {
		r.poll(trackless(-10 + float64(i)))
		r.none("inside the grace window")
	}
	r.age(time.Minute)
	r.poll(trackless(-1)) // still matching, still trackless: judged must not be refreshed
	if a := r.one(); math.Abs(a.DistanceNM-1) > 0.01 {
		t.Fatalf("want the 1 NM snapshot, got %.2f NM", a.DistanceNM)
	}
}

// notifyLoop drops a queue entry whose publish failed and advances no cooldown, so the
// entry has to outlive the offer — and resend the same snapshot, not a later one.
func TestClosestPassRetriesTheSameAlertUntilDelivered(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.poll(flying(-2, 0))
	r.poll(flying(2, 0))
	first := r.one()

	r.poll(flying(6, 0)) // further away now, and the publish has not succeeded
	again := r.one()
	if again != first {
		t.Fatalf("a retry must re-offer the same alert, got %.2f NM", again.DistanceNM)
	}
	if math.Abs(again.DistanceNM-2) > 0.01 {
		t.Fatalf("the snapshot must not move after firing, got %.2f NM", again.DistanceNM)
	}

	r.deliver(again)
	r.poll(flying(10, 0))
	r.none("after delivery")
	if r.holds() != 0 {
		t.Error("a delivered hold must be dropped")
	}
}

// A full queue refuses the alert outright. Dropping the entry on the offer would lose
// the pass; keeping it means the alert lands late rather than never.
func TestClosestPassSurvivesAFullQueue(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.p.q = newQueue(1)
	r.p.q.add(&Alert{Hex: "ffffff", Trigger: "other"})

	r.poll(flying(-2, 0))
	r.poll(flying(2, 0))
	if r.holds() != 1 {
		t.Fatal("an alert the queue refused must stay parked")
	}
	r.p.q.done(cooldownKey("ffffff", "other"))
	r.poll(flying(6, 0))
	if a := r.one(); math.Abs(a.DistanceNM-2) > 0.01 {
		t.Fatalf("want the 2 NM snapshot once the queue drains, got %.2f NM", a.DistanceNM)
	}
}

// An ntfy server down for a day must not leave every aircraft that passed still parked
// and still being offered.
func TestClosestPassGivesUp(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.poll(flying(-2, 0))
	r.poll(flying(2, 0))
	r.one()

	r.age(holdGiveUp + time.Minute)
	r.poll()
	r.none("after the give-up ceiling")
	if r.holds() != 0 {
		t.Error("an abandoned hold must be dropped")
	}
}

// The delivery ack, not the cooldown ledger. A cooldown shorter than the poll interval
// is a legal configuration in which the key is eligible again — and its ledger entry
// pruned — before the next sweep runs.
func TestClosestPassIsDeliveredOnceUnderAShortCooldown(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.alerts.Cooldown = Duration(5 * time.Second)
	r.poll(flying(-2, 0))
	r.poll(flying(2, 0))
	r.deliver(r.one())

	r.clock = r.clock.Add(time.Minute) // the cooldown has lapsed and pruned itself
	if !r.p.state.Eligible(cooldownKey("abc123", "pass"), r.alerts.Cooldown.Std()) {
		t.Fatal("the ledger should say eligible again, which is the trap being tested")
	}
	for i := 0; i < 5; i++ {
		r.poll() // the aircraft has gone; only the sweep can act
		r.none("after delivery under a short cooldown")
	}
}

// The trackless aircraft that reached the fallback while still inbound: freezing the
// snapshot is what keeps its ack attached, so a nearer position after firing changes
// neither what is sent nor how often.
func TestClosestPassDoesNotResendWhenItKeepsClosing(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.alerts.Cooldown = Duration(5 * time.Second)
	r.poll(trackless(-8))
	r.age(time.Minute)
	r.poll(trackless(-8))
	a := r.one()
	if math.Abs(a.DistanceNM-8) > 0.01 {
		t.Fatalf("want the 8 NM snapshot, got %.2f NM", a.DistanceNM)
	}
	r.deliver(a)

	r.clock = r.clock.Add(time.Minute)
	r.poll(trackless(-3)) // closer than the alert that went out, and eligible again
	r.none("a nearer position after firing must not resend")
}

// The default path, unchanged: an on_sight rule alerts on the first poll it matches and
// parks nothing, and a rule inside its cooldown parks nothing either.
func TestOnSightIsUnchangedAndCooldownsSkipTheHold(t *testing.T) {
	for _, rule := range []Rule{{Name: "sight"}, {Name: "sight", Notify: notifyOnSight}} {
		r := newHoldRig(t, []Rule{rule})
		r.poll(flying(-10, 0))
		if a := r.one(); a.AtClosest {
			t.Error("an on_sight alert must not claim to be a closest pass")
		}
		if r.holds() != 0 {
			t.Error("an on_sight rule must park nothing")
		}
	}

	r := newHoldRig(t, closestPassRule())
	r.p.state.Record(cooldownKey("abc123", "pass"), r.alerts.Cooldown.Std())
	r.poll(flying(-2, 0))
	r.poll(flying(2, 0))
	r.none("inside the cooldown")
	if r.holds() != 0 {
		t.Error("a key inside its cooldown must never park")
	}
}

// A feeder that dies is exactly when a parked alert needs the sweep: nothing can refresh
// judged while it is down, so a sweep that only ran on a good poll would hold the alert
// until the feed came back — and could abandon a fired one at the give-up ceiling
// without ever retrying it, since a failed publish leaves the queue empty.
func TestClosestPassKeepsWorkingWhileTheFeedIsDown(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.poll(flying(-2, 0))
	r.pollFailed()
	r.none("inside the grace window")

	r.age(time.Minute)
	r.pollFailed()
	if a := r.one(); math.Abs(a.DistanceNM-2) > 0.01 {
		t.Fatalf("want the 2 NM snapshot, got %.2f NM", a.DistanceNM)
	}
	// And the retry, which nothing else re-offers.
	r.pollFailed()
	if got := r.take(); len(got) != 1 {
		t.Fatalf("a fired alert must still be retried with the feed down, got %d offers", len(got))
	}
}

// Matching is not read-only: a passes_within_nm rule records the pass it predicts in the
// alert it is handed. The sweep re-matches holds every poll, and one that has already
// been offered is in the notifier's hands — being rendered, or waiting on a retry — so
// the recheck must not be able to reach it.
func TestRecheckingAHoldLeavesTheOfferedAlertAlone(t *testing.T) {
	r := newHoldRig(t, closestPassRule()) // no passes_within_nm: nothing predicts a pass
	r.poll(flying(-2, 0))
	r.none("while inbound")
	r.poll(flying(1, 0)) // past the receiver and receding: the hold fires
	a := r.one()
	if a.HasPass {
		t.Fatal("a rule that states no pass condition must not predict one")
	}

	// The rule gains one while the alert it already offered is still parked, waiting for
	// the ack. The hold is still held — same rule, same key — so it is re-matched.
	within := 50.0
	r.alerts.Rules[0].PassesWithinNM = &within
	r.poll(flying(2, 0))
	if a.HasPass {
		t.Error("the recheck wrote its prediction into an alert that had already gone out")
	}
	if r.holds() != 1 {
		t.Errorf("the hold should still be waiting for its ack, %d left", r.holds())
	}
}

// A rule that stops waiting for the pass stops holding: the poll that matches the
// aircraft now queues the ordinary alert, and a hold left alive beside it would carry the
// same cooldown key — a second notification of a pass, from a snapshot taken minutes ago,
// the moment the aircraft leaves the feed and the cooldown lapses.
func TestClosestPassDropsAHoldWhoseRuleStoppedHolding(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.poll(flying(-2, 0))
	r.none("while inbound")
	if r.holds() != 1 {
		t.Fatal("an inbound aircraft should be parked")
	}

	r.alerts.Rules[0].Notify = "" // immediately, from now on
	r.poll(flying(-1, 0))
	if a := r.one(); a.AtClosest {
		t.Error("the parked alert was offered after the rule stopped waiting for the pass")
	}
	if r.holds() != 0 {
		t.Errorf("the hold must be retired with the setting, %d left", r.holds())
	}
	// And it stays retired once the aircraft is gone, which is when the stale pass would
	// otherwise be offered.
	r.age(time.Minute)
	r.poll()
	r.none("after the aircraft left")
}

// alerts.yaml is re-read every five seconds, and a hold outlives several of those. A rule
// deleted or muted while one of its alerts is parked must not still announce it — muting
// is how an exception is written, and it cannot mean "after one more".
// A hold carries a distance, and a distance is only meaningful from somewhere. The
// receiver is hot-reloaded with the rest of alerts.yaml, so it can move while an aircraft
// is still inbound — and then the nearest snapshot was measured from the old position and
// the sighting beside it from the new. Announcing "closest pass 0.4 NM" from a receiver
// the operator has left is worse than announcing nothing: the next poll parks a fresh
// hold, measured throughout from where the receiver actually is.
func TestClosestPassDropsAHoldWhenTheReceiverMoves(t *testing.T) {
	r := newHoldRig(t, closestPassRule())
	r.poll(flying(-2, 0)) // inbound, parked, measured from the old receiver
	if r.holds() != 1 {
		t.Fatal("an inbound aircraft should be parked")
	}
	r.age(time.Minute) // past the grace window: without the check it would fire

	moved := testLat + 1 // a degree north, which is 60 NM of difference
	r.alerts.Lat = &moved

	r.poll(flying(-1, 0))
	r.none("after the receiver moved")
	if r.holds() != 1 {
		t.Fatalf("the stale hold should be replaced by a fresh one, %d held", r.holds())
	}
	// And the fresh one measures from where the receiver is now, so it can go on to
	// announce a pass of its own.
	if h := r.p.holds[cooldownKey("abc123", "pass")]; h.alert.recvLat != moved {
		t.Errorf("the replacement hold measures from %v, want %v", h.alert.recvLat, moved)
	}
}

func TestClosestPassDropsAHoldWhoseRuleIsGone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		amend func(*Alerts)
	}{
		{"deleted", func(a *Alerts) { a.Rules = nil }},
		{"muted", func(a *Alerts) { a.Rules[0].Priority = intp(0) }},
		// An exception is written ahead of the rule it excepts, not on top of it: the
		// parked rule is untouched and still alerts for everything else.
		{"excepted by a muted rule ahead of it", func(a *Alerts) {
			a.Rules = append([]Rule{{Name: "exception", ICAO: []string{"abc123"}, Priority: intp(0)}}, a.Rules...)
		}},
	} {
		r := newHoldRig(t, closestPassRule())
		r.poll(flying(-2, 0))
		r.age(time.Minute) // past the grace window: without the reconcile it would fire
		tc.amend(r.alerts)

		r.poll(flying(-1, 0))
		r.none(tc.name + " rule")
		if r.holds() != 0 {
			t.Errorf("%s rule: the hold must be dropped, %d left", tc.name, r.holds())
		}
	}
}
