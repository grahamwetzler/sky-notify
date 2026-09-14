package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- notifier ----------

type ntfyServer struct {
	statuses []int // consumed in order; the last one repeats
	retryHdr string
	bodies   []ntfyMessage
	paths    []string
	methods  []string
	headers  []http.Header
	files    [][]byte // the attachment body, for the PUT path
	hits     int
}

func (n *ntfyServer) start(t *testing.T, cfg *Config) *Notifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m ntfyMessage
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			n.files = append(n.files, body)
		} else {
			json.NewDecoder(r.Body).Decode(&m)
			n.files = append(n.files, nil)
		}
		n.bodies = append(n.bodies, m)
		n.paths = append(n.paths, r.URL.Path)
		n.methods = append(n.methods, r.Method)
		n.headers = append(n.headers, r.Header.Clone())
		st := http.StatusOK
		if n.hits < len(n.statuses) {
			st = n.statuses[n.hits]
		} else if len(n.statuses) > 0 {
			st = n.statuses[len(n.statuses)-1]
		}
		n.hits++
		if n.retryHdr != "" {
			w.Header().Set("Retry-After", n.retryHdr)
		}
		if st >= 300 && st < 400 {
			w.Header().Set("Location", "https://elsewhere.invalid/x")
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(srv.Close)
	cfg.Ntfy.URL = srv.URL
	nt, err := NewNotifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	nt.client = srv.Client()
	nt.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return nt
}

func testAlert() *Alert {
	return &Alert{
		Hex:      "adeb2f",
		Trigger:  "listed",
		Priority: 2,
		Plane:    &Plane{ICAO: "adeb2f", Reg: "N12345", Operator: "US Air Force", Type: "C-17", Link: "ftp://evil.invalid/x"},
		AC:       Aircraft{Hex: "adeb2f", Flight: "RCH123 ", AltBaro: Altitude{Present: true, Feet: 31000}},

		DistanceNM:  12.34,
		HasDistance: true,
	}
}

// A listed aircraft is not a typed one: plane-alert-pia.csv rows carry an ICAO and
// little else, and the type the aircraft broadcast is still worth saying.
func TestNotifyFallsBackToTheBroadcastType(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{}
	n := s.start(t, cfg)
	a := testAlert()
	a.Plane = &Plane{ICAO: "adeb2f", Reg: "N12345"} // a bare row, as PIA rows are
	a.AC.Type = "C17"
	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.bodies[0].Message, "Type: C17") {
		t.Errorf("body should carry the broadcast type: %q", s.bodies[0].Message)
	}
}

func TestNotifySuccess(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{}
	n := s.start(t, cfg)
	if err := n.Publish(context.Background(), testAlert()); err != nil {
		t.Fatal(err)
	}
	if len(s.bodies) != 1 {
		t.Fatalf("want 1 publish, got %d", len(s.bodies))
	}
	// ntfy parses a JSON publish document only at the server root. POSTing it to
	// /<topic> returns 200 and delivers the raw JSON as the message text.
	if s.paths[0] != "/" {
		t.Errorf("publish must POST to the server root, got %q", s.paths[0])
	}
	m := s.bodies[0]
	if m.Topic != cfg.Ntfy.Topic || m.Title != "US Air Force C-17" {
		t.Errorf("unexpected message: %+v", m)
	}
	if m.Priority != 2 {
		t.Errorf("priority = %d, want alert priority 2", m.Priority)
	}
	if !strings.Contains(m.Message, "N12345") || !strings.Contains(m.Message, "Type: C-17") {
		t.Errorf("body missing detail: %q", m.Message)
	}
	// Telemetry the phone cannot act on: the map and the click target say where it is.
	if strings.Contains(m.Message, "Altitude") || strings.Contains(m.Message, "Ground speed") ||
		strings.Contains(m.Message, "Distance") {
		t.Errorf("body should not carry altitude, speed or distance: %q", m.Message)
	}
	// The hex is phone-screen noise: it identifies nothing a human reads, and the
	// click target already carries it to tar1090.
	if strings.Contains(m.Message, "adeb2f") {
		t.Errorf("body should not carry the ICAO hex: %q", m.Message)
	}
	// plane-alert-db links are untrusted: a non-http scheme must be dropped.
	if m.Click != "" {
		t.Errorf("ftp:// click target should have been dropped, got %q", m.Click)
	}
	if n.LastErr() != nil {
		t.Error("a successful publish should clear the error")
	}
}

