package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/adapters/exec"
	"github.com/PrPlanIT/CryoSheep/src/adapters/nut"
	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/detect"
	"github.com/PrPlanIT/CryoSheep/src/execute"
	"github.com/PrPlanIT/CryoSheep/src/journal"
	"github.com/PrPlanIT/CryoSheep/src/plan"
)

// common flags shared by the acting subcommands.
type runFlags struct {
	runsDir      string
	upsAddr      string
	upsName      string
	guestTimeout time.Duration
	drainTimeout time.Duration
	dryRun       bool
	deadline     time.Duration
	upsUser      string
	upsPassFile  string
}

func (f *runFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.runsDir, "runs", "/var/lib/cryosheep/runs", "where run journals are kept")
	fs.StringVar(&f.upsAddr, "ups-addr", "", "NUT server host[:port]")
	fs.StringVar(&f.upsName, "ups-name", "ups", "UPS name as upsd knows it")
	fs.DurationVar(&f.guestTimeout, "guest-timeout", 90*time.Second, "per-guest ACPI shutdown budget")
	fs.DurationVar(&f.drainTimeout, "drain-timeout", 60*time.Second, "bound on evicting this node's pods")
	fs.BoolVar(&f.dryRun, "dry-run", false, "walk and record the decisions without performing them")
	fs.DurationVar(&f.deadline, "deadline", 0, "arm the UPS to cut power after this long regardless of the sequence; 0 disables")
	fs.StringVar(&f.upsUser, "ups-user", "", "NUT user permitted to send instant commands")
	fs.StringVar(&f.upsPassFile, "ups-pass-file", "", "file holding that user's password")
}

