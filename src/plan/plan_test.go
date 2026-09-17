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
//
// Only the halt commits. Stopping a guest is undoable — it can be started again
// — and treating it as final is what would let a recovered outage complete into
// a real one.
func TestOnlyTheHaltCommitsTheHost(t *testing.T) {
	guests := []core.Guest{{ID: "102", Name: "a", Status: "running"}}
	p := Build("eggplant", []core.Role{core.RoleCephOSD}, guests, core.StatusOnBattery, Options{})
	pnr := p.PointOfNoReturn()
	if pnr != len(p.Steps)-1 {
		t.Fatalf("PointOfNoReturn = %d of %d steps, want only the halt to commit", pnr, len(p.Steps))
	}
	if p.Steps[pnr].Action != ActionHostHalt {
		t.Fatalf("committing step is %q, want %q", p.Steps[pnr].Action, ActionHostHalt)
	}
	for i, s := range p.Steps[:pnr] {
		if !s.Reversible {
			t.Fatalf("step %d (%s) is not reversible; it would be ungated", i+1, s.Action)
		}
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

// Proxmox stops guests in reverse startup order, so order=1 — the firewall, the
// resolver — is the last thing down. Getting this backwards would take the
// network out at the start of a sequence that still has work to do.
func TestGuestsStopInReverseStartupOrder(t *testing.T) {
	guests := []core.Guest{
		{ID: "100", Name: "pfSense", Status: "running", Order: 1},
		{ID: "105", Name: "k8s-a", Status: "running", Order: 5},
		{ID: "106", Name: "k8s-b", Status: "running", Order: 5},
		{ID: "707", Name: "dock", Status: "running", Order: 9},
	}
	p := Build("avocado", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{})
	var got []string
	for _, s := range p.Steps {
		if s.Action == ActionGuestStop {
			got = append(got, s.Detail)
		}
	}
	want := []string{"dock", "k8s-a", "k8s-b", "pfSense"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stop order = %v, want %v (order=1 last)", got, want)
		}
	}
}

// A guest nobody declared important stops before one that was.
func TestUnorderedGuestsStopFirst(t *testing.T) {
	guests := []core.Guest{
		{ID: "100", Name: "pfSense", Status: "running", Order: 1},
		{ID: "500", Name: "scratch", Status: "running", Order: core.OrderUnset},
		{ID: "501", Name: "scratch2", Status: "running", Order: 0},
	}
	p := Build("h", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{})
	var got []string
	for _, s := range p.Steps {
		if s.Action == ActionGuestStop {
			got = append(got, s.Detail)
		}
	}
	if got[len(got)-1] != "pfSense" {
		t.Fatalf("stop order = %v, want pfSense last", got)
	}
}

// With nothing ordered, qm list order is preserved rather than shuffled.
func TestNoOrdersPreservesListOrder(t *testing.T) {
	guests := []core.Guest{
		{ID: "100", Name: "a", Status: "running", Order: core.OrderUnset},
		{ID: "105", Name: "b", Status: "running", Order: core.OrderUnset},
		{ID: "707", Name: "c", Status: "running", Order: core.OrderUnset},
	}
	p := Build("h", nil, guests, core.StatusOnBattery, Options{})
	want := []string{"a", "b", "c"}
	i := 0
	for _, s := range p.Steps {
		if s.Action == ActionGuestStop {
			if s.Detail != want[i] {
				t.Fatalf("order changed with no startup values: got %s want %s", s.Detail, want[i])
			}
			i++
		}
	}
}

// Proxmox already records how long each guest needs. A flat timeout makes the
// whole sequence as slow as its most patient member, which is how a plan ends up
// longer than the battery.
func TestGuestBudgetComesFromItsOwnDownValue(t *testing.T) {
	guests := []core.Guest{
		{ID: "100", Name: "pfSense", Status: "running", Order: 1, Down: 180 * time.Second},
		{ID: "105", Name: "scratch", Status: "running", Order: core.OrderUnset, Down: 20 * time.Second},
		{ID: "106", Name: "nodown", Status: "running", Order: core.OrderUnset},
	}
	p := Build("h", nil, guests, core.StatusOnBattery, Options{GuestTimeout: 90 * time.Second})
	got := map[string]time.Duration{}
	for _, s := range p.Steps {
		if s.Action == ActionGuestStop {
			got[s.Detail] = s.Timeout
		}
	}
	if got["pfSense"] != 180*time.Second {
		t.Fatalf("pfSense budget = %v, want its own 180s", got["pfSense"])
	}
	if got["scratch"] != 20*time.Second {
		t.Fatalf("scratch budget = %v, want its own 20s", got["scratch"])
	}
	if got["nodown"] != 90*time.Second {
		t.Fatalf("nodown budget = %v, want the default 90s", got["nodown"])
	}
}

// A k8s node gets node-local teardown that stock poweroff does badly: cordon and
// evict before stopping, and release the mounts that otherwise stall systemd.
func TestKubeletNodeGetsItsOwnTeardown(t *testing.T) {
	p := Build("dungeon-chest-001", []core.Role{core.RoleKubelet}, nil, core.StatusOnBattery, Options{})
	var got []Action
	for _, s := range p.Steps {
		got = append(got, s.Action)
	}
	want := []Action{ActionK8sRecord, ActionK8sCordon, ActionK8sUnmount, ActionHostHalt}
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}
}

