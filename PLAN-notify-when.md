# Per-rule "when to notify"

Today every rule alerts on the first poll it matches. This adds one optional key per
rule that says *when* the alert goes out:

```yaml
rules:
  - name: Police helicopters
    cmpg: Pol
    max_distance_nm: 25
    notify: closest_pass     # default is on_sight
```

| value | meaning |
|---|---|
| `on_sight` | the current behaviour, and the default: alert on the first poll the rule matches |
| `closest_pass` | hold the alert while the aircraft is inbound and send it once it has passed, describing the nearest point it reached |

`notify` is not a condition. It never changes which aircraft a rule claims, only when the
one it claimed is announced. That is why it stays out of the unnamed-rule fingerprint
(below), out of the database preview, and out of `summarise`'s condition clauses.

---

## How "it has passed" is decided

Stateless test, same flat projection `closestApproach` already uses: the aircraft has
passed when its radial velocity is non-negative — the dot product of the receiver→aircraft
vector and the ground-track velocity vector is `>= 0`.

```go
// receding reports whether an aircraft holding this ground track and speed is moving
// away from (lat0, lon0). A stationary aircraft is receding: it will never get nearer.
func receding(lat0, lon0, lat, lon, gsKt, trackDeg float64) bool
```

Six lines in `geo.go`, beside `closestApproach`, which projects the same way.

A poll interval is 15s and a jet covers ~1.7 NM in that time, so the position at the poll
that first reads "receding" is already past the closest point. Firing the alert from
*that* position would misreport the pass by up to a poll's worth of flying. So the rule
holds the alert it built at the nearest position it has seen, and sends that one.

## The hold

A new field on `poller` (`loops.go`), which is single-goroutine and needs no lock:

```go
// held is one alert parked at the nearest position seen for it so far, for rules that
// notify at the closest pass. seen is the last poll that still matched, so an aircraft
// that leaves the feed still gets its alert rather than being lost mid-approach.
type held struct {
    alert *Alert
    seen  time.Time
}
```

`poller.holds map[string]held`, keyed by the same `cooldownKey(hex, trigger)` everything
else uses. Each poll, for a matching rule with `notify: closest_pass`:

- keep the incoming alert if it is nearer than the parked one, otherwise keep the parked
  one; stamp `seen`;
- if `receding(...)` — or if the aircraft reports no ground track and is not moving —
  send the parked alert and drop the entry;
- otherwise enqueue nothing and let the next poll re-evaluate, exactly as the existing
  "hold the alert until a position arrives" path does. No cooldown is recorded, so
  nothing is consumed by waiting.

After the aircraft loop, any entry whose `seen` is older than `3 * source.poll_interval`
is sent and dropped. That is the fallback that stops two cases from silently losing an
alert: an aircraft that leaves the feed while still inbound, and one that never broadcasts
a ground track, where `receding` has nothing to answer with. Three polls of grace, rather
than "missing from this poll", because feeds drop an aircraft for a poll and come back,
and firing on a flicker would report a pass at 40 NM that was going to be 2 NM.

The parked alert is the whole `*Alert` as of its nearest poll, so the map snapshot and
the recent-track path are the ones from the closest point too — the picture shows the
pass, not the departure.

Bounded by aircraft overhead × closest-pass rules, and every entry exits within the grace
window. It is memory only, like `Tracker`: a restart forgets the holds in flight, and the
aircraft still overhead are picked up on the next poll.

## What the notification says

`Alert` gains one bool, `AtClosest`. `render` in `notify.go` adds one line when it is set:

```
Closest pass: 1.2 NM
```

`a.DistanceNM` is already the distance at the parked position, so there is nothing new to
compute. The existing `Overhead: closest 1.2 NM in 40s` line stays what it is — the
*prediction* a `passes_within_nm` condition recorded. A rule may state both; then the
prediction is what let it match and this line is what actually happened.

---

## Steps, one commit each, `go vet` and the suite green after every one

**1. `receding` in `geo.go`.** Plus a table test in `match_test.go` beside
`TestClosestApproach`: inbound, outbound, abeam (dot product ≈ 0, counts as passed),
stationary, and a track that crosses the antimeridian.

**2. `Rule.Notify string`** with `yaml:"notify,omitempty" json:"notify,omitempty"`, and in
`validate.go`:
- value must be `""`, `on_sight` or `closest_pass`;
- `closest_pass` requires top-level `lat`/`lon`, the same error shape `max_distance_nm`
  already produces — without a receiver there is no distance to be closest to.

In `Rule.Key()`, add `conds.Notify = ""` beside `Name`, `Priority` and `All`, with the
reason the existing comment gives: the fingerprint covers what the rule claims, not how it
is announced. This also means the key is unchanged for every rule that exists today, so
nobody's cooldown ledger or sent-alerts history resets on upgrade — a test asserts that a
rule with `notify` set fingerprints identically to the same rule without it.

**3. The hold.** `held`/`poller.holds` in `loops.go` and the branch in `poll`. Tests in
`match_test.go` driving `poll` over a synthetic sequence of feeds:
- inbound polls enqueue nothing, the pass enqueues exactly one alert, and its
  `DistanceNM` is the minimum of the sequence, not the distance at the firing poll;
- an aircraft that vanishes mid-approach fires after the grace window with its nearest
  seen distance;
- an aircraft with no ground track fires the same way;
- `on_sight` and a rule with no `notify` behave exactly as they do today (the regression
  guard for the default path);
- a held key that is already within its cooldown never parks.

**4. The notification line** in `notify.go`, asserted in `notify_test.go`.

**5. The UI (`ui.html`).** The control belongs with Priority — both say what happens when
a rule matches, neither filters — so `priorityBand` becomes a two-field band titled
*Notification*:

- a `When` select: "As soon as it is seen" / "At its closest pass to the receiver", with
  a help line that says the second one waits for the aircraft to pass and reports the
  nearest point it reached;
- when the receiver has no lat/lon on screen, the closest-pass option carries the same
  warning the distance conditions already show, since the server will reject the save.

Also: `payloadRule` carries `notify` through (it is not in `CONDITIONS`, so it needs the
one line `name`/`priority` get); `question` drops it alongside `name` and `priority`, so
changing it does not ask the server for a preview that cannot differ; and `summarise`
appends `, alerted at its closest pass` only when it is set, so the audit sentence still
reads as conditions first.

**6. Docs.** `README.md`: a row in the rules table, a short paragraph under the flight-path
one, and a line in *Things worth knowing* saying the holds are in memory and a restart
drops the ones in flight. `alerts.example.yaml`: one commented rule. `DESIGN.md` and
`PRODUCT.md` only if step 5's band changes what those describe.

---

## Deliberately not in scope

- **No third option.** "When it leaves range" and "every N minutes while overhead" are
  the obvious next asks; neither was asked for, and the key is a string, so adding one
  later costs a case in `validate` and an option in the select.
- **No held-alert count in `/healthz`.** `poller` is unlocked on purpose and the server
  reads from another goroutine; exposing the depth means a mutex for a number nobody has
  wanted yet.
- **No persistence of holds across restart.** Same trade the `Tracker` already makes.
- **The prediction stays straight-line.** `receding` inherits `closestApproach`'s
  assumption; an aircraft turning hard onto a new course can read as receding for a poll.
  The cost is an alert sent slightly early, with an honest distance for the pass it
  actually flew.
