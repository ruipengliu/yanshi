package bff

import (
	"encoding/json"
	"net/http"
	"strings"

	"persona/query"
	"xinli/kernel"
)

type pendingItem struct {
	RunID string `json:"run_id"`
	kernel.Waiting
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (o Options) pending(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		fail(w, query.ErrInvalid)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	owner, id := ownerOf(r), r.PathValue("id")
	view, err := o.threadView(r.Context(), owner, id)
	if err != nil {
		fail(w, err)
		return
	}
	kind := "approval"
	if strings.HasSuffix(r.URL.Path, "/questions") {
		kind = "question"
	}
	wanted := map[string]bool{}
	for _, run := range view.Runs {
		if run.Waiting != nil && run.Waiting.Kind == kind {
			wanted[run.Waiting.CallID] = true
		}
	}
	arguments := map[string]json.RawMessage{}
	var after uint64
	// 当前等待资格由内核投影决定，显示参数从已提交事件读取，不在 BFF 复制执行状态机。
	for len(wanted) > 0 {
		page, err := o.Kernel.Events(r.Context(), owner.BusinessLine, id, after, 100)
		if err != nil {
			fail(w, err)
			return
		}
		for _, raw := range page.Events {
			var event struct {
				Assistant *struct {
					Calls []struct {
						ID        string `json:"call_id"`
						Arguments string `json:"arguments_json"`
					} `json:"tool_calls"`
				} `json:"assistant_message"`
			}
			if json.Unmarshal(raw, &event) != nil || event.Assistant == nil {
				continue
			}
			for _, call := range event.Assistant.Calls {
				if wanted[call.ID] {
					if !json.Valid([]byte(call.Arguments)) {
						fail(w, &query.Error{Status: 503, Code: "unavailable"})
						return
					}
					arguments[call.ID] = json.RawMessage(call.Arguments)
					delete(wanted, call.ID)
				}
			}
		}
		if len(page.Events) < 100 {
			break
		}
		if page.Last <= after {
			fail(w, &query.Error{Status: 503, Code: "unavailable"})
			return
		}
		after = page.Last
	}
	view, err = o.threadView(r.Context(), owner, id)
	if err != nil {
		fail(w, err)
		return
	}
	out := []pendingItem{}
	for _, run := range view.Runs {
		if run.Waiting != nil && run.Waiting.Kind == kind {
			if len(arguments[run.Waiting.CallID]) == 0 {
				fail(w, &query.Error{Status: 409, Code: "thread_changed"})
				return
			}
			out = append(out, pendingItem{RunID: run.ID, Waiting: *run.Waiting, Arguments: arguments[run.Waiting.CallID]})
		}
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 200, map[string]any{"items": out})
}
