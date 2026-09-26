// Package execute walks a plan.
//
// Three rules shape everything here, and each exists because the alternative
// fails badly during a power cut:
//
//  1. The sequence always reaches the halt. A step that errors is recorded and
//     the walk continues. A routine that gives up halfway leaves the host
//     running until the battery is flat, which is worse than any single step
//     failing.
//
//  2. Only a positive reading aborts. Mains observed back before the point of no
//     return abandons the sequence and reverses it. Being unable to read the UPS
//     changes nothing: blindness must never alter a decision, in either
//     direction. Stopping because we cannot see would leave the host exposed
//     exactly when it can least afford it.
//
//  3. Gates run before every step up to and including the first irreversible
//     one, then stop. The last gate is the most valuable — it is the final
//     chance to abandon before the host is committed. Re-checking after that
//     could only add a way to hang.
package execute

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/audit"
	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/plan"
	"github.com/PrPlanIT/CryoSheep/src/ups"
)

// Recorder is the subset of the journal the executor needs, so a walk can be
// tested without touching a disk.
type Recorder interface {
	StepStart(action, target, gate string) int
	StepEnd(i int, outcome string, err error)
	Note(i int, note string)
}

type nopRecorder struct{}

func (nopRecorder) StepStart(string, string, string) int { return 0 }
func (nopRecorder) StepEnd(int, string, error)           {}
func (nopRecorder) Note(int, string)                     {}

type Executor struct {
	Hyp      core.Hypervisor
	UPS      core.UPS
	Ceph     core.Ceph
	Kube     core.Kube
	Host     core.Host
	Deadline core.Deadline
	Record   Recorder

	// DryRun walks and records the decisions without performing any of them.
	DryRun bool

	// NoGate disables gating entirely, for a sequence whose decision is already
	// made: upsmon's SHUTDOWNCMD is terminal, and a normal reboot reads OL —
	// which would otherwise "abort" a shutdown systemd is performing anyway and
	// leave guests running while the host stops underneath them.
	NoGate bool

	// Clock is swappable so trend logic can be tested without waiting.
	Clock func() time.Time

	// GateTimeout bounds a UPS read. A gate that cannot answer promptly is as
	// useless as one that answers wrongly, and this runs against a battery.
	GateTimeout time.Duration

	// After is swappable so the barrier can be tested without spending the time
	// it exists to spend.
	After func(time.Duration) <-chan time.Time
}

type Result struct {
	Aborted   bool
	Completed bool
	Ran       int
	Reversed  int

	// Gate is the UPS status that caused an abort, so a caller can say why
	// rather than only that.
	Gate string

	// Held is how long the sequence waited at the barrier before committing, so
	// a run can be calibrated against what the wait actually cost.
	Held time.Duration
}

func (e *Executor) recorder() Recorder {
	if e.Record == nil {
		return nopRecorder{}
	}
	return e.Record
}

// Run walks the plan.
func (e *Executor) Run(ctx context.Context, p plan.Plan) Result {
	rec := e.recorder()
	pnr := p.PointOfNoReturn()

	var res Result
	var reversible []plan.Step // completed and undoable, for LIFO reversal
	var model ups.Model        // readings accumulate, so a trend is available

	started := e.now()
	held := false

	for _, wave := range waveIndices(p.Steps) {
		first := wave[0]

		// Gated once per wave rather than once per step. A wave is issued as a
		// set, so there is no moment between its members at which abandoning it
		// would be coherent: half a wave stopped is not a state the reversal
		// path describes.
		gate := ""
		if !e.NoGate && first <= pnr {
			status, back := e.mainsBack(ctx, &model)
			gate = status
			if back {
				res.Aborted = true
				res.Gate = status
				res.Reversed = e.reverse(ctx, reversible, rec, status)
				return res
			}
		}

		// The barrier. Held before the first step that cannot be undone, so
		// several hosts acting on one signal commit together rather than as each
		// becomes ready. Waiting costs battery; not waiting costs whichever host
		// was still writing to storage the early finishers took away.
		if !held && p.HoldUntil > 0 && first >= pnr {
			res.Held = e.hold(ctx, started, p.HoldUntil)
			held = true
		}

		for _, r := range e.runWave(ctx, p.Steps, wave, rec, gate) {
			res.Ran++
			if r.err == nil && p.Steps[r.i].Reversible {
				reversible = append(reversible, p.Steps[r.i])
			}
		}
	}

	res.Completed = true
	return res
}

