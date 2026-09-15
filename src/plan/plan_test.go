package plan

import (
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

func actions(p Plan) []Action {
	out := make([]Action, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Action)
	}
	return out
}

func eq(t *testing.T, got, want []Action) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}
}

func TestBareHostJustHalts(t *testing.T) {
	p := Build("plain", nil, nil, core.StatusOnBattery, Options{})
	eq(t, actions(p), []Action{ActionHostHalt})
}

func TestCephNooutComesBeforeGuests(t *testing.T) {
	guests := []core.Guest{{ID: "102", Name: "chest-002", Status: "running"}}
	p := Build("eggplant", []core.Role{core.RoleProxmox, core.RoleCephOSD}, guests, core.StatusOnBattery, Options{})
	eq(t, actions(p), []Action{ActionCephNoout, ActionGuestStop, ActionHostHalt})
}

// Storage must be quiet before the machines using it disappear, so a host with
// no OSD still stops its guests before halting.
func TestGuestsStopBeforeHalt(t *testing.T) {
	guests := []core.Guest{
		{ID: "102", Name: "a", Status: "running"},
		{ID: "107", Name: "b", Status: "running"},
	}
	p := Build("bamboo", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{})
	eq(t, actions(p), []Action{ActionGuestStop, ActionGuestStop, ActionHostHalt})
	if p.Steps[0].Target != "102" || p.Steps[1].Target != "107" {
		t.Fatalf("guest order not preserved: %+v", p.Steps)
	}
}

// A stopped guest is already where the sequence wants it.
func TestStoppedGuestsProduceNoStep(t *testing.T) {
	guests := []core.Guest{
		{ID: "102", Name: "a", Status: "stopped"},
		{ID: "107", Name: "b", Status: "running"},
	}
	p := Build("cosmos", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{})
	eq(t, actions(p), []Action{ActionGuestStop, ActionHostHalt})
	if p.Steps[0].Target != "107" {
		t.Fatalf("stopped guest was not skipped: %+v", p.Steps)
	}
}

// The gate boundary is the contract the executor relies on: everything before
// it may be abandoned, everything from it on commits the host.
func TestPointOfNoReturnIsFirstIrreversibleStep(t *testing.T) {
	guests := []core.Guest{{ID: "102", Name: "a", Status: "running"}}
	p := Build("eggplant", []core.Role{core.RoleCephOSD}, guests, core.StatusOnBattery, Options{})
	if got := p.PointOfNoReturn(); got != 1 {
		t.Fatalf("PointOfNoReturn = %d, want 1 (noout reversible, guest stop not)", got)
	}
	if !p.Steps[0].Reversible {
		t.Fatal("noout must be reversible")
	}
	if p.Steps[1].Reversible {
		t.Fatal("stopping a guest must not be reversible")
	}
}

func TestEverythingReversibleReportsLength(t *testing.T) {
	p := Plan{Steps: []Step{{Action: ActionCephNoout, Reversible: true}}}
	if got := p.PointOfNoReturn(); got != 1 {
		t.Fatalf("PointOfNoReturn = %d, want 1 (== len)", got)
	}
}

func TestGuestTimeoutDefaultsAndOverrides(t *testing.T) {
	guests := []core.Guest{{ID: "102", Status: "running"}}
	p := Build("h", nil, guests, core.StatusOnBattery, Options{})
	if p.Steps[0].Timeout != 90*time.Second {
		t.Fatalf("default guest timeout = %v, want 90s", p.Steps[0].Timeout)
	}
	p = Build("h", nil, guests, core.StatusOnBattery, Options{GuestTimeout: 30 * time.Second})
	if p.Steps[0].Timeout != 30*time.Second {
		t.Fatalf("override guest timeout = %v, want 30s", p.Steps[0].Timeout)
	}
}
