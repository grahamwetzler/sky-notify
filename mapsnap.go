package main

import (
	"bytes"
	"context"
	"fmt"
	"image/color"
	"image/png"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
)

// snapshotBudget bounds the whole picture: discovery, tiles and raster. It sits outside
// the delivery budget, so a slow map delays the alert by at most this much and never
// eats the time the notification itself is allowed.
const snapshotBudget = 8 * time.Second

// The fiord palette, hard-coded. A MapLibre style-JSON interpreter — zoom interpolations,
// filter expressions, sprites — is a great deal of machinery to colour ten layer classes;
// when a map reads wrong, the fix is another line here.
var (
	colBackground = mustColor("#45516E")
	colLandcover  = color.NRGBA{0x3F, 0x42, 0x5A, 0x92}
	// Residential land is the one landuse fiord paints light, and it is what makes a
	// suburb read as a suburb rather than as empty ground.
	colResidential = color.NRGBA{0xEA, 0xEA, 0xE6, 0x16}
	colPark        = mustColor("#4A5C68")
	// Darker than fiord's own water: on a phone, at this size, its #38435C is a shade
	// away from the ground and a lake vanishes. A recognisable lake is the whole point.
	colWater    = mustColor("#2C3550")
	colWaterway = mustColor("#373B58")
	// A dark casing under a light fill: the inverse of the daylight convention, and what
	// makes a motorway the first thing the eye finds on a dark ground.
	colRoadCasing = mustColor("#2B3145")
	colMotorway   = mustColor("#96A9D2")
	colRoadMajor  = mustColor("#6E7DA6")
	colRoadInner  = mustColor("#3B4359")
	colRoadMinor  = color.NRGBA{0x5A, 0x67, 0x8C, 0xB4}
	// Alpha goes through NRGBA: color.RGBA is alpha-premultiplied, so these values
	// written there would paint far brighter than the style asks for.
	colBoundary   = color.NRGBA{0x71, 0xB5, 0xCC, 0x42}
	colHalo       = color.NRGBA{0x14, 0x18, 0x26, 0xE0}
	colPlace      = mustColor("#B2C9D1")
	colPlaceSmall = mustColor("#A1C7D4")
	colWaterName  = mustColor("#6B799E")
	colRef        = mustColor("#A3BFE6")
	colTrack      = mustColor("#5BE9F2")
	colAircraft   = mustColor("#FFFFFF")
	colReceiver   = mustColor("#FFD166")
	colFooter     = color.NRGBA{0xC8, 0xD4, 0xE4, 0xB4}
)

// mapRenderer draws one PNG per alert. It owns the tile store and the embedded font, both
// of which are built once: the font is parsed at startup so a render never pays for it.
type mapRenderer struct {
	tiles *tileStore
	font  *truetype.Font
}

func newMapRenderer(tiles *tileStore) (*mapRenderer, error) {
	f, err := truetype.Parse(goregular.TTF)
	if err != nil {
		return nil, err
	}
	return &mapRenderer{tiles: tiles, font: f}, nil
}

// snapshot is the only entry point the notifier calls, and it never returns an error:
// a missing picture is an ordinary outcome, and the alert goes out either way.
//
// The recover is not decoration. Tile bytes are third-party input and notifyLoop is a
// single goroutine with nothing above it to catch a panic, so an index slip in the
// decoder would not cost one alert its image — it would end alerting.
func (m *mapRenderer) snapshot(ctx context.Context, a *Alert) (b []byte) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("map snapshot panicked, sending the alert without it", "icao", a.Hex, "err", r)
			b = nil
		}
	}()

	v := fitView(framePoints(a))
	tiles, err := m.tiles.tiles(ctx, v)
	if err != nil {
		slog.Debug("no map tiles for this alert", "icao", a.Hex, "err", err)
		return nil
	}
	img, err := m.draw(ctx, v, tiles, a)
	if err != nil {
		slog.Debug("map render failed", "icao", a.Hex, "err", err)
		return nil
	}
	return img
}

