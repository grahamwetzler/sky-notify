package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const defaultPassHorizon = 5 * time.Minute

// When a rule announces the aircraft it has claimed. Not a condition: neither value
// changes which aircraft the rule matches.
const (
	notifyOnSight     = "on_sight"
	notifyClosestPass = "closest_pass"
)

// emergencySquawks: hijack, radio failure, general emergency. No longer a trigger — a
// rule with a `squawk` field is — but still the label table, and still what earns an
// alert its queue precedence and its notification framing.
var emergencySquawks = map[string]string{
	"7500": "hijack",
	"7600": "radio failure",
	"7700": "general emergency",
}

// Alert is one thing worth notifying about.
type Alert struct {
	Hex         string
	Trigger     string // the cooldown key of the rule that matched
	RuleName    string // the rule's label, when it has one: the notification's title
	Emergency   bool
	Squawk      string
	SquawkMeans string
	Plane       *Plane // nil when the aircraft is not in the interesting list
	AC          Aircraft
	DistanceNM  float64
	HasDistance bool
	Priority    int
	Circling    bool
	// turns is the raw per-turn-count measurement Circling and matchesFlightPath are both
	// judged from, keyed by how many turns a rule requires. Unexported because it is
	// per-rule (each rule states its own required count), unlike every other field here —
	// Circling stays the one fixed answer ("at least one turn") that notify.go and
	// research.go already read.
	turns turnState
	// Research is the answer the provider gave, filled on the notify path and not here:
	// what to ask is read from the rules as they are at delivery, never as they were at
	// the match, so a rule that stopped asking in between is not asked for.
	Research string
	// Route is where the callsign flies between, looked up on the notify path beside
	// Research and for it: it is context for the model, not a line of the notification.
	Route string
	// AtClosest is set by a rule that notifies at the closest pass. Before delivery it
	// is what tells the poll loop to hold the alert; after it, what puts the distance of
	// the pass in the notification. Both are the same fact: this alert describes a pass,
	// not a first sighting.
	AtClosest bool
	// delivered is set once, by the notifier, on a successful publish. The poll loop
	// holds a parked alert until it sees this: the cooldown ledger cannot answer
	// "delivered" — a short cooldown expires, and prunes its own entry, between polls.
	delivered atomic.Bool
	// The predicted closest approach, set only when the matching rule has passes_within_nm.
	PassNM  float64
	PassIn  time.Duration
	HasPass bool
	// The receiver, valid when HasDistance is.
	recvLat, recvLon float64
	// Path is the aircraft's recent positions, copied out of the Tracker at match time
	// because the renderer runs on another goroutine. It keeps the sample timestamps:
	// the map breaks the drawn line wherever the feed went quiet, and a bare coordinate
	// list could not say where that was. Empty on the preview path, which has no map.
	Path []sample
}

func cooldownKey(hex, trigger string) string { return hex + "|" + trigger }

// reg and acType are the registration and ICAO type to describe this aircraft with: the
// database row when it carries one, the feed otherwise. A listed aircraft is not a typed
// one — plane-alert-pia.csv rows carry an ICAO and little else — so asking only the row
// would describe a listed aircraft with less than an unlisted one.
func (a *Alert) reg() string {
	if a.Plane != nil && a.Plane.Reg != "" {
		return a.Plane.Reg
	}
	return a.AC.Reg
}

func (a *Alert) acType() string {
	if a.Plane != nil && a.Plane.Type != "" {
		return a.Plane.Type
	}
	return a.AC.Type
}

// newAlert builds the context a rule is matched against: who the aircraft is, and where
// it is relative to the receiver. Shared by the alert path and the live rule preview, so
// the preview cannot answer a different question than the save. nil when the aircraft
// has no usable address or has not reported a position.
func newAlert(ac Aircraft, db *DB, cfg *Alerts, turns turnState) *Alert {
	hex := normalizeHex(ac.Hex)
	if hex == "" {
		return nil
	}
	var plane *Plane
	if !isNonICAO(hex) {
		plane, _ = db.Lookup(hex)
	}
	one := turns[1]
	a := &Alert{Hex: hex, Plane: plane, AC: ac, Circling: one.ok && one.turns >= 1, turns: turns}
	if cfg.Lat != nil && cfg.Lon != nil && ac.Lat != nil && ac.Lon != nil {
		a.DistanceNM = haversineNM(*cfg.Lat, *cfg.Lon, *ac.Lat, *ac.Lon)
		a.HasDistance = true
		a.recvLat, a.recvLon = *cfg.Lat, *cfg.Lon
	}
	// Hold the alert back until the aircraft has reported a position — and with it the
	// distance to the receiver, when one is configured. Nothing records a cooldown until
	// an alert is published, so this defers rather than drops: the next poll re-evaluates
	// the same aircraft, which is usually still overhead. An aircraft that never
	// broadcasts a position (Mode S only) therefore never alerts.
	if ac.Lat == nil || ac.Lon == nil {
		slog.Debug("holding alert until a position arrives", "icao", hex)
		return nil
	}
	return a
}

