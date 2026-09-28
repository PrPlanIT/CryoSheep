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
	if p.Steps[0].Timeout != core.DefaultGuestTimeout {
		t.Fatalf("default guest timeout = %v, want %v", p.Steps[0].Timeout, core.DefaultGuestTimeout)
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

// waves returns the wave of every step, so a test can assert grouping without
// caring what the numbers are — only which steps share one.
func waves(p Plan) []int {
	out := make([]int, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Wave)
	}
	return out
}

// guestGroups returns the guest targets grouped by wave, in wave order.
func guestGroups(p Plan) [][]string {
	var out [][]string
	last := -1
	for _, s := range p.Steps {
		if s.Action != ActionGuestStop {
			continue
		}
		if s.Wave != last {
			out = append(out, nil)
			last = s.Wave
		}
		out[len(out)-1] = append(out[len(out)-1], s.Target)
	}
	return out
}

func running(id string, order int) core.Guest {
	return core.Guest{ID: id, Name: "vm" + id, Status: "running", Order: order}
}

func TestGuestConcurrencySerialGivesEachGuestItsOwnWave(t *testing.T) {
	guests := []core.Guest{running("1", core.OrderUnset), running("2", core.OrderUnset), running("3", core.OrderUnset)}
	p := Build("eggplant", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{GuestConcurrency: 1})
	got := guestGroups(p)
	if len(got) != 3 {
		t.Fatalf("serial: got %d waves %v, want 3", len(got), got)
	}
}

func TestGuestConcurrencyZeroStopsAnUnorderedTierAtOnce(t *testing.T) {
	guests := []core.Guest{running("1", core.OrderUnset), running("2", core.OrderUnset), running("3", core.OrderUnset)}
	p := Build("eggplant", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{GuestConcurrency: AllAtOnce})
	got := guestGroups(p)
	if len(got) != 1 || len(got[0]) != 3 {
		t.Fatalf("parallel: got %v, want one wave of three", got)
	}
}

func TestGuestConcurrencyBoundedChunksTheTier(t *testing.T) {
	guests := []core.Guest{
		running("1", core.OrderUnset), running("2", core.OrderUnset),
		running("3", core.OrderUnset), running("4", core.OrderUnset), running("5", core.OrderUnset),
	}
	p := Build("eggplant", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{GuestConcurrency: 2})
	got := guestGroups(p)
	if len(got) != 3 {
		t.Fatalf("bounded: got %d waves %v, want 3 (2+2+1)", len(got), got)
	}
	if len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("bounded: sizes %v, want 2,2,1", got)
	}
}

// An order is a statement about what must stop before what. Parallelism is not
// licence to ignore it, so tiers never merge however wide the concurrency is.
func TestOrderedGuestsNeverShareAWaveAcrossTiers(t *testing.T) {
	guests := []core.Guest{running("1", 1), running("2", 2), running("3", 3)}
	p := Build("eggplant", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{GuestConcurrency: AllAtOnce})
	got := guestGroups(p)
	if len(got) != 3 {
		t.Fatalf("ordered: got %v, want three separate waves", got)
	}
	// shutdownOrder stops the highest order first.
	if got[0][0] != "3" || got[2][0] != "1" {
		t.Fatalf("ordered: got %v, want 3 then 2 then 1", got)
	}
}

func TestGuestsSharingAnOrderShareAWave(t *testing.T) {
	guests := []core.Guest{running("1", 2), running("2", 2), running("3", 1)}
	p := Build("eggplant", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{GuestConcurrency: AllAtOnce})
	got := guestGroups(p)
	if len(got) != 2 {
		t.Fatalf("tiers: got %v, want two waves", got)
	}
	if len(got[0]) != 2 {
		t.Fatalf("tiers: first wave %v, want the two order=2 guests together", got[0])
	}
}

func TestUnorderedGuestsDoNotJoinAnOrderedTier(t *testing.T) {
	guests := []core.Guest{running("1", core.OrderUnset), running("2", 5)}
	p := Build("eggplant", []core.Role{core.RoleProxmox}, guests, core.StatusOnBattery, Options{GuestConcurrency: AllAtOnce})
	got := guestGroups(p)
	if len(got) != 2 {
		t.Fatalf("mixed: got %v, want the unordered guest in its own wave", got)
	}
}

