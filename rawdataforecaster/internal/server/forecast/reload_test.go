package forecast

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"

	internalformat "github.com/metno/forti-internalformat"

	"github.com/metno/forti/rawdataforecaster/internal/server/config"
)

func testForecast(t *testing.T, areas ...string) *Forecast {
	t.Helper()

	f, err := newFromClient(testClient(t, nil), testConfig(areas...))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// testClient serves the checked-in fixtures, optionally with extra latest/
// markers written over a copy of them.
//
// The markers are the point: a marker naming a version nobody uploaded is a
// state the bucket really can be in, and it has to stay distinguishable from
// an area that has simply never published. Writing it into a copy keeps a
// deliberately broken area out of the fixture tree, which run.sh regenerates.
func testClient(t *testing.T, markers map[string]int) internalformat.Client {
	t.Helper()

	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Clean(filepath.Dir(filename) + "/../../../test/data")

	if len(markers) > 0 {
		copied := t.TempDir()
		if err := os.CopyFS(copied, os.DirFS(dir)); err != nil {
			t.Fatal(err)
		}
		for area, version := range markers {
			marker := filepath.Join(copied, "latest", area)
			if err := os.WriteFile(marker, []byte(strconv.Itoa(version)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		dir = copied
	}

	bucket, err := blob.OpenBucket(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	return internalformat.NewClientFromBucket(bucket)
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
	if _, err := f.Get(59, 11, nil); err != nil {
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
	if _, err := f.Get(59, 11, nil); err == nil {
		t.Error("expected an error with no areas loaded")
	}
}

// A configured area that has never published has no latest/ marker, and a Go
// map miss reads as version 0. Queueing it for load made GetMeta(area, 0) fail
// and update report "unable to load some areas" — which at startup is
// log.Fatalf, so one name in the config took every other area down with it.
func TestUpdateSkipsAConfiguredAreaThatHasNeverPublished(t *testing.T) {
	f, err := newFromClient(testClient(t, nil), testConfig("group_a", "never_published"))
	if err != nil {
		t.Fatalf("an unpublished area failed the whole load: %s", err)
	}

	if !f.has("group_a") {
		t.Errorf("group_a was not loaded: %v", f.loadedAreas())
	}
	if f.has("never_published") {
		t.Errorf("an area with no marker was loaded: %v", f.loadedAreas())
	}

	// It stays skipped rather than being retried into an error every poll.
	if err := f.update(); err != nil {
		t.Errorf("a later poll errored on the unpublished area: %s", err)
	}
}

// The skip is for a missing marker only. A marker naming a version whose data
// is not in the bucket is a genuine failure and must still be reported, or the
// fix would turn every load error into silence.
func TestUpdateStillFailsWhenAMarkerNamesMissingData(t *testing.T) {
	markers := map[string]int{"dangling": 3}

	if _, err := newFromClient(testClient(t, markers), testConfig("group_a", "dangling")); err == nil {
		t.Error("expected an error for a marker pointing at data that is not there")
	}
}
