package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

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

// A rule's limits ask where an aircraft is, which no database row can answer. Applying
// them to the preview would fail them closed and report every distance-limited rule as
// selecting nothing — the one reading that would send an operator to widen a rule that
// was already correct.
func TestPreviewIgnoresRuntimeLimits(t *testing.T) {
	p := &Plane{ICAO: "abc123", Reg: "N1", Operator: "US Air Force", CMPG: "Mil", Tags: []string{"Cargo"}}
	db := &DB{merged: map[string]*Plane{p.ICAO: p}}
	nm, minAlt := 5.0, 30000
	rule := Rule{Name: "x", CMPG: []string{"Mil"}, MaxDistanceNM: &nm, MinAltitudeFt: &minAlt, Circling: boolp(true)}

	total, sample := db.Match(rule, 10, 0)
	if total != 1 || len(sample) != 1 {
		t.Fatalf("total = %d, sample = %d; limits must not narrow a database preview", total, len(sample))
	}
	// The alert path still enforces them: the same rule rejects an aircraft with no
	// altitude and no position.
	if got := firstMatch([]Rule{rule}, Aircraft{Hex: p.ICAO}, p, &Alert{Hex: p.ICAO}); got != nil {
		t.Fatal("the alert path must still apply the limits the preview skipped")
	}
}

// Squawk is a live-feed field, so a squawk rule selects no database row. That must read
// as "nothing to preview", never as the rule being broken.
func TestPreviewIgnoresSquawk(t *testing.T) {
	p := &Plane{ICAO: "abc123", CMPG: "Mil"}
	db := &DB{merged: map[string]*Plane{p.ICAO: p}}
	if total, _ := db.Match(Rule{Name: "x", CMPG: []string{"Mil"}, Squawk: []string{"7700"}}, 10, 0); total != 1 {
		t.Fatalf("total = %d, want the rule previewed on its database conditions alone", total)
	}
}

// Paging walks a stable order. merged is a map, so without the ICAO sort each page would
// be drawn from a different iteration order and "load more" could repeat a row it had
// already shown while never reaching one it had not.
func TestMatchPagesCoverEveryRowExactlyOnce(t *testing.T) {
	merged := map[string]*Plane{}
	for i := 0; i < 55; i++ {
		p := &Plane{ICAO: fmt.Sprintf("%06x", i), CMPG: "Mil"}
		merged[p.ICAO] = p
	}
	db := &DB{merged: merged}
	rule := Rule{Name: "x", CMPG: []string{"Mil"}}

	var seen []string
	for offset := 0; ; offset += 20 {
		total, page := db.Match(rule, 20, offset)
		if total != len(merged) {
			t.Fatalf("total = %d at offset %d, want %d on every page", total, offset, len(merged))
		}
		if len(page) == 0 {
			break
		}
		for _, p := range page {
			seen = append(seen, p.ICAO)
		}
	}
	if len(seen) != len(merged) {
		t.Fatalf("paged over %d rows, want %d", len(seen), len(merged))
	}
	if !sort.StringsAreSorted(seen) {
		t.Fatal("pages must continue one stable order, not restart it")
	}
	unique := map[string]bool{}
	for _, hex := range seen {
		if unique[hex] {
			t.Fatalf("row %s served twice across pages", hex)
		}
		unique[hex] = true
	}
}

// An offset past the end is what a stale page sends after the database shrinks under a
// refresh. It must read as "no more rows", not panic on the slice bound.
func TestMatchOffsetPastTheEndIsEmpty(t *testing.T) {
	p := &Plane{ICAO: "abc123", CMPG: "Mil"}
	db := &DB{merged: map[string]*Plane{p.ICAO: p}}
	total, sample := db.Match(Rule{Name: "x", CMPG: []string{"Mil"}}, 20, 500)
	if total != 1 || len(sample) != 0 {
		t.Fatalf("total = %d, sample = %d; want the real total and an empty page", total, len(sample))
	}
}

