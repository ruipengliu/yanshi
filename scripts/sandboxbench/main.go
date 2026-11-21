// sandboxbench 比较沙箱隔离运行时（runc 与 runsc 等）的兼容性、冷启动、执行开销与密度，
// 为 ADR-0053 的选型提供压测依据。它直接驱动 Docker Provider，所以测到的是与开发实现相同的安全配置。
//
//	go run ./scripts/sandboxbench -runtime runsc -image yanshi-sandbox:dev -out runsc.json
//
// 运行时为空表示 Docker 默认运行时（runc）。需要能访问 Docker 的环境；结果只含计时与内存，不含代码输出。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"yanshi/internal/sandbox"
	"yanshi/internal/sandbox/docker"
)

// workload 是一种典型的代码解释器负载：Argv 在工作区中执行，期望退出码 0 且 stdout 含 Want。
type workload struct {
	Name string
	Argv []string
	Want string
}

var workloads = []workload{
	{"noop", []string{"true"}, ""},
	{"python_start", []string{"python3", "-c", "print('ok')"}, "ok"},
	{"pandas", []string{"python3", "-c", `
import numpy as np, pandas as pd
df = pd.DataFrame({"k": np.random.randint(0, 100, 500_000), "v": np.random.rand(500_000)})
g = df.groupby("k")["v"].agg(["mean", "sum"])
df.to_csv("bench.csv", index=False)
print("ok", len(pd.read_csv("bench.csv")))
`}, "ok 500000"},
	{"matplotlib", []string{"python3", "-c", `
import matplotlib; matplotlib.use("Agg")
import matplotlib.pyplot as plt
fig, ax = plt.subplots()
ax.plot(range(1000), [i*i for i in range(1000)]); ax.set_title("销售额")
fig.savefig("bench.png", dpi=100); print("ok")
`}, "ok"},
	{"xlsx", []string{"python3", "-c", `
import pandas as pd
pd.DataFrame({"a": range(20000), "b": ["x"]*20000}).to_excel("bench.xlsx", index=False)
print("ok", len(pd.read_excel("bench.xlsx")))
`}, "ok 20000"},
	{"small_files", []string{"python3", "-c", `
import os
os.makedirs("many", exist_ok=True)
for i in range(500):
    open(f"many/{i}.txt", "w").write("x" * 1024)
print("ok", sum(len(open(f"many/{i}.txt").read()) for i in range(500)))
`}, "ok 512000"},
	{"cpu", []string{"python3", "-c", "print('ok', sum(i*i for i in range(5_000_000)) > 0)"}, "ok True"},
	{"subprocess", []string{"sh", "-c", "for i in $(seq 50); do python3 -c pass; done; echo ok"}, "ok"},
	{"ffmpeg", []string{"sh", "-c", "ffmpeg -loglevel error -f lavfi -i sine=frequency=440:duration=5 -y tone.mp3 && echo ok"}, "ok"},
}

type stat struct {
	N                  int     `json:"n"`
	P50, P95, Max, Avg float64 // 毫秒
}

func summarize(ds []time.Duration) stat {
	if len(ds) == 0 {
		return stat{}
	}
	ms := make([]float64, len(ds))
	var sum float64
	for i, d := range ds {
		ms[i] = float64(d.Microseconds()) / 1000
		sum += ms[i]
	}
	sort.Float64s(ms)
	q := func(p float64) float64 { return ms[int(p*float64(len(ms)-1)+0.5)] }
	return stat{N: len(ms), P50: q(0.5), P95: q(0.95), Max: ms[len(ms)-1], Avg: sum / float64(len(ms))}
}

type report struct {
	Runtime   string          `json:"runtime"`
	Image     string          `json:"image"`
	Compat    map[string]bool `json:"compat"`
	ColdStart stat            `json:"cold_start"` // 新工作区：建卷、改属主、启动容器
	WarmStart stat            `json:"warm_start"` // Stop 之后重新 Ensure（工作区已存在）
	Workloads map[string]stat `json:"workloads"`
	// 密度：同时空闲的沙箱数与每个沙箱占用的宿主内存（MemAvailable 的差值）。
	Density struct {
		Sandboxes   int     `json:"sandboxes"`
		PerSandbox  float64 `json:"per_sandbox_mb"`
		ExecP95Busy float64 `json:"exec_p95_busy_ms"` // 并发执行 python_start 时的 p95
	} `json:"density"`
}

