package serve_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ubgo/filesystem/examples/internal/serve"
)

func TestUntilDoneShutsDownOnCancel(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithCancel(t.Context())
	srv := &http.Server{Addr: addr, Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- serve.UntilDone(ctx, srv) }()
	for range 50 {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("UntilDone after cancel = %v", err)
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("still listening after shutdown")
	}
}

func TestUntilDoneReportsStartFailure(t *testing.T) {
	srv := &http.Server{Addr: "256.0.0.1:1", ReadHeaderTimeout: time.Second}
	if err := serve.UntilDone(t.Context(), srv); err == nil {
		t.Fatal("a server that cannot listen returned nil")
	}
}

// TestContextStopCancels pins that the stop function releases the signal
// handler by cancelling the context, so a deferred stop ends UntilDone.
func TestContextStopCancels(t *testing.T) {
	ctx, stop := serve.Context()
	if ctx.Err() != nil {
		t.Fatal("context cancelled before stop")
	}
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the context")
	}
}
