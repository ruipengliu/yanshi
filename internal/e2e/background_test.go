package e2e

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"yanshi/sdk/nodesdk"
)

// backgroundView 是 GET /v1/sessions/{id} 中未了结的后台调用（ADR-0035）。
type backgroundView struct {
	Background []struct {
		CallID     string `json:"call_id"`
		Capability string `json:"capability"`
		NodeOnline *bool  `json:"node_online"`
	} `json:"background"`
	Runs []struct {
		Status         string `json:"status"`
		FromBackground string `json:"from_background"`
	} `json:"runs"`
}

func (e *env) background(sid string) backgroundView {
	var v backgroundView
	e.do(http.MethodGet, "/v1/sessions/"+sid, nil, &v)
	return v
}

// untilBackground 轮询 Session 直到 cond 成立（后台调用要等宽限期，给足时间）。
func (e *env) untilBackground(sid string, cond func(backgroundView) bool) backgroundView {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		v := e.background(sid)
		if cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("condition not met; last view %+v", v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func idle(v backgroundView) bool {
	n := len(v.Runs)
	return n > 0 && v.Runs[n-1].Status == "completed"
}

// 设备离线超过宽限期：调用转入后台，Run 以"已排队"完成；期间用户的输入照常得到回应；设备上线执行后，结果开启新 Run
// 由模型报告。
func TestOfflineDeviceGoesToBackground(t *testing.T) {
	e := setup(t)
	ledger := nodesdk.NewMemLedger()
	e.startNode(ledger)() // 设备曾经上线，现在离线

	sid := e.newSession()
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "call macbook__read_file"}, nil)
	v := e.untilBackground(sid, func(v backgroundView) bool { return idle(v) && len(v.Background) == 1 })
	if b := v.Background[0]; b.Capability != "macbook__read_file" || b.NodeOnline == nil || *b.NodeOnline {
		t.Fatalf("background = %+v", b)
	}
	if got := e.lastAssistantText(sid); !strings.Contains(got, "queued") {
		t.Fatalf("assistant = %q", got)
	}

	// 对话不被阻塞：新的输入开启新 Run 并得到回应。
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "还在吗"}, nil)
	e.untilBackground(sid, func(v backgroundView) bool { return idle(v) && len(v.Runs) == 2 })
	if got := e.lastAssistantText(sid); got != "echo: 还在吗" {
		t.Fatalf("assistant = %q", got)
	}

	defer e.startNode(ledger)() // 设备上线，执行排队的调用
	v = e.untilBackground(sid, func(v backgroundView) bool { return idle(v) && len(v.Runs) == 3 && len(v.Background) == 0 })
	if v.Runs[2].FromBackground == "" {
		t.Fatalf("third run = %+v", v.Runs[2])
	}
	if got := e.lastAssistantText(sid); !strings.Contains(got, "[后台调用结果]") || !strings.Contains(got, "meeting notes") {
		t.Fatalf("assistant = %q", got)
	}
}

// 经 Connection 取消后台调用：设备上线后不执行，再次取消为 conflict。
func TestCancelBackgroundOverConnection(t *testing.T) {
	e := setup(t)
	ledger := nodesdk.NewMemLedger()
	e.startNode(ledger)()
	phone := e.startClient("phone", userToken, nil)
	ctx := context.Background()

	sid := e.newSession()
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "call macbook__write_file"}, nil)
	v := e.until(sid, func(v sessionView) bool {
		return len(v.Runs) > 0 && v.Runs[0].Waiting != nil && v.Runs[0].Waiting.Kind == "approval"
	})
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/approvals/"+v.Runs[0].Waiting.CallID, map[string]bool{"approve": true}, nil)
	bv := e.untilBackground(sid, func(v backgroundView) bool { return idle(v) && len(v.Background) == 1 })
	call := bv.Background[0].CallID

	if err := phone.CancelBackground(ctx, sid, call); err != nil {
		t.Fatal(err)
	}
	var re *nodesdk.RequestError
	if err := phone.CancelBackground(ctx, sid, call); !errors.As(err, &re) || re.Code != "conflict" {
		t.Fatalf("second cancel = %v", err)
	}
	if bv = e.background(sid); len(bv.Background) != 0 || len(bv.Runs) != 1 {
		t.Fatalf("after cancel: %+v", bv)
	}
	defer e.startNode(ledger)()
	time.Sleep(300 * time.Millisecond)
	if n := e.writes.Load(); n != 0 {
		t.Fatalf("cancelled call executed %d times", n)
	}
}

// 后台时限到期：撤回投递，新 Run 告诉用户没办成；设备之后上线也不执行。
func TestBackgroundTimeoutE2E(t *testing.T) {
	st := memStores()
	st.backgroundTimeout = 2 * time.Second
	e := &env{t: t, srv: instance(t, st, 2, true)}
	ledger := nodesdk.NewMemLedger()
	e.startNode(ledger)()

	sid := e.newSession()
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "call macbook__write_file"}, nil)
	v := e.until(sid, func(v sessionView) bool {
		return len(v.Runs) > 0 && v.Runs[0].Waiting != nil && v.Runs[0].Waiting.Kind == "approval"
	})
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/approvals/"+v.Runs[0].Waiting.CallID, map[string]bool{"approve": true}, nil)
	bv := e.untilBackground(sid, func(v backgroundView) bool { return idle(v) && len(v.Runs) == 2 && len(v.Background) == 0 })
	if bv.Runs[1].FromBackground == "" {
		t.Fatalf("runs = %+v", bv.Runs)
	}
	if got := e.lastAssistantText(sid); !strings.Contains(got, "[后台调用到期]") {
		t.Fatalf("assistant = %q", got)
	}
	defer e.startNode(ledger)()
	time.Sleep(300 * time.Millisecond)
	if n := e.writes.Load(); n != 0 {
		t.Fatalf("expired call executed %d times", n)
	}
}