// framePoints is everything the map has to contain: the aircraft, and the receiver only
// when one is configured. Reading recvLat/recvLon unconditionally would plant a receiver
// at (0, 0) and drag the frame into the Atlantic. The path is deliberately not framed —
// a long tail would shrink the two things the alert is about; it just runs off the edge.
func framePoints(a *Alert) []latlon {
	var pts []latlon
	if a.AC.Lat != nil && a.AC.Lon != nil {
		pts = append(pts, latlon{*a.AC.Lat, *a.AC.Lon})
	}
	if a.HasDistance {
		pts = append(pts, latlon{a.recvLat, a.recvLon})
	}
	return pts
}

func (m *mapRenderer) draw(ctx context.Context, v view, tiles []placedTile, a *Alert) ([]byte, error) {
	dc := gg.NewContext(v.w, v.h)
	dc.SetColor(colBackground)
	dc.Clear()

	// Round caps and joins, once: roads are made of many short segments and a butt join
	// leaves a notch at every bend.
	dc.SetLineCapRound()
	dc.SetLineJoinRound()

	// Painter's order, bottom up: a fill where width is 0, a stroke otherwise. The
	// context is checked between tiles inside eachFeature, which is what gives the render
	// deadline teeth — a context cannot interrupt a loop by itself.
	for _, p := range []struct {
		layer   string
		col     color.Color
		width   float64
		minZoom int
		pick    func(*feature) bool
	}{
		{layer: "landcover", col: colLandcover, pick: classIs("wood", "grass", "forest")},
		{layer: "landuse", col: colResidential, pick: propIs("subclass", "residential")},
		{layer: "park", col: colPark},
		{layer: "water", col: colWater},
		{layer: "waterway", col: colWaterway, width: 1.4},
		{layer: "aeroway", col: colRoadInner},
		{layer: "aeroway", col: colRoadCasing, width: 3, pick: classIs("runway")},
		{layer: "aeroway", col: colRoadInner, width: 1.6, pick: classIs("runway")},
		{layer: "transportation", col: colRoadCasing, width: 3.4, pick: classIs("trunk", "primary", "secondary")},
		{layer: "transportation", col: colRoadMajor, width: 1.8, pick: classIs("trunk", "primary", "secondary")},
		{layer: "transportation", col: colRoadCasing, width: 5.5, pick: classIs("motorway")},
		{layer: "transportation", col: colMotorway, width: 3, pick: classIs("motorway")},
		// Residential streets only close in. Further out they stop saying which suburb
		// this is and just cover the frame in a hairball.
		{layer: "transportation", col: colRoadMinor, width: 1, minZoom: 12, pick: classIs("minor")},
		{layer: "boundary", col: colBoundary, width: 1.4, pick: adminLevel(4)},
	} {
		if v.zoom < p.minZoom {
			continue
		}
		err := eachFeature(ctx, tiles, v, p.layer, func(f *feature, project func(pt) pt) {
			filled := p.width == 0
			if (p.pick != nil && !p.pick(f)) || (filled && f.kind != geomPolygon) || f.kind == geomPoint {
				return
			}
			trace(dc, f, project)
			dc.SetColor(p.col)
			if filled {
				dc.Fill()
				return
			}
			dc.SetLineWidth(p.width)
			dc.Stroke()
		})
		if err != nil {
			return nil, err
		}
	}

	m.drawOverlays(dc, v, a)
	if err := m.drawLabels(ctx, dc, v, tiles); err != nil {
		return nil, err
	}
	m.drawFooter(dc, v, a)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dc.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawOverlays paints what the notification is actually about, over the map.
func (m *mapRenderer) drawOverlays(dc *gg.Context, v view, a *Alert) {
	// One polyline per run of samples. The path is split wherever the gap between two
	// samples exceeds maxSampleGap: a dropout is not a straight line across the map, and
	// drawing it as one would invent a leg the aircraft never flew.
	for _, seg := range pathSegments(a.Path) {
		for _, w := range []struct {
			col   color.Color
			width float64
		}{{colHalo, 6}, {colTrack, 3}} {
			for i, s := range seg {
				p := v.pixel(s.lat, s.lon)
				if i == 0 {
					dc.MoveTo(p.x, p.y)
				} else {
					dc.LineTo(p.x, p.y)
				}
			}
			dc.SetColor(w.col)
			dc.SetLineWidth(w.width)
			dc.Stroke()
		}
	}

	if a.HasDistance {
		p := v.pixel(a.recvLat, a.recvLon)
		dc.SetColor(colHalo)
		dc.DrawCircle(p.x, p.y, 9)
		dc.SetLineWidth(5)
		dc.Stroke()
		dc.SetColor(colReceiver)
		dc.DrawCircle(p.x, p.y, 9)
		dc.SetLineWidth(2)
		dc.Stroke()
		dc.DrawCircle(p.x, p.y, 3)
		dc.Fill()
	}

	if a.AC.Lat == nil || a.AC.Lon == nil {
		return
	}
	p := v.pixel(*a.AC.Lat, *a.AC.Lon)
	if a.AC.Track == nil {
		// No heading, so no direction to point: a dot says where without claiming which
		// way. One sample, no samples and no heading are all ordinary.
		dc.SetColor(colHalo)
		dc.DrawCircle(p.x, p.y, 8)
		dc.Fill()
		dc.SetColor(colAircraft)
		dc.DrawCircle(p.x, p.y, 5)
		dc.Fill()
		return
	}
	dc.Push()
	dc.Translate(p.x, p.y)
	dc.Rotate(gg.Radians(*a.AC.Track)) // 0° is north, and so is -Y on the canvas
	plane := [][2]float64{{0, -11}, {2.5, -3}, {11, 4}, {11, 6.5}, {2.5, 4}, {2.5, 8},
		{5, 10.5}, {5, 12}, {0, 10.5}, {-5, 12}, {-5, 10.5}, {-2.5, 8}, {-2.5, 4},
		{-11, 6.5}, {-11, 4}, {-2.5, -3}}
	for i, q := range plane {
		if i == 0 {
			dc.MoveTo(q[0], q[1])
		} else {
			dc.LineTo(q[0], q[1])
		}
	}
	dc.ClosePath()
	dc.SetColor(colHalo)
	dc.SetLineWidth(3.5)
	dc.StrokePreserve()
	dc.SetColor(colAircraft)
	dc.Fill()
	dc.Pop()
}

// pathSegments splits a track wherever it goes quiet for longer than maxSampleGap.
func pathSegments(path []sample) [][]sample {
	var out [][]sample
	start := 0
	for i := 1; i <= len(path); i++ {
		if i == len(path) || path[i].t.Sub(path[i-1].t) > maxSampleGap {
			if i-start >= 2 {
				out = append(out, path[start:i])
			}
			start = i
		}
	}
	return out
}

// ---------- labels ----------

type labelBox struct{ x0, y0, x1, y1 float64 }

func (b labelBox) hits(o labelBox) bool {
	return b.x0 < o.x1 && o.x0 < b.x1 && b.y0 < o.y1 && o.y0 < b.y1
}

type labelCandidate struct {
	text  string
	at    pt
	size  float64
	col   color.Color
	rank  float64 // lower wins the space
	badge bool
}

// drawLabels places names greedily in priority order and drops any whose box lands on one
// already placed. No SDF glyphs and no line-following: place names sit horizontally at
// their point and road numbers become badges, which is the known weak spot of this
// renderer and the first thing to extend if real alerts read badly.
func (m *mapRenderer) drawLabels(ctx context.Context, dc *gg.Context, v view, tiles []placedTile) error {
	var cands []labelCandidate

	err := eachFeature(ctx, tiles, v, "place", func(f *feature, project func(pt) pt) {
		name, _ := f.props["name"].(string)
		class, _ := f.props["class"].(string)
		if name == "" || len(f.rings) == 0 || len(f.rings[0]) == 0 {
			return
		}
		size, col := 15.0, colPlace
		switch class {
		case "city":
		case "town":
			size = 13
		case "village", "suburb", "neighbourhood":
			if v.zoom < 12 {
				return
			}
			size, col = 11, colPlaceSmall
		default:
			return
		}
		rank, _ := f.props["rank"].(float64)
		cands = append(cands, labelCandidate{name, project(f.rings[0][0]), size, col, rank, false})
	})
	if err != nil {
		return err
	}

	// One name per lake, not one per tile and per shoreline segment: a big reservoir is
	// carved across several tiles and would otherwise label itself five times over.
	seen := map[string]bool{}
	err = eachFeature(ctx, tiles, v, "water_name", func(f *feature, project func(pt) pt) {
		name, _ := f.props["name"].(string)
		if name == "" || seen[name] || len(f.rings) == 0 || len(f.rings[0]) == 0 {
			return
		}
		seen[name] = true
		cands = append(cands, labelCandidate{name, project(midpoint(f.rings[0])), 11, colWaterName, 50, false})
	})
	if err != nil {
		return err
	}

	err = eachFeature(ctx, tiles, v, "transportation_name", func(f *feature, project func(pt) pt) {
		ref, _ := f.props["ref"].(string)
		class, _ := f.props["class"].(string)
		// Junctions live in this layer too, and their "ref" is an exit number. Badging
		// those fills the map with 60A and 61B and crowds out the road numbers — the one
		// thing a road badge is for.
		sub, _ := f.props["subclass"].(string)
		if ref == "" || sub == "junction" || f.kind != geomLine ||
			(class != "motorway" && class != "trunk") || seen[ref] {
			return
		}
		if len(f.rings) == 0 || len(f.rings[0]) == 0 {
			return
		}
		seen[ref] = true
		cands = append(cands, labelCandidate{ref, project(midpoint(f.rings[0])), 11, colRef, 80, true})
	})
	if err != nil {
		return err
	}

	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })

	var placed []labelBox
	for _, c := range cands {
		if c.at.x < 0 || c.at.x > float64(v.w) || c.at.y < 0 || c.at.y > float64(v.h-footerHeight) {
			continue
		}
		dc.SetFontFace(m.face(c.size))
		w, h := dc.MeasureString(c.text)
		box := labelBox{c.at.x - w/2 - 3, c.at.y - h/2 - 3, c.at.x + w/2 + 3, c.at.y + h/2 + 3}
		if overlapsAny(placed, box) {
			continue
		}
		placed = append(placed, box)
		if c.badge {
			dc.SetColor(colHalo)
			dc.DrawRoundedRectangle(box.x0, box.y0, box.x1-box.x0, box.y1-box.y0, 3)
			dc.Fill()
		}
		haloText(dc, c.text, c.at.x, c.at.y, c.col)
	}
	return nil
}

