package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/PrPlanIT/CryoSheep/src/adapters/remote"
	"os"
	"strconv"
	"strings"
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
//
// It matches Proxmox's own default deliberately. CryoSheep passes --timeout on
// every qm shutdown, so this value replaces the platform's rather than adding to
// it — and a fallback below the default is a silent downgrade: installing the
// tool would make an undeclared guest less patient than it was before, which is
// the opposite of the point. Raise a specific guest with `startup: down=`, which
// is the field this reads and the one place a per-guest budget belongs.
const guestFallback = core.DefaultGuestTimeout

// sequenceTimeout bounds the whole run, and must stay one minute inside the
// unit's TimeoutStopSec so this fires first and the run exits having recorded
// why, rather than being killed mid-step with nothing written.
//
// Both numbers sit below the UPS runtime on purpose. It was fifteen minutes
// against a measured 461s of battery, which is not a bound at all — the power
// would decide the outcome long before the timeout did. The slowest host's
// realistic sequence is 370s including host halt, so this clears it with margin
// and still cuts losses on anything pathological while there is power to halt on.
const sequenceTimeout = 6 * time.Minute

// runFlags is the whole configurable surface.
//
// Everything else is either a constant, or something another system already
// owns: the UPS connection lives in upsmon.conf, the per-guest budget lives in
// Proxmox, and why we are stopping is read from the environment rather than
// declared. A flag that can contradict the truth is worse than no flag at all.
type runFlags struct {
	dryRun      bool
	upsDeadline time.Duration

	// guestConcurrency is how many of a host's guests may stop at once, and
	// holdUntil is how long to wait before the first step that cannot be undone.
	// Both are per-host policy rather than per-invocation choices, so both read
	// an environment default the unit file can carry.
	guestConcurrency int
	holdUntil        time.Duration

	// orderGroups names spans of startup order that stop together, so a power
	// failure can use the estate's real dependency shape rather than its finer
	// startup tiers. Empty keeps every tier its own wave, which is what a planned
	// shutdown of a single node wants.
	orderGroups string

	// kubeconfig names the identity kubectl should use. See resolveKubeconfig.
	kubeconfig string

	// trigger is detected, not parsed — see detectTrigger.
	trigger string
}

// kubeletConf is the node's own kubelet identity. A kubelet may patch its own
// Node object, which is all the k8s teardown needs, so this is the right default
// for a unit that must work with no operator credentials present.
const kubeletConf = "/etc/kubernetes/kubelet.conf"

// resolveKubeconfig finds an identity for kubectl, most explicit first.
//
// The fallback to the kubelet's own config is what lets the shutdown handler
// work under systemd, where there is no KUBECONFIG and no HOME. Without it
// kubectl quietly tries localhost:8080 and every k8s step becomes a no-op that
// still reports success.
func resolveKubeconfig(flagValue string) string {
	for _, c := range []string{
		flagValue,
		os.Getenv("CRYOSHEEP_KUBECONFIG"),
		os.Getenv("KUBECONFIG"),
	} {
		if c != "" {
			return c
		}
	}
	if _, err := os.Stat(kubeletConf); err == nil {
		return kubeletConf
	}
	return ""
}

func (f *runFlags) bind(fs *flag.FlagSet) {
	f.trigger = detectTrigger()
	fs.BoolVar(&f.dryRun, "dry-run", false,
		"walk and record the decisions without performing them")
	fs.DurationVar(&f.upsDeadline, "ups-deadline", envDuration("CRYOSHEEP_UPS_DEADLINE"),
		"when the UPS cuts power regardless of the sequence; 0 disables")
	fs.StringVar(&f.kubeconfig, "kubeconfig", "",
		"identity for kubectl; defaults to $CRYOSHEEP_KUBECONFIG, $KUBECONFIG, then "+kubeletConf)
	fs.IntVar(&f.guestConcurrency, "guest-concurrency", envInt("CRYOSHEEP_GUEST_CONCURRENCY"),
		"how many guests may stop at once; 1 one at a time, -1 all of a tier together, 0 takes the default (1)")
	fs.StringVar(&f.orderGroups, "order-groups", os.Getenv("CRYOSHEEP_ORDER_GROUPS"),
		"startup-order ranges that stop together, in sequence, e.g. \"5-99,4,2-3,1\"; empty keeps every tier its own wave")
	fs.DurationVar(&f.holdUntil, "hold-until", envDuration("CRYOSHEEP_HOLD_UNTIL"),
		"wait this long from the start before the first step that cannot be undone, so hosts commit together; 0 disables")
}

