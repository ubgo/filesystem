package fshttp_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

// zipEnv serves ServeZip for whatever the test sets, over a disk whose
// reads run an optional hook first.
type zipEnv struct {
	disk   *filesystem.Disk
	srv    *httptest.Server
	files  []fshttp.ZipFile
	name   string
	opts   fshttp.ZipOptions
	reads  atomic.Int32
	onRead atomic.Pointer[func(key string)]
	mu     sync.Mutex
	errs   []error
}

func newZipEnv(t *testing.T) *zipEnv {
	t.Helper()
	z := &zipEnv{}
	hook := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpRead {
			z.reads.Add(1)
			if fn := z.onRead.Load(); fn != nil {
				(*fn)(op.Key)
			}
		}
		return call(ctx)
	}
	z.disk, _ = filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(hook))
	for k, v := range map[string]string{"a.txt": "alpha", "dir/b.txt": "bravo", "photo.jpg": strings.Repeat("j", 1000)} {
		if _, err := z.disk.WriteBytes(t.Context(), k, []byte(v), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	z.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opts := z.opts
		opts.OnError = func(err error) {
			z.mu.Lock()
			z.errs = append(z.errs, err)
			z.mu.Unlock()
		}
		fshttp.ServeZip(w, r, z.disk, z.name, z.files, opts)
	}))
	t.Cleanup(z.srv.Close)
	return z
}

func (z *zipEnv) get(t *testing.T, method string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, z.srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The connection was reset before the headers arrived.
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func TestZipStreamsTheFiles(t *testing.T) {
	z := newZipEnv(t)
	z.files = []fshttp.ZipFile{{Key: "a.txt"}, {Key: "dir/b.txt", Name: "renamed/b.txt"}, {Key: "photo.jpg"}}
	z.name = "résumé files.zip"
	resp, body, err := z.get(t, http.MethodGet)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, %v", resp.StatusCode, err)
	}
	if resp.Header.Get("Content-Type") != "application/zip" || resp.Header.Get("Cache-Control") != fshttp.DefaultSignedCacheControl || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", resp.Header)
	}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err != nil || params["filename"] != z.name {
		t.Errorf("Content-Disposition %q: %v %v", resp.Header.Get("Content-Disposition"), params, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a.txt": "alpha", "renamed/b.txt": "bravo", "photo.jpg": strings.Repeat("j", 1000)}
	if len(zr.File) != len(want) {
		t.Fatalf("%d entries", len(zr.File))
	}
	for _, f := range zr.File {
		rc, _ := f.Open()
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != want[f.Name] || f.Method != zip.Store || f.Modified.IsZero() {
			t.Errorf("%s: %q, method %d, modified %v", f.Name, got, f.Method, f.Modified)
		}
	}

	z.opts.Deflate = true
	z.name = ""
	resp, body, _ = z.get(t, http.MethodGet)
	zr, err = zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || zr.File[2].Method != zip.Deflate || !strings.Contains(resp.Header.Get("Content-Disposition"), "download.zip") {
		t.Errorf("deflated: %v, %v", err, resp.Header)
	}
}

// TestZipDoesNotBuffer pins streaming: the second object is not read until
// the client has received the start of the archive, which a handler that
// built the archive in memory first could never deliver.
func TestZipDoesNotBuffer(t *testing.T) {
	z := newZipEnv(t)
	big := bytes.Repeat([]byte("x"), 256<<10)
	_, _ = z.disk.WriteBytes(t.Context(), "big.bin", big, filesystem.WriteOptions{})
	z.files = []fshttp.ZipFile{{Key: "big.bin"}, {Key: "a.txt"}}
	started := make(chan struct{})
	wait := func(key string) {
		if key == "a.txt" {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
			}
		}
	}
	z.onRead.Store(&wait)
	// The request and the first bytes must both arrive while the second
	// object's read is still held back.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, z.srv.URL, nil)
	head := make([]byte, 4)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- nil
			return
		}
		if _, err := io.ReadFull(resp.Body, head); err != nil {
			_ = resp.Body.Close()
			done <- nil
			return
		}
		done <- resp
	}()
	var resp *http.Response
	select {
	case resp = <-done:
		if resp == nil || string(head) != "PK\x03\x04" {
			t.Fatalf("first bytes %q", head)
		}
	case <-time.After(3 * time.Second):
		close(started)
		t.Fatal("no byte arrived before the last object was read: the archive is buffered")
	}
	defer func() { _ = resp.Body.Close() }()
	close(started)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestZipRefusesBeforeSending(t *testing.T) {
	z := newZipEnv(t)
	cases := []struct {
		files  []fshttp.ZipFile
		opts   fshttp.ZipOptions
		status int
	}{
		{nil, fshttp.ZipOptions{}, http.StatusBadRequest},
		{[]fshttp.ZipFile{{Key: "a.txt"}, {Key: "photo.jpg"}}, fshttp.ZipOptions{MaxFiles: 1}, http.StatusBadRequest},
		{[]fshttp.ZipFile{{Key: "a.txt"}, {Key: "photo.jpg"}}, fshttp.ZipOptions{MaxBytes: 500}, http.StatusRequestEntityTooLarge},
		{[]fshttp.ZipFile{{Key: "a.txt"}, {Key: "missing.txt"}}, fshttp.ZipOptions{}, http.StatusNotFound},
		{[]fshttp.ZipFile{{Key: "a.txt"}, {Key: "dir/b.txt", Name: "A.TXT"}}, fshttp.ZipOptions{}, http.StatusBadRequest},
		{[]fshttp.ZipFile{{Key: "a.txt", Name: "x"}, {Key: "dir/b.txt", Name: "x/b.txt"}}, fshttp.ZipOptions{}, http.StatusBadRequest},
		{[]fshttp.ZipFile{{Key: "dir/b.txt", Name: "x/y/b.txt"}, {Key: "a.txt", Name: "X/Y"}}, fshttp.ZipOptions{}, http.StatusBadRequest},
	}
	for _, name := range []string{"../evil.txt", "/etc/passwd", `a\b.txt`, "C:/x.txt", "a/../b.txt", "a/./b.txt", "a//b.txt", "dir/", ".", "..", "bell\a.txt", "\xff.txt", strings.Repeat("n", fshttp.MaxZipNameBytes+1)} {
		cases = append(cases, struct {
			files  []fshttp.ZipFile
			opts   fshttp.ZipOptions
			status int
		}{[]fshttp.ZipFile{{Key: "a.txt", Name: name}}, fshttp.ZipOptions{}, http.StatusBadRequest})
	}
	for _, c := range cases {
		z.files, z.opts = c.files, c.opts
		z.reads.Store(0)
		resp, _, _ := z.get(t, http.MethodGet)
		if resp.StatusCode != c.status || resp.Header.Get("Content-Type") == "application/zip" {
			t.Errorf("%+v: %d (%s), want %d", c.files, resp.StatusCode, resp.Header.Get("Content-Type"), c.status)
		}
		if n := z.reads.Load(); n != 0 {
			t.Errorf("%+v: %d objects read before the refusal", c.files, n)
		}
	}
}

