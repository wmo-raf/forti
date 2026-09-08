package configwatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// write puts content at path the way the controller does, so tests exercise
// the same rename-into-place the real thing uses.
func write(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// applied records what an Applier was handed, and rejects anything it has been
// told to reject.
type applied struct {
	mutex  sync.Mutex
	seen   []string
	reject map[string]bool
}

func (a *applied) apply(data []byte) Result {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if a.reject[string(data)] {
		return Result{Errors: []error{errors.New("rejected: " + string(data))}}
	}
	a.seen = append(a.seen, string(data))
	return Result{Applied: []string{"everything"}}
}

func (a *applied) all() []string {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return append([]string(nil), a.seen...)
}

func newWatcher(t *testing.T, content string, a *applied) (*Watcher, string, string) {
	t.Helper()
	dir := t.TempDir()
	statusDir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	write(t, path, content)

	return &Watcher{
		Module:    "test",
		Path:      path,
		StatusDir: statusDir,
		Interval:  5 * time.Millisecond,
		Apply:     a.apply,
	}, path, statusDir
}

func readStatus(t *testing.T, statusDir string) Status {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(statusDir, "test.json"))
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	return status
}

// waitFor polls until condition holds, so tests do not depend on how long a
// tick takes.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLoadAppliesAndReportsStatus(t *testing.T) {
	a := &applied{}
	watcher, _, statusDir := newWatcher(t, `first`, a)

	if err := watcher.Load(); err != nil {
		t.Fatal(err)
	}

	if got := a.all(); len(got) != 1 || got[0] != "first" {
		t.Errorf("expected the file to be applied once, got %v", got)
	}

	status := readStatus(t, statusDir)
	if !status.OK {
		t.Errorf("expected ok status, got %+v", status)
	}
	if status.Module != "test" {
		t.Errorf("unexpected module: %q", status.Module)
	}
	if status.LoadedSHA == "" {
		t.Error("expected a digest of the loaded configuration")
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	a := &applied{}
	statusDir := t.TempDir()
	watcher := &Watcher{
		Module:    "test",
		Path:      filepath.Join(t.TempDir(), "absent.json"),
		StatusDir: statusDir,
		Apply:     a.apply,
	}

	if err := watcher.Load(); err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if status := readStatus(t, statusDir); status.OK || len(status.Errors) == 0 {
		t.Errorf("expected a failed status with a reason, got %+v", status)
	}
}

func TestRunAppliesChanges(t *testing.T) {
	a := &applied{}
	watcher, path, _ := newWatcher(t, `first`, a)
	if err := watcher.Load(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	write(t, path, `second`)
	waitFor(t, "the change to be applied", func() bool {
		got := a.all()
		return len(got) == 2 && got[1] == "second"
	})
}

// Touching a file without changing it should not cost a reload: for
// rawdataforecaster that would mean re-reading forecast data for nothing.
func TestRunIgnoresIdenticalContent(t *testing.T) {
	a := &applied{}
	watcher, path, _ := newWatcher(t, `same`, a)
	if err := watcher.Load(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	write(t, path, `same`)
	time.Sleep(60 * time.Millisecond)

	if got := a.all(); len(got) != 1 {
		t.Errorf("expected identical content not to be reapplied, got %v", got)
	}
}

// A configuration that cannot be applied must leave the running one alone and
// say why, and a later good one must still be picked up.
func TestRunKeepsServingAfterARejectedChange(t *testing.T) {
	a := &applied{reject: map[string]bool{"bad": true}}
	watcher, path, statusDir := newWatcher(t, `good`, a)
	if err := watcher.Load(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	write(t, path, `bad`)
	waitFor(t, "the rejection to be reported", func() bool {
		data, err := os.ReadFile(filepath.Join(statusDir, "test.json"))
		if err != nil {
			return false
		}
		var status Status
		return json.Unmarshal(data, &status) == nil && !status.OK
	})

	if got := a.all(); len(got) != 1 || got[0] != "good" {
		t.Errorf("a rejected configuration was applied: %v", got)
	}
	if status := readStatus(t, statusDir); len(status.Errors) == 0 {
		t.Error("expected the reason for the rejection in the status file")
	}

	write(t, path, `better`)
	waitFor(t, "recovery after a rejection", func() bool {
		got := a.all()
		return len(got) == 2 && got[1] == "better"
	})
}

func TestRunStopsWithContext(t *testing.T) {
	a := &applied{}
	watcher, path, _ := newWatcher(t, `first`, a)
	if err := watcher.Load(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(stopped)
	}()

	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}

	write(t, path, `second`)
	time.Sleep(30 * time.Millisecond)
	if got := a.all(); len(got) != 1 {
		t.Errorf("watcher kept working after being stopped: %v", got)
	}
}

// Status is written where a controller can read it without any knowledge of
// the service, so its shape is part of the contract.
func TestStatusFileShape(t *testing.T) {
	statusDir := t.TempDir()
	watcher := &Watcher{Module: "test", StatusDir: statusDir}
	watcher.report(Status{
		Module:         "test",
		LoadedSHA:      "abc",
		LoadedAt:       time.Unix(0, 0).UTC(),
		OK:             true,
		Applied:        []string{"areas"},
		PendingRestart: []string{"source.bucket"},
	})

	data, err := os.ReadFile(filepath.Join(statusDir, "test.json"))
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"module", "loaded_sha", "loaded_at", "ok", "applied", "pending_restart"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("status file is missing %q: %s", key, data)
		}
	}
	if _, ok := raw["errors"]; ok {
		t.Error("expected errors to be omitted when there are none")
	}
}
