# Plan: `callsign` rule condition

Filter on the callsign the aircraft broadcasts (`flight` in aircraft.json), so
`callsign: [SWA]` catches every Southwest flight. Live feed only — plane-alert-db
has no callsign column, and this never touches the database.

## Semantics: prefix, case-insensitive, trimmed

Feed callsigns are `SWA2504`, not `SWA`. Exact matching — what every other condition
does — would make the airline case impossible, so this one field matches on **prefix**:

- `SWA` matches `SWA2504`, `SWA11`, `swa900`
- `SWA2504` still matches exactly that flight (a prefix of itself)

One field, one rule, covers both "an airline" and "one flight". Deliberately not a
substring match: `WA` should not catch Southwest. Deliberately not a second
`airline:` field — a prefix already is the airline code.

This is the only condition that is not exact, so both docs that promise "exact, not
substrings" (README, and the UI help text) have to say so.

## Changes

**config.go** — one field on `Rule`, placed with the other feed identity fields:

```go
Callsign []string `yaml:"callsign,omitempty" json:"callsign,omitempty"`
```

`Key()` fingerprints the whole struct, so cooldown keys pick it up for free. No
`validate()` clause: the helper below handles a blank value, and every other string
field is unvalidated too.

**match.go** — two edits:

1. `matchesIdentity`: add `!matchesPrefix(r.Callsign, ac.Flight)` to the existing
   ICAO/squawk check. New helper beside `matches`:

   ```go
   func matchesPrefix(want []string, got string) bool {
       if len(want) == 0 { return true }
       got = strings.TrimSpace(got)
       for _, v := range want {
           if v = strings.TrimSpace(v); v != "" &&
               strings.HasPrefix(strings.ToUpper(got), strings.ToUpper(v)) { return true }
       }
       return false
   }
   ```

   A blank value is skipped rather than honoured, because `""` is a prefix of every
   string: without the guard one stray empty chip in the UI would silently widen a
   rule to every aircraft overhead. Skipping it instead leaves a blank-only condition
   matching nothing, which is how every other runtime condition fails. (Note this is
   *not* what the exact `matches` does with a blank — there `squawk: [""]` means "no
   squawk", a real question. "No callsign" is not askable through a prefix, and is not
   worth a second field.)

2. `matchesPlane` (database preview): clear `identity.Callsign` alongside
   `identity.Squawk`. **This is the one that bites if missed** — a database row has no
   callsign, so without it every callsign rule previews as zero aircraft and reads to
   the user as dead.

`MatchLive` already carries `Flight` on `LiveHit`, so the live preview shows the
callsign it matched with no change.

**ui.html** — two edits:

1. A `CONDITIONS` entry after `squawk`, feed-sourced and free-text:

   ```js
   {key: 'callsign', label: 'Callsign', short: 'the callsign', group: 'identity',
    source: 'feed', kind: 'free',
    help: 'What the aircraft broadcasts as its flight number, e.g. SWA2504. Matched on
           the start of the callsign, so SWA is every Southwest flight.'}
   ```

   `LIST_KEYS`, `blindKeys`, `coverage()` and `canMatchFeed()` all derive from
   `CONDITIONS`/`source`, so the "read off the live feed, nothing to count here"
   framing and the feed-only preview handling come along with no further edits.

2. `summarise()`: `say('callsign', v => 'callsign starting ' + list(v));` — the
   sentence has to say *starting*, because every other clause reads as exact.

No value picker: the database has no vocabulary for this, and a suggestion list built
from what the receiver has heard is a separate feature. (`health.typeOf` is the place
it would come from if it is ever wanted.)

**Docs**

- README rules table: `callsign` into the "identity, from the feed" row, and amend
  "Matches ignore case and surrounding space but must be exact—not substrings" with
  the `callsign` exception.
- PRODUCT.md line 68: add to "identity, from the feed only"; line 72's list of what
  the preview cannot answer gains it too.
- alerts.example.yaml: one worked example.

  ```yaml
  # Callsigns match on their start, so SWA is every Southwest flight.
  - name: Southwest overhead
    callsign: [SWA]
    max_distance_nm: 10
  ```

## Check

One test in sky_test.go beside `TestRegAndICAOTypeMatchFromEitherSource`:

- `callsign: [SWA]` matches feed flight `SWA2504 ` (trailing space — the feed pads)
- lowercase `swa` matches too
- `SWA` does not match `ASA100`, and `WA` does not match `SWA2504` (prefix, not substring)
- `callsign: [""]` matches nothing, not everything — the guard against a stray blank
- `matchesPlane` with a callsign-only rule still returns true for a database row,
  proving the preview did not go dark

Then `go build ./... && go test ./...`.

## Skipped

- A callsign vocabulary/picker fed from what the receiver has heard — add when typing
  three letters proves annoying.
- Wildcards or regex in the value — add when someone wants a suffix or an infix.
- Matching the database: it has no callsign column.
