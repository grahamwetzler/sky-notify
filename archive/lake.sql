-- Attaches the archive. Every other script, and every ad-hoc session, starts here:
--   docker compose exec sky-archive duckdb -init /archive/lake.sql
ATTACH 'ducklake:sqlite:/lake/catalog.sqlite' AS lake (DATA_PATH '/lake/data/');
USE lake;
