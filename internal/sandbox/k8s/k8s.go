// Package k8s 是生产用的 sandbox.Provider：每个沙箱一个 Pod，隔离运行时经 RuntimeClass 选择
// （默认 gVisor，有 KVM 时可选 Kata；ADR-0053 的选型结论，docs/design/m2-sandbox.md §7）。
//
// 工作区运行期间是 Pod 的 emptyDir；Stop 把它打包写入对象存储后删除 Pod，Ensure 创建 Pod 后恢复。
// 沙箱命名空间须有拒绝全部入站与出站的 NetworkPolicy（CheckNetworkPolicy），Pod 不挂载 ServiceAccount 令牌。
// 控制器经 apiserver 的 exec 执行命令，沙箱内没有平台组件（ADR-0008）。
package k8s

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"yanshi/internal/artifact"
	"yanshi/internal/sandbox"
)

const (
	container = "sandbox"
	// annoID 记录 Pod 对应的沙箱 ID（Pod 名是它的摘要）。
	annoID = "yanshi.io/sandbox-id"
	// annoRestored 表示工作区已从对象存储恢复：没有它的 Pod 不打包，以免用残缺的工作区覆盖上一次的保存。
	annoRestored = "yanshi.io/restored"
	// annoOversize 表示上一次 Stop 因工作区超过上限而没有保存。
	annoOversize = "yanshi.io/oversize"
)

type Provider struct {
	Client *Client
	Image  string
	// RuntimeClass 是沙箱 Pod 的 runtimeClassName，如 gvisor；为空时用节点的默认运行时（只用于开发）。
	RuntimeClass string
	CPUs         float64
	MemoryMB     int
	// UID 是沙箱内运行代码的用户。
	UID int
	// WorkspaceLimit 是 Stop 时可保存的工作区上限（字节，打包后的大小）。
	WorkspaceLimit int64
	// Blobs 保存打包的工作区（与工件共用对象存储）；Destroy 时删除。
	Blobs artifact.BlobStore
	// StartTimeout 是等待 Pod 运行的上限（含拉取镜像）。
	StartTimeout time.Duration

	mu       sync.Mutex
	oversize map[string]bool // 最近一次 Ensure 看到的超限标记，Exec 据此提示模型
}

var _ sandbox.Provider = (*Provider)(nil)

const (
	DefaultWorkspaceLimit = 1 << 30
	tmpSize               = "256Mi"
)

func (p *Provider) defaults() {
	if p.CPUs == 0 {
		p.CPUs = 1
	}
	if p.MemoryMB == 0 {
		p.MemoryMB = 1024
	}
	if p.UID == 0 {
		p.UID = 1000
	}
	if p.WorkspaceLimit == 0 {
		p.WorkspaceLimit = DefaultWorkspaceLimit
	}
	if p.StartTimeout == 0 {
		p.StartTimeout = 2 * time.Minute
	}
}

// podName 由沙箱 ID 的摘要得到：ID 可能含大写或超过 DNS 名称长度，摘要满足 RFC 1123 且不冲突。
func podName(id string) string {
	h := sha256.Sum256([]byte(id))
	return "sbx-" + hex.EncodeToString(h[:12])
}

// blobKey 是打包工作区在对象存储中的键（与工件 ID 的前缀不同）。
func blobKey(id string) string {
	h := sha256.Sum256([]byte(id))
	return "sbxws_" + hex.EncodeToString(h[:20])
}

