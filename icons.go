package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"

	"github.com/fogleman/gg"
)

// icons.json is a dump of three tables from tar1090's html/markers.js — the marker
// shapes, the ICAO type designators that pick one, and the broadcast categories that
// pick one when the designator is unknown. tar1090 is GPL-2.0-or-later; this file and
// the data it embeds carry that licence, not this project's MIT.
//
// Regenerate by evaluating markers.js up to getBaseMarker and writing out `shapes`,
// `TypeDesignatorIcons` and `CategoryIcons`. Three shapes there are whole <svg>
// documents rather than path data — the ground vehicles — and are substituted for the
// plain square below, since a square already says "not an aircraft".
//
//go:embed icons.json
var iconsJSON []byte

// iconShape is one marker: path data in its own viewBox, drawn at W by H pixels. The
// rest are the knobs tar1090 keeps beside each shape — a stroke scale that holds the
// outline to a constant width whatever the viewBox units are, and flags for the few
// shapes that do not follow the usual rules.
type iconShape struct {
	W, H        float64
	ViewBox     string
	StrokeScale float64
	Path        []string
	Accent      []string
	AccentMult  float64
	NoRotate    bool
	NoAspect    bool
	Transform   string

	vx, vy, vw, vh float64 // ViewBox, parsed once
	sx, sy         float64 // Transform, a scale in every shape that carries one
}

// iconRef is markers.js's ["shape", scaling] pair.
type iconRef struct {
	Shape string
	Scale float64
}

func (r *iconRef) UnmarshalJSON(b []byte) error {
	var v []json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil || len(v) != 2 {
		return fmt.Errorf("icon reference is not a [shape, scale] pair: %s", b)
	}
	if err := json.Unmarshal(v[0], &r.Shape); err != nil {
		return err
	}
	return json.Unmarshal(v[1], &r.Scale)
}

var icons = loadIcons()

type iconTable struct {
	Shapes     map[string]*iconShape `json:"shapes"`
	ByType     map[string]iconRef    `json:"byType"`
	ByCategory map[string]iconRef    `json:"byCategory"`
}

// loadIcons parses the embedded tables at startup. The data ships with the binary, so a
// failure here is a broken build, not a broken feed: there is nothing to degrade to.
func loadIcons() iconTable {
	var t iconTable
	if err := json.Unmarshal(iconsJSON, &t); err != nil {
		panic("icons.json: " + err.Error())
	}
	for name, s := range t.Shapes {
		f := strings.FieldsFunc(s.ViewBox, func(r rune) bool { return r == ' ' || r == ',' })
		if len(f) != 4 {
			panic("icons.json: " + name + ": viewBox is not four numbers")
		}
		for i, dst := range []*float64{&s.vx, &s.vy, &s.vw, &s.vh} {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil || (i >= 2 && v <= 0) {
				panic("icons.json: " + name + ": bad viewBox")
			}
			*dst = v
		}
		s.sx, s.sy = 1, 1
		if t := strings.TrimSuffix(strings.TrimPrefix(s.Transform, "scale("), ")"); t != s.Transform {
			f := strings.Split(t, ",")
			if len(f) != 2 {
				panic("icons.json: " + name + ": unsupported transform " + s.Transform)
			}
			s.sx, _ = strconv.ParseFloat(strings.TrimSpace(f[0]), 64)
			s.sy, _ = strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		} else if s.Transform != "" {
			panic("icons.json: " + name + ": unsupported transform " + s.Transform)
		}
		if s.StrokeScale == 0 {
			s.StrokeScale = 1
		}
		if s.AccentMult == 0 {
			s.AccentMult = 1
		}
	}
	if t.Shapes["unknown"] == nil {
		panic("icons.json: no unknown shape to fall back to")
	}
	return t
}

const (
	// iconScale turns tar1090's marker sizes — 32px for an airliner, 22 for a light
	// single — into pixels on the 800px frame. The sizes are relative to each other on
	// purpose: a 747 has to arrive looking like one.
	iconScale = 2.0
	// iconStroke is tar1090's own outline width, before a shape's strokeScale
	// normalises it for that shape's viewBox units.
	iconStroke = 0.7
)

