# Plan: per-aircraft and per-squawk notification priority

_Locked via claudex-loop (Phase 0 recon + Phase 1 interrogation) — by Claude + graham_

## Goal

Let the operator decide, in config, how loud each alert is — and which alerts should not
fire at all. Two independent knobs:

1. **`rules`** — an ordered list matching fields of the plane-alert-db row behind a DB
   alert. The first matching rule sets that alert's ntfy priority, or mutes it entirely.
   This is what makes "mute routine military, but page me for Air Force One" expressible.
2. **`squawk_priority`** — a per-code priority override for the three emergency squawks.

Done looks like: `rules` and `squawk_priority` in `config.yaml`, honoured end to end, with
the current behaviour preserved exactly when neither key is present.

## Approach

### 1. Config schema (`config.go`)

Two new top-level keys:

```yaml
squawk_priority:
  "7700": 5      # general emergency
  "7600": 4      # radio failure
  "7500": 5      # hijack

rules:
  - name: Air Force One       # optional; used only in debug logs
    tags: [Air Force One]
    priority: 5
  - name: Bombers
    tags: [Strategic Bomber]
    priority: 4
  - name: Mute routine military
    cmpg: [Mil]
    priority: 0               # 0 = do not notify
```

Types:

```go
type Rule struct {
    Name     string   `yaml:"name"`
    ICAO     []string `yaml:"icao"`
    Reg      []string `yaml:"reg"`
    Operator []string `yaml:"operator"`
    Type     []string `yaml:"type"`
    ICAOType []string `yaml:"icao_type"`
    CMPG     []string `yaml:"cmpg"`
    Category []string `yaml:"category"`
    Tags     []string `yaml:"tags"`
    Priority *int     `yaml:"priority"`   // pointer: absent must be an error, not a silent 0-mute
}
```

On `Config`: `Rules []Rule \`yaml:"rules"\`` and
`SquawkPriority map[string]int \`yaml:"squawk_priority"\``.

Defaults (in the same place as `c.Ntfy.Priority = 3`): `SquawkPriority` defaults to
`{"7500": 5, "7600": 5, "7700": 5}` — today's hardcoded behaviour. A user-supplied map
is merged over the defaults key by key, so setting only `"7600": 4` leaves 7500 and 7700
at 5.

Validation (fail at startup, in the existing validate path, with the existing error style):
- a `squawk_priority` key that is not `7500`/`7600`/`7700` → error naming the bad key
  (this is a priority override, not a way to add new triggers)
- any priority value, in `squawk_priority` or a rule, outside `0..5` → error
- a rule with `priority` absent → error (`rule %d (%q): priority is required`)
- a rule with no match fields at all → error; it would match every aircraft, which is
  never what someone meant to write
- rules and `squawk_priority` are YAML-only. Do **not** add env-var setters for them;
  they are structured and env cannot express them cleanly.

### 2. Matching (`match.go`)

Add `Priority int` to `Alert`.

```go
// firstMatch returns the first rule matching p, or nil.
func firstMatch(rules []Rule, p *Plane) *Rule
```

Semantics — get these exactly right, they are the spec:
- **Within a field**: OR. `cmpg: [Mil, Gov]` matches either.
- **Across fields**: AND. A rule with `cmpg: [Mil]` and `icao_type: [B52]` matches only
  military B-52s.
- **An empty/absent field is not a constraint** and is skipped.
- **Comparison is case-insensitive and EXACT** on the trimmed value. Never substring.
  This is load-bearing: in the live DB, a substring match for `"B-2"` hits 81 unrelated
  aircraft (Tupolev Tu-154 **B-2**, BN Islander 2**B-2**6, DHC-1 Chipmunk 1**B-2**-S5).
- **`tags`**: the rule matches if the plane carries *any* listed tag (case-insensitive,
  exact per tag).
- **`icao`**: compare against the normalized lowercase hex.

In `Evaluate`, after the existing emergency/db switch determines the trigger:
- **emergency alerts**: `a.Priority = cfg.SquawkPriority[squawk]`. If that value is `0`,
  return `nil` — the operator turned that specific code off. Rules never apply to and
  never mute an emergency: a muted aircraft squawking 7700 still alerts. This preserves
  the existing "emergencies bypass everything" contract.
- **db alerts**: run `firstMatch`. No match → `a.Priority = cfg.Ntfy.Priority`. Match with
  `priority == 0` → return `nil` (muted before the cooldown ledger sees it, so a muted
  aircraft never consumes a cooldown slot). Otherwise `a.Priority = *rule.Priority`.
