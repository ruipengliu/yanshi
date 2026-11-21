package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"yanshi/internal/artifact"
	"yanshi/internal/sandbox"
	sandboxdocker "yanshi/internal/sandbox/docker"
	sandboxk8s "yanshi/internal/sandbox/k8s"
)

// sandboxFlags 是 serve 与 eval 共用的沙箱参数（docs/design/m2-sandbox.md §5、§7）。
type sandboxFlags struct {
	kind, image, runtime                  *string
	k8sNamespace, k8sAPI, k8sToken, k8sCA *string
	runtimeClass                          *string
	workspaceMB                           *int
}

func addSandboxFlags(fs *flag.FlagSet) *sandboxFlags {
	return &sandboxFlags{
		kind:         fs.String("sandbox", "none", "代码沙箱：none | docker（开发）| k8s（生产，ADR-0053）"),
		image:        fs.String("sandbox-image", "yanshi-sandbox:dev", "沙箱镜像（make sandbox-image 构建）"),
		runtime:      fs.String("sandbox-runtime", "", "sandbox=docker 的容器运行时，如 runsc（gVisor）"),
		runtimeClass: fs.String("sandbox-runtime-class", "gvisor", "sandbox=k8s 的 RuntimeClass（默认 gVisor；有 KVM 的部署可用 kata；空为节点默认运行时，只用于开发）"),
		k8sNamespace: fs.String("sandbox-k8s-namespace", "yanshi-sandbox", "sandbox=k8s 的沙箱命名空间（须有拒绝全部网络的 NetworkPolicy）"),
		k8sAPI:       fs.String("sandbox-k8s-api", "", "sandbox=k8s 的 apiserver 地址；为空时用 Pod 内的 ServiceAccount"),
		k8sToken:     fs.String("sandbox-k8s-token-file", "", "sandbox=k8s 的令牌文件（与 -sandbox-k8s-api 一起使用）"),
		k8sCA:        fs.String("sandbox-k8s-ca-file", "", "sandbox=k8s 的 apiserver CA 证书（与 -sandbox-k8s-api 一起使用）"),
		workspaceMB:  fs.Int("sandbox-workspace-limit-mb", 1024, "sandbox=k8s 空闲时可保存的工作区上限（打包后，MB）"),
	}
}

func (f *sandboxFlags) validate() error {
	switch *f.kind {
	case "none", "docker", "k8s":
		return nil
	}
	return fmt.Errorf("unknown sandbox %q (none | docker | k8s)", *f.kind)
}

func (f *sandboxFlags) enabled() bool { return *f.kind != "none" }

// provider 构造沙箱 Provider；sandbox=none 时返回 nil。sandbox=k8s 时先确认沙箱命名空间拒绝全部网络，
// 没有这条策略就拒绝启动（ADR-0053 选型结论第 6 条）。
func (f *sandboxFlags) provider(ctx context.Context, blobs artifact.BlobStore) (sandbox.Provider, error) {
	switch *f.kind {
	case "docker":
		return &sandboxdocker.Provider{Image: *f.image, Runtime: *f.runtime}, nil
	case "k8s":
		cfg := sandboxk8s.Config{API: *f.k8sAPI, TokenFile: *f.k8sToken, CAFile: *f.k8sCA, Namespace: *f.k8sNamespace}
		if cfg.API == "" {
			var err error
			if cfg, err = sandboxk8s.InCluster(*f.k8sNamespace); err != nil {
				return nil, err
			}
		}
		c, err := sandboxk8s.NewClient(cfg)
		if err != nil {
			return nil, err
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := c.CheckNetworkPolicy(cctx); err != nil {
			return nil, err
		}
		return &sandboxk8s.Provider{Client: c, Image: *f.image, RuntimeClass: *f.runtimeClass,
			WorkspaceLimit: int64(*f.workspaceMB) << 20, Blobs: blobs}, nil
	}
	return nil, nil
}

func (f *sandboxFlags) describe() []any {
	if *f.kind == "k8s" {
		return []any{"provider", "k8s", "image", *f.image, "runtime_class", *f.runtimeClass, "namespace", *f.k8sNamespace}
	}
	return []any{"provider", *f.kind, "image", *f.image, "runtime", *f.runtime}
}
