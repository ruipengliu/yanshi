package pg_test

import (
	"context"
	"net"
	"testing"
	"time"

	"persona/pg"
)

func TestOpenBoundsUnresponsivePostgres(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { <-stop; _ = conn.Close() }()
		}
	}()
	defer func() { close(stop); _ = listener.Close(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	pool, err := pg.Open(ctx, "postgres://test@"+listener.Addr().String()+"/test?sslmode=disable&connect_timeout=30", "")
	if pool != nil {
		pool.Close()
	}
	if err == nil || time.Since(started) >= 2*time.Second {
		t.Fatalf("unresponsive startup was not bounded: err=%v elapsed=%s", err, time.Since(started))
	}
}