// iconFor picks the shape tar1090 would draw. The ICAO type designator comes first and
// is nearly always what answers; the broadcast category is the fallback, and says only
// how heavy the thing is and whether it has rotors. tar1090 has a third table keyed on
// the type description (L2J, H) and wake category, which it reads from its own aircraft
// database — readsb's JSON does not carry either, so that rung is missing here and a
// designator the table has never heard of lands on its category instead.
func iconFor(typeCode, category string) (*iconShape, float64) {
	if r, ok := icons.ByType[strings.ToUpper(strings.TrimSpace(typeCode))]; ok {
		if s := icons.Shapes[r.Shape]; s != nil {
			return s, r.Scale * 0.96
		}
	}
	if r, ok := icons.ByCategory[strings.ToUpper(strings.TrimSpace(category))]; ok {
		if s := icons.Shapes[r.Shape]; s != nil {
			return s, r.Scale * 0.96
		}
	}
	return icons.Shapes["unknown"], 0.96
}

// drawIcon paints the aircraft at (x, y) the way tar1090 does: the shape its type earns,
// turned along its track, outlined so it reads over any ground. It reports whether it
// drew anything — a shape whose path will not parse is no reason to leave the alert
// without a marker at all, so the caller keeps its plain fallback.
func drawIcon(dc *gg.Context, x, y, track float64, typeCode, category string, fill, stroke color.Color) bool {
	s, scale := iconFor(typeCode, category)
	return drawShape(dc, s, scale, x, y, track, fill, stroke)
}

// drawShape renders one shape, scaled and turned, centred on (x, y).
func drawShape(dc *gg.Context, s *iconShape, scale, x, y, track float64, fill, stroke color.Color) bool {
	// preserveAspectRatio defaults to "meet": one scale, the smaller of the two, with
	// the viewBox centred in the box. noAspect is SVG's "none" — stretch each axis.
	kx, ky := s.W*scale*iconScale/s.vw, s.H*scale*iconScale/s.vh
	if !s.NoAspect {
		kx = math.Min(kx, ky)
		ky = kx
	}

	dc.Push()
	defer dc.Pop()
	dc.Translate(x, y)
	if !s.NoRotate {
		dc.Rotate(gg.Radians(track)) // 0° is north, and so is -Y on the canvas
	}
	dc.Scale(kx, ky)
	dc.Scale(s.sx, s.sy)
	dc.Translate(-(s.vx + s.vw/2), -(s.vy + s.vh/2))

	// Stroke widths are device pixels: gg transforms the points, not the pen. The shape
	// scale, the shape's own stroke normalisation and any transform all have to be
	// carried across by hand.
	pen := iconStroke * s.StrokeScale * kx * math.Abs(s.sx)

	for _, d := range s.Path {
		dc.ClearPath()
		if err := svgPath(dc, d); err != nil {
			dc.ClearPath()
			return false
		}
		// tar1090 paints the stroke under the fill, so the outline sits wholly outside
		// the silhouette rather than eating half of it. Hence a doubled width, filled over.
		dc.SetColor(stroke)
		dc.SetLineWidth(2 * pen)
		dc.StrokePreserve()
		dc.SetColor(fill)
		dc.Fill()
	}
	// The accents are the panel lines and stripes a few shapes carry: stroke only, no fill.
	for _, d := range s.Accent {
		dc.ClearPath()
		if err := svgPath(dc, d); err != nil {
			dc.ClearPath()
			break // the silhouette is already down and is the part that matters
		}
		dc.SetColor(stroke)
		dc.SetLineWidth(0.6 * s.AccentMult * pen)
		dc.Stroke()
	}
	return true
}

// ---------- SVG path data ----------

// pathScanner walks SVG path data, which packs its tokens: separators are optional
// wherever a sign or a decimal point already ends the previous number.
type pathScanner struct {
	s string
	i int
}

func (p *pathScanner) skip() {
	for p.i < len(p.s) && strings.ContainsRune(" ,\t\r\n", rune(p.s[p.i])) {
		p.i++
	}
}

func (p *pathScanner) letter() (byte, bool) {
	p.skip()
	if p.i < len(p.s) {
		if c := p.s[p.i] | 0x20; c >= 'a' && c <= 'z' {
			c = p.s[p.i]
			p.i++
			return c, true
		}
	}
	return 0, false
}

