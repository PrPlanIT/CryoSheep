// Package exec implements the outside-world interfaces by running local
// commands.
//
// Shelling out is deliberate for these two, not a shortcut. `qm` talks to the
// local cluster filesystem and keeps working while pveproxy is stopping, which
// the PVE REST API does not — and this runs during shutdown. The systemd query
// is here for now so the first milestone carries no dependencies; it is the
// obvious candidate to move onto the D-Bus API, and the interface in core is
// what makes that a one-file change.
package exec

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

type Runner struct {
	// Run is swappable so the parsers can be tested without a hypervisor.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func New() *Runner {
	return &Runner{Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}}
}

// IsActive reports whether a unit is active. systemctl exits non-zero for an
// inactive unit, which is an answer rather than a failure, so the exit status is
// not treated as an error — only the word it prints is read.
func (r *Runner) IsActive(ctx context.Context, unit string) (bool, error) {
	out, _ := r.Run(ctx, "systemctl", "is-active", unit)
	return strings.TrimSpace(string(out)) == "active", nil
}

// Guests lists this host's VMs, with each running guest's Proxmox startup order.
//
// The order needs a second call per guest: `qm list` does not carry it. Only
// running guests are asked, since a stopped one produces no step.
func (r *Runner) Guests(ctx context.Context) ([]core.Guest, error) {
	out, err := r.Run(ctx, "qm", "list")
	if err != nil {
		return nil, fmt.Errorf("qm list: %w", err)
	}
	guests, err := ParseQMList(string(out))
	if err != nil {
		return nil, err
	}
	for i := range guests {
		if !guests[i].Running() {
			continue
		}
		cfg, err := r.Run(ctx, "qm", "config", guests[i].ID)
		if err != nil {
			continue // unreadable config means unordered, not fatal
		}
		guests[i].Order = ParseStartupOrder(string(cfg))
		guests[i].Down = ParseStartupDown(string(cfg))
	}
	return guests, nil
}

// ParseStartupDown reads `down=N` from a guest's startup line — the seconds
// Proxmox allows that guest to stop before forcing it. Zero when unset.
func ParseStartupDown(cfg string) time.Duration {
	if n := startupField(cfg, "down="); n >= 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

// ParseStartupOrder reads `startup: order=N,up=X,down=Y` from `qm config`.
// Returns core.OrderUnset when the guest has no declared order.
func ParseStartupOrder(cfg string) int {
	if n := startupField(cfg, "order="); n >= 0 {
		return n
	}
	return core.OrderUnset
}

// startupField reads one key from the `startup:` line. Returns -1 when the line
// or the key is absent. Only `startup:` is considered — `boot: order=scsi0` is a
// different field entirely, and reading it would invent a priority nobody set.
func startupField(cfg, key string) int {
	sc := bufio.NewScanner(strings.NewReader(cfg))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "startup:") {
			continue
		}
		for _, part := range strings.Split(strings.TrimPrefix(line, "startup:"), ",") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, key) {
				continue
			}
			n, err := strconv.Atoi(strings.TrimPrefix(part, key))
			if err != nil {
				return -1
			}
			return n
		}
	}
	return -1
}

// ParseQMList reads the fixed-column output of `qm list`. Exported so the parser
// can be tested against captured output from a real host.
//
//	VMID NAME       STATUS     MEM(MB)  BOOTDISK(GB) PID
//	 102 chest-002  running    16384    64.00        1234
func ParseQMList(out string) ([]core.Guest, error) {
	var guests []core.Guest
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		if strings.EqualFold(fields[0], "VMID") {
			continue // header
		}
		if !isNumeric(fields[0]) {
			continue
		}
		guests = append(guests, core.Guest{
			ID: fields[0], Name: fields[1], Status: fields[2], Order: core.OrderUnset,
		})
	}
	return guests, sc.Err()
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Shutdown asks a guest to stop over ACPI and waits for it.
//
// `qm shutdown` rather than `qm stop` is the whole point: ACPI starts a normal
// shutdown inside the guest, which is what lets kubelet's shutdownGracePeriod
// drain pods. `qm stop` pulls the virtual plug and the guest never finds out.
func (r *Runner) Shutdown(ctx context.Context, id string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs <= 0 {
		secs = 90
	}
	_, err := r.Run(ctx, "qm", "shutdown", id,
		"--timeout", fmt.Sprint(secs),
		"--forceStop", "1")
	if err != nil {
		return fmt.Errorf("qm shutdown %s: %w", id, err)
	}
	return nil
}

// Cordon stops the scheduler placing new work on a node that is leaving.
func (r *Runner) Cordon(ctx context.Context, node string) error {
	if _, err := r.Run(ctx, "kubectl", "cordon", node); err != nil {
		return fmt.Errorf("kubectl cordon %s: %w", node, err)
	}
	return nil
}

func (r *Runner) Uncordon(ctx context.Context, node string) error {
	if _, err := r.Run(ctx, "kubectl", "uncordon", node); err != nil {
		return fmt.Errorf("kubectl uncordon %s: %w", node, err)
	}
	return nil
}

// StatefulPods lists the stateful workloads on this node and the role each
// claims, so a revival has something to start from.
func (r *Runner) StatefulPods(ctx context.Context, node string) ([]core.StatefulPod, error) {
	out, err := r.Run(ctx, "kubectl", "get", "pods", "--all-namespaces",
		"--field-selector", "spec.nodeName="+node, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("kubectl get pods: %w", err)
	}
	return ParseStatefulPods(out)
}