func TestZipHead(t *testing.T) {
	z := newZipEnv(t)
	z.files = []fshttp.ZipFile{{Key: "a.txt"}}
	resp, body, err := z.get(t, http.MethodHead)
	if err != nil || resp.StatusCode != http.StatusOK || len(body) != 0 || resp.Header.Get("Content-Type") != "application/zip" || z.reads.Load() != 0 {
		t.Errorf("HEAD = %d, %d bytes, %d reads, %v", resp.StatusCode, len(body), z.reads.Load(), err)
	}
}

// TestZipAbortsOnAFailureAfterHeaders pins that a failure mid-stream never
// ends in a well-formed but incomplete archive: the connection is reset,
// the client's read fails, and OnError hears why.
func TestZipAbortsOnAFailureAfterHeaders(t *testing.T) {
	cases := map[string]func(z *zipEnv) func(string){
		"read fails": func(*zipEnv) func(string) { return nil },
		"object grew": func(z *zipEnv) func(string) {
			return func(key string) {
				if key == "photo.jpg" {
					_, _ = z.disk.WriteBytes(context.Background(), "photo.jpg", bytes.Repeat([]byte("j"), 2000), filesystem.WriteOptions{})
				}
			}
		},
		"object shrank": func(z *zipEnv) func(string) {
			return func(key string) {
				if key == "photo.jpg" {
					_, _ = z.disk.WriteBytes(context.Background(), "photo.jpg", []byte("j"), filesystem.WriteOptions{})
				}
			}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			z := newZipEnv(t)
			z.files = []fshttp.ZipFile{{Key: "a.txt"}, {Key: "photo.jpg"}}
			if name == "read fails" {
				// A large first entry, so the headers and part of the
				// body are on the wire when the failure comes.
				_, _ = z.disk.WriteBytes(t.Context(), "big.bin", bytes.Repeat([]byte("x"), 256<<10), filesystem.WriteOptions{})
				z.files = append([]fshttp.ZipFile{{Key: "big.bin"}}, z.files...)
			}
			if fn := mk(z); fn != nil {
				z.onRead.Store(&fn)
			} else {
				_ = z.disk.Delete(t.Context(), "photo.jpg")
				// Stat must still succeed: put it back for the plan, then
				// delete it when the first object is read.
				_, _ = z.disk.WriteBytes(t.Context(), "photo.jpg", []byte("x"), filesystem.WriteOptions{})
				del := func(key string) {
					if key == "a.txt" {
						_ = z.disk.Delete(context.Background(), "photo.jpg")
					}
				}
				z.onRead.Store(&del)
			}
			// Small archives abort before the buffered headers are sent
			// (the request fails), larger ones mid-body (the read fails).
			_, body, err := z.get(t, http.MethodGet)
			if err == nil {
				if _, zerr := zip.NewReader(bytes.NewReader(body), int64(len(body))); zerr == nil {
					t.Fatal("a failed download produced a valid archive")
				}
				t.Fatal("the client's read succeeded")
			}
			z.mu.Lock()
			defer z.mu.Unlock()
			if len(z.errs) != 1 {
				t.Errorf("OnError calls: %v", z.errs)
			}
		})
	}
}

// TestZipClientGone pins that a client disconnecting mid-download is
// reported and does not leave the handler writing.
func TestZipClientGone(t *testing.T) {
	z := newZipEnv(t)
	big := bytes.Repeat([]byte("x"), 4<<20)
	_, _ = z.disk.WriteBytes(t.Context(), "big.bin", big, filesystem.WriteOptions{})
	z.files = []fshttp.ZipFile{{Key: "big.bin"}, {Key: "a.txt"}}
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, z.srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadFull(resp.Body, make([]byte, 1024))
	cancel()
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		z.mu.Lock()
		n := len(z.errs)
		z.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("OnError never heard of the disconnect")
}