- Log a matched rule at debug: `slog.Debug("rule matched", "rule", name, "icao", hex,
  "priority", n)`, using the rule's index when `name` is empty.

### 3. Delivery (`notify.go`)

`render` currently computes priority itself:

```go
priority := n.cfg.Ntfy.Priority
if a.Emergency { priority = 5 }
```

Replace both lines with `a.Priority`. The `rotating_light` tag and the `SQUAWK … —` title
prefix stay tied to `a.Emergency`, not to priority — a 7600 dropped to 4 is still framed
as an emergency.

### 4. Docs

- `config.example.yaml`: both keys, commented, using the real examples above. Fix the
  existing line `priority: 3  # 1-5. Emergency squawks are always sent at 5.` — no longer
  true.
- `README.md`: a short section covering the match fields, first-match-wins, `priority: 0`,
  exact-match-not-substring, and that a muted aircraft still alerts on an emergency squawk.

### 5. Tests (`sky_test.go`, matching the existing table-driven style)

Required cases:
1. OR within a field; AND across fields.
2. Exact-match guard: a rule `type: ["B-2 Spirit"]` must NOT match a plane whose type is
   `"Tupolev Tu-154 B-2"`.
3. Case-insensitivity on a field and on a tag.
4. First match wins when two rules both match.
5. `priority: 0` → `Evaluate` returns nil for a db alert.
6. A plane muted by a rule, squawking 7700 → still alerts, priority from `squawk_priority`.
7. `squawk_priority: {"7600": 0}` → a 7600 produces no alert; 7700 still does.
8. Partial `squawk_priority` merges over defaults (setting only 7600 leaves 7700 at 5).
9. No rules configured → priority is `ntfy.priority` (regression guard on default behaviour).
10. `render` puts `a.Priority` on the wire.
11. Config validation: bad squawk key, out-of-range priority, rule missing priority, rule
    with no match fields.

## Key decisions & tradeoffs

- **First match wins**, top to bottom, like firewall rules. Rejected "most specific wins":
  it needs a specificity metric, which is a second thing to get wrong and to document.
- **`priority: 0` means mute**, rather than a separate `mute: true`. ntfy's own scale is
  1-5, so 0 is unused and reads naturally as "below minimum".
- **Exact match, no substring or regex.** Substring looks friendlier and is a trap — see
  the 81 false hits above. Regex is a config-injection footgun for no gain here.
- **`squawk_priority` overrides only; it cannot add trigger codes** (user's call). Listing
  e.g. `1200` would page for every VFR aircraft in range, so unknown keys are a startup
  error rather than a silent flood.
- **Rules never mute emergencies** (user's call). Muting is about routine noise; a hijack
  code on a muted airframe is exactly when you want the page.
- **Rules tune existing alerts only** (user's call). No new trigger source. Consequence,
  stated plainly: a B-2 Spirit will never alert, because plane-alert-db has no B-2 row —
  confirmed against the live 17,227-row DB. B-1 (48 rows) and B-52 (73) are present and
  share the tag `Strategic Bomber`.

## Assumptions

1. `Plane` fields available to match on are `ICAO, Reg, Operator, Type, ICAOType, CMPG,
   Tags, Category` — source: `db.go:26`.
2. `cmpg` is the coarse civil/military/government/police axis, values `Mil` (9852), `Civ`
   (4615), `Gov` (1764), `Pol` (996) — source: live `data/db.json`.
3. Air Force One is matchable by the tag `Air Force One` (3 rows: `adfdf8`, `adfdf9`,
   `af83f3`) — source: live `data/db.json`.
4. Emergency squawks are `7500`/`7600`/`7700` and currently hardcoded to priority 5 —
   source: `match.go:6`, `notify.go:238`.
5. Config validation already fails startup on bad values and has an established error
   style — source: `config.go:280`.

## Risks / open questions

- `cmpg: [Mil]` as a blanket mute also mutes military aircraft the user might want (the
  DPS-style interesting ones). Mitigated by rule order: specific keeps above the broad
  mute. Worth calling out in the README example.
- plane-alert-db tags and categories are upstream data and can change without notice; a
  rule keyed on a renamed tag silently stops matching. Out of scope to solve — noted.

## Out of scope

- New trigger sources, including readsb `dbFlags` military. Nothing that does not alert
  today starts alerting because of this change.
- Env-var configuration of `rules` / `squawk_priority`.
- Substring, glob, or regex matching.
- Per-rule overrides of anything other than priority (no per-rule tags, title, click URL).
- Per-rule cooldowns.
