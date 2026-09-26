package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fogleman/gg"
)

// ---------- map snapshots ----------

func goldenTile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "tile-11-474-825.pbf"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tileFixture answers every tile request with the same body, which is all the renderer
// needs: an empty body draws nothing but the background, so the overlay tests can assert
// on exact pixels, and the golden tile exercises the real decoder.
type tileFixture struct {
	mu       sync.Mutex
	body     []byte
	hits     int
	jsonCode int // status for the TileJSON, 0 means 200
	tileCode int // status for a tile, 0 means 200
	block    bool
	srv      *httptest.Server
}

func (f *tileFixture) start(t *testing.T) *tileStore {
	t.Helper()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/planet" {
			if f.jsonCode != 0 {
				w.WriteHeader(f.jsonCode)
				return
			}
			fmt.Fprintf(w, `{"tiles":["http://%s/planet/20260906_080001_pt/{z}/{x}/{y}.pbf"]}`, r.Host)
			return
		}
		if f.block {
			<-r.Context().Done() // a tile server that never answers
			return
		}
		f.mu.Lock()
		f.hits++
		body := f.body
		f.mu.Unlock()
		if f.tileCode != 0 {
			w.WriteHeader(f.tileCode)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	return newTileStore(f.srv.Client(), f.srv.URL+"/planet", t.TempDir())
}

