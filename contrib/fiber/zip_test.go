package fsfiber_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/ubgo/filesystem"
	fsfiber "github.com/ubgo/filesystem/contrib/fiber"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

// panicValue is what panickingDriver panics with.
const panicValue = "driver bug"

// panickingDriver is a memory driver whose Read panics, as a buggy
// backend would.
type panickingDriver struct{ *memory.Driver }

func (panickingDriver) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	panic(panicValue)
}

// zipApp serves Zip of one object on d at /zip, reporting failures after
// the response started to onError.
func zipApp(t *testing.T, d *filesystem.Disk, onError func(error)) string {
	t.Helper()
	if _, err := d.WriteBytes(t.Context(), "a.txt", []byte("quarterly numbers\n"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f := fiber.New()
	f.All("/zip/*", func(c fiber.Ctx) error {
		return fsfiber.Zip(c, d, "a.zip", []fshttp.ZipFile{{Key: "a.txt"}}, fshttp.ZipOptions{OnError: onError})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = f.Shutdown() })
	return ln.Addr().String()
}

// TestZip_HeadSendsTheHeadersOnly covers a HEAD: ServeZip checks the list
// and writes headers without a body byte, so nothing streams.
func TestZip_HeadSendsTheHeadersOnly(t *testing.T) {
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	addr := zipApp(t, d, nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodHead, "http://"+addr+"/zip/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Content-Disposition"), "a.zip") {
		t.Fatalf("HEAD = %d %v", resp.StatusCode, resp.Header)
	}
}

// TestZip_ADriverPanicFailsTheDownloadAndIsReported pins that a panic
// other than ServeZip's abort neither crashes the process nor ends the
// archive cleanly, and reaches OnError.
func TestZip_ADriverPanicFailsTheDownloadAndIsReported(t *testing.T) {
	d, err := filesystem.NewDisk("media", panickingDriver{memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reported []error
	addr := zipApp(t, d, func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() })
	resp, err := http.Get("http://" + addr + "/zip/x")
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("a zip whose source panicked ended cleanly")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), panicValue) {
		t.Fatalf("OnError got %v", reported)
	}
}

// TestZip_AnUnparsableRequestIsRefused covers a request URI that fiber
// routes but net/http cannot parse: Zip answers 400, the client's own
// error, rather than serving.
func TestZip_AnUnparsableRequestIsRefused(t *testing.T) {
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	addr := zipApp(t, d, nil)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprint(conn, "GET /zip/%zz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unparsable URI = %+v, want 400", resp)
	}
}
