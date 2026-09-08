package config

import (
	"os"
	"strings"
	"testing"
)

// The configuration jsonfrontend actually ships with must pass validation.
// A validator that rejects real config would turn a reload into an outage.
func TestShippedConfigurationIsAccepted(t *testing.T) {
	data, err := os.ReadFile("../../../cmd/jsonfrontend/jsonformat.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, problems := Parse(data); len(problems) != 0 {
		for _, problem := range problems {
			t.Errorf("shipped jsonformat.json was rejected: %s", problem)
		}
	}
}

func TestProblems(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string // substring of the expected problem, empty means no problem
	}{
		{
			name: "a usable configuration",
			config: `{"parameters":{
				"instant":{"offset":0,"parameters":{"air_temperature_2m":"air_temperature"}},
				"next_1_hours":{"offset":1,"summary":{"symbol_code":"weather_symbol"},
					"parameters":{"precipitation_amount_acc1h":"precipitation_amount"}}}}`,
		},
		{
			// getTimeStep keys periods by offset, so the second period would
			// replace the first and neither is reliably the winner.
			name: "two periods share an offset",
			config: `{"parameters":{
				"next_1_hours":{"offset":6,"parameters":{"a":"x"}},
				"next_6_hours":{"offset":6,"parameters":{"b":"y"}}}}`,
			want: "all use offset 6",
		},
		{
			// Details are keyed by external name, so one of these is dropped.
			name: "two parameters share an external name",
			config: `{"parameters":{"instant":{"offset":0,"parameters":{
				"air_temperature_2m":"air_temperature",
				"air_temperature_10m":"air_temperature"}}}}`,
			want: `all map to "air_temperature"`,
		},
		{
			name:   "no time periods at all",
			config: `{"parameters":{}}`,
			want:   "no time periods configured",
		},
		{
			name:   "a period that can never produce output",
			config: `{"parameters":{"instant":{"offset":0,"parameters":{}}}}`,
			want:   "neither parameters nor a summary",
		},
		{
			name: "a summary without a symbol code",
			config: `{"parameters":{"next_1_hours":{"offset":1,"summary":{},
				"parameters":{"a":"x"}}}}`,
			want: "symbol_code is required",
		},
		{
			name: "an empty external name",
			config: `{"parameters":{"instant":{"offset":0,"parameters":{
				"air_temperature_2m":""}}}}`,
			want: "external name is empty",
		},
		{
			name: "a negative expiry",
			config: `{"data_expiry_offset":-5,"parameters":{
				"instant":{"offset":0,"parameters":{"a":"x"}}}}`,
			want: "must not be negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, problems := Parse([]byte(test.config))

			if test.want == "" {
				if len(problems) != 0 {
					t.Fatalf("expected no problems, got %v", problems)
				}
				return
			}

			for _, problem := range problems {
				if strings.Contains(problem.Error(), test.want) {
					return
				}
			}
			t.Errorf("expected a problem containing %q, got %v", test.want, problems)
		})
	}
}

func TestParseRejectsInvalidJSON(t *testing.T) {
	if _, problems := Parse([]byte(`{"parameters":`)); len(problems) == 0 {
		t.Error("expected invalid json to be rejected")
	}
}

// A rejected configuration must not replace the one in use.
func TestBadConfigurationDoesNotReplaceGoodOne(t *testing.T) {
	good := `{"offer_gzip":true,"parameters":{"instant":{"offset":0,
		"parameters":{"air_temperature_2m":"air_temperature"}}}}`
	if err := InitializeFromString(good); err != nil {
		t.Fatal(err)
	}
	before := Current()

	if err := InitializeFromString(`{"parameters":{}}`); err == nil {
		t.Fatal("expected an empty configuration to be rejected")
	}

	if Current() != before {
		t.Error("a rejected configuration replaced the one in use")
	}
	if !Current().OfferGzip {
		t.Error("the configuration in use was modified")
	}
}
