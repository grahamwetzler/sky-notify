# sky-notify

Watches ultrafeeder's `aircraft.json` and sends an [ntfy](https://ntfy.sh) notification
when an interesting aircraft shows up overhead — matched against the
[sdr-enthusiasts/plane-alert-db](https://github.com/sdr-enthusiasts/plane-alert-db) list.

One static Go binary, no cgo, one container.

```
US Air Force C-17 Globemaster III
Registration: N12345
Callsign: RCH451
Operator: US Air Force
Type: C-17 Globemaster III
Category: Zoomies
Tags: Cargo, Heavy Lift
```

## Quick start

```yaml
services:
  sky-notify:
    image: ghcr.io/grahamwetzler/sky-notify:latest
    restart: unless-stopped
    environment:
      SKY_SOURCE_URL: http://ultrafeeder/data/aircraft.json
      SKY_NTFY_URL: https://ntfy.sh
      SKY_NTFY_TOPIC: your-secret-topic-name
      SKY_DB_BASE_URL: https://raw.githubusercontent.com/sdr-enthusiasts/plane-alert-db/main
      SKY_DB_FILES: plane-alert-db.csv
      SKY_CACHE_DIR: /data
    volumes:
      - sky-notify-data:/data
      - sky-notify-config:/config

volumes:
  sky-notify-data:
  sky-notify-config:
```

All six are required. sky-notify assumes nothing about your deployment — which feeder,
which ntfy server, which database mirror, which volume — so a misconfigured instance
refuses to start instead of quietly talking to somewhere you did not choose.

Subscribe to that topic in the ntfy app and you're done. On public ntfy.sh **the topic
name is the only secret**, so pick something unguessable.

The `/data` volume matters: it holds the cached aircraft list, the alert cooldown ledger
and the record of what has been sent. Without it you re-download the list on every restart,
re-alert on everything currently overhead, and lose the history.

`/config` is where `alerts.yaml` lives, and the web UI writes it. It has to be writable by
the container's non-root user, which a named volume is — a read-only bind mount is not, and
saves will fail against one. Set `SKY_ALERTS_CONFIG` to put the file somewhere else.

See [`docker-compose.yml`](docker-compose.yml) for a fuller example and
[`config.example.yaml`](config.example.yaml) for every setting.

### Rules

Nothing alerts unless a rule in `alerts.yaml` matches, including emergency squawks and
aircraft in plane-alert-db. Every condition is an optional filter of the same kind:

| | |
|---|---|
| identity, from the feed | `icao`, `reg`, `icao_type`, `squawk` |
| identity, from the database | `operator`, `type`, `cmpg`, `category`, `tags`, `listed` |
| altitude and distance | `min_altitude_ft`, `max_altitude_ft`, `max_distance_nm` |
| flight path | `circling`, `passes_within_nm`, `passes_within` |

Values within one field are ORed, conditions are ANDed, and the first matching rule wins.
A rule that states **no** condition is the wildcard — it matches every aircraft, so it
goes last, and `listed: true` alone is "everything in plane-alert-db". Matches ignore case
and surrounding space but must be exact—not substrings. A rule without `priority` uses
`ntfy.priority`; `priority: 0` mutes and stops evaluation. Put exceptions first.

`name` is optional and never reaches the notification — it is the cooldown key and the
log label, so it is worth setting on a rule you want to recognise in the log and not
worth inventing on one whose conditions already say what it is. A rule without a name is
keyed by a fingerprint of its own conditions, like `#9979234a`: reordering the rules
leaves its cooldowns alone, and editing what it matches resets them, which is right,
since the ledger's entries were recorded for a rule that no longer exists. Keys must be
unique, because two rules sharing one would silence each other — so no two rules may
share a name, state the same conditions unnamed, or have one named after the other's
fingerprint.

The flight path conditions read the same recent motion. `passes_within_nm` predicts the
closest approach to your `lat`/`lon` along the aircraft's current ground track and speed,
within `passes_within` (default `5m`), so `passes_within_nm: 1` means "will fly over me in
the next five minutes". `circling: true` matches an aircraft that has turned a full circle
inside 2 NM during the last 10 minutes, which is how news and police helicopters fly. It
needs several minutes of positions before it can match, and that history is held in
memory only, so a restart starts it again. It needs `source.poll_interval` of 30s or less.

## The map

Every notification carries a PNG of where the aircraft is: the receiver marked, the
aircraft pointed along its heading, and the last ten minutes of its track drawn behind
it, on [OpenFreeMap](https://openfreemap.org) tiles in the `fiord` style. It is rendered
here — vector tiles decoded and drawn in Go, no browser and no cgo — and cached on disk
under `cache_dir`, so a familiar patch of sky costs no network at all.

The map is always optional. Tiles down, a slow fetch, a render error, or an ntfy server
that will not take the attachment all cost the notification its picture and nothing else:
the alert still goes out, as text, on the same path it always used. Set
`SKY_MAP_ENABLED=false` to turn it off.

## Configuration

Configure with environment variables and two optional YAML files — **environment wins
over the files, which win over the defaults**. `config.yaml` contains startup-only
settings. `alerts.yaml` contains hot-reloaded settings and defaults beside the resolved
config path. Missing files are fine. See both example files for the exact split.

| Variable | Default | |
|---|---|---|
| `SKY_SOURCE_URL` | — | **required**; an http(s) URL **or** an absolute file path |
| `SKY_NTFY_URL` | — | **required**; `https://ntfy.sh` or your own server |
| `SKY_NTFY_TOPIC` | — | **required** |
| `SKY_DB_BASE_URL` | — | **required**; e.g. plane-alert-db `main`, or a fork |
| `SKY_DB_FILES` | — | **required**; comma-separated, first file to define an ICAO wins |
| `SKY_CACHE_DIR` | — | **required**; holds the cached list and cooldown ledger |
| `SKY_NTFY_TOKEN` | — | or `SKY_NTFY_USER` + `SKY_NTFY_PASSWORD` |
| `SKY_NTFY_PRIORITY` | `3` | 1–5; default for rules that omit `priority` |
| `SKY_SOURCE_POLL_INTERVAL` | `15s` | |
| `SKY_SOURCE_MAX_AGE` | `60s` | how stale `aircraft.json` may be before the feed counts as dead |
| `SKY_COOLDOWN` | `24h` | how long to stay quiet about an aircraft after alerting |
| `SKY_DB_REFRESH_INTERVAL` | `24h` | |
| `SKY_TAR1090_URL` | — | makes notifications click through to your map |
| `SKY_MAP_ENABLED` | `true` | attach a map snapshot to each notification |
| `SKY_MAP_TILES_URL` | `https://tiles.openfreemap.org/planet` | TileJSON endpoint; point at your own OpenFreeMap |
| `SKY_LAT` / `SKY_LON` | — | your receiver; needed by rules with `max_distance_nm` or `passes_within_nm` |
| `SKY_LOG_LEVEL` | `info` | |
| `SKY_LISTEN` | `:8080` | serves `/healthz` |
| `SKY_CONFIG` | `/config/config.yaml` | |
| `SKY_ALERTS_CONFIG` | `alerts.yaml` beside `SKY_CONFIG` | |

A typo'd YAML key or an unrecognised `SKY_*` variable is a **startup error**, not a
silent fallback to the default. `SKY_` is this service's namespace, and
`SKY_COOLDWON=1h` quietly leaving you on 24h is exactly the bug worth failing loudly on.

### Alert settings web UI

Browse to the configured listen address to edit every setting in `alerts.yaml`. The rule
list is the page: each rule reads back as a sentence — "Police aircraft, within 15 NM of
the receiver" — beside a figure for how much of the database it selects, so the set can be
audited by reading down it rather than by reading the YAML. Rules below a wildcard are
struck through as unreachable. Selecting one opens it; conditions are grouped by the
question they ask (which aircraft it is, where it is right now, how it is flying) and are
built from the database's own vocabulary rather than typed. A typo'd tag is otherwise
silent, matching nothing and losing every alert the rule was meant to catch.

The preview searches the database, so it answers which aircraft a rule *can* select, not
which would alert this second — and a figure is only shown where a database row could both
narrow the rule and satisfy it. A rule stating nothing but `squawk` would otherwise read as
the whole database, and a `listed: false` rule as zero, so both read `—` with the reason
instead. The altitude, distance and flight path conditions describe where an aircraft is
right now, and `squawk` comes off the live feed; no database row can answer any of them, so
the page names them under the figure as not counted. `squawk` offers the three emergency
codes as quick selects, and takes any other code typed in.

`reg`, `icao` and `icao_type` accept any value, whether or not the database has it — all
three are matched against the database *and* the live feed, so a type code no listed
aircraft carries still alerts the moment one broadcasts it. The page suggests what the
database holds and marks a value it does not, but never refuses one.

Conditions that would be rejected on save say so where they are built: a distance with no
receiver `lat`/`lon`, a `circling` rule with `source.poll_interval` above 30s, a maximum
altitude below the minimum. Fields an environment variable has claimed are disabled and
name the variable. The page follows the browser's light or dark setting and has a toggle.

Each rule carries its own **sent alerts** list: every notification it has delivered, as
ntfy received it — title, body, priority, tags and link — with the time it went out,
newest first and paged twenty at a time. It is stored in `history.db` (SQLite) on the
`/data` volume and kept for 90 days. Deliveries are filed under the rule's cooldown key,
which is its name, or a fingerprint of its conditions if it has none — so renaming a rule,
or re-conditioning an unnamed one, starts a fresh list rather than claiming alerts it
never sent.

The file remains authoritative, hand edits still work, and environment variables still win
over it. Saved changes are picked up within the next 5-second reload tick. Comments do not
survive a save. There is no authentication — put it on a local network or behind a proxy.

## Things worth knowing

`config.yaml` is read at startup. `alerts.yaml` is re-read every 5 seconds, so every
setting in it updates live. A key placed in the wrong file is a startup error that names
the file it belongs in.

**An alert waits for a position.** No aircraft alerts until it has reported `lat`/`lon`,
and with them its distance from your `lat`/`lon` when you have set one — a notification
you cannot place on a map is one you cannot act on. Nothing records a cooldown until the
alert is published, so this holds the alert rather than dropping it: the next poll
re-evaluates the same aircraft, which is usually still overhead. Mode-S-only traffic that
never broadcasts a position never alerts.

**Conditions fail closed.** A rule stating an altitude does not match an aircraft without
`alt_baro`, and one with `passes_within_nm` does not match without a ground track when the
aircraft is moving. A rule states a condition about data the aircraft has not sent, so it
does not match — rather than matching on a zero.

**Delivery is at-least-once.** Publishing to ntfy and recording the cooldown can't be
made atomic, so a crash in the gap between them re-alerts that aircraft on restart. The
gap is a synchronous `fsync`'d write, so it's small — but it isn't zero, and pretending
otherwise would be the actual defect.

**Cooldowns are per aircraft *and* rule.** Two rules that match the same aircraft have
independent cooldowns, while only the first matching rule alerts in each poll. The key is
the rule's `name`, or a fingerprint of its conditions when it has none.

**A stale list is served forever.** If the refresh fails, sky-notify keeps using the
cached list and warns — an airframe that was interesting last month still is. What it
won't do is start up with no list at all: a cold start that can't obtain one exits
non-zero rather than sitting there looking healthy while matching nothing.

**`/healthz` reports more than "the process is up."** It goes 503 if the feed has gone
stale (checked against `aircraft.json`'s own timestamp, so a frozen-but-valid file from a
dead feeder is caught), if notifications are failing, or if the cooldown ledger can't be
written. Docker restart policies do *not* restart a container for being unhealthy, so
treat this as observability — or wire it to something that does act on it.

## Development

```sh
go test -race ./...   # 70 tests, no framework
go vet ./...
docker build -t sky-notify .
```

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs gofmt/vet/test on every
push and pull request. Pushes to `main` and `v*` tags also publish a multi-arch
(amd64 + arm64) image to
[`ghcr.io/grahamwetzler/sky-notify`](https://github.com/grahamwetzler/sky-notify/pkgs/container/sky-notify)
— `latest` from `main`, semver tags from releases. Nothing to configure: it authenticates with the built-in `GITHUB_TOKEN`.

## Credits & licence

This service is MIT licensed — see [`LICENSE`](LICENSE).

The interesting-aircraft list is [plane-alert-db](https://github.com/sdr-enthusiasts/plane-alert-db)
by the SDR Enthusiasts, used under ODbL 1.0 / DbCL 1.0. This service only reads it.
`aircraft.json` comes from [readsb](https://github.com/wiedehopf/readsb) via
[ultrafeeder](https://github.com/sdr-enthusiasts/docker-adsb-ultrafeeder).
