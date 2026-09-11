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
// condition at all, which is what makes an empty rules list silent rather than universal.
func (r *Rule) matches(ac Aircraft, p *Plane, a *Alert) bool {
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
	return r.withinLimits(ac, a)
}

func matchesEither(want []string, a, b string) bool {
	return len(want) == 0 || matches(want, a) || matches(want, b)
}

// withinLimits fails closed: a rule that states a limit suppresses an aircraft whose data
// cannot answer it, rather than admitting it on a zero value. max_distance_nm therefore
// hides Mode-S-only traffic that broadcasts no position — frequently the military traffic
// the rule was written for — so a rule states a limit only when it means it.
func (r *Rule) withinLimits(ac Aircraft, a *Alert) bool {
	if r.MaxDistanceNM != nil && (!a.HasDistance || a.DistanceNM > *r.MaxDistanceNM) {
		return false
	}
	if r.Circling != nil && *r.Circling != a.Circling {
		return false
	}
	if r.MinAltitudeFt != nil || r.MaxAltitudeFt != nil {
		if !ac.AltBaro.Present {
			return false
		}
		if r.MinAltitudeFt != nil && ac.AltBaro.Feet < *r.MinAltitudeFt {
			return false
		}
		if r.MaxAltitudeFt != nil && ac.AltBaro.Feet > *r.MaxAltitudeFt {
			return false
		}
	}
	return r.passesOverhead(ac, a)
}

// passesOverhead is the last check, so the prediction it records on the alert belongs to
// a rule that matched. It fails closed like every limit: a moving aircraft without a
// position or ground track has no prediction. A stationary one is predicted to stay put.
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
	nm, in := closestApproach(a.recvLat, a.recvLon, *ac.Lat, *ac.Lon, ac.GS, heading, horizon)
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
