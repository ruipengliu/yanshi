// Package httpapi 以 HTTP/JSON + SSE 暴露 Session 操作。
//
//	GET /v1/outbox?shard=0&after=&kinds=&limit=        本业务线的出站流（仅服务令牌，长轮询）
//	GET /v1/outbox/shards                            分片与安全起点 H（仅服务令牌）
//	POST /v1/oneshots/{name}                       无 Session 的单次调用（含 end_user 与 input）
//	POST /v1/loop/chat/completions                 外部循环的模型端点（OpenAI 兼容，只认步令牌，不经令牌中间件；ADR-0063）
//	POST /v1/sessions                              创建 Session（可设 IANA time_zone；delete_at 为 RFC3339，仅服务令牌）
//	GET  /v1/sessions?end_user=&cursor=&limit=     列出 Session（按最近输入倒序）
//	GET  /v1/sessions/{id}                         Session 投影
//	POST /v1/sessions/{id}/close                   关闭 Session（不可逆）
//	DELETE /v1/sessions/{id}                       删除 Session（异步，202；reason=erasure 仅服务令牌）
//	DELETE /v1/end_users/{id}                      删除 EndUser 的全部数据（仅服务令牌，异步，202）
//	GET  /v1/sessions/{id}/deletion                单个 Session 的删除进度（仅服务令牌，按删除记录授权）
//	GET  /v1/deletions/{id}                        删除请求的进度（仅服务令牌）
//	GET  /v1/memories?category=&q=&end_user=       本业务线的 Memory（重复 source_session 查询有效候选，仅服务令牌，最多 32 个）
//	DELETE /v1/memories/{id}                       删除一条 Memory
//	DELETE /v1/sessions/{id}/memories              删除该 Session 写入的全部 Memory（仅服务令牌；Session 已删时按删除记录授权）
//	GET  /v1/memories/access?since=&end_user=      本业务线 Memory 的读取记录
//	GET  /v1/grants                                EndUser 涉及本业务线的授权（仅用户令牌）
//	POST /v1/grants                                创建授权 {"from","to","categories","expires_at"}（仅用户令牌）
//	DELETE /v1/grants/{id}                         撤销授权（仅用户令牌）
//	POST /v1/sessions/{id}/schedules                 创建定时
//	GET /v1/sessions/{id}/schedules                  列出有效定时
//	DELETE /v1/sessions/{id}/schedules/{schedule_id}  取消定时
//	POST /v1/sessions/{id}/inputs                  提交输入（新 Run 或 Steer；带 input_id 可安全重试；内容安全拒绝 422、不可用 503）
//	GET  /v1/sessions/{id}/inputs?input_id=         查询输入接受、Run 状态与调用证据
//	POST /v1/sessions/{id}/runs/{run}/interrupt    中断 Run
//	POST /v1/sessions/{id}/transcriptions          识别一段录音（请求体为音频）→ {"text"}；不写日志，文字由调用方作为输入提交（docs/design/speech.md）
//	POST /v1/sessions/{id}/runs/{run}/speech       把 Run 的回复合成为语音（audio/wav）；未配置语音提供商时 501
//	GET  /v1/sessions/{id}/events?after=N&limit=M  已提交事件（JSON）
//	GET  /v1/sessions/{id}/stream?after=N          已提交事件 + 实时增量（SSE，支持 Last-Event-ID 续传）
//	POST /v1/sessions/{id}/approvals/{call}        审批决定 {"approve": bool}
//	POST /v1/sessions/{id}/answers/{call}          回答 ask_user 提问 {"selected": [...], "values": {...}, "text": ""}（ADR-0025）
//	GET  /v1/nodes?business_line=&end_user=        EndUser 的 Node 列表
//	POST /v1/sessions/{id}/artifacts?name=         上传工件（请求体为文件内容，Content-Type 为 MIME 类型）
//	GET  /v1/sessions/{id}/artifacts               Session 的工件列表
//	GET  /v1/artifacts/{id}                        下载工件
//	GET  /v1/artifacts/{id}/meta                   工件元数据
//	PUT  /v1/end_users/{id}/tier                   设置档位（仅服务令牌）
//	DELETE /v1/end_users/{id}/tier                 恢复默认档位（仅服务令牌）
//	PUT  /v1/end_users/{id}/consents/{kind}        登记 EndUser 的单独同意，kind 目前只有 health（仅服务令牌，ADR-0022）
//	DELETE /v1/end_users/{id}/consents/{kind}?purge= 撤回同意；purge=true 一并删除该用户在本业务线的健康 Memory（仅服务令牌）
//	GET  /v1/quota?end_user=                       当前周期的配额与已用（docs/design/m4-quota-usage.md §5）
//	GET  /v1/usage?from=&to=&group_by=&end_user=   用量汇总（day | end_user | model | agent | oneshot；金额单位：微元）
//	/v1/debug/*                                    故障注入与进程指标，只在 serve -faults 时提供（debug.go）
//
// 除 /healthz 外，所有请求须携带业务线签发的令牌（Authorization: Bearer），
// 调用方只能访问自己的 Session、Node 与工件，其余一律 404（docs/design/auth.md）。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"yanshi/internal/artifact"
	"yanshi/internal/auth"
	"yanshi/internal/extloop"
	"yanshi/internal/faults"
	"yanshi/internal/feed"
	"yanshi/internal/live"
	"yanshi/internal/memory"
	"yanshi/internal/metrics"
	"yanshi/internal/node"
	"yanshi/internal/notify"
	"yanshi/internal/oneshot"
	"yanshi/internal/outbox"
	"yanshi/internal/service"
	"yanshi/internal/speech"
	"yanshi/internal/usage"
)

