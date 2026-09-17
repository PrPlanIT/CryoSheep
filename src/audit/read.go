package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Identifier is how journald tags our lines. It is the executable name, which
// systemd uses by default, so a run started by hand and a run started by a unit
// land under the same tag and read back the same way.
const Identifier = "cryosheep"

// maxLines bounds how far back a read goes. A run is about a dozen lines, so
// this is generous for the twenty-odd runs calibration wants while keeping a
// machine with years of history from being read in full.
const maxLines = "4000"

// Step is one step of a past run, reconstructed from the lines it wrote.
type Step struct {
	Action  string
	Target  string
	Gate    string
	Outcome string
	Elapsed time.Duration
	Err     string
	Note    string

	// Started is when the step began. Present even for a step that never
	// finished, which is what a machine cut mid-sequence leaves behind.
	Started time.Time

	// Finished is false when a step.start was written and no step.end followed.
	Finished bool
}

// Run is one past sequence.
type Run struct {
	ID       string
	Host     string
	Trigger  string
	Started  time.Time
	Duration time.Duration
	Complete bool
	Aborted  bool
	Steps    []Step
}

// LoadRuns reconstructs past runs from the journal, newest first.
//
// This reads back what was written rather than keeping a second copy. It spans
// boots deliberately: the run that matters after a power cut is the one from the
// boot before this one, and reversal has to find it.
func LoadRuns(ctx context.Context) ([]Run, error) {
	out, err := exec.CommandContext(ctx, "journalctl",
		"-t", Identifier, "-o", "cat", "--no-pager", "-n", maxLines).Output()
	if err != nil {
		return nil, err
	}
	return parseRuns(string(out)), nil
}

// parseRuns is split out so the reconstruction can be tested without journald.
func parseRuns(text string) []Run {
	byID := map[string]*Run{}
	var order []string

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(raw, "{") {
			continue // not ours; journald carries other things under this tag
		}
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil || l.Run == "" {
			continue
		}
		r, ok := byID[l.Run]
		if !ok {
			r = &Run{ID: l.Run, Host: l.Host, Trigger: l.Trigger}
			byID[l.Run] = r
			order = append(order, l.Run)
		}
		when, _ := time.Parse(time.RFC3339Nano, l.Time)

		switch l.Event {
		case evRunStart:
			r.Started = when
		case evStepStart:
			for len(r.Steps) <= l.Step {
				r.Steps = append(r.Steps, Step{})
			}
			r.Steps[l.Step] = Step{
				Action: l.Action, Target: l.Target, Gate: l.Gate, Started: when,
			}
		case evStepEnd:
			if l.Step < 0 || l.Step >= len(r.Steps) {
				continue
			}
			s := &r.Steps[l.Step]
			if l.Note != "" {
				s.Note = l.Note
				continue // a note is not the end of the step
			}
			s.Outcome, s.Err = l.Outcome, l.Err
			s.Elapsed = time.Duration(l.ElapsedMS) * time.Millisecond
			s.Finished = true
		case evRunEnd:
			r.Duration = time.Duration(l.DurationMS) * time.Millisecond
			if l.Complete != nil {
				r.Complete = *l.Complete
			}
			if l.Aborted != nil {
				r.Aborted = *l.Aborted
			}
		}
	}

	runs := make([]Run, 0, len(order))
	for _, id := range order {
		runs = append(runs, *byID[id])
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Started.After(runs[j].Started) })
	return runs
}

// WasCut reports whether a run stopped without finishing — a step that started
// and never ended, or a sequence that never wrote its closing line. This is how
// a host learns, on the way back up, that its previous stop was not orderly.
func (r Run) WasCut() bool {
	if r.Complete || r.Aborted {
		return false
	}
	for _, s := range r.Steps {
		if !s.Finished {
			return true
		}
	}
	return r.Duration == 0
}
