package service

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wyw14/solo-039/internal/envelope"
)

// EnvelopeStatus is the operator-facing view of the restriction state machine.
type EnvelopeStatus struct {
	Status envelope.Status `json:"status"`
	Permit envelope.Permit `json:"permit"`
}

// IngestSignals feeds one synchronised pressure/level measurement (and the
// accompanying temperature) into the column. It drives the phase-aware
// operating-envelope state machine, so control permits are decided by the
// de-bounced normal/limiting/holding/recovering state rather than a single
// hard-limit comparison. Restriction state changes are journaled with the
// signal and the trigger so operators can trace why an action is held.
func (p *Plant) IngestSignals(pressure, level, temperature float64, at int64) (EnvelopeStatus, error) {
	if math.IsNaN(pressure) || math.IsNaN(level) || math.IsNaN(temperature) {
		return EnvelopeStatus{}, errors.New("column signal is not numeric")
	}
	if at == 0 {
		at = time.Now().Unix()
	}
	current := p.Campaign.Snapshot()

	changed, moved := p.Envelope.Update(current.Phase, pressure, level, at)
	if !moved {
		return EnvelopeStatus{}, errors.New("column signal timestamp must advance")
	}

	// Reflect the measurement on the column state regardless of phase; the
	// envelope monitor is the authority for whether actions may run.
	p.Column.Apply(current, pressure, level)
	p.Column.SetTemperature(temperature)

	if changed.To != "" {
		payload := fmt.Sprintf("%s:%s:%s:%s",
			changed.From, changed.To, current.Phase, joinSignals(changed.Signals))
		if err := p.record("column.envelope", payload); err != nil {
			p.fail(err)
			return EnvelopeStatus{}, err
		}
	}
	return p.envelopeStatusLocked(), nil
}

// EnvelopeStatus returns the current restriction state and an energy permit.
func (p *Plant) EnvelopeStatus() EnvelopeStatus { return p.envelopeStatusLocked() }

func (p *Plant) envelopeStatusLocked() EnvelopeStatus {
	return EnvelopeStatus{
		Status: p.Envelope.Status(),
		Permit: p.Envelope.Permit(envelope.ActionEnergy),
	}
}

// requireEnergyPermit denies an energy-adding control action while the column
// is limiting, holding, or still inside its recovery window. The returned
// error names the state and every restricting signal so the API can show the
// operator exactly what is holding the action.
func (p *Plant) requireEnergyPermit(action string) (envelope.Permit, error) {
	permit := p.Envelope.Permit(envelope.ActionEnergy)
	if permit.Allowed {
		return permit, nil
	}
	return permit, fmt.Errorf("%s withheld by operating envelope (state=%s): %s",
		action, permit.State, describeRestrictions(permit.Restrictions))
}

func joinSignals(signals []envelope.Signal) string {
	if len(signals) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(signals))
	for _, s := range signals {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ",")
}

func describeRestrictions(restrictions []envelope.Restriction) string {
	if len(restrictions) == 0 {
		return "no signal inside the operating envelope"
	}
	parts := make([]string, 0, len(restrictions))
	for _, r := range restrictions {
		parts = append(parts, fmt.Sprintf(
			"%s %s band at edge %s since t=%d (trigger %.4f, latest %.4f)",
			r.Signal, r.Band, r.Edge, r.Since, r.Trigger.Value, r.Latest.Value))
	}
	return strings.Join(parts, "; ")
}
