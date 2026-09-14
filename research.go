package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
		client: client,
		url:    strings.TrimSuffix(cfg.AI.URL, "/") + "/chat/completions",
		key:    cfg.aiKey(),
		model:  cfg.AI.Model,
	}
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
	if a.AC.AltBaro.Present {
		writeLine(&b, "Altitude", fmt.Sprintf("%d ft", a.AC.AltBaro.Feet))
	}
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
