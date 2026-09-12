package main

import (
	"log/slog"
	"strings"
	"time"
)

const defaultPassHorizon = 5 * time.Minute

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
	Trigger     string // the name of the rule that matched
	Emergency   bool
	Squawk      string
	SquawkMeans string
	Plane       *Plane // nil when the aircraft is not in the interesting list
	AC          Aircraft
	DistanceNM  float64
	HasDistance bool
	Priority    int
	Circling    bool
	// The predicted closest approach, set only when the matching rule has passes_within_nm.
	PassNM  float64
	PassIn  time.Duration
	HasPass bool
	// The receiver, valid when HasDistance is.
	recvLat, recvLon float64
}

func cooldownKey(hex, trigger string) string { return hex + "|" + trigger }

// Evaluate decides whether one aircraft is worth alerting on. Rules are the only reason
// anything alerts: with no rules configured this always returns nil, emergencies included.
// trk is the aircraft's recent history, nil when there is none.
func Evaluate(ac Aircraft, db *DB, cfg *Alerts, trk *track) *Alert {
	hex := normalizeHex(ac.Hex)
	if hex == "" {
		return nil
	}
	var plane *Plane
	if !isNonICAO(hex) {
		plane, _ = db.Lookup(hex)
	}
	a := &Alert{Hex: hex, Plane: plane, AC: ac, Circling: trk.circling()}
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
	rule := firstMatch(cfg.Rules, ac, plane, a)
	if rule == nil {
		return nil
	}
	priority := cfg.Ntfy.Priority
	if rule.Priority != nil {
		priority = *rule.Priority
	}
	if priority == 0 {
		slog.Debug("rule matched but muted", "rule", rule.Name, "icao", hex)
		return nil
	}
	a.Trigger, a.Priority = rule.Name, priority
	// Derived from the squawk itself, never from which rule fired: the queue's eviction
	// and ordering (main.go) and the notification's framing (notify.go) must treat a 7700
	// as urgent however the operator happened to write the rule that caught it.
	if squawk := strings.TrimSpace(ac.Squawk); emergencySquawks[squawk] != "" {
		a.Emergency, a.Squawk, a.SquawkMeans = true, squawk, emergencySquawks[squawk]
	}
	return a
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
	if !matches(r.ICAO, a.Hex) || !matches(r.Squawk, strings.TrimSpace(ac.Squawk)) {
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
// a database row can answer: squawk comes from the live feed, and position and flight
// path ask where an aircraft is right now, so neither is knowable here and all fail
// closed if asked. The preview therefore answers "which aircraft can this rule select",
// not "which would alert this second" — the UI says so next to the count.
func (r *Rule) matchesPlane(p *Plane) bool {
	identity := *r
	identity.Squawk = nil
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
	if r.Circling != nil && *r.Circling != a.Circling {
		return false
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
