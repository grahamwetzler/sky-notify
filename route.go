package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultRouteAPIURL is the routeset service tar1090's own config lists first. aircraft.json
// carries no route — a callsign is all the airframe broadcasts — so tar1090 asks a lookup
// service which airports that callsign flies between, and this asks the same one. Set
// route_api_url to "" to ask nobody.
const defaultRouteAPIURL = "https://adsb.im/api/0/routeset"

// routeBudget caps the detour. The route is one extra line of context for the model, so it
// may not spend the research timeout that the answer itself needs.
const routeBudget = 5 * time.Second

const maxRouteJSONBytes = 1 << 20

// routeAnswer is the part of a routeset reply worth reading. _airports is empty — and the
// codes read "unknown" — for every callsign the service has no schedule for, which is most
// of a military or GA feed, so an empty list is the normal answer and not an error.
type routeAnswer struct {
	AirportCodes string `json:"airport_codes"`
	Airports     []struct {
		ICAO     string `json:"icao"`
		IATA     string `json:"iata"`
		Location string `json:"location"`
		Name     string `json:"name"`
	} `json:"_airports"`
}

// lookupRoute returns the callsign's route as "KMCO (Orlando) → KLAS (Las Vegas)", or ""
// when the service has none. The position goes with the request because a callsign alone is
// ambiguous: the service uses it to pick between the flights that share one.
func lookupRoute(ctx context.Context, client *http.Client, apiURL, callsign string, lat, lon float64) (string, error) {
	body, err := json.Marshal(map[string]any{
		"planes": []map[string]any{{"callsign": callsign, "lat": lat, "lng": lon}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("route api returned %s", resp.Status)
	}
	b, err := readLimited(resp.Body, maxRouteJSONBytes)
	if err != nil {
		return "", err
	}
	var out []routeAnswer
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("route api: %w", err)
	}
	if len(out) == 0 {
		return "", nil
	}
	return out[0].describe(), nil
}

// describe names the airports in the order they are flown. It prefers the list, which
// carries the town each code is in — "KLAS" is a fact the model may or may not know, and
// "Las Vegas" is the one that was worth asking for.
func (r routeAnswer) describe() string {
	var legs []string
	for _, a := range r.Airports {
		code := a.ICAO
		if code == "" {
			code = a.IATA
		}
		if place := strings.TrimSpace(a.Location); place != "" {
			code += " (" + place + ")"
		}
		if code != "" {
			legs = append(legs, code)
		}
	}
	if len(legs) > 0 {
		return strings.Join(legs, " → ")
	}
	// No list: the bare codes are still a route, but "unknown" is the service saying it
	// has none, and that is not one.
	if c := strings.TrimSpace(r.AirportCodes); c != "" && c != "unknown" {
		return strings.ReplaceAll(c, "-", " → ")
	}
	return ""
}
