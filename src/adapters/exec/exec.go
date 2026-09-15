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

// Guests lists this host's VMs by parsing `qm list`.
func (r *Runner) Guests(ctx context.Context) ([]core.Guest, error) {
	out, err := r.Run(ctx, "qm", "list")
	if err != nil {
		return nil, fmt.Errorf("qm list: %w", err)
	}
	return ParseQMList(string(out))
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
		guests = append(guests, core.Guest{ID: fields[0], Name: fields[1], Status: fields[2]})
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
