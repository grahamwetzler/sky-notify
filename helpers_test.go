package main

import (
	"testing"
)

// ---------- helpers ----------

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := defaultConfig()
	c.Source.URL = "http://source.invalid/data/aircraft.json"
	c.Ntfy.URL = "https://ntfy.invalid"
	c.Ntfy.Topic = "test-topic"
	c.DB.BaseURL = "https://db.invalid/plane-alert-db"
	c.DB.Files = []string{"plane-alert-db.csv"}
	c.CacheDir = t.TempDir()
	// Unreachable by default, like every other endpoint here: a test that wants a route
	// stands up its own server and points this at it.
	c.RouteAPIURL = "http://route.invalid/api/0/routeset"
	return c
}

// requiredEnv is the environment LoadConfig needs before it will start: none of these
// have a default any more. ntfy.topic is left out so tests can supply it either way.
func requiredEnv() []string {
	return []string{
		"SKY_SOURCE_URL=http://source.invalid/data/aircraft.json",
		"SKY_NTFY_URL=https://ntfy.invalid",
		"SKY_DB_BASE_URL=https://db.invalid/plane-alert-db",
		"SKY_DB_FILES=plane-alert-db.csv",
		"SKY_CACHE_DIR=/tmp/sky-notify-test",
	}
}

func mustParse(t *testing.T, csv string) []Plane {
	t.Helper()
	rows, _, err := parseCSV([]byte(csv))
	if err != nil {
		t.Fatalf("parseCSV: %v", err)
	}
	return rows
}

const sampleCSV = `$ICAO,$Registration,$Operator,$Type,$ICAO Type,#CMPG,$Tag 1,$#Tag 2,$#Tag 3,Category,$#Link
ADEB2F,N12345,US Air Force,C-17,C17,Mil,Cargo,Heavy,,Zoomies,https://example.com/a
000004,FAC1282,Colombian Aerospace Force,CASA C-295 M,C295,Mil,Cargo,,,Other Air Forces,ftp://example.com/bad
`
