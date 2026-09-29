package envelope

import (
	"sort"
	"sync"

	"github.com/wyw14/solo-039/internal/model"
)

// State is the de-bounced restriction state of the column.
type State string

const (
	// StateNormal: every signal is inside its envelope; control actions run.
	StateNormal State = "normal"
	// StateLimiting: a signal has held in the guard band long enough to
	// confirm the excursion; energy-adding actions are restricted.
	StateLimiting State = "limiting"
	// StateHolding: a hard operating limit was crossed; actions are denied.
	StateHolding State = "holding"
	// StateRecovering: the signals are back inside the recovery band but the
	// stable window has not elapsed; the restriction is still in force.
	StateRecovering State = "recovering"
)

// Action categorises a control request so the envelope can decide whether it
// may run under the current state.
type Action string

const (
	// ActionEnergy raises or demands heat/reflux (steam ramp up, reflux
	// lease). It is only released in the normal state.
	ActionEnergy Action = "energy"
	// ActionSafe moves the plant toward safety (steam cutback, shutdown) and
	// is released in every state so an operator is never locked out of a
	// protective move.
	ActionSafe Action = "safe"
)

// Config tunes the de-bounce and recovery timing. Zero fields are replaced
// with defaults by NewMonitor.
type Config struct {
	// EnterSamples is the number of consecutive guard-band samples required
	// before an excursion is confirmed as "limiting".
	EnterSamples int
	// StableSamples is the number of consecutive samples that must land in
	// the recovery band before the restriction is released.
	StableSamples int
	// StableWindow is the minimum time span, in the unit of the At timestamp,
	// that the stable samples must cover (first to last).
	StableWindow int64
	// HistoryDepth is the number of recent signal updates retained.
	HistoryDepth int
	// HistoryLookback is how many updates before a trigger are included in the
	// evidence history shown to the operator.
	HistoryLookback int
}

// DefaultConfig returns the production timing: two confirming samples to
// enter, four samples spanning at least three seconds to recover.
func DefaultConfig() Config {
	return Config{EnterSamples: 2, StableSamples: 4, StableWindow: 3, HistoryDepth: 32, HistoryLookback: 3}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.EnterSamples <= 0 {
		c.EnterSamples = d.EnterSamples
	}
	if c.StableSamples <= 0 {
		c.StableSamples = d.StableSamples
	}
	if c.StableWindow <= 0 {
		c.StableWindow = d.StableWindow
	}
	if c.HistoryDepth <= 0 {
		c.HistoryDepth = d.HistoryDepth
	}
	if c.HistoryLookback < 0 {
		c.HistoryLookback = d.HistoryLookback
	}
	return c
}

// Reading is one measured value at one point in history.
type Reading struct {
	Seq   int64   `json:"seq"`
	At    int64   `json:"at"`
	Value float64 `json:"value"`
}

// Restriction is the evidence for one signal that is currently limiting or
// holding the plant (or still clearing its recovery window).
type Restriction struct {
	Signal        Signal      `json:"signal"`
	Phase         model.Phase `json:"phase"`
	Band          Band        `json:"band"`
	Edge          string      `json:"edge"`
	Threshold     Thresholds  `json:"thresholds"`
	Since         int64       `json:"since"`
	Trigger       Reading     `json:"trigger"`
	Peak          Reading     `json:"peak"`
	Latest        Reading     `json:"latest"`
	WithinRestore bool        `json:"within_restore"`
	// History is the measured segment for this signal from a few samples
	// before the trigger through the latest sample, so the operator can see
	// exactly which run of readings produced the restriction.
	History []Reading `json:"history"`
}

// Window reports stable-window progress while recovering.
type Window struct {
	Samples       int   `json:"samples"`
	NeededSamples int   `json:"needed_samples"`
	Span          int64 `json:"span"`
	RequiredSpan  int64 `json:"required_span"`
	StartedAt     int64 `json:"started_at"`
}

func (w Window) satisfied(at int64) bool {
	return w.Samples >= w.NeededSamples && at-w.StartedAt >= w.RequiredSpan
}

// Permit answers a control action against the current state and carries the
// restrictions that explain a denial.
type Permit struct {
	State        State         `json:"state"`
	Allowed      bool          `json:"allowed"`
	Restrictions []Restriction `json:"restrictions"`
}

type update struct {
	seq      int64
	at       int64
	pressure float64
	level    float64
}

type track struct {
	r        Restriction
	highSide bool
	restored bool
}

type guardRun struct {
	first Reading
	count int
}

// Monitor consumes continuous pressure/level measurements and maintains the
// restriction state machine for one column.
type Monitor struct {
	mu     sync.Mutex
	limits model.OperatingLimits
	cfg    Config
	phase  model.Phase
	state  State
	seq    int64
	// runs counts consecutive guard-band samples per signal for enter de-bounce.
	runs map[Signal]guardRun
	// tracks holds one active restriction per signal until it recovers.
	tracks  map[Signal]*track
	history []update
	win     Window
	winOn   bool
}

