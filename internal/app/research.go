package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

// defaultResearchPrompt is what a rule asks when it says research: true and nothing more.
// It is an instruction, not a template: the facts about the aircraft are appended by
// facts() below, so a rule that wants to ask something else writes plain English and has
// no syntax to get wrong.
const defaultResearchPrompt = "Research this aircraft and reply with only the owner, " +
	"the operator, and its most likely use, in one concise sentence."

// aiKeyPath is the config path of the provider credential. Named because three places
// have to agree on which value is the secret one: the env-lock listing, the GET that
// blanks it, and the PUT that keeps it.
const aiKeyPath = "ai.key"

// The two variables the guard above reasons about by name.
const (
	aiKeyEnv = "SKY_AI_KEY"
	aiURLEnv = "SKY_AI_URL"
)

// aiOrigin is the part of an endpoint that decides who receives the credential sent with
// it: scheme, host and port. The path does not — the key already reaches that host — but
// the scheme does, since https to http changes who else can read it on the way.
func aiOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// aiKeyEndpoint is the endpoint a document's stored credential belongs to: the
// environment's when it names one, since applyAlertEnv overwrites whatever the page sent
// and the payload's own URL never reaches a request.
//
// A variable set to the empty string names no endpoint. It switches research off for the
// run, which says nothing about which host the file's key was issued for — so the file's
// own URL stands, and both the guard below and the save keep judging it.
func aiKeyEndpoint(a *Alerts, env map[string]string) string {
	if v := env[aiURLEnv]; v != "" {
		return v
	}
	return a.AI.URL
}

// aiKeyReach is every endpoint a document's credential can be sent to. Two, because an
// override is not forever: the run posts to aiKeyEndpoint, and the file's own URL is
// where the key goes the moment SKY_AI_URL is taken away again. They are the same
// endpoint unless that variable is set.
func aiKeyReach(a *Alerts, env map[string]string) []string {
	return []string{aiKeyEndpoint(a, env), a.AI.URL}
}

// checkAIKeyStaysPut refuses a save that would send a credential it did not supply to an
// endpoint it did. The page never holds the key, so "did not supply" is its normal state;
// moving the endpoint is therefore the one edit that has to come with one.
//
// Every endpoint the save leaves reachable is judged, not just the one in use, or a page
// could write an attacker's host into ai.url while an override masked it and wait for the
// override to come off. An endpoint the credential already reaches is not a move, which is
// what lets the env endpoint be written into the file it is overriding.
//
// Turning the provider off is not a move: an empty endpoint sends nothing anywhere.
func checkAIKeyStaysPut(onDisk, saved *Alerts, env map[string]string) error {
	was := aiKeyReach(onDisk, env)
	for _, now := range aiKeyReach(saved, env) {
		if now == "" || slices.ContainsFunc(was, func(w string) bool {
			return w != "" && aiOrigin(w) == aiOrigin(now)
		}) {
			continue
		}
		if err := refuseAIKeyMove(onDisk, saved, env, now); err != nil {
			return err
		}
	}
	return nil
}

// refuseAIKeyMove says why a credential cannot follow the endpoint to now, or nil when
// there is no credential at stake and the move costs nothing.
func refuseAIKeyMove(onDisk, saved *Alerts, env map[string]string, now string) error {
	// An environment key is judged before the payload's own, because a supplied key
	// does not become the one that is sent: the overlay replaces it with the variable's
	// value, so any key at all in the payload — "" included — would otherwise buy a move.
	//
	// A variable set to the empty string holds no key, only silence for this run: the
	// file's credential is still there for the next one, so the move is judged against
	// it below rather than waved through.
	if env[aiKeyEnv] != "" {
		return fmt.Errorf("ai.url: %s holds the API key, so the endpoint it is sent to is not editable here — set %s to the endpoint you want, or move both into alerts.yaml", aiKeyEnv, aiURLEnv)
	}
	if saved.AI.Key != nil {
		return nil // the save brought a key for wherever it is pointing
	}
	if onDisk.aiKey() == "" {
		return nil // nothing stored to carry anywhere
	}
	return fmt.Errorf("ai.url: the stored API key was issued for %s and is never sent to this page, so it cannot follow the endpoint to %s — enter the key for the new endpoint, or clear it", aiOrigin(aiKeyEndpoint(onDisk, env)), aiOrigin(now))
}

// researchMark opens the research line, in place of the "Key: value" every other line of
// a notification body is: the model's sentence is the one line worth reading first, and a
// label would spend the front of it saying so.
const researchMark = "\u2728 "

// researchLimit caps what we will paste into a notification. ntfy renders the body in a
// phone's notification shade; a model that ignores "concise" must not fill it.
const researchLimit = 400

// researcher asks an OpenAI-compatible provider about one aircraft. Every such provider
// — OpenRouter, vLLM, Ollama, LiteLLM, Together, Groq, OpenAI — takes the same POST, so
// this is one request rather than an SDK. Web search, when the answer needs it, is the
// model's job: pick a model that does it (perplexity/sonar, or an :online suffix on
// OpenRouter) and nothing here changes.
type researcher struct {
	client          *http.Client
	url, key, model string
}

// newResearcher returns nil when no provider is configured, which is how research stays
// off by default. Built per alert rather than once at startup: the block is hot-reloaded
// with the rest of alerts.yaml, so a model swapped on the settings page takes effect on
// the next notification instead of the next restart.
func newResearcher(cfg *Alerts, client *http.Client) *researcher {
	if !cfg.aiEnabled() {
		return nil
	}
	return &researcher{
		client: noRedirectClient(client),
		url:    chatCompletionsURL(cfg.AI.URL),
		key:    cfg.aiKey(),
		model:  cfg.AI.Model,
	}
}

