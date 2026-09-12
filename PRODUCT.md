# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

One person: the operator of a self-hosted ADS-B receiver (ultrafeeder/readsb/tar1090)
running sky-notify next to it on their own network. They are not a customer or a team —
they are the same person who wrote the docker-compose file. They open the web UI on a
desktop and, often, one-handed on a phone while something is overhead.

Their dominant job is **auditing the whole rule set**: reading every rule top to bottom to
see what is covered, which rule shadows which, where the wildcard sits, and whether any
rule has quietly stopped matching anything. Tuning a single rule and building a new one
from the database's vocabulary are the other two jobs, in that order.

## Product Purpose

sky-notify watches ultrafeeder's `aircraft.json` and sends an ntfy notification when an
interesting aircraft shows up overhead, matched against sdr-enthusiasts/plane-alert-db.
The web UI is the editor for `alerts.yaml` — the hot-reloaded half of the configuration:
rules, receiver position, cooldown, default priority, poll and refresh intervals, log level.

Success is the operator being able to see, in one pass, exactly which aircraft their rules
select and which they do not — before an aircraft they cared about flies past unannounced.

## Positioning

Rules are an **allowlist**: nothing alerts unless a rule matches it, emergency squawks
included. That makes a silently-empty rule the product's characteristic failure — a typo'd
tag costs every alert the rule was meant to catch, and the YAML looks perfect. So the UI
does not let you type rule values blind: conditions are built from the database's own
vocabulary, every value carries how many aircraft it covers, the vocabulary re-facets as
the rule narrows, and each rule continuously previews what it selects against the live
database. That preview loop is the thing a plain YAML editor cannot do.

## Operating Context

- Runs on the LAN, served by the Go binary itself at `SKY_LISTEN` (default `:8080`).
  No authentication, no internet assumption, no build step, no CDN.
- The UI is a single file, `ui.html`, `go:embed`ed into the binary. It must stay one file
  with no external requests.
- `alerts.yaml` remains authoritative and hand-editable. Environment variables win over the
  file; the UI shows which fields are locked by an env var and disables them.
- Saved changes are picked up on the next 5-second reload tick. Comments do not survive a save.
- Startup-only settings (`config.yaml`: feeder URL, ntfy credentials, db mirror, cache dir)
  are deliberately **not** editable here.

## Capabilities and Constraints

API the UI is built on:

- `GET /api/alerts` → `{alerts, path, locked:[{key,env}], vocabulary:{field:[{value,count}]}}`
- `POST /api/preview?offset=N` (a Rule) → `{total, aircraft[], database, limit, offset, facets}`
  — facets only on offset 0. Faceting scans the whole database per field.
- `PUT /api/alerts` (a full Alerts document) → validated as the service will run it, then written.
- `GET /healthz` → status, aircraft count, db rows, pending queue, staleness reasons.

Rule model — every condition is an optional filter, ANDed; values within one field are ORed;
first matching rule wins; a rule stating no condition is the wildcard and matches everything:

- identity, from the database: `operator`, `type`, `cmpg`, `category`, `tags`, `listed`
- identity, matched against database **and** live feed: `reg`, `icao_type`
- identity, from the feed only: `icao`, `squawk`
- where it is: `min_altitude_ft`, `max_altitude_ft`, `max_distance_nm`
- how it is flying: `circling`, `passes_within_nm`, `passes_within`

The preview searches the database only. Altitude, distance, flight path and squawk cannot
narrow it, and the UI must say so on each such condition rather than looking broken.
Conditions fail closed: a rule stating altitude does not match an aircraft that has not
reported one.

`priority: 0` mutes and stops evaluation. A rule without `priority` follows `ntfy.priority`
and must keep following it — the UI must never bake today's default into a rule.

Rule names are **optional** (decided 2026-09-12). The name never reaches the notification;
it is the cooldown key and the log label. A nameless rule is keyed by a fingerprint of its
own conditions, so reordering rules does not reset cooldowns and editing a rule's conditions
does. Named rules still must be unique.

`max_distance_nm` and `passes_within_nm` require top-level `lat`/`lon`; `circling` requires
`source.poll_interval` ≤ 30s. Both are save-time validation errors from the server, and the
UI should not let the operator walk into them unwarned.

## Brand Commitments

Name: **sky-notify**. Voice, taken from the README and the existing UI copy: plain, direct,
and willing to state the honest limitation in the same breath as the feature ("a zero here
is inconclusive", "delivery is at-least-once"). No marketing register, no exclamation marks,
no emoji in product copy. The UI explains *why* a thing is the way it is, briefly, where it
matters.

## Evidence on Hand

Real data at runtime: plane-alert-db (thousands of airframes; categories, tags, operators,
types, CMPG classes), the live `aircraft.json` feed, and the operator's own `alerts.yaml`.
`alerts.example.yaml` holds a realistic rule set. There are no customers, testimonials,
benchmarks, or pricing, and none may be invented.

## Product Principles

1. **A rule that matches nothing is the failure to design against.** Coverage is visible at
   all times, not on request.
2. **Order is meaning.** First match wins, so the list is a sequence, not a bag; shadowing
   and the wildcard's position must be readable.
3. **Say what the tool cannot know.** Where the preview is blind — live-feed fields, position,
   flight path — the UI says so on the spot instead of implying a confident zero.
4. **The file is the truth.** The UI edits `alerts.yaml`; hand edits and env overrides win
   where they always did, and the UI shows that rather than hiding it.
5. **One file, no network.** It must keep working on a LAN with no internet, from a single
   embedded HTML file.

## Accessibility & Inclusion

Keyboard-operable throughout (the value pickers already are, and must stay so). Both a light
and a dark presentation are a hard requirement from the operator. Real focus indicators,
labelled controls, and text that survives a phone-width viewport.
