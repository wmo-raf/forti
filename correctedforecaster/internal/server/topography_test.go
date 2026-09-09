package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metno/forti/correctedforecaster/internal/download"
	"github.com/metno/forti/internal/internalprotocol"
)

// The fixtures are ESRI ASCII grids, which are plain text and so need no GDAL
// to write. Each carries a .prj naming EPSG:4326, without which the dataset
// has no projection and topo_transformation has nothing to build a transform
// from. The .prj is a sidecar: it has to be on disk beside the grid, and must
// not be handed to GDAL as a dataset in its own right.
const wgs84 = `GEOGCS["GCS_WGS_1984",DATUM["D_WGS_1984",` +
	`SPHEROID["WGS_1984",6378137.0,298.257223563]],PRIMEM["Greenwich",0.0],` +
	`UNIT["Degree",0.0174532925199433]]`

// writeGrid writes a raster of the given constant altitude covering
// [west, west+cols*cell] x [south, south+rows*cell] in degrees.
func writeGrid(t *testing.T, path string, west, south, cell float64, cols, rows int, altitude float64) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ncols %d\nnrows %d\nxllcorner %g\nyllcorner %g\ncellsize %g\nNODATA_value -9999\n",
		cols, rows, west, south, cell)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			fmt.Fprintf(&b, "%g ", altitude)
		}
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	prj := strings.TrimSuffix(path, filepath.Ext(path)) + ".prj"
	if err := os.WriteFile(prj, []byte(wgs84), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A default set over 0-20E, 50-70N at 100 m, and one named set over a small
// patch of it at 500 m. Oslo, 59.93N 10.72E, is inside both; the point at
// 55N 5E is inside the default only.
func testServer(t *testing.T) *Server {
	t.Helper()

	dir := t.TempDir()
	writeGrid(t, filepath.Join(dir, "10-global.asc"), 0, 50, 1, 20, 20, 100)
	writeGrid(t, filepath.Join(dir, "consumer-a", "01-fine.asc"), 10, 59, 0.5, 4, 4, 500)

	grouped, err := download.Get(context.Background(), "", dir)
	if err != nil {
		t.Fatal(err)
	}
	topo, topoSets, err := openTopography(grouped)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{topo: topo, topoSets: topoSets}
}

const (
	osloLat, osloLon         = 59.93, 10.72 // covered by both sets
	offshoreLat, offshoreLon = 55.0, 5.0    // covered by the default set only
	saharaLat, saharaLon     = 20.0, 5.0    // covered by neither
)

func altitudeAt(t *testing.T, s *Server, sets []string, lat, lon float64) *float32 {
	t.Helper()

	got, err := s.lookupTopography(sets, lat, lon)
	if err != nil {
		t.Fatalf("lookupTopography(%v): %s", sets, err)
	}
	return got
}

// The regression that matters most: a request naming no set gets the files at
// the root, which for a directory with no subdirectories is every file there
// is -- exactly what was served before sets existed.
func TestLookupWithNoSetUsesTheDefault(t *testing.T) {
	s := testServer(t)

	for _, sets := range [][]string{nil, {}} {
		got := altitudeAt(t, s, sets, osloLat, osloLon)
		if got == nil || *got != 100 {
			t.Errorf("sets %#v: got %v, want the default set's 100", sets, got)
		}
	}
}

// A named set is consulted before the default, so a consumer's own finer data
// wins where it has any.
func TestLookupNamedSetOverridesTheDefault(t *testing.T) {
	s := testServer(t)

	got := altitudeAt(t, s, []string{"consumer-a"}, osloLat, osloLon)
	if got == nil || *got != 500 {
		t.Errorf("got %v, want consumer-a's 500", got)
	}
}

// Outside its own coverage a named set falls through to the default, so a
// consumer supplies only its own fine data and gets the global one underneath
// it for free.
func TestLookupNamedSetFallsThroughToTheDefault(t *testing.T) {
	s := testServer(t)

	got := altitudeAt(t, s, []string{"consumer-a"}, offshoreLat, offshoreLon)
	if got == nil || *got != 100 {
		t.Errorf("got %v, want the default set's 100", got)
	}
}

// An unrecognised name yields no topography at all -- not the default, which
// the caller did not ask for and could not detect having been given, and not
// an error, which would turn a typo into an outage.
func TestLookupUnknownSetFindsNothing(t *testing.T) {
	s := testServer(t)

	for _, sets := range [][]string{{"nope"}, {"consumer-a", "nope"}, {""}} {
		if got := altitudeAt(t, s, sets, osloLat, osloLon); got != nil {
			t.Errorf("sets %#v: got %v, want nothing at all", sets, *got)
		}
	}
}

// One unrecognised name takes the recognised ones with it. Skipping just the
// unknown one would answer ["consumer-a", "nope"] exactly like
// ["consumer-a"], which is the "ignore unknown names" behaviour the design
// rejects -- a typo that leaves no trace whenever the sets that did resolve
// happen to cover the point.
func TestLookupUnknownSetDiscardsTheKnownOnesWithIt(t *testing.T) {
	s := testServer(t)

	// consumer-a covers Oslo, so skipping "nope" would have found 500 here.
	if got := altitudeAt(t, s, []string{"consumer-a", "nope"}, osloLat, osloLon); got != nil {
		t.Errorf("got %v, want nothing at all", *got)
	}
	// And the same names without the typo do find it, so the test above is
	// not just describing a lookup that could never have succeeded.
	if got := altitudeAt(t, s, []string{"consumer-a"}, osloLat, osloLon); got == nil || *got != 500 {
		t.Errorf("got %v, want consumer-a's 500", got)
	}
}

// A point no set covers finds nothing, which is the case the unknown-set
// decision degrades to on purpose.
func TestLookupOutsideEverythingFindsNothing(t *testing.T) {
	s := testServer(t)

	if got := altitudeAt(t, s, nil, saharaLat, saharaLon); got != nil {
		t.Errorf("got %v, want nothing at all", *got)
	}
}

// The end of the decision: an unknown set has to leave the forecast
// uncorrected, reporting the model's own altitude, rather than correcting it
// towards terrain the caller did not ask for.
func TestUnknownSetLeavesTheForecastUncorrected(t *testing.T) {
	s := testServer(t)

	// Oslo sits at 100 m in the default set and 500 m in consumer-a, against
	// a model altitude of 1000 m -- both far enough to correct by.
	const modelAltitude = 1000

	uncorrected := correctAt(t, s, []string{"nope"}, modelAltitude)
	if uncorrected.altitude != modelAltitude {
		t.Errorf("altitude reported as %v, want the model's own %v", uncorrected.altitude, modelAltitude)
	}
	if uncorrected.temperature != 2 {
		t.Errorf("temperature corrected to %v, want the upstream's 2", uncorrected.temperature)
	}

	// The same request naming a set that does exist is corrected, so the test
	// above is not just describing a request that could never correct.
	corrected := correctAt(t, s, []string{"consumer-a"}, modelAltitude)
	if corrected.altitude != 500 {
		t.Errorf("altitude reported as %v, want consumer-a's 500", corrected.altitude)
	}
	if corrected.temperature == 2 {
		t.Error("temperature was not corrected towards consumer-a's terrain")
	}
}

type correctionResult struct {
	altitude    float32
	temperature float32
}

func correctAt(t *testing.T, s *Server, sets []string, modelAltitude float32) correctionResult {
	t.Helper()

	interpreted := map[string]internalprotocol.InterpretedData{
		"altitude":           {Times: make([]time.Time, 1), Values: []float32{modelAltitude}},
		"air_temperature_2m": {Times: make([]time.Time, 1), Values: []float32{2}},
	}
	request := &internalprotocol.GetForecastRequest{
		Latitude:   osloLat,
		Longitude:  osloLon,
		Topography: sets,
	}
	if err := s.correctWithBetterTopography(request, interpreted); err != nil {
		t.Fatalf("correctWithBetterTopography: %s", err)
	}
	return correctionResult{
		altitude:    interpreted["altitude"].Values[0],
		temperature: interpreted["air_temperature_2m"].Values[0],
	}
}