// Cordon and drain are undoable — an abandoned shutdown uncordons and the
// workloads come back. Releasing mounts is not, and sits after them.
func TestCordonAndDrainAreReversible(t *testing.T) {
	p := Build("node", []core.Role{core.RoleKubelet}, nil, core.StatusOnBattery, Options{})
	for _, s := range p.Steps {
		switch s.Action {
		case ActionK8sRecord, ActionK8sCordon:
			if !s.Reversible {
				t.Fatalf("%s must be reversible", s.Action)
			}
		case ActionK8sUnmount:
			if s.Reversible {
				t.Fatalf("%s must not be reversible", s.Action)
			}
		}
	}
}

// A hypervisor is not a k8s node and must not get kubectl steps.
func TestHypervisorGetsNoKubeSteps(t *testing.T) {
	p := Build("avocado", []core.Role{core.RoleProxmox, core.RoleCephOSD},
		[]core.Guest{{ID: "100", Status: "running"}}, core.StatusOnBattery, Options{})
	for _, s := range p.Steps {
		switch s.Action {
		case ActionK8sRecord, ActionK8sCordon, ActionK8sUnmount:
			t.Fatalf("hypervisor plan contains %s", s.Action)
		}
	}
}

// Run as a systemd shutdown handler, the transition is already in flight. A
// poweroff here races it, and on a reboot turns it into a machine that never
// comes back — a node patched by ansible, dark until somebody walks to it.
func TestOmitHaltLeavesTheFinalPoweroffOut(t *testing.T) {
	roles := []core.Role{core.RoleKubelet}
	p := Build("node-a", roles, nil, "OL", Options{OmitHalt: true})
	for _, s := range p.Steps {
		if s.Action == ActionHostHalt {
			t.Fatalf("host.poweroff planned while systemd is already stopping the machine:\n%+v", p.Steps)
		}
	}
}

func TestHaltIsPlannedWhenNothingElseIsStoppingUs(t *testing.T) {
	p := Build("node-a", []core.Role{core.RoleKubelet}, nil, "OB", Options{})
	last := p.Steps[len(p.Steps)-1]
	if last.Action != ActionHostHalt {
		t.Fatalf("last step is %q, want host.poweroff", last.Action)
	}
}

// Omitting the halt must not quietly change the teardown that precedes it —
// that work is the whole reason the handler runs at all.
func TestOmitHaltChangesNothingBeforeIt(t *testing.T) {
	roles := []core.Role{core.RoleKubelet, core.RoleCephOSD}
	guests := []core.Guest{{ID: "100", Name: "pfsense", Status: "running", Order: 1}}
	full := Build("h", roles, guests, "OB", Options{UPSDeadline: time.Minute})
	short := Build("h", roles, guests, "OB", Options{UPSDeadline: time.Minute, OmitHalt: true})

	if len(short.Steps) != len(full.Steps)-1 {
		t.Fatalf("got %d steps, want %d", len(short.Steps), len(full.Steps)-1)
	}
	for i := range short.Steps {
		if short.Steps[i] != full.Steps[i] {
			t.Fatalf("step %d differs:\n  %+v\n  %+v", i, short.Steps[i], full.Steps[i])
		}
	}
}

// Releasing storage is irreversible whether or not we are the ones halting.
func TestPointOfNoReturnSurvivesOmitHalt(t *testing.T) {
	p := Build("node-a", []core.Role{core.RoleKubelet}, nil, "OB", Options{OmitHalt: true})
	pnr := p.PointOfNoReturn()
	if pnr >= len(p.Steps) {
		t.Fatalf("no irreversible step found in %+v", p.Steps)
	}
	if p.Steps[pnr].Action != ActionK8sUnmount {
		t.Fatalf("point of no return is %q, want k8s.csi.unmount", p.Steps[pnr].Action)
	}
}

// The systemd path is the one that runs most often and the one nobody can watch,
// so it has to be rehearsable. A plan built for it must still describe the whole
// teardown — only the halt is absent.
func TestSystemdPlanStillDescribesTheWholeTeardown(t *testing.T) {
	p := Build("node-a", []core.Role{core.RoleKubelet, core.RoleCephOSD}, nil, "OB",
		Options{UPSDeadline: time.Minute, OmitHalt: true})
	want := []Action{ActionUPSDeadline, ActionCephNoout, ActionK8sRecord, ActionK8sCordon, ActionK8sUnmount}
	if len(p.Steps) != len(want) {
		t.Fatalf("got %d steps, want %d: %+v", len(p.Steps), len(want), p.Steps)
	}
	for i, a := range want {
		if p.Steps[i].Action != a {
			t.Fatalf("step %d is %q, want %q", i, p.Steps[i].Action, a)
		}
	}
}