// Evaluate decides whether one aircraft is worth alerting on. Rules are the only reason
// anything alerts: with no rules configured this always returns nil, emergencies included.
// trk is the aircraft's recent history, nil when there is none.
func Evaluate(ac Aircraft, db *DB, cfg *Alerts, trk *track) *Alert {
	a := newAlert(ac, db, cfg, newTurnState(trk))
	if a == nil {
		return nil
	}
	hex := a.Hex
	rule := firstMatch(cfg.Rules, ac, a.Plane, a)
	if rule == nil {
		return nil
	}
	priority := rule.priorityIn(cfg)
	if priority == 0 {
		slog.Debug("rule matched but muted", "rule", rule.Key(), "icao", hex)
		return nil
	}
	a.Trigger, a.RuleName, a.Priority, a.Path = rule.Key(), rule.Name, priority, trk.path()
	a.AtClosest = rule.Notify == notifyClosestPass
	// Derived from the squawk itself, never from which rule fired: the queue's eviction
	// and ordering (main.go) and the notification's framing (notify.go) must treat a 7700
	// as urgent however the operator happened to write the rule that caught it.
	if squawk := strings.TrimSpace(ac.Squawk); emergencySquawks[squawk] != "" {
		a.Emergency, a.Squawk, a.SquawkMeans = true, squawk, emergencySquawks[squawk]
	}
	return a
}

// matchInput is this alert as the rules read it: the facts a condition can ask about, and
// none of the state anything else owns. Rule matching is not read-only — passesOverhead
// records the pass it predicts in the alert it is handed — so re-matching an alert that
// has been published is done on one of these, never on the alert itself.
func (a *Alert) matchInput() *Alert {
	return &Alert{
		Hex: a.Hex, Plane: a.Plane, AC: a.AC,
		DistanceNM: a.DistanceNM, HasDistance: a.HasDistance, Circling: a.Circling,
		turns:   a.turns,
		recvLat: a.recvLat, recvLon: a.recvLon,
	}
}

// firstMatch returns the first rule matching the aircraft, or nil. First match wins, so
// a narrow exception placed ahead of a broad rule shadows it.
func firstMatch(rules []Rule, ac Aircraft, p *Plane, a *Alert) *Rule {
	for i := range rules {
		if rules[i].matches(ac, p, a) {
			return &rules[i]
		}
	}
	return nil
}

// matches ANDs every condition the rule states. A field the rule leaves out is not a
// condition at all, so a rule that states none matches every aircraft — the wildcard is
// what a rule says by saying nothing, not a key of its own. An empty rules *list* is
// still silent: there is no rule to reach.
func (r *Rule) matches(ac Aircraft, p *Plane, a *Alert) bool {
	return r.matchesIdentity(ac, p, a) && r.matchesPosition(ac, a) && r.matchesFlightPath(ac, a)
}

// matchesIdentity is the part of matches that asks who the aircraft is, separated from
// the two that ask where it is and how it is flying. The alerts UI previews a rule
// against the database, where every row is an identity and nothing has an altitude or a
// position; running the other two there would fail them closed and preview every
// distance-limited rule as selecting nothing.
func (r *Rule) matchesIdentity(ac Aircraft, p *Plane, a *Alert) bool {
	if r.Listed != nil && *r.Listed != (p != nil) {
		return false
	}
	// Database fields need no "requires a listed aircraft" guard: with p nil every value
	// below is empty, and matches() already fails a non-empty want against an empty got.
	var db Plane
	if p != nil {
		db = *p
	}
	if !matches(r.ICAO, a.Hex) || !matches(r.Squawk, strings.TrimSpace(ac.Squawk)) ||
		!matchesPrefix(r.Callsign, ac.Flight) {
		return false
	}
	if !matches(r.Operator, db.Operator) || !matches(r.Type, db.Type) ||
		!matches(r.CMPG, db.CMPG) || !matches(r.Category, db.Category) || !matchesAny(r.Tags, db.Tags) {
		return false
	}
	// Registration and ICAO type exist in both the database and the feed, and disagree
	// often enough (the feed carries what the aircraft broadcasts) that either source
	// satisfying the rule has to count.
	if !matchesEither(r.Reg, db.Reg, ac.Reg) || !matchesEither(r.ICAOType, db.ICAOType, ac.Type) {
		return false
	}
	return true
}

