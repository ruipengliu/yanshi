package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"yanshi/internal/eventlog"
	"yanshi/internal/lifecycle"
	"yanshi/internal/sandbox"
	sandboxdocker "yanshi/internal/sandbox/docker"
	"yanshi/sdk/nodesdk"
)

// doAs 以指定令牌发请求（serviceToken 为业务线服务令牌）。
func (e *env) doAs(token, method, path string, body any, out any) int {
	e.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func serviceToken(t *testing.T) string {
	tok, err := blKey.Sign("yanshi", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// lifecycleEnv 启动带沙箱（设置 YANSHI_TEST_DOCKER 时为真实 Docker，否则为内存实现）与设备 Node 的实例。
func lifecycleEnv(t *testing.T) (*env, stores, *nodesdk.MemLedger, func(id string) bool) {
	st := memStores()
	exists := func(string) bool { return false }
	if os.Getenv("YANSHI_TEST_DOCKER") != "" {
		st.provider = &sandboxdocker.Provider{Image: "python:3.12-slim", MemoryMB: 256}
		name := regexp.MustCompile(`[^a-zA-Z0-9_.-]`)
		exists = func(id string) bool {
			return exec.Command("docker", "volume", "inspect", "yanshi-ws-"+name.ReplaceAllString(id, "-")).Run() == nil
		}
	} else {
		fake := sandbox.NewFake()
		st.provider, exists = fake, fake.Exists
	}
	e := &env{t: t, srv: instance(t, st, 2, true)}
	ledger := nodesdk.NewMemLedger()
	t.Cleanup(e.startNode(ledger))
	return e, st, ledger, exists
}

func (e *env) run(sid, input string) {
	e.t.Helper()
	var before sessionView
	e.do(http.MethodGet, "/v1/sessions/"+sid, nil, &before)
	e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": input}, nil)
	e.until(sid, func(v sessionView) bool {
		return len(v.Runs) == len(before.Runs)+1 && v.Runs[len(v.Runs)-1].Status == "completed"
	})
}

// TestDeleteSessionRemovesEverything：Session 用过设备、沙箱与工件；删除后立即不可见，
// 后台清理完成后日志、索引、工件、沙箱工作区（Docker 卷）、设备端账本中都不再有它的数据。
func TestDeleteSessionRemovesEverything(t *testing.T) {
	e, st, deviceLedger, workspaceExists := lifecycleEnv(t)
	ctx := context.Background()
	sid := e.newSession()
	sbx := sandbox.NodeID(sid)
	t.Cleanup(func() { _ = st.provider.Destroy(context.Background(), sbx) })

	e.run(sid, "call macbook__read_file")
	e.run(sid, `call __exec {"command":"echo secret > note.txt"}`)
	if code := e.do(http.MethodPost, "/v1/sessions/"+sid+"/artifacts?name=a.txt", "personal data", nil); code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	if !deviceLedger.HasSession(sid) || !workspaceExists(sbx) {
		t.Fatalf("setup: device ledger = %v, workspace = %v", deviceLedger.HasSession(sid), workspaceExists(sbx))
	}

	if code := e.do(http.MethodDelete, "/v1/sessions/"+sid, nil, nil); code != http.StatusAccepted {
		t.Fatalf("delete: %d", code)
	}
	if code := e.do(http.MethodGet, "/v1/sessions/"+sid, nil, nil); code != http.StatusNotFound {
		t.Fatalf("deleted session still visible: %d", code)
	}
	eventually(t, "deletion to complete and the device to forget", 30*time.Second, func() bool {
		tb, err := st.deletions.Get(ctx, sid)
		return err == nil && !tb.CompletedAt.IsZero() && !deviceLedger.HasSession(sid)
	})
	if events, _ := eventlog.ReadAll(ctx, st.log, sid, 0); len(events) != 0 {
		t.Errorf("log still has %d events", len(events))
	}
	if _, err := st.index.Get(ctx, sid); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Errorf("session still indexed: %v", err)
	}
	if list, _ := st.artifacts.List(ctx, sid); len(list) != 0 {
		t.Errorf("%d artifacts remain", len(list))
	}
	if workspaceExists(sbx) {
		t.Errorf("sandbox workspace %s still exists", sbx)
	}
	if l, ok := st.ledger.(*nodesdk.MemLedger); ok && l.HasSession(sid) {
		t.Error("sandbox ledger still has records")
	}
}

// TestCloseDestroysWorkspaceButKeepsHistory：关闭后不再接受输入、销毁沙箱工作区，对话仍可查看。
func TestCloseDestroysWorkspaceButKeepsHistory(t *testing.T) {
	e, st, _, workspaceExists := lifecycleEnv(t)
	sid := e.newSession()
	sbx := sandbox.NodeID(sid)
	t.Cleanup(func() { _ = st.provider.Destroy(context.Background(), sbx) })
	e.run(sid, `call __exec {"command":"echo hi > x"}`)

	if code := e.do(http.MethodPost, "/v1/sessions/"+sid+"/close", nil, nil); code != http.StatusNoContent {
		t.Fatalf("close: %d", code)
	}
	if code := e.do(http.MethodPost, "/v1/sessions/"+sid+"/inputs", map[string]string{"text": "more"}, nil); code != http.StatusConflict {
		t.Fatalf("input after close: %d, want 409", code)
	}
	if got := e.lastAssistantText(sid); !strings.HasPrefix(got, "done:") {
		t.Fatalf("history not readable after close: %q", got)
	}
	eventually(t, "workspace to be destroyed", 30*time.Second, func() bool { return !workspaceExists(sbx) })

	var list struct {
		Sessions []struct {
			SessionID string     `json:"session_id"`
			ClosedAt  *time.Time `json:"closed_at"`
		} `json:"sessions"`
	}
	e.do(http.MethodGet, "/v1/sessions", nil, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != sid || list.Sessions[0].ClosedAt == nil {
		t.Fatalf("list = %+v", list)
	}
}

// TestDeleteEndUser：注销账号删除该 EndUser 的全部 Session 与 Node 登记，进度可查询。
func TestDeleteEndUser(t *testing.T) {
	e, st, _, _ := lifecycleEnv(t)
	a, b := e.newSession(), e.newSession()
	e.run(a, "call macbook__read_file")
	e.run(b, "call macbook__read_file")
	svcTok := serviceToken(t)

	var del struct {
		RequestID string `json:"request_id"`
	}
	if code := e.doAs(svcTok, http.MethodDelete, "/v1/end_users/u", nil, &del); code != http.StatusAccepted || del.RequestID == "" {
		t.Fatalf("delete end user: %d %+v", code, del)
	}
	if code := e.do(http.MethodDelete, "/v1/end_users/u", nil, nil); code != http.StatusForbidden {
		t.Fatalf("user token deleting an end user: %d, want 403", code)
	}
	// Node 登记随请求同步删除。设备若仍持有有效令牌，重连时会重新登记，因此业务线应先让该用户的令牌失效。
	var nodes struct {
		Nodes []any `json:"nodes"`
	}
	e.doAs(svcTok, http.MethodGet, "/v1/nodes?end_user=u", nil, &nodes)
	if len(nodes.Nodes) != 0 {
		t.Fatalf("nodes still registered: %v", nodes.Nodes)
	}
	var progress struct {
		Sessions, Completed int
		Done                bool
	}
	eventually(t, "account deletion to complete", 30*time.Second, func() bool {
		e.doAs(svcTok, http.MethodGet, "/v1/deletions/"+del.RequestID, nil, &progress)
		return progress.Done
	})
	if progress.Sessions != 2 || progress.Completed != 2 {
		t.Fatalf("progress = %+v", progress)
	}
	for _, sid := range []string{a, b} {
		if events, _ := eventlog.ReadAll(context.Background(), st.log, sid, 0); len(events) != 0 {
			t.Errorf("session %s still has events", sid)
		}
	}
}
