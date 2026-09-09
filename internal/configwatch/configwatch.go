// Package configwatch lets a service reload its configuration file while
// running, and report what it did to a status file.
//
// A controller (see forti-control) owns the config files and writes them
// atomically. Services watch their own file, validate what they find, and
// apply it only if it is good. A file that cannot be applied never disturbs
// the configuration already in use — the service keeps serving what it has and
// records the reason in its status file.
//
// A service may also publish live state alongside the configuration status —
// see Watcher.State. The two answer different questions: the status says which
// file was accepted, the state says what the service is doing with it.
//
// The filesystem is the whole interface: nothing here opens a socket, and the
// controller needs no way to reach the service.
package configwatch

import (
	"bytes"
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

// DefaultStateInterval is how often a Watcher rewrites its status file to keep
// Watcher.State fresh. Unlike the configuration, state changes without anybody
// editing anything, so it has to be republished on a clock. Unchanged state is
// not rewritten, so this costs one marshal and one comparison.
const DefaultStateInterval = 15 * time.Second

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

	// State is whatever Watcher.State returned, or nil if the service
	// publishes none. It is deliberately untyped here: this package knows
	// when to write a status file and nothing at all about what a particular
	// service does.
	State any `json:"state,omitempty"`
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

	// State, if set, is called for whatever the service wants to publish
	// about itself, and its result is recorded under "state". Returning nil
	// omits the field, which is what a service that has not finished starting
	// should do.
	//
	// It exists because an applied configuration is not evidence that the
	// configuration is working. A service that accepted a file naming three
	// datasets and has been failing to download one of them for six hours has
	// exactly the same status as one serving all three. Only the service can
	// tell them apart, and without this the difference reaches nobody.
	//
	// It is called on the Watcher's goroutine while no lock of this package's
	// is held, and must be safe to call at any time.
	State func() any

	// StateInterval is how often the status file is rewritten so State stays
	// current. The zero value means DefaultStateInterval.
	StateInterval time.Duration

	lastSHA  string
	lastSize int64
	lastMod  time.Time

	// last is the status as last built, so the periodic rewrite can refresh
	// State without having to restate what the configuration did; lastWritten
	// is the bytes that reached disk, so a rewrite that would change nothing
	// does not touch the file.
	//
	// Neither is guarded, like the fields above: Load and Run are the only
	// writers and the package already requires Load to finish before Run
	// starts.
	last        Status
	lastWritten []byte
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

	stateInterval := w.StateInterval
	if stateInterval <= 0 {
		stateInterval = DefaultStateInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	stateTicker := time.NewTicker(stateInterval)
	defer stateTicker.Stop()

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
		case <-stateTicker.C:
			w.refreshState()
		}
	}
}

// refreshState rewrites the status file with current state, leaving everything
// the last configuration attempt reported as it was. A service whose state has
// not moved writes nothing, so an idle process leaves the file's modification
// time alone and whatever mirrors it up has nothing to do.
func (w *Watcher) refreshState() {
	if w.State == nil || w.StatusDir == "" {
		return
	}

	// A status file that has gone missing — a recreated volume, a tidied
	// directory — is put back. Without this the write-only-on-change rule
	// would keep it missing until somebody edited the configuration, which is
	// the same silently-stale failure this whole file exists to report.
	if _, err := os.Stat(filepath.Join(w.StatusDir, w.Module+".json")); err != nil {
		w.lastWritten = nil
	}

	w.report(w.last)
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

// report writes the status file, filling in current state. A service that
// cannot write its status is still a working service, so this only logs.
//
// The status is remembered whether or not it is written, so that a later state
// refresh republishes it rather than an empty one.
func (w *Watcher) report(status Status) {
	w.last = status
	if w.StatusDir == "" {
		return
	}
	if w.State != nil {
		status.State = w.State()
	}

	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		log.Printf("unable to encode status: %s", err)
		return
	}
	data = append(data, '\n')

	// Written only when something changed, so the periodic state refresh is
	// free for a service that is doing what it was doing a minute ago.
	if bytes.Equal(data, w.lastWritten) {
		return
	}
	if err := writeFile(filepath.Join(w.StatusDir, w.Module+".json"), data); err != nil {
		log.Printf("unable to write status file: %s", err)
		return
	}
	w.lastWritten = data
}

// writeFile writes to a temporary file and renames it into place, so a reader
// never sees a half-written file.
func writeFile(path string, data []byte) error {
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
