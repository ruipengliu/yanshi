package bff

import (
	"encoding/json"
	"net/http"

	"persona/command"
	"persona/query"
)

func (o Options) execute(w http.ResponseWriter, r *http.Request, in command.Request, status int) {
	if o.Commands == nil {
		query.WriteJSON(w, 503, map[string]string{"error": "unavailable"})
		return
	}
	out, err := o.Commands.Execute(r.Context(), ownerOf(r), in)
	if err != nil {
		command.WriteError(w, err)
		return
	}
	query.WriteJSON(w, status, out)
}

func (o Options) personaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PATCH /v1/commitments/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Rev    json.RawMessage   `json:"rev"`
			Status string            `json:"status,omitempty"`
			Fields map[string]string `json:"fields,omitempty"`
		}
		if decodeBody(w, r, &in, false) {
			o.execute(w, r, command.Request{Operation: "commitment", ID: r.PathValue("id"), Rev: in.Rev, Status: in.Status, Fields: in.Fields}, 200)
		}
	})
	mux.HandleFunc("PUT /v1/projects/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Rev           json.RawMessage `json:"rev"`
			Status        string          `json:"status"`
			NextSteps     []string        `json:"next_steps"`
			OpenQuestions []string        `json:"open_questions"`
		}
		if decodeBody(w, r, &in, false) {
			o.execute(w, r, command.Request{Operation: "project_status", ID: r.PathValue("id"), Rev: in.Rev, Status: in.Status, NextSteps: in.NextSteps, OpenQuestions: in.OpenQuestions}, 200)
		}
	})
	mux.HandleFunc("POST /v1/days/{date}/exchanges/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Rev     json.RawMessage `json:"rev"`
			Project string          `json:"project"`
		}
		if decodeBody(w, r, &in, false) {
			o.execute(w, r, command.Request{Operation: "resolve", ID: r.PathValue("id"), Date: r.PathValue("date"), Rev: in.Rev, Project: in.Project}, 200)
		}
	})
	mux.HandleFunc("POST /v1/days/{date}/erase", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if decodeBody(w, r, &in, false) {
			o.execute(w, r, command.Request{Operation: "erase", Date: r.PathValue("date"), From: in.From, To: in.To}, 202)
		}
	})
	mux.HandleFunc("DELETE /v1/days/{date}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if decodeBody(w, r, &in, true) {
			o.execute(w, r, command.Request{Operation: "erase_day", Date: r.PathValue("date")}, 202)
		}
	})
	mux.HandleFunc("DELETE /v1/deletions/{id}/memories/{memory}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !decodeBody(w, r, &in, true) {
			return
		}
		if o.Commands == nil {
			query.WriteJSON(w, 503, map[string]string{"error": "unavailable"})
			return
		}
		owner := ownerOf(r)
		_, err := o.Commands.Execute(r.Context(), owner, command.Request{Operation: "confirm_memory", ID: r.PathValue("id"), MemoryID: r.PathValue("memory")})
		if err != nil {
			command.WriteError(w, err)
			return
		}
		ops, err := o.operations()
		if err == nil {
			err = ops.DeleteMemory(r.Context(), owner.BusinessLine, owner.EndUser, r.PathValue("memory"))
		}
		if err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(204)
	})
}
