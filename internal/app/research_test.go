package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// A redirect must not carry the bearer token to a host other than the one that was
// configured. Go's default client would still forward Authorization here, since the
// redirect only changes port, and shouldCopyHeaderOnRedirect compares hostnames, not full
// origins — so the request must simply not be followed.
func TestResearchDoesNotFollowRedirects(t *testing.T) {
	var attackerReqs int
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerReqs++
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "leaked"}}},
		})
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/chat/completions", http.StatusFound)
	}))
	defer origin.Close()

	cfg := defaultAlerts()
	key := "sk-test"
	cfg.AI.URL, cfg.AI.Key, cfg.AI.Model = origin.URL, &key, "perplexity/sonar"
	rs := newResearcher(cfg, origin.Client())

	if got, err := rs.ask(context.Background(), "p", "f"); err == nil {
		t.Fatalf("ask followed the redirect and returned %q, want an error", got)
	}
	if attackerReqs != 0 {
		t.Errorf("the redirect target received %d request(s); the key must never reach it", attackerReqs)
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
	// Everything else aircraft.json carries about the airframe.
	rate, track := -1088.0, 298.29
	a.AC.Desc, a.AC.OwnOp, a.AC.Year = "BOEING C-17A", "US AIR FORCE", "1998"
	a.AC.BaroRate, a.AC.Track, a.AC.GS = &rate, &track, 459.9
	a.AC.Squawk, a.AC.DBFlags = "4571", 1
	facts := a.facts()
	for _, want := range []string{
		"Registration: N12345", "ICAO: adeb2f", "Type: C-17", "Callsign: RCH123",
		"Altitude: 31000 ft", "Position: 32.9126, -96.6389", "Distance from receiver: 3.2 NM",
		"Circling: yes",
		"Description: BOEING C-17A", "Owner/operator: US AIR FORCE", "Year: 1998",
		"Vertical rate: -1088 ft/min", "Ground speed: 460 kt", "Heading: 298°",
		"Squawk: 4571", "Feeder flags: military",
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

// An aircraft on the ground reports alt_baro as the string "ground", which decodes to
// 0 ft: the model must not be told a taxiing airframe is at sea level.
func TestFactsSayOnTheGround(t *testing.T) {
	a := testAlert()
	a.AC.AltBaro = Altitude{Present: true, Ground: true}
	if got := a.facts(); !strings.Contains(got, "Altitude: on the ground") {
		t.Errorf("facts should say the aircraft is on the ground:\n%s", got)
	}
}

// The settings pane shows the facts block rather than describing it, which is only true
// while the two agree. A field added to facts() and not to the page is a promise the page
// stopped keeping — this is the drift that check exists to catch.
func TestSampleFactsShowsEveryLine(t *testing.T) {
	a := testAlert()
	lat, lon := 32.9126, -96.6389
	rate, track := -1088.0, 78.0
	a.AC.Lat, a.AC.Lon = &lat, &lon
	a.HasDistance, a.DistanceNM, a.Circling = true, 3.2, true
	a.AC.Desc, a.AC.OwnOp, a.AC.Year = "BOEING C-17A", "UNITED STATES AIR FORCE", "1998"
	a.AC.BaroRate, a.AC.Track, a.AC.GS = &rate, &track, 412
	a.AC.Squawk, a.AC.DBFlags = "4571", 1

	b, err := uiFS.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, line := range strings.Split(a.facts(), "\n") {
		// The page carries the label and its shape, not this alert's values: the check is
		// that every line facts() can write has a place on screen.
		if label, _, _ := strings.Cut(line, ":"); !strings.Contains(page, "'"+label+": ") {
			t.Errorf("SAMPLE_FACTS in ui.html has no %q line; the settings pane no longer shows what is sent", label)
		}
	}
}

// The provider endpoint is a base with the path appended, not a string with the endpoint
// glued on: Azure carries its API version in a query, and concatenation would bury the
// endpoint inside it.
func TestChatCompletionsURL(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/chat/completions"},
		{"https://openrouter.ai/api/v1/", "https://openrouter.ai/api/v1/chat/completions"},
		{"http://ollama.lan:11434/v1", "http://ollama.lan:11434/v1/chat/completions"},
		{
			"https://acme.openai.azure.com/openai/deployments/gpt?api-version=2024-02-01",
			"https://acme.openai.azure.com/openai/deployments/gpt/chat/completions?api-version=2024-02-01",
		},
	} {
		if got := chatCompletionsURL(tc.base); got != tc.want {
			t.Errorf("chatCompletionsURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// And the same fact seen from the provider's side, which is where it matters.
func TestResearchKeepsTheEndpointQuery(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	cfg := defaultAlerts()
	key := "sk-test"
	cfg.AI.URL, cfg.AI.Key, cfg.AI.Model = srv.URL+"/openai/deployments/gpt?api-version=2024-02-01", &key, "gpt"
	rs := newResearcher(cfg, srv.Client())
	if _, err := rs.ask(context.Background(), "p", "f"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/openai/deployments/gpt/chat/completions" {
		t.Errorf("provider was asked for %q", gotPath)
	}
	if gotQuery != "api-version=2024-02-01" {
		t.Errorf("the endpoint query did not survive: %q", gotQuery)
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
		if got := tc.rule.researchPrompt(defaultResearchPrompt); got != tc.want {
			t.Errorf("%s: researchPrompt = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---------- the settings page's default ----------

func TestEffectiveResearchPrompt(t *testing.T) {
	a := defaultAlerts()
	if got := a.effectiveResearchPrompt(); got != defaultResearchPrompt {
		t.Errorf("with no setting: effectiveResearchPrompt = %q, want the built-in default", got)
	}
	a.AI.ResearchPrompt = "  what airline is this?  "
	if got := a.effectiveResearchPrompt(); got != "what airline is this?" {
		t.Errorf("effectiveResearchPrompt = %q, want the trimmed setting", got)
	}
	a.AI.ResearchPrompt = "   "
	if got := a.effectiveResearchPrompt(); got != defaultResearchPrompt {
		t.Errorf("with a blank setting: effectiveResearchPrompt = %q, want the built-in default", got)
	}
}

// A rule that asks the default gets the settings page's prompt when one is configured,
// and a rule with its own prompt still wins over it.
func TestResearchPromptForUsesSettingsDefault(t *testing.T) {
	on := true
	cfg := defaultAlerts()
	cfg.AI.ResearchPrompt = "what airline is this?"
	cfg.Rules = []Rule{
		{Name: "default", Research: &on},
		{Name: "custom", Research: &on, ResearchPrompt: "who flies this?"},
	}
	if got := researchPromptFor(cfg, "default"); got != "what airline is this?" {
		t.Errorf("default rule: researchPromptFor = %q", got)
	}
	if got := researchPromptFor(cfg, "custom"); got != "who flies this?" {
		t.Errorf("custom rule: researchPromptFor = %q", got)
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
	if want := researchMark + ai.answer; body != want {
		t.Fatalf("notification body:\ngot:  %s\nwant: %s", body, want)
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
	if got, want := nt.bodies[0].Message, researchMark+ai.answer; got != want {
		t.Errorf("notification body = %q, want %q", got, want)
	}
}

// The window the rule can close in is wider than notifyLoop: Publish renders a map first,
// and that render has a budget of its own. What to ask is therefore read after it, from
// the same document that names the provider — a rule switched off mid-render costs the
// notification its research line and nothing more.
func TestResearchIsDecidedAfterTheMapRender(t *testing.T) {
	ai := &aiServer{answer: "should never be asked"}
	n, nt, alerts := researchNotifier(t, ai)
	on := true
	alerts.Rules = []Rule{{Name: "listed", Research: &on}}

	// A tile server that answers only once the test says so, which is how the render is
	// held open for the length of the edit.
	rendering, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tiles := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(rendering) })
		<-release
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(tiles.Close)
	maps, err := newMapRenderer(newTileStore(tiles.Client(), tiles.URL, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	n.maps = maps

	a := testAlert() // Trigger "listed", and a receiver distance, so there is a map to draw
	done := make(chan error, 1)
	go func() { done <- n.Publish(context.Background(), a) }()

	<-rendering
	off := *alerts
	off.Rules = []Rule{{Name: "listed"}}
	n.live.p.Store(&off)
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(ai.reqs) != 0 {
		t.Errorf("provider was asked %d times after research was switched off mid-render", len(ai.reqs))
	}
	if len(nt.bodies) != 1 || strings.Contains(nt.bodies[0].Message, researchMark) {
		t.Errorf("the alert should have gone out without the line:\n%v", nt.bodies)
	}
}

// Publish is past the queue's last gate and will deliver what it has prepared — an alert
// already in flight is not recalled. But a rule muted while the picture was drawing has
// said not to alert on this aircraft, and paying a third party to research it anyway is
// the one part of the work that can still be called off.
func TestAMuteMidRenderStopsTheProviderCall(t *testing.T) {
	ai := &aiServer{answer: "should never be asked"}
	n, nt, alerts := researchNotifier(t, ai)
	on := true
	alerts.Rules = []Rule{{Name: "listed", Research: &on}}

	// A tile server that answers only once the test says so, which is how the render is
	// held open for the length of the edit.
	rendering, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tiles := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(rendering) })
		<-release
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(tiles.Close)
	maps, err := newMapRenderer(newTileStore(tiles.Client(), tiles.URL, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	n.maps = maps

	a := testAlert() // Trigger "listed", and a receiver distance, so there is a map to draw
	done := make(chan error, 1)
	go func() { done <- n.Publish(context.Background(), a) }()

	<-rendering
	muted := *alerts
	muted.Rules = []Rule{{Name: "listed", Research: &on, Priority: intp(0)}}
	n.live.p.Store(&muted)
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(ai.reqs) != 0 {
		t.Errorf("provider was asked %d times about an aircraft whose rule was muted", len(ai.reqs))
	}
	// Still delivered: the queue's gate is the last one, and this alert is past it.
	if len(nt.bodies) != 1 || strings.Contains(nt.bodies[0].Message, researchMark) {
		t.Errorf("the in-flight alert should still have gone out, without the line:\n%v", nt.bodies)
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