// matchesPlane previews a rule against one database row. It applies only the conditions
// a database row can answer: squawk and callsign come from the live feed, and position
// and flight path ask where an aircraft is right now, so none is knowable here and all
// fail closed if asked. The preview therefore answers "which aircraft can this rule
// select", not "which would alert this second" — the UI says so next to the count.
func (r *Rule) matchesPlane(p *Plane) bool {
	identity := *r
	identity.Squawk, identity.Callsign = nil, nil
	ac := Aircraft{Hex: p.ICAO, Reg: p.Reg, Type: p.ICAOType}
	return identity.matchesIdentity(ac, p, &Alert{Hex: p.ICAO})
}

func matchesEither(want []string, a, b string) bool {
	return len(want) == 0 || matches(want, a) || matches(want, b)
}

// matchesPosition asks where the aircraft is: how high, and how far from the receiver.
// It fails closed, as every runtime condition does: a rule that states one suppresses an
// aircraft whose data cannot answer it, rather than admitting it on a zero value. So an
// altitude hides traffic reporting no alt_baro. Distance costs nothing extra: Evaluate
// has already held back anything without a position, and validate requires the receiver
// coordinates, so HasDistance is always true by the time a rule is asked.
func (r *Rule) matchesPosition(ac Aircraft, a *Alert) bool {
	if r.MaxDistanceNM != nil && (!a.HasDistance || a.DistanceNM > *r.MaxDistanceNM) {
		return false
	}
	if r.MinAltitudeFt == nil && r.MaxAltitudeFt == nil {
		return true
	}
	if !ac.AltBaro.Present {
		return false
	}
	if r.MinAltitudeFt != nil && ac.AltBaro.Feet < *r.MinAltitudeFt {
		return false
	}
	return r.MaxAltitudeFt == nil || ac.AltBaro.Feet <= *r.MaxAltitudeFt
}

// matchesFlightPath asks how the aircraft is flying: whether it is orbiting, and whether
// it is predicted to pass close by. Both read the same recent motion, which is why they
// belong together.
func (r *Rule) matchesFlightPath(ac Aircraft, a *Alert) bool {
	if r.Circling != nil {
		n := r.circlingTurns()
		m := a.turns[n]
		is := m.ok && m.turns >= float64(n)
		if *r.Circling != is {
			return false
		}
	}
	return r.passesOverhead(ac, a)
}

// passesOverhead is the last check, so the prediction it records on the alert belongs to
// a rule that matched. It fails closed too: a moving aircraft without a position or
// ground track has no prediction. A stationary one is predicted to stay put.
func (r *Rule) passesOverhead(ac Aircraft, a *Alert) bool {
	if r.PassesWithinNM == nil {
		return true
	}
	if !a.HasDistance || (ac.GS > 0 && ac.Track == nil) {
		return false
	}
	var heading float64
	if ac.Track != nil {
		heading = *ac.Track
	}
	horizon := defaultPassHorizon
	if r.PassesWithin != nil {
		horizon = r.PassesWithin.Std()
	}
	age := time.Duration(ac.SeenPos * float64(time.Second))
	nm, in := closestApproach(a.recvLat, a.recvLon, *ac.Lat, *ac.Lon, ac.GS, heading, age, horizon)
	if nm > *r.PassesWithinNM {
		return false
	}
	a.PassNM, a.PassIn, a.HasPass = nm, in, true
	return true
}

// matchesPrefix is matches for the callsign, the one condition that is not exact: the
// feed carries SWA2504 and the thing worth asking for is SWA, so a value matches what
// it starts. A blank value is skipped rather than honoured — "" is a prefix of every
// string, so one stray empty value would otherwise widen a rule to every aircraft
// overhead. A condition holding nothing else then matches nothing, which is how every
// runtime condition fails.
func matchesPrefix(want []string, got string) bool {
	if len(want) == 0 {
		return true
	}
	got = strings.ToUpper(strings.TrimSpace(got))
	for _, value := range want {
		value = strings.ToUpper(strings.TrimSpace(value))
		if value != "" && strings.HasPrefix(got, value) {
			return true
		}
	}
	return false
}

func matches(want []string, got string) bool {
	if len(want) == 0 {
		return true
	}
	got = strings.TrimSpace(got)
	for _, value := range want {
		if strings.EqualFold(strings.TrimSpace(value), got) {
			return true
		}
	}
	return false
}

