package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxAircraftJSONBytes = 32 << 20

// Altitude decodes readsb's alt_baro, which is a number or the string "ground".
// Present distinguishes "missing" from 0 ft — without it Go's zero value makes every
// altitude-less aircraft look like it is on the ground.
type Altitude struct {
	Feet    int
	Present bool
	Ground  bool
}

func (a *Altitude) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s == "ground" {
			a.Present, a.Ground, a.Feet = true, true, 0
		}
		return nil // any other string: treat as absent rather than failing the document
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return nil
	}
	a.Feet, a.Present = int(f), true
	return nil
}

type Aircraft struct {
	Hex       string   `json:"hex"`
	Flight    string   `json:"flight"`
	Reg       string   `json:"r"`
	Type      string   `json:"t"`
	AltBaro   Altitude `json:"alt_baro"`
	GS        float64  `json:"gs"`
	Track     *float64 `json:"track"` // nil when absent: 0 is due north
	Lat       *float64 `json:"lat"`
	Lon       *float64 `json:"lon"`
	Seen      float64  `json:"seen"`
	SeenPos   float64  `json:"seen_pos"`
	Squawk    string   `json:"squawk"`
	Emergency string   `json:"emergency"`
	Category  string   `json:"category"`
	DBFlags   int      `json:"dbFlags"`
	RSSI      float64  `json:"rssi"`
}

type feed struct {
	Now      float64    `json:"now"`
	Messages int64      `json:"messages"`
	Aircraft []Aircraft `json:"aircraft"`
}

type Source struct {
	cfg    *Config
	client *http.Client
	isHTTP bool
}

func NewSource(cfg *Config, client *http.Client) *Source {
	u, err := url.Parse(cfg.Source.URL)
	return &Source{cfg: cfg, client: client, isHTTP: err == nil && (u.Scheme == "http" || u.Scheme == "https")}
}

// Fetch reads one aircraft.json and validates its freshness. A feeder that dies while
// still serving a valid-but-frozen file is the silent failure this service exists to
// avoid, so a stale document is an error, not a successful empty poll.
func (s *Source) Fetch(ctx context.Context, now time.Time) (*feed, error) {
	var body []byte
	var err error
	if s.isHTTP {
		body, err = s.fetchHTTP(ctx)
	} else {
		body, err = s.readFile()
	}
	if err != nil {
		return nil, err
	}

	// A BOM makes encoding/json fail on byte one ("invalid character 'ï'"); readsb never
	// writes one, but a proxy or an edited file can.
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))

	dec := json.NewDecoder(bytes.NewReader(body))
	var f feed
	if err := dec.Decode(&f); err != nil {
		// The first bytes turn "invalid character" into something diagnosable: gzip, an
		// HTML error page, a truncated write.
		return nil, fmt.Errorf("decode aircraft.json (starts %q): %w", body[:min(len(body), 48)], err)
	}
	// Exactly one complete JSON value: a truncated document must be an error, never a
	// short aircraft list.
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("decode aircraft.json: trailing content after top-level object")
	}

	if f.Now == 0 {
		return nil, fmt.Errorf("aircraft.json has no `now` timestamp")
	}
	age := now.Sub(time.UnixMilli(int64(f.Now * 1000)))
	maxAge := s.cfg.Source.MaxAge.Std()
	// Two-sided: a frozen file dated in the future would otherwise stay healthy forever.
	if age > maxAge || age < -maxAge {
		return nil, fmt.Errorf("aircraft.json is stale: `now` is %s from wall clock (max %s)", age.Round(time.Second), maxAge)
	}
	return &f, nil
}

func (s *Source) fetchHTTP(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.Source.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return readLimited(resp.Body, maxAircraftJSONBytes)
}

func (s *Source) readFile() ([]byte, error) {
	f, err := os.Open(s.cfg.Source.URL)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, maxAircraftJSONBytes)
}

// normalizeHex lowercases and trims. The leading '~' that readsb uses to mark a
// non-ICAO address is deliberately preserved: stripping it would forge a
// legitimate-looking ICAO key and match the wrong airframe.
func normalizeHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func isNonICAO(hex string) bool { return strings.HasPrefix(hex, "~") }

const earthRadiusNM = 3440.065

func haversineNM(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusNM * math.Asin(math.Min(1, math.Sqrt(a)))
}

// closestApproach predicts how near an aircraft that keeps its ground track and speed
// comes to (lat0, lon0) within horizon, and how soon. The position is age old, so the
// window runs from age to age+horizon after it was taken: an aircraft that has gone
// quiet since reporting inbound may already have passed. Clamping to that window means
// an aircraft overhead now counts and one that has already passed does not. A flat-earth
// projection is accurate enough at the few-NM scale this is used for.
// ponytail: straight-line prediction; a turning aircraft mispredicts, fine over minutes.
func closestApproach(lat0, lon0, lat, lon, gsKt, trackDeg float64, age, horizon time.Duration) (float64, time.Duration) {
	rad := math.Pi / 180
	x := wrap180(lon-lon0) * math.Cos(lat0*rad) * 60
	y := (lat - lat0) * 60
	vx, vy := gsKt*math.Sin(trackDeg*rad), gsKt*math.Cos(trackDeg*rad) // NM per hour
	start := age.Hours()
	hrs := start
	if v2 := vx*vx + vy*vy; v2 > 0 {
		hrs = math.Max(start, math.Min(start+horizon.Hours(), -(x*vx+y*vy)/v2))
	}
	return math.Hypot(x+vx*hrs, y+vy*hrs), time.Duration((hrs - start) * float64(time.Hour))
}
