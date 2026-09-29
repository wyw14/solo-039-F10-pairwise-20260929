package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wyw14/solo-039/internal/envelope"
	"github.com/wyw14/solo-039/internal/model"
)

func startedPlant(t *testing.T) *Plant {
	t.Helper()
	p := NewPlant(t.TempDir())
	if _, err := p.Start(context.Background(), "baseline", "primary"); err != nil {
		t.Fatalf("start: %v", err)
	}
	return p
}

func signal(t *testing.T, p *Plant, phase model.Phase, pressure, level float64, at int64) envelope.State {
	t.Helper()
	status, err := p.IngestSignals(pressure, level, 100, at)
	if err != nil {
		t.Fatalf("ingest at %d: %v", at, err)
	}
	return status.Status.State
}

// End-to-end: chatter near the pressure guard edge must not flip the reflux
// permit; only a confirmed excursion restricts it, and the action returns after
// the stable recovery window.
func TestEnvelopeGatesRefluxAcrossChatterAndRecovery(t *testing.T) {
	p := startedPlant(t)
	base := time.Now().Unix() + 10
	phase := p.Campaign.Snapshot().Phase // stabilizing after start

	// Stabilizing: guard-high pressure 1.784, hard 2.0, restore-high 1.532.
	const (
		normalP   = 1.10
		guardP    = 1.90
		restoredP = 1.10
		level     = 0.5
	)

	// Single guard kisses interleaved with normal readings: permit stays open.
	at := base
	signal(t, p, phase, guardP, level, at)
	at++
	if state := signal(t, p, phase, normalP, level, at); state != envelope.StateNormal {
		t.Fatalf("one guard sample must not restrict, got %s", state)
	}
	for i := 0; i < 6; i++ {
		at++
		v := normalP
		if i%2 == 0 {
			v = guardP
		}
		if state := signal(t, p, phase, v, level, at); state != envelope.StateNormal {
			t.Fatalf("chatter restricted at step %d: %s", i, state)
		}
	}
	if _, err := p.AcquireReflux("T-101", 0.1); err != nil {
		t.Fatalf("energy action should be permitted while normal: %v", err)
	}

	// Two consecutive guard samples confirm the restriction.
	at++
	signal(t, p, phase, guardP, level, at)
	at++
	if state := signal(t, p, phase, guardP, level, at); state != envelope.StateLimiting {
		t.Fatalf("expected limiting, got %s", state)
	}
	_, err := p.AcquireReflux("T-101", 0.1)
	if err == nil {
		t.Fatal("reflux must be withheld while limiting")
	}
	if !strings.Contains(err.Error(), "pressure") || !strings.Contains(err.Error(), "envelope") {
		t.Fatalf("denial must name the signal and envelope, got: %v", err)
	}

	// Three restored samples are still inside the recovery window.
	for i := 0; i < 3; i++ {
		at++
		if state := signal(t, p, phase, restoredP, level, at); state != envelope.StateRecovering {
			t.Fatalf("expected recovering at step %d, got %s", i, state)
		}
	}
	if _, err := p.AcquireReflux("T-101", 0.1); err == nil {
		t.Fatal("reflux must stay withheld until the stable window completes")
	}

	// Fourth consecutive in-band sample closes the 3s window.
	at++
	if state := signal(t, p, phase, restoredP, level, at); state != envelope.StateNormal {
		t.Fatalf("expected normal after stable window, got %s", state)
	}
	if _, err := p.AcquireReflux("T-101", 0.1); err != nil {
		t.Fatalf("reflux should be released after recovery: %v", err)
	}
}

func TestEnvelopeHardLimitDeniesImmediately(t *testing.T) {
	p := startedPlant(t)
	base := time.Now().Unix() + 10
	state := signal(t, p, model.Stabilizing, 2.01, 0.5, base)
	if state != envelope.StateHolding {
		t.Fatalf("hard pressure crossing must hold immediately, got %s", state)
	}
	if _, err := p.AcquireReflux("T-101", 0.1); err == nil {
		t.Fatal("reflux must be denied while holding")
	}
}

func TestEnvelopeStatusExposesSignalAndHistory(t *testing.T) {
	p := startedPlant(t)
	base := time.Now().Unix() + 10
	signal(t, p, model.Stabilizing, 1.9, 0.5, base)
	signal(t, p, model.Stabilizing, 1.9, 0.5, base+1) // confirms limiting

	status := p.EnvelopeStatus()
	if len(status.Status.Restrictions) != 1 {
		t.Fatalf("want one restriction, got %d", len(status.Status.Restrictions))
	}
	r := status.Status.Restrictions[0]
	if r.Signal != envelope.Pressure {
		t.Fatalf("want pressure restriction, got %s", r.Signal)
	}
	if len(r.History) < 2 {
		t.Fatalf("restriction must carry the measured history segment, got %d points", len(r.History))
	}
}