// NewMonitor builds a monitor for the given operating limits.
func NewMonitor(limits model.OperatingLimits, cfg Config) *Monitor {
	return &Monitor{
		limits: limits,
		cfg:    cfg.withDefaults(),
		phase:  model.Preparing,
		state:  StateNormal,
		runs:   make(map[Signal]guardRun),
		tracks: make(map[Signal]*track),
	}
}

// Reset clears every restriction, de-bounce run, recovery window and history
// and seeds the monitor with one measurement for the supplied phase. It is used
// to rebuild the monitor from a persisted snapshot on startup; a restarted
// process begins with a clean (normal) envelope rather than inheriting a
// restriction it can no longer evidence.
func (m *Monitor) Reset(phase model.Phase, pressure, level float64, at int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phase = phase
	m.state = StateNormal
	m.seq = 0
	m.runs = make(map[Signal]guardRun)
	m.tracks = make(map[Signal]*track)
	m.history = nil
	m.win = Window{}
	m.winOn = false

	m.seq++
	m.history = append(m.history, update{seq: m.seq, at: at, pressure: pressure, level: level})
}

// Changed is returned by Update when the de-bounced state moves.
type Changed struct {
	From    State
	To      State
	At      int64
	Signals []Signal
}

// Update feeds one synchronised pressure/level measurement for the given
// campaign phase and advances the state machine. Readings are expected in
// non-decreasing time order; a re-ordered or duplicate timestamp is rejected so
// the stable window cannot be satisfied by replay.
func (m *Monitor) Update(phase model.Phase, pressure, level float64, at int64) (Changed, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.history) > 0 && at <= m.history[len(m.history)-1].at {
		return Changed{}, false
	}
	m.phase = phase

	m.seq++
	u := update{seq: m.seq, at: at, pressure: pressure, level: level}
	m.history = append(m.history, u)
	if len(m.history) > m.cfg.HistoryDepth {
		m.history = m.history[len(m.history)-m.cfg.HistoryDepth:]
	}

	env := EnvelopeFor(m.limits, phase)
	m.applySignal(env, Pressure, pressure, at)
	m.applySignal(env, Level, level, at)

	from := m.state
	// Snapshot the restricted signals before advancing: a completed recovery
	// clears the tracks, but the transition event must still name the signals
	// that were released.
	activeSignals := make([]Signal, 0, len(m.tracks))
	for signal := range m.tracks {
		activeSignals = append(activeSignals, signal)
	}
	sort.Slice(activeSignals, func(i, j int) bool { return activeSignals[i] < activeSignals[j] })

	to := m.advanceWindow(at)
	m.state = to
	if from != to {
		signals := activeSignals
		if len(m.tracks) > 0 {
			signals = make([]Signal, 0, len(m.tracks))
			for signal := range m.tracks {
				signals = append(signals, signal)
			}
			sort.Slice(signals, func(i, j int) bool { return signals[i] < signals[j] })
		}
		return Changed{From: from, To: to, At: at, Signals: signals}, true
	}
	return Changed{}, true
}

func (m *Monitor) applySignal(env Envelope, signal Signal, value float64, at int64) {
	th := env.thresholds(signal)
	band := th.Classify(value)
	reading := Reading{Seq: m.seq, At: at, Value: value}

	switch band {
	case BandHard:
		// A hard crossing is immediate and clears the enter de-bounce.
		delete(m.runs, signal)
		m.elevate(signal, env, th, band, reading, reading)
	case BandGuard:
		// Confirm only after EnterSamples consecutive guard samples.
		run := m.runs[signal]
		if run.count == 0 {
			run.first = reading
		}
		run.count++
		m.runs[signal] = run
		if t := m.tracks[signal]; t != nil {
			// An existing restriction tracks the excursion but cannot start
			// recovering while still outside the restore band.
			m.observe(t, band, reading, false)
		} else if run.count >= m.cfg.EnterSamples {
			m.elevate(signal, env, th, band, run.first, reading)
		}
	default:
		delete(m.runs, signal)
		if t := m.tracks[signal]; t != nil {
			// Restored only once the value crosses the inner restore edge.
			// Between that edge and the guard edge it is technically back in
			// the normal band but is still inside the hysteresis gap, so the
			// restriction is shown as guarding and cannot start recovery.
			restored := th.Restored(value)
			effBand := BandNormal
			if !restored {
				effBand = BandGuard
			}
			m.observe(t, effBand, reading, restored)
		}
	}
}

