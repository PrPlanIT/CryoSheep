// Package journal records what a run actually did, on the machine that did it.
//
// The record has to survive the event it describes. Loki, Prometheus and etcd
// all live inside the estate being shut down, so the only thing still standing
// when the last step runs is the local disk. A run is therefore written as it
// goes and shipped later, once something is alive to ship it to.
//
// Hygiene is a design constraint, not an afterthought. This writes one small
// file per run, not a line per poll: a machine that reboots cleanly every few
// weeks should accumulate kilobytes a year, and retention caps it regardless.
// A shutdown routine that floods the very log store it is trying to preserve
// evidence in would be self-defeating.
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Trigger records why a run happened, because a UPS event and a routine reboot
// are different populations and calibration should be able to tell them apart.
const (
	TriggerUPS     = "ups"
	TriggerSystemd = "systemd"
	TriggerManual  = "manual"
)

// Step outcomes.
const (
	OutcomeDone    = "done"
	OutcomeSkipped = "skipped"
	OutcomeForced  = "forced"
	OutcomeAborted = "aborted"
	OutcomeFailed  = "failed"
)

// maxErr bounds a recorded error. A command that dies noisily must not turn a
// kilobyte record into a megabyte one.
const maxErr = 200

// maxNote bounds recorded evidence. Generous enough for the stateful workloads
// of one node, small enough that a run stays a kilobyte.
const maxNote = 2000

// KeepDefault is how many runs are retained. Enough for calibration to have a
// distribution, few enough that the directory never needs thinking about.
const KeepDefault = 20

type Step struct {
	Action  string        `json:"action"`
	Target  string        `json:"target,omitempty"`
	Gate    string        `json:"gate,omitempty"` // ups.status observed at the gate
	Started time.Time     `json:"started"`
	Elapsed time.Duration `json:"elapsed"`
	Outcome string        `json:"outcome"`
	Err     string        `json:"err,omitempty"`

	// Note carries evidence a step gathered — which workloads held authority on
	// this node, for instance. Bounded like Err: a record is for reading later,
	// not a dumping ground.
	Note string `json:"note,omitempty"`
}

type Run struct {
	ID       string        `json:"id"`
	Host     string        `json:"host"`
	Trigger  string        `json:"trigger"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Complete bool          `json:"complete"`
	Aborted  bool          `json:"aborted"`
	Shipped  bool          `json:"shipped"`
	Steps    []Step        `json:"steps"`
}

type Writer struct {
	dir  string
	path string
	run  Run
	now  func() time.Time
}

// Open starts a run. The file exists from this moment, so a machine that loses
// power before the first step still leaves evidence that a run began.
func Open(dir, host, trigger string) (*Writer, error) {
	return open(dir, host, trigger, time.Now)
}

func open(dir, host, trigger string, now func() time.Time) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal dir: %w", err)
	}
	start := now()
	id := start.UTC().Format("20060102T150405Z")
	w := &Writer{
		dir:  dir,
		path: filepath.Join(dir, id+".json"),
		now:  now,
		run: Run{
			ID:      id,
			Host:    host,
			Trigger: trigger,
			Started: start,
		},
	}
	return w, w.flush()
}

func (w *Writer) RunID() string { return w.run.ID }
func (w *Writer) Path() string  { return w.path }

// StepStart records that a step is being attempted and returns its index.
//
// Written before the step runs, deliberately: a run killed mid-step is the case
// that cannot otherwise be reconstructed, and "what was it doing when the power
// went" is the question the record exists to answer.
func (w *Writer) StepStart(action, target, gate string) int {
	w.run.Steps = append(w.run.Steps, Step{
		Action:  action,
		Target:  target,
		Gate:    gate,
		Started: w.now(),
		Outcome: "in-flight",
	})
	_ = w.flush()
	return len(w.run.Steps) - 1
}

// Note attaches evidence to a step.
func (w *Writer) Note(i int, note string) {
	if i < 0 || i >= len(w.run.Steps) {
		return
	}
	w.run.Steps[i].Note = truncate(note, maxNote)
	_ = w.flush()
}

func (w *Writer) StepEnd(i int, outcome string, err error) {
	if i < 0 || i >= len(w.run.Steps) {
		return
	}
	s := &w.run.Steps[i]
	s.Elapsed = w.now().Sub(s.Started)
	s.Outcome = outcome
	if err != nil {
		s.Err = truncate(err.Error(), maxErr)
	}
	_ = w.flush()
}

// Close finalises the run. complete is false when the sequence did not reach its
// end — calibration excludes those, since a run that was cut short says nothing
// about how long a run takes.
func (w *Writer) Close(complete, aborted bool) error {
	w.run.Duration = w.now().Sub(w.run.Started)
	w.run.Complete = complete
	w.run.Aborted = aborted
	if err := w.flush(); err != nil {
		return err
	}
	_, err := Prune(w.dir, KeepDefault)
	return err
}

// flush writes the whole record atomically, so a crash mid-write can never leave
// a half-parsed file where calibration expects data.
func (w *Writer) flush() error {
	if w.run.Steps == nil {
		w.run.Steps = []Step{} // [] rather than null: consumers should not special-case
	}
	b, err := json.MarshalIndent(w.run, "", "  ")
	if err != nil {
		return err
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Load reads every run in a directory, newest first. An unparsable file is
// skipped rather than fatal: one bad record must not hide every good one.
func Load(dir string) ([]Run, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r Run
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started.After(runs[j].Started) })
	return runs, nil
}

// Unshipped returns runs still awaiting delivery, oldest first so a replay
// reconstructs the incident in order.
func Unshipped(dir string) ([]Run, error) {
	all, err := Load(dir)
	if err != nil {
		return nil, err
	}
	var out []Run
	for i := len(all) - 1; i >= 0; i-- {
		if !all[i].Shipped {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// MarkShipped records delivery so a replay is idempotent — a machine that boots,
// ships, and reboots again must not send the same incident twice.
func MarkShipped(dir, id string) error {
	path := filepath.Join(dir, id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	if r.Shipped {
		return nil
	}
	r.Shipped = true
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Prune keeps the newest runs and removes the rest.
//
// An unshipped run is never pruned, however old: retention exists to bound disk,
// not to discard evidence that has not reached anywhere yet.
func Prune(dir string, keep int) (int, error) {
	if keep <= 0 {
		keep = KeepDefault
	}
	runs, err := Load(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for i, r := range runs {
		if i < keep || !r.Shipped {
			continue
		}
		if os.Remove(filepath.Join(dir, r.ID+".json")) == nil {
			removed++
		}
	}
	return removed, nil
}
