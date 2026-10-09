package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"yanshi/internal/service"
	"yanshi/internal/usage"
)

// usageOwner 返回用量接口的范围：EndUser 令牌只能查自己；服务令牌查整个业务线，可用 end_user 缩小范围。
func usageOwner(w http.ResponseWriter, r *http.Request, s *Server) (string, string, bool) {
	p, q := principal(r), r.URL.Query()
	bl, eu := q.Get("business_line"), q.Get("end_user")
	switch {
	case p.Unrestricted:
		if bl == "" {
			s.fail(w, fmt.Errorf("%w: business_line is required", service.ErrInvalid))
			return "", "", false
		}
		return bl, eu, true
	case bl != "" && bl != p.BusinessLine:
		s.fail(w, fmt.Errorf("%w: business_line does not match token", errForbidden))
		return "", "", false
	case p.EndUser != "" && eu != "" && eu != p.EndUser:
		s.fail(w, fmt.Errorf("%w: end_user does not match token", errForbidden))
		return "", "", false
	case p.EndUser != "":
		eu = p.EndUser
	}
	return p.BusinessLine, eu, true
}

func (s *Server) quota(w http.ResponseWriter, r *http.Request) {
	bl, eu, ok := usageOwner(w, r, s)
	if !ok {
		return
	}
	periods, err := s.Service.Quotas.Status(r.Context(), bl, eu, s.Service.Store.Clock.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	if periods == nil {
		periods = []*usage.Period{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"periods": periods})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	if s.Usage == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "usage is not configured"})
		return
	}
	bl, eu, ok := usageOwner(w, r, s)
	if !ok {
		return
	}
	q := r.URL.Query()
	loc := s.Service.Quotas.Location(bl)
	now := s.Service.Store.Clock.Now().In(loc)
	// 默认：本月 1 日到明天（不含）。
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	to := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
	for _, f := range []struct {
		name string
		dst  *time.Time
	}{{"from", &from}, {"to", &to}} {
		if v := q.Get(f.name); v != "" {
			t, err := time.ParseInLocation(time.DateOnly, v, loc)
			if err != nil {
				s.fail(w, fmt.Errorf("%w: %s must be YYYY-MM-DD", service.ErrInvalid, f.name))
				return
			}
			*f.dst = t
		}
	}
	by := usage.GroupBy(q.Get("group_by"))
	switch by {
	case "":
		by = usage.ByDay
	case usage.ByDay, usage.ByModel:
	case usage.ByEndUser:
		if principal(r).EndUser != "" {
			s.fail(w, fmt.Errorf("%w: end user tokens cannot group by end_user", errForbidden))
			return
		}
	default:
		s.fail(w, fmt.Errorf("%w: group_by must be day, end_user or model", service.ErrInvalid))
		return
	}
	if !from.Before(to) || to.Sub(from) > 400*24*time.Hour {
		s.fail(w, fmt.Errorf("%w: from must be before to, within 400 days", service.ErrInvalid))
		return
	}
	rows, err := s.Usage.Report(r.Context(), usage.Query{BusinessLine: bl, EndUser: eu, From: from, To: to, GroupBy: by, Location: loc})
	if err != nil {
		s.fail(w, err)
		return
	}
	if rows == nil {
		rows = []usage.Row{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"business_line": bl, "end_user": eu, "from": from, "to": to,
		"group_by": by, "unit": "micro_yuan", "rows": rows})
}
