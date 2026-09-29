package column

import (
	"fmt"
	"math"
	"sync"

	"github.com/wyw14/solo-039/internal/model"
)

// State 是包络监控的迟滞状态：
//   - Normal      所有信号位于安全带内，控制动作全部许可
//   - Limiting    信号进入预警带，动作被限制（只允许减热/泄压方向），但不立即停车
//   - Hold        信号越过硬停车限（或被其他硬保护置位），增加风险的动作被拒绝并保持
//   - Recovering  信号已退回安全带，但尚未满足连续稳定窗口，维持限制直到窗口完成
type EnvelopeState string

const (
	Normal     EnvelopeState = "normal"
	Limiting   EnvelopeState = "limiting"
	Recovering EnvelopeState = "recovering"
	Hold       EnvelopeState = "hold"
)

// SignalName 标识被监控的连续信号。
type SignalName string

const (
	SignalPressure SignalName = "column-pressure"
	SignalLevel    SignalName = "column-level"
)

// Bound 标识信号命中的包络边界。
type Bound string

const (
	BoundNone      Bound = ""
	BoundPressureH Bound = "pressure-upper"
	BoundPressureL Bound = "pressure-lower"
	BoundLevelH    Bound = "level-upper"
	BoundLevelL    Bound = "level-lower"
)

// Sample 是一次连续压液实测。At 为递增时间戳（秒），与 analyzer.Window 的单调时间戳约定一致。
type Sample struct {
	At       int64
	Pressure float64
	Level    float64
	Phase    model.Phase
}

// Reading 分类单次观测相对包络的位置。
type Reading struct {
	Signal SignalName `json:"signal"`
	Bound  Bound      `json:"bound"`
	// Zone 为 normal/warning/hard（相对预警限/硬限）。
	Zone string `json:"zone"`
	// Released 表示该信号已深入内收的释放带；受限状态下只有它为真才允许恢复。
	Released bool `json:"released"`
	// Value 为实测值，Limit 为命中的那条限（预警带取预警限，硬区取硬限）。
	Value float64 `json:"value"`
	Limit float64 `json:"limit"`
}

// Breach 记录一次限制的成因：哪个信号、命中哪条边界、进入时的实测值与限、时间戳，
// 以及导致该判定的连续历史窗口。ResolvedAt 标记信号退回安全带、进入恢复的时刻。
type Breach struct {
	Signal     SignalName `json:"signal"`
	Bound      Bound      `json:"bound"`
	At         int64      `json:"at"`
	Value      float64    `json:"value"`
	Limit      float64    `json:"limit"`
	History    []Sample   `json:"history"`
	ResolvedAt int64      `json:"resolved_at,omitempty"`
}

// EnvelopeStatus 向操作员与上层暴露当前状态及归因。
type EnvelopeStatus struct {
	State        EnvelopeState `json:"state"`
	Phase        model.Phase   `json:"phase"`
	Latest       *Sample       `json:"latest,omitempty"`
	Active       []Reading     `json:"active_signals,omitempty"`
	Reasons      []string      `json:"reasons,omitempty"`
	Breach       *Breach       `json:"breach,omitempty"`
	StableFor    int           `json:"stable_for"`
	WindowNeeded int           `json:"window_needed"`
}

// band 为单个信号的分带包络，升序排列：
// hardMin < warnMin < relMin <= relMax < warnMax < hardMax。
//   - 硬限外（hardMin/hardMax）-> hard 区（hold）
//   - 从 normal 越过 warnMin/warnMax -> warning 区（limiting）
//   - 已被限制后，只有深入到更内收的 relMin/relMax 释放带内才算"安全"，
//     开始恢复计时。进入限与释放限之间的不对称带即为迟滞区，
//     信号在阈值附近来回时不会在许可/拒绝间翻转。
type band struct {
	hardMin, warnMin, relMin, relMax, warnMax, hardMax float64
}

