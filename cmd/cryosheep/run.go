package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/PrPlanIT/CryoSheep/src/adapters/remote"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	dryRun bool

	// guestConcurrency is how many of a host's guests may stop at once, and
	// holdUntil is how long to wait before the first step that cannot be undone.
	// Both are per-host policy rather than per-invocation choices, so both read
	// an environment default the unit file can carry.
	guestConcurrency int
	holdUntil        time.Duration

	// upsCut is the delay sent with shutdown.return before the halt of a power
	// event; 0 sends nothing.
	upsCut time.Duration

	// orderGroups names spans of startup order that stop together, so a power
	// failure can use the estate's real dependency shape rather than its finer
	// startup tiers. Empty keeps every tier its own wave, which is what a planned
	// shutdown of a single node wants.
	orderGroups string

	// kubeconfig names the identity kubectl should use. See resolveKubeconfig.
	kubeconfig string

	// trigger is detected, not parsed — see detectTrigger.
	trigger string

	// asPowerLoss plans as though power were the reason, so `plan` can show the
	// power-event shape on an ordinary day.
	asPowerLoss bool
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
	fs.StringVar(&f.kubeconfig, "kubeconfig", "",
		"identity for kubectl; defaults to $CRYOSHEEP_KUBECONFIG, $KUBECONFIG, then "+kubeletConf)
	fs.IntVar(&f.guestConcurrency, "guest-concurrency", envInt("CRYOSHEEP_GUEST_CONCURRENCY"),
		"how many guests may stop at once; 1 one at a time, -1 all of a tier together, 0 takes the default (1)")
	fs.StringVar(&f.orderGroups, "order-groups", os.Getenv("CRYOSHEEP_ORDER_GROUPS"),
		"startup-order ranges that stop together, in sequence; @d holds a range until d from the start or until no earlier range runs anywhere, e.g. \"4-99,1-3@4m30s\"; empty keeps every tier its own wave")
	fs.DurationVar(&f.holdUntil, "hold-until", envDuration("CRYOSHEEP_HOLD_UNTIL"),
		"wait this long from the start before the first step that cannot be undone, so hosts commit together; 0 disables")
	fs.DurationVar(&f.upsCut, "ups-cut", envDuration("CRYOSHEEP_UPS_CUT"),
		"on a power event, send shutdown.return with this delay just before the halt; 0 sends nothing")
}

// parseOrderGroups reads "4-99,1-3@4m30s" into the ranges that stop together.
// A range suffixed "@d" may not start before d from the start of the run.
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
		span, at, held := strings.Cut(part, "@")
		var notBefore time.Duration
		var errD error
		if held {
			notBefore, errD = time.ParseDuration(strings.TrimSpace(at))
		}
		lo, hi, found := strings.Cut(span, "-")
		if !found {
			hi = lo
		}
		l, errL := strconv.Atoi(strings.TrimSpace(lo))
		h, errH := strconv.Atoi(strings.TrimSpace(hi))
		if errL != nil || errH != nil || errD != nil || l > h || l < 1 || notBefore < 0 {
			fmt.Fprintf(os.Stderr,
				"warning: order-groups %q is malformed at %q, ignoring the whole value\n", spec, part)
			return nil
		}
		out = append(out, plan.OrderGroup{Lo: l, Hi: h, NotBefore: notBefore})
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
// An unreadable UPS reads false, unless upsmon launched us: it runs SHUTDOWNCMD
// only on a power decision, and the endpoints can be gone by the time we read
// them. Planning that run as a plain stop drops the holds that keep the routers
// up for every other host.
func powerIsTheReason(status, trigger string) bool {
	if status == "" || status == "unknown" {
		return trigger == audit.TriggerUPS
	}
	return nut.Flag(status, core.StatusOnBattery) || nut.Flag(status, core.StatusFSD)
}

// detectTrigger establishes why this run is happening, from the environment of
// whatever started it.
//
// Read rather than declared. The caller already knows, and asking it to say so
// again only creates a way for it to say something untrue. Whether the halt is
// ours to perform depends on this answer, so being told the wrong one is not
// survivable.
//
// systemd passes SERVICE_RESULT only to ExecStop and ExecStopPost, so it marks
// exactly the stasis unit being stopped. INVOCATION_ID does not: every child of
// any service inherits it, including upsmon's SHUTDOWNCMD, which reading it as
// "systemd is stopping us" turned into a run that did nothing. A child of a
// service that is not our ExecStop is upsmon.
func detectTrigger() string {
	switch {
	case os.Getenv("SERVICE_RESULT") != "":
		return audit.TriggerSystemd
	case os.Getenv("NOTIFYTYPE") != "", os.Getenv("INVOCATION_ID") != "":
		return audit.TriggerUPS
	default:
		return audit.TriggerManual
	}
}

