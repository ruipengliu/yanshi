package day

import (
	"context"
	"maps"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
)

type arkExecutionObservation struct {
	BusinessTime       time.Time                 `json:"business_time"`
	ToolResults        int                       `json:"tool_results"`
	ToolErrors         int                       `json:"tool_is_error"`
	NewToolErrors      int                       `json:"new_tool_is_error"`
	ToolErrorsByName   map[string]int            `json:"tool_errors_by_name"`
	ExecutionErrors    map[string]int            `json:"execution_errors"`
	NewExecutionErrors map[string]int            `json:"new_execution_errors"`
	HTTPStatus         map[string]map[string]int `json:"http_status"`
	TaskStates         map[string]int            `json:"task_states"`
	TaskLastErrors     map[string]int            `json:"task_last_error_classes"`
	TaskAttempts       int                       `json:"task_attempts"`
	TaskRetirements    int                       `json:"task_retirements"`
	Waiting            []arkWaitingTask          `json:"waiting_tasks"`
	ReadErrors         []string                  `json:"read_errors"`
}
type arkWaitingTask struct {
	ID         string    `json:"task_id"`
	Operation  string    `json:"operation"`
	State      string    `json:"state"`
	ErrorClass string    `json:"last_error_class"`
	RetryAt    time.Time `json:"retry_at"`
}
type arkExecutionTracker struct {
	seen                            map[string]uint64
	results, errors, previousErrors int
	byTool                          map[string]int
	previousExecution               map[string]int
}

// 已读事件只记序号和计数；擦除删日志后也保留之前观察到的错误数，不保存正文。
func (s *arkExecutionTracker) consume(events []*v1.Event) {
	if s.seen == nil {
		s.seen = map[string]uint64{}
		s.byTool = map[string]int{}
	}
	calls := map[string]string{}
	for _, event := range events {
		for _, call := range event.GetAssistantMessage().GetToolCalls() {
			calls[event.GetSessionId()+"/"+call.GetCallId()] = call.GetCapability()
		}
	}
	for _, event := range events {
		if event.GetSeq() <= s.seen[event.GetSessionId()] {
			continue
		}
		if result := event.GetToolResult(); result != nil {
			s.results++
			if result.GetIsError() {
				s.errors++
				name := calls[event.GetSessionId()+"/"+result.GetCallId()]
				if name == "" {
					name = "unknown"
				}
				s.byTool[name]++
			}
		}
		s.seen[event.GetSessionId()] = event.GetSeq()
	}
}
func (h *httpTrace) statuses() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for key, count := range h.counts {
		out[strconv.Itoa(key.Status)] += count
	}
	return out
}
func (e *environment) arkExecution(s *arkExecutionTracker) arkExecutionObservation {
	out := arkExecutionObservation{BusinessTime: e.control.Now().UTC(), TaskStates: map[string]int{}, TaskLastErrors: map[string]int{}, Waiting: []arkWaitingTask{}, ReadErrors: []string{}}
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	rows, err := e.kernelPool.Query(ctx, `SELECT session_id,seq,data FROM events ORDER BY session_id,seq`)
	if err != nil {
		out.ReadErrors = append(out.ReadErrors, "kernel_events")
	} else {
		var events []*v1.Event
		for rows.Next() {
			var sid string
			var seq uint64
			var data []byte
			if rows.Scan(&sid, &seq, &data) != nil {
				out.ReadErrors = append(out.ReadErrors, "kernel_event_scan")
				break
			}
			event := new(v1.Event)
			if proto.Unmarshal(data, event) != nil {
				out.ReadErrors = append(out.ReadErrors, "kernel_event_decode")
				break
			}
			event.SessionId, event.Seq = sid, seq
			events = append(events, event)
		}
		if rows.Err() != nil {
			out.ReadErrors = append(out.ReadErrors, "kernel_events_read")
		}
		rows.Close()
		s.consume(events)
	}
	out.ToolResults, out.ToolErrors = s.results, s.errors
	out.NewToolErrors = s.errors - s.previousErrors
	s.previousErrors = s.errors
	out.ToolErrorsByName = maps.Clone(s.byTool)
	e.control.mu.Lock()
	out.ExecutionErrors = maps.Clone(e.control.executionErrors)
	e.control.mu.Unlock()
	out.NewExecutionErrors = map[string]int{}
	for key, n := range out.ExecutionErrors {
		if delta := n - s.previousExecution[key]; delta > 0 {
			out.NewExecutionErrors[key] = delta
		}
	}
	s.previousExecution = maps.Clone(out.ExecutionErrors)
	out.HTTPStatus = map[string]map[string]int{"kernel": e.kernelHTTP.statuses(), "bff": e.bff.trace.statuses(), "persona": e.persona.trace.statuses()}
	tasks, err := e.appPool.Query(ctx, `SELECT task_id,operation,state,attempt,retirements,last_error_class,retry_at FROM orchestration_tasks WHERE business_line=$1 AND end_user=$2 ORDER BY sequence`, businessLine, e.endUser)
	if err != nil {
		out.ReadErrors = append(out.ReadErrors, "tasks")
	} else {
		for tasks.Next() {
			var job arkWaitingTask
			var attempts, retirements int
			if tasks.Scan(&job.ID, &job.Operation, &job.State, &attempts, &retirements, &job.ErrorClass, &job.RetryAt) != nil {
				out.ReadErrors = append(out.ReadErrors, "task_scan")
				break
			}
			out.TaskStates[job.State]++
			out.TaskAttempts += attempts
			out.TaskRetirements += retirements
			if job.ErrorClass != "" {
				out.TaskLastErrors[job.ErrorClass]++
			}
			if (job.State == "pending" || job.State == "unknown" || job.State == "accepted") && len(out.Waiting) < 50 {
				out.Waiting = append(out.Waiting, job)
			}
		}
		if tasks.Err() != nil {
			out.ReadErrors = append(out.ReadErrors, "tasks_read")
		}
		tasks.Close()
	}
	return out
}

