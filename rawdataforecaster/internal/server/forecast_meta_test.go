package server

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	_ "gocloud.dev/blob/fileblob"

	"github.com/metno/forti/internal/internalprotocol"
	"github.com/metno/forti/rawdataforecaster/internal/server/config"
	"github.com/metno/forti/rawdataforecaster/internal/server/forecast"
)

// The fixtures put group_a around 5N 1E and group_b around 59N 11E, so 59,11
// is a group_b gridpoint and group_a is the far one.
const (
	nearGroupBLat = 59
	nearGroupBLon = 11
)

func testServer(t *testing.T) *forecastServer {
	t.Helper()

	_, filename, _, _ := runtime.Caller(0)
	testPath := filepath.Clean(filepath.Dir(filename) + "/../../test/data")

	f, err := forecast.New(&config.Configuration{
		Source: config.DataSource{Bucket: "file://" + testPath},
		Areas:  []string{"group_a", "group_b"},
		Loader: config.Loader{Type: "memory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &forecastServer{forecast: f}
}

// The response has to say which area answered, and say the same thing the
// request would have used to name it. Forecast.Get is covered in its own
// package; this is the wire field, which is the half a caller can act on.
func TestGetForecastReportsTheAnsweringArea(t *testing.T) {
	s := testServer(t)

	for _, test := range []struct {
		name  string
		areas []string
		want  string
	}{
		{"unfiltered", nil, "group_b"},
		{"named", []string{"group_a"}, "group_a"},
		{"named nearest", []string{"group_a", "group_b"}, "group_b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := s.GetForecast(context.Background(), &internalprotocol.GetForecastRequest{
				Latitude:  nearGroupBLat,
				Longitude: nearGroupBLon,
				Areas:     test.areas,
			})
			if err != nil {
				t.Fatalf("GetForecast: %s", err)
			}
			if got.ForecastStatus != internalprotocol.ForecastStatus_OK {
				t.Fatalf("status is %s, want OK", got.ForecastStatus)
			}
			if got.ForecastMeta.Area != test.want {
				t.Errorf("area is %q, want %q", got.ForecastMeta.Area, test.want)
			}
		})
	}
}

// A filter naming only areas the process does not have reaches the caller as
// OutsideAllGrids -- an existing status, no new enum value -- and reports no
// area, because none answered.
func TestGetForecastWithUnloadedAreasIsOutsideAllGrids(t *testing.T) {
	s := testServer(t)

	got, err := s.GetForecast(context.Background(), &internalprotocol.GetForecastRequest{
		Latitude:  nearGroupBLat,
		Longitude: nearGroupBLon,
		Areas:     []string{"group_c"},
	})
	if err != nil {
		t.Fatalf("GetForecast: %s", err)
	}
	if got.ForecastStatus != internalprotocol.ForecastStatus_OutsideAllGrids {
		t.Errorf("status is %s, want OutsideAllGrids", got.ForecastStatus)
	}
	if got.ForecastMeta.Area != "" {
		t.Errorf("area is %q, want nothing: no area answered", got.ForecastMeta.Area)
	}
}