// An icao_type rule is not a database query: the feed carries the type code an aircraft
// broadcasts, and plenty of codes belong to no listed aircraft at all. A rule naming one
// must still alert, or the condition silently means "and also happens to be in the
// database" — which is not what it says.
func TestICAOTypeRuleMatchesAnUnlistedAircraft(t *testing.T) {
	cfg := testConfig(t)
	db := dbWith(t, cfg, mustParse(t, sampleCSV))
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "gliders", ICAOType: []string{"AS21"}, Priority: intp(4)}}

	a := Evaluate(at(Aircraft{Hex: "ffffff", Type: "AS21"}), db, alerts, nil)
	if a == nil || a.Plane != nil || a.Trigger != "gliders" {
		t.Fatalf("alert = %+v, want an unlisted live-feed type code to alert", a)
	}
	// And the database preview reports zero without that being a contradiction: it can
	// only search rows, and no row carries this code.
	if total, _ := db.Match(alerts.Rules[0], 20, 0); total != 0 {
		t.Fatalf("database preview = %d, want 0 for a code no row carries", total)
	}
}

// The service runs the file plus the environment, so that is what a save has to be judged
// against. A rule needing coordinates the environment supplies is valid at runtime, and
// rejecting it would make the UI refuse a setting the operator already runs.
func TestAlertsUISaveValidatesWithEnvironmentApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	t.Setenv("SKY_LAT", "41.9")
	t.Setenv("SKY_LON", "-87.6")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	body := `{"rules":[{"name":"near","priority":4,"max_distance_nm":25}]}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	// The override is validated against, never written: the file keeps the operator's own
	// values, so removing the variable does not silently leave its coordinates behind.
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "41.9") {
		t.Fatalf("environment coordinates were baked into the file:\n%s", blob)
	}
	// And it really is what the loader accepts.
	if _, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path, "SKY_LAT=41.9", "SKY_LON=-87.6"}); err != nil {
		t.Fatalf("the saved file must load under the same environment: %v", err)
	}
}

// The mirror of the above: an override that makes the payload invalid has to fail at save
// time, not silently write a file the watcher then refuses on reload.
func TestAlertsUISaveRejectsWhatTheEnvironmentBreaks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	t.Setenv("SKY_COOLDOWN", "0s")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(`{"rules":[{"name":"x"}]}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a payload the environment invalidates", w.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a rejected save must not touch the file")
	}
}

