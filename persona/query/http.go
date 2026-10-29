package query

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"persona/domain"
	"persona/mcpserver"
	"persona/paging"
	"persona/store"
)

type Route struct {
	Path     string
	Resource string
}

// Routes 由内部 HTTP 与 BFF 同时挂载；日期和对象 ID 都是 owner 下的局部键。
var Routes = []Route{
	{Path: "/access", Resource: "access"},
	{Path: "/projects", Resource: "projects"},
	{Path: "/projects/{id}", Resource: "project"},
	{Path: "/projects/{id}/exchanges", Resource: "exchanges"},
	{Path: "/commitments", Resource: "commitments"},
	{Path: "/commitments/{id}/history", Resource: "history"},
	{Path: "/days/{date}", Resource: "day"},
	{Path: "/weeks/{date}", Resource: "week"},
	{Path: "/days/{date}/captures", Resource: "captures"},
	{Path: "/deletions", Resource: "deletions"},
}

func ParseHTTP(r *http.Request, resource string) (Request, error) {
	values, err := urlValues(r)
	if err != nil {
		return Request{}, err
	}
	if date := r.PathValue("date"); date != "" {
		if values.Has("date") {
			return Request{}, ErrInvalid
		}
		values.Set("date", date)
	}
	return Parse(resource, r.PathValue("id"), values)
}

type lineKey struct{}

// Handler 只接受受信 BFF 的服务凭据，EndUser 声明不授权其他业务线。
func Handler(reader Reader, credentials []mcpserver.Credential, now func() time.Time) http.Handler {
	credentials = append([]mcpserver.Credential(nil), credentials...)
	if now == nil {
		now = time.Now
	}
	mux := http.NewServeMux()
	for _, route := range Routes {
		mux.HandleFunc("GET /v1/query/{endUser}"+route.Path, func(w http.ResponseWriter, r *http.Request) {
			q, err := ParseHTTP(r, route.Resource)
			if err != nil {
				WriteError(w, err)
				return
			}
			line, _ := r.Context().Value(lineKey{}).(string)
			owner := store.Owner{BusinessLine: line, EndUser: r.PathValue("endUser")}
			out, err := reader.Read(r.Context(), owner, q)
			if err != nil {
				WriteError(w, err)
				return
			}
			WriteJSON(w, 200, out)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		line := mcpserver.AuthenticateService(r, credentials, now())
		if mcpserver.ValidateCredentials(credentials) != nil || line == "" {
			WriteJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), lineKey{}, line)))
	})
}

// Error 只包含固定分类；网络错误和存储错误的原文不进入产品响应。
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string { return e.Code }

func WriteError(w http.ResponseWriter, err error) {
	status, code := 503, "unavailable"
	var remote *Error
	switch {
	case errors.As(err, &remote):
		status, code = remote.Status, remote.Code
	case errors.Is(err, ErrInvalid), errors.Is(err, paging.ErrInvalid), errors.Is(err, domain.ErrQueryInvalid):
		status, code = 400, "invalid_query"
	case errors.Is(err, domain.ErrQueryNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, domain.ErrCapturesExpired):
		status, code = 410, "expired"
	case errors.Is(err, domain.ErrDayDeleted):
		status, code = 410, "deleted"
	case errors.Is(err, store.ErrOwnerDeleting):
		status, code = 403, "account_deleting"
	}
	WriteJSON(w, status, map[string]string{"error": code})
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