// upsFor opens every upsd endpoint NUT already records on this host. A machine
// with no upsmon.conf has no UPS, which is correct for a guest: it is told to
// stop by something else and simply stops well.
func upsFor() (nut.Endpoints, bool) {
	ms, ok := nut.ReadMonitors(nut.UpsmonConf)
	if !ok {
		return nil, false
	}
	var e nut.Endpoints
	for _, m := range ms {
		if m.Addr != "" {
			c := nut.New(m.Addr, m.Name)
			c.User, c.Pass = m.User, m.Pass
			e = append(e, c)
		}
	}
	return e, len(e) > 0
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
	var cutter core.Cutter
	if c, ok := upsFor(); ok {
		ups, cutter = c, c
		if r, err := c.Read(ctx); err == nil {
			status = r.Status
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not read UPS: %v\n", err)
		}
	}

	powerLoss := powerIsTheReason(status, f.trigger)
	opts := plan.Options{
		GuestTimeout:     guestFallback,
		GuestConcurrency: f.guestConcurrency,
		// systemd invoked us from a unit it is already stopping, so the poweroff
		// or reboot is in flight. Halting on top of that turns a reboot into a
		// poweroff — a node patched by ansible would never come back.
		OmitHalt: f.trigger == audit.TriggerSystemd,
	}
	// Grouping and holds are policy for a power event, when every host acts on
	// one signal. A host stopped on its own — a reboot, maintenance — keeps the
	// careful per-tier shape and waits on nobody: the rest of the estate is still
	// running, so a hold on it would only ever run to its ceiling.
	if powerLoss || f.asPowerLoss {
		opts.OrderGroups = parseOrderGroups(f.orderGroups)
		opts.HoldUntil = f.holdUntil
		opts.UPSCut = f.upsCut
	}
	p := plan.Build(host, roles, guests, status, opts)
	e := &execute.Executor{Hyp: hyp, UPS: ups, Ceph: runner, Kube: runner, Host: halt, Cutter: cutter,
		DryRun: f.dryRun, PowerLoss: powerLoss}
	if _, _, remote := remoteTarget(); remote {
		// Nothing local is being stopped, so the collaborators that act on this
		// machine are dropped rather than left pointing at it.
		e.Ceph, e.Kube = nil, nil
	} else {
		for _, r := range roles {
			if r == core.RoleProxmox {
				e.Estate = runner
			}
		}
	}
	return p, e, runner
}

// remoteTarget builds a hypervisor for a machine CryoSheep is not running on.
//
// Environment rather than flags for the same reason the order groups are: it is
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

// runConserve stops this host's guests tagged shed, and nothing else — what can
// be given back if mains return. Invoked once an outage is confirmed (upssched,
// after the linger), while the budget is still abundant and nothing has been
// committed.
func runConserve(args []string) int {
	fs := flag.NewFlagSet("conserve", flag.ExitOnError)
	var f runFlags
	f.bind(fs)
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	defer lockUndo()()

	full, e, _ := f.build(ctx)
	p := plan.Shed(full.Host, full.Guests, full.UPSStatus, plan.Options{GuestTimeout: guestFallback})
	if len(p.Steps) == 0 {
		fmt.Println("nothing to conserve on this host: no running guest is tagged " + core.TagShed)
		return 0
	}

	w := audit.Open(p.Host, audit.TriggerUPS, audit.VerbConserve)
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

	defer lockUndo()()
	// A sequence still running abandons and reverses itself on mains; undoing
	// its unfinished run here as well would start the same guests twice.
	if unlock, ok := lockSleep(true); ok {
		defer unlock()
	}

	runs, _ := audit.LoadRuns(ctx)
	ep := audit.Episode(runs)
	if len(ep) == 0 {
		fmt.Println("cancel: no run to undo")
		return 0
	}
	for _, r := range ep {
		fmt.Printf("cancel: undid %d step(s) from run %s\n", undoRun(ctx, f, r), r.ID)
	}
	recordUndo(f, audit.VerbCancel)
	return 0
}

