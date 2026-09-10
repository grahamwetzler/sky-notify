# Plan Review Log: per-aircraft and per-squawk notification priority

Phase 0 (recon) and Phase 1 (interrogation) ran in-session; the spec is `PLAN.md`.
Phase 2 (Codex adversarial plan review) was skipped — this went straight to build.

## Act 3 — Build

Builder: Codex, `gpt-5.6-sol`, reasoning effort low (pinned in `~/.codex/config.toml`),
codex-cli 0.153.4. Thread `01a08939-7a75-79b2-bf29-351f16a79afa`.
`PROOF_CMD = gofmt -l . && go vet ./... && go test ./...`. MAX_FIX_ROUNDS=2, used 1.

### Round 1 — Codex build

Reported: `config.go` (schema, defaults, validation), `match.go` (ordered exact rule
matching, priority selection and muting), `notify.go` (render `Alert.Priority`),
`sky_test.go` (all eleven required cases), `config.example.yaml`, `README.md`. Claimed
proof green, `go.mod` unchanged, no deviations.

### Claude's verdict — REVISE (3 findings)

1. **Silent failure, confirmed by experiment.** A bare `squawk_priority:` line decodes as
   YAML null; yaml.v3 zeroes the map field to nil, every lookup returns 0, and the
   `priority == 0 → mute` branch then swallowed 7500, 7600 **and** 7700 with no error and
   no log. Reproduced with a scratch test before demanding the fix:
   `cfg.SquawkPriority == nil`, `Evaluate(hex ffffff, squawk 7700) == nil`. An operator
   commenting out the codes under that key would have lost every emergency alert.
2. **Test integrity.** `TestRuleMatching`'s `want` column was `nil` on all five rows, with
   the real expectation reconstructed inside the loop by string-matching the test's own
   name. Renaming a case would silently invert its assertion.
3. **Fragility.** `Evaluate` recovered the matched rule's index by scanning for pointer
   identity (`for ; &cfg.Rules[i] != rule; i++ {}`) purely to label a debug log — panics
   on index-out-of-range if the rule ever came from another slice.

### Round 2 — Codex fixes

All three addressed: one shared `defaultSquawkPriority` map, with missing keys refilled
after decode in `LoadConfig` so an absent key means "default" and only an explicit `0`
means "muted"; `TestRuleMatching` rewritten with a `wantMatch bool` column and the
name-matching block deleted; `firstMatch` now returns `(int, *Rule)`.

### Claude's verdict — ACCEPT

Diff re-read in full. Proof re-run by Claude, not taken from the report:
`gofmt -l .` silent, `go vet ./...` clean, `go test -count=1 ./...` → `ok sky-notify 4.582s`.
Regression coverage for finding 1 is now in the suite
(`TestNullSquawkPriorityKeepsDefaults`).

### Live verification (against the real feeder, not just tests)

Ran an instance with a YAML config carrying all three rule shapes against
`http://10.0.0.60/data/aircraft.json`, publishing to the real ntfy topic:

```
DEBUG msg="rule matched" rule="Police, quiet" icao=ab9074 priority=1
INFO  msg=notified icao=ab9074 trigger=db reg=N844TX
```

Delivered payload carried `"priority":1` — the rule overrode `ntfy.priority: 3` end to
end, through the YAML-only config path that no unit test exercises against a real server.
