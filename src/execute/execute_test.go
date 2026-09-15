package execute

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/journal"
	"github.com/PrPlanIT/CryoSheep/src/plan"
)

type fakeUPS struct {
	statuses []string // consumed one per gate
	err      error
	calls    int
}

func (f *fakeUPS) Status(context.Context) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if len(f.statuses) == 0 {
		return core.StatusOnBattery, nil
	}
	s := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return s, nil
}

type fakeHyp struct {
	stopped []string
	err     error
}

func (f *fakeHyp) Guests(context.Context) ([]core.Guest, error) { return nil, nil }
func (f *fakeHyp) Shutdown(_ context.Context, id string, _ time.Duration) error {
	f.stopped = append(f.stopped, id)
	return f.err
}

type fakeCeph struct {
	set, unset int
	err        error
}

func (f *fakeCeph) SetNoout(context.Context) error   { f.set++; return f.err }
func (f *fakeCeph) UnsetNoout(context.Context) error { f.unset++; return nil }

type fakeHost struct{ halted int }

func (f *fakeHost) Poweroff(context.Context) error { f.halted++; return nil }

type recStep struct {
	action, target, gate, outcome string
	failed                        bool
}

type recorder struct{ steps []recStep }

func (r *recorder) StepStart(action, target, gate string) int {
	r.steps = append(r.steps, recStep{action: action, target: target, gate: gate})
	return len(r.steps) - 1
}
func (r *recorder) StepEnd(i int, outcome string, err error) {
	r.steps[i].outcome = outcome
	r.steps[i].failed = err != nil
}

func testPlan() plan.Plan {
	return plan.Build("eggplant",
		[]core.Role{core.RoleProxmox, core.RoleCephOSD},
		[]core.Guest{
			{ID: "102", Name: "a", Status: "running"},
			{ID: "107", Name: "b", Status: "running"},
		},
		core.StatusOnBattery, plan.Options{})
}

func TestHappyPathRunsEveryStep(t *testing.T) {
	hyp, ceph, host, rec := &fakeHyp{}, &fakeCeph{}, &fakeHost{}, &recorder{}
	e := &Executor{Hyp: hyp, Ceph: ceph, Host: host, UPS: &fakeUPS{}, Record: rec}

	res := e.Run(context.Background(), testPlan())
	if !res.Completed || res.Aborted {
		t.Fatalf("result = %+v, want completed", res)
	}
	if ceph.set != 1 || len(hyp.stopped) != 2 || host.halted != 1 {
		t.Fatalf("ceph=%d guests=%v halted=%d", ceph.set, hyp.stopped, host.halted)
	}
	if len(rec.steps) != 4 {
		t.Fatalf("recorded %d steps, want 4", len(rec.steps))
	}
}

// Mains positively back before the point of no return: abandon and reverse.
func TestMainsReturningAbortsAndReverses(t *testing.T) {
	hyp, ceph, host, rec := &fakeHyp{}, &fakeCeph{}, &fakeHost{}, &recorder{}
	e := &Executor{Hyp: hyp, Ceph: ceph, Host: host, Record: rec,
		UPS: &fakeUPS{statuses: []string{core.StatusOnline}}}

	res := e.Run(context.Background(), testPlan())
	if !res.Aborted || res.Completed {
		t.Fatalf("result = %+v, want aborted", res)
	}
	if len(hyp.stopped) != 0 || host.halted != 0 {
		t.Fatalf("aborted run still acted: guests=%v halted=%d", hyp.stopped, host.halted)
	}
}

func TestAbortAfterNooutUndoesIt(t *testing.T) {
	ceph, rec := &fakeCeph{}, &recorder{}
	// on battery for the noout gate, back on mains for the next one
	ups := &fakeUPS{statuses: []string{core.StatusOnBattery, core.StatusOnline}}
	e := &Executor{Hyp: &fakeHyp{}, Ceph: ceph, Host: &fakeHost{}, UPS: ups, Record: rec}

	res := e.Run(context.Background(), testPlan())
	if !res.Aborted {
		t.Fatalf("want abort, got %+v", res)
	}
	if ceph.set != 1 || ceph.unset != 1 {
		t.Fatalf("noout set=%d unset=%d; an abort must undo what it did", ceph.set, ceph.unset)
	}
	if res.Reversed != 1 {
		t.Fatalf("Reversed = %d, want 1", res.Reversed)
	}
}

// Blindness must never change a decision. Unreadable is not "mains are back".
func TestUnreadableUPSDoesNotAbort(t *testing.T) {
	hyp, host, rec := &fakeHyp{}, &fakeHost{}, &recorder{}
	e := &Executor{Hyp: hyp, Ceph: &fakeCeph{}, Host: host, Record: rec,
		UPS: &fakeUPS{err: errors.New("no route to upsd")}}

	res := e.Run(context.Background(), testPlan())
	if res.Aborted {
		t.Fatal("an unreadable UPS aborted the sequence; blindness must not stop a shutdown")
	}
	if !res.Completed || host.halted != 1 {
		t.Fatalf("result = %+v halted=%d, want a completed halt", res, host.halted)
	}
	if rec.steps[0].gate != "unreadable" {
		t.Fatalf("gate recorded as %q, want the blindness to be visible in the record", rec.steps[0].gate)
	}
}

// "OB LB" carries OB. Treating status as a whole string would read a dying UPS
// as something else entirely.
func TestOnBatteryLowDoesNotLookLikeMains(t *testing.T) {
	host := &fakeHost{}
	e := &Executor{Hyp: &fakeHyp{}, Ceph: &fakeCeph{}, Host: host,
		UPS: &fakeUPS{statuses: []string{"OB LB"}}}
	if res := e.Run(context.Background(), testPlan()); res.Aborted {
		t.Fatal(`"OB LB" was treated as mains returning`)
	}
}

