package fsfiber_test

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	fsfiber "github.com/ubgo/filesystem/contrib/fiber"
)

// shutdownWait bounds how long a test waits for Shutdown to cancel a handler.
const shutdownWait = 5 * time.Second

// kept is a request context a handler held on to, with the Done channel
// it reported while the request ran.
type kept struct {
	ctx  context.Context
	done <-chan struct{}
}

// TestHandler_ContextOutlivesTheRequest is what tusd does: keep a context
// derived from the request's after the handler returns. fasthttp's pooled
// RequestCtx breaks the context contract once the server shuts down: it
// closes its done channel and then sets it to nil, so a Done channel seen
// closed is followed by a nil Err, and context.propagateCancel panics the
// process ("missing cancel error"). A handler adapted by fsfiber must get
// a context of its own: cancelled when the handler returns, and coherent
// (Done closed means Err set) after shutdown.
func TestHandler_ContextOutlivesTheRequest(t *testing.T) {
	var mu sync.Mutex
	var held []kept
	f := fiber.New()
	f.Get("/", fsfiber.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		held = append(held, kept{ctx: r.Context(), done: r.Context().Done()})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	h := held[0]
	mu.Unlock()
	if h.ctx.Err() == nil {
		t.Fatal("the handler's context was not cancelled when the handler returned")
	}
	if err := f.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.done:
		if h.ctx.Err() == nil {
			t.Fatal("Done is closed but Err is nil after shutdown: a derived context would panic in context.propagateCancel")
		}
	default:
		t.Fatal("Done is still open after the handler returned")
	}
}

// TestHandler_ShutdownCancelsARunningHandler pins that a handler still
// running when the server shuts down sees its context cancelled.
func TestHandler_ShutdownCancelsARunningHandler(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	f := fiber.New()
	f.Get("/", fsfiber.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	go func() { _ = f.Shutdown() }()
	select {
	case <-cancelled:
	case <-time.After(shutdownWait):
		t.Fatal("the running handler's context was not cancelled by Shutdown")
	}
}
