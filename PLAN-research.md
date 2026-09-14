# Per-rule AI research

A rule can ask an LLM who an aircraft belongs to, and the answer rides along in the
notification.

```yaml
# config.yaml — deployment facts, startup-only, never served to the web UI
ai:
  url: https://openrouter.ai/api/v1
  key: sk-or-v1-...
  model: perplexity/sonar
  timeout: 20s
```

```yaml
# alerts.yaml — per rule, hot-reloaded
rules:
  - name: Police helicopters
    cmpg: [Pol]
    max_distance_nm: 25
    research: true
    # research_prompt: optional; the default is below
```

The notification gains one line:

```
Registration: N661HD
Type: EC45
Circling: yes
Research: Owned by Hillwood Development, operated by the Garland PD air unit; most
  likely a police patrol orbit.
```

---

## Decisions, and why

**Credentials live in `config.yaml`, not `alerts.yaml`.** `GET /api/alerts` serves the
alerts file verbatim to the browser. An API key in it would be a key on a page. Endpoint,
key and model are also statements about a deployment, which is exactly what `config.yaml`
is for — same shelf as `ntfy.url` and `ntfy.token`.

**Only the prompt and the on/off switch are per rule.** That is what the request asks
for, and both are safe to serve and to hot-reload.

**`ai.url` is a base URL, not a full endpoint.** We append `/chat/completions`, the way
`ntfy.url` is a base and the topic is appended. OpenRouter, vLLM, Ollama, LiteLLM,
Together, Groq and the OpenAI API are all reachable this way, and it is what every
provider's docs tell you to paste. Cost: a provider on a non-standard path is out of
reach. Accepted — the whole ecosystem converged on this one.

**Web search is the model's job, not ours.** `model` is an opaque string, so
`perplexity/sonar` or `anthropic/claude-sonnet-4.5:online` on OpenRouter brings its own
search. Nothing in this service needs a search provider, a tool loop, or an SDK.

**No template language in the prompt.** The prompt is an instruction; the facts are a
block we always append. So a per-rule prompt stays plain English with nothing to learn
and nothing to get wrong at startup, and the facts cannot drift out of sync with what
the alert actually knows.

```
Research this aircraft and reply with only the owner, the operator, and its most
likely use, in one concise sentence.

Registration: N661HD
ICAO: a8c3f1
Type: EC45
Callsign: N661HD
Altitude: 1200 ft
Position: 32.9126, -96.6389
Distance from receiver: 3.2 NM
Circling: yes
```

**A failed research call costs the notification its line, never the notification.**
Verbatim the rule the map already follows. Unreachable provider, 401, rate limit,
timeout, malformed JSON: log a warning, send the alert without the line.

**No cross-file validation.** A rule with `research: true` on a server with no `ai`
block cannot be rejected at save time — the UI validates `Alerts`, which has never been
able to see `Config`, and that separation is deliberate. Instead: warn once at startup
naming the rules, expose a plain `"ai": true|false` on `/api/alerts` so the UI can say
so next to the toggle, and skip silently at send time.

---

## What changes

### `research.go` — new, ~90 lines

```go
const defaultResearchPrompt = "Research this aircraft and reply with only the owner, " +
    "the operator, and its most likely use, in one concise sentence."

// researchLimit caps what we will paste into a notification. ntfy renders the body in a
// phone's notification shade; a model that ignores "concise" must not fill it.
const researchLimit = 400

type researcher struct {
    client        *http.Client
    url, key, model string
}

// ask posts one OpenAI-shaped chat completion. Returns the trimmed, capped, single-
// paragraph answer.
func (rs *researcher) ask(ctx context.Context, prompt, facts string) (string, error)

// facts is what the model is told about the aircraft: everything the alert knows that
// helps identify it, and nothing about the rule that caught it.
func (a *Alert) facts() string
```

Request body is `{"model":..., "messages":[{"role":"user","content": prompt+"\n\n"+facts}]}`,
`Authorization: Bearer <key>`. Response is read at `choices[0].message.content`. Body
read through the existing `readLimited`. No new dependency — this is one POST.

### `config.go`

- `Config.AI struct { URL, Key, Model string; Timeout Duration }`, `yaml:"ai"`.
- `defaultConfig`: `Timeout = 20s`. URL, key and model stay empty — unset means off,
  and guessing a provider is the same mistake as guessing an ntfy server.
- Env: `SKY_AI_URL`, `SKY_AI_KEY`, `SKY_AI_MODEL`, `SKY_AI_TIMEOUT` in `envBindings()`.
- `alertsMisplaced` gains `ai.url`, `ai.key`, `ai.model`, `ai.timeout` so putting the
  block in the wrong file says which file it belongs in.

