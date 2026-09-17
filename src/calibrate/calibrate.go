// Package calibrate derives the shutdown trigger from what shutdowns have
// actually cost, rather than from a number someone picked.
//
// The threshold is the one value in this whole design with the estate behind it,
// and guessing it is how a routine ends up half-finished when the battery dies.
// Every run journals its per-step timings, so the history answers the question
// directly — and because the input is observed runtime rather than nameplate, a
// battery that is ageing quietly tightens the recommendation instead of silently
// eroding the margin.
package calibrate

import (
	"fmt"
	"sort"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/audit"
)

// Run is one completed sequence, as reconstructed from the journal.
type Run struct {
	ID       string
	Host     string
	Started  time.Time
	Duration time.Duration
	Complete bool // false if the run was cut short — excluded from timing
}

type Summary struct {
	Host string
	N    int
	P50  time.Duration
	P95  time.Duration
	Max  time.Duration
}

// Summarize reports the distribution of completed runs. Incomplete runs are
// excluded: a sequence that was cut short tells you nothing about how long a
// sequence takes, and averaging it in would bias the estimate towards being
// dangerously short.
func Summarize(host string, runs []Run) Summary {
	var ds []time.Duration
	for _, r := range runs {
		if r.Host == host && r.Complete && r.Duration > 0 {
			ds = append(ds, r.Duration)
		}
	}
	s := Summary{Host: host, N: len(ds)}
	if len(ds) == 0 {
		return s
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	s.P50 = percentile(ds, 0.50)
	s.P95 = percentile(ds, 0.95)
	s.Max = ds[len(ds)-1]
	return s
}

// percentile uses nearest-rank: with few samples an interpolated value invents
// precision the data does not support, and every value here is a real shutdown.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Recommendation is a trigger threshold expressed in runtime remaining.
type Recommendation struct {
	RuntimeLow time.Duration // maps onto override.battery.runtime.low
	BasedOn    Summary
	Margin     float64
	Confident  bool // false when there is too little history to lean on
	Note       string
}

// MinRuns is the point below which a recommendation is a suggestion rather than
// a measurement. Two shutdowns do not describe a distribution.
const MinRuns = 3

// Recommend turns observed durations into a threshold.
//
// The basis is the worst observed run, not the median: the threshold has to hold
// on a bad day, and the median is the case that was already fine. Margin is then
// applied on top for the variance no sample set has seen yet.
func Recommend(s Summary, margin float64, fallback time.Duration) Recommendation {
	if margin <= 0 {
		margin = 0.5
	}
	r := Recommendation{BasedOn: s, Margin: margin}

	if s.N == 0 {
		r.RuntimeLow = fallback
		r.Note = "no completed runs; using the conservative fallback until a drill provides data"
		return r
	}
	r.RuntimeLow = time.Duration(float64(s.Max) * (1 + margin)).Round(time.Second)
	r.Confident = s.N >= MinRuns
	if !r.Confident {
		r.Note = fmt.Sprintf("only %d completed run(s); treat as provisional until %d", s.N, MinRuns)
	}
	if r.RuntimeLow < fallback {
		r.RuntimeLow = fallback
		r.Note = "measured value below the floor; using the floor"
	}
	return r
}

// Feasible reports whether the recommended threshold actually fits inside the
// runtime the UPS currently reports. A threshold larger than the battery means
// the sequence cannot finish in time at this load, which is a finding about the
// estate, not a tuning parameter.
func Feasible(r Recommendation, runtimeNow time.Duration) (bool, string) {
	if runtimeNow <= 0 {
		return false, "current runtime unknown"
	}
	if r.RuntimeLow >= runtimeNow {
		return false, fmt.Sprintf(
			"recommended trigger %s exceeds the %s currently available: the sequence cannot complete at this load",
			r.RuntimeLow, runtimeNow)
	}
	return true, ""
}

// FromAudit converts runs reconstructed from the journal into the shape
// calibration works on. Calibration itself stays pure — it is handed a slice and
// never learns where the slice came from.
func FromAudit(rs []audit.Run) []Run {
	out := make([]Run, 0, len(rs))
	for _, r := range rs {
		out = append(out, Run{
			ID: r.ID, Host: r.Host, Started: r.Started,
			Duration: r.Duration, Complete: r.Complete,
		})
	}
	return out
}