// lockUndo serialises conserve, cancel and wake on this host. Every endpoint
// reports each transition, so upssched runs each of them once per endpoint.
// It returns the unlock.
func lockUndo() func() {
	for _, p := range []string{"/run/cryosheep.lock", filepath.Join(os.TempDir(), "cryosheep.lock")} {
		fd, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if err := syscall.Flock(int(fd.Fd()), syscall.LOCK_EX); err != nil {
			fd.Close()
			continue
		}
		return func() { fd.Close() }
	}
	fmt.Fprintln(os.Stderr, "warning: no lock; running unserialised")
	return func() {}
}

// lockSleep takes this host's sequence lock. Without wait, ok is false only
// when another sequence holds it; a lock that cannot be opened at all does not
// stop a shutdown.
func lockSleep(wait bool) (func(), bool) {
	how := syscall.LOCK_EX | syscall.LOCK_NB
	if wait {
		how = syscall.LOCK_EX
	}
	for _, p := range []string{"/run/cryosheep-sleep.lock", filepath.Join(os.TempDir(), "cryosheep-sleep.lock")} {
		fd, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if err := syscall.Flock(int(fd.Fd()), how); err != nil {
			fd.Close()
			if err == syscall.EWOULDBLOCK {
				return nil, false
			}
			continue
		}
		return func() { fd.Close() }, true
	}
	fmt.Fprintln(os.Stderr, "warning: no sequence lock; running unserialised")
	return func() {}, true
}

// recordUndo marks the episode undone, so the next cancel or wake leaves it.
func recordUndo(f runFlags, verb string) {
	if f.dryRun {
		return
	}
	host, _ := os.Hostname()
	_ = audit.Open(host, f.trigger, verb).Close(true, false)
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

	// The UPS asked, and the UPS now reads mains present: the reason is gone
	// before anything started. A plain stop here would halt with mains present
	// and no cut to cycle the machine back on.
	if f.trigger == audit.TriggerUPS && !e.PowerLoss && p.UPSStatus != "unknown" && p.UPSStatus != "" {
		fmt.Printf("sleep: UPS reports %q — mains are back, nothing to do\n", p.UPSStatus)
		return 0
	}

	// One sequence per host. The commit timer and the runtime floor can both
	// start one.
	unlock, ok := lockSleep(false)
	if !ok {
		fmt.Println("sleep: a sequence is already running on this host")
		return 0
	}
	defer unlock()

	// Why we are stopping decides whether power returning matters.
	//
	// A power-loss sleep is conditional: mains coming back means the reason is
	// gone, so the sequence abandons and everything it took down goes back.
	// An operator or a systemd reboot asked for this, and the UPS reporting
	// healthy power is simply not news — gating there would abandon a shutdown
	// that was requested, leaving guests running while the host stops under them.
	e.NoGate = !e.PowerLoss

	w := audit.Open(p.Host, f.trigger, audit.VerbSleep)
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

	defer lockUndo()()
	if unlock, ok := lockSleep(true); ok {
		defer unlock()
	}

	runs, _ := audit.LoadRuns(ctx)
	ep := audit.Episode(runs)
	if len(ep) == 0 {
		fmt.Println("wake: nothing to revive")
		return 0
	}

	last := ep[0]
	how := "graceful"
	if last.WasCut() {
		how = "cut"
	}
	fmt.Printf("wake: reviving from run %s (%s) — previous stop was %s\n", last.ID, last.Trigger, how)

	revived := 0
	for _, r := range ep {
		revived += undoRun(ctx, f, r)
	}
	recordUndo(f, audit.VerbWake)
	fmt.Printf("wake: undid %d action(s)\n", revived)
	return 0
}

// undoRun reverses the actions a run recorded, newest first, and only those it
// actually completed — a noout or a cordon an operator set by hand is not ours
// to clear.
func undoRun(ctx context.Context, f runFlags, run audit.Run) int {
	runner := exec.New()
	runner.Kubeconfig = resolveKubeconfig(f.kubeconfig)

	undone := 0
	for i := len(run.Steps) - 1; i >= 0; i-- {
		s := run.Steps[i]
		if s.Outcome != audit.OutcomeDone {
			continue
		}
		var undo func() error
		var what string
		switch s.Action {
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
