package node_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/clock"
	"yanshi/internal/node"
	"yanshi/internal/sandbox"
)

// 平台的虚拟 Node 必须落在保留范围内：设备若能以沙箱的 ID 接入，就能收到他人沙箱的调用并回写结果。
func TestPlatformNodeIDsAreReserved(t *testing.T) {
	for _, id := range []string{sandbox.NodeID("s_victim"), askuser.NodeID} {
		if !node.Reserved(id) {
			t.Errorf("%q is not reserved", id)
		}
	}
	if node.Reserved("node_ab12") {
		t.Error("an ordinary device id is reserved")
	}
}

func TestConnectRejectsReservedNodeIDs(t *testing.T) {
	h := &node.Hub{Dir: node.NewMemDirectory(clock.NewFake(time.Unix(0, 0))), Inbox: node.NewMemInbox(), Auth: node.InsecureDevAuth{}}
	for _, clientOnly := range []bool{false, true} {
		for _, id := range []string{sandbox.NodeID("s_victim"), askuser.NodeID} {
			_, err := h.Connect(context.Background(), &v1.Hello{NodeId: id, BusinessLine: "bl", EndUser: "mallory", ClientOnly: clientOnly,
				Capabilities: map[bool][]*v1.CapabilitySpec{false: {{Name: "read_file"}}}[clientOnly]})
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Errorf("connect as %q (client only %v): err %v, want reserved", id, clientOnly, err)
			}
		}
	}
	if _, err := h.Dir.Get(context.Background(), sandbox.NodeID("s_victim")); err != node.ErrNotFound {
		t.Fatalf("reserved id registered: %v", err)
	}
}

func TestSandboxLabelIsNotADeviceLabel(t *testing.T) {
	if got := node.SanitizeLabel("Sandbox"); got == "sandbox" {
		t.Fatalf("SanitizeLabel(Sandbox) = %q: device tools would be named like sandbox tools", got)
	}
}
