package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/metno/forti/jsonfrontend/internal/server/config"
)

func requestFor(t *testing.T, query string) (latitude, longitude float32, areas, topography []string) {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
	req, err := getForecastRequest(&config.Elements{}, r)
	if err != nil {
		t.Fatalf("getForecastRequest(%q): %s", query, err)
	}
	return req.Latitude, req.Longitude, req.Areas, req.Topography
}

// The regression that matters most: a request that names neither has to send
// neither, so an upstream sees exactly what it saw before the parameters
// existed.
func TestRequestWithoutSelectionSendsNothing(t *testing.T) {
	lat, lon, areas, topography := requestFor(t, "lat=59.93&lon=10.72")

	if lat != 59.93 || lon != 10.72 {
		t.Errorf("got %v,%v, want 59.93,10.72", lat, lon)
	}
	if len(areas) != 0 {
		t.Errorf("areas is %v, want nothing", areas)
	}
	if len(topography) != 0 {
		t.Errorf("topography is %v, want nothing", topography)
	}
}

// Both parameters repeat, and the order is kept: topography is consulted in
// the order given, and areas reads better repeated than comma-separated next
// to it.
func TestRequestForwardsRepeatedSelection(t *testing.T) {
	_, _, areas, topography := requestFor(t,
		"lat=59.93&lon=10.72&area=meps&area=ecmwf&topography=consumer-a&topography=consumer-b")

	if want := []string{"meps", "ecmwf"}; !slices.Equal(areas, want) {
		t.Errorf("areas is %v, want %v", areas, want)
	}
	if want := []string{"consumer-a", "consumer-b"}; !slices.Equal(topography, want) {
		t.Errorf("topography is %v, want %v", topography, want)
	}
}

// An empty value is a name that matches nothing, not an absent parameter.
// Treating "?area=" as "no filter" would let a caller's bug silently widen
// the query, which is the failure the parameter exists to prevent.
func TestRequestKeepsAnEmptySelectionValue(t *testing.T) {
	_, _, areas, _ := requestFor(t, "lat=59.93&lon=10.72&area=")

	if want := []string{""}; !slices.Equal(areas, want) {
		t.Errorf("areas is %v, want %v", areas, want)
	}
}
