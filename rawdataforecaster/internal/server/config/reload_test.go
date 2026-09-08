package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const shipped = `{
    "source": {"bucket": "azblob://collected"},
    "areas": ["nowcast"],
    "loader": {
        "type": "blob",
        "configuration": {"max_size_gib": {"nowcast": 0.2}}
    }
}`

func TestParseAcceptsShippedConfig(t *testing.T) {
	conf, problems := Parse([]byte(shipped))
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if conf.Source.Bucket != "azblob://collected" {
		t.Errorf("unexpected bucket %q", conf.Source.Bucket)
	}
	if !slices.Equal(conf.Areas, []string{"nowcast"}) {
		t.Errorf("unexpected areas %v", conf.Areas)
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	conf, problems := Parse([]byte(`{"areas": }`))
	if len(problems) != 1 {
		t.Fatalf("expected one problem, got %v", problems)
	}
	if conf != nil {
		t.Error("expected no configuration alongside the problem")
	}
}

func TestProblems(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			"no bucket",
			`{"areas": ["a"], "loader": {"type": "blob"}}`,
			"source.bucket",
		},
		{
			// Every request would answer "no datasets available", which looks
			// like a data problem rather than a configuration one.
			"no areas",
			`{"source": {"bucket": "file:///d"}, "areas": [], "loader": {"type": "blob"}}`,
			"areas",
		},
		{
			"empty area name",
			`{"source": {"bucket": "file:///d"}, "areas": ["a", ""], "loader": {"type": "blob"}}`,
			"areas[1]",
		},
		{
			"duplicate area",
			`{"source": {"bucket": "file:///d"}, "areas": ["a", "a"], "loader": {"type": "blob"}}`,
			"listed twice",
		},
		{
			"unknown loader",
			`{"source": {"bucket": "file:///d"}, "areas": ["a"], "loader": {"type": "blobb"}}`,
			"loader.type",
		},
		{
			"max_size_gib as a number",
			`{"source": {"bucket": "file:///d"}, "areas": ["a"],
			  "loader": {"type": "blob", "configuration": {"max_size_gib": 4}}}`,
			"object keyed by area",
		},
		{
			// The bug pattern this codebase keeps repeating: a limit naming an
			// area nobody serves is silently no limit at all.
			"limit for an unserved area",
			`{"source": {"bucket": "file:///d"}, "areas": ["nowcast"],
			  "loader": {"type": "blob", "configuration": {"max_size_gib": {"nowcst": 0.2}}}}`,
			"nowcst",
		},
		{
			"negative distance",
			`{"source": {"bucket": "file:///d"}, "areas": ["a"], "loader": {"type": "blob"},
			  "maximum_gridpoint_distance": -1}`,
			"maximum_gridpoint_distance",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, problems := Parse([]byte(tt.config))
			if len(problems) == 0 {
				t.Fatalf("expected a problem mentioning %q, got none", tt.want)
			}
			if !mentions(problems, tt.want) {
				t.Errorf("no problem mentions %q: %v", tt.want, problems)
			}
		})
	}
}

// A validator that grows stricter than production is how a config manager
// starts refusing to save what is already running.
func TestShippedFilesValidate(t *testing.T) {
	for _, name := range []string{"forecast.json", "nowcast.json", "oceanforecast.json"} {
		t.Run(name, func(t *testing.T) {
			data := readShipped(t, name)
			if _, problems := Parse(data); len(problems) != 0 {
				t.Errorf("%s does not validate: %v", name, problems)
			}
		})
	}
}

// coldBase carries no max_size_gib, so that the area list can be varied
// without tripping the check that every limit names an area being served.
const coldBase = `{
    "source": {"bucket": "azblob://collected"},
    "areas": ["nowcast"],
    "loader": {"type": "blob"}
}`

func TestColdChanges(t *testing.T) {
	base := mustParse(t, coldBase)

	tests := []struct {
		name    string
		changed string
		want    []string
	}{
		{
			"nothing",
			coldBase,
			nil,
		},
		{
			// Adding an area is the whole point of the slice: it must not ask
			// for a restart.
			"area added",
			strings.Replace(coldBase, `["nowcast"]`, `["nowcast", "arctic"]`, 1),
			nil,
		},
		{
			"area replaced",
			strings.Replace(coldBase, `["nowcast"]`, `["arctic"]`, 1),
			nil,
		},
		{
			"bucket changed",
			strings.Replace(coldBase, "azblob://collected", "azblob://other", 1),
			[]string{"source.bucket"},
		},
		{
			"loader type changed",
			strings.Replace(coldBase, `"type": "blob"`, `"type": "memory"`, 1),
			[]string{"loader.type"},
		},
		{
			"loader configuration added",
			strings.Replace(coldBase, `{"type": "blob"}`,
				`{"type": "blob", "configuration": {"max_size_gib": {"nowcast": 0.2}}}`, 1),
			[]string{"loader.configuration"},
		},
		{
			"distance added",
			strings.Replace(coldBase, `"areas"`, `"maximum_gridpoint_distance": 10000, "areas"`, 1),
			[]string{"maximum_gridpoint_distance"},
		},
		{
			"everything cold at once",
			`{"source": {"bucket": "s3://other"}, "areas": ["nowcast"],
			  "loader": {"type": "memory", "configuration": {"max_size_gib": {"nowcast": 1}}},
			  "maximum_gridpoint_distance": 500}`,
			[]string{"source.bucket", "loader.type", "loader.configuration", "maximum_gridpoint_distance"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ColdChanges(base, mustParse(t, tt.changed))
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// There is no previous configuration at startup, and nothing about a first
// load is pending a restart.
func TestColdChangesFromNothing(t *testing.T) {
	if got := ColdChanges(nil, mustParse(t, shipped)); got != nil {
		t.Errorf("got %v, want nothing", got)
	}
}

func mustParse(t *testing.T, data string) *Configuration {
	t.Helper()
	conf, problems := Parse([]byte(data))
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	return conf
}

// readShipped reads one of the configuration files that ship alongside the
// binary. Note that this makes the test depend on cmd/, so the Dockerfile has
// to copy cmd/ before it runs the tests.
func readShipped(t *testing.T, name string) []byte {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "cmd", "rawdataforecaster", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
