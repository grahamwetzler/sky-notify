-- Refreshes dim_plane's plane-alert-db columns from PLANE_ALERT_CSV_URL. Columns are taken by
-- header name, $/# prefixes included, so an upstream rename fails loudly instead of shifting
-- fields. A fetch or parse failure, or an empty list, keeps the previous values.

CREATE TEMP TABLE alert AS
SELECT DISTINCT ON (icao) *  -- ponytail: an ICAO listed twice keeps an arbitrary row
FROM (
    SELECT lower(trim("$ICAO")) AS icao,
           "$Operator" AS alert_operator, "$Type" AS alert_type, "#CMPG" AS alert_cmpg, "Category" AS alert_category,
           "$Tag 1" AS alert_tag1, "$#Tag 2" AS alert_tag2, "$#Tag 3" AS alert_tag3, "$#Link" AS alert_link
    FROM read_csv(getenv('PLANE_ALERT_CSV_URL'), all_varchar = true, null_padding = true, strict_mode = false)
)
WHERE regexp_full_match(icao, '[0-9a-f]{6}');

SELECT CASE WHEN count(*) = 0 THEN error('plane-alert-db: no usable rows') END FROM alert;

BEGIN;

MERGE INTO dim_plane d
USING alert s ON d.icao = s.icao
WHEN MATCHED AND (d.listed IS NOT TRUE
     OR row(d.alert_operator, d.alert_type, d.alert_cmpg, d.alert_category, d.alert_tag1, d.alert_tag2, d.alert_tag3, d.alert_link)
        IS DISTINCT FROM row(s.alert_operator, s.alert_type, s.alert_cmpg, s.alert_category, s.alert_tag1, s.alert_tag2, s.alert_tag3, s.alert_link))
    THEN UPDATE SET listed = true, alert_operator = s.alert_operator, alert_type = s.alert_type, alert_cmpg = s.alert_cmpg,
                    alert_category = s.alert_category, alert_tag1 = s.alert_tag1, alert_tag2 = s.alert_tag2,
                    alert_tag3 = s.alert_tag3, alert_link = s.alert_link
WHEN NOT MATCHED
    THEN INSERT (icao, listed, alert_operator, alert_type, alert_cmpg, alert_category, alert_tag1, alert_tag2, alert_tag3, alert_link)
         VALUES (s.icao, true, s.alert_operator, s.alert_type, s.alert_cmpg, s.alert_category, s.alert_tag1, s.alert_tag2, s.alert_tag3, s.alert_link);

-- Removed from the list: still a plane, no longer listed.
UPDATE dim_plane
SET listed = false, alert_operator = NULL, alert_type = NULL, alert_cmpg = NULL, alert_category = NULL,
    alert_tag1 = NULL, alert_tag2 = NULL, alert_tag3 = NULL, alert_link = NULL
WHERE listed AND icao NOT IN (SELECT icao FROM alert);

SELECT CASE WHEN count(*) <> count(DISTINCT icao) THEN error('dim_plane: duplicate icao') END FROM dim_plane;

COMMIT;
