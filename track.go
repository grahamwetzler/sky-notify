package main

import (
	"math"
	"time"
)

// ponytail: tuning knobs, not rule keys — promote them once real orbits show the
// defaults are wrong.
const (
	circleWindow      = 10 * time.Minute
	circleMinTurnDeg  = 360.0
	circleMaxRadiusNM = 2.0
	// A heading change across a longer gap has no known direction: 180° could be either way.
	maxSampleGap = 60 * time.Second
)

type sample struct {
	t                 time.Time
	lat, lon, heading float64
}

type track struct{ samples []sample }

// Tracker keeps the last circleWindow of positions per aircraft. pollLoop is its only
// user, so it needs no lock.
// ponytail: memory only; a restart forgets at most circleWindow of history.
type Tracker struct{ tracks map[string]*track }

func NewTracker() *Tracker { return &Tracker{tracks: map[string]*track{}} }

func (tr *Tracker) get(hex string) *track { return tr.tracks[hex] }

// Update records each aircraft's latest position. The sample is timed by when readsb
// last heard a position, not by the poll, and a repeat of it is dropped: readsb keeps
// serving the last position for a while after an aircraft goes quiet.
func (tr *Tracker) Update(f *feed) {
	now := time.UnixMilli(int64(f.Now * 1000))
	for _, ac := range f.Aircraft {
		hex := normalizeHex(ac.Hex)
		if hex == "" || ac.Lat == nil || ac.Lon == nil || ac.Track == nil {
			continue
		}
		t := now.Add(-time.Duration(ac.SeenPos * float64(time.Second)))
		tk := tr.tracks[hex]
		if tk == nil {
			tk = &track{}
			tr.tracks[hex] = tk
		}
		if n := len(tk.samples); n > 0 && !t.After(tk.samples[n-1].t) {
			continue
		}
		tk.samples = append(tk.samples, sample{t, *ac.Lat, *ac.Lon, *ac.Track})
	}
	cutoff := now.Add(-circleWindow)
	for hex, tk := range tr.tracks {
		i := 0
		for i < len(tk.samples) && tk.samples[i].t.Before(cutoff) {
			i++
		}
		tk.samples = tk.samples[i:]
		if len(tk.samples) == 0 {
			delete(tr.tracks, hex)
		}
	}
}

// circling reports whether the aircraft has turned a full circle without leaving a small
// area. The area limit is what separates an orbit from a holding pattern, which turns
// just as far but over several miles. Only samples since the last gap count.
func (tk *track) circling() bool {
	if tk == nil {
		return false
	}
	s := tk.samples
	for i := len(s) - 1; i > 0; i-- {
		if s[i].t.Sub(s[i-1].t) > maxSampleGap {
			s = s[i:]
			break
		}
	}
	if len(s) < 2 {
		return false
	}
	var turn, lat, lon float64
	for i, p := range s {
		if i > 0 {
			turn += wrap180(p.heading - s[i-1].heading)
		}
		lat += p.lat
		lon += p.lon
	}
	if math.Abs(turn) < circleMinTurnDeg {
		return false
	}
	lat, lon = lat/float64(len(s)), lon/float64(len(s))
	for _, p := range s {
		if haversineNM(lat, lon, p.lat, p.lon) > circleMaxRadiusNM {
			return false
		}
	}
	return true
}

// wrap180 maps an angle difference onto [-180, 180).
func wrap180(deg float64) float64 {
	return math.Mod(math.Mod(deg+180, 360)+360, 360) - 180
}
