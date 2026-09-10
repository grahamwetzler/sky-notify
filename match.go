package main

import (
	"log/slog"
	"strconv"
	"strings"
)

// emergencySquawks: hijack, radio failure, general emergency.
var emergencySquawks = map[string]string{
	"7500": "hijack",
	"7600": "radio failure",
	"7700": "general emergency",
}

const triggerDB = "db"

// Alert is one thing worth notifying about.
type Alert struct {
	Hex         string
	Trigger     string // "db" or "emergency:<squawk>"
	Emergency   bool
	Squawk      string
	SquawkMeans string
	Plane       *Plane // nil when the aircraft is not in the interesting list
	AC          Aircraft
	DistanceNM  float64
	HasDistance bool
	Priority    int
}

// Keys are the cooldown keys this alert satisfies. An emergency on a listed aircraft
// advances the db cooldown too, so the routine alert does not follow it.
func (a *Alert) Keys() []string {
	keys := []string{a.Hex + "|" + a.Trigger}
	if a.Emergency && a.Plane != nil {
		keys = append(keys, a.Hex+"|"+triggerDB)
	}
	return keys
}

func cooldownKey(hex, trigger string) string { return hex + "|" + trigger }

// Evaluate decides whether one aircraft is worth alerting on. It returns at most one
// alert: when both triggers fire, the emergency wins and carries the DB metadata.
func Evaluate(ac Aircraft, db *DB, cfg *Alerts) *Alert {
	hex := normalizeHex(ac.Hex)
	if hex == "" {
		return nil
	}

	var plane *Plane
	if !isNonICAO(hex) {
		if p, ok := db.Lookup(hex); ok {
			plane = p
		}
	}

	squawk := strings.TrimSpace(ac.Squawk)
	means, isEmergency := emergencySquawks[squawk]
	isEmergency = isEmergency && cfg.AlertOnEmergencySquawk

	a := &Alert{Hex: hex, Plane: plane, AC: ac}
	if cfg.Filters.Lat != nil && cfg.Filters.Lon != nil && ac.Lat != nil && ac.Lon != nil {
		a.DistanceNM = haversineNM(*cfg.Filters.Lat, *cfg.Filters.Lon, *ac.Lat, *ac.Lon)
		a.HasDistance = true
	}

	switch {
	case isEmergency:
		// Emergencies bypass every filter: a 7700 at any altitude or distance, with or
		// without a position, is always worth knowing about.
		a.Trigger = "emergency:" + squawk
		a.Emergency = true
		a.Squawk = squawk
		a.SquawkMeans = means
		a.Priority = cfg.SquawkPriority[squawk]
		if a.Priority == 0 {
			return nil
		}
		return a
	case plane != nil:
		if !passesFilters(ac, a, cfg) {
			return nil
		}
		a.Trigger = triggerDB
		a.Priority = cfg.Ntfy.Priority
		if i, rule := firstMatch(cfg.Rules, plane); rule != nil {
			name := rule.Name
			if name == "" {
				name = strconv.Itoa(i)
			}
			slog.Debug("rule matched", "rule", name, "icao", hex, "priority", *rule.Priority)
			if *rule.Priority == 0 {
				return nil
			}
			a.Priority = *rule.Priority
		} else if len(cfg.Rules) > 0 {
			slog.Debug("no rule matched", "icao", hex)
			return nil
		}
		return a
	default:
		return nil
	}
}

// firstMatch returns the first rule matching p, or -1 and nil.
func firstMatch(rules []Rule, p *Plane) (int, *Rule) {
	for i := range rules {
		r := &rules[i]
		if matches(r.ICAO, p.ICAO) && matches(r.Reg, p.Reg) &&
			matches(r.Operator, p.Operator) && matches(r.Type, p.Type) &&
			matches(r.ICAOType, p.ICAOType) && matches(r.CMPG, p.CMPG) &&
			matches(r.Category, p.Category) && matchesAny(r.Tags, p.Tags) {
			return i, r
		}
	}
	return -1, nil
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

// passesFilters fails closed: an enabled filter suppresses an aircraft whose data
// cannot answer it, rather than admitting it on a zero value.
func passesFilters(ac Aircraft, a *Alert, cfg *Alerts) bool {
	f := &cfg.Filters
	if f.MaxDistanceNM > 0 {
		if !a.HasDistance {
			return false
		}
		if a.DistanceNM > f.MaxDistanceNM {
			return false
		}
	}
	if f.MinAltitudeFt > 0 || f.MaxAltitudeFt > 0 {
		if !ac.AltBaro.Present {
			return false
		}
		if f.MinAltitudeFt > 0 && ac.AltBaro.Feet < f.MinAltitudeFt {
			return false
		}
		if f.MaxAltitudeFt > 0 && ac.AltBaro.Feet > f.MaxAltitudeFt {
			return false
		}
	}
	return true
}