// noRedirectClient is client with redirects turned off, for the one call that carries the
// provider's bearer token. Go's default policy would follow a redirect and forward
// Authorization along with it as long as the hostname matches — even across a port or a
// scheme, which is exactly the distinction aiOrigin exists to enforce — so a provider that
// redirects (compromised, misconfigured, or malicious) could have the key handed to a
// different endpoint than the one that was configured. The Transport is shared, so
// connection pooling with the rest of the client's traffic is unaffected.
func noRedirectClient(client *http.Client) *http.Client {
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}

// chatCompletionsURL appends the endpoint to the base's path, the way the ntfy topic is
// appended to its base rather than concatenated onto it. A base may carry a query —
// Azure's OpenAI endpoints hold their API version in one — and concatenation would push
// "/chat/completions" inside that query, leaving the path pointing at nothing.
func chatCompletionsURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		// validate.go has already refused anything unparseable; this is only so a
		// malformed value cannot panic the notifier on its way to failing the call.
		return strings.TrimSuffix(base, "/") + "/chat/completions"
	}
	return u.JoinPath("chat/completions").String()
}

// ask posts one chat completion and returns the answer as a single condensed line.
func (rs *researcher) ask(ctx context.Context, prompt, facts string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":    rs.model,
		"messages": []map[string]string{{"role": "user", "content": prompt + "\n\n" + facts}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rs.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if rs.key != "" {
		req.Header.Set("Authorization", "Bearer "+rs.key)
	}
	resp, err := rs.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// Read before the status check so the body is drained either way, but never quoted
	// into an error: a provider's error document can echo the request, key included.
	b, readErr := readLimited(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ai provider returned %s", resp.Status)
	}
	if readErr != nil {
		return "", readErr
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("ai provider: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("ai provider returned no choices")
	}
	answer := condense(out.Choices[0].Message.Content)
	if answer == "" {
		return "", fmt.Errorf("ai provider returned an empty answer")
	}
	return answer, nil
}

// condense flattens an answer to one bounded line. A notification body is a list of
// "Key: value" lines, so a model that replied with paragraphs, a bulleted list or a
// thousand words must not be able to reshape it.
func condense(s string) string {
	s = strings.TrimSpace(strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " "))
	if r := []rune(s); len(r) > researchLimit {
		s = strings.TrimSpace(string(r[:researchLimit])) + "…"
	}
	return s
}

// facts is what the model is told about the aircraft: everything the alert knows that
// helps identify it, and nothing about the rule that caught it.
func (a *Alert) facts() string {
	var b strings.Builder
	writeLine(&b, "Registration", a.reg())
	writeLine(&b, "ICAO", a.Hex)
	writeLine(&b, "Type", a.acType())
	writeLine(&b, "Callsign", strings.TrimSpace(a.AC.Flight))
	writeLine(&b, "Route", a.Route)
	// What the feeder's own database says about the airframe, which is not always what
	// the interesting-aircraft list says: both are worth having in front of the model.
	writeLine(&b, "Description", a.AC.Desc)
	writeLine(&b, "Owner/operator", a.AC.OwnOp)
	writeLine(&b, "Year", a.AC.Year)
	switch {
	case a.AC.AltBaro.Ground:
		writeLine(&b, "Altitude", "on the ground")
	case a.AC.AltBaro.Present:
		writeLine(&b, "Altitude", fmt.Sprintf("%d ft", a.AC.AltBaro.Feet))
	}
	if rate := a.AC.BaroRate; rate != nil || a.AC.GeomRate != nil {
		if rate == nil {
			rate = a.AC.GeomRate
		}
		writeLine(&b, "Vertical rate", fmt.Sprintf("%+.0f ft/min", *rate))
	}
	if a.AC.GS > 0 {
		writeLine(&b, "Ground speed", fmt.Sprintf("%.0f kt", a.AC.GS))
	}
	if a.AC.Track != nil {
		writeLine(&b, "Heading", fmt.Sprintf("%.0f°", *a.AC.Track))
	}
	writeLine(&b, "Squawk", strings.TrimSpace(a.AC.Squawk))
	if a.AC.Lat != nil && a.AC.Lon != nil {
		// ponytail: coordinates, not a place name — there is no geocoder here. Models
		// generally name the nearest town themselves; that is their guess, not ours.
		writeLine(&b, "Position", fmt.Sprintf("%.4f, %.4f", *a.AC.Lat, *a.AC.Lon))
	}
	if a.HasDistance {
		writeLine(&b, "Distance from receiver", fmt.Sprintf("%.1f NM", a.DistanceNM))
	}
	if a.Circling {
		writeLine(&b, "Circling", "yes")
	}
	writeLine(&b, "Feeder flags", describeDBFlags(a.AC.DBFlags))
	return strings.TrimRight(b.String(), "\n")
}

// researchPromptFor is what the rule behind this alert wants asked, as the rules are
// now. "" when that rule no longer asks for research, or is no longer there at all.
func researchPromptFor(cfg *Alerts, trigger string) string {
	for i := range cfg.Rules {
		if r := &cfg.Rules[i]; r.Key() == trigger {
			return r.researchPrompt()
		}
	}
	return ""
}

// researchRules names the rules that asked for research, for the startup warning when
// nothing is configured to answer them.
func researchRules(a *Alerts) []string {
	var names []string
	for i := range a.Rules {
		if a.Rules[i].researchPrompt() != "" {
			names = append(names, a.Rules[i].label())
		}
	}
	return names
}
