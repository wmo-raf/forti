package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// Parse reads a configuration and checks it for problems. It returns the
// problems it found rather than a single error, because a config file usually
// has more than one thing wrong with it and whoever is fixing it wants to see
// them all at once.
func Parse(data []byte) (*Configuration, []error) {
	var conf Configuration
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&conf); err != nil {
		return nil, []error{fmt.Errorf("unable to read json configuration: %w", err)}
	}
	if problems := conf.Problems(); len(problems) != 0 {
		return nil, problems
	}
	return &conf, nil
}

// Problems returns everything wrong with the configuration.
//
// The checks are for mistakes that would otherwise pass silently. This service
// skips what it cannot match rather than complaining, so a misspelled area
// name produces a forecaster that serves nothing over that region and reports
// no error anywhere.
func (c *Configuration) Problems() []error {
	var problems []error

	if c.Source.Bucket == "" {
		problems = append(problems, fmt.Errorf(
			"source.bucket: missing, so there is nowhere to read forecasts from"))
	}

	if len(c.Areas) == 0 {
		problems = append(problems, fmt.Errorf(
			"areas: none configured, so every request would answer \"no datasets available\""))
	}

	seen := make(map[string]bool, len(c.Areas))
	for i, area := range c.Areas {
		if area == "" {
			problems = append(problems, fmt.Errorf("areas[%d]: empty area name", i))
			continue
		}
		if seen[area] {
			problems = append(problems, fmt.Errorf("areas: %q is listed twice", area))
		}
		seen[area] = true
	}

	if _, err := c.DownloadFunction(); err != nil {
		problems = append(problems, err)
	}

	sizes, sizeProblems := c.Loader.MaxSizesGiB()
	problems = append(problems, sizeProblems...)

	// A limit naming an area this instance does not serve caps nothing. It is
	// the shape of mistake this codebase specialises in: a typo that produces
	// silence rather than an error, and here the silence is an absent download
	// limit on something that was meant to have one.
	for _, area := range slices.Sorted(maps.Keys(sizes)) {
		if !seen[area] {
			problems = append(problems, fmt.Errorf(
				"loader.configuration.%s.%s: %q is not in areas, so this limit would cap nothing",
				maxSizeKey, area, area))
		}
	}

	if c.MaximumGridPointDistance < 0 {
		problems = append(problems, fmt.Errorf(
			"maximum_gridpoint_distance: must not be negative, got %d (0 means no limit)",
			c.MaximumGridPointDistance))
	}

	return problems
}

// ColdChanges names the settings in proposed that the running process cannot
// honour without a restart. It returns them in the order they appear in the
// file.
//
// running must be the configuration the process was built from, not the file
// saved before this one. "Needs a restart" is a fact about the running
// process, so comparing one edit against the previous edit would drop the
// warning on the next unrelated save while the old value was still in use.
//
// The forecaster reads its configuration at two moments: when a request
// arrives, and when it downloads a new version of an area. Only the area list
// is consulted often enough to be genuinely live.
//
//   - source.bucket is not consulted again at all. The store is opened once,
//     in New, so a new bucket does exactly nothing until the process restarts.
//   - loader.type, loader.configuration and maximum_gridpoint_distance are
//     read when an area is downloaded. A change reaches each area only when
//     its next version lands — hours away for a forecast area, and one area at
//     a time. Half-applied is not applied, so they are reported here.
//
// Adding or removing an area is deliberately absent: those take effect within
// one poll, and asking for a restart would be a lie.
func ColdChanges(running, proposed *Configuration) []string {
	if running == nil {
		return nil
	}

	var changed []string
	if running.Source.Bucket != proposed.Source.Bucket {
		changed = append(changed, "source.bucket")
	}
	if running.Loader.Type != proposed.Loader.Type {
		changed = append(changed, "loader.type")
	}
	if !reflect.DeepEqual(running.Loader.Configuration, proposed.Loader.Configuration) {
		changed = append(changed, "loader.configuration")
	}
	if running.MaximumGridPointDistance != proposed.MaximumGridPointDistance {
		changed = append(changed, "maximum_gridpoint_distance")
	}
	return changed
}
