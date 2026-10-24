package memory_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestClock(t *testing.T) {
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	d := memory.New(memory.WithClock(func() time.Time { return at }))
	e, err := d.Write(t.Context(), "k", strings.NewReader("x"), filesystem.WriteOptions{})
	if err != nil || !e.ModTime.Equal(at) {
		t.Errorf("mod time = %v, %v", e.ModTime, err)
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("body broke") }

func TestMultipartEdges(t *testing.T) {
	ctx := t.Context()
	d := memory.New()
	id, _ := d.CreateMultipart(ctx, "k", filesystem.WriteOptions{IfNotExists: true})
	if _, err := d.UploadPart(ctx, "k", id, 1, failingBody{}, 1); err == nil {
		t.Error("failing part body accepted")
	}
	if _, err := d.UploadPart(ctx, "other", id, 1, strings.NewReader("x"), 1); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("part for another key: %v", err)
	}
	if _, err := d.ListParts(ctx, "other", id); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("list parts for another key: %v", err)
	}
	if _, err := d.CompleteMultipart(ctx, "other", id, nil); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("complete for another key: %v", err)
	}
	p, _ := d.UploadPart(ctx, "k", id, 1, strings.NewReader("x"), 1)
	if _, err := d.CompleteMultipart(ctx, "k", id, []filesystem.Part{{Number: 1, ETag: "bad"}}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("wrong ETag: %v", err)
	}
	_, _ = d.Write(ctx, "k", strings.NewReader("taken"), filesystem.WriteOptions{})
	if _, err := d.CompleteMultipart(ctx, "k", id, []filesystem.Part{p}); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Errorf("conditional complete over an existing key: %v", err)
	}
}

func TestOpener(t *testing.T) {
	u, _ := url.Parse("mem://x")
	if _, err := memory.Opener(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	u, _ = url.Parse("mem://x?nope=1")
	if _, err := memory.Opener(context.Background(), u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("unknown param: %v", err)
	}
}

// cancelAfterFirstRead yields one byte, then cancels its context.
type cancelAfterFirstRead struct {
	cancel context.CancelFunc
	done   bool
}

func (c *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if c.done {
		return 0, errors.New("read after cancel")
	}
	c.done = true
	c.cancel()
	p[0] = 'x'
	return 1, nil
}

func TestContextIsHonoured(t *testing.T) {
	d := memory.New()
	ctx := t.Context()
	if _, err := d.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	id, _ := d.CreateMultipart(ctx, "k", filesystem.WriteOptions{})
	ended, cancel := context.WithCancel(ctx)
	cancel()
	calls := map[string]error{}
	_, calls["Stat"] = d.Stat(ended, "k")
	_, calls["Read"] = d.Read(ended, "k", filesystem.ByteRange{})
	_, calls["Write"] = d.Write(ended, "k", strings.NewReader("y"), filesystem.WriteOptions{})
	calls["Delete"] = d.Delete(ended, "k")
	calls["DeleteKeys"] = d.DeleteKeys(ended, []string{"k"})
	_, calls["List"] = d.List(ended, "", filesystem.ListOptions{})
	_, calls["Copy"] = d.Copy(ended, "k", "c")
	_, calls["Move"] = d.Move(ended, "k", "m")
	calls["SetVisibility"] = d.SetVisibility(ended, "k", filesystem.VisibilityPublic)
	calls["DeletePrefix"] = d.DeletePrefix(ended, "")
	_, calls["CreateMultipart"] = d.CreateMultipart(ended, "k", filesystem.WriteOptions{})
	_, calls["UploadPart"] = d.UploadPart(ended, "k", id, 1, strings.NewReader("y"), 1)
	_, calls["ListParts"] = d.ListParts(ended, "k", id)
	_, calls["CompleteMultipart"] = d.CompleteMultipart(ended, "k", id, nil)
	calls["AbortMultipart"] = d.AbortMultipart(ended, "k", id)
	for name, err := range calls {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s under an ended context: %v", name, err)
		}
	}
	// Nothing changed.
	if got, err := d.Read(ctx, "k", filesystem.ByteRange{}); err != nil {
		t.Fatalf("object gone: %v", err)
	} else {
		_ = got.Close()
	}
	if parts, err := d.ListParts(ctx, "k", id); err != nil || len(parts) != 0 {
		t.Fatalf("upload changed: %v, %v", parts, err)
	}

	// A context ending mid-body stores nothing.
	live, stop := context.WithCancel(ctx)
	if _, err := d.Write(live, "mid", &cancelAfterFirstRead{cancel: stop}, filesystem.WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("mid-write cancel: %v", err)
	}
	if _, err := d.Stat(ctx, "mid"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("cancelled write stored an object: %v", err)
	}

	// Through a Disk the context error is unavailable.
	disk, _ := filesystem.NewDisk("m", d)
	if _, err := disk.Stat(ended, "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("disk code = %v", err)
	}
}

func TestSweepAbandonedUsesTheClock(t *testing.T) {
	ctx := t.Context()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := start
	drv := memory.New(memory.WithClock(func() time.Time { return now }))
	idle, _ := drv.CreateMultipart(ctx, "idle.bin", filesystem.WriteOptions{})
	busy, _ := drv.CreateMultipart(ctx, "busy.bin", filesystem.WriteOptions{})
	now = start.Add(48 * time.Hour)
	// A part received now keeps an upload started two days ago alive.
	if _, err := drv.UploadPart(ctx, "busy.bin", busy, 1, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if n, err := drv.SweepAbandoned(ctx, start.Add(24*time.Hour)); n != 1 || err != nil {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if _, err := drv.ListParts(ctx, "idle.bin", idle); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("idle upload kept: %v", err)
	}
	if _, err := drv.ListParts(ctx, "busy.bin", busy); err != nil {
		t.Errorf("busy upload swept: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := drv.SweepAbandoned(cancelled, now); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sweep: %v", err)
	}
}

func TestTagsHonourTheContext(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := memory.New().Tags(cancelled, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("tags with a cancelled context: %v", err)
	}
}