func matchesAny(want, got []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, value := range got {
		if matches(want, value) {
			return true
		}
	}
	return false
}

// LiveHit is one aircraft overhead right now that a draft rule matches. Identity comes
// from the database row when there is one and from the feed otherwise, so an unlisted
// aircraft still shows as something recognisable rather than a bare hex.
type LiveHit struct {
	ICAO       string   `json:"icao"`
	Reg        string   `json:"reg,omitempty"`
	Type       string   `json:"type,omitempty"`
	ICAOType   string   `json:"icao_type,omitempty"`
	Operator   string   `json:"operator,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Flight     string   `json:"flight,omitempty"`
	AltitudeFt *int     `json:"altitude_ft,omitempty"`
	DistanceNM *float64 `json:"distance_nm,omitempty"`
}

// MatchLive reports which of the aircraft overhead right now a draft rule matches. It
// runs the whole rule — position and flight path included — against the same alert
// context the poll loop builds, so unlike the database preview this answers "what would
// this alert on if I saved it now" rather than "what could it ever select".
//
// circling is cached by server.snapshot so a request goroutine never reads the Tracker
// directly; it holds every turn count a rule could legally require, so a draft rule's own
// circling_turns is looked up rather than assumed.
//
// ponytail: one rule in isolation, so a match an earlier rule would claim first still
// appears here. Pass the preceding rules too if shadowing needs to show.
func MatchLive(rule Rule, overhead []Aircraft, circling map[string]turnState, db *DB, cfg *Alerts, limit int) (total int, sample []LiveHit) {
	var hits []LiveHit
	for _, ac := range overhead {
		a := newAlert(ac, db, cfg, circling[normalizeHex(ac.Hex)])
		if a == nil || !rule.matches(ac, a.Plane, a) {
			continue
		}
		h := LiveHit{ICAO: a.Hex, Reg: strings.TrimSpace(ac.Reg), ICAOType: strings.TrimSpace(ac.Type),
			Flight: strings.TrimSpace(ac.Flight)}
		if p := a.Plane; p != nil {
			h.Type, h.Operator, h.Tags = p.Type, p.Operator, p.Tags
			if h.Reg == "" {
				h.Reg = p.Reg
			}
			if h.ICAOType == "" {
				h.ICAOType = p.ICAOType
			}
		}
		if ac.AltBaro.Present {
			ft := ac.AltBaro.Feet
			h.AltitudeFt = &ft
		}
		if a.HasDistance {
			nm := a.DistanceNM
			h.DistanceNM = &nm
		}
		hits = append(hits, h)
	}
	// Nearest first: the aircraft a rule is being written for is usually the one closest
	// to the receiver. Without a distance there is nothing to rank by, so those trail in
	// a stable order rather than shuffling with map iteration.
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if (a.DistanceNM == nil) != (b.DistanceNM == nil) {
			return b.DistanceNM == nil
		}
		if a.DistanceNM != nil && *a.DistanceNM != *b.DistanceNM {
			return *a.DistanceNM < *b.DistanceNM
		}
		return a.ICAO < b.ICAO
	})
	total = len(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return total, hits
}

type Rule struct {
	Name     string `yaml:"name,omitempty" json:"name"`
	Priority *int   `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Notify is when the alert goes out: on_sight, the default, on the first poll the
	// rule matches, or closest_pass, held until the aircraft has passed the receiver
	// and then sent with the nearest point it reached.
	Notify   string   `yaml:"notify,omitempty" json:"notify,omitempty"`
	ICAO     []string `yaml:"icao,omitempty" json:"icao,omitempty"`
	Reg      []string `yaml:"reg,omitempty" json:"reg,omitempty"`
	ICAOType []string `yaml:"icao_type,omitempty" json:"icao_type,omitempty"`
	Squawk   []string `yaml:"squawk,omitempty" json:"squawk,omitempty"`
	// Callsign matches on the start of what the aircraft broadcasts, not the whole of
	// it: the feed carries SWA2504, and the thing worth asking for is SWA.
	Callsign []string `yaml:"callsign,omitempty" json:"callsign,omitempty"`
	Operator []string `yaml:"operator,omitempty" json:"operator,omitempty"`
	Type     []string `yaml:"type,omitempty" json:"type,omitempty"`
	CMPG     []string `yaml:"cmpg,omitempty" json:"cmpg,omitempty"`
	Category []string `yaml:"category,omitempty" json:"category,omitempty"`
	Tags     []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	Listed   *bool    `yaml:"listed,omitempty" json:"listed,omitempty"`
	// All is gone: a rule with no conditions already matches every aircraft. It survives
	// only to say so, since DisallowUnknownFields would otherwise report it as a typo
	// with no explanation. Decoded like any other field — validate() is what refuses it,
	// with a message that names what moved, rather than an opaque "unknown field".
	All           *bool    `yaml:"all,omitempty" json:"all,omitempty"`
	MinAltitudeFt *int     `yaml:"min_altitude_ft,omitempty" json:"min_altitude_ft,omitempty"`
	MaxAltitudeFt *int     `yaml:"max_altitude_ft,omitempty" json:"max_altitude_ft,omitempty"`
	MaxDistanceNM *float64 `yaml:"max_distance_nm,omitempty" json:"max_distance_nm,omitempty"`
	Circling      *bool    `yaml:"circling,omitempty" json:"circling,omitempty"`
	// CirclingTurns is how many full rotations circling: true requires, meaningless (and
	// rejected by validate) without it. Nil means 1 — a single lap, today's behavior.
	CirclingTurns  *int      `yaml:"circling_turns,omitempty" json:"circling_turns,omitempty"`
	PassesWithinNM *float64  `yaml:"passes_within_nm,omitempty" json:"passes_within_nm,omitempty"`
	PassesWithin   *Duration `yaml:"passes_within,omitempty" json:"passes_within,omitempty"`
	// Research asks the configured AI provider who the aircraft belongs to and puts the
	// answer in the notification. Not a condition: it describes an aircraft the rule has
	// already claimed. ResearchPrompt replaces the settings page's default prompt when
	// it is set.
	Research       *bool  `yaml:"research,omitempty" json:"research,omitempty"`
	ResearchPrompt string `yaml:"research_prompt,omitempty" json:"research_prompt,omitempty"`
}

