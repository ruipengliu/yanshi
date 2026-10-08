package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
)

// nodeCmd 模拟电脑上的 HostApp：把 root 目录下的文件能力作为 Node 接入。
func nodeCmd(args []string) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	server := fs.String("server", "ws://127.0.0.1:8080/v1/nodes/connect", "网关地址")
	root := fs.String("root", ".", "允许访问的目录")
	label := fs.String("label", hostname(), "设备标签")
	user := fs.String("user", "dev", "终端用户 ID")
	bl := fs.String("business-line", "dev", "业务线")
	state := fs.String("state", filepath.Join(userConfigDir(), "yanshi", "node"), "节点状态目录（node_id 与调用账本）")
	_ = fs.Parse(args)

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	nodeID, err := loadNodeID(*state)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	apiBase, err := nodesdk.APIBaseFromGateway(*server)
	if err != nil {
		return err
	}
	arts := &nodesdk.Artifacts{BaseURL: apiBase}
	exec := nodesdk.NewExecutor(nodesdk.FileLedger{Dir: filepath.Join(*state, "ledger")}, fileCapabilities(absRoot, arts)...)
	c := nodesdk.NewClient(nodesdk.Config{
		URL: *server, NodeID: nodeID, BusinessLine: *bl, EndUser: *user, Label: *label,
		Kind: "desktop", HostApp: "yanshi-node-demo", Executor: exec, Logger: logger,
		OnConnected: func(l string) { fmt.Printf("已上线：标签 %s，开放目录 %s\n", l, absRoot) },
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "desktop"
	}
	return strings.Split(h, ".")[0]
}

func userConfigDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return d
}

// loadNodeID 读取或生成持久的 node_id。
func loadNodeID(dir string) (string, error) {
	p := filepath.Join(dir, "node_id")
	if b, err := os.ReadFile(p); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	id := "node_" + hex.EncodeToString(b[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return id, os.WriteFile(p, []byte(id), 0o600)
}

func textBlocks(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

// within 把相对路径解析到 root 内，拒绝越界访问。
func within(root, rel string) (string, error) {
	p := filepath.Join(root, filepath.Clean("/"+rel))
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the shared directory", rel)
	}
	return p, nil
}

func fileCapabilities(root string, arts *nodesdk.Artifacts) []nodesdk.Capability {
	pathSchema := `{"type":"object","properties":{"path":{"type":"string","description":"相对于共享目录的路径"}},"required":["path"]}`
	parse := func(args string) (string, map[string]string, error) {
		var in map[string]string
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return "", nil, fmt.Errorf("invalid arguments: %w", err)
		}
		p, err := within(root, in["path"])
		return p, in, err
	}
	return []nodesdk.Capability{
		{
			Spec: &v1.CapabilitySpec{Name: "list_files", Description: "列出电脑共享目录中某个目录的文件。",
				InputSchemaJson: pathSchema, Idempotent: true, Risk: v1.Risk_RISK_LOW},
			Handler: func(_ context.Context, args string) ([]*v1.ContentBlock, error) {
				p, _, err := parse(args)
				if err != nil {
					return nil, err
				}
				entries, err := os.ReadDir(p)
				if err != nil {
					return nil, err
				}
				var b strings.Builder
				for _, e := range entries {
					suffix := ""
					if e.IsDir() {
						suffix = "/"
					}
					fmt.Fprintf(&b, "%s%s\n", e.Name(), suffix)
				}
				return textBlocks(b.String()), nil
			},
		},
		{
			Spec: &v1.CapabilitySpec{Name: "read_file", Description: "读取电脑共享目录中的文本文件（最多 64KB）。",
				InputSchemaJson: pathSchema, Idempotent: true, Risk: v1.Risk_RISK_LOW},
			Handler: func(_ context.Context, args string) ([]*v1.ContentBlock, error) {
				p, _, err := parse(args)
				if err != nil {
					return nil, err
				}
				f, err := os.Open(p)
				if err != nil {
					return nil, err
				}
				defer f.Close()
				b := make([]byte, 64<<10)
				n, _ := f.Read(b)
				return textBlocks(string(b[:n])), nil
			},
		},
		{
			Spec: &v1.CapabilitySpec{Name: "upload_file", Description: "把电脑共享目录中的文件（任意类型，如录音、表格）上传为 artifact，供云端沙箱处理。",
				InputSchemaJson: pathSchema, Idempotent: true, Risk: v1.Risk_RISK_LOW},
			Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
				p, in, err := parse(args)
				if err != nil {
					return nil, err
				}
				f, err := os.Open(p)
				if err != nil {
					return nil, err
				}
				defer f.Close()
				b, err := arts.Upload(ctx, nodesdk.Invocation(ctx).GetSessionId(), filepath.Base(in["path"]), "", f)
				if err != nil {
					return nil, err
				}
				return []*v1.ContentBlock{b}, nil
			},
		},
		{
			Spec: &v1.CapabilitySpec{Name: "save_artifact", Description: "把一个 artifact（如沙箱生成的图表、纪要）保存到电脑共享目录。需要用户审批。",
				InputSchemaJson: `{"type":"object","properties":{"artifact":{"type":"string"},"path":{"type":"string"}},"required":["artifact","path"]}`,
				Risk:            v1.Risk_RISK_HIGH},
			Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
				p, in, err := parse(args)
				if err != nil {
					return nil, err
				}
				body, _, sid, err := arts.Download(ctx, in["artifact"])
				if err != nil {
					return nil, err
				}
				defer body.Close()
				// 只接受本次调用所属 Session 的工件。
				if sid != nodesdk.Invocation(ctx).GetSessionId() {
					return nil, fmt.Errorf("artifact %s belongs to another session", in["artifact"])
				}
				f, err := os.Create(p)
				if err != nil {
					return nil, err
				}
				n, err := io.Copy(f, body)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return nil, err
				}
				return textBlocks(fmt.Sprintf("saved %d bytes to %s", n, in["path"])), nil
			},
		},
		{
			Spec: &v1.CapabilitySpec{Name: "write_file", Description: "在电脑共享目录中写入文本文件（覆盖）。需要用户审批。",
				InputSchemaJson: `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`,
				Risk:            v1.Risk_RISK_HIGH},
			Handler: func(_ context.Context, args string) ([]*v1.ContentBlock, error) {
				p, in, err := parse(args)
				if err != nil {
					return nil, err
				}
				if err := os.WriteFile(p, []byte(in["content"]), 0o644); err != nil {
					return nil, err
				}
				return textBlocks(fmt.Sprintf("wrote %d bytes to %s", len(in["content"]), in["path"])), nil
			},
		},
	}
}
