package main

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// A Mapbox Vector Tile is four protobuf messages and two packed integer arrays, which is
// less code than importing a decoder for them: every Go MVT library drags in a protobuf
// runtime, and this repo's only direct dependency is yaml.
//
// Tile bytes arrive from a third party and are decoded on the alert path, so nothing here
// trusts a length: every count is checked against what is left of the buffer before it is
// used, and the two limits below cap what a well-formed but hostile tile can allocate.
const (
	maxTileLayers  = 64
	maxLayerPoints = 200_000
	// The schema default when a layer omits extent. Tile coordinates run [0,extent).
	defaultExtent = 4096
)

var errMalformed = errors.New("mvt: malformed tile")

type geomKind int

const (
	geomPoint   geomKind = 1
	geomLine    geomKind = 2
	geomPolygon geomKind = 3
)

// pt is a point in tile-local units. Features may reach outside [0,extent] — that is the
// tile's buffer, drawn and clipped by the canvas rather than trimmed here.
type pt struct{ x, y float64 }

type feature struct {
	kind  geomKind
	props map[string]any
	// rings holds one slice per part: the points of a multipoint, the points of a line,
	// or one closed ring of a polygon (outer and inner rings both, distinguished by
	// winding, which is why polygons are filled with the nonzero rule).
	rings [][]pt
}

type layer struct {
	name     string
	extent   int
	features []feature
}

