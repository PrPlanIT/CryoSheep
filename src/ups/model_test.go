package ups

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC)

func TestSilenceOnMainsIsAMonitoringFault(t *testing.T) {
	var m Model
	m.Observe(Sample{At: t0, Status: "OL", Charge: 100, Runtime: 461 * time.Second})
	// A UPS does not evaporate at full charge; this must never justify action.
	if got := m.InterpretSilence(t0.Add(5*time.Minute), 30*time.Second); got != ReadingFault {
		t.Fatalf("reading = %q, want %q", got, ReadingFault)
	}
}

func TestSilenceAfterLowBatteryIsConsistentWithLoss(t *testing.T) {
	var m Model
	m.Observe(Sample{At: t0, Status: "OB LB", Charge: 6, Runtime: 20 * time.Second})
	if got := m.InterpretSilence(t0.Add(time.Minute), 30*time.Second); got != ReadingExpected {
		t.Fatalf("reading = %q, want %q", got, ReadingExpected)
	}
}

func TestSilenceOnBatteryWithRuntimeLeftIsNeither(t *testing.T) {
	var m Model
	m.Observe(Sample{At: t0, Status: "OB", Charge: 80, Runtime: 400 * time.Second})
	if got := m.InterpretSilence(t0.Add(40*time.Second), 30*time.Second); got != ReadingDegrading {
		t.Fatalf("reading = %q, want %q", got, ReadingDegrading)
	}
}

// Within the grace window contact has not lapsed at all.
func TestBriefGapIsNotSilence(t *testing.T) {
	var m Model
	m.Observe(Sample{At: t0, Status: "OB", Charge: 50, Runtime: 200 * time.Second})
	if got := m.InterpretSilence(t0.Add(10*time.Second), 30*time.Second); got != Reading("") {
		t.Fatalf("reading = %q, want empty (still current)", got)
	}
}

func TestNoDataWhenNeverObserved(t *testing.T) {
	var m Model
	if got := m.InterpretSilence(t0, time.Second); got != ReadingNoData {
		t.Fatalf("reading = %q, want %q", got, ReadingNoData)
	}
}

// Extrapolation must use the observed drain, not assume a flat second-per-second
// discharge: under heavier load runtime falls faster than the clock.
func TestDrainAndExtrapolation(t *testing.T) {
	var m Model
	m.Observe(Sample{At: t0, Status: "OB", Runtime: 400 * time.Second, Charge: Unknown})
	m.Observe(Sample{At: t0.Add(30 * time.Second), Status: "OB", Runtime: 340 * time.Second, Charge: Unknown})

	d, ok := m.Drain()
	if !ok || d < 1.9 || d > 2.1 {
		t.Fatalf("drain = %v (ok=%v), want ~2.0", d, ok)
	}
	// 60s past the last sample at 2x drain burns 120s of runtime.
	left, ok := m.RuntimeAt(t0.Add(90 * time.Second))
	if !ok {
		t.Fatal("no extrapolation")
	}
	if left < 215*time.Second || left > 225*time.Second {
		t.Fatalf("runtime left = %v, want ~220s", left)
	}
}

func TestExtrapolationFloorsAtZero(t *testing.T) {
	var m Model
	m.Observe(Sample{At: t0, Status: "OB", Runtime: 10 * time.Second, Charge: Unknown})
	left, ok := m.RuntimeAt(t0.Add(time.Hour))
	if !ok || left != 0 {
		t.Fatalf("runtime left = %v (ok=%v), want 0", left, ok)
	}
}

// "OB LB" is a set; equality against "OB" would read a dying UPS as healthy.
func TestStatusFlagsAreASet(t *testing.T) {
	s := Sample{Status: "OB LB"}
	if !s.OnBattery() || !s.LowBattery() || s.Online() {
		t.Fatalf("flags wrong for %q: OB=%v LB=%v OL=%v", s.Status, s.OnBattery(), s.LowBattery(), s.Online())
	}
}
