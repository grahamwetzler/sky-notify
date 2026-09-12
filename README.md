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

The `/data` volume matters: it holds the cached aircraft list and the alert cooldown
ledger. Without it you re-download the list on every restart and re-alert on everything
currently overhead.

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

The flight path conditions read the same recent motion. `passes_within_nm` predicts the
closest approach to your `lat`/`lon` along the aircraft's current ground track and speed,
within `passes_within` (default `5m`), so `passes_within_nm: 1` means "will fly over me in
the next five minutes". `circling: true` matches an aircraft that has turned a full circle
inside 2 NM during the last 10 minutes, which is how news and police helicopters fly. It
needs several minutes of positions before it can match, and that history is held in
memory only, so a restart starts it again. It needs `source.poll_interval` of 30s or less.

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
| `SKY_LAT` / `SKY_LON` | — | your receiver; needed by rules with `max_distance_nm` or `passes_within_nm` |
| `SKY_LOG_LEVEL` | `info` | |
| `SKY_LISTEN` | `:8080` | serves `/healthz` |
| `SKY_CONFIG` | `/config/config.yaml` | |
| `SKY_ALERTS_CONFIG` | `alerts.yaml` beside `SKY_CONFIG` | |

A typo'd YAML key or an unrecognised `SKY_*` variable is a **startup error**, not a
silent fallback to the default. `SKY_` is this service's namespace, and
`SKY_COOLDWON=1h` quietly leaving you on 24h is exactly the bug worth failing loudly on.

### Alert settings web UI

Browse to the configured listen address to edit every setting in `alerts.yaml`. Rules are
built from the database's own vocabulary rather than typed, and each rule previews what it
selects — a typo'd tag is otherwise silent, matching nothing and losing every alert the
rule was meant to catch.

The preview searches the database, so it answers which aircraft a rule *can* select, not
which would alert this second. The altitude, distance and flight path conditions describe
where an aircraft is right now, and `squawk` comes off the live feed; no database row can
answer any of them, so they do not narrow the count — the page says so on each condition
that behaves this way. `squawk` offers the three emergency codes as quick selects, and
takes any other code typed in.

`reg`, `icao` and `icao_type` accept any value, whether or not the database has it —
`reg` and `icao_type` are matched against the database *and* the live feed, so a type code
no listed aircraft carries still alerts the moment one broadcasts it. The page suggests
what the database holds and marks a value it does not, but never refuses one.

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
`alt_baro`, one stating `max_distance_nm` does not match without a position, and one with
`passes_within_nm` does not match without a ground track when the aircraft is moving. So
state one only when you mean it: `max_distance_nm` hides the Mode-S-only traffic that is
often exactly what the rule was written for.

**Delivery is at-least-once.** Publishing to ntfy and recording the cooldown can't be
made atomic, so a crash in the gap between them re-alerts that aircraft on restart. The
gap is a synchronous `fsync`'d write, so it's small — but it isn't zero, and pretending
otherwise would be the actual defect.

**Cooldowns are per aircraft *and* rule name.** Two rules that match the same aircraft
have independent cooldowns, while only the first matching rule alerts in each poll.

**A stale list is served forever.** If the refresh fails, sky-notify keeps using the
cached list and warns — an airframe that was interesting last month still is. What it
won't do is start up with no list at all: a cold start that can't obtain one exits
non-zero rather than sitting there looking healthy while matching nothing.

**`/healthz` reports more than "the process is up."** It goes 503 if the feed has gone
stale (checked against `aircraft.json`'s own timestamp, so a frozen-but-valid file from a
dead feeder is caught), if notifications are failing, or if the cooldown ledger can't be
written. Docker restart policies do *not* restart a container for being unhealthy, so
treat this as observability — or wire it to something that does act on it.

## History and replay

sky-notify forgets an aircraft minutes after it leaves. To keep every aircraft's flight path,
so you can go back to an aircraft you missed or replay a flight that turns out to matter,
run the optional **sky-archive** container next to it. It does not change sky-notify.

- **readsb records every flight path.** With `READSB_ENABLE_TRACES=true`, ultrafeeder
  writes one gzipped trace per aircraft per UTC day to `/var/globe_history`, and tar1090
  can replay any of them for `MAX_GLOBE_HISTORY` days. A trace gets a point at least every
  15 seconds, and more often in turns, climbs, descents and at takeoff or landing.
