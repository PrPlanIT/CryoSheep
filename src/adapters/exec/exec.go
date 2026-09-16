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
	"fmt"
	"os/exec"
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