// Host-wide steps are ordered against each other; sharing a wave would only
// invent a race between, say, releasing mounts and halting.
func TestNonGuestStepsNeverShareAWave(t *testing.T) {
	guests := []core.Guest{running("1", core.OrderUnset)}
	p := Build("eggplant",
		[]core.Role{core.RoleProxmox, core.RoleCephOSD, core.RoleKubelet},
		guests, core.StatusOnBattery,
		Options{UPSDeadline: 5 * time.Minute, GuestConcurrency: AllAtOnce})

	seen := map[int][]Action{}
	for _, s := range p.Steps {
		seen[s.Wave] = append(seen[s.Wave], s.Action)
	}
	for w, as := range seen {
		if len(as) == 1 {
			continue
		}
		for _, a := range as {
			if a != ActionGuestStop {
				t.Fatalf("wave %d shares %v; only guests may share a wave", w, as)
			}
		}
	}
}

func TestWavesAreAscendingAndContiguousPerStep(t *testing.T) {
	guests := []core.Guest{running("1", core.OrderUnset), running("2", core.OrderUnset)}
	p := Build("eggplant", []core.Role{core.RoleProxmox, core.RoleCephOSD}, guests, core.StatusOnBattery,
		Options{GuestConcurrency: AllAtOnce})
	got := waves(p)
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("waves %v are not ascending", got)
		}
	}
	if got[0] == 0 {
		t.Fatalf("waves %v: no step should be left in wave zero", got)
	}
}

// Concurrency must not move the commit boundary: guests stay reversible however
// they are grouped, and the halt stays the first step that is not.
func TestPointOfNoReturnIsUnaffectedByConcurrency(t *testing.T) {
	guests := []core.Guest{running("1", core.OrderUnset), running("2", core.OrderUnset), running("3", core.OrderUnset)}
	roles := []core.Role{core.RoleProxmox, core.RoleCephOSD}
	for _, c := range []int{AllAtOnce, 1, 2} {
		p := Build("eggplant", roles, guests, core.StatusOnBattery, Options{GuestConcurrency: c})
		if got, want := p.PointOfNoReturn(), len(p.Steps)-1; got != want {
			t.Fatalf("concurrency %d: point of no return %d, want %d (the halt)", c, got, want)
		}
	}
}

// The fallback must never be shorter than the platform's own default. Anything
// lower means installing CryoSheep quietly makes an undeclared guest less
// patient than it was before -- which is how a guest measured at 185s came to be
// killed at 90, while the hypervisor was blamed for a default it never had.
func TestDefaultGuestTimeoutIsNotShorterThanProxmoxOwnDefault(t *testing.T) {
	const proxmoxDefault = 180 * time.Second
	if core.DefaultGuestTimeout < proxmoxDefault {
		t.Fatalf("DefaultGuestTimeout = %v, must not be below Proxmox's own %v",
			core.DefaultGuestTimeout, proxmoxDefault)
	}
}

// estate mirrors one hypervisor's share of the real cluster: a router, DNS, two
// Kubernetes nodes, a couple of leaves and something unordered.
func estate() []core.Guest {
	return []core.Guest{
		{ID: "100", Name: "pfSense", Status: "running", Order: 1},
		{ID: "707", Name: "dns", Status: "running", Order: 2},
		{ID: "103", Name: "nas", Status: "running", Order: 3},
		{ID: "105", Name: "map", Status: "running", Order: 4},
		{ID: "106", Name: "chest", Status: "running", Order: 4},
		{ID: "869", Name: "pbx", Status: "running", Order: 5},
		{ID: "107", Name: "moor", Status: "running", Order: 5},
		{ID: "204", Name: "nginx", Status: "running", Order: core.OrderUnset},
	}
}

func stopWaves(t *testing.T, o Options) [][]string {
	t.Helper()
	return guestGroups(Build("h", []core.Role{core.RoleProxmox}, estate(), core.StatusOnBattery, o))
}

