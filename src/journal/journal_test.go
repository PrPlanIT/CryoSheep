package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clock(start time.Time, step time.Duration) func() time.Time {
	n := -1
	return func() time.Time {
		n++
		return start.Add(time.Duration(n) * step)
	}
}

func TestRecordsStepsAndFinalises(t *testing.T) {
	dir := t.TempDir()
	w, err := open(dir, "eggplant", TriggerUPS, clock(time.Unix(0, 0).UTC(), time.Second))
	if err != nil {
		t.Fatal(err)
	}
	i := w.StepStart("guest.shutdown", "102", "OB")
	w.StepEnd(i, OutcomeDone, nil)
	if err := w.Close(true, false); err != nil {
		t.Fatal(err)
	}

	runs, err := Load(dir)
	if err != nil || len(runs) != 1 {
		t.Fatalf("Load = %v, %v", runs, err)
	}
	r := runs[0]
	if r.Host != "eggplant" || r.Trigger != TriggerUPS || !r.Complete || r.Aborted {
		t.Fatalf("run wrong: %+v", r)
	}
	if len(r.Steps) != 1 || r.Steps[0].Outcome != OutcomeDone || r.Steps[0].Gate != "OB" {
		t.Fatalf("steps wrong: %+v", r.Steps)
	}
	if r.Steps[0].Elapsed <= 0 || r.Duration <= 0 {
		t.Fatalf("timings not recorded: %+v", r)
	}
}

// The case the record exists for: power cut mid-step. The file must already be
// valid and must name what was being attempted.
func TestFileIsValidAndInFlightBeforeAStepCompletes(t *testing.T) {
	dir := t.TempDir()
	w, _ := open(dir, "h", TriggerUPS, clock(time.Unix(0, 0).UTC(), time.Second))
	w.StepStart("guest.shutdown", "107", "OB LB")
	// no StepEnd, no Close — simulating the power going here

	b, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("file not valid JSON mid-run: %v", err)
	}
	if r.Complete {
		t.Fatal("an unfinished run must not be marked complete")
	}
	if len(r.Steps) != 1 || r.Steps[0].Outcome != "in-flight" {
		t.Fatalf("in-flight step not recorded: %+v", r.Steps)
	}
	if r.Steps[0].Target != "107" {
		t.Fatalf("lost what it was attempting: %+v", r.Steps[0])
	}
}

// Calibration must never be dragged down by runs that were cut short.
func TestIncompleteRunIsMarkedIncomplete(t *testing.T) {
	dir := t.TempDir()
	w, _ := open(dir, "h", TriggerUPS, clock(time.Unix(0, 0).UTC(), time.Second))
	i := w.StepStart("guest.shutdown", "102", "OB")
	w.StepEnd(i, OutcomeAborted, nil)
	if err := w.Close(false, true); err != nil {
		t.Fatal(err)
	}
	runs, _ := Load(dir)
	if runs[0].Complete || !runs[0].Aborted {
		t.Fatalf("want incomplete+aborted, got %+v", runs[0])
	}
}

func TestErrorsAreBounded(t *testing.T) {
	dir := t.TempDir()
	w, _ := open(dir, "h", TriggerManual, clock(time.Unix(0, 0).UTC(), time.Second))
	i := w.StepStart("guest.shutdown", "102", "")
	w.StepEnd(i, OutcomeFailed, errors.New(strings.Repeat("x", 5000)))
	_ = w.Close(false, false)

	runs, _ := Load(dir)
	if n := len(runs[0].Steps[0].Err); n > maxErr+4 {
		t.Fatalf("error recorded at %d bytes; a noisy command must not bloat the record", n)
	}
}

func TestShippingIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, _ := open(dir, "h", TriggerUPS, clock(time.Unix(0, 0).UTC(), time.Second))
	_ = w.Close(true, false)

	un, _ := Unshipped(dir)
	if len(un) != 1 {
		t.Fatalf("Unshipped = %d, want 1", len(un))
	}
	if err := MarkShipped(dir, w.RunID()); err != nil {
		t.Fatal(err)
	}
	if err := MarkShipped(dir, w.RunID()); err != nil {
		t.Fatalf("re-marking must be harmless: %v", err)
	}
	un, _ = Unshipped(dir)
	if len(un) != 0 {
		t.Fatalf("Unshipped = %d after shipping, want 0 (a reboot must not resend)", len(un))
	}
}

func TestPruneKeepsNewestAndNeverDropsUnshipped(t *testing.T) {
	dir := t.TempDir()
	base := time.Unix(1_700_000_000, 0).UTC()

	// 6 shipped runs, oldest first.
	for i := 0; i < 6; i++ {
		r := Run{
			ID:       fmt.Sprintf("2026010%dT000000Z", i),
			Host:     "h",
			Started:  base.Add(time.Duration(i) * time.Hour),
			Complete: true,
			Shipped:  true,
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		os.WriteFile(filepath.Join(dir, r.ID+".json"), b, 0o644)
	}
	// One ancient run that never reached anywhere.
	old := Run{ID: "20250101T000000Z", Host: "h", Started: base.Add(-999 * time.Hour), Complete: true}
	b, _ := json.MarshalIndent(old, "", "  ")
	os.WriteFile(filepath.Join(dir, old.ID+".json"), b, 0o644)

	if _, err := Prune(dir, 3); err != nil {
		t.Fatal(err)
	}
	runs, _ := Load(dir)

	var kept []string
	unshippedSurvived := false
	for _, r := range runs {
		kept = append(kept, r.ID)
		if r.ID == old.ID {
			unshippedSurvived = true
		}
	}
	if !unshippedSurvived {
		t.Fatalf("pruned an unshipped run — evidence that never left the host: %v", kept)
	}
	if len(runs) != 4 { // 3 newest shipped + the unshipped one
		t.Fatalf("kept %d runs (%v), want 4", len(runs), kept)
	}
}

func TestLoadSkipsUnparsableFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644)
	w, _ := open(dir, "h", TriggerUPS, clock(time.Unix(0, 0).UTC(), time.Second))
	_ = w.Close(true, false)

	runs, err := Load(dir)
	if err != nil {
		t.Fatalf("one bad file must not fail the load: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1 good one", len(runs))
	}
}

// A run leaves exactly one file: no temp files, no per-step artefacts.
func TestLeavesOneFilePerRun(t *testing.T) {
	dir := t.TempDir()
	w, _ := open(dir, "h", TriggerSystemd, clock(time.Unix(0, 0).UTC(), time.Second))
	for i := 0; i < 5; i++ {
		n := w.StepStart("guest.shutdown", fmt.Sprint(100+i), "OL")
		w.StepEnd(n, OutcomeDone, nil)
	}
	_ = w.Close(true, false)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("run left %d files (%v), want 1", len(entries), names)
	}
}