// waveIndices groups consecutive steps sharing a wave.
//
// Consecutive because a plan lists its steps in order and waves ascend; a wave
// that appeared twice with something between would mean the plan had already
// contradicted itself, and running it as two groups is the honest reading.
func waveIndices(steps []plan.Step) [][]int {
	var out [][]int
	for i := 0; i < len(steps); {
		j := i + 1
		for j < len(steps) && steps[j].Wave == steps[i].Wave {
			j++
		}
		g := make([]int, 0, j-i)
		for k := i; k < j; k++ {
			g = append(g, k)
		}
		out = append(out, g)
		i = j
	}
	return out
}

// hold waits until d has elapsed since the sequence began, and reports how long
// it actually waited so the run can be calibrated.
func (e *Executor) hold(ctx context.Context, started time.Time, d time.Duration) time.Duration {
	wait := d - e.now().Sub(started)
	if wait <= 0 {
		return 0
	}
	after := e.After
	if after == nil {
		after = time.After
	}
	select {
	case <-ctx.Done():
	case <-after(wait):
	}
	return wait
}

type waveResult struct {
	i       int
	outcome string
	err     error
}

// runWave issues a wave and waits for all of it.
//
// A wave of one is performed inline, so the overwhelmingly common case keeps
// exactly the behaviour it had before waves existed — same call order, same
// recorder sequence, nothing to go wrong differently.
func (e *Executor) runWave(ctx context.Context, steps []plan.Step, idxs []int, rec Recorder, gate string) []waveResult {
	if len(idxs) == 1 {
		i := idxs[0]
		id := rec.StepStart(string(steps[i].Action), steps[i].Target, gate)
		outcome, err := e.performAt(e.stepCtx(ctx, steps[i]), steps[i], rec, id)
		rec.StepEnd(id, outcome, err)
		return []waveResult{{i: i, outcome: outcome, err: err}}
	}

	// The journal is written from several goroutines here, so it is serialised.
	// The alternative — a recorder that each adapter has to make safe — puts the
	// obligation in the wrong place and would be discovered by a corrupted run.
	safe := &lockedRecorder{inner: rec}

	ids := make([]int, len(idxs))
	for k, i := range idxs {
		ids[k] = safe.StepStart(string(steps[i].Action), steps[i].Target, gate)
	}

	out := make([]waveResult, len(idxs))
	var wg sync.WaitGroup
	for k, i := range idxs {
		wg.Add(1)
		go func(k, i int) {
			defer wg.Done()
			outcome, err := e.performAt(e.stepCtx(ctx, steps[i]), steps[i], safe, ids[k])
			out[k] = waveResult{i: i, outcome: outcome, err: err}
		}(k, i)
	}
	wg.Wait()

	// Ends are recorded in step order, not completion order, so the journal reads
	// the way the plan does.
	for k := range out {
		safe.StepEnd(ids[k], out[k].outcome, out[k].err)
	}
	return out
}

// stepCtx bounds a guest shutdown by its own budget.
//
// This is what makes one hung guest cost only itself. Passing the timeout to the
// adapter and hoping asks every adapter to be careful; bounding the context
// makes it structural, so a guest that never returns cannot hold its wave-mates
// — or, in a serial plan, everything behind it.
//
// Only guest stops are bounded here. ActionUPSDeadline carries the deadline to
// arm in the same field, which is a value rather than a budget for the call.
func (e *Executor) stepCtx(ctx context.Context, s plan.Step) context.Context {
	if s.Action != plan.ActionGuestStop || s.Timeout <= 0 {
		return ctx
	}
	c, cancel := context.WithTimeout(ctx, s.Timeout)
	// The cancel is deliberately not deferred to the caller: the context is used
	// only for the duration of the step, and letting it expire is the point.
	_ = cancel
	return c
}

// lockedRecorder serialises journal writes for a concurrent wave.
type lockedRecorder struct {
	mu    sync.Mutex
	inner Recorder
}

func (l *lockedRecorder) StepStart(action, target, gate string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.StepStart(action, target, gate)
}

func (l *lockedRecorder) StepEnd(i int, outcome string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inner.StepEnd(i, outcome, err)
}

func (l *lockedRecorder) Note(i int, note string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inner.Note(i, note)
}

