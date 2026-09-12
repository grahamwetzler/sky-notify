#!/bin/bash
# One archive pass every ARCHIVE_INTERVAL seconds: set up the lake, refresh plane-alert-db,
# load each completed UTC day of readsb traces not loaded yet, then CHECKPOINT (flush inlined
# rows, merge files, expire snapshots, delete unreferenced files). ARCHIVE_ONCE=1 runs one pass.
set -u
cd /archive

db() { duckdb -bail -c ".read lake.sql" "$@"; }

pass() {
    # stdout is only result tables; errors still reach the log on stderr.
    db -c ".read setup.sql" >/dev/null || return 1
    if [[ -n ${PLANE_ALERT_CSV_URL:-} ]]; then
        db -c ".read plane_alert.sql" >/dev/null || echo "sky-archive: plane-alert-db refresh failed, keeping previous values" >&2
    fi

    local loaded cutoff dir day
    loaded=" $(db -list -noheader -c "SELECT day FROM loaded_days" | tr '\n' ' ') "
    # readsb finishes a day's trace files shortly after UTC midnight; a day loaded half-written
    # would be marked loaded and never revisited.
    # ponytail: fixed settle time; key off readsb's own completion marker if one ever appears.
    cutoff=$(date -u -d "-${SETTLE_HOURS:-2} hours" +%F)

    for dir in /globe_history/[0-9][0-9][0-9][0-9]/[0-9][0-9]/[0-9][0-9]; do
        day=${dir#/globe_history/}
        day=${day//\//-}
        [[ $day < $cutoff && $loaded != *" $day "* ]] || continue
        compgen -G "$dir/traces/*/trace_full_*.json" >/dev/null || continue
        if db -c "SET VARIABLE day = '$day'" -c "SET VARIABLE files = '$dir/traces/*/trace_full_*.json'" -c ".read day.sql" >/dev/null; then
            echo "sky-archive: loaded $day"
        else
            echo "sky-archive: loading $day failed, will retry next pass" >&2
        fi
    done

    db -c "CHECKPOINT" >/dev/null
}

while :; do
    pass
    status=$?
    ((status == 0)) || echo "sky-archive: pass failed" >&2
    [[ -n ${ARCHIVE_ONCE:-} ]] && exit "$status"
    sleep "${ARCHIVE_INTERVAL:-21600}"
done
