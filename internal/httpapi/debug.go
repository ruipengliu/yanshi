package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	dto "github.com/prometheus/client_model/go"

	"yanshi/internal/faults"
	"yanshi/internal/metrics"
	"yanshi/internal/service"
)

// 调试接口只在 serve -faults 时注册（ADR-0033），供调试控制台注入故障、查看进程指标：
//
//	GET    /v1/debug/faults        规则与最近的注入
//	POST   /v1/debug/faults        增加规则 {"kind","session_id","count","params"}
//	DELETE /v1/debug/faults?id=    删除规则（省略 id 删除全部可见规则）
//	GET    /v1/debug/stats         本进程的 yanshi_* 指标（计数与当前值，不含任何内容）
//
// 规则对能访问其 Session 的调用方可见：用户令牌只能对自己的 Session 注入（必须指定 session_id），服务令牌对本业务线的
// Session；不限 Session 的规则只有服务令牌与 -auth none 可以添加。
//
// serve -xinli-mock 时另有（与 -faults 无关）：
//
//	GET    /v1/debug/xinli?business_line=&end_user=   模拟的 xinli 业务线中该 EndUser 的项目与整理结果（控制台的"旁听"视图）
func (s *Server) debugRoutes() map[string]http.HandlerFunc {
	routes := map[string]http.HandlerFunc{}
	if s.Faults != nil {
		routes["GET /v1/debug/faults"] = s.listFaults
		routes["POST /v1/debug/faults"] = s.addFault
		routes["DELETE /v1/debug/faults"] = s.deleteFaults
		routes["GET /v1/debug/stats"] = s.stats
	}
	if s.Xinli != nil {
		routes["GET /v1/debug/xinli"] = s.xinliState
	}
	return routes
}

// xinliState 返回模拟的业务线中调用方（用户令牌为本人，服务令牌与 -auth none 按参数）的数据，鉴权同 Memory 的列表。
func (s *Server) xinliState(w http.ResponseWriter, r *http.Request) {
	bl, eu, ok := memoryOwner(w, r, s)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.Xinli(bl, eu))
}

// faultVisible 报告调用方能否看到、增删这条规则：不限 Session 的规则只对服务令牌（与 -auth none）；
// 针对 Session 的规则对能访问该 Session 的调用方。
func (s *Server) faultVisible(r *http.Request, sessionID string) bool {
	p := principal(r)
	if p.Unrestricted {
		return true
	}
	if sessionID == "" {
		return p.Service()
	}
	st, err := s.Service.Load(r.Context(), sessionID)
	return err == nil && p.Allows(st.Created.GetBusinessLine(), st.Created.GetEndUser())
}

func (s *Server) listFaults(w http.ResponseWriter, r *http.Request) {
	rules, recent := s.Faults.Snapshot()
	rules = slices.DeleteFunc(rules, func(f faults.Rule) bool { return !s.faultVisible(r, f.SessionID) })
	recent = slices.DeleteFunc(recent, func(f faults.Firing) bool { return !s.faultVisible(r, f.SessionID) })
	writeJSON(w, http.StatusOK, map[string]any{"kinds": faults.Kinds, "rules": rules, "recent": recent})
}

func (s *Server) addFault(w http.ResponseWriter, r *http.Request) {
	var req faults.Rule
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if !s.faultVisible(r, req.SessionID) {
		s.fail(w, fmt.Errorf("%w: session %q (a user token can only inject into its own sessions)", service.ErrNotFound, req.SessionID))
		return
	}
	rule, err := s.Faults.Add(req)
	if err != nil {
		s.fail(w, fmt.Errorf("%w: %v", service.ErrInvalid, err))
		return
	}
	if s.Logger != nil {
		s.Logger.Warn("fault rule added", "rule", rule.ID, "kind", rule.Kind, "session", rule.SessionID, "count", rule.Count)
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) deleteFaults(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	rules, _ := s.Faults.Snapshot()
	for _, f := range rules {
		if (id == "" || f.ID == id) && s.faultVisible(r, f.SessionID) {
			s.Faults.Delete(f.ID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type metricView struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
	// Count 与 Sum 只用于直方图。
	Count uint64  `json:"count,omitempty"`
	Sum   float64 `json:"sum,omitempty"`
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	families, err := metrics.Registry.Gather()
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string][]metricView{}
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "yanshi_") {
			continue
		}
		for _, m := range f.GetMetric() {
			v := metricView{}
			for _, l := range m.GetLabel() {
				if v.Labels == nil {
					v.Labels = map[string]string{}
				}
				v.Labels[l.GetName()] = l.GetValue()
			}
			switch f.GetType() {
			case dto.MetricType_COUNTER:
				v.Value = m.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				v.Value = m.GetGauge().GetValue()
			case dto.MetricType_HISTOGRAM:
				v.Count, v.Sum = m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
			default:
				continue
			}
			out[f.GetName()] = append(out[f.GetName()], v)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
