package forecast

import (
	"errors"
	"testing"
)

// The fixtures put group_a around 5N 1E and group_b around 59N 11E, so a
// request at 59,11 sits on a group_b gridpoint and group_a is the far one.
const (
	nearGroupBLat = 59
	nearGroupBLon = 11
)

// The regression that matters most: a request that does not set the field has
// to behave exactly as it did before there was a field, which means picking
// the nearest gridpoint across every loaded area.
func TestGetWithNoFilterSelectsGlobally(t *testing.T) {
	f := testForecast(t, "group_a", "group_b")

	for _, filter := range [][]string{nil, {}} {
		data, err := f.Get(nearGroupBLat, nearGroupBLon, filter)
		if err != nil {
			t.Fatalf("Get with filter %#v: %s", filter, err)
		}
		if data.Area != "group_b" {
			t.Errorf("filter %#v: answered by %q, want the nearest area group_b", filter, data.Area)
		}
	}
}

// The point of the field: a named area answers even when an unnamed one is
// nearer. Without this the filter would be decoration.
func TestGetFilterPrefersNamedAreaOverNearerUnnamedOne(t *testing.T) {
	f := testForecast(t, "group_a", "group_b")

	unfiltered, err := f.Get(nearGroupBLat, nearGroupBLon, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unfiltered.Area != "group_b" {
		t.Fatalf("fixture moved: %q answers 59,11 unfiltered", unfiltered.Area)
	}

	filtered, err := f.Get(nearGroupBLat, nearGroupBLon, []string{"group_a"})
	if err != nil {
		t.Fatalf("Get: %s", err)
	}
	if filtered.Area != "group_a" {
		t.Errorf("answered by %q, want the named group_a", filtered.Area)
	}
}

// Naming several areas narrows to those and still picks the nearest among
// them, so the filter composes with the existing selection rather than
// replacing it.
func TestGetFilterStillPicksNearestAmongNamedAreas(t *testing.T) {
	f := testForecast(t, "group_a", "group_b")

	data, err := f.Get(nearGroupBLat, nearGroupBLon, []string{"group_a", "group_b"})
	if err != nil {
		t.Fatalf("Get: %s", err)
	}
	if data.Area != "group_b" {
		t.Errorf("answered by %q, want the nearer group_b", data.Area)
	}
}

// A name the process has not loaded matches nothing, and a request that
// matches nothing fails closed. Falling back to the unfiltered set would make
// a typo silently widen the query, which is the failure the field exists to
// prevent.
func TestGetFilterNamingOnlyUnloadedAreasIsOutsideAllGrids(t *testing.T) {
	f := testForecast(t, "group_a", "group_b")

	for _, filter := range [][]string{{"group_c"}, {"group_c", "group_d"}, {""}} {
		_, err := f.Get(nearGroupBLat, nearGroupBLon, filter)
		if !errors.Is(err, ErrOutsideAllGrids) {
			t.Errorf("filter %#v: got %v, want ErrOutsideAllGrids", filter, err)
		}
	}
}

// A configured area that has been dropped is no longer loaded, so naming it
// stops working the moment it stops being served rather than quietly falling
// back to whatever is left.
func TestGetFilterNamingADroppedAreaIsOutsideAllGrids(t *testing.T) {
	f := testForecast(t, "group_a", "group_b")

	f.SetConfig(testConfig("group_a"))

	if _, err := f.Get(nearGroupBLat, nearGroupBLon, []string{"group_b"}); !errors.Is(err, ErrOutsideAllGrids) {
		t.Errorf("got %v, want ErrOutsideAllGrids", err)
	}
}

// With nothing loaded at all the unfiltered error stays what it was: a
// process with no data is a different problem from a request that excluded
// everything, and they should not report the same thing.
func TestGetWithNoAreasLoadedKeepsItsOwnError(t *testing.T) {
	f := testForecast(t)

	_, err := f.Get(nearGroupBLat, nearGroupBLon, nil)
	if err == nil {
		t.Fatal("expected an error with no areas loaded")
	}
	if errors.Is(err, ErrOutsideAllGrids) {
		t.Errorf("got ErrOutsideAllGrids, want the no-datasets error: %v", err)
	}
}
