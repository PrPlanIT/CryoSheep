package calibrate

import (
	"testing"
	"time"
)

func run(host string, d time.Duration, complete bool) Run {
	return Run{Host: host, Duration: d, Complete: complete}
}

func TestSummarizeIgnoresOtherHostsAndIncompleteRuns(t *testing.T) {
	runs := []Run{
		run("eggplant", 80*time.Second, true),
		run("eggplant", 142*time.Second, true),
		run("eggplant", 9*time.Second, false), // cut short — says nothing about cost
		run("bamboo", 500*time.Second, true),  // different host
	}
	s := Summarize("eggplant", runs)
	if s.N != 2 {
		t.Fatalf("N = %d, want 2", s.N)
	}
	if s.Max != 142*time.Second {
		t.Fatalf("Max = %v, want 142s", s.Max)
	}
}

// An aborted run is short by definition; including it would drag the estimate
// down and produce a threshold that does not leave enough time.
func TestIncompleteRunsCannotShortenTheEstimate(t *testing.T) {
	withAborts := Summarize("h", []Run{
		run("h", 140*time.Second, true),
		run("h", 2*time.Second, false),
		run("h", 3*time.Second, false),
	})
	if withAborts.Max != 140*time.Second || withAborts.N != 1 {
		t.Fatalf("aborted runs leaked into the summary: %+v", withAborts)
	}
}

func TestRecommendBuildsOnTheWorstRunNotTheMedian(t *testing.T) {
	s := Summarize("h", []Run{
		run("h", 60*time.Second, true),
		run("h", 70*time.Second, true),
		run("h", 140*time.Second, true),
	})
	r := Recommend(s, 0.5, 30*time.Second)
	if r.RuntimeLow != 210*time.Second {
		t.Fatalf("RuntimeLow = %v, want 210s (worst 140s + 50%%)", r.RuntimeLow)
	}
	if !r.Confident {
		t.Fatalf("3 runs should be confident")
	}
}

func TestTooFewRunsIsProvisional(t *testing.T) {
	s := Summarize("h", []Run{run("h", 100*time.Second, true)})
	r := Recommend(s, 0.5, 30*time.Second)
	if r.Confident {
		t.Fatal("one run must not be reported as confident")
	}
	if r.Note == "" {
		t.Fatal("expected a note explaining why")
	}
}

func TestNoHistoryFallsBackConservatively(t *testing.T) {
	r := Recommend(Summarize("h", nil), 0.5, 240*time.Second)
	if r.RuntimeLow != 240*time.Second || r.Confident {
		t.Fatalf("got %+v, want the fallback and not confident", r)
	}
}

// The important failure: a threshold bigger than the battery is a finding about
// the estate, not a number to tune.
func TestInfeasibleWhenThresholdExceedsRuntime(t *testing.T) {
	s := Summarize("h", []Run{
		run("h", 400*time.Second, true),
		run("h", 410*time.Second, true),
		run("h", 420*time.Second, true),
	})
	r := Recommend(s, 0.5, 30*time.Second) // 630s
	ok, why := Feasible(r, 461*time.Second)
	if ok {
		t.Fatal("630s trigger against 461s of battery must not be feasible")
	}
	if why == "" {
		t.Fatal("expected an explanation")
	}
}

func TestFeasibleWithHeadroom(t *testing.T) {
	s := Summarize("h", []Run{
		run("h", 80*time.Second, true),
		run("h", 90*time.Second, true),
		run("h", 142*time.Second, true),
	})
	if ok, why := Feasible(Recommend(s, 0.5, 30*time.Second), 461*time.Second); !ok {
		t.Fatalf("expected feasible, got %q", why)
	}
}

func TestPercentileNearestRank(t *testing.T) {
	ds := []time.Duration{10, 20, 30, 40}
	if got := percentile(ds, 0.5); got != 20 {
		t.Fatalf("p50 = %v, want 20", got)
	}
	if got := percentile(ds, 0.95); got != 40 {
		t.Fatalf("p95 = %v, want 40", got)
	}
}
