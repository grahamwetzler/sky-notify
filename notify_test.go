package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

// The queue is the one place an alert sits out of reach of the poll loop, and alerts.yaml
// is re-read every five seconds. A rule muted or deleted in that window must not still
// get its alert out: priority 0 is how an exception is written, and it cannot mean
// "after one more".
func TestAMutedRuleStopsAnAlertAlreadyQueued(t *testing.T) {
	s := &ntfyServer{}
	n := s.start(t, testConfig(t))
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "live"}, {Name: "muted", Priority: intp(0)}}
	state := NewState(t.TempDir())
	q := newQueue(maxPending)
	q.add(&Alert{Hex: "aaaaaa", Trigger: "live", Priority: 3})
	q.add(&Alert{Hex: "bbbbbb", Trigger: "muted", Priority: 3})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifyLoop(ctx, NewLive(alerts), n, state, q)
	for i := 0; i < 200 && q.depth() > 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}

	// The ledger is the record of what was published, and it is the one the notify loop
	// writes under a lock — so reading it needs no second guess about timing.
	cooldown := alerts.Cooldown.Std()
	if state.Eligible(cooldownKey("aaaaaa", "live"), cooldown) {
		t.Error("the live rule's alert should have been published")
	}
	if !state.Eligible(cooldownKey("bbbbbb", "muted"), cooldown) {
		t.Error("a muted rule's queued alert must not be published")
	}
}

// The same question, asked of a drain rather than of one alert: a publish can spend the
// whole 30s budget retrying while the rules are re-read every five seconds, so the
// config read has to belong to the alert and not to the batch it arrived in.
func TestMutingDuringADrainStopsTheNextAlert(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blocked := false
		first.Do(func() { blocked = true })
		if blocked {
			close(arrived)
			<-release // the operator gets their edit in while this one is in flight
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.Ntfy.URL = srv.URL
	n, err := NewNotifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "first"}, {Name: "second"}}
	live := NewLive(alerts)
	state := NewState(t.TempDir())
	q := newQueue(maxPending)
	// The emergency is taken first, so which alert blocks is not left to map order.
	q.add(&Alert{Hex: "aaaaaa", Trigger: "first", Priority: 5, Emergency: true, Squawk: "7700"})
	q.add(&Alert{Hex: "bbbbbb", Trigger: "second", Priority: 3})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifyLoop(ctx, live, n, state, q)

	<-arrived
	// A reload, exactly as reloadConfig performs it, while the drain is mid-publish.
	next := defaultAlerts()
	next.Rules = []Rule{{Name: "first"}, {Name: "second", Priority: intp(0)}}
	live.p.Store(next)
	close(release)

	for i := 0; i < 200 && q.depth() > 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cooldown := alerts.Cooldown.Std()
	if state.Eligible(cooldownKey("aaaaaa", "first"), cooldown) {
		t.Error("the alert that was already in flight should have been published")
	}
	if !state.Eligible(cooldownKey("bbbbbb", "second"), cooldown) {
		t.Error("a rule muted during the drain must stop the alert still waiting in it")
	}
}

// ---------- research in the notification ----------

// researchNotifier points a notifier at a fake provider and a fake ntfy, and hands back
// the alerts document carrying the provider — which is what the notifier reads at
// publish time, and what a test edits to change the provider under it.
func researchNotifier(t *testing.T, ai *aiServer) (*Notifier, *ntfyServer, *Alerts) {
	t.Helper()
	alerts := defaultAlerts()
	ai.start(t, alerts)
	nt := &ntfyServer{}
	n := nt.start(t, testConfig(t))
	n.live, n.aiClient = NewLive(alerts), ai.client
	return n, nt, alerts
}

// researchRule makes the live document ask for research on the alerts a trigger claims,
// which is the only thing that puts a research line in a notification.
func researchRule(alerts *Alerts, trigger string) {
	on := true
	alerts.Rules = append(alerts.Rules, Rule{Name: trigger, Research: &on})
}

func TestPublishCarriesTheResearchLine(t *testing.T) {
	answer := "Owned by Hillwood, operated by the Garland PD air unit; a police patrol orbit."
	n, nt, alerts := researchNotifier(t, &aiServer{answer: answer})
	a := testAlert()
	researchRule(alerts, a.Trigger)
	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !strings.HasPrefix(nt.bodies[0].Message, researchMark+answer+"\n") {
		t.Errorf("message missing the research line:\n%s", nt.bodies[0].Message)
	}
}

// The rule the map already follows: a line that cannot be written costs the notification
// that line, never the notification.
func TestPublishSurvivesAFailedResearchCall(t *testing.T) {
	n, nt, alerts := researchNotifier(t, &aiServer{status: http.StatusUnauthorized, body: `{"error":"bad key"}`})
	a := testAlert()
	researchRule(alerts, a.Trigger)
	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(nt.bodies) == 0 {
		t.Fatal("nothing was published")
	}
	if strings.Contains(nt.bodies[0].Message, researchMark) {
		t.Errorf("a failed call still wrote a line:\n%s", nt.bodies[0].Message)
	}
}

// A rule that did not ask for research must not cost a request.
func TestPublishWithoutAResearchRuleAsksNothing(t *testing.T) {
	ai := &aiServer{answer: "should never be asked"}
	n, _, _ := researchNotifier(t, ai)
	if err := n.Publish(context.Background(), testAlert()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(ai.reqs) != 0 {
		t.Errorf("provider was asked %d times, want 0", len(ai.reqs))
	}
}

// A model that answered with newlines would otherwise inject headers on the attachment
// path, where the whole body travels in X-Message.
func TestResearchAnswerIsHeaderSafe(t *testing.T) {
	n, nt, alerts := researchNotifier(t, &aiServer{answer: "Owned by X.\nOperated by Y."})
	n.maps = nil
	a := testAlert()
	researchRule(alerts, a.Trigger)
	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msg := nt.bodies[0].Message
	if !strings.HasPrefix(msg, researchMark+"Owned by X. Operated by Y.\n") {
		t.Errorf("answer was not flattened to one line:\n%s", msg)
	}
	if got := headerSafe(strings.ReplaceAll(msg, "\n", `\n`)); strings.ContainsAny(got, "\r\n") {
		t.Errorf("header form still carries a line break: %q", got)
	}
}
