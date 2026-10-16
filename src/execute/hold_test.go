package execute

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/plan"
)

// fakeEstate answers with each listing in turn, holding the last.
type fakeEstate struct {
	listings [][]core.Guest
	err      error
	calls    int
}

func (f *fakeEstate) Running(context.Context) ([]core.Guest, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	l := f.listings[0]
	if len(f.listings) > 1 {
		f.listings = f.listings[1:]
	}
	return l, nil
}

// heldPlan is the estate shape: workloads, then the routers held until 270s or
// until no workload runs anywhere, then the halt.
func heldPlan() plan.Plan {
	workloads := []plan.OrderGroup{{Lo: 4, Hi: 99}}
	return plan.Plan{Host: "bamboo", Steps: []plan.Step{
		{Action: plan.ActionGuestStop, Target: "206", Timeout: time.Second, Reversible: true, Wave: 1},
		{Action: plan.ActionGuestStop, Target: "200", Timeout: time.Second, Reversible: true, Wave: 2,
			NotBefore: 270 * time.Second, Clears: workloads},
		{Action: plan.ActionHostHalt, Wave: 3},
	}}
}

// polledClock advances by whatever the executor waits, so a hold measures what
// it would have cost in real time.
func polledClock() (func() time.Time, func(time.Duration) <-chan time.Time, *time.Duration) {
	start := time.Now()
	var elapsed time.Duration
	now := func() time.Time { return start.Add(elapsed) }
	after := func(d time.Duration) <-chan time.Time {
		elapsed += d
		ch := make(chan time.Time, 1)
		ch <- now()
		return ch
	}
	return now, after, &elapsed
}

// The fast path: the routers stop as soon as no workload runs anywhere, not at
// the ceiling.
func TestHoldReleasesOnceTheEstateClears(t *testing.T) {
	now, after, _ := polledClock()
	k8sElsewhere := core.Guest{ID: "506", Node: "eggplant", Status: "running", Order: 4}
	router := core.Guest{ID: "200", Node: "bamboo", Status: "running", Order: 1}
	estate := &fakeEstate{listings: [][]core.Guest{
		{k8sElsewhere, router}, {k8sElsewhere, router}, {router},
	}}
	e := &Executor{Hyp: &fakeHyp{}, Host: &fakeHost{}, NoGate: true, Estate: estate,
		Clock: now, After: after, Poll: 5 * time.Second}

	res := e.Run(context.Background(), heldPlan())
	if !res.Completed {
		t.Fatal("run did not complete")
	}
	if res.Held != 10*time.Second {
		t.Fatalf("Held = %v, want 10s: released on the third reading", res.Held)
	}
}

// Running guests outside the cleared orders do not hold anything back.
func TestHoldIgnoresGuestsItDoesNotWaitOn(t *testing.T) {
	now, after, _ := polledClock()
	estate := &fakeEstate{listings: [][]core.Guest{
		{{ID: "100", Node: "avocado", Status: "running", Order: 1}},
	}}
	e := &Executor{Hyp: &fakeHyp{}, Host: &fakeHost{}, NoGate: true, Estate: estate,
		Clock: now, After: after}

	if res := e.Run(context.Background(), heldPlan()); res.Held != 0 {
		t.Fatalf("Held = %v, want no wait: only routers run", res.Held)
	}
}

// Blindness never shortens a wait.
func TestHoldRunsToTheCeilingWhenTheEstateIsUnreadable(t *testing.T) {
	now, after, _ := polledClock()
	e := &Executor{Hyp: &fakeHyp{}, Host: &fakeHost{}, NoGate: true,
		Estate: &fakeEstate{err: errors.New("pvesh: no quorum")},
		Clock:  now, After: after, Poll: 5 * time.Second}

	if res := e.Run(context.Background(), heldPlan()); res.Held != 270*time.Second {
		t.Fatalf("Held = %v, want the full 270s", res.Held)
	}
}

func TestHoldRunsToTheCeilingWithNoEstate(t *testing.T) {
	now, after, _ := polledClock()
	e := &Executor{Hyp: &fakeHyp{}, Host: &fakeHost{}, NoGate: true, Clock: now, After: after}

	if res := e.Run(context.Background(), heldPlan()); res.Held != 270*time.Second {
		t.Fatalf("Held = %v, want the full 270s", res.Held)
	}
}

// Mains returning during a long hold abandons the sequence then, not after it.
func TestMainsReturningDuringAHoldAbandons(t *testing.T) {
	now, after, elapsed := polledClock()
	hyp, host := &fakeHyp{}, &fakeHost{}
	ups := &fakeUPS{statuses: []string{
		core.StatusOnBattery, // gate before the workloads
		core.StatusOnBattery, // first poll of the hold
		core.StatusOnline,    // second poll
	}}
	e := &Executor{Hyp: hyp, Host: host, UPS: ups, Clock: now, After: after, Poll: 5 * time.Second}

	res := e.Run(context.Background(), heldPlan())
	if !res.Aborted {
		t.Fatal("mains returned during the hold and the sequence went on")
	}
	if *elapsed != 5*time.Second {
		t.Fatalf("abandoned after %v, want at the second poll (5s)", *elapsed)
	}
	if host.halted != 0 || len(hyp.started) != 1 {
		t.Fatalf("halted=%d restarted=%v, want the workload guest back and the host up", host.halted, hyp.started)
	}
}

// The barrier releases once nothing runs anywhere: with nothing writing there
// is nothing for a staggered halt to strand.
func TestBarrierReleasesOnceNothingRuns(t *testing.T) {
	now, after, _ := polledClock()
	other := core.Guest{ID: "506", Node: "eggplant", Status: "running", Order: 4}
	estate := &fakeEstate{listings: [][]core.Guest{{other}, {}}}
	e := &Executor{Hyp: &fakeHyp{}, Host: &fakeHost{}, NoGate: true, Estate: estate,
		Clock: now, After: after, Poll: 5 * time.Second}

	p := waveOfGuests([]string{"1"}, time.Second)
	p.HoldUntil = 240 * time.Second
	if res := e.Run(context.Background(), p); res.Held != 5*time.Second {
		t.Fatalf("Held = %v, want 5s", res.Held)
	}
}

// ctxHost fails like systemctl would if the context were already done.
type ctxHost struct{ halted int }

func (h *ctxHost) Poweroff(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.halted++
	return nil
}

// A run that used up its own time must still reach the poweroff; otherwise the
// host stays up until the battery is flat.
func TestHaltRunsAfterTheSequenceTimedOut(t *testing.T) {
	host := &ctxHost{}
	e := &Executor{Hyp: &fakeHyp{}, Host: host, NoGate: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e.Run(ctx, waveOfGuests([]string{"1"}, time.Second))
	if host.halted != 1 {
		t.Fatal("the halt did not run on an expired sequence")
	}
}
