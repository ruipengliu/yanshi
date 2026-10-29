package bff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"persona/query"
	"xinli/account"
	"xinli/delivery"
)

func appToken(r *http.Request) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return value
}

func accountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, account.ErrUnauthorized), errors.Is(err, account.ErrInactive):
		query.WriteJSON(w, 401, map[string]string{"error": "unauthorized"})
	case errors.Is(err, account.ErrInvalid):
		query.WriteJSON(w, 400, map[string]string{"error": "invalid_request"})
	default:
		fail(w, err)
	}
}

func (o Options) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/account/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			fail(w, query.ErrInvalid)
			return
		}
		p, _ := account.FromContext(r.Context())
		record, err := o.Accounts.Store.ReadAccount(r.Context(), p.Owner)
		if err != nil {
			accountError(w, err)
			return
		}
		query.WriteJSON(w, 200, map[string]any{"devices": record.Devices})
	})
	mux.HandleFunc("PATCH /v1/account/preferences", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TimeZone    *string         `json:"time_zone"`
			Preferences json.RawMessage `json:"preferences"`
		}
		if !decodeBody(w, r, &in, false) {
			return
		}
		p, _ := account.FromContext(r.Context())
		record, err := o.Accounts.Store.ReadAccount(r.Context(), p.Owner)
		if err != nil {
			accountError(w, err)
			return
		}
		zone, prefs := record.Account.TimeZone, record.Account.Preferences
		if in.TimeZone != nil {
			zone = *in.TimeZone
		}
		if len(in.Preferences) > 0 {
			prefs = in.Preferences
		}
		if _, err := delivery.ReadPreferences(prefs); err != nil {
			accountError(w, err)
			return
		}
		if err := o.Accounts.Profile(r.Context(), p.Owner, zone, prefs); err != nil {
			accountError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /v1/account", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !decodeBody(w, r, &in, true) {
			return
		}
		p, _ := account.FromContext(r.Context())
		token, err := o.Accounts.RequestDeletion(r.Context(), p.Owner)
		if err != nil {
			accountError(w, err)
			return
		}
		query.WriteJSON(w, 202, map[string]string{"status": "deleting", "deletion_token": token})
	})
	mux.HandleFunc("GET /v1/account/deletion", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			fail(w, query.ErrInvalid)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		out, err := o.Accounts.DeletionWithToken(ctx, appToken(r))
		if err != nil {
			accountError(w, err)
			return
		}
		query.WriteJSON(w, 200, out)
	})
}