// A rule that inherits ntfy.priority has to keep inheriting it. Writing the current
// default into the rule freezes it there, silently changing behaviour the next time the
// default moves — and immediately, for anyone whose default is not 3.
func TestRuleWithoutPriorityRoundTripsAsUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	body := `{"ntfy":{"priority":2},"rules":[{"name":"inherits"},{"name":"muted","priority":0}]}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "priority: 3") {
		t.Fatalf("an unset priority was written as the default:\n%s", blob)
	}
	got, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path})
	if err != nil {
		t.Fatal(err)
	}
	if got.Rules[0].Priority != nil {
		t.Fatalf("rule 0 priority = %d, want nil so it follows ntfy.priority", *got.Rules[0].Priority)
	}
	// 0 is "muted", a real choice, and must survive the same round trip as a set value.
	if got.Rules[1].Priority == nil || *got.Rules[1].Priority != 0 {
		t.Fatalf("rule 1 priority = %v, want an explicit 0", got.Rules[1].Priority)
	}
	// The inheriting rule alerts at the configured default, not at 3.
	a := Evaluate(at(Aircraft{Hex: "ffffff"}), NewDB(testConfig(t), http.DefaultClient), got, nil)
	if a == nil || a.Priority != 2 {
		t.Fatalf("alert = %+v, want the inherited priority 2", a)
	}
}

// The live preview answers "what would this alert on right now", so unlike the database
// preview it must apply the runtime conditions — and must agree, aircraft for aircraft,
// with what the poll loop would actually enqueue.
func TestLivePreviewMatchesWhatWouldAlert(t *testing.T) {
	p := &Plane{ICAO: "abc123", Reg: "N1", Operator: "US Air Force", CMPG: "Mil"}
	db := &DB{merged: map[string]*Plane{p.ICAO: p}}
	lat, lon, nm, minAlt := 30.0, -95.0, 25.0, 10000
	rule := Rule{Name: "mil", CMPG: []string{"Mil"}, MaxDistanceNM: &nm, MinAltitudeFt: &minAlt}
	cfg := defaultAlerts()
	cfg.Lat, cfg.Lon, cfg.Rules = &lat, &lon, []Rule{rule}

	near := Aircraft{Hex: "abc123", Lat: &lat, Lon: &lon, AltBaro: Altitude{Feet: 31000, Present: true}}
	low := near
	low.AltBaro = Altitude{Feet: 500, Present: true}
	low.Hex = "abc124"
	far := near
	far.Hex = "abc125"
	farLat := lat + 5
	far.Lat = &farLat
	noPos := Aircraft{Hex: "abc126", AltBaro: Altitude{Feet: 31000, Present: true}}
	overhead := []Aircraft{low, far, noPos, near}
	// low, far and noPos share abc123's row only by hex, so give them one too: the rule
	// is narrowed by CMPG and they must fail on position, not on identity.
	for _, ac := range overhead {
		hex := normalizeHex(ac.Hex)
		db.merged[hex] = &Plane{ICAO: hex, Reg: "N1", Operator: "US Air Force", CMPG: "Mil"}
	}

	total, sample := MatchLive(rule, overhead, nil, db, cfg, previewLimit)
	if total != 1 || len(sample) != 1 || sample[0].ICAO != "abc123" {
		t.Fatalf("live preview = %d %#v, want only abc123", total, sample)
	}
	if sample[0].AltitudeFt == nil || *sample[0].AltitudeFt != 31000 || sample[0].DistanceNM == nil {
		t.Fatalf("hit is missing the position it was matched on: %#v", sample[0])
	}
	// Whatever the preview says, the alert path must reach the same verdict.
	tr := NewTracker()
	for _, ac := range overhead {
		got := Evaluate(ac, db, cfg, tr.get(normalizeHex(ac.Hex))) != nil
		want := normalizeHex(ac.Hex) == "abc123"
		if got != want {
			t.Fatalf("%s: alert path says %v, preview says %v", ac.Hex, got, want)
		}
	}
}

// The nearest aircraft is the one a rule is usually being written for, and a capped
// sample that dropped it would be the wrong twenty.
func TestLivePreviewListsNearestFirst(t *testing.T) {
	lat, lon := 30.0, -95.0
	cfg := defaultAlerts()
	cfg.Lat, cfg.Lon = &lat, &lon
	db := &DB{merged: map[string]*Plane{}}
	var overhead []Aircraft
	for i := 0; i < previewLimit+5; i++ {
		at := lat + float64(previewLimit+5-i)/10
		overhead = append(overhead, Aircraft{Hex: fmt.Sprintf("%06x", i), Lat: &at, Lon: &lon})
	}
	total, sample := MatchLive(Rule{Name: "all"}, overhead, nil, db, cfg, previewLimit)
	if total != len(overhead) || len(sample) != previewLimit {
		t.Fatalf("total = %d, sample = %d; want %d capped at %d", total, len(sample), len(overhead), previewLimit)
	}
	for i := 1; i < len(sample); i++ {
		if *sample[i-1].DistanceNM > *sample[i].DistanceNM {
			t.Fatalf("sample is not nearest-first at %d: %v then %v", i, *sample[i-1].DistanceNM, *sample[i].DistanceNM)
		}
	}
}

// Receiver coordinates are edited in the same session as the rule that needs them, so a
// distance rule previewed against the saved ones answers for the wrong place — or, before
// they are saved at all, for nowhere. The environment still outranks the page, exactly as
// it does at save.
func TestPreviewMeasuresFromTheReceiverOnScreen(t *testing.T) {
	saved, savedLon := 0.0, 0.0
	cfg := defaultAlerts()
	cfg.Lat, cfg.Lon = &saved, &savedLon
	h := &server{live: NewLive(cfg)}

	draft := url.Values{"lat": {"30"}, "lon": {"-95"}}
	got := h.previewAlerts(draft)
	if got.Lat == nil || *got.Lat != 30 || got.Lon == nil || *got.Lon != -95 {
		t.Fatalf("preview receiver = %v/%v, want the coordinates the page is holding", got.Lat, got.Lon)
	}
	// Nothing drafted at all — anyone but the page — answers from the saved receiver.
	for _, q := range []url.Values{{}, {"lat": {"30"}}, {"lon": {"-95"}}} {
		if got := h.previewAlerts(q); got.Lat != cfg.Lat || got.Lon != cfg.Lon {
			t.Fatalf("%v: preview receiver moved without a drafted pair", q)
		}
	}
	// A receiver cleared on screen is a receiver, not a missing parameter: falling back
	// to the saved pair would measure from a place this configuration no longer knows.
	// Half a pair, and a half-typed number, say the same thing.
	for _, q := range []url.Values{
		{"lat": {""}, "lon": {""}},
		{"lat": {"30"}, "lon": {""}},
		{"lat": {"-"}, "lon": {"-95"}},
	} {
		if got := h.previewAlerts(q); got.Lat != nil || got.Lon != nil {
			t.Fatalf("%v: preview receiver = %v/%v, want no receiver at all", q, got.Lat, got.Lon)
		}
	}
	// And a cleared receiver leaves a distance rule matching nothing, rather than
	// matching from wherever the receiver used to be.
	near := 5.0
	overhead := []Aircraft{{Hex: "abc123", Lat: &saved, Lon: &savedLon}}
	cleared := h.previewAlerts(url.Values{"lat": {""}, "lon": {""}})
	if total, _ := MatchLive(Rule{Name: "near", MaxDistanceNM: &near}, overhead, nil, &DB{}, cleared, 10); total != 0 {
		t.Fatalf("a distance rule matched %d aircraft with no receiver", total)
	}
	t.Setenv("SKY_LAT", "10")
	if got := h.previewAlerts(draft); got.Lat != cfg.Lat || got.Lon == nil || *got.Lon != -95 {
		t.Fatalf("preview receiver = %v/%v, want the environment's latitude and the page's longitude", got.Lat, got.Lon)
	}
	// Never through the pointer the running loops read.
	if *cfg.Lat != 0 || *cfg.Lon != 0 {
		t.Fatal("the preview mutated the published config")
	}
}

/* ── Alert history ─────────────────────────────────────────────────────── */

func TestHistoryRecordsPagesAndPrunes(t *testing.T) {
	h, err := NewHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	now := time.Now()
	for i := 0; i < 25; i++ {
		h.Add("overhead", ntfyMessage{
			Title: fmt.Sprintf("alert %d", i), Message: "Registration: N1234",
			Priority: 3, Tags: []string{"airplane", "rotating_light"}, Click: "https://map/",
		}, now.Add(time.Duration(i)*time.Minute))
	}
	h.Add("other rule", ntfyMessage{Title: "not ours"}, now)

	page, err := h.List("overhead", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 25 || len(page.Alerts) != 20 || page.Next == "" {
		t.Fatalf("first page: total %d, %d rows, next %q; want 25, 20 and a cursor",
			page.Total, len(page.Alerts), page.Next)
	}
	// Newest first, and every field the notification carried survives the round trip.
	first := page.Alerts[0]
	if first.Title != "alert 24" {
		t.Fatalf("newest first: got %q", first.Title)
	}
	if first.Priority != 3 || first.Click != "https://map/" ||
		strings.Join(first.Tags, ",") != "airplane,rotating_light" ||
		first.Message != "Registration: N1234" {
		t.Fatalf("fields not preserved: %+v", first)
	}
	if want := now.Add(24 * time.Minute).UnixMilli(); first.SentAt.UnixMilli() != want {
		t.Fatalf("sent_at %v, want %v", first.SentAt, time.UnixMilli(want))
	}

	second, err := h.List("overhead", page.Next, 20)
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 25 || len(second.Alerts) != 5 || second.Alerts[0].Title != "alert 4" {
		t.Fatalf("second page: total %d, %d rows, first %q",
			second.Total, len(second.Alerts), second.Alerts[0].Title)
	}
	// The list ends where the rows do: nothing offers a page that is not there.
	if second.Next != "" {
		t.Fatalf("last page still offers a cursor %q", second.Next)
	}

	// A rule only ever sees its own deliveries.
	if other, _ := h.List("other rule", "", 20); other.Total != 1 {
		t.Fatalf("other rule has %d, want 1", other.Total)
	}

	// An insert past the retention window takes the old rows with it.
	h.Add("overhead", ntfyMessage{Title: "much later"}, now.Add(historyRetention+time.Hour))
	page, err = h.List("overhead", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Alerts[0].Title != "much later" {
		t.Fatalf("after prune: total %d, rows %+v", page.Total, page.Alerts)
	}

	// A store that failed to open is still safe to record to and read from.
	var absent *History
	absent.Add("overhead", ntfyMessage{Title: "x"}, now)
	if p, err := absent.List("overhead", "", 20); err != nil || p.Total != 0 || len(p.Alerts) != 0 {
		t.Fatalf("nil history: %+v, %v", p, err)
	}
}

// Paging is a cursor and not an offset because the list grows at the head while it is
// being read. A delivery landing between two page requests used to push every row down
// one, and the second page then re-served the last row of the first.
func TestHistoryPagingSurvivesConcurrentDeliveries(t *testing.T) {
	h, err := NewHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	now := time.Now()
	for i := 0; i < 25; i++ {
		h.Add("overhead", ntfyMessage{Title: fmt.Sprintf("alert %02d", i)},
			now.Add(time.Duration(i)*time.Minute))
	}

	first, err := h.List("overhead", "", 10)
	if err != nil {
		t.Fatal(err)
	}

	// Two more arrive while the reader is looking at the first page, and the retention
	// prune the second one runs takes the oldest three with it.
	h.Add("overhead", ntfyMessage{Title: "arrived 1"}, now.Add(30*time.Minute))
	h.Add("overhead", ntfyMessage{Title: "arrived 2"}, now.Add(31*time.Minute))
	if _, err := h.db.Exec(`DELETE FROM alerts WHERE title IN ('alert 00','alert 01','alert 02')`); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	got := []string{}
	for _, r := range first.Alerts {
		seen[r.Title], got = true, append(got, r.Title)
	}
	for cursor := first.Next; cursor != ""; {
		p, err := h.List("overhead", cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Alerts {
			if seen[r.Title] {
				t.Fatalf("%q served twice; pages so far: %v", r.Title, got)
			}
			seen[r.Title], got = true, append(got, r.Title)
		}
		cursor = p.Next
	}

	// Every row that was there when the read started and is still there at the end was
	// served exactly once. The two that arrived above the cursor are legitimately missed
	// — they were never below it — and the three pruned away are gone.
	for i := 3; i < 25; i++ {
		if title := fmt.Sprintf("alert %02d", i); !seen[title] {
			t.Fatalf("%q was skipped; pages: %v", title, got)
		}
	}
	if len(got) != 22 {
		t.Fatalf("read %d rows, want the 22 surviving ones: %v", len(got), got)
	}
}

// The page asks for the font by URL now that it is not inlined, so a page that renders
// in the fallback stack is the failure this catches.
func TestFontIsServedAndReferenced(t *testing.T) {
	h := (&server{live: NewLive(defaultAlerts())}).mux(newQueue(1))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/public-sans.woff2", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "font/woff2" {
		t.Fatalf("status = %d, type = %q", w.Code, w.Header().Get("Content-Type"))
	}
	if got := w.Body.Bytes(); len(got) < 1000 || string(got[:4]) != "wOF2" {
		t.Fatalf("body is %d bytes, not a woff2", len(got))
	}
	// The URL is fixed, so the font must revalidate: a browser holding the previous
	// binary's font has nothing else to tell it that this one is different.
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("etag = %q, cache-control = %q", etag, w.Header().Get("Cache-Control"))
	}
	again := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/public-sans.woff2", nil)
	req.Header.Set("If-None-Match", etag)
	h.ServeHTTP(again, req)
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("repeat fetch = %d with %d bytes, want 304 and none", again.Code, again.Body.Len())
	}

	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(page.Body.String(), "src:url(public-sans.woff2)") {
		t.Error("ui.html does not reference the served font")
	}
}

// The API key is handled the way a password field is: the page is never sent it, and a
// save that carries none keeps the one on disk. Without the second half, saving any
// unrelated setting from the page would wipe the credential.
func TestAIKeyIsWriteOnlyToTheBrowser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	if err := os.WriteFile(path, []byte("ai:\n  url: https://openrouter.ai/api/v1\n  key: sk-or-v1-secret\n  model: perplexity/sonar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := alertsHandler(t, path)

	get := func() map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET status %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "sk-or-v1-secret") {
			t.Fatalf("the API key was served to the browser:\n%s", w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := get()
	if body["ai_key_set"] != true {
		t.Errorf("ai_key_set = %v, want true", body["ai_key_set"])
	}
	ai, _ := body["alerts"].(map[string]any)["ai"].(map[string]any)
	if _, ok := ai["key"]; ok {
		t.Errorf("the served document carried a key field at all: %v", ai)
	}
	// What the page holds is what the page sends back: everything but the key.
	if ai["url"] != "https://openrouter.ai/api/v1" || ai["model"] != "perplexity/sonar" {
		t.Fatalf("endpoint and model should still be editable: %v", ai)
	}

	w := httptest.NewRecorder()
	save := `{"ai":{"url":"https://openrouter.ai/api/v1","model":"anthropic/claude-sonnet-4.5","timeout":"20s"},"rules":[]}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(save)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	saved, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path})
	if err != nil {
		t.Fatal(err)
	}
	if saved.aiKey() != "sk-or-v1-secret" {
		t.Errorf("an omitted key wiped the stored one: %q", saved.aiKey())
	}
	if saved.AI.Model != "anthropic/claude-sonnet-4.5" {
		t.Errorf("model = %q, want the edited one", saved.AI.Model)
	}
	if body := get(); body["ai_key_set"] != true {
		t.Error("the key should still be reported as set after a save that omitted it")
	}

	// A key the page does send replaces it.
	w = httptest.NewRecorder()
	save = `{"ai":{"url":"https://openrouter.ai/api/v1","key":"sk-or-v1-new","model":"perplexity/sonar","timeout":"20s"},"rules":[]}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(save)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	if saved, _ := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path}); saved.aiKey() != "sk-or-v1-new" {
		t.Errorf("key = %q, want the one that was sent", saved.aiKey())
	}

	// An empty string is the page saying clear it, which is not the same as sending none.
	w = httptest.NewRecorder()
	save = `{"ai":{"url":"https://openrouter.ai/api/v1","key":"","model":"perplexity/sonar","timeout":"20s"},"rules":[]}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(save)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body.String())
	}
	if saved, _ := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path}); saved.aiKey() != "" {
		t.Errorf("key = %q, want it cleared", saved.aiKey())
	}
	if body := get(); body["ai_key_set"] != false {
		t.Error("ai_key_set should be false once the key is cleared")
	}
}

