package main

import (
	"math"
	"time"
)

const earthRadiusNM = 3440.065

func haversineNM(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusNM * math.Asin(math.Min(1, math.Sqrt(a)))
}

// closestApproach predicts how near an aircraft that keeps its ground track and speed
// comes to (lat0, lon0) within horizon, and how soon. The position is age old, so the
// window runs from age to age+horizon after it was taken: an aircraft that has gone
// quiet since reporting inbound may already have passed. Clamping to that window means
// an aircraft overhead now counts and one that has already passed does not. A flat-earth
// projection is accurate enough at the few-NM scale this is used for.
// ponytail: straight-line prediction; a turning aircraft mispredicts, fine over minutes.
func closestApproach(lat0, lon0, lat, lon, gsKt, trackDeg float64, age, horizon time.Duration) (float64, time.Duration) {
	rad := math.Pi / 180
	x := wrap180(lon-lon0) * math.Cos(lat0*rad) * 60
	y := (lat - lat0) * 60
	vx, vy := gsKt*math.Sin(trackDeg*rad), gsKt*math.Cos(trackDeg*rad) // NM per hour
	start := age.Hours()
	hrs := start
	if v2 := vx*vx + vy*vy; v2 > 0 {
		hrs = math.Max(start, math.Min(start+horizon.Hours(), -(x*vx+y*vy)/v2))
	}
	return math.Hypot(x+vx*hrs, y+vy*hrs), time.Duration((hrs - start) * float64(time.Hour))
}

// wrap180 maps an angle difference onto [-180, 180).
func wrap180(deg float64) float64 {
	return math.Mod(math.Mod(deg+180, 360)+360, 360) - 180
}

// receding reports whether an aircraft holding this ground track and speed is moving
// away from (lat0, lon0) — the radial velocity, which is the sign of the dot product of
// the receiver→aircraft vector and the velocity. A stationary aircraft counts as
// receding: it will never get nearer than it is now.
func receding(lat0, lon0, lat, lon, gsKt, trackDeg float64) bool {
	rad := math.Pi / 180
	x := wrap180(lon-lon0) * math.Cos(lat0*rad) * 60
	y := (lat - lat0) * 60
	vx, vy := gsKt*math.Sin(trackDeg*rad), gsKt*math.Cos(trackDeg*rad)
	// Abeam is a dot product of zero, and sin/cos leave that at ~1e-13 of either sign;
	// real closing traffic is thousands of times larger, so the slack costs nothing and
	// keeps the abeam moment on the "passed" side it is documented to be on.
	return x*vx+y*vy >= -1e-6
}
