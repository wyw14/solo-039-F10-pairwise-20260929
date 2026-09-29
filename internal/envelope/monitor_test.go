package envelope

import (
	"math"
	"testing"

	"github.com/wyw14/solo-039/internal/model"
)

func newTestMonitor() *Monitor {
	return NewMonitor(model.DefaultLimits(), DefaultConfig())
}

// mid returns a safe mid-range value and the envelope for a phase.
func envFor(t *testing.T, phase model.Phase) (Envelope, float64, float64) {
	t.Helper()
	env := EnvelopeFor(model.DefaultLimits(), phase)
	pMid := (env.Pressure.RestoreLow + env.Pressure.RestoreHigh) / 2
	lMid := (env.Level.RestoreLow + env.Level.RestoreHigh) / 2
	return env, pMid, lMid
}

func guardHighValue(th Thresholds) float64 { return th.GuardHigh + (th.HardHigh-th.GuardHigh)/2 }
func hardHighValue(th Thresholds) float64  { return th.HardHigh + 0.01 }
func gapValue(th Thresholds) float64       { return (th.GuardHigh + th.RestoreHigh) / 2 }
func restoreValue(th Thresholds) float64 {
	return (th.RestoreLow + th.RestoreHigh) / 2
}

func feed(t *testing.T, m *Monitor, phase model.Phase, pressure, level float64, at int64) State {
	t.Helper()
	_, ok := m.Update(phase, pressure, level, at)
	if !ok {
		t.Fatalf("update at %d rejected", at)
	}
	return m.StateValue()
}

func TestEnvelopeEdgesAreOrdered(t *testing.T) {
	for _, phase := range model.Phases() {
		env := EnvelopeFor(model.DefaultLimits(), phase)
		for name, th := range map[string]Thresholds{"pressure": env.Pressure, "level": env.Level} {
			if !(th.HardLow <= th.GuardLow && th.GuardLow < th.RestoreLow &&
				th.RestoreLow < th.RestoreHigh && th.RestoreHigh < th.GuardHigh && th.GuardHigh <= th.HardHigh) {
				t.Fatalf("%s/%s restore edges must sit inside guard edges: %+v", phase, name, th)
			}
		}
	}
}

func TestNormalAllowsEnergy(t *testing.T) {
	m := newTestMonitor()
	_, p, l := envFor(t, model.OnSpec)
	feed(t, m, model.OnSpec, p, l, 1)
	permit := m.Permit(ActionEnergy)
	if !permit.Allowed || permit.State != StateNormal {
		t.Fatalf("energy should be allowed when normal, got %+v", permit)
	}
}

// The core defect: a single sample kissing the guard band must not flip the
// permit, and guard/normal chatter must never confirm a restriction.
func TestGuardChatterDoesNotFlipPermit(t *testing.T) {
	m := newTestMonitor()
	env, p, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)

	feed(t, m, model.OnSpec, p, l, 1) // normal
	if got := feed(t, m, model.OnSpec, high, l, 2); got != StateNormal {
		t.Fatalf("one guard sample must not trip, got %s", got)
	}
	// Oscillate guard/normal; the consecutive counter keeps resetting.
	for at := int64(3); at < 20; at++ {
		v := p
		if at%2 == 0 {
			v = high
		}
		if got := feed(t, m, model.OnSpec, v, l, at); got != StateNormal {
			t.Fatalf("chatter at %d tripped state %s", at, got)
		}
	}
	if !m.Permit(ActionEnergy).Allowed {
		t.Fatal("energy permit flipped under chatter")
	}
}

func TestConsecutiveGuardEntersLimiting(t *testing.T) {
	m := newTestMonitor()
	env, _, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)

	feed(t, m, model.OnSpec, high, l, 1)
	if got := feed(t, m, model.OnSpec, high, l, 2); got != StateLimiting {
		t.Fatalf("expected limiting after two guard samples, got %s", got)
	}
	energy := m.Permit(ActionEnergy)
	if energy.Allowed {
		t.Fatal("energy must be restricted while limiting")
	}
	if !m.Permit(ActionSafe).Allowed {
		t.Fatal("protective action must remain available while limiting")
	}
}

func TestHardCrossingImmediatelyHolds(t *testing.T) {
	m := newTestMonitor()
	env, p, l := envFor(t, model.OnSpec)
	hard := hardHighValue(env.Pressure)
	if got := feed(t, m, model.OnSpec, hard, l, 1); got != StateHolding {
		t.Fatalf("expected holding on hard crossing, got %s", got)
	}
	if m.Permit(ActionEnergy).Allowed {
		t.Fatal("energy must be denied while holding")
	}

	// NaN instrument reading fails closed as a hard violation.
	m2 := newTestMonitor()
	if got := feed(t, m2, model.OnSpec, math.NaN(), l, 1); got != StateHolding {
		t.Fatalf("NaN should hold, got %s", got)
	}
	_ = p
}

