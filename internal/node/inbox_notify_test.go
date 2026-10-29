package node

import (
	"context"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
)

func TestMemInboxWaiterKeyIncludesEndUser(t *testing.T) {
	in := NewMemInbox()
	ctx := context.Background()
	_, _, _ = in.Pending(ctx, "a:b", "node:12")
	in.mu.Lock()
	changed := in.boxes[nodeKey{"a:b", "node:12"}].changed
	in.mu.Unlock()
	if err := in.Put(ctx, "a", "node:12", &v1.Invoke{CallId: "c"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
		t.Fatal("another end user signaled the waiter")
	default:
	}
	if err := in.Put(ctx, "a:b", "node:12", &v1.Invoke{CallId: "c"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("own change did not signal the waiter")
	}
}