func (f *tileFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func testRenderer(t *testing.T, f *tileFixture) *mapRenderer {
	t.Helper()
	m, err := newMapRenderer(f.start(t))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// mapAlert is an alert over Garland with a receiver and a short track behind it.
func mapAlert(gap bool) *Alert {
	lat, lon, trk := 32.93, -96.60, 90.0
	a := &Alert{Hex: "adeb2f", Trigger: "listed", Priority: 3,
		AC:          Aircraft{Hex: "adeb2f", Lat: &lat, Lon: &lon, Track: &trk},
		HasDistance: true, recvLat: 32.8998, recvLon: -96.6386}
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for i := range 10 {
		at := base.Add(time.Duration(i) * 20 * time.Second)
		if gap && i >= 5 {
			at = at.Add(5 * time.Minute)
		}
		a.Path = append(a.Path, sample{at, 32.93, -96.66 + float64(i)*0.006, 90})
	}
	return a
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("snapshot is not a PNG: %v", err)
	}
	if d := img.Bounds(); d.Dx() <= 0 || d.Dy() <= 0 || d.Dx() > mapPx || d.Dy() > mapPx {
		t.Fatalf("image is %v, want each edge within 1..%d", d, mapPx)
	}
	return img
}

func sameColor(a, b color.Color) bool {
	r1, g1, b1, _ := a.RGBA()
	r2, g2, b2, _ := b.RGBA()
	return r1 == r2 && g1 == g2 && b1 == b2
}

func TestFitViewFramesTheReceiverAndTheAircraft(t *testing.T) {
	// 40 NM apart, roughly north-east of the receiver.
	pts := []latlon{{32.90, -96.64}, {33.47, -96.09}}
	v := fitView(pts)
	if v.zoom != 10 {
		t.Errorf("zoom = %d, want the largest that still fits (10)", v.zoom)
	}
	// The image is square and cropped to the longer side of the box plus the margin, half
	// of it on each side, so the points sit that far in from the edges that side touches;
	// the other axis has whatever the square left over, which is never less.
	const inset = padding / 2 / (1 + padding)
	want := float64(v.w) * inset
	a, b := v.pixel(pts[0].lat, pts[0].lon), v.pixel(pts[1].lat, pts[1].lon)
	long := math.Abs(a.y-b.y) > math.Abs(a.x-b.x) // this pair is taller than it is wide
	for _, e := range []struct {
		name    string
		lo, hi  float64
		size    int
		decides bool
	}{
		{"x", math.Min(a.x, b.x), math.Max(a.x, b.x), v.w, !long},
		{"y", math.Min(a.y, b.y), math.Max(a.y, b.y), v.h, long},
	} {
		margin := math.Min(e.lo, float64(e.size)-e.hi)
		if margin < want-2 || (e.decides && margin > want+2) {
			t.Errorf("%s: points span %.1f..%.1f of %d, a %.1fpx margin; want %.1fpx%s",
				e.name, e.lo, e.hi, e.size, margin, want, map[bool]string{true: " exactly", false: " or more"}[e.decides])
		}
	}
}

// The crop is only as good as its edges: every framed point has to survive it, and the
// image has to stay between the width the footer needs and the long edge it is built for.
func TestFitViewCropKeepsItsPointsAndItsBounds(t *testing.T) {
	for name, c := range map[string]struct {
		pts    []latlon
		framed bool // false when the box cannot fit even at minZoom and is cropped instead
	}{
		// Mercator stretches towards the poles: the mean latitude is not the middle of
		// the projected box, and a frame centred on it drops the northern point.
		"a ten-degree span up north": {[]latlon{{70, 0}, {80, 0}}, true},
		"a tall narrow box":          {[]latlon{{0, 0}, {0.5, 0}}, true},
		"either side of the strait":  {[]latlon{{10, 179.9}, {10, -179.9}}, true},
		"a quarter of the equator":   {[]latlon{{0, 0}, {0, 100}}, false},
	} {
		v := fitView(c.pts)
		if v.w < minWidthPx || v.w > mapPx || v.h < 1 || v.h > mapPx {
			t.Errorf("%s: image is %dx%d, want %d..%d wide and at most %d tall",
				name, v.w, v.h, minWidthPx, mapPx, mapPx)
		}
		if !c.framed {
			continue
		}
		for _, p := range c.pts {
			q := v.pixel(p.lat, p.lon)
			if q.x < 0 || q.x > float64(v.w) || q.y < 0 || q.y > float64(v.h) {
				t.Errorf("%s: %v landed off the %dx%d image at %v", name, p, v.w, v.h, q)
			}
		}
	}
}

// A single point has no extent; the minimum span is what stops the zoom clamp dividing
// by zero and the receiver coinciding with the aircraft blanking the map.
func TestFitViewGivesADegenerateBoxAMinimumSpan(t *testing.T) {
	for _, pts := range [][]latlon{{{32.9, -96.6}}, {{32.9, -96.6}, {32.9, -96.6}}} {
		v := fitView(pts)
		if v.zoom != maxZoom {
			t.Errorf("a %d-point box should frame at the closest zoom, got %d", len(pts), v.zoom)
		}
		if q := v.pixel(32.9, -96.6); math.Abs(q.x-float64(v.w)/2) > 1 || math.Abs(q.y-float64(v.h)/2) > 1 {
			t.Errorf("single point should be centred, got %v", q)
		}
	}
}

func TestDecodeGoldenTile(t *testing.T) {
	layers, err := decodeTile(context.Background(), goldenTile(t))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]layer{}
	for _, l := range layers {
		byName[l.name] = l
	}
	for _, want := range []string{"place", "water", "transportation", "transportation_name", "boundary"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("layer %q missing; got %v", want, byName)
		}
	}
	place := byName["place"]
	if place.extent != 4096 {
		t.Errorf("extent = %d, want 4096", place.extent)
	}
	var found *feature
	for i := range place.features {
		if name, _ := place.features[i].props["name"].(string); name == "Garland" {
			found = &place.features[i]
		}
	}
	if found == nil {
		t.Fatal("no Garland in the place layer")
	}
	if found.kind != geomPoint || len(found.rings) != 1 || len(found.rings[0]) != 1 {
		t.Errorf("Garland should be one point, got kind %d rings %v", found.kind, found.rings)
	}
	if class, _ := found.props["class"].(string); class != "city" {
		t.Errorf("Garland class = %q, want city", class)
	}
}

// Tile bytes are third-party input decoded on the alert path. Every mangling of a real
// tile must come back as an error, never as a panic and never as a wild allocation.
func TestCorruptTilesAreRejectedNotFatal(t *testing.T) {
	good := goldenTile(t)
	for name, b := range map[string][]byte{
		"truncated":     good[:len(good)/2],
		"one byte":      good[:1],
		"garbage":       []byte("this is not a vector tile, not even slightly"),
		"absurd length": {0x1a, 0xff, 0xff, 0xff, 0xff, 0x7f},
		"zero field":    {0x00, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("decoding panicked: %v", r)
				}
			}()
			if _, err := decodeTile(context.Background(), b); err == nil {
				t.Error("want an error")
			}
		})
	}
	// Every single-byte truncation of the real tile, which walks the decoder through
	// every length and command count it reads.
	for i := 1; i < len(good); i += 977 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("truncation at %d panicked: %v", i, r)
				}
			}()
			decodeTile(context.Background(), good[:i])
		}()
	}
}