type Server struct {
	Outbox outbox.Reader
	// OutboxWait 默认 25 秒；OutboxRecheck 默认 1 秒，独立于通知以保证水位推进后的活性。
	OutboxWait    time.Duration
	OutboxRecheck time.Duration
	// Auth 校验令牌；nil 时等同 auth.Insecure（信任自报身份，仅限回环地址开发）。
	Auth     auth.Verifier
	Service  *service.Service
	Oneshots *oneshot.Service
	// LoopModels 非 nil 时提供外部循环的模型端点（ADR-0063）。它不经令牌中间件，只接受步令牌。
	LoopModels http.Handler
	Live       live.Bus
	// Nodes 为 nil 时不提供 Node 相关信息。
	Nodes node.Directory
	// Artifacts 为 nil 时不提供工件接口。
	Artifacts *artifact.Service
	// Memory 为 nil 时不提供 Memory 与 Grant 接口。
	Memory *memory.Service
	// Usage 为 nil 时不提供用量接口；配额取自 Service.Quotas（docs/design/m4-quota-usage.md §5）。
	Usage usage.Store
	// Push 非 nil 时，注销账号一并删除推送设备登记。
	Push notify.Registry
	// Feeds 非 nil 时，本进程同一 Session 的事件流共用一个读者（feed.Hub）。
	Feeds *feed.Hub
	// Faults 非 nil 时注册调试接口（/v1/debug/*，serve -faults，ADR-0033）。
	Faults *faults.Injector
	// Speech 为 nil 或未配置提供商时，语音识别与播报接口返回 501。
	Speech *speech.Service
	Logger *slog.Logger
}

// routes 是全部需要鉴权的接口；授权矩阵测试据此检查每个接口都被覆盖。
func (s *Server) routes() map[string]http.HandlerFunc {
	routes := map[string]http.HandlerFunc{
		"GET /v1/outbox":                                   s.readOutbox,
		"GET /v1/outbox/shards":                            s.outboxShards,
		"POST /v1/oneshots/{name}":                         s.oneshot,
		"POST /v1/sessions":                                s.create,
		"GET /v1/sessions":                                 s.list,
		"GET /v1/sessions/{id}":                            s.get,
		"POST /v1/sessions/{id}/close":                     s.close,
		"DELETE /v1/sessions/{id}":                         s.delete,
		"DELETE /v1/end_users/{id}":                        s.deleteEndUser,
		"PUT /v1/end_users/{id}/tier":                      s.putTier,
		"DELETE /v1/end_users/{id}/tier":                   s.deleteTier,
		"PUT /v1/end_users/{id}/consents/{kind}":           s.putConsent,
		"DELETE /v1/end_users/{id}/consents/{kind}":        s.deleteConsent,
		"GET /v1/sessions/{id}/deletion":                   s.sessionDeletion,
		"GET /v1/deletions/{id}":                           s.deletion,
		"GET /v1/memories":                                 s.listMemories,
		"DELETE /v1/memories/{id}":                         s.forgetMemory,
		"DELETE /v1/sessions/{id}/memories":                s.forgetSessionMemories,
		"GET /v1/memories/access":                          s.memoryAccess,
		"GET /v1/grants":                                   s.listGrants,
		"POST /v1/grants":                                  s.createGrant,
		"DELETE /v1/grants/{id}":                           s.revokeGrant,
		"POST /v1/sessions/{id}/inputs":                    s.submit,
		"GET /v1/sessions/{id}/inputs":                     s.inputStatus,
		"POST /v1/sessions/{id}/runs/{run}/interrupt":      s.interrupt,
		"POST /v1/sessions/{id}/transcriptions":            s.transcribe,
		"POST /v1/sessions/{id}/runs/{run}/speech":         s.speak,
		"GET /v1/sessions/{id}/events":                     s.events,
		"GET /v1/sessions/{id}/stream":                     s.stream,
		"POST /v1/sessions/{id}/approvals/{call}":          s.decide,
		"POST /v1/sessions/{id}/answers/{call}":            s.answer,
		"POST /v1/sessions/{id}/background/{call}/cancel":  s.cancelBackground,
		"POST /v1/sessions/{id}/schedules":                 s.createSchedule,
		"GET /v1/sessions/{id}/schedules":                  s.listSchedules,
		"DELETE /v1/sessions/{id}/schedules/{schedule_id}": s.cancelSchedule,
		"GET /v1/nodes":                                    s.listNodes,
		"GET /v1/quota":                                    s.quota,
		"GET /v1/usage":                                    s.usage,
		"POST /v1/sessions/{id}/artifacts":                 s.uploadArtifact,
		"GET /v1/sessions/{id}/artifacts":                  s.listArtifacts,
		"GET /v1/artifacts/{id}":                           s.downloadArtifact,
		"GET /v1/artifacts/{id}/meta":                      s.artifactMeta,
	}
	for p, h := range s.debugRoutes() {
		routes[p] = h
	}
	return routes
}

