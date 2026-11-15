package delegation_test

import (
	"context"
	"errors"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/clock"
	"yanshi/internal/delegation"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/service"
	"yanshi/internal/session"
)

func TestDispatchUsesParentEndUserAndRetriesSameChild(t *testing.T) {
	ctx := context.Background()
	// 父 Session 由平台生成或由调用方指定（带版本、与 EndUser 匹配），两种情况的派生都一致。
	for _, specified := range []bool{false, true} {
		stores := app.Memory(clock.Real{})
		store := &session.Store{Log: stores.Log, Waking: stores.Waking, Clock: clock.Real{}, IDs: ids.Sequential("id")}
		agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "a", Version: "1", Model: "echo/any"})
		if err != nil {
			t.Fatal(err)
		}
		svc := &service.Service{Store: store, Agents: agents, Index: stores.Index, Deletions: stores.Deletions}
		req := service.CreateRequest{BusinessLine: "bl", EndUser: "real-user", Agent: "a"}
		if specified {
			if req.SessionID, err = ids.SessionID("specified", req.EndUser); err != nil {
				t.Fatal(err)
			}
		}
		parent, err := svc.Create(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		child, err := delegation.ChildSessionID(parent, "external", req.EndUser)
		if err != nil {
			t.Fatal(err)
		}
		d := &delegation.Delegator{Service: svc}
		inv := &v1.Invoke{SessionId: parent, RunId: "run", CallId: "external", Capability: "a", ArgumentsJson: `{"task":"work"}`}
		for range 2 {
			if err := d.Dispatch(ctx, delegation.NodeID(child), inv); err != nil {
				t.Fatal(err)
			}
		}
		st, err := store.Load(ctx, child)
		if err != nil || st.Created.GetEndUser() != req.EndUser || st.Created.GetBusinessLine() != req.BusinessLine || st.Created.GetParent().GetSessionId() != parent {
			t.Fatalf("child ownership: %+v, %v", st, err)
		}
		events, err := eventlog.ReadAll(ctx, store.Log, child, 0)
		if err != nil {
			t.Fatal(err)
		}
		creates, runs := 0, 0
		for _, e := range events {
			if e.GetSessionCreated() != nil {
				creates++
			}
			if e.GetRunRequested() != nil {
				runs++
			}
		}
		if creates != 1 || runs != 1 {
			t.Fatalf("retry created %d sessions and %d runs", creates, runs)
		}
		// 伪造的路由目标不能改掉从父投影读取的归属，即使目标是语法合法的新 ID。
		foreign, _ := ids.SessionID("foreign", "other-user")
		if err := d.Dispatch(ctx, delegation.NodeID(foreign), inv); !errors.Is(err, service.ErrInvalid) {
			t.Fatalf("foreign target accepted: %v", err)
		}
		if st, err := store.Load(ctx, foreign); err != nil || st.Created != nil {
			t.Fatalf("foreign child was created: %+v, %v", st, err)
		}
	}
}