func TestDrawProducesAMapWithTheOverlaysOnIt(t *testing.T) {
	f := &tileFixture{body: goldenTile(t)}
	m := testRenderer(t, f)
	a := mapAlert(false)
	b := m.snapshot(context.Background(), a)
	if b == nil {
		t.Fatal("no snapshot")
	}
	img := decodePNG(t, b)

	v := fitView(framePoints(a))
	recv := v.pixel(a.recvLat, a.recvLon)
	if got := img.At(int(recv.x), int(recv.y)); sameColor(got, colBackground) {
		t.Errorf("nothing drawn at the receiver (%v)", got)
	}
	mid := a.Path[len(a.Path)/2]
	track := v.pixel(mid.lat, mid.lon)
	if got := img.At(int(track.x), int(track.y)); sameColor(got, colBackground) {
		t.Errorf("nothing drawn on the track (%v)", got)
	}
}

// A dropout is a gap, not a leg: drawing straight through one would invent a course the
// aircraft never flew.
func TestATrackWithAHoleIsDrawnAsTwoSegments(t *testing.T) {
	f := &tileFixture{} // empty tiles, so anything non-background is ours
	m := testRenderer(t, f)
	a := mapAlert(true)
	img := decodePNG(t, m.snapshot(context.Background(), a))

	v := fitView(framePoints(a))
	before, after := a.Path[4], a.Path[5]
	p, q := v.pixel(before.lat, before.lon), v.pixel(after.lat, after.lon)
	if segs := pathSegments(a.Path); len(segs) != 2 {
		t.Fatalf("want 2 segments, got %d", len(segs))
	}
	// The ends of the hole belong to the round caps of the two segments — at this line
	// width they reach about a tenth of the way in — so the question is whether the
	// middle of it is clear, not whether every pixel between the samples is.
	painted := 0
	for i := 2; i <= 8; i++ {
		frac := float64(i) / 10
		x, y := p.x+(q.x-p.x)*frac, p.y+(q.y-p.y)*frac
		if !sameColor(img.At(int(x), int(y)), colBackground) {
			painted++
		}
	}
	if painted > 0 {
		t.Errorf("%d of 7 pixels across the middle of the gap were painted; the hole was bridged", painted)
	}
}

// With no receiver configured the frame is about the aircraft, and nothing is marked at
// (0, 0) — which is in the Gulf of Guinea, not in Texas.
func TestFramingWithoutAReceiverCentresOnTheAircraft(t *testing.T) {
	f := &tileFixture{}
	m := testRenderer(t, f)
	a := mapAlert(false)
	a.HasDistance, a.recvLat, a.recvLon = false, 0, 0
	a.Path = nil // a brand-new contact: one position and nothing behind it

	v := fitView(framePoints(a))
	if q := v.pixel(*a.AC.Lat, *a.AC.Lon); math.Abs(q.x-float64(v.w)/2) > float64(v.w)/4 || math.Abs(q.y-float64(v.h)/2) > float64(v.h)/4 {
		t.Errorf("aircraft should sit near the middle, got %v", q)
	}
	img := decodePNG(t, m.snapshot(context.Background(), a))
	for y := range img.Bounds().Dy() {
		for x := range img.Bounds().Dx() {
			if sameColor(img.At(x, y), colReceiver) {
				t.Fatalf("a receiver was drawn at (%d,%d) with none configured", x, y)
			}
		}
	}
}

// The overlay is the subject of the picture, so nothing from the map may be drawn over it.
// Place names sit right where this aircraft is, and they used to cross it: the same alert
// over a blank map and over a real one has to show the same amount of aircraft.
func TestLabelsDoNotPaintOverTheAircraft(t *testing.T) {
	a := mapAlert(false)
	v := fitView(framePoints(a))
	p := v.pixel(*a.AC.Lat, *a.AC.Lon)

	aircraftPixels := func(f *tileFixture) int {
		m := testRenderer(t, f)
		img := decodePNG(t, m.snapshot(context.Background(), a))
		n := 0
		for y := max(int(p.y)-40, 0); y <= min(int(p.y)+40, img.Bounds().Dy()-1); y++ {
			for x := max(int(p.x)-40, 0); x <= min(int(p.x)+40, img.Bounds().Dx()-1); x++ {
				if sameColor(img.At(x, y), colAircraft) {
					n++
				}
			}
		}
		return n
	}

	over, blank := aircraftPixels(&tileFixture{body: goldenTile(t)}), aircraftPixels(&tileFixture{})
	if over != blank {
		t.Errorf("the aircraft is %d pixels over a map and %d over nothing; the map is drawn on top of it",
			over, blank)
	}
}