func TestOrderGroupsCollapseRangesIntoOneWave(t *testing.T) {
	got := stopWaves(t, Options{
		GuestConcurrency: AllAtOnce,
		OrderGroups:      []OrderGroup{{5, 99}, {4, 4}, {1, 3}},
	})
	want := [][]string{
		{"204", "869", "107"}, // leaves and unordered, together
		{"105", "106"},        // kubernetes alone
		{"103", "707", "100"}, // infrastructure, together
	}
	if len(got) != len(want) {
		t.Fatalf("waves = %v, want %d groups", got, len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("wave %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestOrderGroupsKeepRangesInDeclaredOrder(t *testing.T) {
	got := stopWaves(t, Options{
		GuestConcurrency: AllAtOnce,
		OrderGroups:      []OrderGroup{{5, 99}, {4, 4}, {1, 3}},
	})
	// The router must still be in the last wave: everything routed through it
	// has to be gone before it goes.
	last := got[len(got)-1]
	var found bool
	for _, id := range last {
		found = found || id == "100"
	}
	if !found {
		t.Fatalf("pfSense not in final wave; waves = %v", got)
	}
}

func TestWithoutOrderGroupsEveryTierIsItsOwnWave(t *testing.T) {
	got := stopWaves(t, Options{GuestConcurrency: AllAtOnce})
	// unordered, 5, 4, 3, 2, 1 — the planned-shutdown shape, unchanged.
	if len(got) != 6 {
		t.Fatalf("waves = %v, want 6 tiers", got)
	}
}

func TestIncompleteOrderGroupsFallBackToTiers(t *testing.T) {
	// A range that leaves order 3 uncovered is a misconfiguration. Degrading to
	// the careful per-tier shape is the safe direction; silently dropping a
	// guest from the sequence is not.
	got := stopWaves(t, Options{
		GuestConcurrency: AllAtOnce,
		OrderGroups:      []OrderGroup{{5, 99}, {4, 4}, {1, 2}},
	})
	if len(got) != 6 {
		t.Fatalf("waves = %v, want fallback to 6 tiers", got)
	}
}

func TestOrderGroupsStillRespectConcurrency(t *testing.T) {
	// Grouping says which guests may stop together; concurrency says how many
	// actually do. A group of three at concurrency 2 is two waves, not one.
	got := stopWaves(t, Options{
		GuestConcurrency: 2,
		OrderGroups:      []OrderGroup{{5, 99}, {4, 4}, {1, 3}},
	})
	if len(got[0]) != 2 {
		t.Fatalf("first wave = %v, want 2 guests at concurrency 2", got[0])
	}
}

func TestOrderGroupsDoNotDisturbTheHalt(t *testing.T) {
	p := Build("h", []core.Role{core.RoleProxmox}, estate(), core.StatusOnBattery, Options{
		GuestConcurrency: AllAtOnce,
		OrderGroups:      []OrderGroup{{5, 99}, {4, 4}, {1, 3}},
	})
	a := actions(p)
	if a[len(a)-1] != ActionHostHalt {
		t.Fatalf("last action = %v, want host poweroff", a[len(a)-1])
	}
}

func TestRoutersCanBeIsolatedInTheirOwnFinalWave(t *testing.T) {
	// [2,3] and [1,1] rather than one [1,3]: the routers must not be stopping
	// while DNS and storage still are. Everything else has an L2 path to its
	// own disk, but anything still draining over a routed path does not.
	got := stopWaves(t, Options{
		GuestConcurrency: AllAtOnce,
		OrderGroups:      []OrderGroup{{5, 99}, {4, 4}, {2, 3}, {1, 1}},
	})
	if len(got) != 4 {
		t.Fatalf("waves = %v, want 4 groups", got)
	}
	last := got[len(got)-1]
	if len(last) != 1 || last[0] != "100" {
		t.Fatalf("final wave = %v, want the router alone", last)
	}
	// and nothing else may share it
	for _, id := range got[len(got)-2] {
		if id == "100" {
			t.Fatalf("router appears in the penultimate wave: %v", got)
		}
	}
}
