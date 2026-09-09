package forecast

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	internalformat "github.com/metno/forti-internalformat"
)

func statusFor(t *testing.T, f *Forecast, area string) AreaStatus {
	t.Helper()
	for _, status := range f.Status().Areas {
		if status.Area == area {
			return status
		}
	}
	t.Fatalf("%q is not in the status: %+v", area, f.Status().Areas)
	return AreaStatus{}
}

func mustVersion(t *testing.T, v *int) int {
	t.Helper()
	if v == nil {
		t.Fatal("expected a version, got none")
	}
	return *v
}

// The three states have to be told apart, which is the whole reason this
// exists: a configuration that applied cleanly says nothing about whether the
// areas it names are being served.
func TestAreaStatusReportsWhatIsResident(t *testing.T) {
	f, err := newFromClient(testClient(t, nil), testConfig("group_a", "never_published"))
	if err != nil {
		t.Fatal(err)
	}

	served := statusFor(t, f, "group_a")
	if got := mustVersion(t, served.Available); got != 1 {
		t.Errorf("group_a available: got %d, want 1", got)
	}
	if got := mustVersion(t, served.Loaded); got != 1 {
		t.Errorf("group_a loaded: got %d, want 1", got)
	}

	// Configured, no marker: nobody has published it. Not an error, and not
	// something to page anyone about, but it must not look like it is being
	// served.
	unpublished := statusFor(t, f, "never_published")
	if unpublished.Available != nil || unpublished.Loaded != nil {
		t.Errorf("an area with no marker reports %+v", unpublished)
	}
}

// The case the status file exists for: a marker says there is data and the
// process is not serving it. From the configuration status alone this is
// indistinguishable from a healthy service.
func TestAreaStatusShowsAPublishedAreaThatIsNotLoaded(t *testing.T) {
	f, err := newFromClient(testClient(t, map[string]int{"dangling": 3}), testConfig("group_a"))
	if err != nil {
		t.Fatal(err)
	}

	f.SetConfig(testConfig("group_a", "dangling"))
	if err := f.update(); err == nil {
		t.Fatal("expected the dangling marker to fail its load")
	}

	dangling := statusFor(t, f, "dangling")
	if got := mustVersion(t, dangling.Available); got != 3 {
		t.Errorf("dangling available: got %d, want 3", got)
	}
	if dangling.Loaded != nil {
		t.Errorf("dangling reports loaded %d, want nothing", *dangling.Loaded)
	}
}

// A dropped area leaves the status the moment it leaves the configuration:
// the list answers "what is this process meant to be doing, and is it", so an
// area nobody asked for has no row.
func TestAreaStatusFollowsTheConfiguration(t *testing.T) {
	f := testForecast(t, "group_a", "group_b")

	f.SetConfig(testConfig("group_a"))

	if len(f.Status().Areas) != 1 {
		t.Fatalf("expected one area, got %+v", f.Status().Areas)
	}
	if f.Status().Areas[0].Area != "group_a" {
		t.Errorf("unexpected area: %+v", f.Status().Areas)
	}
}

// A load failure has to name the area that failed. The message is the only
// account of it anywhere: update returns one error for the whole round, and
// with one process holding every area, "something did not load" is not a
// starting point for anybody.
func TestUpdateLogsTheAreaThatActuallyFailed(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// Started with nothing loaded, so all three areas download in the same
	// round: the misattribution needed more than one in flight at once.
	f, err := newFromClient(testClient(t, map[string]int{"dangling": 3}), testConfig())
	if err != nil {
		t.Fatal(err)
	}

	f.SetConfig(testConfig("group_a", "group_b", "dangling"))
	if err := f.update(); err == nil {
		t.Fatal("expected the dangling marker to fail its load")
	}

	if !strings.Contains(logged.String(), "unable to load dangling/3") {
		t.Errorf("the failing area is not in the log:\n%s", logged.String())
	}
	for _, loaded := range []string{"unable to load group_a", "unable to load group_b"} {
		if strings.Contains(logged.String(), loaded) {
			t.Errorf("an area that loaded was blamed for the failure:\n%s", logged.String())
		}
	}
}

// A store nobody can reach any more is the one failure that stops every area
// ever being updated again, and it is the one the area list cannot show:
// Available is the last listing that worked, so on its own the report gets
// healthier the longer the outage lasts.
func TestStatusReportsAStoreItCannotList(t *testing.T) {
	f := testForecast(t, "group_a")

	if store := f.Status().StoreError; store != "" {
		t.Fatalf("a working store reported %q", store)
	}

	f.store = unreachableStore{}
	if err := f.update(); err == nil {
		t.Fatal("expected the listing to fail")
	}

	status := f.Status()
	if status.StoreError == "" {
		t.Error("the status does not say the store could not be listed")
	}
	// The area keeps serving and keeps reporting what it has. Losing the
	// listing is not losing the data.
	if got := mustVersion(t, status.Areas[0].Loaded); got != 1 {
		t.Errorf("group_a loaded: got %d, want 1", got)
	}
}

// And it clears when the store comes back, or the first blip would read as an
// outage for the life of the process.
func TestStatusClearsAStoreErrorOnceListingWorks(t *testing.T) {
	f := testForecast(t, "group_a")
	working := f.store

	f.store = unreachableStore{}
	if err := f.update(); err == nil {
		t.Fatal("expected the listing to fail")
	}

	f.store = working
	if err := f.update(); err != nil {
		t.Fatal(err)
	}

	if store := f.Status().StoreError; store != "" {
		t.Errorf("the store error outlived the outage: %q", store)
	}
}

// unreachableStore is a client whose bucket has gone away. Only Latest is
// reached: update gives up on it before it asks for anything else.
type unreachableStore struct {
	internalformat.Client
}

func (unreachableStore) Latest(context.Context) (map[string]int, error) {
	return nil, errors.New("dial tcp: connection refused")
}
