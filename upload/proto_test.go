package upload_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func TestProgress(t *testing.T) {
	var calls []int64
	p := anyFile
	p.Progress = func(_ context.Context, info upload.Info, n int64) {
		if info.Filename != "big.bin" {
			t.Errorf("progress for %q", info.Filename)
		}
		calls = append(calls, n)
	}
	size := 3*upload.ProgressStep + 10
	if _, err := upload.Receive(t.Context(), newDisk(t, memory.New()), strings.NewReader(strings.Repeat("x", size)), "big.bin", p); err != nil {
		t.Fatal(err)
	}
	if len(calls) < 3 || calls[len(calls)-1] != int64(size) {
		t.Fatalf("progress calls %v", calls)
	}
	for i := 1; i < len(calls)-1; i++ {
		if calls[i]-calls[i-1] < upload.ProgressStep {
			t.Errorf("progress reported more often than every %d bytes: %v", upload.ProgressStep, calls)
		}
	}
}

func TestSafePath(t *testing.T) {
	for in, want := range map[string]string{
		"a.png":                        "a.png",
		"Holiday/2024/IMG_1.jpg":       "Holiday/2024/IMG_1.jpg",
		`Holiday\2024\IMG_1.jpg`:       "Holiday/2024/IMG_1.jpg",
		"../../etc/passwd":             "etc/passwd",
		"/abs/./x//y.txt":              "abs/x/y.txt",
		"dir/\x00evil\n.txt":           "dir/evil.txt",
		"..":                           "file",
		strings.Repeat("d/", 40) + "f": strings.Repeat("d/", upload.MaxPathDepth) + "f",
	} {
		if got := upload.SafePath(in); got != want {
			t.Errorf("SafePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFolderUpload(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	p := images
	p.Key = upload.FolderKey("drive/u1")
	res, err := upload.Receive(ctx, d, strings.NewReader(pngData), "Holiday/../2024/a.png", p)
	if err != nil || res.Key != "drive/u1/Holiday/2024/a.png" || res.Path != "Holiday/2024/a.png" || res.Filename != "a.png" {
		t.Fatalf("folder upload %+v, %v", res, err)
	}
	// A Process step that renames the file keeps its folder.
	p.Process = func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		return body, upload.Info{Filename: "clean.png"}, nil
	}
	if res, _ := upload.Receive(ctx, d, strings.NewReader(pngData), "Holiday/raw.png", p); res.Key != "drive/u1/Holiday/clean.png" {
		t.Errorf("renamed in a folder: %q", res.Key)
	}
	// The form handler keeps the relative path the client sent.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="Trip/day1/b.png"`)
	h.Set("Content-Type", "image/png")
	w, _ := mw.CreatePart(h)
	_, _ = io.WriteString(w, pngData)
	_ = mw.Close()
	rec := post(t, upload.Handler(d, images, upload.HandlerOptions{}), &buf, mw.FormDataContentType())
	var resp upload.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Files) != 1 || resp.Files[0].Path != "Trip/day1/b.png" {
		t.Errorf("form folder upload %d %s", rec.Code, rec.Body)
	}
}

// statFailsFor fails Stat of keys containing a marker, as a store that
// cannot answer for the content key would.
type statFailsFor struct {
	*memory.Driver
	marker string
}

func (s statFailsFor) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if strings.Contains(key, s.marker) {
		return filesystem.Entry{}, filesystem.ErrUnavailable
	}
	return s.Driver.Stat(ctx, key)
}

func TestContentAddressed(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	p := images
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	a, err := upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != "blobs/"+a.SHA256+".png" {
		t.Errorf("content key %q", a.Key)
	}
	dup, err := upload.Receive(ctx, d, strings.NewReader(pngData), "copy.png", p)
	if err != nil || dup.Key != a.Key {
		t.Errorf("duplicate %+v, %v", dup, err)
	}
	other, _ := upload.Receive(ctx, d, strings.NewReader(pngData+"other"), "b.png", p)
	if other.Key == a.Key {
		t.Error("different content shares a key")
	}
	if n := count(t, d); n != 2 {
		t.Errorf("%d objects, want the two distinct contents only", n)
	}
	// Confirm settles a direct upload the same way.
	_, _ = d.WriteBytes(ctx, "blobs/.incoming/direct", []byte(pngData), filesystem.WriteOptions{})
	if res, err := upload.Confirm(ctx, d, "blobs/.incoming/direct", "d.png", p); err != nil || res.Key != a.Key {
		t.Errorf("confirm settle %+v, %v", res, err)
	}
	// A store that cannot say whether the key exists refuses the upload
	// and leaves nothing behind.
	broken := newDisk(t, statFailsFor{memory.New(), "blobs/"})
	_, err = upload.Receive(ctx, broken, strings.NewReader(pngData), "a.png", p)
	wantCode(t, err, filesystem.CodeUnavailable)
	if n := count(t, broken); n != 0 {
		t.Errorf("%d objects left after a failed settle", n)
	}
}

