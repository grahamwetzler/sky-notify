package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- provider ----------

type aiServer struct {
	status  int
	body    string // raw response; overrides answer when set
	answer  string
	delay   time.Duration
	reqs    []map[string]any
	headers []http.Header
	client  *http.Client
}

// start stands up a fake OpenAI-compatible provider and points an alerts document at it,
// returning the researcher that document would build.
func (s *aiServer) start(t *testing.T, cfg *Alerts) *researcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		s.reqs = append(s.reqs, req)
		s.headers = append(s.headers, r.Header.Clone())
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-r.Context().Done():
				return
			}
		}
		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		if s.body != "" {
			w.Write([]byte(s.body))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": s.answer}}},
		})
	}))
	t.Cleanup(srv.Close)
	s.client = srv.Client()
	key := "sk-test"
	cfg.AI.URL, cfg.AI.Key, cfg.AI.Model = srv.URL, &key, "perplexity/sonar"
	rs := newResearcher(cfg, s.client)
	if rs == nil {
		t.Fatal("newResearcher returned nil for a configured provider")
	}
	return rs
}

func TestResearchAsksTheProvider(t *testing.T) {
	cfg := defaultAlerts()
	ai := &aiServer{answer: "Owned by Hillwood, operated by the Garland PD air unit; a police patrol orbit."}
	rs := ai.start(t, cfg)

	got, err := rs.ask(context.Background(), defaultResearchPrompt, "Registration: N661HD\nCircling: yes")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if got != ai.answer {
		t.Errorf("answer = %q, want %q", got, ai.answer)
	}
	// The base URL is a base: the endpoint is appended, exactly as ntfy's topic is.
	if !strings.HasSuffix(rs.url, "/chat/completions") {
		t.Errorf("url = %q, want a /chat/completions suffix", rs.url)
	}
	if auth := ai.headers[0].Get("Authorization"); auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", auth)
	}
	if model, _ := ai.reqs[0]["model"].(string); model != "perplexity/sonar" {
		t.Errorf("model = %q", model)
	}
	msgs, _ := ai.reqs[0]["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", ai.reqs[0]["messages"])
	}
	m, _ := msgs[0].(map[string]any)
	content, _ := m["content"].(string)
	// The prompt is an instruction and the facts follow it: both must arrive, in order.
	if !strings.HasPrefix(content, defaultResearchPrompt) || !strings.Contains(content, "Registration: N661HD") {
		t.Errorf("content = %q", content)
	}
}

func TestResearchNoKeySendsNoAuthorization(t *testing.T) {
	cfg := defaultAlerts()
	ai := &aiServer{answer: "ok"}
	ai.start(t, cfg) // sets the URL and a key
	cfg.AI.Key = nil
	rs := newResearcher(cfg, ai.client)
	if _, err := rs.ask(context.Background(), "p", "f"); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if auth := ai.headers[0].Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q, want none for a keyless provider", auth)
	}
}

func TestResearchFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		ai   aiServer
	}{
		{"non-200", aiServer{status: http.StatusUnauthorized, body: `{"error":"bad key"}`}},
		{"malformed JSON", aiServer{body: "not json at all"}},
		{"no choices", aiServer{body: `{"choices":[]}`}},
		{"empty answer", aiServer{answer: "   \n  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ai := tc.ai
			rs := ai.start(t, defaultAlerts())
			if got, err := rs.ask(context.Background(), "p", "f"); err == nil {
				t.Fatalf("ask returned %q, want an error", got)
			}
		})
	}
}

func TestResearchHonoursTheDeadline(t *testing.T) {
	ai := &aiServer{answer: "too late", delay: time.Second}
	rs := ai.start(t, defaultAlerts())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := rs.ask(ctx, "p", "f"); err == nil {
		t.Fatal("ask outlived its deadline without an error")
	}
}

// condense is what keeps a chatty model from reshaping a notification body, which is a
// list of "Key: value" lines.
func TestCondense(t *testing.T) {
	if got := condense("Line one.\n\n  Line two.\t"); got != "Line one. Line two." {
		t.Errorf("condense = %q", got)
	}
	// The cap, plus the ellipsis — less whatever trailing space the cut left behind.
	long := condense(strings.Repeat("word ", 400))
	if n := len([]rune(long)); n > researchLimit+1 || n < researchLimit {
		t.Errorf("condense length = %d, want about %d", n, researchLimit)
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("a truncated answer should say so: %q", long[len(long)-10:])
	}
}

func TestFactsCarryWhatIdentifiesTheAircraft(t *testing.T) {
	a := testAlert()
	lat, lon := 32.9126, -96.6389
	a.AC.Lat, a.AC.Lon = &lat, &lon
	a.HasDistance, a.DistanceNM, a.Circling = true, 3.2, true
	facts := a.facts()
	for _, want := range []string{
		"Registration: N12345", "ICAO: adeb2f", "Type: C-17", "Callsign: RCH123",
		"Altitude: 31000 ft", "Position: 32.9126, -96.6389", "Distance from receiver: 3.2 NM",
		"Circling: yes",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q:\n%s", want, facts)
		}
	}
	// Nothing about the rule that caught it: the model is asked about an aircraft.
	if strings.Contains(facts, a.Trigger) {
		t.Errorf("facts name the rule:\n%s", facts)
	}
}

// ---------- the rule's side ----------

