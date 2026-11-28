package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/model"
	"yanshi/internal/service"
)

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text          json.RawMessage `json:"text"`
		Input         json.RawMessage `json:"input"`
		Redo          bool            `json:"redo"`
		RedoOf        json.RawMessage `json:"redo_of"`
		ExpectedRunID string          `json:"expected_run_id"`
		Schedule      json.RawMessage `json:"schedule"`
		// Content 是 ContentBlock 的 JSON 数组；与 Text 同时给出时 Text 在前。
		Content json.RawMessage `json:"content"`
		// InputID 是客户端生成的输入 ID：超时等结果未知时以同一 ID 重试是安全的（docs/design/m3-duplex-channel.md §5）。
		InputID string `json:"input_id"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if len(req.Schedule) > 0 {
		s.fail(w, service.CheckClientInput(nil, &v1.ScheduleMark{}))
		return
	}
	if len(req.RedoOf) > 0 {
		s.fail(w, service.CheckClientInput(nil, nil, "client"))
		return
	}
	var res *service.SubmitResult
	var err error
	if req.Redo {
		if len(req.Text) > 0 || len(req.Content) > 0 || len(req.Input) > 0 {
			s.fail(w, fmt.Errorf("%w: redo and input content are mutually exclusive", service.ErrInvalid))
			return
		}
		res, err = s.Service.Redo(owned(r), r.PathValue("id"), req.InputID, req.ExpectedRunID)
	} else {
		if req.ExpectedRunID != "" {
			s.fail(w, fmt.Errorf("%w: expected_run_id requires redo", service.ErrInvalid))
			return
		}
		var text string
		var content []json.RawMessage
		if len(req.Text) > 0 {
			if err := json.Unmarshal(req.Text, &text); err != nil {
				s.fail(w, fmt.Errorf("%w: text must be a string", service.ErrInvalid))
				return
			}
		}
		if len(req.Content) > 0 {
			if err := json.Unmarshal(req.Content, &content); err != nil {
				s.fail(w, fmt.Errorf("%w: content must be an array", service.ErrInvalid))
				return
			}
		}
		var input []*v1.ContentBlock
		if text != "" {
			input = model.TextBlocks(text)
		}
		for _, raw := range content {
			b := &v1.ContentBlock{}
			if err := protojson.Unmarshal(raw, b); err != nil {
				s.fail(w, fmt.Errorf("%w: content: %v", service.ErrInvalid, err))
				return
			}
			input = append(input, b)
		}
		res, err = s.Service.SubmitWithID(owned(r), r.PathValue("id"), req.InputID, input)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"run_id": res.RunID, "steered": res.Steered}
	if res.Answered != "" {
		out["answered"] = res.Answered
	}
	if res.Duplicate {
		out["duplicate"] = true
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.Service.Interrupt(owned(r), r.PathValue("id"), r.PathValue("run")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var req askuser.Answer
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Service.Answer(owned(r), r.PathValue("id"), r.PathValue("call"), &req); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cancelBackground 取消一个未了结的后台调用（ADR-0035）：设备上线后不再执行，不开启新 Run。已了结时 409。
func (s *Server) cancelBackground(w http.ResponseWriter, r *http.Request) {
	if err := s.Service.CancelBackground(owned(r), r.PathValue("id"), r.PathValue("call"), actor(principal(r))); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Approve *bool  `json:"approve"`
		By      string `json:"by"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if req.Approve == nil {
		s.fail(w, fmt.Errorf("%w: approve is required", service.ErrInvalid))
		return
	}
	// 审批记录的决定者取自令牌，而不是请求中自报的值。
	if p := principal(r); !p.Unrestricted || req.By == "" {
		req.By = actor(p)
	}
	if err := s.Service.Decide(owned(r), r.PathValue("id"), r.PathValue("call"), *req.Approve, req.By); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
