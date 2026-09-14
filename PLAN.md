# Restructuring plan

A review of the Go sources as they stood at `8ab9ac6`: what is dead, where the files
disagree with the seams in the code, and what is worth changing.

**Applied on this branch, in the order below, one commit per step, `go vet` and the full
suite green after each.** The one exception is 1b, which is gated on a file that is not
in this repo and is still waiting on that answer. Everything else is done, and the
sections below describe what was done rather than what was proposed.

The repo was one `package main`, 15 files, ~5,500 non-test lines, plus a 3,577-line test
file and a 2,094-line embedded UI. It is still one package; it is now 22 non-test files,
seven test files, and a UI whose largest line is no longer a font.

## Not proposed: splitting into packages

`Alert` threads through `match`, `notify`, `history`, `mapsnap` and `main`. Cutting
package boundaries through that means exporting most of the identifiers in the repo and
inventing interfaces to break the cycles, in exchange for nothing a reader gains. One
package stays.

The one real seam, recorded so it does not have to be rediscovered: the map renderer
(`mapsnap.go`, `tiles.go`, `mvt.go`, `view.go`, `icons.go` — ~1,800 lines plus 93 KB of
`icons.json`) reads exactly ten `Alert` fields, all read-only:

    a.Hex  a.Path  a.HasDistance  a.recvLat  a.recvLon
    a.AC.Lat  a.AC.Lon  a.AC.Track  a.AC.Type  a.AC.Category

plus `sample` and `maxSampleGap` from `track.go`. If a package is ever wanted, that is
where it cuts, behind a struct of those ten fields. Nothing forces it now.

---

## 1. Dead code

### 1a. Four struct fields decoded and never read

```go
aircraft.go  Seen      float64  `json:"seen"`       // never read; SeenPos is the one used
aircraft.go  RSSI      float64  `json:"rssi"`       // never read
aircraft.go  Emergency string   `json:"emergency"`  // never read; Alert.Emergency is derived from Squawk
aircraft.go  Messages  int64    `json:"messages"`   // feed.Messages, never read
```

Verified: with all four deleted, `go vet ./...` and `go test ./...` both pass.
`encoding/json` ignores unknown keys unless `DisallowUnknownFields` is set, and neither
decoder on this path sets it, so removing the fields does not change what parses.

### 1b. Migration scaffolding for an unreleased format — gated

`alertsRemoved`, `removedEnv`, `Rule.All`, the `all is no longer a key` branch in
`Alerts.validate`, and `TestMigrationErrors` — about 45 lines plus the test — exist to
give a good error to configs written against the format the rules rewrite replaced.

That rewrite landed 2026-09-10 (`8e2455c`, "Make rules the only reason anything alerts").
The repo has zero tags and zero releases, and no file in the tree — including
`alerts.example.yaml`, `config.example.yaml` and `README.md` — mentions `filters:`,
`all:`, `alert_on_emergency_squawk:`, `squawk_priority:` or any `SKY_FILTERS_*` variable.
The only config that could reach these messages is the live one under the `/config` bind
mount, which is not in this repo and was not inspected.

**Gate — NOT APPLIED.** Check the deployed `alerts.yaml` for those keys first. If it is
clean, delete all of it. If it is not, migrate the file and then delete all of it. The
file lives in the `sky-notify-config` Docker volume, which is not on the machine this was
written on, so the question is still open.

### 1c. Nothing else

`golang.org/x/tools/cmd/deadcode` reports no unreachable functions. Every declared
constant has at least one use beyond its declaration.

---

## 2. File layout

Files do not match the seams already in the code. All of the following are pure
relocation: no new types, no new functions, no signature changes.

| File | Now | Problem |
|---|---|---|
| `sky_test.go` | 3,577 | 40% of the repo in one file, already sectioned by `// ----------` comments that name the split |
| `main.go` | 825 | four unrelated things |
| `config.go` | 790 | six unrelated things |
| `db.go` | 527 | accidental home of two shared helpers |
| `aircraft.go` | 190 | half of the geo math; `track.go` has the other half |

Proposed:

