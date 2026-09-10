# sky-notify

Watches ultrafeeder's `aircraft.json` and sends an [ntfy](https://ntfy.sh) notification
when an interesting aircraft shows up overhead — matched against the
[sdr-enthusiasts/plane-alert-db](https://github.com/sdr-enthusiasts/plane-alert-db) list.

One static Go binary, one dependency, one container.

```
US Air Force C-17 Globemaster III
Registration: N12345
ICAO: adeb2f
Callsign: RCH451
Category: Zoomies
Tags: Cargo, Heavy Lift
Altitude: 31000 ft
Ground speed: 420 kt
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

volumes:
  sky-notify-data:
```

All six are required. sky-notify assumes nothing about your deployment — which feeder,
which ntfy server, which database mirror, which volume — so a misconfigured instance
refuses to start instead of quietly talking to somewhere you did not choose.

Subscribe to that topic in the ntfy app and you're done. On public ntfy.sh **the topic
name is the only secret**, so pick something unguessable.

The `/data` volume matters: it holds the cached aircraft list and the alert cooldown
ledger. Without it you re-download the list on every restart and re-alert on everything
currently overhead.

See [`docker-compose.yml`](docker-compose.yml) for a fuller example and
[`config.example.yaml`](config.example.yaml) for every setting.

### Alert priority rules

The YAML-only `rules` list matches database aircraft by `icao`, `reg`, `operator`,
`type`, `icao_type`, `cmpg`, `category`, or `tags`. Values within one field are ORed,
fields are ANDed, and the first matching rule wins. Matches ignore case and surrounding
space but must be exact—not substrings. Without `rules`, every database aircraft alerts at
`ntfy.priority`. With `rules`, only matching aircraft alert; set `priority: 0` to mute a
match. Put specific exceptions before broad rules.

Use `squawk_priority` to override priority for emergency codes `7500`, `7600`, and
`7700`; `0` disables that code. Emergency squawks are independent of rules either way:
an aircraft excluded or muted by rules still alerts when it broadcasts an enabled one.

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
| `SKY_NTFY_PRIORITY` | `3` | 1–5; database alerts use this when `rules` is empty |
| `SKY_SOURCE_POLL_INTERVAL` | `15s` | |
| `SKY_SOURCE_MAX_AGE` | `60s` | how stale `aircraft.json` may be before the feed counts as dead |
| `SKY_COOLDOWN` | `24h` | how long to stay quiet about an aircraft after alerting |
| `SKY_DB_REFRESH_INTERVAL` | `24h` | |
| `SKY_TAR1090_URL` | — | makes notifications click through to your map |
| `SKY_ALERT_ON_EMERGENCY_SQUAWK` | `true` | 7500 / 7600 / 7700, listed or not |
| `SKY_FILTERS_LAT` / `_LON` | — | your receiver; needed only for the distance filter |
| `SKY_FILTERS_MAX_DISTANCE_NM` | `0` (off) | |
| `SKY_FILTERS_MIN_ALTITUDE_FT` / `_MAX_ALTITUDE_FT` | `0` (off) | |
| `SKY_LOG_LEVEL` | `info` | |
| `SKY_LISTEN` | `:8080` | serves `/healthz` |
| `SKY_CONFIG` | `/config/config.yaml` | |
| `SKY_ALERTS_CONFIG` | `alerts.yaml` beside `SKY_CONFIG` | |

A typo'd YAML key or an unrecognised `SKY_*` variable is a **startup error**, not a
silent fallback to the default. `SKY_` is this service's namespace, and
`SKY_COOLDWON=1h` quietly leaving you on 24h is exactly the bug worth failing loudly on.

## Things worth knowing

`config.yaml` is read at startup. `alerts.yaml` is re-read every 5 seconds, so every
setting in it updates live. A key placed in the wrong file is a startup error that names
the file it belongs in.

**Filters are off by default, and they fail closed.** The interesting-aircraft list — and
your `rules` allowlist, once you write one — already narrow what alerts; a default radius
would silently suppress the alerts you installed this for. If you *do* enable one, an
aircraft whose data can't answer it is suppressed rather than admitted: `max_distance_nm`
hides Mode-S-only traffic that broadcasts no position, which is frequently the military
traffic you wanted. Emergency squawks bypass every filter.

**Delivery is at-least-once.** Publishing to ntfy and recording the cooldown can't be
made atomic, so a crash in the gap between them re-alerts that aircraft on restart. The
gap is a synchronous `fsync`'d write, so it's small — but it isn't zero, and pretending
otherwise would be the actual defect.

**Cooldowns are per aircraft *and* per trigger.** A routine sighting of a listed airframe
never mutes a later emergency squawk from the same one, and a 7600 doesn't mute a
subsequent 7700. If a listed aircraft squawks an emergency you get one notification, not
two.

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
go test -race ./...   # 44 tests, no framework
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
