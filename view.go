package main

import "math"

const (
	// The long edge of the image, baked in. The tile cover, the label density and the
	// marker sizes are all tuned to it; an operator-supplied number would have to
	// re-derive all three to mean anything, and nobody has asked for a different one.
	mapPx     = 800
	tilePx    = 256
	minZoom   = 4
	maxZoom   = 14 // OpenFreeMap's planet tiles stop here
	padding   = 0.10
	minSpanNM = 2.0
	// The footer is one line — scale bar, its distance, and the attribution — and it
	// measures ~381px. A frame narrower than this would wrap the credit off its own
	// left edge, and the credit is an ODbL obligation, so narrow frames widen instead.
	minWidthPx = 400
	// Web Mercator cannot represent the poles; this is where the square world ends.
	maxMercatorLat = 85.05112878
)

type latlon struct{ lat, lon float64 }

// view is the frame: which zoom the tiles come from, how big the image is, and where its
// top-left corner sits in that zoom's world-pixel plane. The image is cropped to what it
// has to show, so it is only square when the thing it frames is.
type view struct {
	zoom             int
	w, h             int
	originX, originY float64
}

// fitView frames every point it is given. Callers pass whatever exists — the aircraft
// always, the receiver only when one is configured — so an unconfigured receiver simply
// frames the aircraft rather than dragging the map towards (0, 0).
func fitView(pts []latlon) view {
	if len(pts) == 0 {
		return view{zoom: minZoom, w: mapPx, h: mapPx}
	}
	// Longitudes are measured relative to the first point, so a pair straddling the
	// antimeridian spans the few degrees between them rather than the 359 the other way.
	ref := pts[0].lon
	minLat, maxLat := math.Inf(1), math.Inf(-1)
	minLon, maxLon := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		lat := math.Max(-maxMercatorLat, math.Min(maxMercatorLat, p.lat))
		lon := ref + wrap180(p.lon-ref)
		minLat, maxLat = math.Min(minLat, lat), math.Max(maxLat, lat)
		minLon, maxLon = math.Min(minLon, lon), math.Max(maxLon, lon)
	}

	// A single point, or a receiver the aircraft is sitting on top of, is a box of zero
	// size: give it a floor before anything divides by it.
	centreLat, centreLon := (minLat+maxLat)/2, (minLon+maxLon)/2
	minLatSpan := minSpanNM / 60
	minLonSpan := minLatSpan / math.Max(math.Cos(centreLat*math.Pi/180), 0.01)
	latSpan := math.Max(maxLat-minLat, minLatSpan) * (1 + padding)
	lonSpan := math.Max(maxLon-minLon, minLonSpan) * (1 + padding)

	minLat, maxLat = centreLat-latSpan/2, centreLat+latSpan/2
	minLon, maxLon = centreLon-lonSpan/2, centreLon+lonSpan/2
	minLat = math.Max(minLat, -maxMercatorLat)
	maxLat = math.Min(maxLat, maxMercatorLat)

	// Largest zoom that still fits. If even minZoom cannot hold it — an aircraft on the
	// far side of the planet from a mistyped receiver — frame at minZoom and let the
	// aircraft fall where it falls; a picture of the wrong place beats no picture.
	zoom := minZoom
	for z := maxZoom; z >= minZoom; z-- {
		w := worldX(maxLon, z) - worldX(minLon, z)
		h := worldY(minLat, z) - worldY(maxLat, z)
		if w <= mapPx && h <= mapPx {
			zoom = z
			break
		}
	}
	// Crop to the box rather than padding it out to a square: mapPx is the long edge, not
	// the picture. A receiver and an aircraft nearly in line would make a letterbox strip,
	// so the short edge stops at half the long one, and mapPx still caps both — when even
	// minZoom could not fit the box, the frame is a crop of it rather than a 4096px mural.
	x0, x1 := worldX(minLon, zoom), worldX(maxLon, zoom)
	y0, y1 := worldY(maxLat, zoom), worldY(minLat, zoom)
	w, h := x1-x0, y1-y0
	long := math.Max(w, h)
	w = math.Min(math.Max(w, math.Max(long/2, minWidthPx)), mapPx)
	h = math.Min(math.Max(h, long/2), mapPx)
	// Centre on the projected box, not on the mean latitude: Mercator stretches towards
	// the poles, so the two are not the same point and a tight crop would drop the
	// northern edge off a frame centred the naive way.
	return view{
		zoom:    zoom,
		w:       int(math.Round(w)),
		h:       int(math.Round(h)),
		originX: (x0+x1)/2 - w/2,
		originY: (y0+y1)/2 - h/2,
	}
}

func worldSize(zoom int) float64 { return float64(tilePx) * math.Exp2(float64(zoom)) }

func worldX(lon float64, zoom int) float64 {
	return (lon + 180) / 360 * worldSize(zoom)
}

func worldY(lat float64, zoom int) float64 {
	lat = math.Max(-maxMercatorLat, math.Min(maxMercatorLat, lat))
	s := math.Sin(lat * math.Pi / 180)
	return (0.5 - math.Log((1+s)/(1-s))/(4*math.Pi)) * worldSize(zoom)
}

// pixel places a coordinate on the image. Longitudes are wrapped towards the frame so a
// view straddling the antimeridian keeps its points together instead of flinging one of
// them a whole world away.
func (v view) pixel(lat, lon float64) pt {
	x := worldX(lon, v.zoom) - v.originX
	world := worldSize(v.zoom)
	for x < -world/2 {
		x += world
	}
	for x > world/2 {
		x -= world
	}
	return pt{x, worldY(lat, v.zoom) - v.originY}
}

// tileRange is the block of tiles the image covers: at most five across, since the 800px
// long edge is 3.125 tiles and the frame need not be tile-aligned.
func (v view) tileRange() (x0, y0, x1, y1 int) {
	x0 = int(math.Floor(v.originX / tilePx))
	y0 = int(math.Floor(v.originY / tilePx))
	x1 = int(math.Floor((v.originX + float64(v.w) - 1) / tilePx))
	y1 = int(math.Floor((v.originY + float64(v.h) - 1) / tilePx))
	// Above the north pole or below the south there are no tiles; sideways there always
	// are, because the world repeats.
	n := int(math.Exp2(float64(v.zoom)))
	y0, y1 = max(y0, 0), min(y1, n-1)
	return x0, y0, x1, y1
}

// tileOrigin is where a tile's top-left corner lands on the image.
func (v view) tileOrigin(x, y int) pt {
	n := worldSize(v.zoom) / tilePx
	wrapped := math.Mod(math.Mod(float64(x), n)+n, n)
	px := wrapped*tilePx - v.originX
	world := worldSize(v.zoom)
	for px > float64(v.w) {
		px -= world
	}
	for px+tilePx < 0 {
		px += world
	}
	return pt{px, float64(y)*tilePx - v.originY}
}
