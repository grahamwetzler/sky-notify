-- Loads one completed UTC day of readsb traces. run.sh sets the `day` and `files` variables.
-- Trace point layout (readsb README-json.md): [secs after timestamp, lat, lon,
-- alt ft | "ground" | null, gs, track, flags, baro rate, details | null, source, ...].
-- The file header carries readsb's aircraft database entry: r, t, desc, ownOp, year, dbFlags.

CREATE TEMP TABLE staged AS
SELECT getvariable('day')::DATE AS day,
       lower(icao) AS icao,
       to_timestamp("timestamp" + (p->>'$[0]')::DOUBLE) AS ts,
       (p->>'$[1]')::DOUBLE AS lat,
       (p->>'$[2]')::DOUBLE AS lon,
       TRY_CAST(p->>'$[3]' AS DOUBLE)::INTEGER AS alt_ft,
       coalesce(p->>'$[3]' = 'ground', false) AS on_ground,
       (p->>'$[4]')::REAL AS gs,
       (p->>'$[5]')::REAL AS track,
       (p->>'$[6]')::UTINYINT AS flags,
       TRY_CAST(p->>'$[7]' AS DOUBLE)::INTEGER AS baro_rate,
       nullif(trim(p->>'$[8].flight'), '') AS callsign,
       nullif(trim(r), '') AS reg,
       nullif(trim(t), '') AS type,
       nullif(trim("desc"), '') AS description,
       nullif(trim(ownOp), '') AS owner_operator,
       TRY_CAST(year AS SMALLINT) AS year,
       dbFlags
FROM (
    SELECT icao, r, t, "desc", ownOp, year, dbFlags, "timestamp", unnest(trace) AS p
    FROM read_json(getvariable('files'), compression = 'gzip',
                   columns = {icao: 'VARCHAR', r: 'VARCHAR', t: 'VARCHAR', "desc": 'VARCHAR', ownOp: 'VARCHAR',
                              year: 'VARCHAR', dbFlags: 'INTEGER', "timestamp": 'DOUBLE', trace: 'JSON[]'})
);

-- The facts, the dimension update and the day's marker commit together, and every write skips
-- a day already marked, so a rerun, or a crash between commit and exit, never loads a day twice.
BEGIN;

INSERT INTO fact_position BY NAME
SELECT day, icao, ts, lat, lon, alt_ft, on_ground, gs, track, flags, baro_rate, callsign
FROM staged
WHERE getvariable('day')::DATE NOT IN (FROM loaded_days);

-- A visit ends after 10 minutes without a point, matching how long a gap tar1090 draws as one
-- track. Visits never cross UTC midnight because readsb's traces don't: one file per day.
INSERT INTO fact_visit BY NAME
WITH gaps AS (
    SELECT *, coalesce(ts - lag(ts) OVER (PARTITION BY icao ORDER BY ts) > INTERVAL 10 MINUTE, false)::INTEGER AS brk
    FROM staged
), numbered AS (
    SELECT *, sum(brk) OVER (PARTITION BY icao ORDER BY ts) AS visit FROM gaps
)
SELECT day, icao,
       arg_min(callsign, ts) FILTER (WHERE callsign IS NOT NULL) AS callsign,
       min(ts) AS first_seen,
       max(ts) AS last_seen,
       min(alt_ft) FILTER (WHERE NOT on_ground) AS min_alt_ft,
       max(alt_ft) AS max_alt_ft,
       count(*)::INTEGER AS points,
       -- Fixed at load time: a later TAR1090_URL change does not rewrite old visits.
       nullif(getenv('TAR1090_URL'), '') || '?icao=' || icao
           || '&showTrace=' || strftime(day, '%Y-%m-%d')
           || '&startTime=' || strftime(min(ts) AT TIME ZONE 'UTC', '%H:%M:%S')
           || '&endTime=' || strftime(max(ts) AT TIME ZONE 'UTC', '%H:%M:%S') AS replay_url
FROM numbered
WHERE getvariable('day')::DATE NOT IN (FROM loaded_days)
GROUP BY day, icao, visit;

-- Type 1: the newest day's details win. A row is rewritten only when they changed, and never
-- from a day older than the details it holds, so a backfill cannot roll a plane back.
MERGE INTO dim_plane d
USING (
    SELECT icao, day,
           any_value(reg) AS reg, any_value(type) AS type, any_value(description) AS description,
           any_value(owner_operator) AS owner_operator, any_value(year) AS year,
           any_value(dbFlags & 1 > 0) AS military, any_value(dbFlags & 2 > 0) AS interesting,
           any_value(dbFlags & 4 > 0) AS pia, any_value(dbFlags & 8 > 0) AS ladd
    FROM staged
    WHERE getvariable('day')::DATE NOT IN (FROM loaded_days)
    GROUP BY icao, day
) s ON d.icao = s.icao
WHEN MATCHED AND (d.details_since IS NULL OR s.day > d.details_since)
     AND row(d.reg, d.type, d.description, d.owner_operator, d.year, d.military, d.interesting, d.pia, d.ladd)
         IS DISTINCT FROM row(s.reg, s.type, s.description, s.owner_operator, s.year, s.military, s.interesting, s.pia, s.ladd)
    THEN UPDATE SET reg = s.reg, type = s.type, description = s.description, owner_operator = s.owner_operator,
                    year = s.year, military = s.military, interesting = s.interesting, pia = s.pia, ladd = s.ladd,
                    details_since = s.day
WHEN NOT MATCHED
    THEN INSERT (icao, reg, type, description, owner_operator, year, military, interesting, pia, ladd, details_since, listed)
         VALUES (s.icao, s.reg, s.type, s.description, s.owner_operator, s.year, s.military, s.interesting, s.pia, s.ladd, s.day, false);

SELECT CASE WHEN count(*) <> count(DISTINCT icao) THEN error('dim_plane: duplicate icao') END FROM dim_plane;

INSERT INTO loaded_days
SELECT getvariable('day')::DATE WHERE getvariable('day')::DATE NOT IN (FROM loaded_days);

COMMIT;
