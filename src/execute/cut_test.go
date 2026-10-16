package execute

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/plan"
)

type fakeCutter struct {
	after []time.Duration
	err   error
	order *[]string
}

func (f *fakeCutter) Cut(ctx context.Context, after time.Duration) error {
	f.after = append(f.after, after)
	if f.order != nil {
		*f.order = append(*f.order, "cut")
	}
	return f.err
}

type orderedHost struct{ order *[]string }

func (h orderedHost) Poweroff(context.Context) error { *h.order = append(*h.order, "halt"); return nil }

func cutPlan(cut time.Duration) plan.Plan {
	return plan.Build("bamboo", []core.Role{core.RoleProxmox},
		[]core.Guest{{ID: "206", Status: "running"}}, core.StatusOnBattery,
		plan.Options{UPSCut: cut})
}

func TestTheCutIsSentOnceJustBeforeTheHalt(t *testing.T) {
	var order []string
	c := &fakeCutter{order: &order}
	e := &Executor{Hyp: &fakeHyp{}, UPS: &fakeUPS{}, Cutter: c, Host: orderedHost{&order}}

	e.Run(context.Background(), cutPlan(45*time.Second))
	if len(c.after) != 1 || c.after[0] != 45*time.Second {
		t.Fatalf("cut sent %v, want once with 45s", c.after)
	}
	if len(order) != 2 || order[0] != "cut" || order[1] != "halt" {
		t.Fatalf("order = %v, want cut then halt", order)
	}
}

// A UPS that refuses the cut leaves the NM2's own outage sequence; the host
// still halts.
func TestAFailedCutStillHalts(t *testing.T) {
	host := &fakeHost{}
	e := &Executor{Hyp: &fakeHyp{}, UPS: &fakeUPS{}, Cutter: &fakeCutter{err: errors.New("ERR ACCESS-DENIED")}, Host: host}
	if res := e.Run(context.Background(), cutPlan(45*time.Second)); !res.Completed || host.halted != 1 {
		t.Fatalf("result %+v, halted %d: a failed cut must not stop the halt", res, host.halted)
	}
}

// The cut is a commitment: mains returning before it abandons the run without
// sending it.
func TestMainsBackBeforeTheCutSendsNothing(t *testing.T) {
	c := &fakeCutter{}
	e := &Executor{Hyp: &fakeHyp{}, UPS: &fakeUPS{statuses: []string{core.StatusOnline}},
		Cutter: c, Host: &fakeHost{}, PowerLoss: true}
	res := e.Run(context.Background(), cutPlan(45*time.Second))
	if !res.Aborted || len(c.after) != 0 {
		t.Fatalf("result %+v, cuts %v: want abandoned with no cut", res, c.after)
	}
}

// A sequence that used up its time still sends the cut.
func TestTheCutRunsPastAnExpiredSequence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &fakeCutter{}
	e := &Executor{Cutter: c}
	s := plan.Step{Action: plan.ActionUPSCut, After: time.Minute}
	if _, err := e.perform(e.stepCtx(ctx, s), s); err != nil || len(c.after) != 1 {
		t.Fatalf("err %v, cuts %v: the cut must run on its own context", err, c.after)
	}
}
