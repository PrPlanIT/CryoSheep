// Package ups keeps a small local model of UPS state so that losing contact can
// be interpreted rather than merely noticed.
//
// The design point: blindness is not a trigger. What silence means depends
// entirely on what preceded it — a UPS that was full and online does not
// evaporate, so silence there is a monitoring fault; a UPS that was on battery
// with little runtime left going quiet is consistent with death. The model
// decides how alarmed to be. It never decides to shut anything down: the armed
// hardware deadline is the failsafe, precisely because it keeps working when
// this model cannot.
package ups

import (
	"strings"
	"time"
)

// Unknown marks a field the driver did not report. The snmp-ups subdriver in use
// reports charge and runtime but not the low thresholds, so callers must cope
// with absence rather than assume zero.
const Unknown = -1

type Sample struct {
	At      time.Time
	Status  string        // raw ups.status, e.g. "OL" or "OB LB"
	Charge  float64       // percent, or Unknown
	Runtime time.Duration // remaining, or Unknown
}

func (s Sample) Has(flag string) bool {
	for _, f := range strings.Fields(s.Status) {
		if f == flag {
			return true
		}
	}
	return false
}

func (s Sample) OnBattery() bool  { return s.Has("OB") }
func (s Sample) Online() bool     { return s.Has("OL") }
func (s Sample) LowBattery() bool { return s.Has("LB") }

// Model holds the last two observations, which is all the history needed to see
// a direction of travel.
type Model struct {
	last Sample
	prev Sample
	seen int
}

func (m *Model) Observe(s Sample) {
	m.prev = m.last
	m.last = s
	m.seen++
}

func (m Model) Last() Sample { return m.last }
func (m Model) Seen() bool   { return m.seen > 0 }

// Drain returns how fast runtime is being consumed, as a ratio: 1.0 means one
// second of runtime lost per second elapsed, which is what a flat discharge
// looks like. Above 1.0 the load is heavier than when the estimate was made.
func (m Model) Drain() (float64, bool) {
	if m.seen < 2 || m.last.Runtime == Unknown || m.prev.Runtime == Unknown {
		return 0, false
	}
	elapsed := m.last.At.Sub(m.prev.At).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	lost := (m.prev.Runtime - m.last.Runtime).Seconds()
	return lost / elapsed, true
}

// RuntimeAt extrapolates remaining runtime forward from the last observation.
// Used only while blind; once contact returns the real value replaces it.
func (m Model) RuntimeAt(now time.Time) (time.Duration, bool) {
	if !m.Seen() || m.last.Runtime == Unknown {
		return 0, false
	}
	elapsed := now.Sub(m.last.At)
	if elapsed < 0 {
		elapsed = 0
	}
	rate := 1.0
	if d, ok := m.Drain(); ok && d > 0 {
		rate = d
	}
	left := m.last.Runtime - time.Duration(float64(elapsed)*rate)
	if left < 0 {
		left = 0
	}
	return left, true
}

// Reading is how silence should be understood.
type Reading string

const (
	// ReadingNoData — never heard from the UPS, so nothing can be inferred.
	ReadingNoData Reading = "no-data"
	// ReadingFault — last seen healthy on mains. A UPS does not disappear at
	// full charge, so this is a monitoring failure and must not cause action.
	ReadingFault Reading = "monitoring-fault"
	// ReadingExpected — last seen on battery and close to exhausted. Silence is
	// consistent with the UPS having ended; the armed deadline covers it.
	ReadingExpected Reading = "consistent-with-loss"
	// ReadingDegrading — on battery with runtime still in hand. Neither benign
	// nor terminal: keep the deadline armed and stay blind-safe.
	ReadingDegrading Reading = "on-battery-blind"
)

// InterpretSilence classifies a loss of contact.
//
// grace is how long contact may lapse before it is considered silence at all;
// below that the last sample is simply still current.
func (m Model) InterpretSilence(now time.Time, grace time.Duration) Reading {
	if !m.Seen() {
		return ReadingNoData
	}
	if now.Sub(m.last.At) < grace {
		return Reading("")
	}
	if m.last.Online() && !m.last.OnBattery() {
		return ReadingFault
	}
	if m.last.LowBattery() {
		return ReadingExpected
	}
	if left, ok := m.RuntimeAt(now); ok && left <= 0 {
		return ReadingExpected
	}
	return ReadingDegrading
}
