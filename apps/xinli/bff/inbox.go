package bff

import (
	"errors"
	"net/http"

	"persona/paging"
	"persona/query"
	"xinli/account"
	"xinli/delivery"
)

func (o Options) inbox(w http.ResponseWriter, r *http.Request) {
	owner := ownerOf(r)
	limit, after, scope, err := pageRequest(r, owner, "inbox")
	if err != nil {
		fail(w, err)
		return
	}
	if err = o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	if o.Inbox == nil {
		fail(w, errors.New("inbox unavailable"))
		return
	}
	p, _ := account.FromContext(r.Context())
	state, err := o.Inbox.ReadDelivery(r.Context(), p.Owner)
	if err != nil {
		accountError(w, err)
		return
	}
	if err = o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	query.WriteJSON(w, 200, paging.Slice(state.List(), limit, after, scope, func(item delivery.Item) string { return item.ID }))
}

func (o Options) readInbox(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !decodeBody(w, r, &body, true) {
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, query.ErrInvalid)
		return
	}
	if err := o.accessible(r); err != nil {
		fail(w, err)
		return
	}
	if o.Inbox == nil {
		fail(w, errors.New("inbox unavailable"))
		return
	}
	p, _ := account.FromContext(r.Context())
	err := o.Inbox.UpdateDelivery(r.Context(), p.Owner, func(state *delivery.State) error {
		item, ok := state.Items[r.PathValue("id")]
		if !ok {
			return delivery.ErrNotFound
		}
		item.Read = true
		state.Items[item.ID] = item
		return nil
	})
	if errors.Is(err, delivery.ErrNotFound) {
		query.WriteJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	if err != nil {
		accountError(w, err)
		return
	}
	w.WriteHeader(204)
}