// circlingTurns is how many full rotations this rule requires of a circling: true
// condition, 1 when it does not say — today's behavior, unchanged.
func (r *Rule) circlingTurns() int {
	if r.CirclingTurns != nil {
		return *r.CirclingTurns
	}
	return 1
}

// researchPrompt is what to ask about an aircraft this rule claimed, or "" when the rule
// did not ask for research. def is what to ask when the rule sets no question of its
// own — the caller's effective default, so this rule can be answered the same way
// whether that default came from the settings page or, failing that, the constant.
func (r *Rule) researchPrompt(def string) string {
	if r.Research == nil || !*r.Research {
		return ""
	}
	if p := strings.TrimSpace(r.ResearchPrompt); p != "" {
		return p
	}
	return def
}

// Key is what the cooldown ledger and the logs call this rule. A name is optional: a
// named rule titles its own notifications, an unnamed one lets the aircraft do it.
//
// An unnamed rule is keyed by a fingerprint of its own conditions rather than by its
// position, so reordering the list leaves its cooldowns alone — and editing what it
// matches resets them, which is right, since the ledger's entries were recorded for a
// rule that no longer exists. Name, priority, notify and the two research keys are
// excluded: none of them changes which aircraft the rule claims, only how and when it is
// announced — and were research in the fingerprint, switching it on would reset that
// rule's cooldowns and re-alert every aircraft it had already claimed.
func (r *Rule) Key() string {
	if r.Name != "" {
		return r.Name
	}
	conds := *r
	conds.Name, conds.Priority, conds.All, conds.Notify = "", nil, nil, ""
	conds.Research, conds.ResearchPrompt = nil, ""
	// encoding/json marshals struct fields in declaration order just as deterministically
	// as yaml.v3 did, so the property this fingerprint depends on holds identically.
	blob, err := json.Marshal(conds)
	if err != nil {
		// A struct of scalars and string slices cannot fail to marshal, but a key that
		// silently collapsed to one value for every rule would merge their cooldowns.
		panic("rule fingerprint: " + err.Error())
	}
	sum := sha256.Sum256(blob)
	return fmt.Sprintf("#%x", sum[:4])
}

// priorityIn is what this rule sends at: its own, or the default it inherits. Zero mutes.
func (r *Rule) priorityIn(cfg *Alerts) int {
	if r.Priority != nil {
		return *r.Priority
	}
	return cfg.Ntfy.Priority
}

// label names a rule in an error message. An unnamed one is identified the way the log
// and the ledger will identify it, so a validation error points at something findable.
func (r *Rule) label() string {
	if r.Name != "" {
		return strconv.Quote(r.Name)
	}
	return "unnamed, " + r.Key()
}
