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
}

type Options struct {
	// GuestTimeout bounds each guest's ACPI shutdown before it is forced.
	GuestTimeout time.Duration

	// UPSDeadline is how long the UPS should wait before cutting power
	// regardless of what the sequence managed to do. Zero omits the step, for a
	// host with no authority over the UPS.
	UPSDeadline time.Duration
}

func (o Options) withDefaults() Options {
	if o.GuestTimeout <= 0 {
		o.GuestTimeout = 90 * time.Second
	}
	return o
}

type Plan struct {
	Host      string
	Roles     []core.Role
	Guests    []core.Guest
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
func shutdownOrder(guests []core.Guest) []core.Guest {
	var running []core.Guest
	for _, g := range guests {
		if g.Running() {
			running = append(running, g)
		}
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

	p := Plan{Host: host, Roles: roles, Guests: guests, UPSStatus: upsStatus}
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
				Detail: "note which workloads held authority here", Reversible: true},
			Step{Action: ActionK8sCordon, Target: host,
				Detail: "stop scheduling onto a node that is leaving", Reversible: true},
			Step{Action: ActionK8sUnmount,
				Detail: "release Ceph/CSI mounts before the network goes"},
		)
	}

	for _, g := range shutdownOrder(guests) {
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
			// Undoable: the guest can be started again. Gating every guest is
			// what stops a recovered outage from becoming a real one — power
			// back while the third of six is stopping must abandon the
			// sequence, not complete it.
			Reversible: true,
		})
	}

	p.Steps = append(p.Steps, Step{Action: ActionHostHalt, Detail: host})
	return p
}
