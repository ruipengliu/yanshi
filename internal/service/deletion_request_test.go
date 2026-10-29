package service_test

import (
	"context"
	"errors"
	"testing"

	"yanshi/internal/lifecycle"
	"yanshi/internal/service"
)

type countedDeletionProgress struct {
	lifecycle.Deletions
	counts int
}

func (d *countedDeletionProgress) RequestInfo(_ context.Context, id string) (*lifecycle.Request, error) {
	return &lifecycle.Request{ID: id, BusinessLine: "owner"}, nil
}
func (d *countedDeletionProgress) CountRequest(context.Context, string) (int, int, error) {
	d.counts++
	return 0, 0, errors.New("shard unavailable")
}

func TestDeletionRequestAuthorizesBeforeCounting(t *testing.T) {
	d := &countedDeletionProgress{}
	s := &service.Service{Deletions: d}
	if _, err := s.DeletionRequest(context.Background(), "other", "request"); !errors.Is(err, lifecycle.ErrNotFound) {
		t.Fatalf("foreign request: %v", err)
	}
	if d.counts != 0 {
		t.Fatal("foreign request queried shard progress")
	}
	if r, err := s.DeletionRequest(context.Background(), "owner", "request"); err == nil || r != nil {
		t.Fatal("partial result returned")
	}
	if d.counts != 1 {
		t.Fatal("authorized request did not query progress")
	}
}
