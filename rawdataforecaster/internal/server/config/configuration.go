package config

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	internalformat "github.com/metno/forti-internalformat"
	"github.com/metno/forti/rawdataforecaster/internal/server/forecast/dataset/values"
	"github.com/metno/forti/rawdataforecaster/internal/server/forecast/dataset/values/blob"
	"github.com/metno/forti/rawdataforecaster/internal/server/forecast/dataset/values/memory"
)

type DownloadFunction func(ctx context.Context, source internalformat.Client, datasetMeta *internalformat.DatasetMeta, gridid string, config map[string]interface{}) (values.Reader, error)

type Configuration struct {
	Source                   DataSource `json:"source"`
	Areas                    []string   `json:"areas"`
	Loader                   Loader     `json:"loader"`
	MaximumGridPointDistance int        `json:"maximum_gridpoint_distance"` // in meters
}

// downloaders is every value loader.type accepts. It is the single source of
// truth for both the lookup and the error message, so the two cannot drift.
var downloaders = map[string]DownloadFunction{
	"memory": memory.Download,
	"blob":   blob.Download,
}

// DownloadFunction returns the function that loads grid values for the
// configured loader type.
//
// An unknown type used to panic. It has to be an error now: this file is
// written by forti-control, so the value can be a typo in a web form, and a
// typo must not take down a process that is serving forecasts.
func (c *Configuration) DownloadFunction() (DownloadFunction, error) {
	if download, ok := downloaders[c.Loader.Type]; ok {
		return download, nil
	}
	if c.Loader.Type == "" {
		return nil, fmt.Errorf("loader.type: missing, expected one of %s", loaderTypes())
	}
	return nil, fmt.Errorf("loader.type: unknown value %q, expected one of %s", c.Loader.Type, loaderTypes())
}

func loaderTypes() string {
	return strings.Join(slices.Sorted(maps.Keys(downloaders)), ", ")
}

type DataSource struct {
	Bucket string `json:"bucket"`
}

type Loader struct {
	Type          string                 `json:"type"`
	Configuration map[string]interface{} `json:"configuration,omitempty"`
}

// maxSizeKey caps how much a single area may download, in GiB.
const maxSizeKey = "max_size_gib"

// MaxSizesGiB returns the per-area download limits, and everything wrong with
// them. Areas with no limit, and areas whose limit could not be read, are
// absent from the map — an unreadable limit is never treated as no limit.
//
// The value is an object keyed by area name, because one rawdataforecaster
// instance serves areas of very different sizes: forecast.json covers arctic,
// nordic and world at once. A single number could not cap them separately.
//
// Everything here is checked. It used to be two bare type assertions, so a
// limit written the way the README described it — a plain number — crashed the
// process instead of being rejected.
func (l Loader) MaxSizesGiB() (map[string]float64, []error) {
	raw, ok := l.Configuration[maxSizeKey]
	if !ok {
		return nil, nil
	}

	byArea, ok := raw.(map[string]interface{})
	if !ok {
		return nil, []error{fmt.Errorf(
			`loader.configuration.%s: must be an object keyed by area, like {"nowcast": 0.2}, got %s`,
			maxSizeKey, jsonType(raw))}
	}

	var problems []error
	sizes := make(map[string]float64, len(byArea))
	// Sorted, so that a file with several bad limits reports them in the same
	// order every time.
	for _, area := range slices.Sorted(maps.Keys(byArea)) {
		size, ok := byArea[area].(float64)
		if !ok {
			problems = append(problems, fmt.Errorf(
				"loader.configuration.%s.%s: must be a number of GiB, got %s",
				maxSizeKey, area, jsonType(byArea[area])))
			continue
		}
		if size <= 0 {
			problems = append(problems, fmt.Errorf(
				"loader.configuration.%s.%s: must be greater than zero, got %v",
				maxSizeKey, area, size))
			continue
		}
		sizes[area] = size
	}

	if len(sizes) == 0 {
		return nil, problems
	}
	return sizes, problems
}

// jsonType names what a decoded JSON value turned out to be, so that a message
// about the wrong shape can say which wrong shape it was.
func jsonType(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []interface{}:
		return "an array"
	case map[string]interface{}:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}
