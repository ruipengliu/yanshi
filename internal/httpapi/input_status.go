package httpapi

import (
	"net/http"

	"yanshi/internal/service"
)

func (s *Server) inputStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.Service.InputStatus(owned(r), r.PathValue("id"), r.URL.Query().Get("input_id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := struct {
		Found   bool   `json:"found"`
		AsOfSeq uint64 `json:"as_of_seq,string"`
		*inputStatusHit
	}{Found: status.Found, AsOfSeq: status.AsOfSeq}
	if status.Found {
		out.inputStatusHit = &inputStatusHit{
			InputID: status.InputID, RunID: status.RunID, EventID: status.EventID,
			AcceptedSeq: status.AcceptedSeq, AcceptanceKind: status.AcceptanceKind, AnsweredCallID: status.AnsweredCallID,
			RunStatus: status.RunStatus.String(), AttemptCount: status.AttemptCount, TakeoverCount: status.TakeoverCount,
			Calls: status.Calls, SinceAcceptance: status.SinceAcceptance,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type inputStatusHit struct {
	InputID         string                           `json:"input_id"`
	RunID           string                           `json:"run_id"`
	EventID         string                           `json:"event_id"`
	AcceptedSeq     uint64                           `json:"accepted_seq,string"`
	AcceptanceKind  string                           `json:"acceptance_kind"`
	AnsweredCallID  string                           `json:"answered_call_id,omitempty"`
	RunStatus       string                           `json:"run_status"`
	AttemptCount    uint32                           `json:"attempt_count"`
	TakeoverCount   uint32                           `json:"takeover_count"`
	Calls           service.InputCallEvidence        `json:"calls"`
	SinceAcceptance *service.InputAcceptanceEvidence `json:"since_acceptance,omitempty"`
}
