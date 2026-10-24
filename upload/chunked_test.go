package upload_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// indexGate holds every reader of an owner's upload index, after it has
// read the bytes, until n have arrived, so concurrent new uploads all work
// from the same version of it.
type indexGate struct {
	*memory.Driver
	n       int32
	arrived *atomic.Int32
	open    chan struct{}
}

func (g indexGate) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := g.Driver.Read(ctx, key, r)
	if strings.HasSuffix(key, ".json") && !strings.HasSuffix(key, "manifest.json") {
		if g.arrived.Add(1) == g.n {
			close(g.open)
		}
		select {
		case <-g.open:
		case <-time.After(2 * time.Second):
		}
	}
	return rc, err
}

// Uploads starting together cannot pass MaxUploadsPerOwner between
// them: admission is one compare-and-swap of the owner's index.
func TestChunkedAdmissionIsAtomic(t *testing.T) {
	const starts = 8
	drv := memory.New()
	opts := upload.ChunkedOptions{Staging: "chunks", Owner: byUser, MaxUploadsPerOwner: 3}
	start := func(h http.Handler, id string) int {
		b, ct := chunkBody(t, upload.DropzoneFields(), id, 0, 2, 100, "a.png", pngData)
		return sendChunk(t, h, "u1", b, ct).Code
	}
	if code := start(chunked(t, newDisk(t, drv), images, opts), "first"); code != http.StatusOK {
		t.Fatalf("first upload = %d", code)
	}
	gated := chunked(t, newDisk(t, indexGate{Driver: drv, n: starts, arrived: new(atomic.Int32), open: make(chan struct{})}), images, opts)
	codes := make(chan int, starts)
	var wg sync.WaitGroup
	for i := range starts {
		wg.Go(func() { codes <- start(gated, "racer-"+string(rune('a'+i))) })
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for code := range codes {
		got[code]++
	}
	if got[http.StatusOK] != 2 || got[http.StatusInsufficientStorage] != starts-2 {
		t.Errorf("racing starts = %v, want 2 admitted next to the first", got)
	}
}

// An upload stops counting against MaxUploadsPerOwner once it is
// assembled, or once Sweep has removed it as abandoned.
func TestChunkedUploadsLeaveTheCount(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, MaxUploadsPerOwner: 2})
	f := upload.DropzoneFields()
	file := pngData + "tail"
	send := func(id string, index int, part string) int {
		b, ct := chunkBody(t, f, id, index, 2, int64(len(file)), "a.png", part)
		return sendChunk(t, h, "u1", b, ct).Code
	}
	if send("a", 0, pngData) != http.StatusOK || send("b", 0, pngData) != http.StatusOK || send("c", 0, pngData) != http.StatusInsufficientStorage {
		t.Fatal("the cap of 2 did not hold")
	}
	if code := send("b", 1, "tail"); code != http.StatusCreated {
		t.Fatalf("completing b = %d", code)
	}
	if code := send("c", 0, pngData); code != http.StatusOK {
		t.Errorf("after b was assembled = %d", code)
	}
	// a is abandoned: Sweep removes its staging, and a new upload may take
	// its place.
	for _, k := range listKeys(t, d, "chunks") {
		if strings.HasSuffix(k, "/a/manifest.json") {
			if err := d.DeletePrefix(ctx, strings.TrimSuffix(k, "manifest.json")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code := send("d", 0, pngData); code != http.StatusOK {
		t.Errorf("after a was swept = %d", code)
	}
	if code := send("e", 0, pngData); code != http.StatusInsufficientStorage {
		t.Errorf("c and d in progress, e = %d", code)
	}
	// With every upload swept, index included, counting starts afresh.
	if _, err := upload.Sweep(ctx, d, "chunks", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if send("e", 0, pngData) != http.StatusOK || send("f", 0, pngData) != http.StatusOK || send("g", 0, pngData) != http.StatusInsufficientStorage {
		t.Error("the cap did not hold after a full sweep")
	}
}

// An assembler taken over after AssemblyTimeout stops before it settles
// the file and never removes the chunks its successor reads: the file is
// stored and reported once.
func TestChunkedTakeoverIsFenced(t *testing.T) {
	for _, successorFirst := range []bool{false, true} {
		f := upload.DropzoneFields()
		file := pngData + "tail"
		d := newDisk(t, memory.New())
		inspecting := make(chan struct{})
		proceed := []chan struct{}{make(chan struct{}), make(chan struct{})}
		var calls, stored atomic.Int32
		p := images
		p.Inspect = func(context.Context, *filesystem.Disk, upload.Result) error {
			i := calls.Add(1) - 1
			inspecting <- struct{}{}
			<-proceed[i]
			return nil
		}
		h := chunked(t, d, p, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, AssemblyTimeout: time.Minute, OnStored: func(context.Context, *http.Request, upload.Result) error {
			stored.Add(1)
			return nil
		}})
		b, ct := chunkBody(t, f, "id", 0, 2, int64(len(file)), "a.png", pngData)
		if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
			t.Fatalf("first chunk = %d", rec.Code)
		}
		complete := func() chan *httptest.ResponseRecorder {
			done := make(chan *httptest.ResponseRecorder, 1)
			b, ct := chunkBody(t, f, "id", 1, 2, int64(len(file)), "a.png", "tail")
			go func() { done <- sendChunk(t, h, "u1", b, ct) }()
			return done
		}
		stalled := complete()
		<-inspecting
		expireAssembly(t, d)
		successor := complete()
		<-inspecting
		if successorFirst {
			close(proceed[1])
			if rec := <-successor; rec.Code != http.StatusCreated {
				t.Fatalf("successor = %d %s", rec.Code, rec.Body)
			}
		}
		close(proceed[0])
		if rec := <-stalled; rec.Code != http.StatusConflict {
			t.Errorf("superseded assembler = %d %s", rec.Code, rec.Body)
		}
		if !successorFirst {
			if n := len(listKeys(t, d, "chunks")); n != 4 { // two chunks, the manifest, the owner's index
				t.Errorf("superseded assembler left %d staging objects, want 4", n)
			}
			close(proceed[1])
			if rec := <-successor; rec.Code != http.StatusCreated {
				t.Fatalf("successor = %d %s", rec.Code, rec.Body)
			}
		}
		if keys := listKeys(t, d, ""); len(keys) != 1 || stored.Load() != 1 {
			t.Errorf("successor first %v: left %v, OnStored ran %d times", successorFirst, keys, stored.Load())
		}
	}
}

// expireAssembly backdates the assembler's last claim of every upload on
// d past any AssemblyTimeout, as a crashed or stalled assembler leaves it.
func expireAssembly(t *testing.T, d *filesystem.Disk) {
	t.Helper()
	for _, k := range listKeys(t, d, "chunks") {
		if !strings.HasSuffix(k, "manifest.json") {
			continue
		}
		data, err := d.ReadAll(t.Context(), k)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		m["assembling_since"], _ = json.Marshal(time.Now().Add(-time.Hour))
		data, _ = json.Marshal(m)
		if _, err := d.WriteBytes(t.Context(), k, data, filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// stagingFaults fails the staging operations fail picks: op is "stat" or
// "write", and o the write's options (zero for a stat).
type stagingFaults struct {
	*memory.Driver
	fail func(op, key string, o filesystem.WriteOptions) error
}

func (s stagingFaults) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if err := s.fail("stat", key, filesystem.WriteOptions{}); err != nil {
		return filesystem.Entry{}, err
	}
	return s.Driver.Stat(ctx, key)
}

func (s stagingFaults) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	if err := s.fail("write", key, o); err != nil {
		return filesystem.Entry{}, err
	}
	return s.Driver.Write(ctx, key, body, o)
}

// isIndex reports whether key is an owner's upload index.
func isIndex(key string) bool {
	return strings.HasSuffix(key, ".json") && !strings.HasSuffix(key, "manifest.json")
}

// The owner's index and the assembler's claims fail cleanly when the
// staging disk does.
func TestChunkedStagingFailures(t *testing.T) {
	f := upload.DropzoneFields()
	file := pngData + "tail"
	var held, assembling atomic.Bool
	for name, c := range map[string]struct {
		fail func(op, key string, o filesystem.WriteOptions) error
		max  int
	}{
		"index write fails": {func(op, key string, _ filesystem.WriteOptions) error {
			if op == "write" && isIndex(key) {
				return filesystem.ErrUnavailable
			}
			return nil
		}, 1},
		"index keeps changing": {func(op, key string, o filesystem.WriteOptions) error {
			if op == "write" && isIndex(key) && o.IfMatch != "" {
				return filesystem.ErrConflict
			}
			return nil
		}, 2},
		"prune cannot stat": {func(op, key string, _ filesystem.WriteOptions) error {
			if held.Load() && op == "stat" && strings.HasSuffix(key, "/held/manifest.json") {
				return filesystem.ErrUnavailable
			}
			return nil
		}, 1},
		"claim cannot read": {func(op, key string, _ filesystem.WriteOptions) error {
			if assembling.Load() && op == "stat" && strings.HasSuffix(key, "manifest.json") {
				return filesystem.ErrUnavailable
			}
			return nil
		}, 1},
		"claim cannot write": {func(op, key string, o filesystem.WriteOptions) error {
			if assembling.Load() && op == "write" && strings.HasSuffix(key, "manifest.json") {
				return filesystem.ErrUnavailable
			}
			return nil
		}, 1},
		"claim keeps losing": {func(op, key string, o filesystem.WriteOptions) error {
			if assembling.Load() && op == "write" && strings.HasSuffix(key, "manifest.json") && o.IfMatch != "" {
				return filesystem.ErrConflict
			}
			return nil
		}, 1},
	} {
		held.Store(false)
		assembling.Store(false)
		p := images
		p.Inspect = func(context.Context, *filesystem.Disk, upload.Result) error {
			assembling.Store(true)
			return nil
		}
		d := newDisk(t, stagingFaults{Driver: memory.New(), fail: c.fail})
		h := chunked(t, d, p, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, MaxUploadsPerOwner: c.max})
		send := func(id string, index int, part string) *httptest.ResponseRecorder {
			b, ct := chunkBody(t, f, id, index, 2, int64(len(file)), "a.png", part)
			return sendChunk(t, h, "u1", b, ct)
		}
		// The first upload creates the index; the second finds it full.
		rec := send("held", 0, pngData)
		held.Store(true)
		if rec.Code == http.StatusOK {
			if name == "prune cannot stat" || name == "index keeps changing" {
				rec = send("next", 0, pngData)
			} else {
				rec = send("held", 1, "tail")
			}
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
}

// A settling policy still runs behind the claim, and an index that
// vanished (swept) during the upload is not recreated by its release.
func TestChunkedSettleAndVanishedIndex(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	p := images
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	h := chunked(t, d, p, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
	file := pngData + "tail"
	b, ct := chunkBody(t, upload.DropzoneFields(), "id", 0, 2, int64(len(file)), "a.png", pngData)
	sendChunk(t, h, "u1", b, ct)
	for _, k := range listKeys(t, d, "chunks") {
		if isIndex(k) {
			if err := d.Delete(ctx, k); err != nil {
				t.Fatal(err)
			}
		}
	}
	b, ct = chunkBody(t, upload.DropzoneFields(), "id", 1, 2, int64(len(file)), "a.png", "tail")
	rec := sendChunk(t, h, "u1", b, ct)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"key":"blobs/`) {
		t.Fatalf("settled chunked upload = %d %s", rec.Code, rec.Body)
	}
	if keys := listKeys(t, d, "chunks"); len(keys) != 0 {
		t.Errorf("staging left %v", keys)
	}
}
