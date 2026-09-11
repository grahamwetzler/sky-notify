#!/bin/bash
# Runs the archive image against testdata/ and checks inlining, the CHECKPOINT flush,
# idempotence, and the plane dimension. Usage: archive/test.sh [image]   (default: sky-archive)
set -euo pipefail
cd "$(dirname "$0")"
img=${1:-sky-archive}
lake=$(mktemp -d)
trap 'rm -rf "$lake"' EXIT

run() {
    docker run --rm -v "$PWD/testdata:/testdata:ro" -v "$PWD/testdata/globe_history:/globe_history:ro" -v "$lake:/lake" \
        -e TAR1090_URL=http://tar1090/ -e PLANE_ALERT_CSV_URL=/testdata/plane-alert-db.csv -e ARCHIVE_ONCE=1 "$@"
}
q() { run --entrypoint duckdb "$img" -bail -c ".read lake.sql" "$@"; }
expect() {
    local got
    got=$(q -list -noheader -c "$1")
    [[ $got == "$2" ]] || { echo "FAIL: $1"$'\n'"  got:  $got"$'\n'"  want: $2" >&2; exit 1; }
    echo "ok: $1"
}
files="SELECT count(*) FROM ducklake_list_files('lake', 'fact_position')"
abc123="SELECT reg, type, description, owner_operator, year, military, pia, listed, alert_category, details_since FROM dim_plane WHERE icao = 'abc123'"

# 1. Load the newer day first, without CHECKPOINT: the rows are queryable but live in the catalog.
q -c ".read setup.sql" -c ".read plane_alert.sql" \
  -c "SET VARIABLE day = '2026-09-11'" \
  -c "SET VARIABLE files = '/globe_history/2026/09/11/traces/*/trace_full_*.json'" -c ".read day.sql" >/dev/null
expect "SELECT count(*) FROM fact_position" 1
expect "$files" 0
expect "$abc123" "G-TEST|TEST|Test Aircraft|New Owner|2008|true|false|true|Test Category|2026-09-11"

# 2. A full pass backfills the older day, then CHECKPOINT flushes one file per day. The older
#    day's details do not overwrite the newer ones.
run "$img"
expect "SELECT count(*) FROM fact_position" 8
expect "$files" 2
expect "SELECT count(*) FROM ducklake_list_files('lake', 'loaded_days')" 0
expect "$abc123" "G-TEST|TEST|Test Aircraft|New Owner|2008|true|false|true|Test Category|2026-09-11"

# 3. Another pass changes nothing.
run "$img"
expect "SELECT count(*) FROM fact_position" 8
expect "$files" 2
expect "SELECT count(*) FROM loaded_days" 2

# Facts.
expect "SELECT strftime(ts AT TIME ZONE 'UTC', '%H:%M:%S'), lat, alt_ft, baro_rate FROM fact_position WHERE icao = 'abc123' AND NOT on_ground ORDER BY ts LIMIT 1" "08:00:30|51.475|1200|1500"
expect "SELECT count(*) FROM fact_position WHERE on_ground" 1
expect "SELECT flags FROM fact_position WHERE icao = 'abc123' ORDER BY ts OFFSET 4 LIMIT 1" 1
expect "SELECT count(*) FROM fact_visit" 4
expect "SELECT callsign, points, min_alt_ft, max_alt_ft FROM fact_visit WHERE icao = 'abc123' ORDER BY first_seen LIMIT 1" "TEST123|4|1200|3500"

# The dimension: one row per airframe, heard or only listed.
expect "SELECT count(*), count(DISTINCT icao) FROM dim_plane" "3|3"
expect "SELECT icao, listed, reg IS NULL FROM dim_plane ORDER BY icao" $'abc123|true|false\ndef456|true|true\n~c0ffee|false|true'
expect "SELECT count(*) FROM interesting" 3
expect "SELECT replay_url FROM interesting ORDER BY first_seen LIMIT 1" \
    "http://tar1090/?icao=abc123&showTrace=2026-09-10&startTime=08:00:00&endTime=08:01:00"

# Delisted: still a plane with its readsb details, no longer interesting.
run -e PLANE_ALERT_CSV_URL=/testdata/plane-alert-db-delisted.csv --entrypoint duckdb "$img" -bail \
    -c ".read lake.sql" -c ".read plane_alert.sql" >/dev/null
expect "SELECT listed, alert_category IS NULL, owner_operator FROM dim_plane WHERE icao = 'abc123'" "false|true|New Owner"
expect "SELECT count(*) FROM interesting" 0
echo PASS