// 诊断不参与旧版 outcome/stage 比较；旧基线无需填造新字段或升级版本。
func arkDifferences(previous, current arkReport) []string {
	out := []string{}
	for stage, value := range current.Stages {
		if !reflect.DeepEqual(previous.Stages[stage], value) {
			out = append(out, "stage:"+stage)
		}
	}
	for name, value := range current.Outcomes {
		if old, ok := previous.Outcomes[name]; !ok || old != value {
			out = append(out, "outcome:"+name)
		}
	}
	for stage := range previous.Stages {
		if _, ok := current.Stages[stage]; !ok {
			out = append(out, "missing-stage:"+stage)
		}
	}
	for name := range previous.Outcomes {
		if _, ok := current.Outcomes[name]; !ok {
			out = append(out, "missing-outcome:"+name)
		}
	}
	sort.Strings(out)
	return out
}
func TestArkDiagnosticsDoNotChangeComparison(t *testing.T) {
	before := arkReport{Version: 1, Stages: map[string]arkObservation{"phase": {Processed: true}}, Outcomes: map[string]bool{"review": true}}
	after := before
	after.Diagnostics = map[string]arkExecutionObservation{"phase": {ToolErrors: 2}}
	if diff := arkDifferences(before, after); len(diff) != 0 {
		t.Fatalf("diagnostics changed baseline comparison: %v", diff)
	}
	after.Outcomes = map[string]bool{"review": false}
	if diff := arkDifferences(before, after); !reflect.DeepEqual(diff, []string{"outcome:review"}) {
		t.Fatalf("outcome comparison changed: %v", diff)
	}
}
func TestArkToolDiagnosticsCountErrorsOnce(t *testing.T) {
	events := []*v1.Event{
		{SessionId: "ses_probe", Seq: 1, Payload: &v1.Event_AssistantMessage{AssistantMessage: &v1.AssistantMessage{ToolCalls: []*v1.ToolCall{{CallId: "call_probe", Capability: "mcp_xinli__file_day"}}}}},
		{SessionId: "ses_probe", Seq: 2, Payload: &v1.Event_ToolResult{ToolResult: &v1.ToolResult{CallId: "call_probe", IsError: true}}},
	}
	tracker := new(arkExecutionTracker)
	tracker.consume(events)
	tracker.consume(events)
	tracker.consume(nil)
	if tracker.results != 1 || tracker.errors != 1 || tracker.byTool["mcp_xinli__file_day"] != 1 {
		t.Fatal("tool errors duplicated or lost after deletion")
	}
}
