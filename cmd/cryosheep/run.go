package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/adapters/exec"
	"github.com/PrPlanIT/CryoSheep/src/adapters/nut"
	"github.com/PrPlanIT/CryoSheep/src/audit"
	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/detect"
	"github.com/PrPlanIT/CryoSheep/src/execute"
	"github.com/PrPlanIT/CryoSheep/src/plan"
)

// guestFallback bounds a guest's ACPI shutdown when Proxmox has no `down=` for
// it. Not configurable: Proxmox is already where a per-guest budget belongs, and
// a flag here would only be a worse copy of a field that already exists.
const guestFallback = 90 * time.Second

// runFlags is the whole configurable surface.
//
// Everything else is either a constant, or something another system already
// owns: the UPS connection lives in upsmon.conf, the per-guest budget lives in
// Proxmox, and why we are stopping is read from the environment rather than
// declared. A flag that can contradict the truth is worse than no flag at all.
type runFlags struct {
	dryRun      bool
	upsDeadline time.Duration

	// trigger is detected, not parsed — see detectTrigger.
	trigger string
}

func (f *runFlags) bind(fs *flag.FlagSet) {
	f.trigger = detectTrigger()
	fs.BoolVar(&f.dryRun, "dry-run", false,
		"walk and record the decisions without performing them")
	fs.DurationVar(&f.upsDeadline, "ups-deadline", envDuration("CRYOSHEEP_UPS_DEADLINE"),
		"when the UPS cuts power regardless of the sequence; 0 disables")
}

// envDuration lets the one policy number be set per host in the unit file, where
// ansible can manage it, without it becoming an argument that has to be repeated
// at every call site.
func envDuration(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a duration, ignoring\n", key, v)
		return 0
	}
	return d
}

// detectTrigger establishes why this run is happening, from the environment of
// whatever started it.
//
// Read rather than declared. The caller already knows — upsmon exports
// NOTIFYTYPE, systemd exports INVOCATION_ID — and asking it to say so again only
// creates a way for it to say something untrue. Whether power returning matters
// depends on this answer, so being told the wrong one is not survivable.
func detectTrigger() string {
	switch {
	case os.Getenv("NOTIFYTYPE") != "":
		return audit.TriggerUPS
	case os.Getenv("INVOCATION_ID") != "":
		return audit.TriggerSystemd
	default:
		return audit.TriggerManual
	}
}

// upsFor opens a client from what NUT already records on this host. A machine
// with no upsmon.conf has no UPS, which is correct for a guest: it is told to
// stop by something else and simply stops well.
func upsFor() (*nut.Client, bool) {
	m, ok := nut.ReadMonitor(nut.UpsmonConf)
	if !ok || m.Addr == "" {
		return nil, false
	}
	c := nut.New(m.Addr, m.Name)
	c.User, c.Pass = m.User, m.Pass
	return c, true
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
	if c, ok := upsFor(); ok {
		ups = c
		if r, err := c.Read(ctx); err == nil {
			status = r.Status
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not read UPS: %v\n", err)
		}
		// The backstop needs an authenticated user; without one the deadline
		// step is planned but skipped, and the run records that it was.
		if c.User != "" && c.Pass != "" {
			deadline = c
		}
	}

	opts := plan.Options{
		GuestTimeout: guestFallback,
		UPSDeadline:  f.upsDeadline,
		// systemd invoked us from a unit it is already stopping, so the poweroff
		// or reboot is in flight. Halting on top of that turns a reboot into a
		// poweroff — a node patched by ansible would never come back.
		OmitHalt: f.trigger == audit.TriggerSystemd,
	}
	p := plan.Build(host, roles, guests, status, opts)
	e := &execute.Executor{Hyp: runner, UPS: ups, Ceph: runner, Kube: runner, Host: runner,
		Deadline: deadline, DryRun: f.dryRun}
	return p, e, runner
}