func TestResearchPrompt(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		rule Rule
		want string
	}{
		{"absent", Rule{}, ""},
		{"off", Rule{Research: &off, ResearchPrompt: "ask anyway"}, ""},
		{"on", Rule{Research: &on}, defaultResearchPrompt},
		{"custom", Rule{Research: &on, ResearchPrompt: "  who flies this?  "}, "who flies this?"},
		{"blank custom", Rule{Research: &on, ResearchPrompt: "   "}, defaultResearchPrompt},
	} {
		if got := tc.rule.researchPrompt(); got != tc.want {
			t.Errorf("%s: researchPrompt = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Neither key changes which aircraft a rule claims, so neither may move its fingerprint:
// if it did, switching research on would reset that rule's cooldowns and re-alert
// everything it had already claimed.
func TestResearchDoesNotMoveTheCooldownKey(t *testing.T) {
	base := Rule{ICAO: []string{"adeb2f"}}
	on := true
	with := base
	with.Research, with.ResearchPrompt = &on, "who flies this?"
	if base.Key() != with.Key() {
		t.Errorf("key moved: %q -> %q", base.Key(), with.Key())
	}
}

// ---------- end to end ----------
// The whole path from a rule to the line in the notification, with nothing stubbed but
// the two servers: the rule says research, Evaluate carries the prompt, the notifier
// asks, and the answer arrives in the body ntfy was sent.
func TestResearchEndToEnd(t *testing.T) {
	ai := &aiServer{answer: "Owned by Hillwood Development, operated by the Garland PD air unit; a police patrol orbit."}
	n, nt, alerts := researchNotifier(t, ai)

	on := true
	alerts.Rules = []Rule{{Name: "Police helicopters", ICAO: []string{"a8c3f1"}, Research: &on}}
	lat, lon := 32.9126, -96.6389
	alerts.Lat, alerts.Lon = &lat, &lon
	if err := alerts.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	aclat, aclon := 32.95, -96.70
	ac := Aircraft{Hex: "a8c3f1", Reg: "N661HD", Type: "EC45", Lat: &aclat, Lon: &aclon,
		AltBaro: Altitude{Present: true, Feet: 1200}}
	a := Evaluate(ac, NewDB(testConfig(t), nil), alerts, nil)
	if a == nil {
		t.Fatal("rule did not match")
	}
	// The prompt is resolved from the rules as they are at delivery, not as they were at
	// the match, and this rule asks the default.
	if got := researchPromptFor(alerts, a.Trigger); got != defaultResearchPrompt {
		t.Fatalf("prompt = %q", got)
	}
	if err := n.Publish(context.Background(), a); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	body := nt.bodies[0].Message
	if !strings.HasPrefix(body, researchMark+ai.answer+"\n") {
		t.Fatalf("notification body:\n%s", body)
	}
}

// ---------- resolved at delivery, not at the match ----------

// drainOne runs notifyLoop over one queued alert and waits for the queue to empty.
func drainOne(t *testing.T, n *Notifier, alerts *Alerts, a *Alert) *State {
	t.Helper()
	state := NewState(t.TempDir())
	q := newQueue(maxPending)
	q.add(a)
	live := NewLive(alerts)
	n.live = live
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifyLoop(ctx, live, n, state, q)
	for i := 0; i < 200 && q.depth() > 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if q.depth() > 0 {
		t.Fatal("the queue never drained")
	}
	return state
}

// A closest-pass alert waits minutes for its aircraft to pass, and alerts.yaml is re-read
// every five seconds. Research switched off in that window must not still reach the
// provider: it is a third party, and a bill.
func TestResearchTurnedOffWhileAnAlertWaits(t *testing.T) {
	ai := &aiServer{answer: "should never be asked"}
	n, nt, alerts := researchNotifier(t, ai)

	// Matched under a rule that asked for research; delivered under one that no longer
	// does. The provider stays configured throughout, so the only thing that can stop
	// the call is the rule itself.
	a := testAlert()
	a.Trigger = "watch"
	alerts.Rules = []Rule{{Name: "watch"}}

	state := drainOne(t, n, alerts, a)
	if len(ai.reqs) != 0 {
		t.Errorf("provider was asked %d times after research was switched off", len(ai.reqs))
	}
	if len(nt.bodies) != 1 || strings.Contains(nt.bodies[0].Message, researchMark) {
		t.Errorf("notification should have gone out without the line:\n%v", nt.bodies)
	}
	if state.Eligible(cooldownKey(a.Hex, a.Trigger), alerts.Cooldown.Std()) {
		t.Error("the alert itself should still have been published")
	}
}

// The other direction: switched on while the alert waited, and the rule as it is now is
// what decides — including a prompt edited in that same window.
func TestResearchTurnedOnWhileAnAlertWaits(t *testing.T) {
	ai := &aiServer{answer: "Operated by the Garland PD air unit."}
	n, nt, alerts := researchNotifier(t, ai)

	a := testAlert()
	a.Trigger = "watch" // matched before the rule asked for anything
	on := true
	alerts.Rules = []Rule{{Name: "watch", Research: &on, ResearchPrompt: "Who flies this?"}}

	drainOne(t, n, alerts, a)
	if len(ai.reqs) != 1 {
		t.Fatalf("provider was asked %d times, want 1", len(ai.reqs))
	}
	msgs, _ := ai.reqs[0]["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	if content, _ := m["content"].(string); !strings.HasPrefix(content, "Who flies this?") {
		t.Errorf("asked with the stale prompt: %q", content)
	}
	if !strings.HasPrefix(nt.bodies[0].Message, researchMark+ai.answer+"\n") {
		t.Errorf("notification body:\n%s", nt.bodies[0].Message)
	}
}

// A rule deleted outright is the same answer as one that stopped asking.
func TestResearchPromptForAMissingRule(t *testing.T) {
	alerts := defaultAlerts()
	alerts.Rules = []Rule{{Name: "other"}}
	if got := researchPromptFor(alerts, "watch"); got != "" {
		t.Errorf("researchPromptFor = %q, want empty for a rule that is gone", got)
	}
}