// elevate opens a restriction or raises an existing one to a harder band.
// trigger is the first reading that confirms the excursion; latest is the
// reading that elevated it.
func (m *Monitor) elevate(signal Signal, env Envelope, th Thresholds, band Band, trigger, latest Reading) {
	highSide := latest.Value >= th.GuardHigh
	if t := m.tracks[signal]; t != nil {
		if band == BandHard {
			t.r.Band = BandHard
		}
		t.highSide = highSide
		t.restored = false
		m.observe(t, band, latest, false)
		return
	}
	t := &track{highSide: highSide}
	t.r = Restriction{
		Signal: signal, Phase: env.Phase, Band: band,
		Edge: edgeName(highSide), Threshold: th,
		Since: trigger.At, Trigger: trigger, Peak: trigger, Latest: trigger,
	}
	m.tracks[signal] = t
	m.observe(t, band, latest, false)
}

// observe folds a new reading into an open restriction.
func (m *Monitor) observe(t *track, band Band, reading Reading, restored bool) {
	if band == BandHard {
		t.r.Band = BandHard
	} else if t.r.Band != BandHard {
		t.r.Band = band
	}
	if t.highSide {
		if reading.Value > t.r.Peak.Value {
			t.r.Peak = reading
		}
	} else {
		if reading.Value < t.r.Peak.Value {
			t.r.Peak = reading
		}
	}
	t.r.Latest = reading
	t.restored = restored
	t.r.WithinRestore = restored
}

// advanceWindow drives the global state once both signals have been folded in.
func (m *Monitor) advanceWindow(at int64) State {
	if len(m.tracks) == 0 {
		m.winOn = false
		m.win = Window{}
		return StateNormal
	}

	allRestored := true
	anyHard := false
	for _, t := range m.tracks {
		if !t.restored {
			allRestored = false
		}
		if t.r.Band == BandHard {
			anyHard = true
		}
	}

	if !allRestored {
		// Restriction in force; any excursion outside the restore band resets
		// the stable window so a single dip re-starts the recovery timer.
		m.winOn = false
		m.win = Window{}
		if anyHard {
			return StateHolding
		}
		return StateLimiting
	}

	if !m.winOn {
		m.winOn = true
		m.win = Window{Samples: 1}
		m.win.NeededSamples = m.cfg.StableSamples
		m.win.RequiredSpan = m.cfg.StableWindow
		m.win.StartedAt = at
	} else {
		m.win.Samples++
	}
	m.win.Span = at - m.win.StartedAt

	if m.win.satisfied(at) {
		m.tracks = make(map[Signal]*track)
		m.runs = make(map[Signal]guardRun)
		m.winOn = false
		m.win = Window{}
		return StateNormal
	}
	return StateRecovering
}

func edgeName(highSide bool) string {
	if highSide {
		return "high"
	}
	return "low"
}

// StateValue returns the current de-bounced state.
func (m *Monitor) StateValue() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Phase returns the phase of the most recent update.
func (m *Monitor) Phase() model.Phase {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phase
}

// Permit decides whether the given action may run now. Safe actions are always
// released; energy actions only in the normal state.
func (m *Monitor) Permit(action Action) Permit {
	m.mu.Lock()
	defer m.mu.Unlock()
	restrictions := m.restrictionsLocked()
	allowed := action == ActionSafe || m.state == StateNormal
	return Permit{State: m.state, Allowed: allowed, Restrictions: restrictions}
}

// Status is the operator-facing snapshot: state, the envelope in force,
// restrictions with their measured history segment, and recovery-window
// progress.
type Status struct {
	Phase        model.Phase   `json:"phase"`
	State        State         `json:"state"`
	Envelope     Envelope      `json:"envelope"`
	Restrictions []Restriction `json:"restrictions"`
	Window       Window        `json:"window"`
}

// Status returns a copy of the full monitor status.
func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Phase:        m.phase,
		State:        m.state,
		Envelope:     EnvelopeFor(m.limits, m.phase),
		Restrictions: m.restrictionsLocked(),
		Window:       m.win,
	}
}

func (m *Monitor) restrictionsLocked() []Restriction {
	out := make([]Restriction, 0, len(m.tracks))
	for signal, t := range m.tracks {
		r := t.r
		r.History = m.segmentLocked(signal, r.Trigger.Seq)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Band == BandHard && out[j].Band != BandHard {
			return true
		}
		if out[j].Band == BandHard && out[i].Band != BandHard {
			return false
		}
		return out[i].Signal < out[j].Signal
	})
	return out
}

// segmentLocked returns the readings for one signal from a few samples before
// the trigger through the most recent sample.
func (m *Monitor) segmentLocked(signal Signal, triggerSeq int64) []Reading {
	start := triggerSeq - int64(m.cfg.HistoryLookback)
	if start < 0 {
		start = 0
	}
	out := make([]Reading, 0)
	for _, u := range m.history {
		if u.seq < start {
			continue
		}
		value := u.pressure
		if signal == Level {
			value = u.level
		}
		out = append(out, Reading{Seq: u.seq, At: u.at, Value: value})
	}
	return out
}
