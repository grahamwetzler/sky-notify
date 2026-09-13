package main

import "math"

const (
	// One size, baked in. The tile cover, the label density and the marker sizes are all
	// tuned to it; an operator-supplied number would have to re-derive all three to mean
	// anything, and nobody has asked for a different one.
	mapPx     = 800
	tilePx    = 256
	minZoom   = 4
	maxZoom   = 14 // OpenFreeMap's planet tiles stop here
	padding   = 0.12
	minSpanNM = 2.0
	// Web Mercator cannot represent the poles; this is where the square world ends.
	maxMercatorLat = 85.05112878
)

type latlon struct{ lat, lon float64 }

// view is the frame: which zoom the tiles come from, and where the top-left corner of the
// image sits in that zoom's world-pixel plane.
type view struct {
	zoom             int
	originX, originY float64
}

// fitView frames every point it is given. Callers pass whatever exists — the aircraft
// always, its path when there is one, the receiver only when one is configured — so an
// unconfigured receiver simply frames the aircraft rather than dragging the map towards
// (0, 0).
func fitView(pts []latlon) view {
	if len(pts) == 0 {
		return view{zoom: minZoom}
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
	return view{
		zoom:    zoom,
		originX: worldX(centreLon, zoom) - mapPx/2,
		originY: worldY(centreLat, zoom) - mapPx/2,
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

// tileRange is the block of tiles the image covers: at most five across, since 800px is
// 3.125 tiles and the frame need not be tile-aligned.
func (v view) tileRange() (x0, y0, x1, y1 int) {
	x0 = int(math.Floor(v.originX / tilePx))
	y0 = int(math.Floor(v.originY / tilePx))
	x1 = int(math.Floor((v.originX + mapPx - 1) / tilePx))
	y1 = int(math.Floor((v.originY + mapPx - 1) / tilePx))
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
	for px > mapPx {
		px -= world
	}
	for px+tilePx < 0 {
		px += world
	}
	return pt{px, float64(y)*tilePx - v.originY}
}