func main() {
	runtime := flag.String("runtime", "", "Docker 运行时（空为 runc，runsc 为 gVisor）")
	image := flag.String("image", "yanshi-sandbox:dev", "沙箱镜像")
	starts := flag.Int("starts", 20, "冷启动与热启动的次数")
	iters := flag.Int("iters", 10, "每种负载的执行次数")
	density := flag.Int("density", 30, "密度测试同时启动的沙箱数（0 跳过）")
	out := flag.String("out", "", "JSON 报告路径")
	flag.Parse()

	ctx := context.Background()
	p := &docker.Provider{Image: *image, Runtime: *runtime}
	rt := *runtime
	if rt == "" {
		rt = "runc"
	}
	prefix := fmt.Sprintf("bench-%s-%d", rt, time.Now().Unix())
	var created []string
	defer func() {
		for _, id := range created {
			_ = p.Destroy(context.Background(), id)
		}
	}()
	newID := func(tag string, i int) string {
		id := fmt.Sprintf("%s-%s-%d", prefix, tag, i)
		created = append(created, id)
		return id
	}
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	r := report{Runtime: rt, Image: *image, Compat: map[string]bool{}, Workloads: map[string]stat{}}

	// 冷启动与热启动：每次一个新沙箱，Ensure 后执行一次 true 才算可用。
	var cold, warm []time.Duration
	for i := range *starts {
		id := newID("start", i)
		t0 := time.Now()
		must(p.Ensure(ctx, id))
		_, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"true"}})
		must(err)
		cold = append(cold, time.Since(t0))
		must(p.Stop(ctx, id))
		t0 = time.Now()
		must(p.Ensure(ctx, id))
		_, err = p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"true"}})
		must(err)
		warm = append(warm, time.Since(t0))
		must(p.Destroy(ctx, id))
	}
	r.ColdStart, r.WarmStart = summarize(cold), summarize(warm)
	fmt.Fprintf(os.Stderr, "starts done: cold p95 %.0f ms, warm p95 %.0f ms\n", r.ColdStart.P95, r.WarmStart.P95)

	// 负载：同一个沙箱内先执行一次（兼容性判定，同时预热页缓存），再计时 iters 次。
	id := newID("work", 0)
	must(p.Ensure(ctx, id))
	for _, w := range workloads {
		res, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: w.Argv, Timeout: 5 * time.Minute})
		ok := err == nil && res.ExitCode == 0 && strings.Contains(string(res.Stdout), w.Want)
		r.Compat[w.Name] = ok
		if !ok {
			fmt.Fprintf(os.Stderr, "compat %s failed: err=%v res=%+v\n", w.Name, err, res)
			continue
		}
		var ds []time.Duration
		for range *iters {
			t0 := time.Now()
			_, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: w.Argv, Timeout: 5 * time.Minute})
			must(err)
			ds = append(ds, time.Since(t0))
		}
		r.Workloads[w.Name] = summarize(ds)
		fmt.Fprintf(os.Stderr, "%s p50 %.0f ms\n", w.Name, r.Workloads[w.Name].P50)
	}
	r.Compat["isolation"] = isolated(ctx, p, id)

	if *density > 0 {
		before := memAvailableMB()
		ids := make([]string, *density)
		for i := range ids {
			ids[i] = newID("dense", i)
			must(p.Ensure(ctx, ids[i]))
		}
		time.Sleep(5 * time.Second) // 等运行时的后台初始化稳定
		after := memAvailableMB()
		r.Density.Sandboxes = *density
		if before > 0 && after > 0 {
			r.Density.PerSandbox = (before - after) / float64(*density)
		}
		var mu sync.Mutex
		var ds []time.Duration
		var wg sync.WaitGroup
		for _, did := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				t0 := time.Now()
				_, err := p.Exec(ctx, did, sandbox.ExecRequest{Argv: []string{"python3", "-c", "print('ok')"}})
				if err == nil {
					mu.Lock()
					ds = append(ds, time.Since(t0))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		r.Density.ExecP95Busy = summarize(ds).P95
	}

	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		must(os.WriteFile(*out, b, 0o644))
	}
}

// isolated 复核安全配置在该运行时下仍然生效：非 root、无网络、根文件系统只读。
func isolated(ctx context.Context, p sandbox.Provider, id string) bool {
	res, err := p.Exec(ctx, id, sandbox.ExecRequest{Argv: []string{"python3", "-c", `
import os, socket
print("uid", os.getuid())
try:
    socket.create_connection(("1.1.1.1", 53), timeout=2); print("net ok")
except OSError: print("net blocked")
try:
    open("/usr/evil", "w"); print("rootfs writable")
except OSError: print("rootfs read-only")
`}})
	if err != nil {
		return false
	}
	out := string(res.Stdout)
	return strings.Contains(out, "uid 1000") && strings.Contains(out, "net blocked") && strings.Contains(out, "rootfs read-only")
}

// memAvailableMB 读宿主（运行压测的 Linux）的可用内存；不在 Linux 上时返回 0。
func memAvailableMB() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		var kb float64
		if n, _ := fmt.Sscanf(line, "MemAvailable: %f kB", &kb); n == 1 {
			return kb / 1024
		}
	}
	return 0
}
