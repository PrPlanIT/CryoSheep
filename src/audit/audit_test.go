package audit

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// clock returns a deterministic time source advancing by step on each call.
func clock(step time.Duration) func() time.Time {
	t := time.Date(2026, 9, 17, 3, 18, 32, 0, time.UTC)
	return func() time.Time {
		now := t
		t = t.Add(step)
		return now
	}
}

func write(t *testing.T, f func(*Log)) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	syncs := 0
	l := open(&buf, func() error { syncs++; return nil }, clock(time.Second), "host-a", TriggerUPS)
	f(l)
	return buf.String(), syncs
}

// Every line is forced to disk before the next one is written; that guarantee is
// the reason this package exists rather than a plain logger.
func TestEveryLineIsSynced(t *testing.T) {
	out, syncs := write(t, func(l *Log) {
		i := l.StepStart("k8s.cordon", "host-a", "OB")
		l.StepEnd(i, OutcomeDone, nil)
		_ = l.Close(true, false)
	})
	lines := strings.Count(strings.TrimSpace(out), "\n") + 1
	if syncs != lines {
		t.Fatalf("%d lines but %d syncs — a line can be lost to a power cut", lines, syncs)
	}
}

// A step that began and never ended is the shape of a machine cut mid-sequence.
// Without a start line there would be nothing at all to show for it.
func TestStepStartIsWrittenBeforeTheStepRuns(t *testing.T) {
	out, _ := write(t, func(l *Log) { l.StepStart("k8s.csi.unmount", "", "OB") })
	if !strings.Contains(out, `"ev":"step.start"`) {
		t.Fatalf("no start line written:\n%s", out)
	}
	if strings.Contains(out, `"ev":"step.end"`) {
		t.Fatalf("end line written for a step that never ended:\n%s", out)
	}
}

func TestRoundTripsThroughTheReader(t *testing.T) {
	out, _ := write(t, func(l *Log) {
		i := l.StepStart("k8s.quorum.record", "host-a", "OB")
		l.Note(i, "replica pg-1")
		l.StepEnd(i, OutcomeDone, nil)
		j := l.StepStart("k8s.cordon", "host-a", "OB")
		l.StepEnd(j, OutcomeFailed, errors.New("apiserver unreachable"))
		_ = l.Close(false, true)
	})

	runs := parseRuns(out)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.Host != "host-a" || r.Trigger != TriggerUPS {
		t.Fatalf("identity lost: %+v", r)
	}
	if !r.Aborted || r.Complete {
		t.Fatalf("closing state lost: complete=%v aborted=%v", r.Complete, r.Aborted)
	}
	if len(r.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(r.Steps))
	}
	if r.Steps[0].Note != "replica pg-1" {
		t.Fatalf("note lost: %q", r.Steps[0].Note)
	}
	if r.Steps[0].Outcome != OutcomeDone || !r.Steps[0].Finished {
		t.Fatalf("step 0 not reconstructed: %+v", r.Steps[0])
	}
	if !strings.Contains(r.Steps[1].Err, "apiserver") {
		t.Fatalf("error lost: %q", r.Steps[1].Err)
	}
}

// A note must not be mistaken for the end of the step it annotates.
func TestNoteDoesNotEndItsStep(t *testing.T) {
	out, _ := write(t, func(l *Log) {
		i := l.StepStart("k8s.quorum.record", "host-a", "")
		l.Note(i, "evidence")
	})
	if s := parseRuns(out)[0].Steps[0]; s.Finished {
		t.Fatalf("step marked finished by a note alone: %+v", s)
	}
}

func TestWasCutDistinguishesAnUnfinishedRun(t *testing.T) {
	cut, _ := write(t, func(l *Log) {
		i := l.StepStart("host.poweroff", "host-a", "")
		_ = i // power goes here: no end line, no close
	})
	if !parseRuns(cut)[0].WasCut() {
		t.Fatal("a run that stopped mid-step should read as cut")
	}

	clean, _ := write(t, func(l *Log) {
		i := l.StepStart("k8s.cordon", "host-a", "")
		l.StepEnd(i, OutcomeDone, nil)
		_ = l.Close(true, false)
	})
	if parseRuns(clean)[0].WasCut() {
		t.Fatal("a completed run should not read as cut")
	}
}

// Reversal walks the most recent run, so ordering across boots must be by time.
func TestRunsComeBackNewestFirst(t *testing.T) {
	var buf bytes.Buffer
	base := time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)
	for i, h := range []string{"older", "newer"} {
		at := base.Add(time.Duration(i) * time.Hour)
		l := open(&buf, nil, func() time.Time { t := at; at = at.Add(time.Second); return t }, h, TriggerUPS)
		_ = l.Close(true, false)
	}
	runs := parseRuns(buf.String())
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].Started.Before(runs[1].Started) {
		t.Fatalf("runs not newest-first: %v then %v", runs[0].Started, runs[1].Started)
	}
}

// journald carries other things under the same tag; anything unparseable is
// skipped rather than fatal.
func TestForeignLinesAreIgnored(t *testing.T) {
	out, _ := write(t, func(l *Log) { _ = l.Close(true, false) })
	mixed := "starting cryosheep\n" + out + "{not json\n"
	if runs := parseRuns(mixed); len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
}

func TestNoteIsBounded(t *testing.T) {
	out, _ := write(t, func(l *Log) {
		i := l.StepStart("k8s.quorum.record", "", "")
		l.Note(i, strings.Repeat("x", MaxNote*2))
	})
	if n := len(parseRuns(out)[0].Steps[0].Note); n > MaxNote {
		t.Fatalf("note is %d bytes, over the %d budget", n, MaxNote)
	}
}