// postWithKey posts body to h with an Idempotency-Key and user.
func postWithKey(t *testing.T, h http.Handler, body func() (io.Reader, string), key, user string) *httptest.ResponseRecorder {
	t.Helper()
	b, ct := body()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/upload", b)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-User", user)
	if key != "" {
		req.Header.Set(upload.HeaderIdempotencyKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func byUser(r *http.Request) (string, error) {
	if u := r.Header.Get("X-User"); u != "" {
		return u, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "sign in")
}

func TestIdempotentUploads(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store, err := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	d := newDisk(t, memory.New())
	h := upload.Handler(d, images, upload.HandlerOptions{Idempotency: &upload.Idempotency{Store: store, Owner: byUser}})
	png := func() (io.Reader, string) { return multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil) }
	first := postWithKey(t, h, png, "k1", "u1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	// An offline queue retries three times: the same response, one object.
	for range 3 {
		again := postWithKey(t, h, png, "k1", "u1")
		if again.Code != http.StatusOK || again.Body.String() != first.Body.String() || again.Header().Get(upload.HeaderIdempotentReplayed) != "true" {
			t.Errorf("replay = %d %s %v", again.Code, again.Body, again.Header())
		}
	}
	if n := count(t, d); n != 1 {
		t.Errorf("%d objects after retries", n)
	}
	// Another user's identical key is another request.
	if other := postWithKey(t, h, png, "k1", "u2"); other.Code != http.StatusCreated {
		t.Errorf("other owner = %d", other.Code)
	}
	// A failed request is not recorded: its retry runs.
	html := func() (io.Reader, string) { return multipartBody(t, []formFile{{"file", "a.html", htmlData}}, nil) }
	if bad := postWithKey(t, h, html, "k2", "u1"); bad.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("refused = %d", bad.Code)
	}
	if retry := postWithKey(t, h, png, "k2", "u1"); retry.Code != http.StatusCreated {
		t.Errorf("retry after a failure = %d", retry.Code)
	}
	// After the TTL the key is forgotten.
	now = now.Add(2 * time.Hour)
	if late := postWithKey(t, h, png, "k1", "u1"); late.Code != http.StatusCreated {
		t.Errorf("after the TTL = %d", late.Code)
	}
	// Without the header nothing changes; malformed setups are refused.
	if plain := postWithKey(t, h, png, "", "u1"); plain.Code != http.StatusCreated {
		t.Errorf("no key = %d", plain.Code)
	}
	for name, c := range map[string]struct {
		h    http.Handler
		key  string
		user string
		code int
	}{
		"no store": {upload.Handler(d, images, upload.HandlerOptions{Idempotency: &upload.Idempotency{Owner: byUser}}), "k", "u1", http.StatusInternalServerError},
		"long key": {h, strings.Repeat("k", 256), "u1", http.StatusBadRequest},
		"no owner": {h, "k3", "", http.StatusForbidden},
	} {
		if rec := postWithKey(t, c.h, png, c.key, c.user); rec.Code != c.code {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	if _, err := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("zero TTL: %v", err)
	}
}

func TestIdempotencyWhileRunning(t *testing.T) {
	store, _ := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: time.Hour})
	d := newDisk(t, memory.New())
	entered, release := make(chan struct{}), make(chan struct{})
	h := upload.Handler(d, images, upload.HandlerOptions{
		Idempotency: &upload.Idempotency{Store: store, Owner: byUser},
		OnStored: func(context.Context, *http.Request, []upload.Result) error {
			close(entered)
			<-release
			return nil
		},
	})
	png := func() (io.Reader, string) { return multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil) }
	var wg sync.WaitGroup
	wg.Go(func() { postWithKey(t, h, png, "k", "u1") })
	<-entered
	if twin := postWithKey(t, h, png, "k", "u1"); twin.Code != http.StatusConflict {
		t.Errorf("twin while running = %d %s", twin.Code, twin.Body)
	}
	close(release)
	wg.Wait()
}

func TestIdempotentTickets(t *testing.T) {
	store, _ := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: time.Hour})
	h := upload.PresignHandlerWith(newDisk(t, presigning{memory.New()}), images, time.Minute, upload.PresignHandlerOptions{Idempotency: &upload.Idempotency{Store: store, Owner: byUser}})
	body := func() (io.Reader, string) {
		return strings.NewReader(`{"filename":"a.png","content_type":"image/png","size":10}`), "application/json"
	}
	a := postWithKey(t, h, body, "t1", "u1")
	b := postWithKey(t, h, body, "t1", "u1")
	if a.Code != 200 || a.Body.String() != b.Body.String() {
		t.Errorf("tickets %s / %s", a.Body, b.Body)
	}
}

