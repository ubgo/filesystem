package scan_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

// eicar is the EICAR test file: harmless, and detected by every scanner.
const eicar = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

// fakeClamd serves clamd's protocol as clamd(8) documents it: PING, and
// INSTREAM with length-prefixed chunks. reply decides the answer to a
// stream; limit makes it stop reading after that many bytes and report
// the size limit, the way clamd does.
type fakeClamd struct {
	addr     string
	reply    func(data []byte) string
	limit    int
	pong     string // the answer to PING
	got      []byte
	commands []string
}

func newFake(t *testing.T, network string) *fakeClamd {
	t.Helper()
	f := &fakeClamd{pong: "PONG", reply: func(data []byte) string {
		if bytes.Contains(data, []byte(eicar)) {
			return "stream: Eicar-Test-Signature FOUND"
		}
		return "stream: OK"
	}}
	addr := "127.0.0.1:0"
	if network == "unix" {
		dir, err := os.MkdirTemp("", "clamd") // short: Unix socket paths are limited to ~104 bytes
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		addr = filepath.Join(dir, "s")
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	f.addr = l.Addr().String()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			f.serve(conn)
		}
	}()
	return f
}

func (f *fakeClamd) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	cmd := make([]byte, 0, 16)
	one := make([]byte, 1)
	for {
		if _, err := conn.Read(one); err != nil {
			return
		}
		if one[0] == 0 {
			break
		}
		cmd = append(cmd, one[0])
	}
	f.commands = append(f.commands, string(cmd))
	switch string(cmd) {
	case "zPING":
		_, _ = io.WriteString(conn, f.pong+"\x00")
		return
	case "zINSTREAM":
	default:
		_, _ = io.WriteString(conn, "UNKNOWN COMMAND\x00")
		return
	}
	var data []byte
	var prefix [4]byte
	for {
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(prefix[:])
		if n == 0 {
			break
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(conn, chunk); err != nil {
			return
		}
		data = append(data, chunk...)
		if f.limit > 0 && len(data) > f.limit {
			_, _ = io.WriteString(conn, "INSTREAM size limit exceeded. ERROR\x00")
			return
		}
	}
	f.got = data
	_, _ = io.WriteString(conn, f.reply(data)+"\x00")
}

func (f *fakeClamd) clamd(network string) scan.Clamd {
	return scan.Clamd{Network: network, Address: f.addr, ChunkSize: 7}
}

func TestClamd(t *testing.T) {
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			ctx := t.Context()
			f := newFake(t, network)
			c := f.clamd(network)
			if err := c.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("clean bytes ", 10)
			if v, err := c.Scan(ctx, strings.NewReader(body)); err != nil || !v.Clean || string(f.got) != body {
				t.Fatalf("clean scan = %+v, %v; clamd got %q", v, err, f.got)
			}
			v, err := c.Scan(ctx, strings.NewReader("prefix "+eicar))
			if err != nil || v.Clean || v.Signature != "Eicar-Test-Signature" {
				t.Fatalf("infected scan = %+v, %v", v, err)
			}
			if v, err := c.Scan(ctx, strings.NewReader("")); err != nil || !v.Clean {
				t.Fatalf("empty scan = %+v, %v", v, err)
			}
		})
	}
}

func TestClamdFailures(t *testing.T) {
	ctx := t.Context()
	f := newFake(t, "tcp")
	c := f.clamd("tcp")

	f.limit = 20
	_, err := c.Scan(ctx, strings.NewReader(strings.Repeat("x", 1<<20)))
	wantCode(t, err, filesystem.CodeTooLarge)
	f.limit = 0

	for reply, code := range map[string]filesystem.Code{
		"stream: lstat() failed: Permission denied. ERROR": filesystem.CodeUnavailable,
		"something else": filesystem.CodeUnavailable,
	} {
		f.reply = func([]byte) string { return reply }
		_, err := c.Scan(ctx, strings.NewReader("x"))
		wantCode(t, err, code)
	}
	f.pong = "PANG"
	wantCode(t, c.Ping(ctx), filesystem.CodeUnavailable)

	readErr := errors.New("disk read failed")
	if _, err := c.Scan(ctx, io.MultiReader(strings.NewReader("abc"), iotestErr(readErr))); !errors.Is(err, readErr) {
		t.Fatalf("a failing body = %v", err)
	}

	down := scan.Clamd{Network: "tcp", Address: closedAddr(t)}
	_, err = down.Scan(ctx, strings.NewReader("x"))
	wantCode(t, err, filesystem.CodeUnavailable)
	wantCode(t, down.Ping(ctx), filesystem.CodeUnavailable)
}

// TestClamdSilent bounds a daemon that accepts and never answers.
func TestClamdSilent(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, conn) }() // read everything, answer nothing
		}
	}()
	c := scan.Clamd{Network: "tcp", Address: l.Addr().String(), Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err = c.Scan(t.Context(), strings.NewReader("x"))
	wantCode(t, err, filesystem.CodeUnavailable)
	wantCode(t, c.Ping(t.Context()), filesystem.CodeUnavailable)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	// A daemon that closes while the stream is being sent and says nothing.
	closer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	go func() {
		for {
			conn, err := closer.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	c = scan.Clamd{Network: "tcp", Address: closer.Addr().String()}
	_, err = c.Scan(t.Context(), strings.NewReader(strings.Repeat("x", 4<<20)))
	wantCode(t, err, filesystem.CodeUnavailable)
	wantCode(t, c.Ping(t.Context()), filesystem.CodeUnavailable)
}

func TestInspect(t *testing.T) {
	ctx := t.Context()
	disk, _ := filesystem.NewDisk("uploads", memory.New())
	f := newFake(t, "tcp")
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Inspect: scan.Inspect(f.clamd("tcp"))}

	if _, err := upload.Receive(ctx, disk, strings.NewReader("hello"), "a.txt", p); err != nil {
		t.Fatal(err)
	}
	_, err := upload.Receive(ctx, disk, strings.NewReader(eicar), "eicar.txt", p)
	wantCode(t, err, filesystem.CodeRejected)
	if !strings.Contains(err.Error(), "Eicar-Test-Signature") {
		t.Fatalf("err = %v", err)
	}

	p.Inspect = scan.Inspect(scan.Clamd{Network: "tcp", Address: closedAddr(t)}) // fails closed
	_, err = upload.Receive(ctx, disk, strings.NewReader("hello"), "b.txt", p)
	wantCode(t, err, filesystem.CodeUnavailable)

	p.Inspect = scan.Inspect(scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) {
		return scan.Verdict{}, errors.New("cloud scanner down")
	}))
	_, err = upload.Receive(ctx, disk, strings.NewReader("hello"), "c.txt", p)
	wantCode(t, err, filesystem.CodeUnavailable)

	page, _ := disk.ListPage(ctx, "", filesystem.ListOptions{Recursive: true})
	if len(page.Entries) != 1 {
		t.Fatalf("%d objects, want only the clean one", len(page.Entries))
	}

	// A read failure of the stored object is reported as it is.
	gone := scan.Inspect(scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) { return scan.Verdict{Clean: true}, nil }))
	wantCode(t, gone(ctx, disk, upload.Result{Key: "missing"}), filesystem.CodeNotFound)
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func iotestErr(err error) io.Reader { return errReader{err} }

func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if filesystem.CodeOf(err) != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}
