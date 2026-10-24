package fshttp_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

// countingDriver is a memory driver that records every Read with its
// range, counts the readers it hands out and the ones closed, and fails
// every Read with fail when set (Stat keeps working, as for an object
// deleted or a backend throttled between Stat and GET).
type countingDriver struct {
	filesystem.Driver
	mu     sync.Mutex
	reads  []filesystem.ByteRange
	opened int
	closed int
	fail   error
}

func (c *countingDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = append(c.reads, rng)
	if c.fail != nil {
		return nil, c.fail
	}
	rc, err := c.Driver.Read(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	c.opened++
	return &countedReader{ReadCloser: rc, c: c}, nil
}

// Capabilities declares none, so the wrapper owes no optional interface;
// objects are public through the disk's default visibility.
func (c *countingDriver) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{} }

func (c *countingDriver) reset(fail error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads, c.opened, c.closed, c.fail = nil, 0, 0, fail
}

func (c *countingDriver) counts() ([]filesystem.ByteRange, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]filesystem.ByteRange(nil), c.reads...), c.opened, c.closed
}

type countedReader struct {
	io.ReadCloser
	c *countingDriver
}

func (r *countedReader) Close() error {
	r.c.mu.Lock()
	r.c.closed++
	r.c.mu.Unlock()
	return r.ReadCloser.Close()
}

// eagerServer is one of the three handlers that share serveObject,
// answering a request for the object "k" with the given headers.
type eagerServer func(t *testing.T, method string, header http.Header) *httptest.ResponseRecorder

// eagerServers builds Public, Signed and ServeEntry over one counting disk
// holding "k" (ten bytes), each with an ErrorWriter that records the
// status it was handed into *seen.
func eagerServers(t *testing.T, seen *int) (*countingDriver, map[string]eagerServer) {
	t.Helper()
	drv := &countingDriver{Driver: memory.New()}
	signer, err := signedurl.New("http://files.test/files", []signedurl.Key{secret})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("uploads", drv, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic), filesystem.WithURLSigner(signer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "k", []byte("0123456789"), filesystem.WriteOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	m, err := filesystem.NewManager(d)
	if err != nil {
		t.Fatal(err)
	}
	ew := func(w http.ResponseWriter, r *http.Request, status int, err error) {
		*seen = status
		fshttp.PlainErrors(w, r, status, err)
	}
	run := func(h http.Handler, url, method string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, url, nil)
		for k, v := range header {
			req.Header[k] = v
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	public := fshttp.Public(d, fshttp.PublicOptions{Errors: ew})
	signed := http.StripPrefix("/files", fshttp.Signed(m, signer, fshttp.SignedOptions{Errors: ew}))
	return drv, map[string]eagerServer{
		"Public": func(t *testing.T, method string, header http.Header) *httptest.ResponseRecorder {
			return run(public, "/k", method, header)
		},
		"Signed": func(t *testing.T, method string, header http.Header) *httptest.ResponseRecorder {
			req, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			return run(signed, req.URL, method, header)
		},
		"ServeEntry": func(t *testing.T, method string, header http.Header) *httptest.ResponseRecorder {
			e, err := d.Stat(t.Context(), "k")
			if err != nil {
				t.Fatal(err)
			}
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fshttp.ServeEntry(w, r, d, e, fshttp.ServeOptions{Errors: ew})
			})
			return run(h, "/k", method, header)
		},
	}
}

// TestServeReadFailsBeforeStatus proves a read that fails on a plain GET
// is answered with its own status through the ErrorWriter, with
// Retry-After, instead of a 200 with an empty body, on every handler.
func TestServeReadFailsBeforeStatus(t *testing.T) {
	var seen int
	drv, servers := eagerServers(t, &seen)
	unavailable := filesystem.NewError(filesystem.CodeUnavailable, errors.New("throttled")).WithRetryAfter(3 * time.Second)
	cases := []struct {
		name       string
		fail       error
		status     int
		retryAfter string
	}{
		{"deleted since Stat", filesystem.ErrNotFound, http.StatusNotFound, ""},
		{"throttled", unavailable, http.StatusServiceUnavailable, "3"},
	}
	for name, serve := range servers {
		for _, c := range cases {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				drv.reset(c.fail)
				seen = 0
				rec := serve(t, http.MethodGet, nil)
				if rec.Code != c.status || seen != c.status || rec.Header().Get(fshttp.HeaderRetryAfter) != c.retryAfter {
					t.Errorf("got %d (writer saw %d, Retry-After %q) %q, want %d", rec.Code, seen, rec.Header().Get(fshttp.HeaderRetryAfter), rec.Body, c.status)
				}
				if rec.Header().Get("ETag") != "" {
					t.Errorf("error response carries the object's headers: %v", rec.Header())
				}
			})
		}
	}
}

// TestServeReadsOnce proves the eager open is the reader streamed: a plain
// GET makes exactly one full read, a ranged GET one read of its range,
// HEAD and a matching If-None-Match none, and every reader is closed.
func TestServeReadsOnce(t *testing.T) {
	var seen int
	drv, servers := eagerServers(t, &seen)
	for name, serve := range servers {
		t.Run(name, func(t *testing.T) {
			drv.reset(nil)
			rec := serve(t, http.MethodGet, nil)
			etag := rec.Header().Get("ETag")
			reads, opened, closed := drv.counts()
			if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" || len(reads) != 1 || reads[0] != (filesystem.ByteRange{}) || opened != closed {
				t.Errorf("plain GET: %d %q reads %v opened %d closed %d", rec.Code, rec.Body, reads, opened, closed)
			}

			drv.reset(nil)
			rec = serve(t, http.MethodGet, http.Header{"Range": {"bytes=2-4"}})
			reads, opened, closed = drv.counts()
			if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" || len(reads) != 1 || reads[0].Offset != 2 || opened != closed {
				t.Errorf("ranged GET: %d %q reads %v opened %d closed %d", rec.Code, rec.Body, reads, opened, closed)
			}

			for label, c := range map[string]struct {
				method string
				header http.Header
				status int
			}{
				"HEAD":          {http.MethodHead, nil, http.StatusOK},
				"If-None-Match": {http.MethodGet, http.Header{"If-None-Match": {etag}}, http.StatusNotModified},
			} {
				drv.reset(nil)
				rec = serve(t, c.method, c.header)
				reads, _, _ = drv.counts()
				if etag == "" || rec.Code != c.status || len(reads) != 0 {
					t.Errorf("%s: %d reads %v (etag %q)", label, rec.Code, reads, etag)
				}
			}
		})
	}
}

// TestServeEagerReaderClosed proves the reader opened before the status is
// closed even when ServeContent never reads it: an empty object.
func TestServeEagerReaderClosed(t *testing.T) {
	drv := &countingDriver{Driver: memory.New()}
	d, err := filesystem.NewDisk("m", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "empty", nil, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rec := serveEntry(t, d, "empty", nil, fshttp.ServeOptions{})
	reads, opened, closed := drv.counts()
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || len(reads) != 1 || opened != 1 || closed != 1 {
		t.Errorf("empty object: %d %q reads %v opened %d closed %d", rec.Code, rec.Body, reads, opened, closed)
	}
}