func TestLimitsHandler(t *testing.T) {
	p := images
	p.MinSize = 1
	p.Extensions = []string{".png"}
	h := upload.LimitsHandler(p, upload.LimitsOptions{MaxWidth: 4096, MaxHeight: 4096, Resolve: func(r *http.Request, base upload.Policy) (upload.Policy, error) {
		if r.URL.Query().Get("plan") == "broken" {
			return upload.Policy{}, nil
		}
		if r.URL.Query().Get("plan") == "pro" {
			base.MaxSize = 100 << 20
		}
		return base, nil
	}})
	get := func(target, method string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
		return rec
	}
	var l upload.Limits
	rec := get("/limits", http.MethodGet)
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil || l.MaxBytes != 1<<20 || l.MinBytes != 1 || l.MaxWidth != 4096 || l.Allowed[0] != "image/*" || len(l.Denied) == 0 || l.Extensions[0] != ".png" {
		t.Errorf("limits %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(get("/limits?plan=pro", http.MethodGet).Body.Bytes(), &l)
	if l.MaxBytes != 100<<20 {
		t.Errorf("pro plan max %d", l.MaxBytes)
	}
	for target, code := range map[string]int{"/limits?plan=broken": 500} {
		if rec := get(target, http.MethodGet); rec.Code != code {
			t.Errorf("%s = %d", target, rec.Code)
		}
	}
	if rec := get("/limits", http.MethodPost); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
	// Active content is listed as denied unless the policy allows it.
	active := images
	active.AllowActiveContent = true
	if l := upload.LimitsOf(active, upload.LimitsOptions{}); len(l.Denied) != 0 {
		t.Errorf("denied with active content allowed: %v", l.Denied)
	}
}

// chunkBody builds one chunk request with Dropzone-style fields.
func chunkBody(t *testing.T, fields upload.ChunkFields, id string, index, total int, size int64, filename, data string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{fields.UploadID: id, fields.Index: fmt.Sprint(index), fields.Total: fmt.Sprint(total), fields.TotalSize: fmt.Sprint(size)} {
		_ = mw.WriteField(k, v)
	}
	w, _ := mw.CreateFormFile(fields.File, filename)
	_, _ = io.WriteString(w, data)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func sendChunk(t *testing.T, h http.Handler, user string, body io.Reader, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chunks", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-User", user)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChunkedUpload(t *testing.T) {
	ctx := t.Context()
	file := pngData + strings.Repeat("-", 40)
	parts := []string{file[:20], file[20:40], file[40:]}
	for name, fields := range map[string]upload.ChunkFields{"dropzone": {}, "resumable": upload.ResumableFields()} {
		t.Run(name, func(t *testing.T) {
			d := newDisk(t, memory.New())
			var stored []upload.Result
			h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, Fields: fields, OnStored: func(_ context.Context, _ *http.Request, f upload.Result) error {
				stored = append(stored, f)
				return nil
			}})
			f := fields
			if f == (upload.ChunkFields{}) {
				f = upload.DropzoneFields()
			}
			// Out of order, one retried.
			var last *httptest.ResponseRecorder
			for _, i := range []int{1, 0, 1, 2} {
				b, ct := chunkBody(t, f, "file-1", i+f.IndexBase, 3, int64(len(file)), "Trip/photo.png", parts[i])
				last = sendChunk(t, h, "u1", b, ct)
			}
			var resp upload.Response
			if last.Code != http.StatusCreated || json.Unmarshal(last.Body.Bytes(), &resp) != nil || len(resp.Files) != 1 || resp.Files[0].Path != "Trip/photo.png" {
				t.Fatalf("last chunk = %d %s", last.Code, last.Body)
			}
			if got, _ := d.ReadAll(ctx, resp.Files[0].Key); string(got) != file || len(stored) != 1 {
				t.Errorf("assembled %q, OnStored %d", got, len(stored))
			}
			if n := count(t, d); n != 1 {
				t.Errorf("%d objects left: the chunks must be gone", n)
			}
		})
	}
}

func TestChunkedRefusals(t *testing.T) {
	f := upload.DropzoneFields()
	d := newDisk(t, memory.New())
	h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, MaxChunkBytes: 64})
	for name, c := range map[string]struct {
		body func() (io.Reader, string)
		user string
		code int
	}{
		"refused type at once": {func() (io.Reader, string) { return chunkBody(t, f, "case1", 0, 2, 100, "a.html", htmlData) }, "u1", http.StatusUnsupportedMediaType},
		"declared too large":   {func() (io.Reader, string) { return chunkBody(t, f, "case2", 0, 2, 2<<20, "a.png", pngData) }, "u1", http.StatusRequestEntityTooLarge},
		"declared empty":       {func() (io.Reader, string) { return chunkBody(t, f, "case3", 0, 1, 0, "a.png", "") }, "u1", http.StatusUnsupportedMediaType},
		"bad upload id":        {func() (io.Reader, string) { return chunkBody(t, f, "../x", 0, 2, 100, "a.png", pngData) }, "u1", http.StatusBadRequest},
		"index out of range":   {func() (io.Reader, string) { return chunkBody(t, f, "case4", 5, 2, 100, "a.png", pngData) }, "u1", http.StatusBadRequest},
		"too many chunks": {func() (io.Reader, string) {
			return chunkBody(t, f, "case5", 0, upload.MaxChunks+1, 100, "a.png", pngData)
		}, "u1", http.StatusBadRequest},
		"chunk too large": {func() (io.Reader, string) {
			return chunkBody(t, f, "case6", 1, 2, 1000, "a.png", strings.Repeat("y", 100))
		}, "u1", http.StatusRequestEntityTooLarge},
		"no owner":      {func() (io.Reader, string) { return chunkBody(t, f, "case7", 0, 2, 100, "a.png", pngData) }, "", http.StatusForbidden},
		"not multipart": {func() (io.Reader, string) { return strings.NewReader("x"), "text/plain" }, "u1", http.StatusBadRequest},
		"no file part": {func() (io.Reader, string) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField(f.UploadID, "x")
			_ = mw.Close()
			return &buf, mw.FormDataContentType()
		}, "u1", http.StatusBadRequest},
		"non-integer fields": {func() (io.Reader, string) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField(f.UploadID, "x")
			_ = mw.WriteField(f.Index, "first")
			w, _ := mw.CreateFormFile(f.File, "a.png")
			_, _ = io.WriteString(w, pngData)
			_ = mw.Close()
			return &buf, mw.FormDataContentType()
		}, "u1", http.StatusBadRequest},
	} {
		b, ct := c.body()
		if rec := sendChunk(t, h, c.user, b, ct); rec.Code != c.code {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	// Chunks that do not add up to the declared size are refused, and
	// nothing is left.
	b, ct := chunkBody(t, f, "short", 0, 1, 500, "a.png", pngData)
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusBadRequest {
		t.Errorf("short = %d %s", rec.Code, rec.Body)
	}
	// Nothing but staging (uploads a client may still finish, left for
	// the sweep) is stored.
	for _, k := range listKeys(t, d, "") {
		if !strings.HasPrefix(k, "chunks/") || strings.Contains(k, "/short/") {
			t.Errorf("left %q", k)
		}
	}
	// Options are refused when the handler is built, never per chunk.
	for name, o := range map[string]struct {
		disk *filesystem.Disk
		opts upload.ChunkedOptions
	}{
		"no staging":             {d, upload.ChunkedOptions{Owner: byUser}},
		"root staging":           {d, upload.ChunkedOptions{Staging: "/", Owner: byUser}},
		"no owner func":          {d, upload.ChunkedOptions{Staging: "c"}},
		"no cond. write":         {newDisk(t, unconditional{memory.New()}), upload.ChunkedOptions{Staging: "c", Owner: byUser}},
		"staging no cond. write": {d, upload.ChunkedOptions{Staging: "c", Owner: byUser, StagingDisk: newDisk(t, unconditional{memory.New()})}},
		"staging no CAS":         {d, upload.ChunkedOptions{Staging: "c", Owner: byUser, StagingDisk: newDisk(t, noCAS{memory.New()})}},
	} {
		if h, err := upload.Chunked(o.disk, images, o.opts); h != nil || filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s = %v, %v", name, h, err)
		}
	}
	// Request errors and method.
	for name, c := range map[string]struct {
		h    http.Handler
		code int
	}{
		"broken resolver": {chunked(t, d, images, upload.ChunkedOptions{Staging: "c", Owner: byUser, Resolve: func(*http.Request, upload.Policy) (upload.Policy, error) { return upload.Policy{}, nil }}), http.StatusInternalServerError},
	} {
		b, ct := chunkBody(t, f, "x", 0, 2, 100, "a.png", pngData)
		if rec := sendChunk(t, c.h, "u1", b, ct); rec.Code != c.code {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/chunks", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
}

func TestChunkedOwnersAndAssembly(t *testing.T) {
	ctx := t.Context()
	f := upload.DropzoneFields()
	d := newDisk(t, memory.New())
	failStore := errors.New("no row")
	failing := false
	h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, AssemblyTimeout: time.Minute, OnStored: func(context.Context, *http.Request, upload.Result) error {
		if failing {
			return filesystem.NewError(filesystem.CodeUnavailable, failStore)
		}
		return nil
	}})
	file := pngData + "tail"
	// u2 cannot complete u1's upload with the same ID.
	b, ct := chunkBody(t, f, "same", 0, 2, int64(len(file)), "a.png", pngData)
	sendChunk(t, h, "u1", b, ct)
	b, ct = chunkBody(t, f, "same", 1, 2, int64(len(file)), "a.png", "tail")
	if rec := sendChunk(t, h, "u2", b, ct); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"received":1`) {
		t.Errorf("another owner's chunk = %d %s", rec.Code, rec.Body)
	}
	// While another request is assembling, a completing chunk answers 200.
	var manifest string
	for _, k := range listKeys(t, d, "chunks") {
		if strings.HasSuffix(k, "/same/0") { // u1's chunk; u2's is chunk 1
			manifest = strings.TrimSuffix(k, "0") + "manifest.json"
		}
	}
	setAssembling := func(since time.Time) {
		data, _ := d.ReadAll(ctx, manifest)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		m["assembling_since"] = since
		data, _ = json.Marshal(m)
		_, _ = d.WriteBytes(ctx, manifest, data, filesystem.WriteOptions{})
	}
	setAssembling(time.Now())
	b, ct = chunkBody(t, f, "same", 1, 2, int64(len(file)), "a.png", "tail")
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
		t.Errorf("while assembling = %d %s", rec.Code, rec.Body)
	}
	// An assembler that crashed long ago is taken over.
	setAssembling(time.Now().Add(-time.Hour))
	// OnStored failing deletes the file and the chunks.
	failing = true
	b, ct = chunkBody(t, f, "same", 1, 2, int64(len(file)), "a.png", "tail")
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("OnStored failure = %d %s", rec.Code, rec.Body)
	}
	// Only u2's own upload (its chunk and manifest) remains.
	for _, k := range listKeys(t, d, "") {
		if strings.HasPrefix(k, strings.TrimSuffix(manifest, "manifest.json")) || !strings.HasPrefix(k, "chunks/") {
			t.Errorf("left %q after a failed assembly", k)
		}
	}
}

func listKeys(t *testing.T, d *filesystem.Disk, dir string) []string {
	t.Helper()
	var keys []string
	for e, err := range d.List(t.Context(), dir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, e.Key)
	}
	return keys
}

// chunkFaults fails chosen steps of chunked uploads and content settling.
type chunkFaults struct {
	*memory.Driver
	failList, failManifest, failChunkRead, splitReads, failMove, failDelete bool
	// conflictManifest makes every compare-and-swap of a manifest lose;
	// vanishingManifest makes a manifest both missing and existing, as an
	// endlessly racing first chunk would.
	conflictManifest, vanishingManifest, failManifestUpdate bool
	// afterChunk runs after a chunk is stored, to change the manifest
	// under a request in flight.
	afterChunk func(ctx context.Context)
	// largest records the largest chunk object ever stored.
	largest *int64
}

func (c chunkFaults) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	if c.failList {
		return filesystem.Page{}, filesystem.ErrUnavailable
	}
	return c.Driver.List(ctx, dir, o)
}

func (c chunkFaults) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	isManifest := strings.HasSuffix(key, "manifest.json")
	switch {
	case c.failManifest && isManifest:
		return filesystem.Entry{}, filesystem.ErrUnavailable
	case c.failManifestUpdate && isManifest && o.IfMatch != "":
		return filesystem.Entry{}, filesystem.ErrUnavailable
	case c.conflictManifest && isManifest && o.IfMatch != "":
		return filesystem.Entry{}, filesystem.ErrConflict
	case c.vanishingManifest && isManifest:
		return filesystem.Entry{}, filesystem.ErrExists
	}
	e, err := c.Driver.Write(ctx, key, body, o)
	isChunk := err == nil && strings.HasPrefix(key, "chunks/") && !strings.HasSuffix(key, ".json")
	if isChunk && c.largest != nil && e.Size > *c.largest {
		*c.largest = e.Size
	}
	if isChunk && c.afterChunk != nil {
		c.afterChunk(ctx)
	}
	return e, err
}

func (c chunkFaults) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if c.vanishingManifest && strings.HasSuffix(key, "manifest.json") {
		return filesystem.Entry{}, filesystem.ErrNotFound
	}
	return c.Driver.Stat(ctx, key)
}

func (c chunkFaults) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	inChunks := strings.HasPrefix(key, "chunks/") && !strings.HasSuffix(key, ".json")
	if c.failChunkRead && inChunks {
		return nil, filesystem.ErrUnavailable
	}
	rc, err := c.Driver.Read(ctx, key, r)
	if c.splitReads && inChunks && err == nil {
		// Report the last bytes together with io.EOF, as some readers do.
		return io.NopCloser(iotest.DataErrReader(rc)), nil
	}
	return rc, err
}

func (c chunkFaults) Move(ctx context.Context, src, dst string) (filesystem.Entry, error) {
	if c.failMove {
		return filesystem.Entry{}, filesystem.ErrUnavailable
	}
	return c.Driver.Move(ctx, src, dst)
}

func (c chunkFaults) Delete(ctx context.Context, key string) error {
	if c.failDelete && strings.Contains(key, ".incoming") {
		return filesystem.ErrUnavailable
	}
	return c.Driver.Delete(ctx, key)
}

func TestChunkedBackendFailures(t *testing.T) {
	f := upload.DropzoneFields()
	file := pngData + "tail"
	send := func(drv chunkFaults, p upload.Policy) *httptest.ResponseRecorder {
		h := chunked(t, newDisk(t, drv), p, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
		b, ct := chunkBody(t, f, "id", 0, 2, int64(len(file)), "a.png", pngData)
		first := sendChunk(t, h, "u1", b, ct)
		if first.Code != http.StatusOK {
			return first
		}
		b, ct = chunkBody(t, f, "id", 1, 2, int64(len(file)), "a.png", "tail")
		return sendChunk(t, h, "u1", b, ct)
	}
	refusing := images
	refusing.Inspect = func(context.Context, *filesystem.Disk, upload.Result) error {
		return filesystem.Errorf(filesystem.CodeRejected, "inspection failed")
	}
	for name, c := range map[string]struct {
		drv  chunkFaults
		p    upload.Policy
		code int
	}{
		"listing fails":        {chunkFaults{Driver: memory.New(), failList: true}, images, http.StatusServiceUnavailable},
		"manifest write fails": {chunkFaults{Driver: memory.New(), failManifest: true}, images, http.StatusServiceUnavailable},
		"chunk read fails":     {chunkFaults{Driver: memory.New(), failChunkRead: true}, images, http.StatusServiceUnavailable},
		"assembled refused":    {chunkFaults{Driver: memory.New()}, refusing, http.StatusUnsupportedMediaType},
		"reads end with data":  {chunkFaults{Driver: memory.New(), splitReads: true}, images, http.StatusCreated},
	} {
		if rec := send(c.drv, c.p); rec.Code != c.code {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestChunkedBrokenBodies(t *testing.T) {
	f := upload.DropzoneFields()
	h := chunked(t, newDisk(t, memory.New()), images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
	b, ct := chunkBody(t, f, "id", 0, 2, 100, "a.png", strings.Repeat("p", 600))
	whole, _ := io.ReadAll(b)
	text := string(whole)
	errBroken := errors.New("connection reset")
	for name, cut := range map[string]int{
		"in part headers":  strings.Index(text, "\r\n--") + strings.Index(text[strings.Index(text, "\r\n--"):], "Content-Disposition") + 5,
		"inside a field":   strings.Index(text, f.UploadID) + len(f.UploadID) + 8,
		"inside the chunk": strings.Index(text, "pppp") + 10,
	} {
		body := io.MultiReader(strings.NewReader(text[:cut]), iotest.ErrReader(errBroken))
		if rec := sendChunk(t, h, "u1", body, ct); rec.Code < 400 {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestContentAddressedFailures(t *testing.T) {
	p := images
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	ctx := t.Context()
	moveFails := newDisk(t, chunkFaults{Driver: memory.New(), failMove: true})
	_, err := upload.Receive(ctx, moveFails, strings.NewReader(pngData), "a.png", p)
	wantCode(t, err, filesystem.CodeUnavailable)
	deleteFails := newDisk(t, chunkFaults{Driver: memory.New(), failDelete: true})
	_, _ = upload.Receive(ctx, deleteFails, strings.NewReader(pngData), "a.png", p)
	_, err = upload.Receive(ctx, deleteFails, strings.NewReader(pngData), "dup.png", p)
	wantCode(t, err, filesystem.CodeUnavailable)
}

// sendFile sends file as chunks of the given sizes, in order, and returns
// the last response.
func sendFile(t *testing.T, h http.Handler, user, id, file string, sizes ...int) *httptest.ResponseRecorder {
	t.Helper()
	f := upload.DropzoneFields()
	var rec *httptest.ResponseRecorder
	off := 0
	for i, n := range sizes {
		b, ct := chunkBody(t, f, id, i, len(sizes), int64(len(file)), "a.png", file[off:off+n])
		rec = sendChunk(t, h, user, b, ct)
		off += n
	}
	return rec
}

// One upload can never stage more than its declared size, however many
// chunks or bytes per chunk the client sends.
func TestChunkedStagesAtMostTheDeclaredSize(t *testing.T) {
	f := upload.DropzoneFields()
	file := pngData + strings.Repeat("-", 71) // 100 bytes
	d := newDisk(t, memory.New())
	h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
	b, ct := chunkBody(t, f, "bomb", 0, 3, 100, "a.png", file[:60])
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
		t.Fatalf("first chunk = %d %s", rec.Code, rec.Body)
	}
	// 60 + 60 would pass the declared 100: refused, nothing staged.
	b, ct = chunkBody(t, f, "bomb", 1, 3, 100, "a.png", strings.Repeat("x", 60))
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the declared size = %d %s", rec.Code, rec.Body)
	}
	b, ct = chunkBody(t, f, "bomb", 1, 3, 100, "a.png", strings.Repeat("x", 40))
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
		t.Errorf("filling the declared size = %d %s", rec.Code, rec.Body)
	}
	// Nothing is left for a third chunk.
	b, ct = chunkBody(t, f, "bomb", 2, 3, 100, "a.png", "z")
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunk past the full size = %d %s", rec.Code, rec.Body)
	}
	// Another declaration for the same upload is refused.
	b, ct = chunkBody(t, f, "bomb", 1, 3, 90, "a.png", "x")
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusBadRequest {
		t.Errorf("changed declaration = %d %s", rec.Code, rec.Body)
	}
	var staged int64
	for e, err := range d.List(t.Context(), "chunks", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(e.Key, ".json") { // not the manifest or the owner's index
			staged += e.Size
		}
	}
	if staged > 100 {
		t.Errorf("staged %d bytes for a 100-byte file", staged)
	}
	// Chunks that arrive together and pass the size between them: the one
	// recorded last is refused and removed.
	var racing *httptest.ResponseRecorder
	drv := chunkFaults{Driver: memory.New()}
	once := false
	rh := http.Handler(nil)
	drv.afterChunk = func(context.Context) {
		if once {
			return
		}
		once = true
		b, ct := chunkBody(t, f, "race", 1, 2, 100, "a.png", strings.Repeat("y", 60))
		racing = sendChunk(t, rh, "u1", b, ct)
	}
	rh = chunked(t, newDisk(t, drv), images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
	b, ct = chunkBody(t, f, "race", 0, 2, 100, "a.png", file[:60])
	first := sendChunk(t, rh, "u1", b, ct)
	if racing == nil || racing.Code != http.StatusOK || first.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("racing chunks: first %d %s, racer %v", first.Code, first.Body, racing)
	}
}

func TestChunkedUploadsPerOwner(t *testing.T) {
	d := newDisk(t, memory.New())
	h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, MaxUploadsPerOwner: 2})
	f := upload.DropzoneFields()
	start := func(user, id string) int {
		b, ct := chunkBody(t, f, id, 0, 2, 100, "a.png", pngData)
		return sendChunk(t, h, user, b, ct).Code
	}
	if start("u1", "a") != http.StatusOK || start("u1", "b") != http.StatusOK {
		t.Fatal("first two uploads refused")
	}
	if code := start("u1", "c"); code != http.StatusInsufficientStorage {
		t.Errorf("third upload in progress = %d", code)
	}
	if code := start("u2", "c"); code != http.StatusOK {
		t.Errorf("another owner's upload = %d", code)
	}
	// A chunk of an upload already in progress is not a new upload.
	b, ct := chunkBody(t, f, "a", 0, 2, 100, "a.png", pngData)
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
		t.Errorf("retry of an open upload = %d", rec.Code)
	}
}

func TestChunkedConcurrentChunks(t *testing.T) {
	d := newDisk(t, memory.New())
	h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
	file := pngData + strings.Repeat("z", 99)
	const n = 8
	size := (len(file) + n - 1) / n
	f := upload.DropzoneFields()
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := range n {
		wg.Go(func() {
			part := file[i*size : min((i+1)*size, len(file))]
			b, ct := chunkBody(t, f, "par", i, n, int64(len(file)), "a.png", part)
			rec := sendChunk(t, h, "u1", b, ct)
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		})
	}
	wg.Wait()
	created := 0
	for i, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
			var resp upload.Response
			_ = json.Unmarshal([]byte(bodies[i]), &resp)
			if got, _ := d.ReadAll(t.Context(), resp.Files[0].Key); string(got) != file {
				t.Errorf("assembled %q", got)
			}
		case http.StatusOK:
		default:
			t.Errorf("chunk %d = %d %s", i, c, bodies[i])
		}
	}
	if created != 1 {
		t.Errorf("%d requests assembled the file, want one", created)
	}
}

func TestChunkedManifestFailures(t *testing.T) {
	file := pngData + "tail"
	for name, c := range map[string]struct {
		drv  chunkFaults
		code int
	}{
		"swap always loses":        {chunkFaults{Driver: memory.New(), conflictManifest: true}, http.StatusServiceUnavailable},
		"update fails":             {chunkFaults{Driver: memory.New(), failManifestUpdate: true}, http.StatusServiceUnavailable},
		"first chunk keeps racing": {chunkFaults{Driver: memory.New(), vanishingManifest: true}, http.StatusServiceUnavailable},
	} {
		h := chunked(t, newDisk(t, c.drv), images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
		if rec := sendFile(t, h, "u1", "id", file, len(pngData), 4); rec.Code != c.code {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	// A manifest that vanishes while a chunk is stored fails the chunk.
	var gone *filesystem.Disk
	vanish := chunkFaults{Driver: memory.New(), afterChunk: func(ctx context.Context) {
		for _, k := range listKeys(t, gone, "chunks") {
			if strings.HasSuffix(k, "manifest.json") {
				_ = gone.Delete(ctx, k)
			}
		}
	}}
	gone = newDisk(t, vanish)
	if rec := sendFile(t, chunked(t, gone, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser}), "u1", "id", file, len(pngData), 4); rec.Code != http.StatusNotFound {
		t.Errorf("vanished manifest = %d %s", rec.Code, rec.Body)
	}
	// A corrupt manifest is an error, not a fresh start.
	d := newDisk(t, memory.New())
	h := chunked(t, d, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
	b0, ct0 := chunkBody(t, upload.DropzoneFields(), "id", 0, 2, int64(len(file)), "a.png", pngData)
	sendChunk(t, h, "u1", b0, ct0)
	for _, k := range listKeys(t, d, "chunks") {
		if strings.HasSuffix(k, "manifest.json") {
			_, _ = d.WriteBytes(t.Context(), k, []byte("{not json"), filesystem.WriteOptions{})
		}
	}
	b, ct := chunkBody(t, upload.DropzoneFields(), "id", 1, 2, int64(len(file)), "a.png", "tail")
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusInternalServerError {
		t.Errorf("corrupt manifest = %d %s", rec.Code, rec.Body)
	}
	// A manifest without its chunk sizes (written by hand, or an older
	// version) still records chunks.
	for _, k := range listKeys(t, d, "chunks") {
		if strings.HasSuffix(k, "manifest.json") {
			_, _ = d.WriteBytes(t.Context(), k, []byte(fmt.Sprintf(`{"total":2,"size":%d}`, len(file))), filesystem.WriteOptions{})
		}
	}
	b, ct = chunkBody(t, upload.DropzoneFields(), "id", 1, 2, int64(len(file)), "a.png", "tail")
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusOK {
		t.Errorf("manifest without sizes = %d %s", rec.Code, rec.Body)
	}
}

// Chunks can wait on a separate staging disk, so the destination needs
// neither compare-and-swap nor conditional writes.
func TestChunkedStagingDisk(t *testing.T) {
	dest := newDisk(t, unconditional{memory.New()})
	staging := newDisk(t, memory.New())
	h := chunked(t, dest, images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser, StagingDisk: staging})
	file := pngData + "tail"
	if rec := sendFile(t, h, "u1", "id", file, len(pngData), 4); rec.Code != http.StatusCreated {
		t.Fatalf("separate staging = %d %s", rec.Code, rec.Body)
	}
	if n := count(t, staging); n != 0 {
		t.Errorf("%d staging objects left", n)
	}
}

// A duplicate stored with other settings is not deduplicated against the
// existing object, so it can never change who may read it.
func TestContentAddressedRespectsSettings(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	private := images
	private.Key, private.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	if _, err := upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", private); err != nil {
		t.Fatal(err)
	}
	public := private
	public.Visibility = filesystem.VisibilityPublic
	_, err := upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", public)
	wantCode(t, err, filesystem.CodeConflict)
	headed := private
	headed.Headers = upload.StaticHeaders(filesystem.HTTPHeaders{CacheControl: "no-store"})
	_, err = upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", headed)
	wantCode(t, err, filesystem.CodeConflict)
	for _, k := range listKeys(t, d, "") {
		if strings.Contains(k, ".incoming") {
			t.Errorf("refused duplicate left %q", k)
		}
		if e, _ := d.Stat(ctx, k); e.Visibility != filesystem.VisibilityPrivate {
			t.Errorf("%s became %s", k, e.Visibility)
		}
	}
}

// ownerKey carries the uploading tenant in a test request's context.
type ownerKey struct{}

func tenantOf(ctx context.Context) (string, error) {
	if t, ok := ctx.Value(ownerKey{}).(string); ok {
		return t, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "no tenant")
}

// By default identical files are shared only within one owner's uploads.
func TestContentAddressedPerOwner(t *testing.T) {
	d := newDisk(t, memory.New())
	p := images
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Owner: tenantOf})
	as := func(tenant string) context.Context { return context.WithValue(t.Context(), ownerKey{}, tenant) }
	a1, err := upload.Receive(as("t1"), d, strings.NewReader(pngData), "a.png", p)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := upload.Receive(as("t1"), d, strings.NewReader(pngData), "again.png", p)
	b, _ := upload.Receive(as("t2"), d, strings.NewReader(pngData), "a.png", p)
	if a1.Key != a2.Key || a1.Key == b.Key || strings.Contains(a1.Key, "t1") {
		t.Errorf("keys %q %q %q: same owner must share, owners must not, and the owner ID must not show", a1.Key, a2.Key, b.Key)
	}
	// Deleting one owner's file leaves the other's.
	_ = d.Delete(t.Context(), a1.Key)
	if ok, _ := d.Exists(t.Context(), b.Key); !ok {
		t.Error("another owner's file went with it")
	}
	// No owner in the context, or no Owner at all, refuses the upload and
	// leaves nothing.
	_, err = upload.Receive(t.Context(), d, strings.NewReader(pngData), "a.png", p)
	wantCode(t, err, filesystem.CodePermission)
	bare := images
	bare.Key, bare.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{})
	_, err = upload.Receive(as("t1"), d, strings.NewReader(pngData), "a.png", bare)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	for _, k := range listKeys(t, d, "") {
		if strings.Contains(k, ".incoming") {
			t.Errorf("refused upload left %q", k)
		}
	}
}

// sameSettings needs the incoming copy's settings.
type incomingStatFails struct{ *memory.Driver }

func (s incomingStatFails) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if strings.Contains(key, ".incoming") {
		return filesystem.Entry{}, filesystem.ErrUnavailable
	}
	return s.Driver.Stat(ctx, key)
}

func TestContentAddressedSettingsUnknown(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, incomingStatFails{memory.New()})
	p := images
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	_, _ = upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", p)
	_, err := upload.Receive(ctx, d, strings.NewReader(pngData), "b.png", p)
	wantCode(t, err, filesystem.CodeUnavailable)
}

// A key is bound to the request it first ran: the same key with another
// file, another path or a garbled body is refused with 422.
func TestIdempotencyKeyBindsTheRequest(t *testing.T) {
	store, _ := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: time.Hour})
	idem := &upload.Idempotency{Store: store, Owner: byUser}
	d := newDisk(t, memory.New())
	h := upload.Handler(d, images, upload.HandlerOptions{Idempotency: idem})
	file := func(data string) func() (io.Reader, string) {
		return func() (io.Reader, string) { return multipartBody(t, []formFile{{"file", "a.png", data}}, nil) }
	}
	if rec := postWithKey(t, h, file(pngData), "k", "u1"); rec.Code != http.StatusCreated {
		t.Fatalf("first = %d", rec.Code)
	}
	for name, body := range map[string]func() (io.Reader, string){
		"other file": file(pngData + "x"),
		"garbled body": func() (io.Reader, string) {
			return strings.NewReader("--x\r\nbroken"), "multipart/form-data; boundary=x"
		},
	} {
		if rec := postWithKey(t, h, body, "k", "u1"); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
	// A ticket key reused for another declaration.
	ph := upload.PresignHandlerWith(newDisk(t, presigning{memory.New()}), images, time.Minute, upload.PresignHandlerOptions{Idempotency: idem})
	ticket := func(size int) func() (io.Reader, string) {
		return func() (io.Reader, string) {
			return strings.NewReader(fmt.Sprintf(`{"filename":"a.png","content_type":"image/png","size":%d}`, size)), "application/json"
		}
	}
	postWithKey(t, ph, ticket(10), "t", "u1")
	if rec := postWithKey(t, ph, ticket(20), "t", "u1"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("ticket with another size = %d", rec.Code)
	}
	// A replay whose body cannot be read is refused too.
	broken := errors.New("connection reset")
	brokenJSON := func() (io.Reader, string) {
		return io.MultiReader(strings.NewReader(`{"filename"`), iotest.ErrReader(broken)), "application/json"
	}
	if rec := postWithKey(t, ph, brokenJSON, "t", "u1"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unreadable ticket replay = %d", rec.Code)
	}
	brokenFile := func() (io.Reader, string) {
		b, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
		whole, _ := io.ReadAll(b)
		cut := bytes.Index(whole, []byte("IHDR"))
		return io.MultiReader(bytes.NewReader(whole[:cut]), iotest.ErrReader(broken)), ct
	}
	if rec := postWithKey(t, h, brokenFile, "k", "u1"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("unreadable upload replay = %d", rec.Code)
	}
	// A body that goes on long after what the handler read is not
	// recorded, since it cannot be fingerprinted in full.
	long := func() (io.Reader, string) {
		b, ct := multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
		return io.MultiReader(b, strings.NewReader("\r\n"+strings.Repeat("e", 70<<10))), ct
	}
	for range 2 {
		if rec := postWithKey(t, h, long, "long", "u1"); rec.Code != http.StatusCreated {
			t.Errorf("unrecorded request = %d %s", rec.Code, rec.Body)
		}
	}
}

func TestMemoryIdempotencyBounds(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	m, err := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: time.Minute, MaxEntries: 2, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = m.Reserve(ctx, "a")
	_ = m.Complete(ctx, "a", upload.Recorded{Body: []byte("A")})
	_, _, _ = m.Reserve(ctx, "b")
	_, _, _ = m.Reserve(ctx, "c") // full: drops the oldest, a
	if _, done, err := m.Reserve(ctx, "a"); done || err != nil {
		t.Errorf("evicted key still replays: %v %v", done, err)
	}
	if _, _, err := m.Reserve(ctx, "c"); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Errorf("running key: %v", err)
	}
	// Expired entries go, oldest first.
	now = now.Add(2 * time.Minute)
	if _, done, err := m.Reserve(ctx, "c"); done || err != nil {
		t.Errorf("expired running key: %v %v", done, err)
	}
	_ = m.Release(ctx, "missing")
	if _, err := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: time.Minute, MaxEntries: -1}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("negative MaxEntries: %v", err)
	}
}

// A chunk past what is left of the declared size is cut off as it
// streams: it is never stored, not even for a moment.
func TestChunkedNeverStagesPastTheDeclaredSize(t *testing.T) {
	var largest int64
	h := chunked(t, newDisk(t, chunkFaults{Driver: memory.New(), largest: &largest}), images, upload.ChunkedOptions{Staging: "chunks", Owner: byUser})
	f := upload.DropzoneFields()
	b, ct := chunkBody(t, f, "big", 0, 2, 100, "a.png", pngData)
	sendChunk(t, h, "u1", b, ct)
	b, ct = chunkBody(t, f, "big", 1, 2, 100, "a.png", strings.Repeat("x", 5000))
	if rec := sendChunk(t, h, "u1", b, ct); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized chunk = %d", rec.Code)
	}
	if largest > 100 {
		t.Errorf("a %d-byte chunk was stored for a 100-byte file", largest)
	}
}

// chunked builds a Chunked handler whose options the test expects valid.
func chunked(t *testing.T, disk *filesystem.Disk, p upload.Policy, opts upload.ChunkedOptions) http.Handler {
	t.Helper()
	h, err := upload.Chunked(disk, p, opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
