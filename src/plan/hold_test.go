package plan

import (
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

// The estate shape: workloads and leaves together, then the core held until the
// slowest workload anywhere is down.
var estateGroups = []OrderGroup{{Lo: 4, Hi: 99}, {Lo: 1, Hi: 3, NotBefore: 270 * time.Second}}

func TestAHeldGroupCarriesItsHoldAndWhatItWaitsOn(t *testing.T) {
	guests := []core.Guest{
		{ID: "206", Status: "running", Order: 4, Down: 240 * time.Second},
		{ID: "110", Status: "running", Order: 5, Down: 90 * time.Second},
		{ID: "200", Status: "running", Order: 1, Down: 120 * time.Second},
	}
	p := Build("bamboo", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery,
		Options{GuestConcurrency: AllAtOnce, OrderGroups: estateGroups})

	for _, s := range p.Steps {
		if s.Action != ActionGuestStop {
			continue
		}
		switch s.Target {
		case "206", "110":
			if s.NotBefore != 0 || s.Clears != nil {
				t.Fatalf("workload %s is held: %+v", s.Target, s)
			}
		case "200":
			if s.NotBefore != 270*time.Second {
				t.Fatalf("router NotBefore = %v, want 270s", s.NotBefore)
			}
			if len(s.Clears) != 1 || s.Clears[0].Lo != 4 || s.Clears[0].Hi != 99 {
				t.Fatalf("router Clears = %v, want the workload group", s.Clears)
			}
		}
	}
}

// The first group has nothing before it to wait on, so a hold there is ignored.
func TestTheFirstGroupIsNeverHeld(t *testing.T) {
	groups := []OrderGroup{{Lo: 4, Hi: 99, NotBefore: time.Minute}, {Lo: 1, Hi: 3}}
	p := Build("h", []core.Role{core.RoleProxmox},
		[]core.Guest{{ID: "1", Status: "running", Order: 4}}, core.StatusOnBattery,
		Options{GuestConcurrency: AllAtOnce, OrderGroups: groups})
	if p.Steps[0].NotBefore != 0 {
		t.Fatalf("first group held for %v", p.Steps[0].NotBefore)
	}
}

// Holds count at their ceiling in the worst case.
func TestWorstCaseCountsHoldsAtTheirCeiling(t *testing.T) {
	guests := []core.Guest{
		{ID: "206", Status: "running", Order: 4, Down: 240 * time.Second},
		{ID: "200", Status: "running", Order: 1, Down: 120 * time.Second},
	}
	p := Build("bamboo", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery,
		Options{GuestConcurrency: AllAtOnce, OrderGroups: estateGroups, HoldUntil: 400 * time.Second})

	// workloads 240s; router not before 270s, then 120s → 390s; halt not before 400s.
	if got := p.WorstCase(); got != 400*time.Second {
		t.Fatalf("WorstCase = %v, want 400s", got)
	}
}

func TestShedStopsOnlyTaggedRunningGuests(t *testing.T) {
	guests := []core.Guest{
		{ID: "1", Status: "running", Tags: []string{"docker", core.TagShed}},
		{ID: "2", Status: "running", Tags: []string{"dns"}},
		{ID: "3", Status: "stopped", Tags: []string{core.TagShed}},
		{ID: "4", Status: "running", Tags: []string{core.TagShed}, Down: 30 * time.Second},
	}
	p := Shed("cosmos", guests, core.StatusOnBattery, Options{})
	got := actions(p)
	if len(got) != 2 {
		t.Fatalf("steps = %+v, want guests 1 and 4 only", p.Steps)
	}
	for _, s := range p.Steps {
		if s.Action != ActionGuestStop || !s.Reversible || s.Wave != 1 {
			t.Fatalf("shed step %+v: want a reversible guest stop in one wave", s)
		}
	}
	if p.Steps[1].Target != "4" || p.Steps[1].Timeout != 30*time.Second {
		t.Fatalf("guest 4 = %+v, want its own down= budget", p.Steps[1])
	}
	if p.PointOfNoReturn() != len(p.Steps) {
		t.Fatal("a shed plan committed the host")
	}
}

func TestCovers(t *testing.T) {
	g := []OrderGroup{{Lo: 4, Hi: 99}}
	if !Covers(g, core.Guest{Order: core.OrderUnset}) {
		t.Fatal("an unordered guest is in the last group")
	}
	if Covers(g, core.Guest{Order: 1}) {
		t.Fatal("order 1 is not in 4-99")
	}
}

func TestTheCutIsTheLastStepBeforeTheHalt(t *testing.T) {
	g := []core.Guest{{ID: "206", Status: "running"}}
	p := Build("bamboo", []core.Role{core.RoleProxmox}, g, core.StatusOnBattery, Options{UPSCut: 45 * time.Second})
	n := len(p.Steps)
	if n < 2 || p.Steps[n-2].Action != ActionUPSCut || p.Steps[n-1].Action != ActionHostHalt {
		t.Fatalf("steps = %v, want ... cut, halt", actions(p))
	}
	if p.Steps[n-2].Reversible || p.Steps[n-2].After != 45*time.Second {
		t.Fatalf("cut step %+v", p.Steps[n-2])
	}
	if p.PointOfNoReturn() != n-2 {
		t.Fatal("the cut is the commitment, so the barrier holds before it")
	}

	for _, o := range []Options{{}, {UPSCut: 45 * time.Second, OmitHalt: true}} {
		for _, s := range Build("bamboo", []core.Role{core.RoleProxmox}, g, core.StatusOnBattery, o).Steps {
			if s.Action == ActionUPSCut {
				t.Fatalf("options %+v sent a cut", o)
			}
		}
	}
}
