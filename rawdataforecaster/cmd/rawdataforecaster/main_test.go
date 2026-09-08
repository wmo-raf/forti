package main

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testData is the bucket URL of the checked-in sample datasets.
func testData(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return "file://" + filepath.Clean(filepath.Dir(filename)+"/../../test/data")
}

func configFor(bucket string, areas ...string) []byte {
	data, _ := json.Marshal(map[string]interface{}{
		"source": map[string]string{"bucket": bucket},
		"areas":  areas,
		"loader": map[string]string{"type": "memory"},
	})
	return data
}

func started(t *testing.T) *reloader {
	t.Helper()
	r := &reloader{}
	if result := r.apply(configFor(testData(t), "group_a")); !result.OK() {
		t.Fatalf("startup rejected: %v", result.Errors)
	}
	return r
}

func TestApplyBuildsTheForecasterOnce(t *testing.T) {
	r := started(t)

	forecaster := r.running()
	if forecaster == nil {
		t.Fatal("no forecaster after a successful startup")
	}
	if !r.apply(configFor(testData(t), "group_a", "group_b")).OK() {
		t.Fatal("reload rejected")
	}
	if r.running() != forecaster {
		t.Error("a reload replaced the forecaster instead of reconfiguring it")
	}
}

// A bucket that cannot be opened has to reach the status file. Reporting
// startup as successful and then exiting would leave a config manager looking
// at a green status file behind a dead process.
func TestApplyReportsAnUnusableBucket(t *testing.T) {
	r := &reloader{}

	result := r.apply(configFor("nonsense://nowhere", "group_a"))

	if result.OK() {
		t.Fatal("expected an unusable bucket to be reported")
	}
	if !strings.Contains(result.Errors[0].Error(), "source.bucket") {
		t.Errorf("error %q does not name the setting at fault", result.Errors[0])
	}
	if r.running() != nil {
		t.Error("a forecaster was left behind by a failed startup")
	}
}

func TestApplyRejectsBadConfigWithoutDisturbingTheRunningOne(t *testing.T) {
	r := started(t)
	forecaster := r.running()

	result := r.apply([]byte(`{"source": {"bucket": "file:///d"}, "areas": [], "loader": {"type": "nope"}}`))

	if result.OK() {
		t.Fatal("expected the configuration to be rejected")
	}
	if len(result.Errors) < 2 {
		t.Errorf("expected every problem at once, got %v", result.Errors)
	}
	if r.running() != forecaster {
		t.Error("a rejected configuration disturbed the running forecaster")
	}
}

// pending_restart describes the running process, not the last edit.
//
// Diffing each file against the one before it made the warning disappear on
// the next unrelated save, while the service was still using the old bucket —
// a change that quietly did nothing, reported as though it had worked.
func TestPendingRestartPersistsAcrossLaterSaves(t *testing.T) {
	r := started(t)

	changed := r.apply(configFor("file:///somewhere-else", "group_a"))
	if !changed.OK() {
		t.Fatalf("rejected: %v", changed.Errors)
	}
	if len(changed.PendingRestart) != 1 || changed.PendingRestart[0] != "source.bucket" {
		t.Fatalf("got %v, want [source.bucket]", changed.PendingRestart)
	}

	// A later save that only adds an area. The bucket is unchanged since the
	// last file, but the running process is still using neither of them.
	again := r.apply(configFor("file:///somewhere-else", "group_a", "group_b"))
	if !again.OK() {
		t.Fatalf("rejected: %v", again.Errors)
	}
	if len(again.PendingRestart) != 1 || again.PendingRestart[0] != "source.bucket" {
		t.Errorf("the restart warning was dropped by an unrelated save: got %v", again.PendingRestart)
	}

	// Putting the original bucket back clears it: the file now matches what
	// the process is actually running.
	restored := r.apply(configFor(testData(t), "group_a"))
	if len(restored.PendingRestart) != 0 {
		t.Errorf("expected no pending restart once the file matches the running config, got %v",
			restored.PendingRestart)
	}
}