### `match.go`

```go
Research       *bool  `yaml:"research,omitempty" json:"research,omitempty"`
ResearchPrompt string `yaml:"research_prompt,omitempty" json:"research_prompt,omitempty"`
```

**`Key()` must clear both**, beside `Name`, `Priority`, `All` and `Notify`. Neither
changes which aircraft the rule claims, so neither belongs in an unnamed rule's
fingerprint — and if we forget, switching research on resets that rule's cooldowns and
re-alerts every aircraft it had already claimed.

`Alert` gains `Research string`, filled by the notifier before `render`. Nothing in
`matches()` reads it.

### `notify.go`

`Publish` gains one line next to the snapshot, before the delivery budget starts:

```go
img := n.snapshot(ctx, a)
a.Research = n.research(ctx, a)   // before msg is rendered
msg := n.render(a)
```

(`render` moves below the two, since it now consumes the result.)

`research()` mirrors `snapshot()` exactly: nil researcher → `""`; shutdown already
signalled → `""`; own `context.WithTimeout(ctx, cfg.AI.Timeout)`; every failure logged
and swallowed.

`render()` gains `line("Research", a.Research)` after the `Circling` line and before
`Feeder flags`.

**Extract the two fallbacks `render` already does** — `reg := a.AC.Reg` overridden by
`p.Reg`, and the same for type — into `a.reg()` and `a.acType()`, so `facts()` reuses
them instead of restating them. One change, two callers, no third version of the rule.

### `main.go`

Build the researcher when `cfg.AI.URL`, `Key` and `Model` are all set; warn naming the
rules that asked for research when they are not. Pass `cfg` to `server` for the
`/api/alerts` flag.

### `ui.html`

- Notification band gains a **Research** checkbox and, when it is on, a prompt
  `<textarea>` placeholdered with the default. Empty textarea = use the default.
- `payloadRule`: carry `research` and `research_prompt`.
- `question()`: destructure both out alongside `name`, `priority`, `notify`. They do not
  move the preview count, and leaving them in would leave a rule permanently marked as
  having a stale preview after the toggle is touched.
- When `/api/alerts` reports `ai: false`, the toggle renders with
  `help bad`: "No AI provider is configured on this server." — the same shape the
  closest-pass toggle already uses for a missing receiver.

### Docs

`config.example.yaml` (the `ai` block, commented, with the OpenRouter values),
`alerts.example.yaml` (one rule using it), `README.md` (a short section under
**Configuration**, and the new env vars in the table).

---

## Known ceilings

- **`notifyLoop` is serial.** A 20s research call delays every later alert by 20s, an
  emergency squawk included. The map snapshot already has this shape at 8s. Mitigated by
  the fact that only rules that opt in pay it, and the cooldown means one call per
  aircraft per rule per 24h. `ponytail: serial; give Publish its own goroutine per alert
  if a research rule ever starves the queue.`
- **No cache.** Two rules that both research the same aircraft in the same poll each pay
  a call. Rare enough not to buy a cache for.
- **Coordinates, not place names.** The facts block carries lat/lon; there is no
  geocoder here. Models generally name the nearest town from a coordinate, but that is
  the model's guess, not ours.
- **Nothing is verified.** The line in the notification is whatever the model said.

## Tests

`research_test.go`, plus additions to the existing files:

- `httptest` provider: the request carries the bearer token, the model, and the prompt
  followed by the facts; the reply is parsed from `choices[0].message.content`, trimmed
  and capped at `researchLimit`.
- Non-200, malformed JSON, empty `choices`, and a context deadline each return an error
  and `Publish` still succeeds without the line.
- `Rule.Key()` is unchanged by toggling `research` and by editing `research_prompt`
  (`match_test.go`).
- `render()` puts the line in the body, and `headerSafe` survives a model that answered
  with newlines (`notify_test.go`).
- `ai.*` loads from YAML and from env; the block in `alerts.yaml` errors naming
  `config.yaml` (`config_test.go`).

## Order

1. `config.go` + its tests — the block, the env bindings, the misplaced-key entries.
2. `research.go` + `research_test.go` against `httptest`, with `facts()` and the two
   extracted `Alert` accessors.
3. `notify.go` wiring and `main.go` construction; `notify_test.go`.
4. `match.go` rule fields and the `Key()` exclusion; `match_test.go`.
5. `ui.html` and `/api/alerts`.
6. Docs.

Each step is runnable on its own: after 3 the feature works from YAML, and 5 only makes
it clickable.