// Once limiting, falling into the gap between restore and guard edges must not
// release the action — that is the hysteresis that kills boundary flapping.
func TestHysteresisGapKeepsRestriction(t *testing.T) {
	m := newTestMonitor()
	env, p, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)
	gap := gapValue(env.Pressure)

	feed(t, m, model.OnSpec, high, l, 1)
	feed(t, m, model.OnSpec, high, l, 2) // limiting
	if got := feed(t, m, model.OnSpec, gap, l, 3); got != StateLimiting {
		t.Fatalf("value in hysteresis gap must stay limiting, got %s", got)
	}
	if got := feed(t, m, model.OnSpec, gap, l, 4); got != StateLimiting {
		t.Fatalf("gap must not start recovery, got %s", got)
	}
	_ = p
}

func TestRecoveryNeedsFullStableWindow(t *testing.T) {
	m := newTestMonitor()
	env, _, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)
	restored := restoreValue(env.Pressure)

	feed(t, m, model.OnSpec, high, l, 1)
	feed(t, m, model.OnSpec, high, l, 2) // limiting, window starts at t=3

	for at := int64(3); at <= 5; at++ {
		if got := feed(t, m, model.OnSpec, restored, l, at); got != StateRecovering {
			t.Fatalf("at %d expected recovering, got %s", at, got)
		}
	}
	win := m.Status().Window
	if win.Samples != 3 || win.NeededSamples != 4 {
		t.Fatalf("window progress wrong: %+v", win)
	}
	// Fourth consecutive in-band sample also satisfies the 3s span.
	if got := feed(t, m, model.OnSpec, restored, l, 6); got != StateNormal {
		t.Fatalf("expected normal once stable window elapsed, got %s", got)
	}
	if len(m.Status().Restrictions) != 0 {
		t.Fatal("restrictions must clear after recovery")
	}
}

func TestRecoveryTransitionNamesReleasedSignals(t *testing.T) {
	m := newTestMonitor()
	env, _, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)
	restored := restoreValue(env.Pressure)

	m.Update(model.OnSpec, high, l, 1)
	m.Update(model.OnSpec, high, l, 2)
	m.Update(model.OnSpec, restored, l, 3)
	m.Update(model.OnSpec, restored, l, 4)
	m.Update(model.OnSpec, restored, l, 5)
	changed, ok := m.Update(model.OnSpec, restored, l, 6)
	if !ok {
		t.Fatal("final update rejected")
	}
	if changed.From != StateRecovering || changed.To != StateNormal {
		t.Fatalf("want recovering->normal, got %s->%s", changed.From, changed.To)
	}
	if len(changed.Signals) != 1 || changed.Signals[0] != Pressure {
		t.Fatalf("recovery event must name the released pressure signal, got %v", changed.Signals)
	}
}

func TestRecoveryWindowResetsOnDip(t *testing.T) {
	m := newTestMonitor()
	env, p, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)
	restored := restoreValue(env.Pressure)

	feed(t, m, model.OnSpec, high, l, 1)
	feed(t, m, model.OnSpec, high, l, 2)
	feed(t, m, model.OnSpec, restored, l, 3)
	feed(t, m, model.OnSpec, restored, l, 4) // 2 stable samples
	if got := feed(t, m, model.OnSpec, high, l, 5); got != StateLimiting {
		t.Fatalf("dip back into guard band must leave limiting, got %s", got)
	}
	if m.Status().Window.Samples != 0 {
		t.Fatal("stable window must reset on dip")
	}
	// Window restarts from scratch; three restored samples are not enough.
	feed(t, m, model.OnSpec, restored, l, 6)
	feed(t, m, model.OnSpec, restored, l, 7)
	if got := feed(t, m, model.OnSpec, restored, l, 8); got != StateRecovering {
		t.Fatalf("expected recovering after restart, got %s", got)
	}
	_ = p
}

func TestHardWhileRecoveringGoesHolding(t *testing.T) {
	m := newTestMonitor()
	env, _, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)
	hard := hardHighValue(env.Pressure)
	restored := restoreValue(env.Pressure)

	feed(t, m, model.OnSpec, high, l, 1)
	feed(t, m, model.OnSpec, high, l, 2)
	feed(t, m, model.OnSpec, restored, l, 3)
	if got := feed(t, m, model.OnSpec, hard, l, 4); got != StateHolding {
		t.Fatalf("hard crossing during recovery must hold, got %s", got)
	}
}