// mainsBack reports whether power has positively returned.
//
// Each reading is fed to the model, so the decision can use a trend rather than
// a single flag: a transfer can flap OL/OB, and what actually proves recovery is
// the battery no longer draining with input power present.
//
// Unreadable means "no information", which is not the same as "mains are back"
// and must not be treated as it. The armed hardware deadline covers the blind
// case; this reacts only to something it can actually see.
func (e *Executor) mainsBack(ctx context.Context, model *ups.Model) (string, bool) {
	if e.UPS == nil {
		return "", false
	}
	timeout := e.GateTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	gctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	r, err := e.UPS.Read(gctx)
	if err != nil {
		return "unreadable", false
	}
	model.Observe(ups.Sample{
		At: e.now(), Status: r.Status, Charge: r.Charge, Runtime: r.Runtime, InputV: r.InputV,
	})
	return r.Status, model.MainsBack()
}

func (e *Executor) now() time.Time {
	if e.Clock != nil {
		return e.Clock()
	}
	return time.Now()
}

// performAt runs a step, letting it attach evidence to its own journal entry.
func (e *Executor) performAt(ctx context.Context, s plan.Step, rec Recorder, idx int) (string, error) {
	if s.Action == plan.ActionK8sRecord {
		if e.Kube == nil {
			return audit.OutcomeSkipped, nil
		}
		pods, err := e.Kube.StatefulPods(ctx, s.Target)
		if err != nil {
			return audit.OutcomeFailed, err
		}
		rec.Note(idx, describePods(pods))
		return audit.OutcomeDone, nil
	}
	return e.perform(ctx, s)
}

