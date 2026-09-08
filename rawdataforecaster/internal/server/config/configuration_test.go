package config

import (
	"strings"
	"testing"
)

func TestDownloadFunctionAcceptsKnownLoaders(t *testing.T) {
	for _, loaderType := range []string{"memory", "blob"} {
		c := &Configuration{Loader: Loader{Type: loaderType}}
		download, err := c.DownloadFunction()
		if err != nil {
			t.Errorf("%s: unexpected error: %s", loaderType, err)
		}
		if download == nil {
			t.Errorf("%s: no download function", loaderType)
		}
	}
}

// A typo in loader.type used to panic. It now has to be an error: the file is
// written by a web form, and a typo must not kill a serving process.
func TestDownloadFunctionRejectsUnknownLoader(t *testing.T) {
	for _, loaderType := range []string{"blobb", "", "MEMORY"} {
		c := &Configuration{Loader: Loader{Type: loaderType}}
		download, err := c.DownloadFunction()
		if err == nil {
			t.Errorf("%q: expected an error", loaderType)
			continue
		}
		if download != nil {
			t.Errorf("%q: got a download function as well as an error", loaderType)
		}
		// The message has to name the alternatives, or the operator is left
		// guessing at what the form would accept.
		for _, want := range []string{"blob", "memory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: error %q does not mention %q", loaderType, err, want)
			}
		}
	}
}

func TestMaxSizesGiBAbsent(t *testing.T) {
	sizes, problems := Loader{Type: "blob"}.MaxSizesGiB()
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if sizes != nil {
		t.Errorf("expected no limits, got %v", sizes)
	}
}

func TestMaxSizesGiBParsesShippedShape(t *testing.T) {
	// The shape nowcast.json ships.
	l := Loader{Type: "blob", Configuration: map[string]interface{}{
		"max_size_gib": map[string]interface{}{"nowcast": 0.2, "world": 12.0},
	}}

	sizes, problems := l.MaxSizesGiB()
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if sizes["nowcast"] != 0.2 || sizes["world"] != 12 {
		t.Errorf("unexpected limits: %v", sizes)
	}
}

// Following the old README produced an unchecked type assertion and took the
// process down. It now has to say what to write instead.
func TestMaxSizesGiBRejectsBareNumber(t *testing.T) {
	l := Loader{Configuration: map[string]interface{}{"max_size_gib": 4.0}}

	sizes, problems := l.MaxSizesGiB()
	if len(problems) != 1 {
		t.Fatalf("expected one problem, got %v", problems)
	}
	if sizes != nil {
		t.Errorf("expected no limits alongside the problem, got %v", sizes)
	}
	if msg := problems[0].Error(); !strings.Contains(msg, "object keyed by area") {
		t.Errorf("error %q does not say what shape is wanted", msg)
	}
}

func TestMaxSizesGiBRejectsBadValues(t *testing.T) {
	l := Loader{Configuration: map[string]interface{}{
		"max_size_gib": map[string]interface{}{
			"stringy":  "4",
			"negative": -1.0,
			"zero":     0.0,
			"fine":     1.5,
		},
	}}

	sizes, problems := l.MaxSizesGiB()
	if len(problems) != 3 {
		t.Fatalf("expected three problems, got %v", problems)
	}
	// Every bad value is reported, not just the first: whoever is fixing the
	// file wants to see all of them at once.
	for _, area := range []string{"stringy", "negative", "zero"} {
		if !mentions(problems, area) {
			t.Errorf("no problem mentions %q: %v", area, problems)
		}
	}
	if len(sizes) != 1 || sizes["fine"] != 1.5 {
		t.Errorf("expected the good limit to survive, got %v", sizes)
	}
}

func mentions(problems []error, want string) bool {
	for _, p := range problems {
		if strings.Contains(p.Error(), want) {
			return true
		}
	}
	return false
}