func TestBothSignalsMustRestore(t *testing.T) {
	m := newTestMonitor()
	env, _, _ := envFor(t, model.OnSpec)
	pHigh := guardHighValue(env.Pressure)
	lHigh := guardHighValue(env.Level)
	pRest := restoreValue(env.Pressure)
	lRest := restoreValue(env.Level)

	feed(t, m, model.OnSpec, pHigh, lHigh, 1)
	feed(t, m, model.OnSpec, pHigh, lHigh, 2) // both limiting
	// Pressure holds the restore band; level keeps guarding.
	for at := int64(3); at <= 8; at++ {
		if got := feed(t, m, model.OnSpec, pRest, lHigh, at); got != StateLimiting {
			t.Fatalf("must stay limiting while level still guards, got %s", got)
		}
	}
	// Now bring level home too; the window counts both signals together.
	feed(t, m, model.OnSpec, pRest, lRest, 9)
	feed(t, m, model.OnSpec, pRest, lRest, 10)
	feed(t, m, model.OnSpec, pRest, lRest, 11)
	if got := feed(t, m, model.OnSpec, pRest, lRest, 12); got != StateNormal {
		t.Fatalf("expected normal once both signals stable, got %s", got)
	}
}

func TestStaleTimestampRejected(t *testing.T) {
	m := newTestMonitor()
	_, p, l := envFor(t, model.OnSpec)
	feed(t, m, model.OnSpec, p, l, 10)
	if _, ok := m.Update(model.OnSpec, p, l, 10); ok {
		t.Fatal("duplicate timestamp must be rejected")
	}
	if _, ok := m.Update(model.OnSpec, p, l, 9); ok {
		t.Fatal("out-of-order timestamp must be rejected")
	}
}

func TestRestrictionEvidenceNamesSignalAndHistory(t *testing.T) {
	m := newTestMonitor()
	env, p, l := envFor(t, model.OnSpec)
	high := guardHighValue(env.Pressure)

	feed(t, m, model.OnSpec, p, l, 1)
	feed(t, m, model.OnSpec, p, l, 2)
	feed(t, m, model.OnSpec, high, l, 3) // first guard = trigger
	feed(t, m, model.OnSpec, high, l, 4) // confirms limiting
	feed(t, m, model.OnSpec, high, l, 5)

	status := m.Status()
	if len(status.Restrictions) != 1 {
		t.Fatalf("want one restriction, got %d", len(status.Restrictions))
	}
	r := status.Restrictions[0]
	if r.Signal != Pressure || r.Edge != "high" || r.Band != BandGuard {
		t.Fatalf("wrong restriction identity: %+v", r)
	}
	if r.Trigger.At != 3 {
		t.Fatalf("trigger should be first guard sample at t=3, got t=%d", r.Trigger.At)
	}
	if r.Peak.Value != high || r.Latest.At != 5 {
		t.Fatalf("peak/latest tracking wrong: peak=%v latest=%d", r.Peak, r.Latest.At)
	}
	if len(r.History) != 5 {
		t.Fatalf("history should run from pre-trigger through latest, got %d", len(r.History))
	}
	if r.History[0].At != 1 || r.History[len(r.History)-1].At != 5 {
		t.Fatalf("history span wrong: %+v", r.History)
	}
}

func TestPhaseEnvelopeChangesWithCampaignStage(t *testing.T) {
	warming := EnvelopeFor(model.DefaultLimits(), model.Warming)
	onSpec := EnvelopeFor(model.DefaultLimits(), model.OnSpec)
	if warming.Pressure.GuardHigh == onSpec.Pressure.GuardHigh {
		t.Fatal("warming and on-spec pressure envelopes should differ")
	}

	// A pressure between the two guard edges is guard in one phase and normal
	// in the other; the monitor must classify it against the live phase.
	v := (warming.Pressure.GuardHigh + onSpec.Pressure.GuardHigh) / 2
	if warming.Pressure.Classify(v) == onSpec.Pressure.Classify(v) {
		t.Fatalf("value %v between phase edges should classify differently", v)
	}

	m := newTestMonitor()
	_, _, l := envFor(t, model.OnSpec)
	if warming.Pressure.Classify(v) == BandGuard {
		feed(t, m, model.Warming, v, l, 1)
		if got := feed(t, m, model.Warming, v, l, 2); got != StateLimiting {
			t.Fatalf("value guards under warming, got %s", got)
		}
		// On-spec moves it below the guard edge, but until it crosses the
		// lower restore edge it stays held by the hysteresis gap.
		if got := feed(t, m, model.OnSpec, v, l, 3); got != StateLimiting {
			t.Fatalf("hysteresis gap under new phase should keep limiting, got %s", got)
		}
	} else {
		feed(t, m, model.OnSpec, v, l, 1)
		if got := feed(t, m, model.OnSpec, v, l, 2); got != StateNormal {
			t.Fatalf("value normal on-spec, got %s", got)
		}
	}
}

func TestActiveRestrictionSurvivesPhaseChange(t *testing.T) {
	m := newTestMonitor()
	env, _, l := envFor(t, model.OnSpec)
	hard := hardHighValue(env.Pressure)
	feed(t, m, model.OnSpec, hard, l, 1) // holding
	if got := feed(t, m, model.Transitioning, hard, l, 2); got != StateHolding {
		t.Fatalf("a hard restriction must not be cleared by changing phase, got %s", got)
	}
}
