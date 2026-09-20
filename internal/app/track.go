package app

import (
	"math"
	"time"
)

// ponytail: tuning knobs, not rule keys — promote them once real orbits show the
// defaults are wrong.
const (
	circleWindow      = 10 * time.Minute
	circleMaxRadiusNM = 2.0
	// A heading change across a longer gap has no known direction: 180° could be either way.
	maxSampleGap = 60 * time.Second
	// maxCirclingTurns bounds circling_turns (validate.go): windowFor grows the tracker's
	// history by circleWindow per turn, so this is also the ceiling on how much history is
	// kept for one orbiting aircraft — an hour, at the default circleWindow. Past this many
	// laps "how many times has it gone around" stops being the interesting question.
	maxCirclingTurns = 6
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
//
// window is how much history to keep, from windowFor — a fixed circleWindow when nothing
// configured asks for more than one turn, longer when something does, so a slow orbit has
// room to complete the laps a rule requires before its early samples are trimmed away.
func (tr *Tracker) Update(f *feed, window time.Duration) {
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
	cutoff := now.Add(-window)
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

// path copies the samples out for the renderer. Tracker belongs to pollLoop and is
// deliberately unlocked, so the map must be handed a snapshot rather than a live slice.
// nil-safe like circling: an aircraft first heard this poll has no track at all.
func (tk *track) path() []sample {
	if tk == nil {
		return nil
	}
	return append([]sample(nil), tk.samples...)
}

// turns reports how many full rotations the aircraft has completed within the trailing
// window, and whether that measurement means anything at all. The area limit is what
// separates an orbit from a holding pattern, which turns just as far but over several
// miles: ok is false, regardless of how far it turned, the moment the path leaves it. ok
// is also false with fewer than two samples since the last gap — too little to say
// anything. Only the magnitude of the turn is reported: a rule compares it against how
// many turns it requires, and direction never matters, only that it kept turning the same
// way for that long.
//
// window bounds the measurement to the aircraft's own recent history, not whatever the
// tracker happens to retain: retention (windowFor) grows to fit the strictest configured
// rule, and without this bound that longer history would leak into every other rule's
// answer — a stricter rule elsewhere could carry stale, out-of-radius samples into a
// lenient rule's check and fail it. The cutoff is relative to the latest sample, not wall
// clock time, so it works the same whether called live or against a frozen track.
func (tk *track) turns(window time.Duration) (turns float64, ok bool) {
	if tk == nil || len(tk.samples) == 0 {
		return 0, false
	}
	s := tk.samples
	cutoff := s[len(s)-1].t.Add(-window)
	i := 0
	for i < len(s) && s[i].t.Before(cutoff) {
		i++
	}
	s = s[i:]
	for i := len(s) - 1; i > 0; i-- {
		if s[i].t.Sub(s[i-1].t) > maxSampleGap {
			s = s[i:]
			break
		}
	}
	if len(s) < 2 {
		return 0, false
	}
	var turn, lat, lon float64
	for i, p := range s {
		if i > 0 {
			turn += wrap180(p.heading - s[i-1].heading)
		}
		lat += p.lat
		// Relative to the first sample, so an orbit across the antimeridian does not
		// average 179.99 and -179.99 to 0.
		lon += wrap180(p.lon - s[0].lon)
	}
	lat, lon = lat/float64(len(s)), s[0].lon+lon/float64(len(s))
	for _, p := range s {
		if haversineNM(lat, lon, p.lat, p.lon) > circleMaxRadiusNM {
			return 0, false
		}
	}
	return math.Abs(turn) / 360, true
}

// windowFor is how much track history to keep so the slowest circling rule configured
// still has room to complete its turns: circleWindow per turn required, for whichever
// rule asks for the most. 1 when nothing asks for more than the default, so a config with
// no circling rule — or only ordinary ones — keeps today's fixed circleWindow unchanged.
func windowFor(rules []Rule) time.Duration {
	turns := 1
	for _, r := range rules {
		if r.Circling != nil && *r.Circling && r.circlingTurns() > turns {
			turns = r.circlingTurns()
		}
	}
	return circleWindow * time.Duration(turns)
}

// turnMeasurement is track.turns()'s result for one requested turn count.
type turnMeasurement struct {
	turns float64
	ok    bool
}

// turnState is one aircraft's circling measurement, keyed by how many turns a rule might
// require (1..maxCirclingTurns) and each measured over that count's own window. A map
// rather than a fixed-size array so an out-of-range key — a preview request has not gone
// through validate() — reads as the zero value (not circling) instead of panicking.
type turnState map[int]turnMeasurement

// newTurnState measures every turn count a rule can legally require, once per aircraft
// per poll, so both the live alert path and the draft-rule preview can look up whichever
// count the rule in front of them asks for without recomputing from raw samples.
func newTurnState(tk *track) turnState {
	ts := make(turnState, maxCirclingTurns)
	for n := 1; n <= maxCirclingTurns; n++ {
		turns, ok := tk.turns(circleWindow * time.Duration(n))
		ts[n] = turnMeasurement{turns, ok}
	}
	return ts
}