// password is read from a file or the environment, never from a flag: an
// argument is visible in ps to every user on the host.
func (f *runFlags) password() string {
	if f.upsPassFile != "" {
		b, err := os.ReadFile(f.upsPassFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", f.upsPassFile, err)
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return os.Getenv("CRYOSHEEP_UPS_PASSWORD")
}

// build assembles the plan and the executor for this host.
func (f *runFlags) build(ctx context.Context) (plan.Plan, *execute.Executor, *exec.Runner) {
	host, _ := os.Hostname()
	runner := exec.New()
	roles := detect.Roles(ctx, runner)

	var guests []core.Guest
	for _, r := range roles {
		if r == core.RoleProxmox {
			g, err := runner.Guests(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not list guests: %v\n", err)
			}
			guests = g
		}
	}

	status := "unknown"
	var ups core.UPS
	var deadline core.Deadline
	if f.upsAddr != "" {
		c := nut.New(f.upsAddr, f.upsName)
		ups = c
		if s, err := c.Status(ctx); err == nil {
			status = s
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not read UPS: %v\n", err)
		}
		// The backstop needs an authenticated user; without one the deadline
		// step is planned but skipped, and the run records that it was.
		if pw := f.password(); f.upsUser != "" && pw != "" {
			c.User, c.Pass = f.upsUser, pw
			deadline = c
		}
	}

	opts := plan.Options{
		GuestTimeout: f.guestTimeout,
		DrainTimeout: f.drainTimeout,
		UPSDeadline:  f.deadline,
	}
	p := plan.Build(host, roles, guests, status, opts)
	e := &execute.Executor{Hyp: runner, UPS: ups, Ceph: runner, Kube: runner, Host: runner,
		Deadline: deadline, DryRun: f.dryRun}
	return p, e, runner
}

// runQuiesce performs only the reversible prefix — what can be given back if
// mains return. Invoked from upsmon's NOTIFYCMD on ONBATT, while the budget is
// still abundant and nothing has been committed.
func runQuiesce(args []string) int {
	fs := flag.NewFlagSet("quiesce", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p, e, _ := f.build(ctx)
	p.Steps = p.Steps[:p.PointOfNoReturn()] // reversible prefix only
	if len(p.Steps) == 0 {
		fmt.Println("nothing to quiesce on this host")
		return 0
	}

	w, err := journal.Open(f.runsDir, p.Host, journal.TriggerUPS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: no journal: %v\n", err)
	} else {
		e.Record = w
	}

	res := e.Run(ctx, p)
	if w != nil {
		_ = w.Close(res.Completed, res.Aborted)
	}
	if res.Aborted {
		fmt.Printf("quiesce: stopped before acting — UPS reports %q, so there is nothing to quiesce (reversed %d)\n",
			res.Gate, res.Reversed)
		return 0
	}
	fmt.Printf("quiesce: ran %d step(s)\n", res.Ran)
	return 0
}

// runRestore undoes the last quiesce. Invoked from NOTIFYCMD on ONLINE.
//
// It reverses what the journal says this host actually did, not everything it
// could have done — a noout an operator set for maintenance is not ours to clear.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	runs, err := journal.Load(f.runsDir)
	if err != nil || len(runs) == 0 {
		fmt.Println("restore: no run to undo")
		return 0
	}
	last := runs[0]

	runner := exec.New()
	var ups *nut.Client
	if f.upsAddr != "" {
		ups = nut.New(f.upsAddr, f.upsName)
		ups.User, ups.Pass = f.upsUser, f.password()
	}

	undone := 0
	for i := len(last.Steps) - 1; i >= 0; i-- {
		s := last.Steps[i]
		if s.Outcome != journal.OutcomeDone {
			continue
		}
		var undo func() error
		switch s.Action {
		case string(plan.ActionUPSDeadline):
			// Cancelling the armed power-off is the most urgent undo: left
			// standing, the UPS cuts power even though mains came back.
			if ups != nil {
				undo = func() error { return ups.Cancel(ctx) }
			}
		case string(plan.ActionCephNoout):
			undo = func() error { return runner.UnsetNoout(ctx) }
		}
		if undo == nil {
			continue
		}
		if f.dryRun {
			fmt.Printf("restore: would undo %s (from run %s)\n", s.Action, last.ID)
			undone++
			continue
		}
		if err := undo(); err != nil {
			fmt.Fprintf(os.Stderr, "restore: %s: %v\n", s.Action, err)
			continue
		}
		undone++
	}
	fmt.Printf("restore: undid %d step(s) from run %s\n", undone, last.ID)
	return 0
}

// runHalt performs the whole sequence. Invoked from upsmon's SHUTDOWNCMD, and
// from systemd on a normal reboot.
//
// Ungated on purpose. SHUTDOWNCMD is terminal in NUT — the decision is already
// made — and a normal reboot reads OL, which a gate would treat as "mains are
// back" and abandon, leaving guests running while the host stops underneath them.
func runHalt(args []string) int {
	fs := flag.NewFlagSet("halt", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	trigger := fs.String("trigger", journal.TriggerUPS, "ups | systemd | manual — recorded, and kept apart in calibration")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	p, e, _ := f.build(ctx)
	e.NoGate = true

	w, err := journal.Open(f.runsDir, p.Host, *trigger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: no journal: %v\n", err)
	} else {
		e.Record = w
	}

	res := e.Run(ctx, p)
	if w != nil {
		_ = w.Close(res.Completed, res.Aborted)
	}
	fmt.Printf("halt: ran %d step(s), complete=%v\n", res.Ran, res.Completed)
	return 0
}

// runReport emits run journals that have not been delivered and marks them so a
// reboot cannot send the same incident twice.
//
// Emitted to stdout as one JSON object per line: whatever collects this process
// (journald today, a shipper later) gets the record without CryoSheep needing to
// know where the logs live.
func runReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	runsDir := fs.String("runs", "/var/lib/cryosheep/runs", "where run journals are kept")
	keep := fs.Bool("keep", false, "emit without marking as shipped")
	_ = fs.Parse(args)

	runs, err := journal.Unshipped(*runsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "report: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	for _, r := range runs {
		if err := enc.Encode(r); err != nil {
			return 1
		}
		if !*keep {
			if err := journal.MarkShipped(*runsDir, r.ID); err != nil {
				fmt.Fprintf(os.Stderr, "report: could not mark %s shipped: %v\n", r.ID, err)
			}
		}
	}
	return 0
}
