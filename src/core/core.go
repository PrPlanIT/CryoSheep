// Package core holds the types every other package speaks, and the interfaces
// through which CryoSheep reaches the outside world.
//
// Everything that touches a hypervisor, a UPS or an init system goes through an
// interface here. The sequencing logic never shells out itself, which is what
// makes it testable without a hypervisor, a UPS, or root.
package core

import (
	"context"
	"time"
)

// Role is something a host is, established by what is running on it rather than
// by what an inventory claims. A host can hold several.
type Role string

const (
	RoleProxmox Role = "proxmox"
	RoleCephOSD Role = "ceph-osd"
	RoleKubelet Role = "kubelet"
)

// Unit names the roles are detected from. A host that is not running the unit
// does not have the role, whatever an inventory says.
var RoleUnits = map[Role]string{
	RoleProxmox: "pve-cluster.service",
	RoleCephOSD: "ceph-osd.target",
	RoleKubelet: "kubelet.service",
}

// OrderUnset marks a guest with no Proxmox startup order.
const OrderUnset = -1

// Guest is a virtual machine on this host.
type Guest struct {
	ID     string
	Name   string
	Status string // "running", "stopped", ...

	// Down is Proxmox's own per-guest shutdown budget, from `startup: down=N`.
	// Zero means unset, and the caller's default applies. Read for the same
	// reason as Order: a firewall that needs three minutes and a scratch VM that
	// needs twenty seconds should not share one flat timeout, and Proxmox is
	// already where that is written down.
	Down time.Duration

	// Order is Proxmox's own startup order, from `startup: order=N`.
	// CryoSheep reads it rather than keeping a second list: Proxmox already
	// honours this field when it starts and stops guests itself, and two
	// sources of truth for the same ordering would silently disagree.
	Order int
}

func (g Guest) Running() bool { return g.Status == "running" }

// UPS status strings, as NUT reports them in ups.status. The value is a
// space-separated set of flags, so callers test for membership rather than
// equality.
const (
	StatusOnline    = "OL"
	StatusOnBattery = "OB"
	StatusLowBatt   = "LB"
)

// Units reports on the init system. Implemented over systemd.
type Units interface {
	IsActive(ctx context.Context, unit string) (bool, error)
}

// Hypervisor enumerates and stops the guests of the host it runs on. It is
// deliberately local-only: CryoSheep never reaches across a host boundary,
// because a sequencer that shuts down its own host mid-loop cannot finish.
type Hypervisor interface {
	Guests(ctx context.Context) ([]Guest, error)
	Shutdown(ctx context.Context, id string, timeout time.Duration) error

	// Start puts a guest back. Stopping one is undoable — the guest boots again
	// — which is what lets mains returning mid-sequence abandon the shutdown
	// instead of completing an outage nobody needed.
	Start(ctx context.Context, id string) error
}

// Reading is one observation of the UPS.
//
// More than the status flag, because the flag alone is a poor test for "mains
// are back": a transfer can flap it, and a UPS need not clear OB promptly. What
// actually proves recovery is the battery no longer draining while input power
// is present.
type Reading struct {
	Status  string
	Charge  float64       // percent, or Unknown
	Runtime time.Duration // remaining, or Unknown
	InputV  float64       // input voltage, or Unknown
}

// Unknown marks a value the driver did not report. The snmp-ups subdriver in use
// reports charge, runtime and input voltage but not the low thresholds, so
// callers must cope with absence rather than assume zero.
const Unknown = -1

// UPS reads UPS state. CryoSheep does not monitor it — upsmon owns the event
// stream — but it re-reads before each reversible step, so that power returning
// mid-sequence abandons the sequence rather than being noticed too late.
type UPS interface {
	Read(ctx context.Context) (Reading, error)
}

// Ceph stops and restarts the cluster's reaction to a host leaving. Only the
// flags this host is responsible for — CryoSheep never manages the cluster.
type Ceph interface {
	SetNoout(ctx context.Context) error
	UnsetNoout(ctx context.Context) error
}

// Kube is this node's own membership of a cluster. Every call is about the node
// it runs on: CryoSheep never manages other nodes.
//
// A stock Debian/Ubuntu poweroff handles none of this well. Pods are killed
// rather than evicted, and CSI mounts that cannot be unmounted stall systemd for
// TimeoutStopSec each — which is why a node that should stop in seconds takes
// minutes.
// StatefulPod is a workload whose identity matters when it comes back.
//
// Role is what the workload calls itself — CNPG writes primary/replica, Galera
// and others use their own vocabulary. It is recorded verbatim rather than
// normalised: the value is evidence for a human or a later revival, not
// something CryoSheep interprets.
type StatefulPod struct {
	Namespace string
	Name      string
	Owner     string // controlling StatefulSet or Cluster, where there is one
	Role      string // e.g. "primary", "replica", or empty
}

type Kube interface {
	Cordon(ctx context.Context, node string) error
	Uncordon(ctx context.Context, node string) error

	// StatefulPods lists the stateful workloads on this node and the role each
	// claims, read before anything is stopped.
	//
	// This is the record a revival needs. A quorum service elects from what it
	// finds on disk, and the node that stopped last cleanly is usually the one
	// to start from — Galera writes safe_to_bootstrap for exactly this, but only
	// on a clean stop. If a node is forced off, nothing local says who was
	// authoritative. A journal that says which instance held primary, on which
	// node, at what time, is what makes the answer knowable afterwards.
	StatefulPods(ctx context.Context, node string) ([]StatefulPod, error)

	// UnmountCSI releases Ceph and CSI mounts before the network goes, returning
	// how many it released. Left mounted, each one is a unit systemd waits on
	// while its backing network is already gone.
	UnmountCSI(ctx context.Context) (int, error)
}

// Host is the machine itself. Separate from Hypervisor because a host with no
// guests still has to stop, and because it is the one call that ends the process
// making it.
type Host interface {
	Poweroff(ctx context.Context) error
}

// Deadline is the UPS's own shutdown timer — the backstop that fires whether or
// not the software sequence completed.
//
// This is the answer to a sequencer that cannot finish: a host that wedges, a
// step that hangs, a run that never started. Armed while the UPS is still
// reachable, cancelled if mains return. Nothing in software can guarantee an
// ending; hardware honouring a timer it already holds can.
type Deadline interface {
	Arm(ctx context.Context, in time.Duration) error
	Cancel(ctx context.Context) error
}
