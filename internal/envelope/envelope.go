// Package envelope turns continuous column pressure/level measurements into a
// phase-aware operating envelope and a de-bounced restriction state machine.
//
// A single hard-limit comparison flips the control permit on every sample when
// a signal hovers near its threshold. This package replaces that comparison
// with three bands per signal:
//
//	hard limit ........ action is immediately denied and the plant is held
//	guard band ........ approaching the hard limit; actions are restricted
//	restore band ...... the signal must return below this lower edge and hold
//	                    there for a stable window before actions are released
//
// The gap between the guard edge and the restore edge is hysteresis, and the
// stable window requires N consecutive in-band samples spanning a minimum
// duration, so recovery is only granted after sustained stability rather than
// a single favourable sample.
package envelope

import (
	"math"

	"github.com/wyw14/solo-039/internal/model"
)

// Signal identifies which measured value crossed a band.
type Signal string

const (
	Pressure Signal = "pressure"
	Level    Signal = "level"
)

// Band classifies a single reading against the envelope.
type Band string

const (
	// BandNormal is inside the envelope, away from every edge.
	BandNormal Band = "normal"
	// BandGuard means the signal is in the hysteresis band: at or past the
	// guard edge but still below the hard limit.
	BandGuard Band = "guard"
	// BandHard means the signal is at or beyond the hard operating limit.
	BandHard Band = "hard"
)

// Thresholds describes the three bands for one signal on the high side plus
// the hard low limit. GuardHigh sits below HardHigh, and RestoreHigh sits below
// GuardHigh: a signal leaves the guard band only once it falls below
// RestoreHigh. A signal below HardLow is always a hard violation.
type Thresholds struct {
	HardLow     float64
	RestoreLow  float64
	GuardLow    float64
	GuardHigh   float64
	RestoreHigh float64
	HardHigh    float64
}

// Classify places a value into its band. NaN is treated as a hard violation so
// a failed instrument fails closed rather than "normal".
func (t Thresholds) Classify(value float64) Band {
	if math.IsNaN(value) {
		return BandHard
	}
	if value >= t.HardHigh || value <= t.HardLow {
		return BandHard
	}
	if value >= t.GuardHigh || value <= t.GuardLow {
		return BandGuard
	}
	return BandNormal
}

// Restored reports whether a value is inside the recovery band, i.e. past the
// inner restore edges and therefore eligible to count toward the stable
// window.
func (t Thresholds) Restored(value float64) bool {
	if math.IsNaN(value) {
		return false
	}
	return value > t.RestoreLow && value < t.RestoreHigh
}

// Envelope is the set of bands in force for one campaign phase.
type Envelope struct {
	Phase    model.Phase
	Pressure Thresholds
	Level    Thresholds
}

func (e Envelope) thresholds(signal Signal) Thresholds {
	if signal == Level {
		return e.Level
	}
	return e.Pressure
}

// margin returns how far a value is past a high edge, used to rank the most
// severe excursion. Negative values mean the value is below the edge.
func margin(edge, value float64) float64 { return value - edge }

// envelopesForPhase maps each campaign phase to the bands that are meaningful
// while that phase is active. Warming tolerates a lower pressure than the
// on-spec run; shut-down widens the low-side bands because the column is being
// de-pressurised and drained deliberately.
func envelopesForPhase(limits model.OperatingLimits) map[model.Phase]Envelope {
	pSpan := limits.MaxPressure - limits.MinPressure
	lSpan := limits.MaxLevel - limits.MinLevel

	makeE := func(phase model.Phase, pGuardLo, pGuardHi, pRestLo, pRestHi, lGuardLoFrac, lGuardHiFrac, lRestLoFrac, lRestHiFrac float64) Envelope {
		return Envelope{
			Phase: phase,
			Pressure: Thresholds{
				HardLow: limits.MinPressure, RestoreLow: limits.MinPressure + pSpan*pRestLo, GuardLow: limits.MinPressure + pSpan*pGuardLo,
				GuardHigh: limits.MinPressure + pSpan*pGuardHi, RestoreHigh: limits.MinPressure + pSpan*pRestHi, HardHigh: limits.MaxPressure,
			},
			Level: Thresholds{
				HardLow: limits.MinLevel, RestoreLow: limits.MinLevel + lSpan*lRestLoFrac, GuardLow: limits.MinLevel + lSpan*lGuardLoFrac,
				GuardHigh: limits.MinLevel + lSpan*lGuardHiFrac, RestoreHigh: limits.MinLevel + lSpan*lRestHiFrac, HardHigh: limits.MaxLevel,
			},
		}
	}

	// Fractions are given as: guard-low, guard-high, restore-low, restore-high,
	// measured inward from each hard edge. The restore edges always sit inside
	// the guard edges, which is what creates the hysteresis gap.
	return map[model.Phase]Envelope{
		model.Preparing:     makeE(model.Preparing, 0.18, 0.80, 0.30, 0.66, 0.16, 0.84, 0.28, 0.70),
		model.Warming:       makeE(model.Warming, 0.05, 0.85, 0.20, 0.70, 0.12, 0.82, 0.26, 0.68),
		model.Stabilizing:   makeE(model.Stabilizing, 0.15, 0.88, 0.28, 0.74, 0.14, 0.86, 0.26, 0.72),
		model.OnSpec:        makeE(model.OnSpec, 0.15, 0.90, 0.28, 0.76, 0.14, 0.88, 0.26, 0.74),
		model.Transitioning: makeE(model.Transitioning, 0.12, 0.88, 0.26, 0.72, 0.12, 0.84, 0.24, 0.70),
		model.Shutting:      makeE(model.Shutting, 0.02, 0.80, 0.12, 0.62, 0.04, 0.80, 0.16, 0.64),
		model.Stopped:       makeE(model.Stopped, 0.0, 0.60, 0.05, 0.45, 0.0, 0.60, 0.08, 0.45),
	}
}

// EnvelopeFor returns the configured envelope for a phase, falling back to the
// on-spec envelope if the phase is not explicitly configured.
func EnvelopeFor(limits model.OperatingLimits, phase model.Phase) Envelope {
	if e, ok := envelopesForPhase(limits)[phase]; ok {
		return e
	}
	return envelopesForPhase(limits)[model.OnSpec]
}
