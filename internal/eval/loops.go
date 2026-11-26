package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/clock"
	"yanshi/internal/extloop"
	"yanshi/internal/ids"
)

// 评测外部循环（ADR-0062、ADR-0063）：Config.Loops 把 AgentDef 指向的循环名接到正在运行的外部服务。评测在
// LoopEndpointAddr 上开内核的模型端点，外部服务把模型的 base URL 配成 http://<地址>/v1/loop、以步令牌为 API key；
// 评测的模型覆盖替换 loop.models，经端点的调用计入 Run 的 token 与缓存命中。

// DefaultLoopEndpointAddr 是评测模型端点的默认监听地址。
const DefaultLoopEndpointAddr = "127.0.0.1:18472"

// loopState 是一次评测中所有实例共用的外部循环设施：步令牌密钥与经端点调用的用量（按 Session）。
type loopState struct {
	key   extloop.TokenKey
	mu    sync.Mutex
	usage map[string]*v1.Usage
}

func (l *loopState) add(c *extloop.StepClaims, _ string, u *v1.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.usage[c.SessionID]
	if t == nil {
		t = &v1.Usage{}
		l.usage[c.SessionID] = t
	}
	t.InputTokens += u.GetInputTokens()
	t.CachedInputTokens += u.GetCachedInputTokens()
	t.OutputTokens += u.GetOutputTokens()
}

// take 取走一个 Session 至今经端点调用的用量。
func (l *loopState) take(sid string) *v1.Usage {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	u := l.usage[sid]
	delete(l.usage, sid)
	return u
}

// addLoopUsage 把这一轮经端点的模型调用计入观察：外部循环那一步的 AssistantMessage 不带模型用量。
func (in *instance) addLoopUsage(sid string, obs *observation) {
	u := in.cfg.loops.take(sid)
	if u == nil || obs == nil {
		return
	}
	obs.tokens += u.GetInputTokens() + u.GetOutputTokens()
	obs.inTokens += u.GetInputTokens()
	obs.cachedTokens += u.GetCachedInputTokens()
}

// loopServers 是评测的循环登记：每个循环为两条评测业务线各登记一次，共用评测生成的密钥。
func loopServers(cfg Config) []extloop.Server {
	var out []extloop.Server
	for name, url := range cfg.Loops {
		for _, bl := range []string{BusinessLine, NoHealthBusinessLine} {
			out = append(out, extloop.Server{Name: name, BusinessLine: bl, URL: url, TokenKeys: []extloop.TokenKey{cfg.loops.key}})
		}
	}
	return out
}

// startLoops 生成密钥、开模型端点。agents 是全部用例派生出的 AgentDef（各实例派生出的版本与它相同）。
func startLoops(ctx context.Context, cfg *Config, agents *agentdef.Registry) (stop func(), err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	cfg.loops = &loopState{key: extloop.TokenKey{KID: "eval", Secret: hex.EncodeToString(secret)}, usage: map[string]*v1.Usage{}}
	keys := []extloop.TokenKey{cfg.loops.key}
	endpoint := &extloop.Endpoint{
		Keys: func(bl, name string) ([]extloop.TokenKey, time.Duration) {
			if _, ok := cfg.Loops[name]; ok && (bl == BusinessLine || bl == NoHealthBusinessLine) {
				return keys, extloop.DefaultTimeout
			}
			return nil, 0
		},
		Agents: agents, Model: cfg.Model, Clock: clock.Real{}, IDs: ids.Random(), Logger: cfg.Logger, OnUsage: cfg.loops.add,
	}
	addr := cfg.LoopEndpointAddr
	if addr == "" {
		addr = DefaultLoopEndpointAddr
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("loop endpoint: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST "+extloop.EndpointPath, endpoint)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cfg.Logger.Warn("eval: loop endpoint stopped", "err", err)
		}
	}()
	return func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}, nil
}

// missingLoop 返回用例的 AgentDef 指向、但评测没有接上（Config.Loops）的循环名；用例因此跳过。
func missingLoop(cfg Config, agents *agentdef.Registry, version string, c *Case) string {
	if agents == nil || c.Agent == "" {
		return ""
	}
	d, err := agents.Get(&v1.AgentRef{Name: c.Agent, Version: version})
	if err != nil || d.Loop == nil {
		return ""
	}
	if _, ok := cfg.Loops[d.Loop.Server]; ok {
		return ""
	}
	return d.Loop.Server
}