// classify 用于 normal 状态下的越限判定，返回区域与方向（"upper"/"lower"/""）。
func (b band) classify(value float64) (zone, dir string) {
	switch {
	case value < b.hardMin:
		return "hard", "lower"
	case value > b.hardMax:
		return "hard", "upper"
	case value < b.warnMin:
		return "warning", "lower"
	case value > b.warnMax:
		return "warning", "upper"
	default:
		return "normal", ""
	}
}

// released 表示值已深入释放带，受限状态下只有它为真才开始/继续恢复。
func (b band) released(value float64) bool {
	return value >= b.relMin && value <= b.relMax
}

func (b band) limit(zone, dir string) float64 {
	if zone == "hard" {
		if dir == "upper" {
			return b.hardMax
		}
		return b.hardMin
	}
	if dir == "upper" {
		return b.warnMax
	}
	return b.warnMin
}

// phaseEnvelope 是一个批次阶段的压液包络（压力、液位各一条带）。
type phaseEnvelope struct {
	pressure band
	level    band
}

// Envelopes 按当前批次阶段返回可操作包络。硬限与 model.DefaultLimits 对齐
// （压力上限 2.0、液位上限 0.95）；预警限在硬限内按阶段收窄，
// 越靠后/越敏感的阶段收得越紧。这样接近硬限时控制动作先进入"限制"，
// 而不是在许可与拒绝间瞬时翻转。
func Envelopes() map[model.Phase]phaseEnvelope {
	return map[model.Phase]phaseEnvelope{
		model.Preparing: {
			pressure: band{hardMin: 0.0, warnMin: 0.20, relMin: 0.30, relMax: 1.4, warnMax: 1.6, hardMax: 2.0},
			level:    band{hardMin: 0.0, warnMin: 0.05, relMin: 0.15, relMax: 0.80, warnMax: 0.90, hardMax: 0.95},
		},
		model.Warming: {
			pressure: band{hardMin: 0.0, warnMin: 0.25, relMin: 0.35, relMax: 1.3, warnMax: 1.5, hardMax: 2.0},
			level:    band{hardMin: 0.0, warnMin: 0.10, relMin: 0.20, relMax: 0.75, warnMax: 0.85, hardMax: 0.95},
		},
		model.Stabilizing: {
			pressure: band{hardMin: 0.0, warnMin: 0.30, relMin: 0.40, relMax: 1.2, warnMax: 1.4, hardMax: 2.0},
			level:    band{hardMin: 0.0, warnMin: 0.15, relMin: 0.25, relMax: 0.70, warnMax: 0.80, hardMax: 0.95},
		},
		model.OnSpec: {
			pressure: band{hardMin: 0.0, warnMin: 0.35, relMin: 0.45, relMax: 1.1, warnMax: 1.3, hardMax: 2.0},
			level:    band{hardMin: 0.0, warnMin: 0.20, relMin: 0.30, relMax: 0.65, warnMax: 0.75, hardMax: 0.95},
		},
		model.Transitioning: {
			pressure: band{hardMin: 0.0, warnMin: 0.25, relMin: 0.35, relMax: 1.3, warnMax: 1.5, hardMax: 2.0},
			level:    band{hardMin: 0.0, warnMin: 0.10, relMin: 0.20, relMax: 0.75, warnMax: 0.85, hardMax: 0.95},
		},
		model.Shutting: {
			pressure: band{hardMin: 0.0, warnMin: 0.0, relMin: 0.0, relMax: 1.5, warnMax: 1.7, hardMax: 2.0},
			level:    band{hardMin: 0.0, warnMin: 0.0, relMin: 0.0, relMax: 0.85, warnMax: 0.95, hardMax: 1.0},
		},
		model.Stopped: {
			pressure: band{hardMin: 0.0, warnMin: 0.0, relMin: 0.0, relMax: 1.5, warnMax: 1.7, hardMax: 2.0},
			level:    band{hardMin: 0.0, warnMin: 0.0, relMin: 0.0, relMax: 0.85, warnMax: 0.95, hardMax: 1.0},
		},
	}
}

