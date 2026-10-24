package remote_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// pngBytes sniffs as image/png.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

// svgBytes is an SVG that runs script: what a hostile server sends as
// "image/png".
var svgBytes = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)

// loopback4 is the address of the "public" test servers. The guard
// refuses loopback, so tests allow exactly this one address; servers on
// [::1] stand for the internal network and stay refused.
var loopback4 = netip.MustParseAddr("127.0.0.1")

func allowLoopback4(a netip.Addr) bool { return a == loopback4 }

// counted is a test server that counts its requests.
type counted struct {
	*httptest.Server
	hits atomic.Int64
}

// serve starts a server on addr ("127.0.0.1:0" public, "[::1]:0"
// internal) answering with h.
func serve(t *testing.T, addr string, h http.HandlerFunc) *counted {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	c := &counted{}
	c.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		h(w, r)
	}))
	c.Listener = l
	c.Start()
	t.Cleanup(c.Close)
	return c
}

// body answers with b under contentType.
func body(contentType string, b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(b)
	}
}

// clock is a settable time source.
type clock struct{ now atomic.Pointer[time.Time] }

func newClock() *clock {
	c := &clock{}
	c.set(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	return c
}
func (c *clock) set(t time.Time)     { c.now.Store(&t) }
func (c *clock) add(d time.Duration) { c.set(c.Now().Add(d)) }
func (c *clock) Now() time.Time      { return *c.now.Load() }
func imagePolicy() upload.Policy {
	return upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
}
func newDisk(t *testing.T) *filesystem.Disk { return mustDisk(t, memory.New()) }

func mustDisk(t *testing.T, d filesystem.Driver) *filesystem.Disk {
	t.Helper()
	disk, err := filesystem.NewDisk("cache", d)
	if err != nil {
		t.Fatal(err)
	}
	return disk
}

// newCache returns a cache on a memory disk that may reach 127.0.0.1 only.
func newCache(t *testing.T, mutate ...func(*remote.Options)) (*remote.Cache, *filesystem.Disk) {
	t.Helper()
	disk := newDisk(t)
	o := remote.Options{Dir: "remote", Policy: imagePolicy(), Fetch: upload.FetchOptions{AllowAddr: allowLoopback4}}
	for _, m := range mutate {
		m(&o)
	}
	c, err := remote.New(disk, o)
	if err != nil {
		t.Fatal(err)
	}
	return c, disk
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("error %v has code %q, want %q", err, got, code)
	}
}
