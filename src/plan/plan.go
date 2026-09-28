// Package plan turns what a host is into the ordered sequence of steps that
// stops it. It is pure: given roles, guests and UPS state it returns a Plan and
// touches nothing. Execution happens elsewhere, against the same Plan.
//
// The point of no return is a property of the plan, not of the executor. Steps
// before it can be abandoned and reversed; steps at or after it commit the host.
// Making that explicit here is what lets `cryosheep plan` tell an operator
// exactly where the decision becomes irreversible, before any power is lost.
package plan

import (
	"sort"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

type Action string

const (
	// ActionUPSDeadline arms the UPS's own power-off timer. First, and
	// reversible: it is armed while the UPS can still be reached, and cancelled
	// if mains return before the host is committed.
	ActionUPSDeadline Action = "ups.deadline.arm"
	ActionCephNoout   Action = "ceph.noout.set"

	// Node-local kubernetes teardown. Cordon and drain are reversible — an
	// abandoned shutdown uncordons and the workloads return. Releasing mounts is
	// not, but by then the node is going down regardless.
	ActionK8sRecord  Action = "k8s.quorum.record"
	ActionK8sCordon  Action = "k8s.cordon"
	ActionK8sUnmount Action = "k8s.csi.unmount"
	ActionGuestStop  Action = "guest.shutdown"
	ActionHostHalt   Action = "host.poweroff"
)

// Step is one unit of the sequence.
type Step struct {
	Action  Action
	Target  string        // guest id, or empty where the host is the target
	Detail  string        // human context, e.g. the guest name
	Timeout time.Duration // 0 where the step is not time-bounded

	// Reversible steps are gated: before running one, the executor re-reads UPS
	// status and abandons the sequence if mains have returned. Once a step that
	// is not reversible has run, the gates stop — the host is committed and
	// re-checking would only add a way to hang.
	Reversible bool

	// Wave groups steps that may run at once. A wave is issued together and
	// awaited as a set; waves run in ascending order. Every step has a wave, and
	// a wave of one is the serial case, so a plan that shares no waves behaves
	// exactly as it did before waves existed.
	//
	// Waves exist because the right concurrency differs by flow rather than by
	// action. Stopping an estate wants every guest at once, because nothing is
	// staying up and the budget is a battery. Rolling maintenance wants one at a
	// time, because the point is that the cluster keeps serving. Same steps,
	// opposite policy — so the policy belongs in the plan, not in the executor
	// and not in a second code path.
	Wave int
}

type Options struct {
	// GuestTimeout bounds each guest's ACPI shutdown before it is forced.
	GuestTimeout time.Duration

	// UPSDeadline is how long the UPS should wait before cutting power
	// regardless of what the sequence managed to do. Zero omits the step, for a
	// host with no authority over the UPS.
	UPSDeadline time.Duration

	// OrderGroups collapses spans of startup order into single waves, in the
	// sequence declared. Empty means every tier is its own wave, which is the
	// planned-shutdown shape and the safe default.
	//
	// Groups must cover every order from 1 to an unordered guest, because a
	// guest in no group would be a guest left running. Incomplete groups are
	// discarded rather than honoured: degrading to the careful per-tier shape
	// is the right direction to fail in.
	OrderGroups []OrderGroup

	// GuestConcurrency is how many guests may be stopping at once.
	//
	// 1 stops them one at a time. n stops n. AllAtOnce stops every guest in a
	// tier together, which is what a power failure wants: serial does not fit a
	// battery, and a serial queue also means one hung guest starves the ones
	// behind it.
	//
	// Unset means serial, deliberately. Concurrency is opted into, because it
	// coarsens when a sequence can be abandoned: guests issued together cannot be
	// abandoned between, so mains returning mid-wave stops all of them and then
	// restarts all of them. Serial keeps that granularity and is what every
	// caller got before this existed.
	//
	// Guests that declare an order are never merged across tiers whatever this
	// says. An order is a statement about what must stop before what, and
	// parallelism is not licence to ignore it.
	GuestConcurrency int

	// HoldUntil delays the first committing step until this long after the
	// sequence began, so several hosts acting on one signal halt together rather
	// than as each finishes.
	//
	// It exists for shared storage. With OSDs on the machines being stopped, a
	// host that halts the moment it is ready takes its OSDs while another host's
	// guests are still writing, and below min_size the stragglers freeze
	// mid-sync. Waiting costs battery; not waiting costs the slowest guests,
	// which are the databases.
	HoldUntil time.Duration

	// OmitHalt leaves the final poweroff out, because something else is already
	// stopping this machine.
	//
	// This is what separates a shutdown from a reboot. Run as a systemd shutdown
	// handler, the transition is already in flight — halting here either races
	// it or, on a reboot, turns it into a poweroff and the machine never comes
	// back. The teardown is still the whole point; ending it is not ours to do.
	OmitHalt bool
}

// AllAtOnce stops every guest in an order tier together. Negative rather than
// zero so that the zero value stays serial and nobody inherits concurrency by
// forgetting to ask for it.
const AllAtOnce = -1

// OrderGroup is an inclusive span of Proxmox startup orders whose guests stop
// together, as one wave.
//
// Tiers exist for startup dependency and for planned maintenance, where stopping
// one thing at a time is the point. A power failure wants the opposite: the
// estate's dependencies are far coarser than its tiers, and waiting out five of
// them in sequence spends battery on an order nothing actually needs.
//
// Here that is not a guess. Guest disks reach Ceph over L2 on the storage VLAN,
// but Kubernetes PVC traffic is routed through the firewall — so Kubernetes is
// the only tier that needs other tiers still standing while it drains. Leaves
// above it depend on nothing; the infrastructure below it is mutually
// independent. Three groups, not five tiers.
type OrderGroup struct {
	Lo, Hi int
}

// unorderedOrder is the order an unordered guest counts as when grouping.
//
// Proxmox starts unordered guests last and stops them first, which is precisely
// what the highest order means, so a group reaching this covers them without a
// special case.
const unorderedOrder = 99

func (o Options) withDefaults() Options {
	if o.GuestTimeout <= 0 {
		o.GuestTimeout = core.DefaultGuestTimeout
	}
	if o.GuestConcurrency == 0 {
		o.GuestConcurrency = 1
	}
	return o
}

type Plan struct {
	Host   string
	Roles  []core.Role
	Guests []core.Guest

	// HoldUntil is how long after the sequence began the first committing step
	// may run. It lives on the plan rather than on the executor because it is
	// part of what was decided, not part of how it is carried out — and because
	// a plan that can be read before it runs should say that it will wait.
	HoldUntil time.Duration
	UPSStatus string
	Steps     []Step
}

// shutdownOrder returns the running guests in the order they should be stopped.
//
// Proxmox starts guests in ascending `startup: order=` and stops them in the
// reverse, so the first thing up is the last thing down. Guests with no order
// start last and therefore stop first, which is also what you want: an
// unordered guest is one nobody declared important.
//
// This is why the field is read rather than duplicated. A firewall or DNS guest
// given order=1 boots first and survives longest, and that single value governs
// both directions — no second list to drift out of step with it.
func shutdownOrder(guests []core.Guest, groups []OrderGroup) []core.Guest {
	var running []core.Guest
	for _, g := range guests {
		if g.Running() {
			running = append(running, g)
		}
	}
	if g := usableGroups(groups); g != nil {
		// Groups stop in the sequence they are declared in. Within one they are
		// a single wave, so relative position inside it does not matter.
		sort.SliceStable(running, func(i, j int) bool {
			return groupIndexOf(running[i], g) < groupIndexOf(running[j], g)
		})
		return running
	}
	sort.SliceStable(running, func(i, j int) bool {
		a, b := running[i], running[j]
		au := a.Order == core.OrderUnset || a.Order == 0
		bu := b.Order == core.OrderUnset || b.Order == 0
		if au != bu {
			return au // unordered guests stop first
		}
		if au {
			return false // both unordered: keep qm list order
		}
		return a.Order > b.Order // ordered: highest stops first, order=1 last
	})
	return running
}

// guestWaves splits guests, already in shutdown order, into the groups that may
// run concurrently.
//
// A tier is a run of guests sharing an Order. Tiers never merge: an order says
// this stops before that, and the whole point of honouring it is that it is not
// negotiable for speed. Within a tier, concurrency decides how finely it is cut
// — 1 for one at a time, 0 for the whole tier at once, n for a bounded roll.
func guestWaves(ordered []core.Guest, concurrency int, groups []OrderGroup) [][]core.Guest {
	groups = usableGroups(groups)
	together := func(a, b core.Guest) bool {
		if groups == nil {
			return sameTier(a, b)
		}
		return groupIndexOf(a, groups) == groupIndexOf(b, groups)
	}
	var out [][]core.Guest
	for i := 0; i < len(ordered); {
		j := i
		for j < len(ordered) && together(ordered[i], ordered[j]) {
			j++
		}
		tier := ordered[i:j]
		switch {
		case concurrency < 0:
			out = append(out, tier)
		default:
			for k := 0; k < len(tier); k += concurrency {
				end := k + concurrency
				if end > len(tier) {
					end = len(tier)
				}
				out = append(out, tier[k:end])
			}
		}
		i = j
	}
	return out
}

// sameTier reports whether two guests may stop together. Unordered guests are
// one tier; ordered guests share a tier only with the same Order.
// orderOf is a guest's order for grouping, with unordered folded to the top.
func orderOf(g core.Guest) int {
	if g.Order == core.OrderUnset || g.Order == 0 {
		return unorderedOrder
	}
	return g.Order
}

// groupIndexOf returns which declared group a guest belongs to, or -1.
func groupIndexOf(g core.Guest, groups []OrderGroup) int {
	o := orderOf(g)
	for i, r := range groups {
		if o >= r.Lo && o <= r.Hi {
			return i
		}
	}
	return -1
}

// usableGroups returns the groups only if they cover every order a guest could
// hold, and nil otherwise. A partial cover would silently leave guests out of
// the sequence, so it is treated as no configuration at all.
func usableGroups(groups []OrderGroup) []OrderGroup {
	if len(groups) == 0 {
		return nil
	}
	for o := 1; o <= unorderedOrder; o++ {
		var covered bool
		for _, r := range groups {
			covered = covered || (o >= r.Lo && o <= r.Hi)
		}
		if !covered {
			return nil
		}
	}
	return groups
}

func sameTier(a, b core.Guest) bool {
	au := a.Order == core.OrderUnset || a.Order == 0
	bu := b.Order == core.OrderUnset || b.Order == 0
	if au || bu {
		return au && bu
	}
	return a.Order == b.Order
}

// PointOfNoReturn is the index of the first step that cannot be reversed, or
// len(Steps) when every step can. Steps before it are gated on UPS state.
func (p Plan) PointOfNoReturn() int {
	for i, s := range p.Steps {
		if !s.Reversible {
			return i
		}
	}
	return len(p.Steps)
}

// Build composes the sequence for one host.
//
// Guests are stopped before the host halts, and before any OSD on it goes down,
// so storage is quiet before the machines using it disappear. Only running
// guests produce a step: a stopped guest is already where the sequence wants it.
func Build(host string, roles []core.Role, guests []core.Guest, upsStatus string, opts Options) Plan {
	opts = opts.withDefaults()

	p := Plan{Host: host, Roles: roles, Guests: guests, UPSStatus: upsStatus, HoldUntil: opts.HoldUntil}

	// Waves are handed out as steps are appended. Everything that is not a guest
	// takes a wave of its own: these are host-wide and ordered against each
	// other, and running two of them at once would only invent a race.
	wave := 0
	solo := func() int { wave++; return wave }

	has := func(r core.Role) bool {
		for _, x := range roles {
			if x == r {
				return true
			}
		}
		return false
	}

	// Arm the hardware backstop first, while the UPS is certainly still
	// reachable. Everything after this can fail without the estate being left
	// powered on until the battery is flat.
	if opts.UPSDeadline > 0 {
		p.Steps = append(p.Steps, Step{
			Action:     ActionUPSDeadline,
			Detail:     "UPS cuts power regardless of what follows",
			Timeout:    opts.UPSDeadline,
			Reversible: true,
			Wave:       solo(),
		})
	}

	// Stop the cluster reacting to a departure that is deliberate. Reversible,
	// and first, so the abundant part of the budget is spent where it can be
	// given back.
	if has(core.RoleCephOSD) {
		p.Steps = append(p.Steps, Step{
			Action:     ActionCephNoout,
			Detail:     "stop rebalance for a planned outage",
			Reversible: true,
			Wave:       solo(),
		})
	}

	// Node-local kubernetes teardown.
	//
	// Deliberately no drain. Draining evicts pods so they can go somewhere else,
	// which is right for rolling maintenance and wrong here: in a full shutdown
	// there is nowhere else, so evicting only forces failovers nobody wanted —
	// moving a database primary off a node that is stopping, onto a node that is
	// also stopping. It also fights PodDisruptionBudgets that exist for good
	// reasons, and forcing past one is how a primary gets killed.
	//
	// Instead the node is cordoned so nothing new lands on it, and kubelet's own
	// graceful shutdown terminates the pods in place — real SIGTERM, honouring
	// terminationGracePeriodSeconds, in pod-priority order. Nothing is asked to
	// move; everything is asked to stop.
	//
	// The record comes first, while the cluster is still whole, because after
	// this node stops nothing local knows which instance held primary.
	if has(core.RoleKubelet) {
		p.Steps = append(p.Steps,
			Step{Action: ActionK8sRecord, Target: host,
				Detail: "note which workloads held authority here", Reversible: true, Wave: solo()},
			Step{Action: ActionK8sCordon, Target: host,
				Detail: "stop scheduling onto a node that is leaving", Reversible: true, Wave: solo()},
			Step{Action: ActionK8sUnmount,
				Detail: "release Ceph/CSI mounts before the network goes", Wave: solo()},
		)
	}

	for _, group := range guestWaves(shutdownOrder(guests, opts.OrderGroups), opts.GuestConcurrency, opts.OrderGroups) {
		w := solo()
		for _, g := range group {
			// A guest that declares its own budget gets it. Proxmox already knows a
			// firewall needs longer than a scratch VM; a flat timeout would make the
			// whole sequence as slow as its most patient member.
			budget := opts.GuestTimeout
			if g.Down > 0 {
				budget = g.Down
			}
			p.Steps = append(p.Steps, Step{
				Action:  ActionGuestStop,
				Target:  g.ID,
				Detail:  g.Name,
				Timeout: budget,
				Wave:    w,
				// Undoable: the guest can be started again. Gating every guest is
				// what stops a recovered outage from becoming a real one — power
				// back while the third of six is stopping must abandon the
				// sequence, not complete it.
				Reversible: true,
			})
		}
	}

	if !opts.OmitHalt {
		p.Steps = append(p.Steps, Step{Action: ActionHostHalt, Detail: host, Wave: solo()})
	}
	return p
}