// parseOrderGroups reads "5-99,4,2-3,1" into the ranges that stop together.
//
// A malformed value yields nothing rather than something partial, because a
// partial grouping is a guest left out of the sequence. The planner discards
// incomplete cover for the same reason, so both ends fail toward the careful
// per-tier shape rather than away from it.
func parseOrderGroups(spec string) []plan.OrderGroup {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	var out []plan.OrderGroup
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		lo, hi, found := strings.Cut(part, "-")
		if !found {
			hi = lo
		}
		l, errL := strconv.Atoi(strings.TrimSpace(lo))
		h, errH := strconv.Atoi(strings.TrimSpace(hi))
		if errL != nil || errH != nil || l > h || l < 1 {
			fmt.Fprintf(os.Stderr,
				"warning: order-groups %q is malformed at %q, ignoring the whole value\n", spec, part)
			return nil
		}
		out = append(out, plan.OrderGroup{Lo: l, Hi: h})
	}
	return out
}

// envInt reads a per-host policy number from the unit file, the same way
// envDuration does, so a value that belongs to a machine is set once where
// ansible manages it rather than repeated at every call site.
func envInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a number, ignoring\n", key, v)
		return 0
	}
	return n
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

// powerIsTheReason reports whether the UPS says power is why we are stopping.
//
// Asked of the UPS rather than inferred from whatever launched us, because the
// launcher cannot say. Proved against NUT 2.8.3: upsmon sets NOTIFYTYPE for
// NOTIFYCMD and passes nothing at all to SHUTDOWNCMD — and because upsmon runs
// as a systemd service, its children inherit INVOCATION_ID, so an environment
// sniff concludes "systemd asked for this" on exactly the path where nobody
// did. That answer switches off the gate, and the gate is what lets mains
// returning abandon a sequence nobody needed.
//
// The UPS is the authority on this, and the gate already re-reads it to decide
// whether to abandon — so both decisions now come from one source that cannot
// disagree with itself.
//
// An unreadable UPS reads false, which is the honest answer rather than a
// pessimistic one: without it there is no way to observe mains returning, so
// there is nothing to gate on either way.
func powerIsTheReason(status string) bool {
	return nut.Flag(status, core.StatusOnBattery) || nut.Flag(status, core.StatusFSD)
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
	runner.Kubeconfig = resolveKubeconfig(f.kubeconfig)
	roles := detect.Roles(ctx, runner)

	// Said loudly because the alternative is silence. Without an identity every
	// kubernetes step still reports success while doing nothing, and the node
	// goes down uncordoned with its mounts held.
	if runner.Kubeconfig == "" {
		for _, r := range roles {
			if r == core.RoleKubelet {
				fmt.Fprintf(os.Stderr,
					"warning: this node runs kubelet but no kubeconfig was found — "+
						"cordon and the quorum record will not work; set --kubeconfig or CRYOSHEEP_KUBECONFIG\n")
			}
		}
	}

	// A remote target replaces the local hypervisor and the local halt, and
	// nothing else: Ceph and kubernetes steps are about this machine, and this
	// machine is not the one being stopped. Left nil they are planned and
	// recorded as skipped, which is the honest account of a run that had no
	// authority over them.
	var hyp core.Hypervisor = runner
	var halt core.Host = runner
	if px, name, ok := remoteTarget(); ok {
		hyp, halt = px, px
		host = name
		roles = []core.Role{core.RoleProxmox}
		runner.Kubeconfig = ""
	}

	var guests []core.Guest
	for _, r := range roles {
		if r == core.RoleProxmox {
			g, err := hyp.Guests(ctx)
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
		GuestTimeout:     guestFallback,
		UPSDeadline:      f.upsDeadline,
		GuestConcurrency: f.guestConcurrency,
		OrderGroups:      parseOrderGroups(f.orderGroups),
		HoldUntil:        f.holdUntil,
		// systemd invoked us from a unit it is already stopping, so the poweroff
		// or reboot is in flight. Halting on top of that turns a reboot into a
		// poweroff — a node patched by ansible would never come back.
		OmitHalt: f.trigger == audit.TriggerSystemd,
	}
	p := plan.Build(host, roles, guests, status, opts)
	e := &execute.Executor{Hyp: hyp, UPS: ups, Ceph: runner, Kube: runner, Host: halt,
		Deadline: deadline, DryRun: f.dryRun, PowerLoss: powerIsTheReason(status)}
	if _, _, remote := remoteTarget(); remote {
		// Nothing local is being stopped, so the collaborators that act on this
		// machine are dropped rather than left pointing at it.
		e.Ceph, e.Kube = nil, nil
	}
	return p, e, runner
}

// remoteTarget builds a hypervisor for a machine CryoSheep is not running on.
//
// Environment rather than flags for the same reason the UPS deadline is: it is
// per-host configuration that belongs in a unit file ansible manages, not an
// argument repeated at every call site. All four must be present — a partial
// configuration is a mistake, and falling back to the local machine silently
// would stop the wrong thing.
func remoteTarget() (*remote.Proxmox, string, bool) {
	base := os.Getenv("CRYOSHEEP_PROXMOX_URL")
	id := os.Getenv("CRYOSHEEP_PROXMOX_TOKEN_ID")
	secret := os.Getenv("CRYOSHEEP_PROXMOX_TOKEN_SECRET")
	node := os.Getenv("CRYOSHEEP_PROXMOX_NODE")
	if base == "" || id == "" || secret == "" || node == "" {
		if base != "" || id != "" || secret != "" || node != "" {
			fmt.Fprintln(os.Stderr,
				"warning: CRYOSHEEP_PROXMOX_* is incomplete — all of URL, TOKEN_ID, "+
					"TOKEN_SECRET and NODE are needed; acting on the local machine instead")
		}
		return nil, "", false
	}
	return &remote.Proxmox{
		Client: &remote.Client{Base: strings.TrimRight(base, "/"),
			Auth: remote.TokenAuth{TokenID: id, Secret: secret}},
		Node: node,
	}, node, true
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

	ctx, cancel := context.WithTimeout(context.Background(), sequenceTimeout)
	defer cancel()

	p, e, runner := f.build(ctx)

	// systemd asked, but is the machine actually going down?
	//
	// ExecStop fires whenever the unit stops, including when an operator runs
	// `systemctl stop`. Acting on that would cordon a healthy node and release
	// its mounts because somebody restarted a service. systemd knows whether a
	// shutdown is in progress, so ask it rather than assume.
	// A rehearsal performs nothing, so refusing it protects nobody and prevents
	// the only way to see this path before trusting it on a real shutdown.
	if !f.dryRun && f.trigger == audit.TriggerSystemd && !runner.SystemStopping(ctx) {
		fmt.Println("sleep: the unit stopped but the machine is not shutting down — nothing to do")
		return 0
	}

	// Why we are stopping decides whether power returning matters.
	//
	// A power-loss sleep is conditional: mains coming back means the reason is
	// gone, so the sequence abandons and everything it took down goes back.
	// An operator or a systemd reboot asked for this, and the UPS reporting
	// healthy power is simply not news — gating there would abandon a shutdown
	// that was requested, leaving guests running while the host stops under them.
	e.NoGate = !e.PowerLoss

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
	runner.Kubeconfig = resolveKubeconfig(f.kubeconfig)
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