// Monitor 消费连续压液实测，按阶段包络把控制许可迁移到
// normal / limiting / recovering / hold，并保留归因历史。
type Monitor struct {
	mu sync.Mutex

	state        EnvelopeState
	phase        model.Phase
	latest       *Sample
	active       []Reading
	reasons      []string
	breach       *Breach
	stableFor    int
	windowNeeded int
	// 触发恢复判定的最近连续安全带样本。
	stableWindow []Sample
	// 完整近期历史，供 breach.History 与操作员核验。
	history    []Sample
	historyCap int
	envelopes  map[model.Phase]phaseEnvelope
}

// MonitorConfig 调整恢复窗口与历史长度。
type MonitorConfig struct {
	// WindowNeeded 是从限制/保持恢复前必须连续落在安全带内的样本数。
	WindowNeeded int
	// HistoryCap 是保留的近期实测样本数。
	HistoryCap int
}

// NewMonitor 创建监控器。windowNeeded 小于 3 时取 3。
func NewMonitor(cfg MonitorConfig) *Monitor {
	windowNeeded := cfg.WindowNeeded
	if windowNeeded < 3 {
		windowNeeded = 3
	}
	historyCap := cfg.HistoryCap
	if historyCap < windowNeeded*2 {
		historyCap = windowNeeded * 4
	}
	return &Monitor{
		state:        Normal,
		phase:        model.Preparing,
		windowNeeded: windowNeeded,
		historyCap:   historyCap,
		envelopes:    Envelopes(),
	}
}

// Observe 消费一次连续压液实测并推进状态机。
// 返回的 error 仅表示该样本本身非法（NaN），此时不改变限制状态。
func (m *Monitor) Observe(campaign model.Campaign, sample Sample) error {
	if math.IsNaN(sample.Pressure) || math.IsNaN(sample.Level) {
		return fmt.Errorf("column signal is not numeric")
	}
	sample.Phase = campaign.Phase

	m.mu.Lock()
	defer m.mu.Unlock()

	// 阶段推进时换用对应包络，并按新包络立即重新分类，避免旧包络下"看似正常"。
	m.phase = campaign.Phase
	m.latest = &sample
	m.pushHistory(sample)

	env, ok := m.envelopes[campaign.Phase]
	if !ok {
		env = m.envelopes[model.Preparing]
	}

	readings := []Reading{
		classifyReading(SignalPressure, sample.Pressure, env.pressure),
		classifyReading(SignalLevel, sample.Level, env.level),
	}
	hardSignals := pickZone(readings, "hard")
	warningSignals := pickZone(readings, "warning")
	// offending 含预警带信号以及"尚未回到释放带"的信号；后者用于受限状态下的迟滞判定。
	offending := pickOffending(readings)
	allReleased := len(hardSignals) == 0 && len(offending) == 0

	switch {
	case len(hardSignals) > 0:
		// 越过硬停车限：进入保持并记录成因与历史；恢复窗口中断。
		m.enterHold(hardSignals, sample)
	case allReleased:
		switch m.state {
		case Recovering:
			// 恢复计时：连续停留在释放带内的样本满窗口后才回到 normal。
			m.stableFor++
			m.stableWindow = append(m.stableWindow, sample)
			if m.stableFor >= m.windowNeeded {
				m.enterNormal()
			} else {
				m.reasons = m.recoveryReasons()
			}
		case Hold, Limiting:
			m.enterRecovering(sample)
		default:
			// 已 normal：保持干净。
			m.stableFor = 0
			m.stableWindow = m.stableWindow[:0]
		}
	default:
		// 未越硬限但也没回到释放带：
		//   - normal 下只有真正越过预警限才限制（预警限与释放限之间的迟滞带不触发）；
		//   - hold 降级为 limiting；
		//   - recovering 的稳定窗口被打断，哪怕只是漂出释放带、尚未再越预警限，也重新等待。
		switch m.state {
		case Normal:
			if len(warningSignals) > 0 {
				m.enterLimiting(warningSignals)
			}
		default:
			m.enterLimiting(offending)
		}
	}
	return nil
}

