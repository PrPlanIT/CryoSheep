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
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
	"github.com/PrPlanIT/CryoSheep/src/journal"
	"github.com/PrPlanIT/CryoSheep/src/plan"
	"github.com/PrPlanIT/CryoSheep/src/ups"
)

// Recorder is the subset of the journal the executor needs, so a walk can be
// tested without touching a disk.
type Recorder interface {
	StepStart(action, target, gate string) int
	StepEnd(i int, outcome string, err error)
}

type nopRecorder struct{}

func (nopRecorder) StepStart(string, string, string) int { return 0 }
func (nopRecorder) StepEnd(int, string, error)           {}

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
}

type Result struct {
	Aborted   bool
	Completed bool
	Ran       int
	Reversed  int

	// Gate is the UPS status that caused an abort, so a caller can say why
	// rather than only that.
	Gate string
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

	for i, step := range p.Steps {
		gate := ""
		if !e.NoGate && i <= pnr {
			status, back := e.mainsBack(ctx, &model)
			gate = status
			if back {
				res.Aborted = true
				res.Gate = status
				res.Reversed = e.reverse(ctx, reversible, rec, status)
				return res
			}
		}

		idx := rec.StepStart(string(step.Action), step.Target, gate)
		outcome, err := e.perform(ctx, step)
		rec.StepEnd(idx, outcome, err)
		res.Ran++

		if err == nil && step.Reversible {
			reversible = append(reversible, step)
		}
	}

	res.Completed = true
	return res
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

func (e *Executor) perform(ctx context.Context, s plan.Step) (string, error) {
	if e.DryRun {
		return journal.OutcomeSkipped, nil
	}
	switch s.Action {
	case plan.ActionUPSDeadline:
		if e.Deadline == nil {
			return journal.OutcomeSkipped, nil
		}
		if err := e.Deadline.Arm(ctx, s.Timeout); err != nil {
			return journal.OutcomeFailed, err
		}
	case plan.ActionCephNoout:
		if e.Ceph == nil {
			return journal.OutcomeSkipped, nil
		}
		if err := e.Ceph.SetNoout(ctx); err != nil {
			return journal.OutcomeFailed, err
		}
	case plan.ActionK8sCordon:
		if e.Kube == nil {
			return journal.OutcomeSkipped, nil
		}
		if err := e.Kube.Cordon(ctx, s.Target); err != nil {
			return journal.OutcomeFailed, err
		}
	case plan.ActionK8sDrain:
		if e.Kube == nil {
			return journal.OutcomeSkipped, nil
		}
		// A pod that will not evict must not hold the sequence open. The node is
		// going down either way; the bound decides whether it goes tidily.
		if err := e.Kube.Drain(ctx, s.Target, s.Timeout); err != nil {
			return journal.OutcomeForced, err
		}
	case plan.ActionK8sUnmount:
		if e.Kube == nil {
			return journal.OutcomeSkipped, nil
		}
		if _, err := e.Kube.UnmountCSI(ctx); err != nil {
			return journal.OutcomeFailed, err
		}
	case plan.ActionGuestStop:
		if e.Hyp == nil {
			return journal.OutcomeSkipped, nil
		}
		if err := e.Hyp.Shutdown(ctx, s.Target, s.Timeout); err != nil {
			return journal.OutcomeFailed, err
		}
	case plan.ActionHostHalt:
		if e.Host == nil {
			return journal.OutcomeSkipped, nil
		}
		if err := e.Host.Poweroff(ctx); err != nil {
			return journal.OutcomeFailed, err
		}
	default:
		return journal.OutcomeSkipped, nil
	}
	return journal.OutcomeDone, nil
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
				rec.StepEnd(idx, journal.OutcomeFailed, err)
			} else {
				rec.StepEnd(idx, journal.OutcomeDone, nil)
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
				rec.StepEnd(idx, journal.OutcomeFailed, err)
				continue
			}
			rec.StepEnd(idx, journal.OutcomeDone, nil)
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
				rec.StepEnd(idx, journal.OutcomeFailed, err)
				continue
			}
			rec.StepEnd(idx, journal.OutcomeDone, nil)
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
			rec.StepEnd(idx, journal.OutcomeFailed, err)
			continue
		}
		rec.StepEnd(idx, journal.OutcomeDone, nil)
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
	case plan.ActionK8sDrain:
		return nil // uncordoning is what brings workloads back; draining has no separate undo
	}
	return nil
}