// runConserve performs only the reversible prefix — what can be given back if
// mains return. Invoked from upsmon's NOTIFYCMD on ONBATT, while the budget is
// still abundant and nothing has been committed.
func runConserve(args []string) int {
	fs := flag.NewFlagSet("conserve", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p, e, _ := f.build(ctx)
	p.Steps = p.Steps[:p.PointOfNoReturn()] // reversible prefix only
	if len(p.Steps) == 0 {
		fmt.Println("nothing to conserve on this host")
		return 0
	}

	w := audit.Open(p.Host, audit.TriggerUPS)
	e.Record = w

	res := e.Run(ctx, p)
	_ = w.Close(res.Completed, res.Aborted)

	if res.Aborted {
		fmt.Printf("conserve: stopped before acting — UPS reports %q, so there is nothing to conserve (reversed %d)\n",
			res.Gate, res.Reversed)
		return 0
	}
	fmt.Printf("conserve: ran %d step(s)\n", res.Ran)
	return 0
}

// runCancel abandons a sleep in progress and undoes what it did. Invoked from
// NOTIFYCMD on ONLINE.
//
// It reverses what the journal says this host actually did, not everything it
// could have done — a noout an operator set for maintenance is not ours to clear.
func runCancel(args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	runs, err := audit.LoadRuns(ctx)
	if err != nil || len(runs) == 0 {
		fmt.Println("cancel: no run to undo")
		return 0
	}
	last := runs[0]

	undone := undoRun(ctx, f, last)
	fmt.Printf("cancel: undid %d step(s) from run %s\n", undone, last.ID)
	return 0
}

// runSleep performs the whole sequence. Invoked from upsmon's SHUTDOWNCMD, and
// from systemd on a normal shutdown or reboot.
func runSleep(args []string) int {
	fs := flag.NewFlagSet("sleep", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	p, e, _ := f.build(ctx)

	// Why we are stopping decides whether power returning matters.
	//
	// A power-loss sleep is conditional: mains coming back means the reason is
	// gone, so the sequence abandons and everything it took down goes back.
	// An operator or a systemd reboot asked for this, and the UPS reporting
	// healthy power is simply not news — gating there would abandon a shutdown
	// that was requested, leaving guests running while the host stops under them.
	e.NoGate = f.trigger != audit.TriggerUPS

	w := audit.Open(p.Host, f.trigger)
	e.Record = w

	res := e.Run(ctx, p)
	_ = w.Close(res.Completed, res.Aborted)

	if res.Aborted {
		fmt.Printf("sleep: abandoned — UPS reports %q, so the reason for stopping is gone; undid %d action(s)\n",
			res.Gate, res.Reversed)
		return 0
	}
	fmt.Printf("sleep: ran %d step(s), complete=%v\n", res.Ran, res.Completed)
	return 0
}

// runWake revives a machine after stasis.
//
// Stopped is never the desired state, so coming back is as much a part of this
// as going down. On boot, wake undoes what the last sleep did — uncordons the
// node, starts the guests it stopped, clears noout — and says whether the stop
// it is recovering from was orderly or a cut.
//
// It undoes CryoSheep's own actions and nothing else. Databases and quorum
// services bring themselves back: they are built to, given a clean stop, and a
// second thing deciding who is primary is how split brain happens. If their
// recovery goes wrong, the journal says what was true beforehand — for a person
// to read, not for this to act on.
func runWake(args []string) int {
	fs := flag.NewFlagSet("wake", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	runs, err := audit.LoadRuns(ctx)
	if err != nil || len(runs) == 0 {
		fmt.Println("wake: nothing to revive")
		return 0
	}

	last := runs[0]
	how := "graceful"
	if last.WasCut() {
		how = "cut"
	}
	fmt.Printf("wake: reviving from run %s (%s) — previous stop was %s\n", last.ID, last.Trigger, how)

	revived := undoRun(ctx, f, last)
	fmt.Printf("wake: undid %d action(s)\n", revived)
	return 0
}

// undoRun reverses the actions a run recorded, newest first, and only those it
// actually completed — a noout or a cordon an operator set by hand is not ours
// to clear.
func undoRun(ctx context.Context, f runFlags, run audit.Run) int {
	runner := exec.New()
	ups, _ := upsFor()

	undone := 0
	for i := len(run.Steps) - 1; i >= 0; i-- {
		s := run.Steps[i]
		if s.Outcome != audit.OutcomeDone {
			continue
		}
		var undo func() error
		var what string
		switch s.Action {
		case string(plan.ActionUPSDeadline):
			// Most urgent: left standing, the UPS cuts power to a machine that
			// has already come back.
			if ups != nil {
				undo, what = func() error { return ups.Cancel(ctx) }, "cancel UPS deadline"
			}
		case string(plan.ActionK8sCordon):
			undo, what = func() error { return runner.Uncordon(ctx, s.Target) }, "uncordon "+s.Target
		case string(plan.ActionGuestStop):
			undo, what = func() error { return runner.Start(ctx, s.Target) }, "start guest "+s.Target
		case string(plan.ActionCephNoout):
			undo, what = func() error { return runner.UnsetNoout(ctx) }, "unset noout"
		}
		if undo == nil {
			continue
		}
		if f.dryRun {
			fmt.Printf("  would %s\n", what)
			undone++
			continue
		}
		if err := undo(); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", what, err)
			continue
		}
		fmt.Printf("  %s\n", what)
		undone++
	}
	return undone
}
