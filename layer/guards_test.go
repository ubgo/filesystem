package layer_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// TestConstructorsRefuseBadSettings pins the panics that reject settings
// which would make an interceptor meaningless (a zero bandwidth, a
// probability outside [0, 1], a non-positive hedge delay): a typo must
// fail at start-up, not throttle to nothing or hedge every call at once.
func TestConstructorsRefuseBadSettings(t *testing.T) {
	cases := map[string]func(){
		"layer.Bandwidth": func() { layer.Bandwidth(0) },
		"layer.Fault":     func() { layer.Fault(layer.FaultOptions{Rate: 1.5}) },
		"layer.Hedge":     func() { layer.Hedge(0) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				msg, _ := recover().(string)
				if !strings.HasPrefix(msg, name+":") {
					t.Fatalf("panic = %q, want one naming %s", msg, name)
				}
			}()
			build()
		})
	}
}

// ctxBlind is the memory driver writing whatever the context says, so a
// write that reaches it succeeds and only Fault can fail it.
type ctxBlind struct{ *memory.Driver }

func (d ctxBlind) Write(_ context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	return d.Driver.Write(context.Background(), key, body, o)
}

// TestFaultLatencyEndsWithTheContext proves the latency Fault adds gives
// up when the operation's context ends, failing it as unavailable before
// it reaches the driver.
func TestFaultLatencyEndsWithTheContext(t *testing.T) {
	d, err := filesystem.NewDisk("d", ctxBlind{memory.New()}, filesystem.WithInterceptors(layer.Fault(layer.FaultOptions{Latency: time.Hour, Ops: []filesystem.OpKind{filesystem.OpWrite}})))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{})
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write = %v, want CodeUnavailable wrapping the deadline", err)
	}
	if time.Since(start) > time.Minute {
		t.Fatal("the latency outlived the context")
	}
	if ok, _ := d.Exists(t.Context(), "k"); ok {
		t.Fatal("the cancelled write reached the driver")
	}
}