// Permissive 返回当前是否允许"增热/提量"方向的控制动作。
func (m *Monitor) Permissive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == Normal
}

// ActionAllowed 按动作方向给出许可：
//   - "heat-up"/"raise-level"/"increase"：增加压/液风险的动作，仅 normal 允许
//   - "relieve"/"reduce"/"decrease"：降低压/液的缓解动作，hold 以外允许
//   - "hold"/"monitor"/""：中性动作，hold 以外允许
//
// 这使接近阈值时不是"全许可/全拒绝"翻转，而是按方向分级。
func (m *Monitor) ActionAllowed(direction string) bool {
	m.mu.Lock()
	state := m.state
	m.mu.Unlock()

	switch direction {
	case "heat-up", "raise-level", "increase":
		return state == Normal
	case "relieve", "reduce", "decrease":
		// 缓解动作在任何状态都允许——越硬限后更要能泄压/减量。
		return true
	case "hold", "monitor", "":
		return state != Hold
	default:
		return state == Normal
	}
}

// SnapshotValid 兼容既有校验语义：当前实测是否位于阶段硬限内。
func (m *Monitor) SnapshotValid() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.latest == nil {
		return true
	}
	env, ok := m.envelopes[m.phase]
	if !ok {
		env = m.envelopes[model.Preparing]
	}
	pZone, _ := env.pressure.classify(m.latest.Pressure)
	lZone, _ := env.level.classify(m.latest.Level)
	return pZone != "hard" && lZone != "hard"
}

// State_ 返回当前迟滞状态（方法名避免与 State 类型冲突）。
func (m *Monitor) CurrentState() EnvelopeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Status 返回操作员可见的当前限制状态与归因。
func (m *Monitor) Status() EnvelopeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := EnvelopeStatus{
		State:        m.state,
		Phase:        m.phase,
		StableFor:    m.stableFor,
		WindowNeeded: m.windowNeeded,
		Active:       append([]Reading(nil), m.active...),
		Reasons:      append([]string(nil), m.reasons...),
	}
	if m.latest != nil {
		latest := *m.latest
		status.Latest = &latest
	}
	if m.breach != nil {
		breach := *m.breach
		breach.History = append([]Sample(nil), m.breach.History...)
		status.Breach = &breach
	}
	return status
}

// History 返回最近的连续实测样本（副本），供操作员核验"哪段历史"导致当前限制。
func (m *Monitor) History() []Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sample(nil), m.history...)
}

// ForceHold 供其他硬保护（如 relief 超时）显式置入保持，不等待下一个样本。
func (m *Monitor) ForceHold(signal SignalName, bound Bound, at int64, value, limit float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	readings := []Reading{{Signal: signal, Bound: bound, Zone: "hard", Value: value, Limit: limit}}
	sample := Sample{At: at, Phase: m.phase}
	if m.latest != nil {
		sample.Pressure = m.latest.Pressure
		sample.Level = m.latest.Level
	}
	m.enterHold(readings, sample)
}

// --- 内部迁移（调用方持锁） ---

func (m *Monitor) pushHistory(sample Sample) {
	m.history = append(m.history, sample)
	if len(m.history) > m.historyCap {
		m.history = m.history[len(m.history)-m.historyCap:]
	}
}

func (m *Monitor) enterHold(hard []Reading, sample Sample) {
	first := hard[0]
	m.breach = &Breach{
		Signal:  first.Signal,
		Bound:   first.Bound,
		At:      sample.At,
		Value:   first.Value,
		Limit:   first.Limit,
		History: append([]Sample(nil), m.history...),
	}
	m.state = Hold
	m.active = hard
	m.reasons = reasonsFor(hard)
	m.stableFor = 0
	m.stableWindow = m.stableWindow[:0]
}

