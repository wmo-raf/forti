package forecast

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"

	internalformat "github.com/metno/forti-internalformat"

	"github.com/metno/forti/rawdataforecaster/internal/server/config"
)

func testForecast(t *testing.T, areas ...string) *Forecast {
	t.Helper()

	_, filename, _, _ := runtime.Caller(0)
	testPath := filepath.Clean(filepath.Dir(filename) + "/../../../test/data")

	bucket, err := blob.OpenBucket(context.Background(), "file://"+testPath)
	if err != nil {
		t.Fatal(err)
	}

	f, err := newFromClient(
		internalformat.NewClientFromBucket(bucket),
		testConfig(areas...),
	)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func testConfig(areas ...string) *config.Configuration {
	return &config.Configuration{
		Source: config.DataSource{Bucket: "file:///unused"},
		Areas:  areas,
		Loader: config.Loader{Type: "memory"},
	}
}

func (f *Forecast) loadedAreas() []string {
	f.m.RLock()
	defer f.m.RUnlock()

	areas := make([]string, 0, len(f.datasets))
	for area := range f.datasets {
		areas = append(areas, area)
	}
	return areas
}

func (f *Forecast) has(area string) bool {
	for _, loaded := range f.loadedAreas() {
		if loaded == area {
			return true
		}
	}
	return false
}

// Removing an area has to actually stop it being served. update only ever
// adds, so without an explicit delete a dropped area stayed in memory and kept
// answering requests until the process restarted.
func TestSetConfigDropsRemovedAreas(t *testing.T) {
	f := testForecast(t, "group_a", "group_b")
	if len(f.loadedAreas()) != 2 {
		t.Fatalf("expected both areas loaded, got %v", f.loadedAreas())
	}

	f.SetConfig(testConfig("group_a"))

	if f.has("group_b") {
		t.Errorf("group_b is still loaded: %v", f.loadedAreas())
	}
	if !f.has("group_a") {
		t.Errorf("group_a was dropped as well: %v", f.loadedAreas())
	}

	// The one that remains still answers.
	if _, err := f.Get(59, 11); err != nil {
		t.Errorf("forecast stopped working after a reload: %s", err)
	}
}

// Adding an area needs no code of its own: swapping the configuration is
// enough, and the existing poll loop picks it up.
func TestSetConfigAddsAreasOnNextPoll(t *testing.T) {
	f := testForecast(t, "group_a")

	f.SetConfig(testConfig("group_a", "group_b"))

	if f.has("group_b") {
		t.Fatal("group_b appeared before the poll ran")
	}
	if err := f.update(); err != nil {
		t.Fatal(err)
	}
	if !f.has("group_b") {
		t.Errorf("group_b was not picked up: %v", f.loadedAreas())
	}
}

// A download started before an area was removed must not put it back. update
// releases the lock while downloading, so the configuration can change
// underneath a load that is already in flight.
func TestLoadDiscardsAreaRemovedWhileDownloading(t *testing.T) {
	f := testForecast(t, "group_a")

	// Stands in for a download that began while group_b was still configured.
	cfg := testConfig("group_a", "group_b")
	if err := f.load(context.Background(), cfg, "group_b", 2); err != nil {
		t.Fatal(err)
	}

	if f.has("group_b") {
		t.Errorf("a removed area was resurrected by an in-flight download: %v", f.loadedAreas())
	}
}

// Emptying the area list leaves a forecaster with nothing to serve, which has
// to be an ordinary error rather than a panic or a stale answer.
func TestSetConfigToNoAreas(t *testing.T) {
	f := testForecast(t, "group_a")

	f.SetConfig(testConfig())

	if len(f.loadedAreas()) != 0 {
		t.Errorf("expected nothing loaded, got %v", f.loadedAreas())
	}
	if _, err := f.Get(59, 11); err == nil {
		t.Error("expected an error with no areas loaded")
	}
}
