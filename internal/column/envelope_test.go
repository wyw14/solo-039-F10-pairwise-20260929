package column

import (
	"math"
	"testing"

	"github.com/wyw14/solo-039/internal/model"
)

func campaignAt(phase model.Phase) model.Campaign {
	return model.Campaign{Phase: phase, Epoch: 1, Fencing: 1}
}

func TestNormalBaselinePermitsAll(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 1, Pressure: 0.8, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Normal {
		t.Fatalf("state = %v, want normal", got)
	}
	if !m.Permissive() {
		t.Fatal("baseline should be permissive")
	}
	for _, dir := range []string{"heat-up", "raise-level", "relieve", "hold", ""} {
		if !m.ActionAllowed(dir) {
			t.Fatalf("direction %q should be allowed in normal", dir)
		}
	}
}

// 接近上预警限时进入 limiting；在预警限上方抖动不会翻转许可；
// 只要没回到内收的释放带，即使短暂低于预警限也不恢复许可（迟滞）。
func TestWarningBandDoesNotChatter(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	// on-spec: 压力 warnMax=1.3, relMax=1.1
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 1, Pressure: 0.8, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	// 越过预警限 -> limiting
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 2, Pressure: 1.35, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Limiting {
		t.Fatalf("state = %v, want limiting", got)
	}
	if m.ActionAllowed("heat-up") {
		t.Fatal("heat-up must be denied in limiting")
	}
	if !m.ActionAllowed("relieve") {
		t.Fatal("relieve must remain allowed in limiting")
	}

	// 在预警限上下抖动（1.31..1.39），始终保持 limiting，不抖回 normal
	for i, p := range []float64{1.39, 1.31, 1.36, 1.32} {
		at := i + 3
		if err := m.Observe(campaignAt(model.OnSpec), Sample{At: int64(at), Pressure: p, Level: 0.5}); err != nil {
			t.Fatal(err)
		}
		if got := m.CurrentState(); got != Limiting {
			t.Fatalf("at %d pressure %.2f: state = %v, want limiting (no chatter)", at, p, got)
		}
		if m.Permissive() {
			t.Fatalf("at %d pressure %.2f must not become permissive", at, p)
		}
	}

	// 回到预警限以下但仍在释放带外（1.2，介于 1.1 与 1.3）：仍保持 limiting，
	// 不允许许可——这正是消除阈值附近抖动的迟滞带。
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 7, Pressure: 1.2, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Limiting {
		t.Fatalf("state = %v, want still limiting in hysteresis band", got)
	}
	if m.Permissive() {
		t.Fatal("must not be permissive while in hysteresis band")
	}

	// 进入释放带（<=1.1）后开始恢复，连续 3 个稳定样本才回到 normal
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 8, Pressure: 1.05, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Recovering {
		t.Fatalf("state = %v, want recovering inside release band", got)
	}
	for at, p := range []float64{1.0, 1.0} {
		if err := m.Observe(campaignAt(model.OnSpec), Sample{At: int64(9 + at), Pressure: p, Level: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.CurrentState(); got != Normal {
		t.Fatalf("state = %v, want normal after stable window", got)
	}
	if !m.Permissive() {
		t.Fatal("permissive must be restored after stable window")
	}
}

// 恢复必须连续满足稳定窗口；窗口内任何一次漂出释放带都重新计时。
func TestRecoveryRequiresStableWindow(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	base := campaignAt(model.OnSpec)

	// 触发 limiting
	if err := m.Observe(base, Sample{At: 1, Pressure: 1.35, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	// 回到释放带（<=1.1）：recovering, stableFor=1
	if err := m.Observe(base, Sample{At: 2, Pressure: 1.0, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Recovering {
		t.Fatalf("state = %v, want recovering", got)
	}
	if got := m.Status().StableFor; got != 1 {
		t.Fatalf("stableFor = %d, want 1", got)
	}
	// 第二个稳定样本
	if err := m.Observe(base, Sample{At: 3, Pressure: 1.0, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	// 窗口中漂出释放带（1.2）：回退 limiting，计数清零
	if err := m.Observe(base, Sample{At: 4, Pressure: 1.2, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Limiting {
		t.Fatalf("state = %v, want limiting after window break", got)
	}
	if got := m.Status().StableFor; got != 0 {
		t.Fatalf("stableFor = %d, want 0 after break", got)
	}
	// 重新连续三个稳定样本
	for at, p := range []float64{1.0, 1.0, 1.0} {
		if err := m.Observe(base, Sample{At: int64(5 + at), Pressure: p, Level: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.CurrentState(); got != Normal {
		t.Fatalf("state = %v, want normal after full stable window", got)
	}
	if !m.Permissive() {
		t.Fatal("permissive should return after stable window")
	}
}

// 越过硬限进入 hold，并记录是哪项信号、哪段历史导致；泄压等缓解动作仍允许。
func TestHardLimitHoldsWithAttribution(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	base := campaignAt(model.Warming)
	for at, p := range []float64{1.0, 1.2, 1.5} {
		if err := m.Observe(base, Sample{At: int64(at + 1), Pressure: p, Level: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	// 压力越过硬上限 2.0
	if err := m.Observe(base, Sample{At: 4, Pressure: 2.05, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Hold {
		t.Fatalf("state = %v, want hold", got)
	}
	if m.ActionAllowed("heat-up") {
		t.Fatal("heat-up must be denied in hold")
	}
	if !m.ActionAllowed("relieve") {
		t.Fatal("relieve must stay allowed even in hold")
	}
	if m.SnapshotValid() {
		t.Fatal("snapshot must be invalid beyond hard limit")
	}

	status := m.Status()
	if status.Breach == nil {
		t.Fatal("breach attribution missing")
	}
	if status.Breach.Signal != SignalPressure {
		t.Fatalf("breach signal = %v, want column-pressure", status.Breach.Signal)
	}
	if status.Breach.Bound != BoundPressureH {
		t.Fatalf("breach bound = %v, want pressure-upper", status.Breach.Bound)
	}
	if status.Breach.Limit != 2.0 || status.Breach.Value != 2.05 {
		t.Fatalf("breach value/limit = %.2f/%.2f", status.Breach.Value, status.Breach.Limit)
	}
	if len(status.Breach.History) != 4 {
		t.Fatalf("breach history = %d samples, want 4", len(status.Breach.History))
	}
	if len(status.Active) != 1 || status.Active[0].Zone != "hard" {
		t.Fatalf("active readings = %+v, want one hard pressure reading", status.Active)
	}
	if len(status.Reasons) == 0 {
		t.Fatal("operator-facing reasons missing")
	}

	// 回到预警带（仍超限方向）-> limiting，不归零
	if err := m.Observe(base, Sample{At: 5, Pressure: 1.6, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Limiting {
		t.Fatalf("state = %v, want limiting after backing off hard limit", got)
	}
}

// 阶段决定包络：同一实测在 on-spec 受限、在 warming 正常。
func TestEnvelopeVariesByPhase(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	// on-spec warnMax=1.3
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 1, Pressure: 1.35, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Limiting {
		t.Fatalf("on-spec state = %v, want limiting", got)
	}
	// 同一压力在 warming（warnMax=1.5）属于迟滞/正常：
	// 已受限后 warming 下 1.35 位于释放带(<=1.3)外，仍需恢复——先验证新监控器下 warming 直接不限制。
	m2 := NewMonitor(MonitorConfig{WindowNeeded: 3})
	if err := m2.Observe(campaignAt(model.Warming), Sample{At: 1, Pressure: 1.35, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	if got := m2.CurrentState(); got != Normal {
		t.Fatalf("warming state = %v, want normal for same pressure", got)
	}
}

func TestLevelLowerLimit(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	// on-spec 液位 warnMin=0.20
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 1, Pressure: 0.8, Level: 0.18}); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentState(); got != Limiting {
		t.Fatalf("state = %v, want limiting for low level", got)
	}
	if status := m.Status(); len(status.Active) != 1 || status.Active[0].Signal != SignalLevel || status.Active[0].Bound != BoundLevelL {
		t.Fatalf("active = %+v, want level-lower", status.Active)
	}
}

func TestNaNRejectedWithoutStateChange(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 1, Pressure: 0.8, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	before := m.Status()
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 2, Pressure: math.NaN(), Level: 0.5}); err == nil {
		t.Fatal("NaN pressure must be rejected")
	}
	after := m.Status()
	if after.State != before.State || after.Latest.At != before.Latest.At {
		t.Fatal("rejected NaN sample must not change monitor state/latest")
	}
}

func TestForceHold(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	if err := m.Observe(campaignAt(model.OnSpec), Sample{At: 1, Pressure: 0.8, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	m.ForceHold(SignalPressure, BoundPressureH, 99, 2.4, 2.0)
	if got := m.CurrentState(); got != Hold {
		t.Fatalf("state = %v, want hold after ForceHold", got)
	}
	if status := m.Status(); status.Breach == nil || status.Breach.At != 99 {
		t.Fatalf("breach = %+v, want forced breach at 99", status.Breach)
	}
}

func TestBreachClearedAfterRecovery(t *testing.T) {
	m := NewMonitor(MonitorConfig{WindowNeeded: 3})
	base := campaignAt(model.Warming)
	if err := m.Observe(base, Sample{At: 1, Pressure: 2.05, Level: 0.5}); err != nil {
		t.Fatal(err)
	}
	// 回到释放带（warming relMax=1.3）并连续稳定
	for at, p := range []float64{1.0, 1.0, 1.0} {
		if err := m.Observe(base, Sample{At: int64(2 + at), Pressure: p, Level: 0.5}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.CurrentState(); got != Normal {
		t.Fatalf("state = %v, want normal", got)
	}
	if status := m.Status(); status.Breach != nil {
		t.Fatalf("breach should clear after recovery, got %+v", status.Breach)
	}
}
