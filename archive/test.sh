#!/bin/bash
# Runs the archive image against testdata/ and checks inlining, the CHECKPOINT flush,
# idempotence, and the plane dimension. Usage: archive/test.sh [image]   (default: sky-archive)
#
# Fixture days, all for abc123 (and ~c0ffee on the first):
#   2026-01-10 Test Owner, 2026-01-11 New Owner, 2026-01-12 Test Owner, 2026-01-13 Third Owner
set -euo pipefail
cd "$(dirname "$0")"
img=${1:-sky-archive}
# A named volume, not a host directory: the container writes as root, and the host user
# could not delete what it wrote.
lake=sky-archive-test-$$
docker volume create "$lake" >/dev/null
trap 'docker volume rm -f "$lake" >/dev/null' EXIT

run() {
    docker run --rm -v "$PWD/testdata:/testdata:ro" -v "$PWD/testdata/globe_history:/globe_history:ro" -v "$lake:/lake" \
        -e TAR1090_URL=http://tar1090/ -e PLANE_ALERT_CSV_URL=/testdata/plane-alert-db.csv -e ARCHIVE_ONCE=1 "$@"
}
q() { run --entrypoint duckdb "$img" -bail -c ".read lake.sql" "$@"; }
load() {  # load one day by hand, without CHECKPOINT; $2 is the history root (default /globe_history)
    q -c "SET VARIABLE day = '$1'" -c "SET VARIABLE files = '${2:-/globe_history}/${1//-//}/traces/*/trace_full_*.json'" \
      -c ".read day.sql" >/dev/null
}
expect() {
    local got
    got=$(q -list -noheader -c "$1")
    [[ $got == "$2" ]] || { echo "FAIL: $1"$'\n'"  got:  $got"$'\n'"  want: $2" >&2; exit 1; }
    echo "ok: $1"
}
files="SELECT count(*) FROM ducklake_list_files('lake', 'fact_position')"
abc123="SELECT reg, type, description, owner_operator, year, military, pia, listed, alert_category, details_since FROM dim_plane WHERE icao = 'abc123'"

# 1. Load two days by hand, without CHECKPOINT: the rows are queryable but live in the catalog.
#    The 12th repeats the 10th's details, so details_since stays on the 10th.
q -c ".read setup.sql" -c ".read plane_alert.sql" >/dev/null
load 2026-01-10
load 2026-01-12
expect "SELECT count(*) FROM fact_position" 8
expect "$files" 0
expect "$abc123" "G-TEST|TEST|Test Aircraft|Test Owner|2008|true|false|true|Test Category|2026-01-10"

# 2. A full pass backfills the 11th, whose different details are older than the 12th's and
#    must not overwrite them. CHECKPOINT then flushes one file per day.
run "$img"
expect "SELECT count(*) FROM fact_position" 9
expect "$files" 3
expect "SELECT count(*) FROM ducklake_list_files('lake', 'loaded_days')" 0
expect "$abc123" "G-TEST|TEST|Test Aircraft|Test Owner|2008|true|false|true|Test Category|2026-01-10"

# 3. Another pass changes nothing.
run "$img"
expect "SELECT count(*) FROM fact_position" 9
expect "$files" 3
expect "SELECT count(*) FROM loaded_days" 3

# Facts.
expect "SELECT strftime(ts AT TIME ZONE 'UTC', '%H:%M:%S'), lat, alt_ft, baro_rate FROM fact_position WHERE icao = 'abc123' AND NOT on_ground ORDER BY ts LIMIT 1" "08:00:30|51.475|1200|1500"
expect "SELECT count(*) FROM fact_position WHERE on_ground" 1
expect "SELECT flags FROM fact_position WHERE icao = 'abc123' ORDER BY ts OFFSET 4 LIMIT 1" 1
expect "SELECT count(*) FROM fact_visit" 5
expect "SELECT callsign, points, min_alt_ft, max_alt_ft FROM fact_visit WHERE icao = 'abc123' ORDER BY first_seen LIMIT 1" "TEST123|4|1200|3500"

# The dimension: one row per airframe, heard or only listed.
expect "SELECT count(*), count(DISTINCT icao) FROM dim_plane" "3|3"
expect "SELECT icao, listed, reg IS NULL FROM dim_plane ORDER BY icao" $'abc123|true|false\ndef456|true|true\n~c0ffee|false|true'
expect "SELECT count(*) FROM interesting" 4
expect "SELECT replay_url FROM interesting ORDER BY first_seen LIMIT 1" \
    "http://tar1090/?icao=abc123&showTrace=2026-01-10&startTime=08:00:00&endTime=08:01:00"

# A newer day with changed details does update the plane. It lives outside /globe_history so
# the passes above do not load it.
load 2026-01-13 /testdata/later
expect "$abc123" "G-TEST|TEST|Test Aircraft|Third Owner|2008|true|false|true|Test Category|2026-01-13"

# Delisted: still a plane with its readsb details, no longer interesting.
run -e PLANE_ALERT_CSV_URL=/testdata/plane-alert-db-delisted.csv --entrypoint duckdb "$img" -bail \
    -c ".read lake.sql" -c ".read plane_alert.sql" >/dev/null
expect "SELECT listed, alert_category IS NULL, owner_operator FROM dim_plane WHERE icao = 'abc123'" "false|true|Third Owner"
expect "SELECT count(*) FROM interesting" 0
echo PASS