func (m *Monitor) enterLimiting(warning []Reading) {
	m.state = Limiting
	if len(warning) > 0 {
		m.active = warning
		m.reasons = reasonsFor(warning)
	}
	m.stableFor = 0
	m.stableWindow = m.stableWindow[:0]
	// 不覆盖已有 hard breach：保留首次硬越限的信号与历史归因。
}

func (m *Monitor) enterRecovering(sample Sample) {
	m.state = Recovering
	m.stableFor = 1
	m.stableWindow = []Sample{sample}
	if m.breach != nil && m.breach.ResolvedAt == 0 {
		m.breach.ResolvedAt = sample.At
	}
	m.reasons = m.recoveryReasons()
}

func (m *Monitor) enterNormal() {
	m.state = Normal
	m.active = nil
	m.reasons = nil
	m.breach = nil
	m.stableFor = 0
	m.stableWindow = m.stableWindow[:0]
}

func (m *Monitor) recoveryReasons() []string {
	reasons := make([]string, 0, len(m.active)+1)
	reasons = append(reasons, reasonsFor(m.active)...)
	reasons = append(reasons, fmt.Sprintf("recovery in progress: %d/%d consecutive stable samples required", m.stableFor, m.windowNeeded))
	return reasons
}

// --- 纯函数辅助 ---

func classifyReading(name SignalName, value float64, b band) Reading {
	zone, dir := b.classify(value)
	reading := Reading{Signal: name, Zone: zone, Value: value, Released: b.released(value)}
	switch {
	case zone != "normal":
		reading.Limit = b.limit(zone, dir)
	case value < b.relMin:
		// 下侧迟滞带：目标是回到内收的下释放限。
		dir = "lower"
		reading.Limit = b.relMin
	case value > b.relMax:
		// 上侧迟滞带：目标是回到内收的上释放限。
		dir = "upper"
		reading.Limit = b.relMax
	}
	if dir == "upper" {
		if name == SignalPressure {
			reading.Bound = BoundPressureH
		} else {
			reading.Bound = BoundLevelH
		}
	} else if dir == "lower" {
		if name == SignalPressure {
			reading.Bound = BoundPressureL
		} else {
			reading.Bound = BoundLevelL
		}
	}
	return reading
}

func pickZone(readings []Reading, zone string) []Reading {
	out := make([]Reading, 0, len(readings))
	for _, r := range readings {
		if r.Zone == zone {
			out = append(out, r)
		}
	}
	return out
}

// pickOffending 返回受限状态下仍阻止恢复的信号：越预警/硬限，或尚未回到释放带。
func pickOffending(readings []Reading) []Reading {
	out := make([]Reading, 0, len(readings))
	for _, r := range readings {
		if r.Zone == "hard" || r.Zone == "warning" || !r.Released {
			out = append(out, r)
		}
	}
	return out
}

func reasonsFor(readings []Reading) []string {
	reasons := make([]string, 0, len(readings))
	for _, r := range readings {
		switch {
		case r.Zone == "hard":
			reasons = append(reasons, fmt.Sprintf("%s beyond %s limit: %.3f vs %.3f", r.Signal, r.Bound, r.Value, r.Limit))
		case r.Zone == "warning":
			reasons = append(reasons, fmt.Sprintf("%s approaching %s limit: %.3f vs %.3f", r.Signal, r.Bound, r.Value, r.Limit))
		case !r.Released:
			reasons = append(reasons, fmt.Sprintf("%s not back inside release band: %.3f must reach %.3f", r.Signal, r.Value, r.Limit))
		default:
			reasons = append(reasons, fmt.Sprintf("%s in normal band: %.3f", r.Signal, r.Value))
		}
	}
	return reasons
}
