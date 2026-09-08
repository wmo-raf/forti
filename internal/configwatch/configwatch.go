// Package configwatch lets a service reload its configuration file while
// running, and report what it did to a status file.
//
// A controller (see forti-control) owns the config files and writes them
// atomically. Services watch their own file, validate what they find, and
// apply it only if it is good. A file that cannot be applied never disturbs
// the configuration already in use — the service keeps serving what it has and
// records the reason in its status file.
//
// The filesystem is the whole interface: nothing here opens a socket, and the
// controller needs no way to reach the service.
package configwatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// DefaultInterval is how often a Watcher looks at its file. Config changes a
// few times a week, so this only needs to feel immediate to whoever clicked
// Save.
const DefaultInterval = time.Second

// Result is what a service made of a configuration file.
type Result struct {
	// Errors is empty if the configuration was applied. If it is not empty
	// nothing was applied, and the previous configuration is still in use.
	Errors []error

	// Applied names the settings that took effect.
	Applied []string

	// PendingRestart names settings that changed in the file but cannot take
	// effect until the service restarts.
	PendingRestart []string
}

// OK reports whether the configuration was applied.
func (r Result) OK() bool { return len(r.Errors) == 0 }

// An Applier validates and applies one configuration file. It must leave the
// running configuration untouched when it returns errors, and it must not
// panic: it is called with whatever bytes are on disk, which by now means
// whatever somebody typed into a web form.
type Applier func(data []byte) Result

// Status is the record a service leaves for the controller. It is rewritten
// after every attempt, successful or not.
type Status struct {
	Module         string    `json:"module"`
	LoadedSHA      string    `json:"loaded_sha"`
	LoadedAt       time.Time `json:"loaded_at"`
	OK             bool      `json:"ok"`
	Errors         []string  `json:"errors,omitempty"`
	Applied        []string  `json:"applied,omitempty"`
	PendingRestart []string  `json:"pending_restart,omitempty"`
}

// A Watcher applies one configuration file whenever its contents change.
//
// The zero Interval means DefaultInterval. An empty StatusDir means no status
// file is written, which is how the service behaves when it runs without a
// controller.
type Watcher struct {
	Module    string
	Path      string
	StatusDir string
	Interval  time.Duration
	Apply     Applier

	lastSHA  string
	lastSize int64
	lastMod  time.Time
}

// Load reads and applies the file once, and writes the status file. It is how
// a service loads its configuration at startup, so that startup and reload use
// the same validation. The returned error means the configuration was not
// applied.
func (w *Watcher) Load() error {
	data, err := os.ReadFile(w.Path)
	if err != nil {
		w.report(Status{
			Module:   w.Module,
			LoadedAt: time.Now().UTC(),
			OK:       false,
			Errors:   []string{err.Error()},
		})
		return err
	}
	w.stampFileInfo()
	return w.apply(data)
}

// Run applies the file again every time its contents change, until ctx is
// done. Call Load first: Run reports only changes.
func (w *Watcher) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, changed := w.readIfChanged()
			if !changed {
				continue
			}
			if err := w.apply(data); err != nil {
				// Already reported in the status file. Keep watching: the
				// next write is usually the fix.
				log.Printf("config %s not applied: %s", w.Path, err)
			}
		}
	}
}

// readIfChanged returns the file contents, and whether they differ from what
// was last applied. It stats first so an unchanged file costs no read, and
// compares digests so that a touched-but-identical file is not reapplied.
func (w *Watcher) readIfChanged() ([]byte, bool) {
	info, err := os.Stat(w.Path)
	if err != nil {
		return nil, false
	}
	if info.Size() == w.lastSize && info.ModTime().Equal(w.lastMod) {
		return nil, false
	}

	// The controller writes with rename(2), so a read either sees the whole
	// old file or the whole new one, never a mixture.
	data, err := os.ReadFile(w.Path)
	if err != nil {
		return nil, false
	}
	w.lastSize = info.Size()
	w.lastMod = info.ModTime()

	if digest(data) == w.lastSHA {
		return nil, false
	}
	return data, true
}

func (w *Watcher) stampFileInfo() {
	if info, err := os.Stat(w.Path); err == nil {
		w.lastSize = info.Size()
		w.lastMod = info.ModTime()
	}
}

// apply hands the bytes to the service and records the outcome. The digest is
// remembered whatever the outcome, so a file that fails is not retried until
// somebody changes it.
func (w *Watcher) apply(data []byte) error {
	sha := digest(data)
	result := w.Apply(data)
	w.lastSHA = sha

	status := Status{
		Module:         w.Module,
		LoadedSHA:      sha,
		LoadedAt:       time.Now().UTC(),
		OK:             result.OK(),
		Applied:        result.Applied,
		PendingRestart: result.PendingRestart,
	}
	for _, err := range result.Errors {
		status.Errors = append(status.Errors, err.Error())
	}
	w.report(status)

	if !result.OK() {
		return fmt.Errorf("%d problem(s) in %s", len(result.Errors), w.Path)
	}
	if len(result.PendingRestart) > 0 {
		log.Printf("config %s applied; restart required for: %v", w.Path, result.PendingRestart)
	} else {
		log.Printf("config %s applied", w.Path)
	}
	return nil
}

// report writes the status file. A service that cannot write its status is
// still a working service, so this only logs.
func (w *Watcher) report(status Status) {
	if w.StatusDir == "" {
		return
	}
	if err := writeJSONFile(filepath.Join(w.StatusDir, w.Module+".json"), status); err != nil {
		log.Printf("unable to write status file: %s", err)
	}
}

// writeJSONFile writes to a temporary file and renames it into place, so a
// reader never sees a half-written file.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
