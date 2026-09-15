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

	"github.com/PrPlanIT/CryoSheep/src/adapters/nut"
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
	case "quiesce":
		os.Exit(runQuiesce(os.Args[2:]))
	case "restore":
		os.Exit(runRestore(os.Args[2:]))
	case "halt":
		os.Exit(runHalt(os.Args[2:]))
	case "report":
		os.Exit(runReport(os.Args[2:]))
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
	fmt.Fprint(os.Stderr, `cryosheep — orderly host shutdown

  plan       show what would happen on this host, changing nothing
  quiesce    perform the reversible prefix        (upsmon NOTIFYCMD, ONBATT)
  restore    undo the last quiesce                (upsmon NOTIFYCMD, ONLINE)
  halt       perform the whole sequence, ungated  (upsmon SHUTDOWNCMD / systemd)
  report     emit undelivered run journals, once
  calibrate  recommend a shutdown trigger from what past runs actually cost
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
	if f.upsAddr != "" {
		if d, err := nut.New(f.upsAddr, f.upsName).Runtime(ctx); err == nil {
			runtime = d
		}
	}
	render(p, runtime)
	return 0
}

func render(p plan.Plan, runtime time.Duration) {
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
			fmt.Printf("  %-6s %-24s %-9s %s\n", g.ID, g.Name, g.Status, order)
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
	runsDir := fs.String("runs", "/var/lib/cryosheep/runs", "directory of run journals")
	margin := fs.Float64("margin", 0.5, "safety margin applied over the worst observed run")
	floor := fs.Duration("floor", 120*time.Second, "never recommend below this")
	upsAddr := fs.String("ups-addr", "", "NUT server host[:port], to check the recommendation fits")
	upsName := fs.String("ups-name", "ups", "UPS name as upsd knows it")
	_ = fs.Parse(args)

	host, _ := os.Hostname()
	runs, err := calibrate.LoadRuns(*runsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", *runsDir, err)
	}

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

	if *upsAddr != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if s, err := nut.New(*upsAddr, *upsName).Runtime(ctx); err == nil {
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
