package main

import "strings"

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
func Evaluate(ac Aircraft, db *DB, cfg *Config) *Alert {
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
		return a
	case plane != nil:
		if !passesFilters(ac, a, cfg) {
			return nil
		}
		a.Trigger = triggerDB
		return a
	default:
		return nil
	}
}

// passesFilters fails closed: an enabled filter suppresses an aircraft whose data
// cannot answer it, rather than admitting it on a zero value.
func passesFilters(ac Aircraft, a *Alert, cfg *Config) bool {
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
