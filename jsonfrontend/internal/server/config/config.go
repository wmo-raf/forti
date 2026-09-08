package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync/atomic"
)

// current holds the configuration in use. It is replaced wholesale on reload,
// never modified in place: a request takes one snapshot with Current and reads
// from it throughout, so a response cannot mix settings from two versions.
var current atomic.Pointer[Elements]

// Current returns the configuration in use. Callers should take it once per
// request and pass it down rather than calling this repeatedly.
func Current() *Elements {
	return current.Load()
}

// Set replaces the configuration in use.
func Set(conf *Elements) {
	current.Store(conf)
}

// Parse reads a configuration and checks it for problems. It returns the
// problems it found rather than a single error, because a config file usually
// has more than one thing wrong with it and the person fixing it wants to see
// them all at once.
func Parse(data []byte) (*Elements, []error) {
	var conf Elements
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&conf); err != nil {
		return nil, []error{fmt.Errorf("unable to read json configuration: %w", err)}
	}
	if problems := conf.Problems(); len(problems) != 0 {
		return nil, problems
	}
	return &conf, nil
}

// InitializeFromString populates the configuration from a string.
func InitializeFromString(config string) error {
	return initialize([]byte(config))
}

// Initialize populates the configuration from the given file path.
func Initialize(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	return initialize(data)
}

func initialize(data []byte) error {
	conf, problems := Parse(data)
	if len(problems) != 0 {
		return problems[0]
	}
	Set(conf)
	return nil
}

// Problems returns everything wrong with the configuration.
//
// The checks here are all for mistakes that would otherwise pass silently.
// jsonfrontend drops anything it cannot match instead of complaining, so a
// misspelled parameter or a duplicated time offset produces a forecast that is
// quietly missing data rather than an error anybody notices.
func (e *Elements) Problems() []error {
	var problems []error

	if len(e.Parameters) == 0 {
		problems = append(problems, fmt.Errorf("parameters: no time periods configured, so every forecast would be empty"))
	}

	if e.DataExpiryOffset < 0 {
		problems = append(problems, fmt.Errorf("data_expiry_offset: must not be negative, got %d", e.DataExpiryOffset))
	}

	// Time periods are keyed by their offset when a response is built, so two
	// periods sharing an offset means one of them is dropped — and which one
	// varies between requests.
	byOffset := make(map[int][]string)
	for name, group := range e.Parameters {
		byOffset[group.Offset] = append(byOffset[group.Offset], name)
	}
	for offset, names := range byOffset {
		if len(names) > 1 {
			sort.Strings(names)
			problems = append(problems, fmt.Errorf(
				"parameters: periods %v all use offset %d, so only one of them would appear in a forecast",
				names, offset))
		}
	}

	for _, name := range sortedKeys(e.Parameters) {
		group := e.Parameters[name]

		// Details are keyed by external name within a period, so two internal
		// parameters mapping to the same external name means one is lost.
		byExternal := make(map[string][]string)
		for internal, external := range group.Parameters {
			if external == "" {
				problems = append(problems, fmt.Errorf(
					"parameters.%s.parameters.%s: external name is empty", name, internal))
				continue
			}
			byExternal[external] = append(byExternal[external], internal)
		}
		for _, external := range sortedKeys(byExternal) {
			internals := byExternal[external]
			if len(internals) > 1 {
				sort.Strings(internals)
				problems = append(problems, fmt.Errorf(
					"parameters.%s: %v all map to %q, so only one of them would appear in a forecast",
					name, internals, external))
			}
		}

		if group.Summary != nil && group.Summary.SymbolCode == "" {
			problems = append(problems, fmt.Errorf(
				"parameters.%s.summary: symbol_code is required when summary is set", name))
		}

		if group.Summary == nil && len(group.Parameters) == 0 {
			problems = append(problems, fmt.Errorf(
				"parameters.%s: has neither parameters nor a summary, so it would never appear in a forecast", name))
		}
	}

	for i, header := range e.HTTPHeaders {
		if header.Key == "" {
			problems = append(problems, fmt.Errorf("http_headers[%d]: key is empty", i))
		}
	}

	return problems
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type Elements struct {
	CutForecast      bool                 `json:"cut_forecast,omitempty"`
	LocationFromGrid bool                 `json:"location_from_grid,omitempty"`
	HTTPHeaders      []HTTPHeader         `json:"http_headers"`
	OfferGzip        bool                 `json:"offer_gzip"`
	SkipAltitude     bool                 `json:"skip_altitude,omitempty"`
	DataExpiryOffset int                  `json:"data_expiry_offset"`
	Meta             Meta                 `json:"meta,omitempty"`
	Parameters       map[string]TimeGroup `json:"parameters"`
}

type HTTPHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Meta struct {
	RadarCoverage string `json:"radar_coverage,omitempty"`
}

type TimeGroup struct {
	Offset     int               `json:"offset"`
	Summary    *Summary          `json:"summary,omitempty"`
	Parameters map[string]string `json:"parameters"`
}

type Summary struct {
	SymbolCode       string `json:"symbol_code"`
	SymbolConfidence string `json:"symbol_confidence,omitempty"`
}
