package nodesdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "yanshi/gen/yanshi/v1"
)

// 本文件把本机的 MCP Server 桥接为设备 Capability（docs/design/m5-mcp.md §5，ADR-0017）：
// 调用沿用设备节点的全部机制（Inbox、离线挂起、call_id 去重、审批），数据与凭证留在本机。

// MaxMCPText 是单个 MCP 结果中文本的上限；更长的部分截断并注明。
const MaxMCPText = 64 << 10

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// SafeName 把名字规范为工具名允许的字符 [a-zA-Z0-9_-]，且不超过 limit 字节。已合规的名字原样返回；
// 否则规范化、截断，并附加原名的短哈希，使 "a.b" 与 "a_b" 这类名字不会撞在一起。
func SafeName(s string, limit int) string {
	if s != "" && len(s) <= limit && !unsafeName.MatchString(s) {
		return s
	}
	clean := strings.Trim(unsafeName.ReplaceAllString(s, "_"), "_")
	if clean == "" {
		clean = "tool"
	}
	h := shortHash(s)
	if keep := limit - len(h) - 1; len(clean) > keep {
		clean = clean[:max(keep, 1)]
	}
	return clean + "_" + h
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:3])
}

// MCPRisk 把 MCP 工具注解映射为风险与幂等性：只有声明只读的工具免审批（安全默认）；
// 第三方 Server 的注解只当作提示，放宽审批由调用方另行决定。
func MCPRisk(t *mcp.Tool) (risk v1.Risk, idempotent bool) {
	a := t.Annotations
	if a != nil && a.ReadOnlyHint {
		return v1.Risk_RISK_LOW, true
	}
	return v1.Risk_RISK_HIGH, a != nil && a.IdempotentHint
}

// MCPDestructive 报告工具是否明确声明了 destructiveHint: true（未声明时规范默认为 true，但不算明确声明）。
func MCPDestructive(t *mcp.Tool) bool {
	return t.Annotations != nil && t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint
}

// Uploader 把二进制内容保存为工件并返回引用它的内容块；为 nil 时二进制内容只以描述文本呈现。
type Uploader func(ctx context.Context, name, mimeType string, data []byte) (*v1.ContentBlock, error)

func textBlock(s string) *v1.ContentBlock {
	return &v1.ContentBlock{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: strings.ToValidUTF8(s, "�")}}}
}

// MCPContent 把 MCP 调用结果映射为内容块（docs/design/m5-mcp.md §4）。第二个返回值表示是否为错误结果。
// 结果是不可信数据：只作为内容交给模型，不解释其中的任何指令。
func MCPContent(ctx context.Context, res *mcp.CallToolResult, upload Uploader) ([]*v1.ContentBlock, bool, error) {
	var out []*v1.ContentBlock
	textBytes := 0
	addText := func(s string) {
		if textBytes >= MaxMCPText {
			return
		}
		if textBytes+len(s) > MaxMCPText {
			n := MaxMCPText - textBytes
			for n > 0 && !utf8.RuneStart(s[n]) {
				n--
			}
			s = s[:n] + fmt.Sprintf("\n[已截断：结果超过 %d 字节]", MaxMCPText)
		}
		textBytes += len(s)
		out = append(out, textBlock(s))
	}
	binary := func(kind, name, mimeType string, data []byte) error {
		if upload == nil {
			addText(fmt.Sprintf("[%s: %s, %s, %d 字节，未保存]", kind, name, mimeType, len(data)))
			return nil
		}
		b, err := upload(ctx, name, mimeType, data)
		if err != nil {
			return err
		}
		out = append(out, b)
		return nil
	}
	for i, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			addText(c.Text)
		case *mcp.ImageContent:
			if err := binary("image", fmt.Sprintf("image-%d", i+1), c.MIMEType, c.Data); err != nil {
				return nil, false, err
			}
		case *mcp.AudioContent:
			if err := binary("audio", fmt.Sprintf("audio-%d", i+1), c.MIMEType, c.Data); err != nil {
				return nil, false, err
			}
		case *mcp.ResourceLink:
			addText(fmt.Sprintf("[资源 %s：%s %s %s]", c.URI, c.Name, c.MIMEType, c.Description))
		case *mcp.EmbeddedResource:
			if r := c.Resource; r != nil {
				if len(r.Blob) > 0 {
					name := r.URI
					if i := strings.LastIndex(name, "/"); i >= 0 {
						name = name[i+1:]
					}
					if err := binary("resource", name, r.MIMEType, r.Blob); err != nil {
						return nil, false, err
					}
				} else {
					addText(fmt.Sprintf("[资源 %s]\n%s", r.URI, r.Text))
				}
			}
		}
	}
	if res.StructuredContent != nil && len(res.Content) == 0 {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, false, err
		}
		addText(string(b))
	}
	if len(out) == 0 {
		out = append(out, textBlock("(无输出)"))
	}
	return out, res.IsError, nil
}

// MCPCapabilities 把一个本机 MCP Server 的工具声明为设备 Capability。能力名为 "<server>_<tool>"，
// 模型看到的工具名为 "<label>__<server>_<tool>"。只读工具免审批，其余需要 EndUser 审批。
// arts 非 nil 时，结果中的图片、音频上传为当前 Session 的工件。
func MCPCapabilities(ctx context.Context, server string, cs *mcp.ClientSession, arts *Artifacts) ([]Capability, error) {
	server = SafeName(server, 24)
	var caps []Capability
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools of %s: %w", server, err)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil || string(schema) == "null" {
			schema = []byte(`{"type":"object"}`)
		}
		risk, idem := MCPRisk(tool)
		name := tool.Name
		caps = append(caps, Capability{
			Spec: &v1.CapabilitySpec{Name: server + "_" + SafeName(name, 36), Description: tool.Description,
				InputSchemaJson: string(schema), Idempotent: idem, Risk: risk},
			Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
				res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
				if err != nil {
					return nil, err
				}
				var upload Uploader
				if inv := Invocation(ctx); arts != nil && inv != nil {
					upload = func(ctx context.Context, n, mt string, data []byte) (*v1.ContentBlock, error) {
						return arts.Upload(ctx, inv.GetSessionId(), n, mt, strings.NewReader(string(data)))
					}
				}
				content, isErr, err := MCPContent(ctx, res, upload)
				if err != nil {
					return nil, err
				}
				if isErr {
					return nil, fmt.Errorf("%s", contentText(content))
				}
				return content, nil
			},
		})
	}
	return caps, nil
}

func contentText(blocks []*v1.ContentBlock) string {
	var b strings.Builder
	for _, c := range blocks {
		b.WriteString(c.GetText().GetText())
	}
	return b.String()
}

// ConnectMCPCommand 启动一个 stdio 型本机 MCP Server（如 "npx -y @modelcontextprotocol/server-filesystem ~/Documents"）并连接。
func ConnectMCPCommand(ctx context.Context, command string) (*mcp.ClientSession, error) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty MCP server command")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "yanshi-node", Version: Version}, nil)
	return client.Connect(ctx, &mcp.CommandTransport{Command: exec.CommandContext(ctx, fields[0], fields[1:]...)}, nil)
}
