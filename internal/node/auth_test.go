package node_test

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/auth"
	"yanshi/internal/clock"
	"yanshi/internal/node"
)

func TestTokenAuth(t *testing.T) {
	k, _ := auth.GenerateDevKey("demo", "demo-1")
	pub, _ := k.Public()
	v, err := auth.NewJWT("yanshi", clock.Real{}, pub)
	if err != nil {
		t.Fatal(err)
	}
	a := node.TokenAuth{Verifier: v}
	now := time.Now()
	user, _ := k.Sign("yanshi", "u1", time.Hour, now)
	service, _ := k.Sign("yanshi", "", time.Hour, now)

	id, err := a.Authenticate(context.Background(), &v1.Hello{NodeId: "n1", Token: user})
	if err != nil || id.BusinessLine != "demo" || id.EndUser != "u1" || id.Expires.IsZero() {
		t.Fatalf("id = %+v, err = %v", id, err)
	}
	if _, err := a.Authenticate(context.Background(), &v1.Hello{NodeId: "n1", Token: user, BusinessLine: "demo", EndUser: "u1"}); err != nil {
		t.Fatalf("matching self-reported identity rejected: %v", err)
	}
	for name, h := range map[string]*v1.Hello{
		"no token":            {NodeId: "n1"},
		"bad token":           {NodeId: "n1", Token: "x.y.z"},
		"service token":       {NodeId: "n1", Token: service},
		"other end user":      {NodeId: "n1", Token: user, EndUser: "u2"},
		"other business line": {NodeId: "n1", Token: user, BusinessLine: "other"},
	} {
		if _, err := a.Authenticate(context.Background(), h); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