func overlapsAny(boxes []labelBox, b labelBox) bool {
	for _, o := range boxes {
		if b.hits(o) {
			return true
		}
	}
	return false
}

// footerHeight is the strip the attribution and scale bar own; labels stay out of it.
const footerHeight = 34

// credit is the ODbL obligation. It travels in the image because the image is what gets
// forwarded, so it is the one part of the footer that is never dropped.
const credit = "© OpenStreetMap contributors · OpenFreeMap"

// scaleBar picks the longest round distance whose bar and label still fit in the room the
// credit leaves. The bar is a courtesy and the credit is not, so on a narrow frame — or a
// close zoom near the poles, where a single mile can be a thousand pixels — the honest
// answer is no bar at all rather than one written through the attribution.
func (m *mapRenderer) scaleBar(dc *gg.Context, v view, lat float64) (px float64, label string, ok bool) {
	// Metres per pixel at this latitude, converted to nautical miles.
	nmPerPx := 156543.03392 * math.Cos(lat*math.Pi/180) / math.Exp2(float64(v.zoom)) / 1852
	dc.SetFontFace(m.face(11))
	creditW, _ := dc.MeasureString(credit)
	limit := float64(v.w-14) - creditW - 8
	for _, nm := range []float64{500, 200, 100, 50, 20, 10, 5, 2, 1} {
		l := fmt.Sprintf("%.0f NM", nm)
		lw, _ := dc.MeasureString(l)
		if w := nm / nmPerPx; 16+w+6+lw <= limit {
			return w, l, true
		}
	}
	return 0, "", false
}

