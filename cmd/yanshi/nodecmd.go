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
	server := fs.String("server", "ws://127.0.0.1:8080/v1/connect", "网关地址")
	root := fs.String("root", ".", "允许访问的目录")
	label := fs.String("label", hostname(), "设备标签")
	user := fs.String("user", "dev", "终端用户 ID")
	bl := fs.String("business-line", "dev", "业务线")
	state := fs.String("state", filepath.Join(userConfigDir(), "yanshi", "node"), "节点状态目录（node_id 与调用账本）")
	var mcpServers multiFlag
	fs.Var(&mcpServers, "mcp", "把本机 stdio 型 MCP Server 桥接为设备能力，格式 name=command，可重复（docs/design/m5-mcp.md §5）")
	tf := addTokenFlags(fs)
	_ = fs.Parse(args)

	token, businessLine, err := tf.source(*bl, *user)
	if err != nil {
		return err
	}

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
	arts := &nodesdk.Artifacts{BaseURL: apiBase, TokenSource: token}
	caps := fileCapabilities(absRoot, arts)
	for _, spec := range mcpServers {
		name, command, ok := strings.Cut(spec, "=")
		if !ok {
			return fmt.Errorf("-mcp %q: want name=command", spec)
		}
		cs, err := nodesdk.ConnectMCPCommand(context.Background(), command)
		if err != nil {
			return fmt.Errorf("start MCP server %s: %w", name, err)
		}
		defer cs.Close()
		mcpCaps, err := nodesdk.MCPCapabilities(context.Background(), name, cs, arts)
		if err != nil {
			return err
		}
		fmt.Printf("已桥接本机 MCP Server %s：%d 个工具\n", name, len(mcpCaps))
		caps = append(caps, mcpCaps...)
	}
	exec := nodesdk.NewExecutor(nodesdk.FileLedger{Dir: filepath.Join(*state, "ledger")}, caps...)
	c := nodesdk.NewClient(nodesdk.Config{
		URL: *server, NodeID: nodeID, TokenSource: token, BusinessLine: businessLine, EndUser: *user, Label: *label,
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
				b := make([]byte, nodesdk.ReadLimit)
				n, _ := io.ReadFull(f, b)
				body := strings.ToValidUTF8(string(b[:n]), "\uFFFD")
				if fi, err := f.Stat(); err == nil && fi.Size() > int64(n) {
					body += nodesdk.TruncatedNotice(fi.Size())
				}
				return textBlocks(body), nil
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

// multiFlag 收集可重复的字符串参数。
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
