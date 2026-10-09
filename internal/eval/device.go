package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/node"
	"yanshi/sdk/nodesdk"
)

// device 是评测用的虚拟设备：以 Hub 直接接入（与模拟测试相同，不经 WebSocket），
// 提供只读文件与两个需要审批的副作用能力，并记录副作用供断言。
type device struct {
	mu     sync.Mutex
	files  map[string]string
	writes map[string]string
	sent   int
}

func text(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

func (d *device) capabilities() []nodesdk.Capability {
	str := `{"type":"object","properties":{%s},"required":[%s]}`
	return []nodesdk.Capability{
		{Spec: &v1.CapabilitySpec{Name: "list_files", Description: "列出电脑共享目录中的文件。", Idempotent: true, Risk: v1.Risk_RISK_LOW,
			InputSchemaJson: `{"type":"object"}`},
			Handler: func(context.Context, string) ([]*v1.ContentBlock, error) {
				d.mu.Lock()
				defer d.mu.Unlock()
				var names []string
				for n := range d.files {
					names = append(names, n)
				}
				sort.Strings(names)
				return text(strings.Join(names, "\n")), nil
			}},
		{Spec: &v1.CapabilitySpec{Name: "read_file", Description: "读取电脑共享目录中的文本文件。", Idempotent: true, Risk: v1.Risk_RISK_LOW,
			InputSchemaJson: fmt.Sprintf(str, `"path":{"type":"string"}`, `"path"`)},
			Handler: func(_ context.Context, args string) ([]*v1.ContentBlock, error) {
				var in struct{ Path string }
				_ = json.Unmarshal([]byte(args), &in)
				d.mu.Lock()
				defer d.mu.Unlock()
				body, ok := d.files[strings.TrimPrefix(in.Path, "./")]
				if !ok {
					return nil, fmt.Errorf("no such file: %s", in.Path)
				}
				return text(body), nil
			}},
		{Spec: &v1.CapabilitySpec{Name: "write_file", Description: "在电脑共享目录中写入文本文件。", Idempotent: true, Risk: v1.Risk_RISK_HIGH,
			InputSchemaJson: fmt.Sprintf(str, `"path":{"type":"string"},"content":{"type":"string"}`, `"path","content"`)},
			Handler: func(_ context.Context, args string) ([]*v1.ContentBlock, error) {
				var in struct{ Path, Content string }
				_ = json.Unmarshal([]byte(args), &in)
				d.mu.Lock()
				defer d.mu.Unlock()
				p := strings.TrimPrefix(in.Path, "./")
				d.writes[p], d.files[p] = in.Content, in.Content
				return text(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), p)), nil
			}},
		{Spec: &v1.CapabilitySpec{Name: "send_message", Description: "以用户的身份发送消息或邮件。", Risk: v1.Risk_RISK_HIGH,
			InputSchemaJson: fmt.Sprintf(str, `"to":{"type":"string"},"text":{"type":"string"}`, `"to","text"`)},
			Handler: func(context.Context, string) ([]*v1.ContentBlock, error) {
				d.mu.Lock()
				defer d.mu.Unlock()
				d.sent++
				return text("sent"), nil
			}},
	}
}

// run 把设备接入 Hub，并持续执行投递给它的调用，直到 ctx 结束。
func (d *device) run(ctx context.Context, hub *node.Hub, nodeID, businessLine, endUser, label string) error {
	exec := nodesdk.NewExecutor(nodesdk.NewMemLedger(), d.capabilities()...)
	var caps []*v1.CapabilitySpec
	for _, c := range d.capabilities() {
		caps = append(caps, c.Spec)
	}
	conn, err := hub.Connect(ctx, &v1.Hello{NodeId: nodeID, BusinessLine: businessLine, EndUser: endUser, Label: label, Kind: "desktop", Capabilities: caps})
	if err != nil {
		return err
	}
	go func() {
		defer hub.Disconnect(context.WithoutCancel(ctx), conn)
		for ctx.Err() == nil {
			pending, version, err := hub.Inbox.Pending(ctx, nodeID)
			if err != nil {
				return
			}
			for _, inv := range pending {
				res, err := exec.Execute(ctx, inv)
				if err != nil || res == nil {
					continue
				}
				_ = hub.Result(ctx, nodeID, res)
			}
			if len(pending) == 0 {
				_ = hub.Inbox.Wait(ctx, nodeID, version)
			}
		}
	}()
	return nil
}

func (d *device) snapshot() (map[string]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	w := map[string]string{}
	for k, v := range d.writes {
		w[k] = v
	}
	return w, d.sent
}