// The attribution is the obligation and the scale bar is the courtesy, so the bar is the
// one that gives way. Close in at 55N a single mile is 169px, which will not fit beside a
// 229px credit on a 440px frame — before the bar yielded, it was drawn straight through it.
func TestTheScaleBarYieldsToTheAttribution(t *testing.T) {
	m, err := newMapRenderer(nil)
	if err != nil {
		t.Fatal(err)
	}
	dc := gg.NewContext(10, 10)
	dc.SetFontFace(m.face(11))
	creditW, _ := dc.MeasureString(credit)

	for name, c := range map[string]struct {
		pts     []latlon
		wantBar bool
	}{
		"a mile apart at 55N":  {[]latlon{{55, 0}, {55.005, 0}}, false},
		"forty miles in Texas": {[]latlon{{32.90, -96.64}, {33.47, -96.09}}, true},
	} {
		v := fitView(c.pts)
		px, label, ok := m.scaleBar(dc, v, c.pts[0].lat)
		if ok != c.wantBar {
			t.Errorf("%s: %dpx wide at zoom %d drew bar=%v (%q, %.0fpx), want bar=%v",
				name, v.w, v.zoom, ok, label, px, c.wantBar)
			continue
		}
		if !ok {
			continue
		}
		lw, _ := dc.MeasureString(label)
		if right, creditLeft := 22+px+lw, float64(v.w-14)-creditW; right > creditLeft {
			t.Errorf("%s: scale bar ends at %.0f, over the credit starting at %.0f",
				name, right, creditLeft)
		}
	}
}

func TestSnapshotSurvivesWhatTheTileServerDoes(t *testing.T) {
	for name, f := range map[string]*tileFixture{
		"corrupt tile":      {body: []byte("not a tile at all, truly")},
		"tile server error": {tileCode: http.StatusInternalServerError},
		"discovery fails":   {jsonCode: http.StatusInternalServerError},
		// A 404 used to take the same mutex twice and hang the alert, everything queued
		// behind it, and shutdown — with no context able to break the wait.
		"discovery 404": {jsonCode: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			m := testRenderer(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan []byte, 1)
			go func() { done <- m.snapshot(ctx, mapAlert(false)) }()
			select {
			case b := <-done:
				if b != nil {
					t.Errorf("want no image, got %d bytes", len(b))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("snapshot never returned")
			}
		})
	}
}