// drawFooter writes the attribution and, when there is room for it, a scale bar.
func (m *mapRenderer) drawFooter(dc *gg.Context, v view, a *Alert) {
	lat := 0.0
	if a.AC.Lat != nil {
		lat = *a.AC.Lat
	}
	y := float64(v.h - 14)

	if barPx, label, ok := m.scaleBar(dc, v, lat); ok {
		dc.SetColor(colHalo)
		dc.SetLineWidth(5)
		dc.DrawLine(16, y, 16+barPx, y)
		dc.Stroke()
		dc.SetColor(colFooter)
		dc.SetLineWidth(2)
		dc.DrawLine(16, y, 16+barPx, y)
		dc.DrawLine(16, y-4, 16, y+4)
		dc.DrawLine(16+barPx, y-4, 16+barPx, y+4)
		dc.Stroke()
		dc.SetFontFace(m.face(11))
		haloTextAnchored(dc, label, 22+barPx, y, 0, 0.4, colFooter)
	}

	dc.SetFontFace(m.face(11))
	haloTextAnchored(dc, credit, float64(v.w-14), y, 1, 0.4, colFooter)
}

// ---------- drawing helpers ----------

// eachFeature visits one named layer across every tile, handing the callback a projection
// from that tile's local units onto the image. ctx is checked per tile: without these
// checkpoints the render deadline would be decoration, since a context cannot stop a loop.
func eachFeature(ctx context.Context, tiles []placedTile, v view, name string, fn func(*feature, func(pt) pt)) error {
	for _, t := range tiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		origin := v.tileOrigin(t.x, t.y)
		for i := range t.layers {
			l := &t.layers[i]
			if l.name != name {
				continue
			}
			scale := float64(tilePx) / float64(l.extent)
			project := func(p pt) pt { return pt{origin.x + p.x*scale, origin.y + p.y*scale} }
			for j := range l.features {
				fn(&l.features[j], project)
			}
		}
	}
	return nil
}