func (p *pathScanner) num() (float64, bool) {
	p.skip()
	j := p.i
	if j < len(p.s) && (p.s[j] == '+' || p.s[j] == '-') {
		j++
	}
	digits := func() {
		for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
			j++
		}
	}
	digits()
	if j < len(p.s) && p.s[j] == '.' {
		j++
		digits()
	}
	// Only consume an exponent that actually has digits, or "2e" in a stream would eat
	// the letter that starts the next command.
	if j < len(p.s) && (p.s[j] == 'e' || p.s[j] == 'E') {
		k := j + 1
		if k < len(p.s) && (p.s[k] == '+' || p.s[k] == '-') {
			k++
		}
		if k < len(p.s) && p.s[k] >= '0' && p.s[k] <= '9' {
			j = k
			digits()
		}
	}
	v, err := strconv.ParseFloat(p.s[p.i:j], 64)
	if err != nil {
		return 0, false
	}
	p.i = j
	return v, true
}

// flag reads one arc flag, which is a single character and may be run together with the
// number after it: "0 011 1" is two flags and a coordinate, not the number eleven.
func (p *pathScanner) flag() (bool, bool) {
	p.skip()
	if p.i < len(p.s) && (p.s[p.i] == '0' || p.s[p.i] == '1') {
		p.i++
		return p.s[p.i-1] == '1', true
	}
	return false, false
}

func (p *pathScanner) pair() (float64, float64, bool) {
	x, ok := p.num()
	if !ok {
		return 0, 0, false
	}
	y, ok := p.num()
	return x, y, ok
}

// svgPath walks SVG path data into dc. It covers what tar1090's shapes use — moves,
// lines, cubics, elliptical arcs and close, absolute and relative — and refuses anything
// else rather than drawing it wrong.
func svgPath(dc *gg.Context, d string) error {
	p := &pathScanner{s: d}
	var cx, cy, sx, sy float64 // current point, subpath start
	var rx, ry float64         // reflection point for the smooth cubic S
	var prev byte
	for {
		cmd, ok := p.letter()
		if !ok {
			if p.skip(); p.i < len(p.s) {
				return fmt.Errorf("svg path: junk at offset %d: %q", p.i, d[p.i:])
			}
			return nil
		}
		rel := cmd >= 'a' && cmd <= 'z'
		abs := cmd &^ 0x20
		// Each command takes as many argument groups as follow it. A second group after
		// a move is a line, per SVG; everything else simply repeats.
		for n := 0; ; n++ {
			ox, oy := 0.0, 0.0
			if rel {
				ox, oy = cx, cy
			}
			switch abs {
			case 'M':
				x, y, ok := p.pair()
				if !ok {
					if n == 0 {
						return fmt.Errorf("svg path: %c wants a coordinate pair", cmd)
					}
					goto next
				}
				cx, cy = ox+x, oy+y
				if n == 0 {
					sx, sy = cx, cy
					dc.MoveTo(cx, cy)
				} else {
					dc.LineTo(cx, cy)
				}
				rx, ry = cx, cy
			case 'L':
				x, y, ok := p.pair()
				if !ok {
					if n == 0 {
						return fmt.Errorf("svg path: %c wants a coordinate pair", cmd)
					}
					goto next
				}
				cx, cy = ox+x, oy+y
				dc.LineTo(cx, cy)
				rx, ry = cx, cy
			case 'H', 'V':
				v, ok := p.num()
				if !ok {
					if n == 0 {
						return fmt.Errorf("svg path: %c wants a number", cmd)
					}
					goto next
				}
				if abs == 'H' {
					cx = ox + v
				} else {
					cy = oy + v
				}
				dc.LineTo(cx, cy)
				rx, ry = cx, cy
			case 'C', 'S':
				var x1, y1 float64
				if abs == 'C' {
					var ok bool
					if x1, y1, ok = p.pair(); !ok {
						if n == 0 {
							return fmt.Errorf("svg path: C wants three coordinate pairs")
						}
						goto next
					}
					x1, y1 = ox+x1, oy+y1
				} else {
					// The first control point is the reflection of the last one, but
					// only when a cubic came immediately before; otherwise it is here.
					x1, y1 = cx, cy
					if prev == 'C' || prev == 'S' {
						x1, y1 = 2*cx-rx, 2*cy-ry
					}
				}
				x2, y2, ok := p.pair()
				if !ok {
					if abs == 'S' && n == 0 {
						return fmt.Errorf("svg path: S wants two coordinate pairs")
					}
					if abs == 'S' {
						goto next
					}
					return fmt.Errorf("svg path: C is missing its second control point")
				}
				x, y, ok := p.pair()
				if !ok {
					return fmt.Errorf("svg path: %c is missing its end point", cmd)
				}
				x2, y2 = ox+x2, oy+y2
				x, y = ox+x, oy+y
				dc.CubicTo(x1, y1, x2, y2, x, y)
				rx, ry = x2, y2
				cx, cy = x, y
			case 'A':
				arx, ary, ok := p.pair()
				if !ok {
					if n == 0 {
						return fmt.Errorf("svg path: A wants radii")
					}
					goto next
				}
				rot, ok := p.num()
				if !ok {
					return fmt.Errorf("svg path: A is missing its rotation")
				}
				large, ok := p.flag()
				if !ok {
					return fmt.Errorf("svg path: A is missing its large-arc flag")
				}
				sweep, ok := p.flag()
				if !ok {
					return fmt.Errorf("svg path: A is missing its sweep flag")
				}
				x, y, ok := p.pair()
				if !ok {
					return fmt.Errorf("svg path: A is missing its end point")
				}
				x, y = ox+x, oy+y
				svgArc(dc, cx, cy, arx, ary, rot, large, sweep, x, y)
				cx, cy = x, y
				rx, ry = cx, cy
			case 'Z':
				dc.ClosePath()
				cx, cy = sx, sy
				rx, ry = cx, cy
				goto next
			default:
				return fmt.Errorf("svg path: unsupported command %q", cmd)
			}
		}
	next:
		prev = abs
	}
}