- **sky-archive keeps them for good.** Every 6 hours it loads each completed UTC day into
  a [DuckLake](https://ducklake.select/): Parquet files partitioned by day, with a SQLite
  catalog, all on the `/lake` volume.

The archive is a star schema. It covers every aircraft readsb hears, not only listed ones:

| Table | Grain | Contents |
|---|---|---|
| `dim_plane` | one row per airframe, keyed by `icao` | readsb's aircraft database (registration, type, description, owner, year, military/interesting/PIA/LADD), plus plane-alert-db's columns (`alert_*`) and `listed`. The latest values win; `details_since` is the day readsb's details last changed. Listed aircraft that were never heard are included. |
| `fact_position` | one row per trace point | `day`, `icao`, `ts`, position, altitude, speed, track, climb rate, readsb `flags`, callsign |
| `fact_visit` | one row per pass (a 10-minute gap starts a new one) | `day`, `icao`, callsign, first and last seen, altitude range, points, tar1090 `replay_url` |
| `interesting` (view) | | `fact_visit` joined to `dim_plane`, listed aircraft only |

Enable both in [`docker-compose.yml`](docker-compose.yml): the two ultrafeeder settings
and the `sky-archive` service share the `globe-history` volume.

### Storage

These are estimates for one feeder. Check yours with `du -sh` on a day directory.

| Store | Per day | Kept | Total |
|---|---|---|---|
| readsb traces and heatmap | 20–50 MB, a few thousand files | `MAX_GLOBE_HISTORY` (90 days) | 2–4.5 GB |
| sky-archive positions (zstd Parquet) | 5–15 MB, one file | forever | 2–5 GB a year |

**The archive does not create many small files.** A table's inserts below its inlining limit
(50,000 rows for the fact tables) go into the catalog, not into Parquet. Each pass
ends with `CHECKPOINT`, which writes them out as one file per day. A full day of positions
is larger than the limit, so it is written as one file directly. The `loaded_days` marker
table is never written out. `CHECKPOINT` also expires snapshots after 7 days and deletes
files a day after nothing uses them. Until then you can query the lake as it was, for
example to look past a bad load. If the catalog grows too large or too many files appear,
change the limits in [`archive/setup.sql`](archive/setup.sql).

**A day is loaded once, after it is complete.** sky-archive waits `SETTLE_HOURS` (default 2)
after UTC midnight, then loads the day and records it in `loaded_days` in the same
transaction. A day that fails to load is logged and tried again on the next pass. The raw
traces stay available for `MAX_GLOBE_HISTORY` days, so there is time to fix the cause.

### Queries

```sh
docker compose exec sky-archive duckdb -init /archive/lake.sql
```

Interesting aircraft on a day, with links that open the flight in tar1090:

```sql
SELECT first_seen, icao, reg, alert_operator, alert_category, replay_url
FROM interesting WHERE day = DATE '2026-09-10' ORDER BY first_seen;
```

Every military aircraft seen in the last week, listed or not:

```sql
SELECT p.icao, p.reg, p.type, p.owner_operator, count(*) AS visits, max(v.last_seen) AS last_seen
FROM fact_visit v JOIN dim_plane p USING (icao)
WHERE p.military AND v.day >= current_date - 7
GROUP BY ALL ORDER BY last_seen DESC;
```

Aircraft that came within 2 NM of a point:

```sql
SET VARIABLE lat = 51.5007;
SET VARIABLE lon = -0.1246;
SELECT f.icao, p.reg, p.type, any_value(f.callsign) AS callsign, min(f.ts) AS first, max(f.ts) AS last
FROM fact_position f JOIN dim_plane p USING (icao)
WHERE f.day = DATE '2026-09-10'
  AND 2 * 3440.065 * asin(sqrt(pow(sin(radians(f.lat - getvariable('lat')) / 2), 2)
      + cos(radians(getvariable('lat'))) * cos(radians(f.lat)) * pow(sin(radians(f.lon - getvariable('lon')) / 2), 2))) < 2
GROUP BY ALL;
```

Aircraft that circled, which is roughly the `circling` rule applied to a past day. It
looks for a full turn in a 10-minute window, inside a box about 4 NM across:

```sql
WITH d AS (
    SELECT icao, ts, lat, lon, time_bucket(INTERVAL 10 MINUTE, ts) AS w,
           CASE WHEN ts - lag(ts) OVER p <= INTERVAL 60 SECOND
                THEN (track - lag(track) OVER p + 540) % 360 - 180 END AS turn
    FROM fact_position
    WHERE day = DATE '2026-09-10' AND NOT on_ground
    WINDOW p AS (PARTITION BY icao ORDER BY ts)
)
SELECT icao, w, round(sum(turn)) AS turned_deg
FROM d GROUP BY icao, w
HAVING abs(sum(turn)) >= 360
   AND 60 * greatest(max(lat) - min(lat), (max(lon) - min(lon)) * cos(radians(avg(lat)))) <= 4
ORDER BY w;
```

A flight as GeoJSON, for flights older than `MAX_GLOBE_HISTORY` that tar1090 can no longer
replay:

```sql
SELECT json_object('type', 'LineString', 'coordinates', list([lon, lat] ORDER BY ts))
FROM fact_position
WHERE icao = 'abc123' AND ts BETWEEN '2026-09-10 08:00:00Z' AND '2026-09-10 08:30:00Z';
```

The trace format is readsb's own and may change between readsb releases. If it does, loading
fails and the error is logged. sky-archive does not quietly store wrong data.

## Development

```sh
go test -race ./...   # 70 tests, no framework
go vet ./...
docker build -t sky-notify .
docker build -t sky-archive archive/ && archive/test.sh   # needs Docker
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
