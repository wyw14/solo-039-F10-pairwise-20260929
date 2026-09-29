package api

import (
	"net/http"
	"time"
)

func (s *Server) transition(w http.ResponseWriter, r *http.Request) {
	var input transitionRequest
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	value, err := s.plant.Transition(r.Context(), input.Grade, input.Source)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, value)
}

func (s *Server) sample(w http.ResponseWriter, r *http.Request) {
	var input sampleRequest
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if input.At == 0 {
		input.At = time.Now().Unix()
	}
	count, err := s.plant.AddSample(input.Value, input.At)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "count": count})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"count": count})
}

func (s *Server) calibrate(w http.ResponseWriter, r *http.Request) {
	revision := s.plant.CalibrateAnalyzer()
	writeJSON(w, http.StatusAccepted, map[string]any{"calibration_revision": revision})
}

func (s *Server) routeProduct(w http.ResponseWriter, r *http.Request) {
	var input routeRequest
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.plant.RouteProduct(input.Target); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"target": input.Target})
}

func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	var input leaseRequest
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	value, err := s.plant.AcquireReflux(input.Owner, input.Amount)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, value)
}

// ingestSignals accepts the continuous pressure/level measurement stream. The
// response always carries the de-bounced envelope state, permit and the
// restrictions (signal + measured history) behind any hold, so an operator can
// see why a control action is currently withheld.
func (s *Server) ingestSignals(w http.ResponseWriter, r *http.Request) {
	var input signalsRequest
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	status, err := s.plant.IngestSignals(input.Pressure, input.Level, input.Temperature, input.At)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "envelope": s.plant.EnvelopeStatus()})
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func (s *Server) envelope(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.plant.EnvelopeStatus())
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	timeout, err := boolQuery(r, "relief_timeout")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	result, err := s.plant.Shutdown(r.Context(), timeout)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "result": result})
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}
