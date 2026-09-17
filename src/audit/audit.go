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
	"strings"
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

	out    io.Writer
	human  io.Writer // nil when the operator is not watching
	sync   func() error
	now    func() time.Time
	detach func() // closes the journal pipe, when we opened one
}

// Open begins a run. The first line is written and synced immediately, so a
// machine that loses power before the first step still leaves evidence that a
// run began at all.
//
// Under systemd, stderr is already a journal stream and writing to it is enough.
// Run from a shell it is not — the lines go to the terminal and journald never
// sees them, which would leave cancel and wake with no record of what to undo.
// A run started by hand must be as reversible as one started by a unit, so when
// stderr is not already going to the journal we send a copy there ourselves.
func Open(host, trigger string) *Log {
	out := io.Writer(os.Stderr)
	var human io.Writer
	var detach func()

	if os.Getenv("JOURNAL_STREAM") == "" {
		// Nobody is piping stderr into the journal for us, so send the record
		// there ourselves — and give the terminal something a person can read
		// instead of the same JSON they cannot parse at a glance.
		if w, stop, ok := journalPipe(); ok {
			out, human, detach = w, os.Stderr, stop
		}
	}

	l := open(out, journalSync, time.Now, host, trigger)
	l.human, l.detach = human, detach
	return l
}

// journalPipe holds one systemd-cat open for the life of the run. One process,
// not one per line: the per-line cost is already a sync, and a second fork for
// every step would be felt in a sequence that is racing a battery.
func journalPipe() (io.Writer, func(), bool) {
	cmd := exec.Command("systemd-cat", "-t", Identifier)
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, false
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, false
	}
	return w, func() { _ = w.Close(); _ = cmd.Wait() }, true
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
	// Close the pipe before the last sync, so what it is still holding is on
	// disk rather than in flight when this process ends.
	if l.detach != nil {
		l.detach()
		if l.sync != nil {
			_ = l.sync()
		}
	}
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
	if l.human != nil {
		if line := readable(rec); line != "" {
			fmt.Fprintln(l.human, line)
		}
	}

	// Durability is the whole reason this package exists rather than a plain
	// logger. A failure here is not fatal: losing the guarantee is better than
	// refusing to shut a machine down because journald was busy.
	if l.sync != nil {
		_ = l.sync()
	}
}

// readable renders one line for a person at a terminal. The JSON is the record;
// this is the running commentary, and it exists because an operator rehearsing a
// shutdown should be able to see what happened without a parser.
func readable(rec line) string {
	at := rec.Time[11:19]
	switch rec.Event {
	case evStepStart:
		return fmt.Sprintf("%s  %-18s %s", at, short(rec.Action), rec.Target)
	case evStepEnd:
		if rec.Note != "" {
			n := strings.Count(rec.Note, "\n") + 1
			return fmt.Sprintf("%s  %-18s recorded %d workloads", at, "", n)
		}
		out := fmt.Sprintf("%s  %-18s %-8s %6dms", at, "", rec.Outcome, rec.ElapsedMS)
		if rec.Err != "" {
			out += "  " + rec.Err
		}
		return out
	case evRunEnd:
		state := "complete"
		if rec.Aborted != nil && *rec.Aborted {
			state = "abandoned"
		} else if rec.Complete != nil && !*rec.Complete {
			state = "incomplete"
		}
		return fmt.Sprintf("%s  %-18s %s in %dms", at, "run", state, rec.DurationMS)
	}
	return ""
}

// short drops the namespace from an action, which is repeated on every line and
// carries nothing once you know what you are reading.
func short(action string) string {
	if _, rest, ok := strings.Cut(action, "."); ok {
		return rest
	}
	return action
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
