package column

import (
	"github.com/google/uuid"
	"github.com/wyw14/solo-039/internal/model"
	"sync"
)

type State struct {
	mu    sync.RWMutex
	value model.Column
}

func New(id string) *State {
	return &State{value: model.Column{ID: id, Phase: model.Preparing, FeedRevision: uuid.New()}}
}

func (s *State) Snapshot() model.Column { s.mu.RLock(); defer s.mu.RUnlock(); return s.value }

func (s *State) Apply(c model.Campaign, pressure, level float64) {
	s.mu.Lock()
	s.value.Pressure = pressure
	s.value.Level = level
	s.value.Phase = c.Phase
	s.value.Fencing = c.Fencing
	s.mu.Unlock()
}

func (s *State) SetFeedRevision(revision uuid.UUID) {
	s.mu.Lock()
	s.value.FeedRevision = revision
	s.mu.Unlock()
}

func (s *State) SetTemperature(temperature float64) {
	s.mu.Lock()
	s.value.Temperature = temperature
	s.mu.Unlock()
}

func (s *State) Safe() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value.Pressure < 2.0 && s.value.Level >= 0 && s.value.Level <= 1
}