// A tile the server does not have is ordinary — empty ocean, or past the dataset edge —
// so the map is still drawn, and the template is dropped in case it was the stale one.
func TestAMissingTileIsNotAMissingMap(t *testing.T) {
	f := &tileFixture{tileCode: http.StatusNotFound}
	store := f.start(t)
	if _, _, err := store.urlTemplate(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, err := newMapRenderer(store)
	if err != nil {
		t.Fatal(err)
	}
	if b := m.snapshot(context.Background(), mapAlert(false)); b == nil {
		t.Fatal("a tile that does not exist should still leave a map")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.template != "" {
		t.Error("a 404 should drop the template, in case it named a retired planet build")
	}
}

// One failure silences the fetcher for a while. Without it, a tile outage would add the
// whole render budget to every alert, and the notifier is one goroutine.
func TestOneTileFailureStopsTheNextAlertFromWaiting(t *testing.T) {
	f := &tileFixture{tileCode: http.StatusInternalServerError}
	store := f.start(t)
	if _, err := store.tiles(context.Background(), fitView([]latlon{{32.9, -96.6}})); err == nil {
		t.Fatal("want an error")
	}
	hits := f.count()
	if _, err := store.tiles(context.Background(), fitView([]latlon{{32.9, -96.6}})); err == nil {
		t.Fatal("want an error while the breaker is open")
	}
	if f.count() != hits {
		t.Errorf("the breaker should have answered without asking the server again")
	}
}

func TestCachedTilesAreReusedRefreshedAndRepaired(t *testing.T) {
	f := &tileFixture{body: goldenTile(t)}
	store := f.start(t)
	v := fitView([]latlon{{32.9, -96.6}})

	if _, err := store.tiles(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	first := f.count()
	if first == 0 {
		t.Fatal("nothing was fetched")
	}
	if _, err := store.tiles(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if f.count() != first {
		t.Errorf("a second look should come from disk, got %d extra fetches", f.count()-first)
	}

	var cached []string
	filepath.WalkDir(store.dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			cached = append(cached, path)
		}
		return nil
	})
	if len(cached) == 0 {
		t.Fatal("nothing was cached")
	}

	// A file scribbled on by a half-finished write is thrown away, not decoded.
	if err := os.WriteFile(cached[0], []byte("rubbish"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.tiles(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if f.count() <= first {
		t.Error("a corrupt cached tile should have been refetched")
	}

	// Past its TTL it is refetched even though it is perfectly good.
	second := f.count()
	old := time.Now().Add(-tileCacheTTL - time.Hour)
	for _, p := range cached {
		os.Chtimes(p, old, old)
	}
	if _, err := store.tiles(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if f.count() <= second {
		t.Error("a stale cached tile should have been refetched")
	}
}

// The cache key carries the planet build the tiles came from, so a refreshed template
// cannot keep serving tiles the server has already replaced.
func TestTemplateVersionKeysTheCache(t *testing.T) {
	for template, want := range map[string]string{
		"https://tiles.example/planet/20260906_080001_pt/{z}/{x}/{y}.pbf": "20260906_080001_pt",
		"https://tiles.example/planet/{z}/{x}/{y}.pbf":                    "planet",
		"https://tiles.example/{z}/{x}/{y}.pbf":                           "tiles.example",
		"https://tiles.example/planet/../{z}/{x}/{y}.pbf":                 "unversioned",
	} {
		if got := templateVersion(template); got != want {
			t.Errorf("templateVersion(%q) = %q, want %q", template, got, want)
		}
	}
}

func TestNotifyPutsTheImageWithTheSameMessageInHeaders(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tar1090URL = "https://tar1090.example.com/"
	s := &ntfyServer{}
	n := s.start(t, cfg)
	n.maps = testRenderer(t, &tileFixture{body: goldenTile(t)})

	a := mapAlert(false)
	a.Plane = &Plane{ICAO: "adeb2f", Reg: "N12345", Operator: "US Air Force", Type: "C-17"}
	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if len(s.methods) != 1 || s.methods[0] != http.MethodPut {
		t.Fatalf("want one PUT, got %v", s.methods)
	}
	// ntfy takes an attachment only at /<topic>, not at the root the JSON path uses.
	if s.paths[0] != "/"+cfg.ntfyTopic() {
		t.Errorf("attachment path = %q, want /%s", s.paths[0], cfg.ntfyTopic())
	}
	if len(s.files[0]) == 0 || !bytes.HasPrefix(s.files[0], []byte("\x89PNG")) {
		t.Errorf("body is not a PNG (%d bytes)", len(s.files[0]))
	}
	h := s.headers[0]
	if h.Get("X-Title") != "US Air Force C-17" {
		t.Errorf("X-Title = %q", h.Get("X-Title"))
	}
	if h.Get("X-Priority") != "3" {
		t.Errorf("X-Priority = %q", h.Get("X-Priority"))
	}
	if !strings.Contains(h.Get("X-Tags"), "airplane") {
		t.Errorf("X-Tags = %q", h.Get("X-Tags"))
	}
	if h.Get("X-Click") != "https://tar1090.example.com/?icao=adeb2f" {
		t.Errorf("X-Click = %q", h.Get("X-Click"))
	}
	if !strings.Contains(h.Get("X-Actions"), "view") || !strings.Contains(h.Get("X-Actions"), "tar1090") {
		t.Errorf("X-Actions = %q", h.Get("X-Actions"))
	}
	if h.Get("X-Filename") == "" {
		t.Error("X-Filename must be set or ntfy will not treat the body as a file")
	}
	// A header is one line, so the body's newlines travel as the literal escape ntfy
	// turns back into newlines. Nothing may be lost on the way.
	msg := h.Get("X-Message")
	if strings.Contains(msg, "\n") || !strings.Contains(msg, `\n`) {
		t.Errorf("X-Message should carry escaped newlines: %q", msg)
	}
	if !strings.Contains(msg, "N12345") || !strings.Contains(msg, "Registration") {
		t.Errorf("X-Message lost the body: %q", msg)
	}
}

// Without an image nothing about delivery changes, so the no-snapshot case cannot regress.
func TestNotifyWithoutAnImagePostsExactlyAsBefore(t *testing.T) {
	for name, f := range map[string]*tileFixture{
		"render fails": {tileCode: http.StatusInternalServerError},
		"maps off":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			s := &ntfyServer{}
			n := s.start(t, cfg)
			if f != nil {
				n.maps = testRenderer(t, f)
			}
			if err := n.Publish(context.Background(), mapAlert(false)); err != nil {
				t.Fatal(err)
			}
			if len(s.methods) != 1 || s.methods[0] != http.MethodPost || s.paths[0] != "/" {
				t.Fatalf("want one POST to the root, got %v %v", s.methods, s.paths)
			}
			if s.bodies[0].Topic != cfg.ntfyTopic() {
				t.Errorf("the JSON publish document lost its topic: %+v", s.bodies[0])
			}
		})
	}
}

// A server that will not take the attachment must cost the alert its picture, not the
// alert: notifyLoop answers a permanent rejection by backing off without a cooldown.
func TestAnAttachmentTooBigStillDeliversTheAlert(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{statuses: []int{http.StatusRequestEntityTooLarge, http.StatusOK}}
	n := s.start(t, cfg)
	n.maps = testRenderer(t, &tileFixture{body: goldenTile(t)})
	hist, err := NewHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	n.history = hist

	if err := n.Publish(context.Background(), mapAlert(false)); err != nil {
		t.Fatalf("the alert must survive its picture being refused: %v", err)
	}
	if len(s.methods) != 2 || s.methods[0] != http.MethodPut || s.methods[1] != http.MethodPost {
		t.Fatalf("want a PUT then a POST, got %v", s.methods)
	}
	if n.LastErr() != nil {
		t.Errorf("a delivered alert must not degrade health: %v", n.LastErr())
	}
	page, err := hist.List("listed", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Errorf("want exactly one history row for one delivered alert, got %d", page.Total)
	}
}

// notifyCtx is cancelled only after the drain window, so a render hung on it would eat
// the drain and then have its own delivery cancelled. Shutdown drops the picture instead.
func TestShutdownAbandonsTheRenderAndStillSendsTheAlert(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{}
	n := s.start(t, cfg)
	n.maps = testRenderer(t, &tileFixture{block: true})
	shutdown, stop := context.WithCancel(context.Background())
	n.shutdown = shutdown

	done := make(chan error, 1)
	go func() { done <- n.Publish(context.Background(), mapAlert(false)) }()
	time.Sleep(50 * time.Millisecond)
	stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the alert should still go out: %v", err)
		}
	case <-time.After(drainTimeout):
		t.Fatal("the alert did not land inside the drain window")
	}
	if len(s.methods) != 1 || s.methods[0] != http.MethodPost {
		t.Errorf("want the plain JSON publish, got %v", s.methods)
	}
	// And once it is shutting down, nothing even starts drawing.
	s2 := &ntfyServer{}
	n2 := s2.start(t, cfg)
	n2.maps, n2.shutdown = n.maps, shutdown
	if b := n2.snapshot(context.Background(), mapAlert(false)); b != nil {
		t.Error("a render should not begin during shutdown")
	}
}

// Evaluate is where the path is copied: the Tracker belongs to pollLoop and is unlocked,
// so the renderer must never be handed a live slice.
func TestAlertCarriesACopyOfTheTrack(t *testing.T) {
	cfg := testConfig(t)
	alerts := defaultAlerts()
	alerts.Lat, alerts.Lon = &testLat, &testLon
	alerts.Rules = []Rule{{Name: "listed", Listed: boolp(true)}}
	db := dbWith(t, cfg, mustParse(t, sampleCSV))

	lat, lon, hdg := testLat, testLon, 90.0
	tr := NewTracker()
	for i := range 3 {
		tr.Update(&feed{Now: float64(1000 + i*10), Aircraft: []Aircraft{
			{Hex: "adeb2f", Lat: &lat, Lon: &lon, Track: &hdg, SeenPos: float64(-i * 10)}}}, circleWindow)
	}
	a := Evaluate(at(Aircraft{Hex: "adeb2f"}), db, alerts, tr.get("adeb2f"))
	if a == nil {
		t.Fatal("no alert")
	}
	if len(a.Path) != 3 {
		t.Fatalf("want the three samples the tracker holds, got %d", len(a.Path))
	}
	a.Path[0].lat = 0
	if tr.get("adeb2f").samples[0].lat == 0 {
		t.Error("the alert shares the Tracker's slice; a render would race the poll loop")
	}
	// The preview path has no map and no use for a path.
	if hits := func() []LiveHit {
		_, h := MatchLive(alerts.Rules[0], []Aircraft{at(Aircraft{Hex: "adeb2f"})},
			map[string]turnState{}, db, alerts, 10)
		return h
	}(); len(hits) != 1 {
		t.Fatalf("the live preview should still match, got %d", len(hits))
	}
}

// A tile is a compression format, so the byte limit the fetcher applies is not a memory
// limit. These are the compact inputs that used to blow past it.
func TestCompactTilesCannotInflateIntoMemory(t *testing.T) {
	for name, tile := range map[string][]byte{
		// Two bytes per feature, and each becomes a struct plus a slice header.
		"a flood of empty features": mvtTile(bytes.Repeat([]byte{0x12, 0x00}, maxTileFeatures+1)),
		// One byte per ClosePath, and each appends another point to the ring.
		"a flood of closed rings": mvtTile(mvtFeature(append(
			[]byte{9, 0, 0}, // MoveTo(1) at the origin, so there is a ring to close
			bytes.Repeat([]byte{15}, maxTilePoints+1)...))),
		// Two bytes per key name, and each becomes a string header in the layer table.
		"a flood of key names": mvtTile(bytes.Repeat([]byte{0x1a, 0x00}, maxTileEntries+1)),
		// Two bytes per tag pair, all naming the same key. The props map used to be
		// sized by the tag count rather than by how many distinct keys can exist, so
		// this asked for millions of buckets to store one property.
		"a flood of repeated tags": mvtTile(mvtTagBomb(maxTileEntries + 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeTile(context.Background(), tile); err == nil {
				t.Error("want the tile rejected before it is allocated")
			} else if !strings.Contains(err.Error(), "in one tile") {
				t.Errorf("want a budget error, got %v", err)
			}
		})
	}
	// And the real thing still decodes, so the ceilings are not set below reality.
	if _, err := decodeTile(context.Background(), goldenTile(t)); err != nil {
		t.Errorf("a real tile must still decode: %v", err)
	}
}

// mvtTile wraps layer bytes as Tile.layers, and mvtFeature wraps a geometry command
// stream as one Layer.feature. Enough protobuf to build a hostile tile by hand.
func mvtTile(layer []byte) []byte {
	body := append([]byte{0x0a, 0x01, 'x'}, layer...) // Layer.name = "x"
	return append(append([]byte{0x1a}, varint(len(body))...), body...)
}

func mvtFeature(geometry []byte) []byte {
	geom := append(append([]byte{0x22}, varint(len(geometry))...), geometry...) // Feature.geometry
	body := append([]byte{0x18, 0x03}, geom...)                                 // Feature.type = POLYGON
	return append(append([]byte{0x12}, varint(len(body))...), body...)
}

// mvtTagBomb is one key, one value, and a feature naming that key pairs times over.
func mvtTagBomb(pairs int) []byte {
	tags := make([]byte, pairs*2) // packed (0, 0) varints, two bytes a pair
	feature := append(append([]byte{0x12}, varint(len(tags))...), tags...)
	out := []byte{0x1a, 0x01, 'k', 0x22, 0x03, 0x0a, 0x01, 'v'} // keys ["k"], values ["v"]
	return append(out, append(append([]byte{0x12}, varint(len(feature))...), feature...)...)
}

func varint(n int) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}

// The shapes are third-party path data, and the parser that walks them is ours. Either
// one can break in a way that draws nothing at all, so draw every shape in the table and
// insist each one leaves ink where it was asked to leave it. The placement half is not
// decoration: a shape carrying its own transform is drawn through one more matrix than
// the rest, and getting that matrix in the wrong order moves the marker off the aircraft
// while still painting a perfectly good aeroplane.
func TestEveryAircraftIconDrawsWhereItIsPut(t *testing.T) {
	// Room for the longest shape turned onto the diagonal, so nothing is clipped and the
	// ink the test measures is the whole shape.
	const size, mid = 200, 100.0
	for name, s := range icons.Shapes {
		dc := gg.NewContext(size, size)
		dc.SetColor(colBackground)
		dc.Clear()
		if !drawShape(dc, s, 1, mid, mid, 45, colAircraft, colHalo) {
			t.Errorf("%s: path would not parse", name)
			continue
		}
		painted, minX, minY, maxX, maxY := 0, size, size, -1, -1
		for y := range size {
			for x := range size {
				if sameColor(dc.Image().At(x, y), colBackground) {
					continue
				}
				painted++
				minX, minY = min(minX, x), min(minY, y)
				maxX, maxY = max(maxX, x), max(maxY, y)
			}
		}
		if painted < 100 {
			t.Errorf("%s: only %d pixels painted; the shape came out empty", name, painted)
			continue
		}
		// A shape is centred in its viewBox, not in its own ink — a long tail or an
		// accent hanging off the back pulls this a few pixels either way. Six is the
		// worst of them; anything beyond this is a marker that has left its aircraft.
		off := math.Hypot(float64(minX+maxX)/2-mid, float64(minY+maxY)/2-mid)
		if off > 12 {
			t.Errorf("%s: ink sits %.0fpx from where it was drawn", name, off)
		}
	}
}

// The designator is what makes a 747 look like a 747. The category is the fallback and
// says only how heavy and whether it has rotors; an aircraft with neither still gets a
// marker rather than nothing.
func TestTheIconFollowsTheTypeThenTheCategory(t *testing.T) {
	for _, tc := range []struct{ typ, cat, want string }{
		{"B744", "A3", "heavy_4e"},   // the designator wins over a category that disagrees
		{"EC35", "A7", "helicopter"}, // no designator entry, so the category answers
		{"ZZZZ", "A7", "helicopter"},
		{"", "A1", "cessna"},
		{"ZZZZ", "ZZ", "unknown"},
		{"b738", "", "b738"}, // a lower-case type code off the feed is the same aircraft
	} {
		s, _ := iconFor(tc.typ, tc.cat)
		if s != icons.Shapes[tc.want] {
			t.Errorf("iconFor(%q, %q) is not %s", tc.typ, tc.cat, tc.want)
		}
	}
}

// A circling alert is about the circle, so the whole orbit has to be in the picture —
// but only the orbit: the leg the aircraft flew to get there still runs off the edge.
func TestACirclingAircraftsOrbitIsFramed(t *testing.T) {
	a := mapAlert(false)
	a.HasDistance = false
	a.Circling = true
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// A long approach from the far west, well outside circleWindow of the latest sample.
	a.Path = []sample{{base, 32.93, -97.5, 90}, {base.Add(30 * time.Second), 32.93, -97.4, 90}}
	// Then a 1.5 NM orbit whose centre is east of where the aircraft is now, sampled
	// every 20 s for one lap starting 15 minutes later.
	cLat, cLon, r := 32.93, -96.60+1.5/60/math.Cos(32.93*math.Pi/180), 1.5/60.0
	start := base.Add(15 * time.Minute)
	for i := 0; i <= 24; i++ {
		ang := math.Pi + float64(i)*2*math.Pi/24
		lat := cLat + r*math.Sin(ang)
		lon := cLon + r*math.Cos(ang)/math.Cos(cLat*math.Pi/180)
		a.Path = append(a.Path, sample{start.Add(time.Duration(i) * 20 * time.Second), lat, lon, 0})
	}
	last := a.Path[len(a.Path)-1]
	*a.AC.Lat, *a.AC.Lon = last.lat, last.lon

	v := fitView(framePoints(a))
	for _, s := range a.Path[2:] {
		if p := v.pixel(s.lat, s.lon); p.x < 0 || p.y < 0 || p.x > float64(v.w) || p.y > float64(v.h) {
			t.Fatalf("orbit sample %v falls outside the %dx%d frame at %v", s, v.w, v.h, p)
		}
	}
	if p := v.pixel(a.Path[0].lat, a.Path[0].lon); p.x >= 0 {
		t.Errorf("the approach leg was framed too (x = %.0f); it should run off the edge", p.x)
	}

	a.Circling = false
	v = fitView(framePoints(a))
	far := a.Path[2+12] // the opposite side of the orbit from the aircraft
	if p := v.pixel(far.lat, far.lon); p.x >= 0 && p.x <= float64(v.w) {
		t.Errorf("without Circling the orbit should not drive the frame (x = %.0f of %d)", p.x, v.w)
	}
}
