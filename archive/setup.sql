-- Idempotent schema and settings, run at the start of every pass. A star schema: one plane
-- dimension keyed by ICAO hex, and fact tables holding only measurements and that key.
-- Every column is flat, since a SQLite catalog stores inlined nested types as strings.
-- DuckLake enforces no primary or unique keys: the scripts that write dim_plane keep icao
-- unique, and check it before they commit.

-- One row per airframe, type 1 (latest wins). readsb's aircraft database comes from the trace
-- files; the alert_* columns and `listed` come from plane-alert-db, so listed aircraft that
-- were never heard are members too.
CREATE TABLE IF NOT EXISTS dim_plane (
    icao VARCHAR NOT NULL,  -- 6 hex digits, or ~ + 6 for a non-ICAO address
    reg VARCHAR, type VARCHAR, description VARCHAR, owner_operator VARCHAR, year SMALLINT,
    military BOOLEAN, interesting BOOLEAN, pia BOOLEAN, ladd BOOLEAN,
    details_since DATE,     -- trace day the readsb details above last changed
    listed BOOLEAN,         -- in plane-alert-db
    alert_operator VARCHAR, alert_type VARCHAR, alert_cmpg VARCHAR, alert_category VARCHAR,
    alert_tag1 VARCHAR, alert_tag2 VARCHAR, alert_tag3 VARCHAR, alert_link VARCHAR
);

-- One row per trace point.
CREATE TABLE IF NOT EXISTS fact_position (
    day DATE, icao VARCHAR, ts TIMESTAMPTZ, lat DOUBLE, lon DOUBLE,
    alt_ft INTEGER, on_ground BOOLEAN, gs REAL, track REAL,
    flags UTINYINT,  -- readsb: 1 stale, 2 new leg, 4 geometric rate, 8 geometric altitude
    baro_rate INTEGER,
    callsign VARCHAR -- degenerate dimension: what the flight broadcast, which changes per flight
);

-- One row per pass over the receiver.
CREATE TABLE IF NOT EXISTS fact_visit (
    day DATE, icao VARCHAR, callsign VARCHAR, first_seen TIMESTAMPTZ, last_seen TIMESTAMPTZ,
    min_alt_ft INTEGER, max_alt_ft INTEGER, points INTEGER, replay_url VARCHAR
);

CREATE TABLE IF NOT EXISTS loaded_days (day DATE);

ALTER TABLE fact_position SET PARTITIONED BY (day);

CALL lake.set_option('parquet_compression', 'zstd');
-- Any insert smaller than this is written to the catalog, not to a Parquet file, and CHECKPOINT
-- later flushes it as one file per partition. A completed day of positions is larger, so it
-- lands as one file on its own; a day's visits, dimension changes and deletes stay inlined
-- until then.
-- ponytail: knob; raise it for fewer files, lower it for a smaller catalog.
-- Set per table: re-setting the lake-wide limit on an existing lake segfaults the ducklake
-- extension in DuckDB v1.5.5, and this file runs every pass.
CALL lake.set_option('data_inlining_row_limit', 50000, table_name => 'fact_position');
CALL lake.set_option('data_inlining_row_limit', 50000, table_name => 'fact_visit');
CALL lake.set_option('data_inlining_row_limit', 100000, table_name => 'dim_plane');
-- One row a day: kept inlined for good, since tables without auto_compact are skipped by the flush.
CALL lake.set_option('auto_compact', false, table_name => 'loaded_days');
-- Without these, CHECKPOINT keeps every snapshot and every superseded file forever. A week of
-- snapshots allows time travel back past a bad load; a day's grace protects files that a
-- concurrent ad-hoc query may still be reading. Both are lake-wide and safe to re-set.
CALL lake.set_option('expire_older_than', '7 days');
CALL lake.set_option('delete_older_than', '1 day');

CREATE OR REPLACE VIEW interesting AS
SELECT * FROM fact_visit JOIN dim_plane USING (icao) WHERE listed;
