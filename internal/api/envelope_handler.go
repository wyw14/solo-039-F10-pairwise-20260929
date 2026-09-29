package api

import "net/http"

// columnSignals 接收连续塔压/液位实测，推进分阶段包络状态机，
// 返回当前限制/保持/恢复状态以及导致该状态的信号与历史归因。
func (s *Server) columnSignals(w http.ResponseWriter, r *http.Request) {
	var input columnSignalsRequest
	if err := decodeJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	status, err := s.plant.ObserveColumnSignals(input.Pressure, input.Level, input.Temperature)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "envelope": status})
		return
	}
	code := http.StatusAccepted
	writeJSON(w, code, map[string]any{"envelope": status})
}

// envelope 返回当前可操作包络状态，供操作员查看是哪项信号、哪段历史导致当前限制。
func (s *Server) envelope(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"envelope": s.plant.ColumnEnvelopeStatus(),
		"history":  s.plant.ColumnEnvelopeHistory(),
	})
}