// roleTokens are the words workloads use when they publish which instance is
// authoritative.
//
// Matched against label keys and values rather than against a list of known
// operators. A curated vendor list only finds the orchestrators someone thought
// of, and the point of the record is to be useful for a cluster running
// something nobody here has heard of. CNPG writes cnpg.io/instanceRole=primary,
// another operator writes role=master, a third writes its own — all of them say
// one of these words somewhere.
var roleTokens = []string{"primary", "master", "leader", "replica", "standby", "secondary", "role"}

// roleOf finds the label that says what this instance is, without knowing who
// wrote it. Returns "key=value" so the record keeps the operator's own
// vocabulary — CryoSheep gathers evidence, it does not interpret it, and a
// normalised guess is exactly the kind of confident-but-wrong that hurts during
// a recovery.
func roleOf(labels map[string]string) string {
	// Deterministic: a pod with several matching labels must record the same one
	// every time, or two runs of the same cluster disagree.
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := labels[k]
		if v == "" {
			continue
		}
		lk, lv := strings.ToLower(k), strings.ToLower(v)
		for _, tok := range roleTokens {
			if strings.Contains(lk, tok) || strings.Contains(lv, tok) {
				return k + "=" + v
			}
		}
	}
	return ""
}

// ParseStatefulPods picks the workloads worth recording out of `kubectl get
// pods -o json`: anything owned by a StatefulSet, and anything that publishes a
// role. Stateless pods are omitted — they reschedule and their identity does not
// survive or need to.
func ParseStatefulPods(data []byte) ([]core.StatefulPod, error) {
	var list struct {
		Items []struct {
			Spec struct {
				// A priority class is the platform's own statement of what
				// matters, set by whoever deployed the workload rather than
				// inferred here.
				PriorityClassName string `json:"priorityClassName"`
			} `json:"spec"`
			Metadata struct {
				Name            string            `json:"name"`
				Namespace       string            `json:"namespace"`
				Labels          map[string]string `json:"labels"`
				OwnerReferences []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse pods: %w", err)
	}

	var out []core.StatefulPod
	for _, it := range list.Items {
		m := it.Metadata
		owner := ""
		stateful := false
		for _, o := range m.OwnerReferences {
			if o.Kind == "StatefulSet" || o.Kind == "Cluster" {
				owner, stateful = o.Kind+"/"+o.Name, true
				break
			}
		}
		role := roleOf(m.Labels)
		if !stateful && role == "" {
			continue
		}
		out = append(out, core.StatefulPod{
			Namespace: m.Namespace, Name: m.Name, Owner: owner, Role: role,
			Priority: it.Spec.PriorityClassName,
		})
	}
	return out, nil
}

// UnmountCSI releases Ceph and CSI mounts before the network goes.
//
// This is the stall. systemd waits TimeoutStopSec on every mount unit whose
// backing store has already become unreachable, which is how a node that should
// stop in seconds takes minutes. Lazy unmount detaches the tree immediately and
// lets the kernel clean up, rather than blocking on a server that is gone.
func (r *Runner) UnmountCSI(ctx context.Context) (int, error) {
	out, err := r.Run(ctx, "findmnt", "-rn", "-o", "TARGET,FSTYPE")
	if err != nil {
		return 0, fmt.Errorf("findmnt: %w", err)
	}
	n := 0
	for _, target := range CSIMounts(string(out)) {
		if _, err := r.Run(ctx, "umount", "-l", target); err == nil {
			n++
		}
	}
	return n, nil
}

// CSIMounts picks the mounts worth releasing from findmnt output: Ceph and the
// fuse mounts CSI drivers create. Exported so the selection can be tested
// against real output — unmounting the wrong thing on a live node is worse than
// unmounting nothing.
func CSIMounts(out string) []string {
	var targets []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		target, fstype := f[0], f[1]
		switch {
		case fstype == "ceph", fstype == "rbd", fstype == "fuse.ceph-fuse":
		case strings.HasPrefix(fstype, "fuse.") && strings.Contains(target, "/kubelet/"):
		case strings.Contains(target, "/kubelet/pods/"), strings.Contains(target, "/kubelet/plugins/"):
		default:
			continue
		}
		targets = append(targets, target)
	}
	return targets
}

// Start boots a guest again, undoing a shutdown this run performed.
func (r *Runner) Start(ctx context.Context, id string) error {
	if _, err := r.Run(ctx, "qm", "start", id); err != nil {
		return fmt.Errorf("qm start %s: %w", id, err)
	}
	return nil
}

// SetNoout stops the cluster reacting to a departure that is deliberate.
//
// Shelled to the ceph CLI rather than go-ceph: those bindings are cgo and need
// librados at build and run time, which would forfeit the static binary this
// whole approach depends on.
func (r *Runner) SetNoout(ctx context.Context) error {
	if _, err := r.Run(ctx, "ceph", "osd", "set", "noout"); err != nil {
		return fmt.Errorf("ceph osd set noout: %w", err)
	}
	return nil
}

func (r *Runner) UnsetNoout(ctx context.Context) error {
	if _, err := r.Run(ctx, "ceph", "osd", "unset", "noout"); err != nil {
		return fmt.Errorf("ceph osd unset noout: %w", err)
	}
	return nil
}

// Poweroff stops this machine. The last thing the process does.
func (r *Runner) Poweroff(ctx context.Context) error {
	if _, err := r.Run(ctx, "systemctl", "poweroff"); err != nil {
		return fmt.Errorf("systemctl poweroff: %w", err)
	}
	return nil
}