// describePods renders the record compactly, one workload per line.
//
// Workloads that claim a role are written first. They are the only lines that
// answer the question the record exists for — which instance was authoritative
// — and the budget is finite, so they must not be crowded out by workloads that
// merely happened to be here. The first drill did exactly that: thirty lines, of
// which nine carried a role, and the cut landed mid-way through a CNPG replica.
//
// What does not fit is counted rather than dropped in silence. A record that
// says it omitted seven workloads sends a reader to the cluster; one that simply
// ends looks complete and is not.
func describePods(pods []core.StatefulPod) string {
	if len(pods) == 0 {
		return "no stateful workloads on this node"
	}

	// A quorum member is a stateful workload that claims a role. A DaemonSet pod
	// labelled role=worker matches the same vocabulary and is not one, so being
	// stateful ranks ahead of merely carrying the word.
	rank := func(p core.StatefulPod) int {
		switch {
		case p.Owner != "" && p.Role != "":
			return 0
		case p.Owner != "":
			return 1
		default:
			return 2
		}
	}
	ordered := make([]core.StatefulPod, len(pods))
	copy(ordered, pods)
	sort.SliceStable(ordered, func(i, j int) bool {
		return rank(ordered[i]) < rank(ordered[j])
	})

	line := func(p core.StatefulPod) string {
		role := p.Role
		if role == "" {
			role = "-"
		}
		s := role + " " + p.Namespace + "/" + p.Name
		if p.Owner != "" {
			s += " (" + p.Owner + ")"
		}
		if p.Priority != "" {
			s += " prio=" + p.Priority
		}
		return s
	}

	// Leave room for the omission notice, so recording that something was
	// dropped can never itself be the thing that gets dropped.
	const reserve = 40
	var b strings.Builder
	for i, p := range ordered {
		l := line(p)
		if b.Len()+len(l)+1 > audit.MaxNote-reserve {
			fmt.Fprintf(&b, "\n+%d more not recorded", len(ordered)-i)
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return b.String()
}

func (e *Executor) perform(ctx context.Context, s plan.Step) (string, error) {
	if e.DryRun {
		return audit.OutcomeSkipped, nil
	}
	switch s.Action {
	case plan.ActionUPSDeadline:
		if e.Deadline == nil {
			return audit.OutcomeSkipped, nil
		}
		if err := e.Deadline.Arm(ctx, s.Timeout); err != nil {
			return audit.OutcomeFailed, err
		}
	case plan.ActionCephNoout:
		if e.Ceph == nil {
			return audit.OutcomeSkipped, nil
		}
		if err := e.Ceph.SetNoout(ctx); err != nil {
			return audit.OutcomeFailed, err
		}
	case plan.ActionK8sCordon:
		if e.Kube == nil {
			return audit.OutcomeSkipped, nil
		}
		if err := e.Kube.Cordon(ctx, s.Target); err != nil {
			return audit.OutcomeFailed, err
		}
	case plan.ActionK8sUnmount:
		if e.Kube == nil {
			return audit.OutcomeSkipped, nil
		}
		if _, err := e.Kube.UnmountCSI(ctx); err != nil {
			return audit.OutcomeFailed, err
		}
	case plan.ActionGuestStop:
		if e.Hyp == nil {
			return audit.OutcomeSkipped, nil
		}
		if err := e.Hyp.Shutdown(ctx, s.Target, s.Timeout); err != nil {
			return audit.OutcomeFailed, err
		}
	case plan.ActionHostHalt:
		if e.Host == nil {
			return audit.OutcomeSkipped, nil
		}
		if err := e.Host.Poweroff(ctx); err != nil {
			return audit.OutcomeFailed, err
		}
	default:
		return audit.OutcomeSkipped, nil
	}
	return audit.OutcomeDone, nil
}

// reverse undoes completed reversible steps, newest first, and only steps this
// run actually performed — a noout an operator set for maintenance is not ours
// to clear.
func (e *Executor) reverse(ctx context.Context, done []plan.Step, rec Recorder, gate string) int {
	// The armed UPS deadline is cancelled before anything else. It is a timer
	// the hardware already holds: leaving it standing while guests are restarted
	// would cut power to a recovered estate a few minutes later, which is the
	// outage this abort exists to prevent.
	n := 0
	for _, s := range done {
		if s.Action == plan.ActionUPSDeadline && e.Deadline != nil {
			idx := rec.StepStart(string(s.Action)+".undo", "", gate)
			var err error
			if !e.DryRun {
				err = e.Deadline.Cancel(ctx)
			}
			if err != nil {
				rec.StepEnd(idx, audit.OutcomeFailed, err)
			} else {
				rec.StepEnd(idx, audit.OutcomeDone, nil)
				n++
			}
			break
		}
	}

	for i := len(done) - 1; i >= 0; i-- {
		s := done[i]
		if s.Action == plan.ActionUPSDeadline {
			continue // already cancelled above
		}
		if s.Action == plan.ActionK8sCordon {
			if e.Kube == nil {
				continue
			}
			idx := rec.StepStart(string(s.Action)+".undo", s.Target, gate)
			var err error
			if !e.DryRun {
				err = e.Kube.Uncordon(ctx, s.Target)
			}
			if err != nil {
				rec.StepEnd(idx, audit.OutcomeFailed, err)
				continue
			}
			rec.StepEnd(idx, audit.OutcomeDone, nil)
			n++
			continue
		}
		if s.Action == plan.ActionGuestStop {
			if e.Hyp == nil {
				continue
			}
			idx := rec.StepStart(string(s.Action)+".undo", s.Target, gate)
			var err error
			if !e.DryRun {
				err = e.Hyp.Start(ctx, s.Target)
			}
			if err != nil {
				rec.StepEnd(idx, audit.OutcomeFailed, err)
				continue
			}
			rec.StepEnd(idx, audit.OutcomeDone, nil)
			n++
			continue
		}
		undo := e.undoFor(s.Action)
		if undo == nil {
			continue
		}
		idx := rec.StepStart(string(s.Action)+".undo", s.Target, gate)
		var err error
		if !e.DryRun {
			err = undo(ctx)
		}
		if err != nil {
			rec.StepEnd(idx, audit.OutcomeFailed, err)
			continue
		}
		rec.StepEnd(idx, audit.OutcomeDone, nil)
		n++
	}
	return n
}

// undoFor returns the inverse of an action, or nil where there is none.
func (e *Executor) undoFor(a plan.Action) func(context.Context) error {
	switch a {
	case plan.ActionGuestStop:
		return nil // handled per-target in reverse, which knows which guest
	case plan.ActionUPSDeadline:
		if e.Deadline == nil {
			return nil
		}
		return e.Deadline.Cancel
	case plan.ActionCephNoout:
		if e.Ceph == nil {
			return nil
		}
		return e.Ceph.UnsetNoout
	case plan.ActionK8sRecord:
		return nil // reading state changes nothing, so there is nothing to undo
	}
	return nil
}
