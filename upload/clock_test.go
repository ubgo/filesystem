package upload_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestChunkedTakeoverFollowsItsClock pins ChunkedOptions.Now: a stalled
// assembler keeps its claim until AssemblyTimeout has passed by the given
// clock, and then a retried last chunk takes the assembly over, with no
// sleeping and no backdated manifest.
func TestChunkedTakeoverFollowsItsClock(t *testing.T) {
	const timeout = time.Minute
	clock := &fakeClock{now: time.Now()}
	file := pngData + "tail"
	f := upload.DropzoneFields()
	inspecting, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	p := images
	p.Inspect = func(context.Context, *filesystem.Disk, upload.Result) error {
		if calls.Add(1) == 1 {
			inspecting <- struct{}{}
			<-release
		}
		return nil
	}
	h := chunked(t, newDisk(t, memory.New()), p, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, AssemblyTimeout: timeout, Now: clock.Now})
	b, ct := chunkBody(t, f, "id", 0, 2, int64(len(file)), "a.png", pngData)
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
		t.Fatalf("first chunk = %d", rec.Code)
	}
	last := func() *httptest.ResponseRecorder {
		b, ct := chunkBody(t, f, "id", 1, 2, int64(len(file)), "a.png", "tail")
		return sendChunk(t, h, "u1", b, ct)
	}
	stalled := make(chan *httptest.ResponseRecorder, 1)
	go func() { stalled <- last() }()
	<-inspecting

	clock.Advance(timeout / 2)
	if rec := last(); rec.Code == http.StatusCreated || calls.Load() != 1 {
		t.Errorf("retry within the timeout = %d, %d assemblies; want the claim kept", rec.Code, calls.Load())
	}
	clock.Advance(timeout)
	if rec := last(); rec.Code != http.StatusCreated {
		t.Errorf("retry past the timeout = %d %s, want the assembly taken over", rec.Code, rec.Body)
	}
	close(release)
	if rec := <-stalled; rec.Code != http.StatusConflict {
		t.Errorf("superseded assembler = %d %s", rec.Code, rec.Body)
	}
}

// TestFetchMeasuresRetryAfterByItsClock pins FetchOptions.Now: a
// Retry-After given as an HTTP date becomes the wait from the given clock.
func TestFetchMeasuresRetryAfterByItsClock(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	const wait = 90 * time.Second
	srv := server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", now.Add(wait).Format(http.TimeFormat))
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	o := loopback
	o.Now = func() time.Time { return now }
	_, err := upload.Fetch(t.Context(), srv+"/x.png", o)
	wantCode(t, err, filesystem.CodeUnavailable)
	if got, ok := filesystem.RetryAfterOf(err); !ok || got != wait {
		t.Errorf("RetryAfter = %s, %v; want %s", got, ok, wait)
	}
}

// TestChunkedClaimRenewsByItsClock pins that the assembler renews its
// claim by ChunkedOptions.Now before settling: a retry that comes more
// than AssemblyTimeout after the assembly started, but less after the
// renewal, does not take it over.
func TestChunkedClaimRenewsByItsClock(t *testing.T) {
	const timeout = time.Minute
	clock := &fakeClock{now: time.Now()}
	file := pngData + "tail"
	f := upload.DropzoneFields()
	settling, release := make(chan struct{}), make(chan struct{})
	var settles atomic.Int32
	p := images
	// A slow stream: the clock moves while the file is inspected, before
	// the renewal.
	p.Inspect = func(context.Context, *filesystem.Disk, upload.Result) error {
		clock.Advance(timeout * 3 / 4)
		return nil
	}
	p.Settle = func(_ context.Context, _ *filesystem.Disk, r upload.Result) (upload.Result, error) {
		if settles.Add(1) == 1 {
			settling <- struct{}{}
			<-release
		}
		return r, nil
	}
	h := chunked(t, newDisk(t, memory.New()), p, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, AssemblyTimeout: timeout, Now: clock.Now})
	b, ct := chunkBody(t, f, "id", 0, 2, int64(len(file)), "a.png", pngData)
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
		t.Fatalf("first chunk = %d", rec.Code)
	}
	last := func() *httptest.ResponseRecorder {
		b, ct := chunkBody(t, f, "id", 1, 2, int64(len(file)), "a.png", "tail")
		return sendChunk(t, h, "u1", b, ct)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- last() }()
	<-settling
	clock.Advance(timeout / 2) // 1.25 timeouts after the start, 0.5 after the renewal
	if rec := last(); rec.Code == http.StatusCreated || settles.Load() != 1 {
		t.Errorf("retry after the renewal = %d, %d settles; want the renewed claim kept", rec.Code, settles.Load())
	}
	close(release)
	if rec := <-done; rec.Code != http.StatusCreated {
		t.Errorf("assembler = %d %s", rec.Code, rec.Body)
	}
}
