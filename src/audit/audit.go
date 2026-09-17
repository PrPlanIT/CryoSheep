// Package audit records what a run did, as structured lines on stderr.
//
// There is no second copy. systemd captures stderr into journald, journald here
// is persistent, and Alloy ships journald to Loki — so writing our own file
// would produce a parallel record with its own rotation that nothing reads, and
// that no collector knows about.
//
// The one thing journald does not give for free is durability at the moment it
// matters. Its default SyncIntervalSec is five minutes, and a power cut inside
// that window loses the tail — which is precisely the part worth having. So
// every line is followed by an explicit journal sync, measured at ~5ms on a real
// node. That is cheap enough that no line has to argue for itself, and it means
// severity stays honest: nothing is logged at CRIT merely to force a flush.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
	"unicode/utf8"
)

// Trigger records why a run happened, because a UPS event and a routine reboot
// are different populations and calibration should tell them apart.
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

// maxErr bounds a recorded error. A command that dies noisily must not turn one
// line into a megabyte.
const maxErr = 200

// MaxNote bounds recorded evidence. A real worker carries around thirty stateful
// workloads; the budget is the caller's to spend, so it is exported and the
// writer decides what to drop rather than having its tail cut off here.
const MaxNote = 4000

// Event kinds, as they appear in the ev field.
const (
	evRunStart  = "run.start"
	evStepStart = "step.start"
	evStepEnd   = "step.end"
	evRunEnd    = "run.end"
)

// line is one record. Fields are omitted when empty so a step line stays short
// enough to read on a console at three in the morning.
type line struct {
	Time    string `json:"t"`
	Run     string `json:"run"`
	Host    string `json:"host"`
	Trigger string `json:"trigger"`
	Event   string `json:"ev"`

	Step    int    `json:"step,omitempty"`
	Action  string `json:"action,omitempty"`
	Target  string `json:"target,omitempty"`
	Gate    string `json:"gate,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Err     string `json:"err,omitempty"`
	Note    string `json:"note,omitempty"`

	ElapsedMS  int64 `json:"elapsed_ms,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`

	Complete *bool `json:"complete,omitempty"`
	Aborted  *bool `json:"aborted,omitempty"`
}

// Log writes one run's lines.
type Log struct {
	id      string
	host    string
	trigger string
	started time.Time
	starts  []time.Time // per step, to compute elapsed at the end

	out  io.Writer
	sync func() error
	now  func() time.Time
}

// Open begins a run. The first line is written and synced immediately, so a
// machine that loses power before the first step still leaves evidence that a
// run began at all.
func Open(host, trigger string) *Log {
	return open(os.Stderr, journalSync, time.Now, host, trigger)
}

func open(out io.Writer, sync func() error, now func() time.Time, host, trigger string) *Log {
	start := now()
	l := &Log{
		// The pid disambiguates two runs that begin in the same second — a
		// conserve and the sleep that follows it, say — which would otherwise
		// share an id and be read back as one impossible run.
		id:      start.UTC().Format("20060102T150405Z") + "-" + strconv.Itoa(os.Getpid()),
		host:    host,
		trigger: trigger,
		started: start,
		out:     out,
		sync:    sync,
		now:     now,
	}
	l.write(line{Event: evRunStart})
	return l
}

// ID is the run's identifier, shared by every line it writes.
func (l *Log) ID() string { return l.id }

// StepStart records a step beginning and returns its index.
//
// A step is announced before it runs, not only after, because "what was it doing
// when the power went" is a question only a start line can answer: a step.start
// with no step.end is exactly the shape of a machine that was cut mid-step.
func (l *Log) StepStart(action, target, gate string) int {
	i := len(l.starts)
	l.starts = append(l.starts, l.now())
	l.write(line{Event: evStepStart, Step: i, Action: action, Target: target, Gate: gate})
	return i
}

// Note attaches evidence gathered by a step — which workloads held authority on
// this node, for instance.
func (l *Log) Note(i int, note string) {
	if i < 0 || i >= len(l.starts) {
		return
	}
	l.write(line{Event: evStepEnd, Step: i, Note: truncate(note, MaxNote), Outcome: "note"})
}

// StepEnd records how a step finished and how long it took.
func (l *Log) StepEnd(i int, outcome string, err error) {
	if i < 0 || i >= len(l.starts) {
		return
	}
	rec := line{
		Event:     evStepEnd,
		Step:      i,
		Outcome:   outcome,
		ElapsedMS: l.now().Sub(l.starts[i]).Milliseconds(),
	}
	if err != nil {
		rec.Err = truncate(err.Error(), maxErr)
	}
	l.write(rec)
}

// Close finalises the run. complete is false when the sequence did not reach its
// end — calibration excludes those, since a run that was cut short says nothing
// about how long a run takes.
func (l *Log) Close(complete, aborted bool) error {
	c, a := complete, aborted
	l.write(line{
		Event:      evRunEnd,
		DurationMS: l.now().Sub(l.started).Milliseconds(),
		Complete:   &c,
		Aborted:    &a,
	})
	return nil
}

// write emits one line and forces it to disk before returning.
func (l *Log) write(rec line) {
	rec.Time = l.now().UTC().Format(time.RFC3339Nano)
	rec.Run, rec.Host, rec.Trigger = l.id, l.host, l.trigger

	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	fmt.Fprintf(l.out, "%s\n", b)

	// Durability is the whole reason this package exists rather than a plain
	// logger. A failure here is not fatal: losing the guarantee is better than
	// refusing to shut a machine down because journald was busy.
	if l.sync != nil {
		_ = l.sync()
	}
}

// journalSync asks journald to flush everything it holds to the filesystem and
// waits for it. This is the documented interface for the guarantee; signalling
// the daemon directly would couple us to its pid and its signal numbering.
func journalSync() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "journalctl", "--sync").Run()
}

// truncate bounds a string to n bytes, ellipsis included, without splitting a
// rune. The ellipsis is three bytes, so cutting at n-1 overshoots the budget it
// exists to enforce.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	cut := n - len(ell)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}