// decodeTile reads a whole tile. ctx is checked between layers: a context cannot
// interrupt CPU work by itself, so these checkpoints are what gives the render deadline
// any effect on a large tile.
func decodeTile(ctx context.Context, b []byte) ([]layer, error) {
	var out []layer
	p := &pbuf{b: b}
	for p.more() {
		field, wire, err := p.key()
		if err != nil {
			return nil, err
		}
		if field != 3 || wire != wireBytes { // Tile.layers
			if err := p.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		raw, err := p.bytes()
		if err != nil {
			return nil, err
		}
		if len(out) >= maxTileLayers {
			return nil, fmt.Errorf("%w: over %d layers", errMalformed, maxTileLayers)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l, err := decodeLayer(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// decodeLayer collects the features raw and resolves them at the end: keys and values are
// field 3 and 4 while features are field 2, so a feature's tags name a key table that has
// not been read yet when the feature itself arrives.
func decodeLayer(b []byte) (layer, error) {
	l := layer{extent: defaultExtent}
	var (
		raws   [][]byte
		keys   []string
		values []any
	)
	p := &pbuf{b: b}
	for p.more() {
		field, wire, err := p.key()
		if err != nil {
			return l, err
		}
		switch {
		case field == 1 && wire == wireBytes: // name
			v, err := p.bytes()
			if err != nil {
				return l, err
			}
			l.name = string(v)
		case field == 2 && wire == wireBytes: // features
			v, err := p.bytes()
			if err != nil {
				return l, err
			}
			raws = append(raws, v)
		case field == 3 && wire == wireBytes: // keys
			v, err := p.bytes()
			if err != nil {
				return l, err
			}
			keys = append(keys, string(v))
		case field == 4 && wire == wireBytes: // values
			v, err := p.bytes()
			if err != nil {
				return l, err
			}
			val, err := decodeValue(v)
			if err != nil {
				return l, err
			}
			values = append(values, val)
		case field == 5 && wire == wireVarint: // extent
			v, err := p.varint()
			if err != nil {
				return l, err
			}
			l.extent = int(v)
		default:
			if err := p.skip(wire); err != nil {
				return l, err
			}
		}
	}
	if l.extent <= 0 || l.extent > 1<<16 {
		return l, fmt.Errorf("%w: layer %q extent %d", errMalformed, l.name, l.extent)
	}

	points := 0
	l.features = make([]feature, 0, len(raws))
	for _, raw := range raws {
		f, n, err := decodeFeature(raw, keys, values)
		if err != nil {
			return l, err
		}
		if points += n; points > maxLayerPoints {
			return l, fmt.Errorf("%w: layer %q over %d points", errMalformed, l.name, maxLayerPoints)
		}
		l.features = append(l.features, f)
	}
	return l, nil
}

func decodeFeature(b []byte, keys []string, values []any) (feature, int, error) {
	var (
		f    feature
		tags []uint32
		geom []uint32
	)
	f.kind = geomPoint
	p := &pbuf{b: b}
	for p.more() {
		field, wire, err := p.key()
		if err != nil {
			return f, 0, err
		}
		switch {
		case field == 2: // tags, packed or not
			tags, err = p.uint32s(wire, tags)
		case field == 3 && wire == wireVarint: // type
			var v uint64
			if v, err = p.varint(); err == nil {
				f.kind = geomKind(v)
			}
		case field == 4: // geometry, packed or not
			geom, err = p.uint32s(wire, geom)
		default:
			err = p.skip(wire)
		}
		if err != nil {
			return f, 0, err
		}
	}

	if len(tags)%2 != 0 {
		return f, 0, fmt.Errorf("%w: odd tag count", errMalformed)
	}
	for i := 0; i+1 < len(tags); i += 2 {
		k, v := int(tags[i]), int(tags[i+1])
		if k >= len(keys) || v >= len(values) {
			return f, 0, fmt.Errorf("%w: tag index out of range", errMalformed)
		}
		if f.props == nil {
			f.props = make(map[string]any, len(tags)/2)
		}
		f.props[keys[k]] = values[v]
	}

	rings, n, err := decodeGeometry(geom)
	if err != nil {
		return f, 0, err
	}
	f.rings = rings
	return f, n, nil
}

// decodeGeometry runs the MoveTo/LineTo/ClosePath command stream. A MoveTo starts a new
// part, so a multipoint's points land in one part, a multilinestring's lines in one part
// each, and a polygon's rings likewise — which is all the drawing side needs.
func decodeGeometry(g []uint32) (rings [][]pt, count int, err error) {
	var cur []pt
	var x, y int32
	flush := func() {
		if len(cur) > 0 {
			rings = append(rings, cur)
			cur = nil
		}
	}
	for i := 0; i < len(g); {
		cmd, n := g[i]&7, int(g[i]>>3)
		i++
		switch cmd {
		case 1, 2: // MoveTo, LineTo
			// The guard is the whole point: a count claiming more pairs than the buffer
			// holds is the obvious way to make a decoder read past its slice.
			if n == 0 || n > (len(g)-i)/2 {
				return nil, 0, fmt.Errorf("%w: geometry runs past the buffer", errMalformed)
			}
			if cmd == 1 {
				flush()
			}
			for ; n > 0; n-- {
				x += zigzag(g[i])
				y += zigzag(g[i+1])
				i += 2
				cur = append(cur, pt{float64(x), float64(y)})
				count++
			}
		case 7: // ClosePath
			if n != 1 || len(cur) == 0 {
				return nil, 0, fmt.Errorf("%w: bad ClosePath", errMalformed)
			}
			cur = append(cur, cur[0])
			count++
		default:
			return nil, 0, fmt.Errorf("%w: unknown geometry command %d", errMalformed, cmd)
		}
	}
	flush()
	return rings, count, nil
}

func zigzag(v uint32) int32 { return int32(v>>1) ^ -int32(v&1) }

// decodeValue reads Tile.Value, whose seven fields are seven ways to spell one scalar.
func decodeValue(b []byte) (any, error) {
	var out any
	p := &pbuf{b: b}
	for p.more() {
		field, wire, err := p.key()
		if err != nil {
			return nil, err
		}
		switch {
		case field == 1 && wire == wireBytes:
			v, e := p.bytes()
			out, err = string(v), e
		case field == 2 && wire == wireFixed32:
			v, e := p.fixed32()
			out, err = float64(math.Float32frombits(v)), e
		case field == 3 && wire == wireFixed64:
			v, e := p.fixed64()
			out, err = math.Float64frombits(v), e
		case field == 4 && wire == wireVarint:
			v, e := p.varint()
			out, err = float64(int64(v)), e
		case field == 5 && wire == wireVarint:
			v, e := p.varint()
			out, err = float64(v), e
		case field == 6 && wire == wireVarint:
			v, e := p.varint()
			out, err = float64(int64(v>>1)^-int64(v&1)), e
		case field == 7 && wire == wireVarint:
			v, e := p.varint()
			out, err = v != 0, e
		default:
			err = p.skip(wire)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---------- protobuf reader ----------

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

type pbuf struct {
	b []byte
	i int
}

func (p *pbuf) more() bool { return p.i < len(p.b) }

func (p *pbuf) key() (field, wire int, err error) {
	v, err := p.varint()
	if err != nil {
		return 0, 0, err
	}
	field, wire = int(v>>3), int(v&7)
	if field == 0 {
		return 0, 0, fmt.Errorf("%w: field number 0", errMalformed)
	}
	return field, wire, nil
}

func (p *pbuf) varint() (uint64, error) {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if p.i >= len(p.b) {
			return 0, fmt.Errorf("%w: truncated varint", errMalformed)
		}
		c := p.b[p.i]
		p.i++
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%w: varint too long", errMalformed)
}

func (p *pbuf) bytes() ([]byte, error) {
	n, err := p.varint()
	if err != nil {
		return nil, err
	}
	// Compared against what is left rather than against any absolute cap: an absurd
	// length cannot then be turned into an allocation at all.
	if n > uint64(len(p.b)-p.i) {
		return nil, fmt.Errorf("%w: length %d past end of buffer", errMalformed, n)
	}
	out := p.b[p.i : p.i+int(n)]
	p.i += int(n)
	return out, nil
}

func (p *pbuf) fixed32() (uint32, error) {
	if len(p.b)-p.i < 4 {
		return 0, fmt.Errorf("%w: truncated fixed32", errMalformed)
	}
	v := uint32(p.b[p.i]) | uint32(p.b[p.i+1])<<8 | uint32(p.b[p.i+2])<<16 | uint32(p.b[p.i+3])<<24
	p.i += 4
	return v, nil
}

func (p *pbuf) fixed64() (uint64, error) {
	lo, err := p.fixed32()
	if err != nil {
		return 0, err
	}
	hi, err := p.fixed32()
	if err != nil {
		return 0, err
	}
	return uint64(hi)<<32 | uint64(lo), nil
}

// uint32s appends a repeated uint32 field, which an encoder may write packed or one
// varint per key. Both spellings occur in the wild, so both are read.
func (p *pbuf) uint32s(wire int, dst []uint32) ([]uint32, error) {
	if wire == wireVarint {
		v, err := p.varint()
		if err != nil {
			return dst, err
		}
		return append(dst, uint32(v)), nil
	}
	if wire != wireBytes {
		return dst, fmt.Errorf("%w: wire type %d for a packed field", errMalformed, wire)
	}
	raw, err := p.bytes()
	if err != nil {
		return dst, err
	}
	inner := &pbuf{b: raw}
	for inner.more() {
		v, err := inner.varint()
		if err != nil {
			return dst, err
		}
		dst = append(dst, uint32(v))
	}
	return dst, nil
}

func (p *pbuf) skip(wire int) error {
	switch wire {
	case wireVarint:
		_, err := p.varint()
		return err
	case wireFixed64:
		_, err := p.fixed64()
		return err
	case wireBytes:
		_, err := p.bytes()
		return err
	case wireFixed32:
		_, err := p.fixed32()
		return err
	}
	return fmt.Errorf("%w: unknown wire type %d", errMalformed, wire)
}