```
sky_test.go → config_test.go, match_test.go, db_test.go,
              notify_test.go, map_test.go, api_test.go
              (split along the existing section comments)

main.go     → main.go    main, run, probeSelf, parseLevel
              loops.go   seedDB, pollLoop, poll, notifyLoop, refreshLoop
              queue.go   queue, pendingAlert and methods
              api.go     mux and its handlers, writeJSONError
              server.go  the struct now called health, and its methods

config.go   → config.go    Duration, Config, Alerts, Live, defaults,
                           env bindings, Load*, loadYAML, paths, checkEnv,
                           nearestName, editDistance
              validate.go  Config.validate, Alerts.validate, checkHTTPURL,
                           mapEnabled, dbFileRe
              watch.go     configWatcher, reloadConfig
              → Rule, Rule.Key, Rule.label move to match.go, beside the
                code that matches them

db.go       → io.go   readLimited, writeFileDurable
                      (each used by four other files; they are in db.go by accident)

aircraft.go → geo.go  haversineNM, closestApproach, earthRadiusNM,
                      and wrap180 out of track.go
                      (all four are consumed by view.go, track.go, match.go, mapsnap.go)
```

Applied in that order, `sky_test.go` first: it is the largest file, the split is
mechanical, and a green suite afterwards is the evidence that nothing moved that should
not have. All 162 test functions survived it unchanged.

One addition to the list above: the helpers section of `sky_test.go` became
`helpers_test.go`, since every one of the six new test files uses it.

---

## 3. Refactors

Ranked. Each is small and independent. All seven are applied.

1. **Cache the vocabulary.** Every `GET /api/alerts` calls `DB.Vocabulary()`, which is
   `Facets(Rule{})`: one full scan of `merged` per pickable field — six passes over the
   whole database, with a `map[string]bool` allocated per row — on every page load. It
   changes only when the database refreshes, which is daily. Compute it in `commit` and
   store it beside `merged`.

2. **`poll()` takes 8 parameters.** `pollLoop` already holds every one of them for its
   lifetime. Make them a `poller` struct so the call reads `p.poll(ctx, cfg)`.

3. **`notifyLoop`'s two parallel maps.** `backoff map[string]time.Time` and
   `delay map[string]time.Duration` are keyed identically and are always written together
   and deleted together. One map of a two-field struct removes the chance of the pair
   drifting apart.

4. **Rename `health` to `server`.** It owns the mux, the liveness fields and the preview
   snapshot. Those three do belong together — one `setPollOK` from one poll fills all of
   them — so this is a rename, not a split. The name is what is wrong.

5. **`previewAlerts` walks `alertEnvBindings()` to switch on `"lat"` and `"lon"`.** Two
   direct `env["SKY_LAT"]` / `env["SKY_LON"]` lookups say the same thing and grep.

6. **`ui.html` line 12 is a 35 KB base64 `woff2` on a single line.** Measured: the file is
   135,979 bytes, the data URI's base64 payload is 35,516, and the font it decodes to is
   26,636. Embedding the `.woff2` as its own file and serving it leaves `ui.html` at
   100,411 bytes (a 26% cut) and drops the embedded payload from 35,516 to 26,636 — about
   8.7 KB off the binary, because `go:embed` stores `ui.html` verbatim and base64 costs a
   third. Verified by finding the base64 text literally inside the compiled binary.
   Neither saving is the reason to do it; a 35 KB line in the middle of a source file is.
   The no-network property DESIGN.md requires is unchanged — the font still comes off the
   binary rather than off the network. Low value, listed last on purpose.

   As applied: `ui.html` is 100,457 bytes, `__rodata` fell 8,768, and the binary fell
   16,448 — more than the payload, because `__TEXT` is 16 KB-aligned on arm64 and the
   saving crossed a page. DESIGN.md said "one file" and "a base64 `woff2`"; it records the
   artifact, so it now says what the artifact does.

7. **`.DS_Store` is untracked but not in `.gitignore`.**

---

## 4. Deliberately left alone

- **Four separate backoff schemes** — `seedDB`'s cold-start loop, `notifyLoop`'s per-key
  backoff, `Publish`'s retry budget, `tileStore`'s breaker. Each has different semantics
  and a different failure it is protecting against. Unifying them means a configuration
  object nobody asked for.
- **`writeCachedTile` vs `writeFileDurable`** — near-twins on purpose: one fsyncs the file
  and its parent directory, one is best-effort. A `durable bool` parameter reads worse
  than two functions.
- **`ui.html` as one file with no build step** — DESIGN.md commits to it explicitly.
- **Facet full scans, beyond caching the vocabulary in item 1** — roughly 30k rows,
  scanned at a human keystroke cadence.
- **`run()` exiting via `os.Exit(1)` for config errors while returning errors afterwards**
  — it looks like two failure protocols in one function, and it is, but the comment gives
  the reason: `slog` is not configured yet and the message still has to be legible.