// trace walks a feature's parts onto the current path. Each part starts with MoveTo,
// which is also what separates one fill subpath from the next, so a polygon's inner rings
// cut holes in it under the nonzero rule.
func trace(dc *gg.Context, f *feature, project func(pt) pt) {
	for _, ring := range f.rings {
		for i, p := range ring {
			q := project(p)
			if i == 0 {
				dc.MoveTo(q.x, q.y)
			} else {
				dc.LineTo(q.x, q.y)
			}
		}
	}
}

func classIs(want ...string) func(*feature) bool { return propIs("class", want...) }

func propIs(key string, want ...string) func(*feature) bool {
	return func(f *feature) bool {
		got, _ := f.props[key].(string)
		for _, w := range want {
			if got == w {
				return true
			}
		}
		return false
	}
}

func adminLevel(level float64) func(*feature) bool {
	return func(f *feature) bool {
		l, _ := f.props["admin_level"].(float64)
		return l == level
	}
}

func midpoint(ring []pt) pt { return ring[len(ring)/2] }

func haloText(dc *gg.Context, s string, x, y float64, col color.Color) {
	haloTextAnchored(dc, s, x, y, 0.5, 0.5, col)
}

// haloTextAnchored draws text ringed in the background colour so a name stays legible
// wherever it lands — over water, over a road, over a runway.
func haloTextAnchored(dc *gg.Context, s string, x, y, ax, ay float64, col color.Color) {
	dc.SetColor(colHalo)
	for dx := -1.5; dx <= 1.5; dx += 1.5 {
		for dy := -1.5; dy <= 1.5; dy += 1.5 {
			if dx != 0 || dy != 0 {
				dc.DrawStringAnchored(s, x+dx, y+dy, ax, ay)
			}
		}
	}
	dc.SetColor(col)
	dc.DrawStringAnchored(s, x, y, ax, ay)
}

func (m *mapRenderer) face(size float64) font.Face {
	return truetype.NewFace(m.font, &truetype.Options{Size: size})
}

func mustColor(hex string) color.RGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b); err != nil {
		panic("bad colour " + hex)
	}
	return color.RGBA{r, g, b, 0xff}
}