// Routes 返回全部需要鉴权的接口模式。
func (s *Server) Routes() []string {
	var out []string
	for p := range s.routes() {
		out = append(out, p)
	}
	return out
}

func (s *Server) Handler() http.Handler {
	v := s.Auth
	if v == nil {
		v = auth.Insecure{}
	}
	api := http.NewServeMux()
	for p, h := range s.routes() {
		api.Handle(p, instrument(p, h))
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", auth.Middleware(v, api))
	if s.LoopModels != nil {
		p := "POST " + extloop.EndpointPath
		mux.Handle(p, instrument(p, s.LoopModels.ServeHTTP))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// statusWriter 记录响应码；保留 Flush 以支持 SSE。
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }

// Unwrap 让 http.ResponseController 找到底层连接（外部循环的模型端点据此设置读请求体的时限）。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// instrument 按路由模式记录请求数与耗时（模式而非路径，避免标签基数随 ID 增长）。
func instrument(route string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h(sw, r)
		metrics.HTTPRequests.WithLabelValues(route, strconv.Itoa(sw.code)).Inc()
		metrics.HTTPDuration.WithLabelValues(route).Observe(metrics.Since(start))
	})
}

func principal(r *http.Request) auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

// owned 返回只能作用于调用方 Session 的 ctx（service.WithAccess）：写操作由 Service 读取 Session 时一并校验归属，
// 不必先读一遍。
func owned(r *http.Request) context.Context {
	return service.WithAccess(r.Context(), principal(r).Allows)
}

// claim 返回调用方以 (businessLine, endUser) 身份行事时实际生效的值：令牌中有的以令牌为准，
// 请求中自报的值只能为空或与令牌一致。服务令牌须由请求给出 endUser。
func claim(p auth.Principal, businessLine, endUser string) (string, string, error) {
	if p.Unrestricted {
		return businessLine, endUser, nil
	}
	if businessLine != "" && businessLine != p.BusinessLine {
		return "", "", fmt.Errorf("%w: business_line does not match token", errForbidden)
	}
	if p.EndUser != "" {
		if endUser != "" && endUser != p.EndUser {
			return "", "", fmt.Errorf("%w: end_user does not match token", errForbidden)
		}
		endUser = p.EndUser
	}
	if endUser == "" {
		return "", "", fmt.Errorf("%w: end_user is required for service tokens", service.ErrInvalid)
	}
	return p.BusinessLine, endUser, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20)).Decode(v); err != nil {
		return fmt.Errorf("%w: %v", service.ErrInvalid, err)
	}
	return nil
}

func parseUint(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a non-negative integer", service.ErrInvalid, s)
	}
	return n, nil
}

// actor 描述调用方，记入关闭、审批等事件。
func actor(p auth.Principal) string {
	switch {
	case p.Unrestricted:
		return "api"
	case p.Service():
		return "service:" + p.BusinessLine
	}
	return "end_user:" + p.EndUser
}

// businessLineOf 返回只接受服务令牌的接口所作用的业务线：开发模式下取自查询参数。
func businessLineOf(w http.ResponseWriter, r *http.Request, s *Server) (string, bool) {
	p := principal(r)
	switch {
	case p.Unrestricted:
		if bl := r.URL.Query().Get("business_line"); bl != "" {
			return bl, true
		}
		s.fail(w, fmt.Errorf("%w: business_line is required", service.ErrInvalid))
		return "", false
	case p.Service():
		return p.BusinessLine, true
	}
	s.fail(w, fmt.Errorf("%w: a service token is required", errForbidden))
	return "", false
}