// Gating stops once the host is committed, so mains returning late cannot leave
// it half-stopped. The final gate sits immediately before the committing step.
func TestGatingStopsOnceCommitted(t *testing.T) {
	p := testPlan()
	// Stay on battery throughout so the walk runs to completion and every gate
	// that exists is actually consulted.
	ups := &fakeUPS{statuses: []string{core.StatusOnBattery}}
	e := &Executor{Hyp: &fakeHyp{}, Ceph: &fakeCeph{}, Host: &fakeHost{}, UPS: ups}

	_ = e.Run(context.Background(), p)
	want := p.PointOfNoReturn() + 1 // every reversible step, plus the last chance
	if ups.calls != want {
		t.Fatalf("UPS consulted %d times, want %d (reversible steps plus the final pre-commit gate)", ups.calls, want)
	}
}

// The worst outcome is a host left running until the battery dies, so a failed
// step must not stop the walk.
func TestFailedStepStillReachesTheHalt(t *testing.T) {
	hyp := &fakeHyp{err: errors.New("guest will not stop")}
	host, rec := &fakeHost{}, &recorder{}
	e := &Executor{Hyp: hyp, Ceph: &fakeCeph{}, Host: host, UPS: &fakeUPS{}, Record: rec}

	res := e.Run(context.Background(), testPlan())
	if host.halted != 1 {
		t.Fatal("a failing guest prevented the halt; the host would run until the battery died")
	}
	if !res.Completed {
		t.Fatalf("result = %+v, want completed despite the failure", res)
	}
	var failed int
	for _, s := range rec.steps {
		if s.failed {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("recorded %d failures, want both guest steps recorded as failed", failed)
	}
}

func TestCephFailureDoesNotStopTheWalk(t *testing.T) {
	ceph := &fakeCeph{err: errors.New("mon unreachable")}
	host := &fakeHost{}
	e := &Executor{Hyp: &fakeHyp{}, Ceph: ceph, Host: host, UPS: &fakeUPS{}}
	if res := e.Run(context.Background(), testPlan()); !res.Completed || host.halted != 1 {
		t.Fatalf("result = %+v halted=%d, want the halt to still happen", res, host.halted)
	}
}

func TestDryRunPerformsNothing(t *testing.T) {
	hyp, ceph, host, rec := &fakeHyp{}, &fakeCeph{}, &fakeHost{}, &recorder{}
	e := &Executor{Hyp: hyp, Ceph: ceph, Host: host, UPS: &fakeUPS{}, Record: rec, DryRun: true}

	res := e.Run(context.Background(), testPlan())
	if !res.Completed {
		t.Fatalf("result = %+v", res)
	}
	if ceph.set != 0 || len(hyp.stopped) != 0 || host.halted != 0 {
		t.Fatalf("dry run acted: ceph=%d guests=%v halted=%d", ceph.set, hyp.stopped, host.halted)
	}
	for _, s := range rec.steps {
		if s.outcome != journal.OutcomeSkipped {
			t.Fatalf("dry-run step %q recorded as %q, want skipped", s.action, s.outcome)
		}
	}
	if len(rec.steps) != 4 {
		t.Fatalf("dry run recorded %d steps, want the full walk", len(rec.steps))
	}
}

func TestGateStatusIsRecordedOnEachGatedStep(t *testing.T) {
	rec := &recorder{}
	e := &Executor{Hyp: &fakeHyp{}, Ceph: &fakeCeph{}, Host: &fakeHost{}, Record: rec,
		UPS: &fakeUPS{statuses: []string{"OB"}}}
	_ = e.Run(context.Background(), testPlan())
	if rec.steps[0].gate != "OB" {
		t.Fatalf("gate = %q, want OB recorded against the step it guarded", rec.steps[0].gate)
	}
	// Steps past the point of no return are not gated, so they carry no status.
	if rec.steps[len(rec.steps)-1].gate != "" {
		t.Fatalf("ungated step carries a gate value: %+v", rec.steps[len(rec.steps)-1])
	}
}

// A normal reboot reads OL. Without NoGate the executor would "abort" a
// shutdown systemd is performing regardless, leaving guests running while the
// host stops underneath them.
func TestNoGateCompletesOnMains(t *testing.T) {
	hyp, host := &fakeHyp{}, &fakeHost{}
	e := &Executor{Hyp: hyp, Ceph: &fakeCeph{}, Host: host, NoGate: true,
		UPS: &fakeUPS{statuses: []string{core.StatusOnline}}}

	res := e.Run(context.Background(), testPlan())
	if res.Aborted {
		t.Fatal("a normal reboot on mains was aborted")
	}
	if len(hyp.stopped) != 2 || host.halted != 1 {
		t.Fatalf("guests=%v halted=%d, want both guests stopped and the host halted", hyp.stopped, host.halted)
	}
}

func TestNoGateNeverConsultsTheUPS(t *testing.T) {
	ups := &fakeUPS{statuses: []string{core.StatusOnline}}
	e := &Executor{Hyp: &fakeHyp{}, Ceph: &fakeCeph{}, Host: &fakeHost{}, UPS: ups, NoGate: true}
	_ = e.Run(context.Background(), testPlan())
	if ups.calls != 0 {
		t.Fatalf("UPS consulted %d times with NoGate set, want 0", ups.calls)
	}
}
