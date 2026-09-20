package app

import (
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type server struct {
	live     *Live
	db       *DB
	state    *State
	notifier *Notifier
	history  *History
	store    *SettingsStore

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
	circling map[string]turnState
}

// setPollOK records a good poll. tr may be nil; when it is not it must already have
// been updated with this feed, or the circling flags describe the poll before it.
func (s *server) setPollOK(f *feed, tr *Tracker) {
	s.mu.Lock()
	s.lastFreshPoll, s.lastPollErr, s.aircraft = time.Now(), nil, len(f.Aircraft)
	if s.typeOf == nil {
		s.typeOf = map[string]string{}
	}
	s.overhead = f.Aircraft
	s.circling = map[string]turnState{}
	for _, ac := range f.Aircraft {
		hex, t := normalizeHex(ac.Hex), strings.TrimSpace(ac.Type)
		if hex == "" {
			continue
		}
		if t != "" {
			s.typeOf[hex] = t
		}
		s.circling[hex] = newTurnState(tr.get(hex))
	}
	s.mu.Unlock()
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
func (s *server) previewAlerts(q url.Values) *Alerts {
	cfg := s.live.Get()
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
	if _, set := env["SKY_LAT"]; set {
		draft.Lat = cfg.Lat
	}
	if _, set := env["SKY_LON"]; set {
		draft.Lon = cfg.Lon
	}
	return &draft
}

// pollAge is how old the snapshot is, and whether it still describes the sky by the same
// staleness rule /healthz uses. A preview drawn from a dead feeder must say so rather
// than read as an empty sky — and the age travels with the answer, so a page holding one
// can watch it go stale without asking again.
func (s *server) pollAge() (age time.Duration, ok, fresh bool) {
	s.mu.Lock()
	last := s.lastFreshPoll
	s.mu.Unlock()
	if last.IsZero() {
		return 0, false, false
	}
	age = time.Since(last)
	return age, true, age <= s.live.Get().Source.PollInterval.Std()*3
}

// snapshot is the last poll's traffic, for matching a draft rule against what is
// overhead right now.
func (s *server) snapshot() ([]Aircraft, map[string]turnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overhead, s.circling
}

// feedTypes is every ICAO type code the receiver has heard, and how many aircraft
// broadcast each one. Offered alongside the database's own codes so a type nothing in
// the database carries is still a value you can pick rather than one you must know to
// type.
func (s *server) feedTypes() []FacetValue {
	s.mu.Lock()
	counts := map[string]int{}
	for _, t := range s.typeOf {
		counts[t]++
	}
	s.mu.Unlock()
	out := make([]FacetValue, 0, len(counts))
	for v, n := range counts {
		out = append(out, FacetValue{Value: v, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func (s *server) setPollErr(err error) {
	s.mu.Lock()
	s.lastPollErr = err
	s.mu.Unlock()
}
