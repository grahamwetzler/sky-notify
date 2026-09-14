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
// notify at the closest pass.
//
// judged is the last poll that could tell whether the aircraft had passed — present in
// the feed, and with the ground track receding needs. Not "last seen": an aircraft that
// keeps matching while broadcasting no track would refresh a last-seen stamp forever and
// never reach the fallback that exists for exactly that case. It is set when the entry is
// created, so an aircraft that has never once been judgeable waits out the same grace
// window rather than tripping a zero timestamp on its first sweep.
//
// fired is when the alert was first offered to the queue, zero until then. The entry
// outlives the offer, because an offer is not a delivery — delivered is what says it was.
type held struct {
    alert  *Alert
    judged time.Time
    fired  time.Time
}
```

`poller.holds map[string]held`, keyed by the same `cooldownKey(hex, trigger)` everything
else uses. Each poll, for a matching rule with `notify: closest_pass`:

- an ineligible key — the cooldown says this rule already alerted on this aircraft —
  drops any entry and is skipped, as it is today;
- park the alert if there is no entry yet, stamping `judged` with this poll; otherwise
  keep whichever of the parked and incoming alerts is nearer;
- refresh `judged` if the aircraft can be judged at all: `ac.GS == 0 || ac.Track != nil`,
  the same guard `passesOverhead` already fails closed on. A stationary aircraft is
  judgeable and `receding` answers true for it, since it will never get nearer;
- set `fired` if `receding(...)` and it is not already set;
- enqueue nothing here. No cooldown is recorded by waiting, so nothing is consumed by it,
  and the next poll re-evaluates exactly as the existing "hold the alert until a position
  arrives" path does.

One sweep after the aircraft loop is the only thing that touches the queue, so there is a
single place where a parked alert can be offered, retried or dropped:

- delivered — drop the entry. What says so is an ack on the alert itself: `Alert` gains an
  `atomic.Bool`, `notifyLoop` sets it beside the `state.Record` that follows a successful
  publish, and the sweep reads it. The alert is one pointer shared by the poller, the
  queue and the notifier, so the ack needs no map, no cleanup and no second lock.

  Not `!state.Eligible(key, cooldown)`, which is what an earlier draft of this plan said:
  the ledger cannot answer this. `validate` requires `cooldown > 0` and
  `source.poll_interval > 0` and relates them not at all, so `cooldown: 5s` on 15s polls
  is a legal configuration in which a delivered key is eligible again before the next
  sweep looks — and `State.prune` has very likely dropped its ledger entry by then
  anyway. The sweep would re-offer a delivered alert every poll until `holdGiveUp`;
- not fired, and `judged` older than `3 * source.poll_interval` — set `fired`. That is the
  fallback for the two aircraft `receding` can never answer for: one that left the feed
  mid-approach, and one that stays in it broadcasting a position but no ground track.
  Three polls of grace rather than "missing from this poll", because feeds drop an
  aircraft for a poll and come back, and firing on a flicker would report a pass at 40 NM
  that was going to be 2 NM;
- fired and not yet acked — `q.add(h.alert)` every poll. Not "enqueue once and forget":
  `notifyLoop` deletes a queue entry whose publish failed without advancing the cooldown,
  and `q.add` refuses an alert outright at `maxPending`. Dropping the entry on the offer would
  answer either by rebuilding the alert from a later poll — a worse distance for the pass,
  which is the one thing this whole mechanism exists to get right — or, for an aircraft
  that has left the feed, by losing it. `q.add` ignores a key that is already queued or in
  flight, so re-offering costs a map lookup;
- fired longer than `holdGiveUp` (1h, two attempts past the 30-minute ceiling
  `notifyLoop`'s per-key backoff climbs to) — drop it. An ntfy server that has been down
  for a day must not park every aircraft that passed in that time.

The parked alert is the whole `*Alert` as of its nearest poll, so the map snapshot and
the recent-track path are the ones from the closest point too — the picture shows the
pass, not the departure.

Bounded by aircraft overhead × closest-pass rules, and no entry outlives delivery by more
than `holdGiveUp`. It is memory only, like `Tracker`: a restart forgets the holds in
flight, and the aircraft still overhead are picked up on the next poll.

## What the notification says

`Alert` gains one plain bool, `AtClosest` — the delivery ack above is the other field, and
the two are unrelated. `render` in `notify.go` adds one line when `AtClosest` is set:

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

**3. The hold.** `held`/`poller.holds` in `loops.go`, the branch in `poll`, the sweep, and
the one-line delivery ack `notifyLoop` sets beside `state.Record`.
Tests in `match_test.go` driving `poll` over a synthetic sequence of feeds, with the
queue read directly rather than a notifier stood up:
- inbound polls enqueue nothing, the pass enqueues exactly one alert, and its
  `DistanceNM` is the minimum of the sequence, not the distance at the firing poll;
- an aircraft that vanishes mid-approach fires after the grace window with its nearest
  seen distance;
- an aircraft that stays in the feed, keeps matching and never broadcasts a ground track
  enqueues nothing for three polls and then fires with its nearest distance. Both halves
  are the assertion: a last-seen stamp would hold it forever, and a `judged` left zero at
  creation would fire it on the first sweep. The test drives ten polls, not three;
- a failed delivery — the queue entry taken and dropped with no cooldown recorded, which
  is what `notifyLoop` does — is re-offered on the next poll as the *same* alert, with the
  nearest distance intact, and stops being offered once a publish succeeds;
- an alert refused because the queue is at `maxPending` is still parked, and lands with
  its nearest distance once the queue drains;
- an entry fired longer ago than `holdGiveUp` is dropped and stops being re-offered;
- a delivered alert is offered exactly once under `cooldown: 5s` with `poll_interval: 15s`
  — the legal configuration in which the cooldown ledger says "eligible" again, and has
  pruned its own entry, before the next sweep runs;
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
