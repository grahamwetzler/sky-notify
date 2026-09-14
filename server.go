package main

import (
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type health struct {
	live     *Live
	db       *DB
	state    *State
	notifier *Notifier
	history  *History

	mu            sync.Mutex
	lastFreshPoll time.Time
	lastPollErr   error
	aircraft      int
	// typeOf is the ICAO type code each aircraft was last heard broadcasting. The
	// database knows the type of the aircraft it lists and nothing else; aircraft.json
	// carries one for everything overhead, which is what makes the type picker
	// answerable without the database.
	// ponytail: memory only, one entry per aircraft seen since start — a restart
	// starts the list again, as the tracker does.
	typeOf map[string]string
	// The last poll's aircraft, and which of them the tracker called circling at the
	// time. This is what a rule preview is matched against to say what would alert right
	// now. The tracker belongs to the poll loop and is not safe to read from a request,
	// so the one question a rule asks it is answered while the poll loop still holds it.
	overhead []Aircraft
	circling map[string]bool
}

// setPollOK records a good poll. tr may be nil; when it is not it must already have
// been updated with this feed, or the circling flags describe the poll before it.
func (h *health) setPollOK(f *feed, tr *Tracker) {
	h.mu.Lock()
	h.lastFreshPoll, h.lastPollErr, h.aircraft = time.Now(), nil, len(f.Aircraft)
	if h.typeOf == nil {
		h.typeOf = map[string]string{}
	}
	h.overhead = f.Aircraft
	h.circling = map[string]bool{}
	for _, ac := range f.Aircraft {
		hex, t := normalizeHex(ac.Hex), strings.TrimSpace(ac.Type)
		if hex == "" {
			continue
		}
		if t != "" {
			h.typeOf[hex] = t
		}
		if tr.get(hex).circling() {
			h.circling[hex] = true
		}
	}
	h.mu.Unlock()
}

// coord reads one drafted coordinate. Blank is a cleared receiver, and so is anything
// unparseable: the page sends what is in the box, and a half-typed "-" is not a place.
func coord(v string) *float64 {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

// previewAlerts is the running config with the receiver the page is holding, which is
// not yet the saved one: coordinates are edited in the same session as the rule that
// needs them, and matching a distance rule against the old ones answers for the wrong
// place — or, before they are first saved, for nowhere at all. The environment still
// wins, exactly as it does at save; a coordinate it sets is not editable in the page
// either.
func (h *health) previewAlerts(q url.Values) *Alerts {
	cfg := h.live.Get()
	// Absent is not blank. Only the page sends these, and it sends both or neither, so
	// nothing here means nothing is being drafted; a blank one means the receiver has
	// been cleared on screen, and answering that from the saved pair would show
	// distances from a place this configuration no longer knows.
	if !q.Has("lat") || !q.Has("lon") {
		return cfg
	}
	draft := *cfg
	draft.Lat, draft.Lon = coord(q.Get("lat")), coord(q.Get("lon"))
	if draft.Lat == nil || draft.Lon == nil {
		// Half a pair measures nothing. Both go, or a rule with a distance would be
		// matched against a receiver at a longitude the page never gave.
		draft.Lat, draft.Lon = nil, nil
	}
	env := environMap(os.Environ())
	for _, b := range alertEnvBindings() {
		if _, set := env[b.name]; !set {
			continue
		}
		switch b.key {
		case "lat":
			draft.Lat = cfg.Lat
		case "lon":
			draft.Lon = cfg.Lon
		}
	}
	return &draft
}

// pollAge is how old the snapshot is, and whether it still describes the sky by the same
// staleness rule /healthz uses. A preview drawn from a dead feeder must say so rather
// than read as an empty sky — and the age travels with the answer, so a page holding one
// can watch it go stale without asking again.
func (h *health) pollAge() (age time.Duration, ok, fresh bool) {
	h.mu.Lock()
	last := h.lastFreshPoll
	h.mu.Unlock()
	if last.IsZero() {
		return 0, false, false
	}
	age = time.Since(last)
	return age, true, age <= h.live.Get().Source.PollInterval.Std()*3
}

// snapshot is the last poll's traffic, for matching a draft rule against what is
// overhead right now.
func (h *health) snapshot() ([]Aircraft, map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.overhead, h.circling
}

// feedTypes is every ICAO type code the receiver has heard, and how many aircraft
// broadcast each one. Offered alongside the database's own codes so a type nothing in
// the database carries is still a value you can pick rather than one you must know to
// type.
func (h *health) feedTypes() []FacetValue {
	h.mu.Lock()
	counts := map[string]int{}
	for _, t := range h.typeOf {
		counts[t]++
	}
	h.mu.Unlock()
	out := make([]FacetValue, 0, len(counts))
	for v, n := range counts {
		out = append(out, FacetValue{Value: v, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func (h *health) setPollErr(err error) {
	h.mu.Lock()
	h.lastPollErr = err
	h.mu.Unlock()
}