// An env-set key is not the page's to edit, and its value is the one locked setting whose
// value must not travel with the lock.
func TestEnvAIKeyIsLockedWithoutItsValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	t.Setenv("SKY_AI_KEY", "sk-or-v1-from-env")
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/alerts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-or-v1-from-env") {
		t.Fatalf("the locked key's value was served:\n%s", w.Body.String())
	}
	var body struct {
		Locked []map[string]string `json:"locked"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range body.Locked {
		if l["key"] == aiKeyPath {
			found = true
			if _, ok := l["value"]; ok {
				t.Error("the locked entry carried the key's value")
			}
			if l["env"] != "SKY_AI_KEY" {
				t.Errorf("env = %q", l["env"])
			}
		}
	}
	if !found {
		t.Error("an env-set key must still be reported as locked")
	}
}

// The page never holds the key, so a save that supplies none and moves the endpoint is
// asking for the stored credential to be posted to a host the payload chose. That is the
// same disclosure as serving the key outright, by a longer route.
func TestAIKeyDoesNotFollowTheEndpoint(t *testing.T) {
	const stored = "ai:\n  url: https://openrouter.ai/api/v1\n  key: sk-or-v1-secret\n  model: perplexity/sonar\n"
	put := func(t *testing.T, h http.Handler, ai string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		body := `{"ai":{` + ai + `,"model":"perplexity/sonar","timeout":"20s"},"rules":[]}`
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
		return w
	}

	t.Run("a move with no key is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		h := alertsHandler(t, path)
		w := put(t, h, `"url":"https://attacker.invalid/v1"`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		saved, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path})
		if err != nil {
			t.Fatal(err)
		}
		if saved.AI.URL != "https://openrouter.ai/api/v1" || saved.aiKey() != "sk-or-v1-secret" {
			t.Fatalf("a refused save must not touch the file: %+v", saved.AI)
		}
	})

	t.Run("a move with a key of its own is allowed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		h := alertsHandler(t, path)
		w := put(t, h, `"url":"https://api.together.xyz/v1","key":"sk-together-new"`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if saved, _ := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path}); saved.aiKey() != "sk-together-new" {
			t.Errorf("key = %q, want the one the save supplied", saved.aiKey())
		}
	})

	t.Run("staying put keeps the key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		h := alertsHandler(t, path)
		// The path may move freely: the key already reaches that host.
		if w := put(t, h, `"url":"https://openrouter.ai/api/v2"`); w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		if saved, _ := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path}); saved.aiKey() != "sk-or-v1-secret" {
			t.Errorf("a same-host save should keep the key, got %q", saved.aiKey())
		}
	})

	t.Run("a downgrade to http is a move", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		h := alertsHandler(t, path)
		if w := put(t, h, `"url":"http://openrouter.ai/api/v1"`); w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an https to http move", w.Code)
		}
	})

	t.Run("turning the provider off is not a move", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		h := alertsHandler(t, path)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts",
			strings.NewReader(`{"ai":{"url":"","model":"","timeout":"20s"},"rules":[]}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		// The key goes with the endpoint: kept, it would have no origin for the next
		// save's move to be judged against.
		if saved, _ := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path}); saved.aiKey() != "" {
			t.Errorf("turning research off should take the key with it, got %q", saved.aiKey())
		}
		// And the endpoint can be turned back on without re-entering one.
		if w := put(t, h, `"url":"https://openrouter.ai/api/v1"`); w.Code != http.StatusOK {
			t.Fatalf("re-enable status = %d: %s", w.Code, w.Body.String())
		}
	})

	// A variable set to the empty string holds no key either — it silences this run and
	// leaves the file's credential for the next one, so the move is still refused.
	t.Run("an empty env key does not buy a move", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		t.Setenv("SKY_AI_URL", "")
		t.Setenv("SKY_AI_MODEL", "")
		t.Setenv("SKY_AI_KEY", "")
		h := alertsHandler(t, path)
		if w := put(t, h, `"url":"https://attacker.invalid/v1"`); w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if blob, _ := os.ReadFile(path); string(blob) != stored {
			t.Fatalf("a refused save must leave the file alone:\n%s", blob)
		}
	})

	// A variable that blanks the endpoint turns research off for the run without saying
	// anything about the file's key. An unrelated save must leave both alone, and the
	// file's endpoint is still the one the key is pinned to while the override is up.
	t.Run("an empty env endpoint is not the file turning off", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		t.Setenv("SKY_AI_URL", "")
		t.Setenv("SKY_AI_MODEL", "")
		h := alertsHandler(t, path)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(
			`{"ai":{"url":"https://openrouter.ai/api/v1","model":"perplexity/sonar","timeout":"20s"},"source":{"poll_interval":"30s"},"rules":[]}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		// Loaded without the overrides — research comes back with the key it had.
		saved, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path})
		if err != nil {
			t.Fatal(err)
		}
		if saved.aiKey() != "sk-or-v1-secret" {
			t.Errorf("key = %q, want the stored one kept through an unrelated save", saved.aiKey())
		}
		// And the guard is not blinded by the override either.
		if w := put(t, h, `"url":"https://attacker.invalid/v1"`); w.Code != http.StatusBadRequest {
			t.Fatalf("move status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	// An endpoint the environment pins is still an endpoint: a save whose own ai.url is
	// blank has not turned anything off, so it must not drop the file's key.
	t.Run("an env endpoint keeps the key through a blank url", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte("ai:\n  key: sk-or-v1-secret\n  model: perplexity/sonar\n"), 0o644)
		t.Setenv("SKY_AI_URL", "https://openrouter.ai/api/v1")
		h := alertsHandler(t, path)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts",
			strings.NewReader(`{"ai":{"url":"","model":"perplexity/sonar","timeout":"20s"},"rules":[]}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		saved, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path, "SKY_AI_URL=https://openrouter.ai/api/v1"})
		if err != nil {
			t.Fatal(err)
		}
		if saved.aiKey() != "sk-or-v1-secret" {
			t.Errorf("key = %q, want the stored one kept", saved.aiKey())
		}
	})

	t.Run("with no key stored there is nothing to carry", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte("ai:\n  url: http://localhost:11434/v1\n  model: llama3\n"), 0o644)
		h := alertsHandler(t, path)
		if w := put(t, h, `"url":"https://api.openai.com/v1"`); w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
	})

	// An environment key is not the page's to redirect either, and it cannot be replaced
	// from the page, so the endpoint is fixed with it.
	t.Run("an env key pins its endpoint", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte("ai:\n  url: https://openrouter.ai/api/v1\n  model: perplexity/sonar\n"), 0o644)
		t.Setenv("SKY_AI_KEY", "sk-or-v1-from-env")
		h := alertsHandler(t, path)
		w := put(t, h, `"url":"https://attacker.invalid/v1"`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "SKY_AI_URL") {
			t.Errorf("the error should say how to move it: %s", w.Body.String())
		}
	})

	// A key in the payload does not buy the move either, because it is not the key that
	// gets sent: the overlay replaces it with the variable's value.
	t.Run("an env key pins its endpoint against a supplied key", func(t *testing.T) {
		for _, supplied := range []string{`,"key":""`, `,"key":"sk-dummy"`} {
			path := filepath.Join(t.TempDir(), "alerts.yaml")
			os.WriteFile(path, []byte("ai:\n  url: https://openrouter.ai/api/v1\n  model: perplexity/sonar\n"), 0o644)
			t.Setenv("SKY_AI_KEY", "sk-or-v1-from-env")
			h := alertsHandler(t, path)
			w := put(t, h, `"url":"https://attacker.invalid/v1"`+supplied)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d for %s, want 400: %s", w.Code, supplied, w.Body.String())
			}
			saved, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path, "SKY_AI_KEY=sk-or-v1-from-env"})
			if err != nil {
				t.Fatal(err)
			}
			if saved.AI.URL != "https://openrouter.ai/api/v1" {
				t.Fatalf("the environment key must not follow the endpoint to %q", saved.AI.URL)
			}
		}
	})

	// An override is not forever. The payload's URL is overwritten for as long as the
	// variable is up, so a move looks harmless while it is — and becomes the disclosure
	// the moment it comes off, with the stored key pointed at whatever the page wrote.
	t.Run("an env endpoint does not license a move in the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte(stored), 0o644)
		t.Setenv("SKY_AI_KEY", "sk-or-v1-from-env")
		t.Setenv("SKY_AI_URL", "https://openrouter.ai/api/v1")
		h := alertsHandler(t, path)
		if w := put(t, h, `"url":"https://attacker.invalid/v1"`); w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		// Read back with the overrides gone, which is the run that would have posted the
		// key to whatever the refused save wanted written.
		saved, err := LoadAlerts([]string{"SKY_ALERTS_CONFIG=" + path})
		if err != nil {
			t.Fatal(err)
		}
		if saved.AI.URL != "https://openrouter.ai/api/v1" || saved.aiKey() != "sk-or-v1-secret" {
			t.Errorf("the file moved under the override: url = %q, key = %q", saved.AI.URL, saved.aiKey())
		}
	})

	// The other side of it: writing the endpoint the environment already pins into the
	// file is not a move, because the key is being sent there either way.
	t.Run("the env endpoint may be written into the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alerts.yaml")
		os.WriteFile(path, []byte("ai:\n  key: sk-or-v1-secret\n  model: perplexity/sonar\n"), 0o644)
		t.Setenv("SKY_AI_URL", "https://openrouter.ai/api/v1")
		h := alertsHandler(t, path)
		if w := put(t, h, `"url":"https://openrouter.ai/api/v1"`); w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
	})
}

// A file that cannot be read is a refusal, not a silent fresh start: the save is about to
// overwrite it, and defaulting writes away the credential it holds.
func TestSaveRefusesOverAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.yaml")
	broken := "ai:\n  url: https://openrouter.ai/api/v1\n  key: sk-or-v1-secret\n  model: perplexity/sonar\n  ["
	os.WriteFile(path, []byte(broken), 0o644)
	h := alertsHandler(t, path)
	w := httptest.NewRecorder()
	body := `{"ai":{"url":"https://openrouter.ai/api/v1","model":"perplexity/sonar","timeout":"20s"},"rules":[]}`
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/alerts", strings.NewReader(body)))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != broken {
		t.Fatalf("a refused save must leave the file alone:\n%s", blob)
	}
}