func TestNotifyRetryClassification(t *testing.T) {
	t.Run("429 then success", func(t *testing.T) {
		cfg := testConfig(t)
		s := &ntfyServer{statuses: []int{http.StatusTooManyRequests, http.StatusOK}, retryHdr: "1"}
		n := s.start(t, cfg)
		if err := n.Publish(context.Background(), testAlert()); err != nil {
			t.Fatalf("429 should be retried: %v", err)
		}
		if s.hits != 2 {
			t.Errorf("want 2 attempts, got %d", s.hits)
		}
	})
	t.Run("403 is not retried", func(t *testing.T) {
		cfg := testConfig(t)
		s := &ntfyServer{statuses: []int{http.StatusForbidden}}
		n := s.start(t, cfg)
		if err := n.Publish(context.Background(), testAlert()); err == nil {
			t.Fatal("403 should fail")
		}
		if s.hits != 1 {
			t.Errorf("403 must not be retried, got %d attempts", s.hits)
		}
		if n.LastErr() == nil {
			t.Error("health must see a non-retryable failure")
		}
	})
	t.Run("exhausted retryable failure is still recorded", func(t *testing.T) {
		cfg := testConfig(t)
		s := &ntfyServer{statuses: []int{http.StatusBadGateway}}
		n := s.start(t, cfg)
		if err := n.Publish(context.Background(), testAlert()); err == nil {
			t.Fatal("sustained 502 should fail")
		}
		// A wedged sink starves alerts exactly as completely as a bad token.
		if n.LastErr() == nil {
			t.Error("an exhausted retryable failure must also degrade health")
		}
	})
}

// Go's default would downgrade a redirected POST to a GET and return 200, writing the
// cooldown for something that was never published.
func TestNotifyDoesNotFollowRedirects(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{statuses: []int{http.StatusFound}}
	n := s.start(t, cfg)
	if err := n.Publish(context.Background(), testAlert()); err == nil {
		t.Fatal("a redirect must be reported as a failure, not silently accepted")
	}
	if s.hits != 1 {
		t.Errorf("want 1 attempt, got %d", s.hits)
	}
}

func TestNotifyEmergencyFraming(t *testing.T) {
	cfg := testConfig(t)
	s := &ntfyServer{}
	n := s.start(t, cfg)
	a := testAlert()
	a.Trigger, a.Emergency, a.Squawk, a.SquawkMeans = "emergency:7700", true, "7700", "general emergency"
	a.Priority = 4

	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	m := s.bodies[0]
	if m.Priority != 4 {
		t.Errorf("emergency priority should come from the alert, got %d", m.Priority)
	}
	if !strings.Contains(strings.Join(m.Tags, ","), "rotating_light") {
		t.Errorf("emergency should carry the alert tag, got %v", m.Tags)
	}
	if !strings.HasPrefix(m.Title, "SQUAWK 7700") {
		t.Errorf("title should lead with the squawk, got %q", m.Title)
	}
}

func TestNamedRuleTitlesTheNotification(t *testing.T) {
	n := &Notifier{cfg: testConfig(t)}
	a := testAlert()
	a.RuleName = "Overhead the house"
	if got := n.render(a).Title; got != "Overhead the house" {
		t.Errorf("title = %q, want the rule name", got)
	}
	a.Emergency, a.Squawk = true, "7700"
	if got := n.render(a).Title; got != "SQUAWK 7700 — Overhead the house" {
		t.Errorf("title = %q, want the squawk ahead of the rule name", got)
	}
}

func TestClickURLPrefersTar1090(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tar1090URL = "https://tar1090.example.com/"
	s := &ntfyServer{}
	n := s.start(t, cfg)
	if err := n.Publish(context.Background(), testAlert()); err != nil {
		t.Fatal(err)
	}
	if got := s.bodies[0].Click; got != "https://tar1090.example.com/?icao=adeb2f" {
		t.Errorf("click url: got %q", got)
	}
	// A click alone is invisible; the button is what the user can actually see and tap.
	if acts := s.bodies[0].Actions; len(acts) != 1 || acts[0].Action != "view" ||
		acts[0].URL != "https://tar1090.example.com/?icao=adeb2f" || acts[0].Label == "" {
		t.Errorf("view action: got %+v", acts)
	}
}

func TestAuthHeaders(t *testing.T) {
	for _, tc := range []struct {
		name              string
		token, user, pass string
		check             func(*testing.T, *http.Request)
	}{
		{"bearer", "tok", "", "", func(t *testing.T, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("want bearer, got %q", r.Header.Get("Authorization"))
			}
		}},
		{"basic", "", "u", "p", func(t *testing.T, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != "u" || p != "p" {
				t.Errorf("want basic auth u/p, got %q/%q ok=%v", u, p, ok)
			}
		}},
		{"none", "", "", "", func(t *testing.T, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				t.Error("no auth expected")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Ntfy.Token, cfg.Ntfy.User, cfg.Ntfy.Password = tc.token, tc.user, tc.pass
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.check(t, r)
			}))
			defer srv.Close()
			cfg.Ntfy.URL = srv.URL
			n, err := NewNotifier(cfg)
			if err != nil {
				t.Fatal(err)
			}
			n.client = srv.Client()
			if err := n.Publish(context.Background(), testAlert()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The distance an alert held for the closest pass carries is the pass itself, so the
// message has to say so — an on_sight alert's distance is just where the aircraft was
// when it was noticed, and the two must not read alike.
func TestClosestPassIsNamedInTheMessage(t *testing.T) {
	n := &Notifier{cfg: testConfig(t)}
	a := testAlert()
	if strings.Contains(n.render(a).Message, "Closest pass") {
		t.Error("an on_sight alert must not claim a closest pass")
	}
	a.AtClosest = true
	if got := n.render(a).Message; !strings.Contains(got, "Closest pass: 12.3 NM") {
		t.Errorf("message should report the pass, got:\n%s", got)
	}
}
