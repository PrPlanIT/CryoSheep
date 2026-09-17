// Command cryosheep sequences the orderly shutdown of a host and its guests.
//
// It is invoked, never resident: by systemd on a normal reboot, and by upsmon's
// SHUTDOWNCMD when a UPS gives up. upsmon owns the event stream and decides when
// a sequence starts; cryosheep owns the sequence itself.
//
// `plan` is the subcommand that matters most day to day. It reads the host and
// prints exactly what would happen, changing nothing — which is the only way to
// rehearse a routine whose real execution cannot be practised safely.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/audit"
	"github.com/PrPlanIT/CryoSheep/src/calibrate"
	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/plan"
	"github.com/PrPlanIT/CryoSheep/src/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "plan":
		os.Exit(runPlan(os.Args[2:]))
	case "conserve":
		os.Exit(runConserve(os.Args[2:]))
	case "sleep":
		os.Exit(runSleep(os.Args[2:]))
	case "cancel":
		os.Exit(runCancel(os.Args[2:]))
	case "wake":
		os.Exit(runWake(os.Args[2:]))
	case "calibrate":
		os.Exit(runCalibrate(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Printf("cryosheep %s (%s, built %s)\n", version.Version, version.Commit, version.BuildDate)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cryosheep — put a machine into stasis, and bring it back

  plan       show what would happen here, changing nothing
  conserve   shed load and hold, reversibly        (upsmon NOTIFYCMD, ONBATT)
  sleep      put this machine into stasis         (upsmon SHUTDOWNCMD / systemd)
             a UPS-triggered sleep abandons if mains return; a requested one does not
  cancel     abandon a sleep and undo it          (upsmon NOTIFYCMD, ONLINE)
  wake       revive after stasis                  (on boot)
  calibrate  recommend a threshold from what past runs actually cost
  version    build identity

`)
}

func runPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, _, _ := f.build(ctx)

	// The worst case is what has to fit, not the typical one.
	var runtime time.Duration
	if c, ok := upsFor(); ok {
		if d, err := c.Runtime(ctx); err == nil {
			runtime = d
		}
	}
	render(p, runtime, guestFallback)
	return 0
}

func render(p plan.Plan, runtime, defaultBudget time.Duration) {
	fmt.Printf("host:  %s\n", p.Host)
	fmt.Printf("roles: %s\n", join(p.Roles))
	fmt.Printf("ups:   %s\n", p.UPSStatus)
	ordered := 0
	if len(p.Guests) > 0 {
		fmt.Println("guests:")
		for _, g := range p.Guests {
			order := "-"
			if g.Order != core.OrderUnset && g.Order != 0 {
				order = fmt.Sprintf("order=%d", g.Order)
				if g.Running() {
					ordered++
				}
			}
			down := ""
			if g.Down > 0 {
				down = fmt.Sprintf("down=%s", g.Down)
			}
			fmt.Printf("  %-6s %-24s %-9s %-9s %s\n", g.ID, g.Name, g.Status, order, down)
		}
	}
	fmt.Println("plan:")
	pnr := p.PointOfNoReturn()
	for i, s := range p.Steps {
		mark := "reversible"
		if i >= pnr {
			mark = "COMMITS THE HOST"
		}
		t := ""
		if s.Timeout > 0 {
			t = fmt.Sprintf(" (≤%s)", s.Timeout)
		}
		target := s.Target
		if target == "" {
			target = s.Detail
		} else if s.Detail != "" {
			target = fmt.Sprintf("%s %s", s.Target, s.Detail)
		}
		fmt.Printf("  %d. %-18s %-28s%s  [%s]\n", i+1, s.Action, target, t, mark)
	}
	if pnr < len(p.Steps) {
		fmt.Printf("\npoint of no return: step %d — power returning before it aborts and reverses\n", pnr+1)
	}

	// Guests stop in reverse startup order. With none set the order is whatever
	// qm list returned, which is VMID ascending — so a firewall on a low VMID
	// would be the first thing stopped.
	running := 0
	for _, g := range p.Guests {
		if g.Running() {
			running++
		}
	}
	// A guest with no down= falls back to the flat timeout, which is what makes a
	// sequence longer than it needs to be.
	var nodown int
	for _, g := range p.Guests {
		if g.Running() && g.Down == 0 {
			nodown++
		}
	}
	if nodown > 0 {
		fmt.Printf("\n%d running guest(s) declare no `down=`, so each is budgeted %s.\n"+
			"  set it per guest to shorten the sequence: qm set <id> --startup order=N,down=SECONDS\n",
			nodown, defaultBudget)
	}
	if running > 0 && ordered == 0 {
		fmt.Printf("\nno guest declares `startup: order=` — they will stop in VMID order.\n" +
			"  set it in Proxmox to control this: order=1 boots first and stops last.\n")
	}

	// Worst case has to fit inside the battery, not the typical case.
	var worst time.Duration
	for _, s := range p.Steps {
		worst += s.Timeout
	}
	if worst > 0 {
		line := fmt.Sprintf("\nworst case: %s", worst)
		if runtime > 0 {
			line += fmt.Sprintf("   battery now: %s", runtime)
		}
		fmt.Println(line)
		if runtime > 0 && worst > runtime {
			fmt.Printf("  WARNING: the sequence cannot complete at this load — %s short.\n"+
				"  shorten --guest-timeout, stop non-critical guests earlier, or rely on the UPS deadline.\n",
				worst-runtime)
		}
	}
}

func join(roles []core.Role) string {
	if len(roles) == 0 {
		return "(none)"
	}
	out := ""
	for i, r := range roles {
		if i > 0 {
			out += ", "
		}
		out += string(r)
	}
	return out
}

// runCalibrate reports what the trigger threshold should be, from what past runs
// actually cost. It reads history and prints; it changes no configuration,
// because a threshold with the estate behind it should be applied by a person
// who has seen the numbers.
func runCalibrate(args []string) int {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	margin := fs.Float64("margin", 0.5, "safety margin applied over the worst observed run")
	floor := fs.Duration("floor", 120*time.Second, "never recommend below this")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	host, _ := os.Hostname()
	past, err := audit.LoadRuns(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read past runs: %v\n", err)
	}
	runs := calibrate.FromAudit(past)

	sum := calibrate.Summarize(host, runs)
	rec := calibrate.Recommend(sum, *margin, *floor)

	fmt.Printf("host:      %s\n", host)
	fmt.Printf("runs:      %d completed\n", sum.N)
	if sum.N > 0 {
		fmt.Printf("duration:  p50 %s  p95 %s  max %s\n", sum.P50, sum.P95, sum.Max)
	}
	fmt.Printf("recommend: override.battery.runtime.low = %d   (%s)\n",
		int(rec.RuntimeLow.Seconds()), rec.RuntimeLow)
	if rec.Note != "" {
		fmt.Printf("note:      %s\n", rec.Note)
	}

	if c, ok := upsFor(); ok {
		if s, err := c.Runtime(ctx); err == nil {
			ok, why := calibrate.Feasible(rec, s)
			fmt.Printf("battery:   %s remaining now\n", s)
			if !ok {
				fmt.Fprintf(os.Stderr, "INFEASIBLE: %s\n", why)
				return 1
			}
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not read UPS runtime: %v\n", err)
		}
	}
	return 0
}