type pod struct {
	Metadata struct {
		Name              string            `json:"name"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Running *struct{} `json:"running"`
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (p *Provider) get(ctx context.Context, name string) (*pod, error) {
	var po pod
	if err := p.Client.do(ctx, "GET", p.Client.nsPath("pods/"+url.PathEscape(name)), "", nil, &po); err != nil {
		if isStatus(err, 404) {
			return nil, nil
		}
		return nil, err
	}
	return &po, nil
}

func (p *Provider) delete(ctx context.Context, name string) error {
	err := p.Client.do(ctx, "DELETE", p.Client.nsPath("pods/"+url.PathEscape(name))+"?gracePeriodSeconds=0", "", nil, nil)
	if isStatus(err, 404) {
		return nil
	}
	return err
}

func (p *Provider) annotate(ctx context.Context, name string, kv map[string]any) error {
	patch := map[string]any{"metadata": map[string]any{"annotations": kv}}
	return p.Client.do(ctx, "PATCH", p.Client.nsPath("pods/"+url.PathEscape(name)), "application/merge-patch+json", patch, nil)
}

// podSpec 与 Docker 实现的安全配置对齐，并满足 Pod Security 的 restricted 级别。
func (p *Provider) podSpec(id string) map[string]any {
	res := map[string]any{
		"cpu":    strconv.FormatFloat(p.CPUs, 'f', -1, 64),
		"memory": fmt.Sprintf("%dMi", p.MemoryMB),
	}
	spec := map[string]any{
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": 0,
		"securityContext": map[string]any{
			"runAsUser": p.UID, "runAsGroup": p.UID, "runAsNonRoot": true, "fsGroup": p.UID,
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"containers": []any{map[string]any{
			"name":            container,
			"image":           p.Image,
			"imagePullPolicy": "IfNotPresent",
			"command":         []string{"sleep", "infinity"},
			"workingDir":      sandbox.Workspace,
			"env": []any{
				map[string]any{"name": "HOME", "value": sandbox.Workspace},
				map[string]any{"name": "MPLCONFIGDIR", "value": "/tmp"},
			},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"capabilities":             map[string]any{"drop": []string{"ALL"}},
			},
			"resources": map[string]any{"limits": res, "requests": res},
			"volumeMounts": []any{
				map[string]any{"name": "workspace", "mountPath": sandbox.Workspace},
				map[string]any{"name": "tmp", "mountPath": "/tmp"},
			},
		}},
		"volumes": []any{
			// 硬上限是可保存上限的两倍：超过可保存上限时仍能运行并提示模型清理，超过硬上限时 kubelet 驱逐 Pod。
			map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": strconv.FormatInt(2*p.WorkspaceLimit, 10)}},
			map[string]any{"name": "tmp", "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": tmpSize}},
		},
	}
	if p.RuntimeClass != "" {
		spec["runtimeClassName"] = p.RuntimeClass
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name":        podName(id),
			"labels":      map[string]string{"app.kubernetes.io/name": "yanshi-sandbox", "app.kubernetes.io/component": "sandbox"},
			"annotations": map[string]string{annoID: id},
		},
		"spec": spec,
	}
}

func (p *Provider) Ensure(ctx context.Context, id string) error {
	p.defaults()
	name := podName(id)
	ctx, cancel := context.WithTimeout(ctx, p.StartTimeout)
	defer cancel()
	for {
		po, err := p.get(ctx, name)
		if err != nil {
			return err
		}
		switch {
		case po == nil:
			err := p.Client.do(ctx, "POST", p.Client.nsPath("pods"), "", p.podSpec(id), nil)
			if err != nil && !isStatus(err, 409) { // 409：并发创建或旧 Pod 尚未删完，重新查看
				return err
			}
		case po.Metadata.DeletionTimestamp != nil:
			// Stop 之后的 Pod 还在删除，名称被占用：等它消失。
		case po.Status.Phase == "Failed" || po.Status.Phase == "Succeeded":
			// 被驱逐或节点故障：上次 Stop 之后的改动随 emptyDir 丢失，删除后从保存的工作区重建。
			if err := p.delete(ctx, name); err != nil {
				return err
			}
		case running(po):
			p.mu.Lock()
			if p.oversize == nil {
				p.oversize = map[string]bool{}
			}
			p.oversize[id] = po.Metadata.Annotations[annoOversize] == "true"
			p.mu.Unlock()
			if po.Metadata.Annotations[annoRestored] == "true" {
				return nil
			}
			return p.restore(ctx, id, name)
		default:
			if w := waiting(po); w != "" {
				return fmt.Errorf("k8s: sandbox pod %s cannot start: %s", name, w)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("k8s: sandbox pod %s not running after %s: %w", name, p.StartTimeout, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func running(po *pod) bool {
	if po.Status.Phase != "Running" {
		return false
	}
	for _, c := range po.Status.ContainerStatuses {
		if c.Name == container {
			return c.State.Running != nil
		}
	}
	return false
}

// waiting 返回不会自行恢复的等待原因（镜像拉取失败、配置错误）；其余等待原因返回空。
func waiting(po *pod) string {
	for _, c := range po.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				return w.Reason + ": " + w.Message
			}
		}
	}
	return ""
}

// restore 把保存的工作区解包到新 Pod，再标记已恢复。中途失败时不标记，下次 Ensure 重做（解包可重复）。
func (p *Provider) restore(ctx context.Context, id, name string) error {
	rc, err := p.Blobs.Get(ctx, blobKey(id))
	switch {
	case errors.Is(err, artifact.ErrNotFound):
	case err != nil:
		return err
	default:
		defer rc.Close()
		var stderr bytes.Buffer
		code, err := p.Client.exec(ctx, name, container, []string{"tar", "-C", sandbox.Workspace, "-xf", "-"}, rc, io.Discard, &stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("k8s: restore workspace: tar exited %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
		}
	}
	return p.annotate(ctx, name, map[string]any{annoRestored: "true"})
}

func (p *Provider) Exec(ctx context.Context, id string, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = sandbox.DefaultExecTimeout
	}
	secs := strconv.Itoa(int(timeout.Seconds()))
	// 超时在沙箱内由 timeout 强制执行：即使控制器断开，进程也不会无限运行。exec 没有工作目录参数，由 sh 切换。
	argv := append([]string{"sh", "-c", `cd "$0" && exec "$@"`, sandbox.Workspace, "timeout", "-s", "KILL", secs}, req.Argv...)
	cctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	stdout := &sandbox.LimitedBuffer{Max: sandbox.OutputLimit}
	stderr := &sandbox.LimitedBuffer{Max: sandbox.OutputLimit}
	var in io.Reader
	if req.Stdin != nil {
		in = bytes.NewReader(req.Stdin)
	}
	start := time.Now()
	code, err := p.Client.exec(cctx, podName(id), container, argv, in, stdout, stderr)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	res := &sandbox.ExecResult{
		ExitCode: code,
		Stdout:   stdout.Bytes(), Stderr: stderr.Bytes(),
		StdoutTruncated: stdout.Truncated, StderrTruncated: stderr.Truncated,
		TimedOut: code == 137 && time.Since(start) >= timeout-time.Second,
	}
	p.mu.Lock()
	over := p.oversize[id]
	p.mu.Unlock()
	if over {
		res.Stderr = append(res.Stderr, fmt.Sprintf("\n[sandbox] 工作区超过可保存的上限（%d MB），空闲后无法保存；请删除不需要的文件。\n", p.WorkspaceLimit>>20)...)
	}
	return res, nil
}

func (p *Provider) CopyIn(ctx context.Context, id, path string, r io.Reader) error {
	var stderr bytes.Buffer
	code, err := p.Client.exec(ctx, podName(id), container, []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path}, r, io.Discard, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("copy into sandbox: exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func (p *Provider) CopyOut(ctx context.Context, id, path string, w io.Writer) error {
	var stderr bytes.Buffer
	code, err := p.Client.exec(ctx, podName(id), container, []string{"sh", "-c", `test -f "$1" && cat "$1"`, "sh", path}, nil, w, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		if stderr.Len() == 0 {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return fmt.Errorf("copy out of sandbox: exit %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func (p *Provider) ReadFile(ctx context.Context, id, path string, max int) ([]byte, bool, error) {
	var out, stderr bytes.Buffer
	code, err := p.Client.exec(ctx, podName(id), container, []string{"head", "-c", strconv.Itoa(max + 1), path}, nil, &out, &stderr)
	if err != nil {
		return nil, false, err
	}
	if code != 0 {
		return nil, false, fmt.Errorf("read %s: %s", path, bytes.TrimSpace(stderr.Bytes()))
	}
	if out.Len() > max {
		return out.Bytes()[:max], true, nil
	}
	return out.Bytes(), false, nil
}

// Stop 保存工作区并删除 Pod。工作区超过上限时不删除 Pod、标记超限并返回 sandbox.ErrWorkspaceTooLarge，
// 上一次保存的工作区保持不变（对象存储的写入是原子的）。
func (p *Provider) Stop(ctx context.Context, id string) error {
	p.defaults()
	name := podName(id)
	po, err := p.get(ctx, name)
	if err != nil || po == nil {
		return err
	}
	if running(po) && po.Metadata.Annotations[annoRestored] == "true" && po.Metadata.DeletionTimestamp == nil {
		if err := p.save(ctx, id, name); err != nil {
			if errors.Is(err, sandbox.ErrWorkspaceTooLarge) {
				if aerr := p.annotate(ctx, name, map[string]any{annoOversize: "true"}); aerr != nil {
					return aerr
				}
			}
			return err
		}
	}
	return p.delete(ctx, name)
}

// save 把工作区打包流式写入对象存储；打包失败或超过上限时写入失败，不留下部分内容。
func (p *Provider) save(ctx context.Context, id, name string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var stderr bytes.Buffer
		// 打包工作区的内容而不是 "."：解包时不必修改挂载点本身（它属于 root）。
		code, err := p.Client.exec(ctx, name, container, []string{"sh", "-c",
			`cd "$0" && find . -mindepth 1 -maxdepth 1 -print0 | tar --null -T - -cf -`, sandbox.Workspace}, nil, pw, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("k8s: save workspace: tar exited %d: %s", code, bytes.TrimSpace(stderr.Bytes()))
		}
		pw.CloseWithError(err) // err 为 nil 时读端得到 EOF
	}()
	lr := &limitReader{r: pr, left: p.WorkspaceLimit}
	err := p.Blobs.Put(ctx, blobKey(id), lr)
	if lr.exceeded {
		err = sandbox.ErrWorkspaceTooLarge
	}
	pr.CloseWithError(errors.New("save aborted"))
	cancel()
	<-done
	return err
}

type limitReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (l *limitReader) Read(b []byte) (int, error) {
	n, err := l.r.Read(b)
	l.left -= int64(n)
	if l.left < 0 {
		l.exceeded = true
		return 0, sandbox.ErrWorkspaceTooLarge
	}
	return n, err
}

// Destroy 删除 Pod 与保存的工作区；幂等。
func (p *Provider) Destroy(ctx context.Context, id string) error {
	if err := p.delete(ctx, podName(id)); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.oversize, id)
	p.mu.Unlock()
	if err := p.Blobs.Delete(ctx, blobKey(id)); err != nil && !errors.Is(err, artifact.ErrNotFound) {
		return err
	}
	return nil
}

// CheckNetworkPolicy 确认沙箱命名空间有一条选中全部 Pod、拒绝全部入站与出站的 NetworkPolicy。
// 控制器启动时调用：没有它，沙箱内的不可信代码可以访问集群网络（ADR-0053 选型结论第 6 条）。
func (c *Client) CheckNetworkPolicy(ctx context.Context) error {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				PodSelector struct {
					MatchLabels      map[string]string `json:"matchLabels"`
					MatchExpressions []any             `json:"matchExpressions"`
				} `json:"podSelector"`
				PolicyTypes []string `json:"policyTypes"`
				Ingress     []any    `json:"ingress"`
				Egress      []any    `json:"egress"`
			} `json:"spec"`
		} `json:"items"`
	}
	path := "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(c.cfg.Namespace) + "/networkpolicies"
	if err := c.do(ctx, "GET", path, "", nil, &list); err != nil {
		return err
	}
	for _, np := range list.Items {
		s := np.Spec
		if len(s.PodSelector.MatchLabels) != 0 || len(s.PodSelector.MatchExpressions) != 0 || len(s.Ingress) != 0 || len(s.Egress) != 0 {
			continue
		}
		var in, out bool
		for _, t := range s.PolicyTypes {
			in = in || t == "Ingress"
			out = out || t == "Egress"
		}
		if in && out {
			return nil
		}
	}
	return fmt.Errorf("k8s: namespace %s has no deny-all NetworkPolicy (empty podSelector, policyTypes Ingress and Egress, no rules); refusing to run sandboxes with network access", c.cfg.Namespace)
}
