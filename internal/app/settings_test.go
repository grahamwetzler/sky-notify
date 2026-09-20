package app

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestSettingsRoundTripAndVersion(t *testing.T) {
	store, err := OpenSettings(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	v, err := store.Put("alerts", `{"cooldown":"1h"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("version after first write = %d, want 1", v)
	}

	data, version, ok, err := store.Get("alerts")
	if err != nil || !ok || data != `{"cooldown":"1h"}` || version != 1 {
		t.Fatalf("Get = %q, %d, %v, %v", data, version, ok, err)
	}

	v, err = store.Put("alerts", `{"cooldown":"2h"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("version after second write = %d, want 2 (bumped even though content changed only once)", v)
	}

	// A write of identical content still bumps the version — the poller compares the
	// counter, not the content, so a touch must look like a change.
	v, err = store.Put("alerts", `{"cooldown":"2h"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v != 3 {
		t.Fatalf("version after identical write = %d, want 3", v)
	}
}

func TestSettingsMissingSectionIsFine(t *testing.T) {
	store, err := OpenSettings(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	data, version, ok, err := store.Get("config")
	if err != nil || ok || data != "" || version != 0 {
		t.Fatalf("Get on a fresh store = %q, %d, %v, %v; want ok=false", data, version, ok, err)
	}
}

// Concurrent successful writes to the same section must report distinct versions: Put
// upserts and reads the version back atomically (a single RETURNING statement), so no
// other write can land between the upsert and the read and hand back a stale number.
func TestSettingsConcurrentPutsGetDistinctVersions(t *testing.T) {
	store, err := OpenSettings(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const n = 20
	versions := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			versions[i], errs[i] = store.Put("alerts", `{"cooldown":"1h"}`)
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		if seen[versions[i]] {
			t.Fatalf("version %d returned by more than one concurrent Put: %v", versions[i], versions)
		}
		seen[versions[i]] = true
	}
}

func TestSettingsGetSections(t *testing.T) {
	store, err := OpenSettings(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Put("config", `{"listen":":8080"}`); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetSections("config", "alerts")
	if err != nil {
		t.Fatal(err)
	}
	if got["config"] != `{"listen":":8080"}` {
		t.Fatalf("config = %q", got["config"])
	}
	if _, ok := got["alerts"]; ok {
		t.Fatalf("alerts present with no row written: %q", got["alerts"])
	}
}

func TestSettingsSectionsAreIndependent(t *testing.T) {
	store, err := OpenSettings(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Put("config", `{"listen":":8080"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := store.Get("alerts"); ok || err != nil {
		t.Fatalf("writing config must not create an alerts row: ok=%v err=%v", ok, err)
	}
	data, _, ok, err := store.Get("config")
	if err != nil || !ok || data != `{"listen":":8080"}` {
		t.Fatalf("Get config = %q, %v, %v", data, ok, err)
	}
}