// svgArc appends one elliptical arc, in the endpoint parameterisation SVG uses and the
// centre parameterisation trigonometry wants. The conversion is F.6.5 of the SVG spec.
//
// ponytail: flattened to line segments rather than converted to cubics — at marker size
// the difference is well under a pixel. Convert if these are ever drawn large.
func svgArc(dc *gg.Context, x0, y0, rx, ry, deg float64, large, sweep bool, x, y float64) {
	rx, ry = math.Abs(rx), math.Abs(ry)
	if rx == 0 || ry == 0 || (x0 == x && y0 == y) {
		dc.LineTo(x, y)
		return
	}
	phi := gg.Radians(deg)
	cosp, sinp := math.Cos(phi), math.Sin(phi)
	dx, dy := (x0-x)/2, (y0-y)/2
	x1 := cosp*dx + sinp*dy
	y1 := -sinp*dx + cosp*dy

	// Radii too small to reach the far end are scaled up until they just do.
	if l := x1*x1/(rx*rx) + y1*y1/(ry*ry); l > 1 {
		rx, ry = rx*math.Sqrt(l), ry*math.Sqrt(l)
	}
	num := rx*rx*ry*ry - rx*rx*y1*y1 - ry*ry*x1*x1
	den := rx*rx*y1*y1 + ry*ry*x1*x1
	c := math.Sqrt(math.Max(0, num/den))
	if large == sweep {
		c = -c
	}
	cx1, cy1 := c*rx*y1/ry, -c*ry*x1/rx
	cx := cosp*cx1 - sinp*cy1 + (x0+x)/2
	cy := sinp*cx1 + cosp*cy1 + (y0+y)/2

	theta := math.Atan2((y1-cy1)/ry, (x1-cx1)/rx)
	end := math.Atan2((-y1-cy1)/ry, (-x1-cx1)/rx)
	sweepAngle := end - theta
	if !sweep && sweepAngle > 0 {
		sweepAngle -= 2 * math.Pi
	} else if sweep && sweepAngle < 0 {
		sweepAngle += 2 * math.Pi
	}

	steps := int(math.Ceil(math.Abs(sweepAngle) / (math.Pi / 8)))
	if steps < 2 {
		steps = 2
	}
	for i := 1; i <= steps; i++ {
		t := theta + sweepAngle*float64(i)/float64(steps)
		ex, ey := rx*math.Cos(t), ry*math.Sin(t)
		dc.LineTo(cx+cosp*ex-sinp*ey, cy+sinp*ex+cosp*ey)
	}
}
