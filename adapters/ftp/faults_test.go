package ftp_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/ftp"
)

// The server operations a fault can target (the server filesystem's method names).
const (
	opStat     = "Stat"
	opOpen     = "Open"
	opOpenFile = "OpenFile"
	opRemove   = "Remove"
	opMkdir    = "Mkdir"
	opRename   = "Rename"
)

// errStall makes a fault hang the command instead of failing it, so the
// caller's deadline cuts the connection: what a hung server does.
var errStall = errors.New("stall")

// stallFor is how long a stalled command hangs, past every test deadline.
const stallFor = time.Second

// faultFs answers the server's filesystem calls through hook, which sees
// the operation and the path and returns nil to proceed, an error for the
// server to reply with, or errStall to hang.
type faultFs struct {
	afero.Fs
	mu   *sync.Mutex
	hook *func(op, name string) error
}

func (f faultFs) check(op, name string) error {
	f.mu.Lock()
	h := *f.hook
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	err := h(op, name)
	if errors.Is(err, errStall) {
		time.Sleep(stallFor)
		return nil
	}
	return err
}

func (f faultFs) Stat(name string) (os.FileInfo, error) {
	if err := f.check(opStat, name); err != nil {
		return nil, err
	}
	return f.Fs.Stat(name)
}

func (f faultFs) Open(name string) (afero.File, error) {
	if err := f.check(opOpen, name); err != nil {
		return nil, err
	}
	return f.Fs.Open(name)
}

func (f faultFs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	if err := f.check(opOpenFile, name); err != nil {
		return nil, err
	}
	return f.Fs.OpenFile(name, flag, perm)
}

func (f faultFs) Remove(name string) error {
	if err := f.check(opRemove, name); err != nil {
		return err
	}
	return f.Fs.Remove(name)
}

func (f faultFs) Mkdir(name string, perm os.FileMode) error {
	if err := f.check(opMkdir, name); err != nil {
		return err
	}
	return f.Fs.Mkdir(name, perm)
}

func (f faultFs) Rename(oldname, newname string) error {
	if err := f.check(opRename, newname); err != nil {
		return err
	}
	return f.Fs.Rename(oldname, newname)
}

// faulty is a server and a driver whose server-side calls fail as set.
type faulty struct {
	m    *mainDriver
	addr string
	drv  *ftp.Driver
	mu   sync.Mutex
	hook func(op, name string) error
}

// newFaulty starts a server whose every session goes through a faultFs
// and opens a driver on it; set arms a fault afterwards.
func newFaulty(t *testing.T, settings func(*ftpserver.Settings)) *faulty {
	t.Helper()
	f := &faulty{}
	f.m, f.addr = startServer(t, nil)
	if settings != nil {
		settings(f.m.settings)
	}
	f.m.wrapFs = func(inner afero.Fs) afero.Fs { return faultFs{Fs: inner, mu: &f.mu, hook: &f.hook} }
	f.drv = newDriver(t, config(f.addr))
	return f
}

func (f *faulty) set(h func(op, name string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

// on returns a hook answering err to op on paths holding part.
func on(op, part string, err error) func(string, string) error {
	return func(o, name string) error {
		if o == op && strings.Contains(name, part) {
			return err
		}
		return nil
	}
}

// shortCtx is a context the stalled commands outlive.
func shortCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func put(t *testing.T, drv *ftp.Driver, key, body string) {
	t.Helper()
	if _, err := drv.Write(t.Context(), key, strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// TestReservedKeysAreRefused pins that every operation refuses a key in
// the staging directory before any command.
func TestReservedKeysAreRefused(t *testing.T) {
	_, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	ctx := t.Context()
	k := ftp.StagingDir + "/tmp/x"
	errs := map[string]error{}
	_, errs["Stat"] = drv.Stat(ctx, k)
	_, errs["Read"] = drv.Read(ctx, k, filesystem.ByteRange{})
	_, errs["Write"] = drv.Write(ctx, k, strings.NewReader("x"), filesystem.WriteOptions{})
	errs["Delete"] = drv.Delete(ctx, k)
	_, errs["Move"] = drv.Move(ctx, "a", k)
	errs["DeletePrefix"] = drv.DeletePrefix(ctx, ftp.StagingDir)
	for name, err := range errs {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("%s: %v, want CodeInvalidKey", name, err)
		}
	}
}

// TestConfigEdges pins the Config and New paths the other tests do not
// take: an empty password prints empty, an address without a port gets
// FTP's (nothing listens there in the test, so the dial fails as
// CodeUnavailable), no username means anonymous (refused by the test
// server: CodePermission), and a Root under a file cannot be created.
func TestConfigEdges(t *testing.T) {
	if s := (ftp.Config{Addr: "h"}).String(); !strings.Contains(s, `Password: ""`) {
		t.Fatalf("String() = %s", s)
	}
	ctx := shortCtx(t)
	if _, err := ftp.New(ctx, ftp.Config{Addr: "127.0.0.1", Root: "/"}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("default port: %v, want CodeUnavailable", err)
	}
	m, addr := startServer(t, nil)
	if _, err := ftp.New(t.Context(), ftp.Config{Addr: addr, Root: "/app"}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("anonymous: %v, want CodePermission", err)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config(addr)
	cfg.Root = "/file/app"
	if _, err := ftp.New(t.Context(), cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Root under a file: %v, want CodeInvalidArgument", err)
	}
}

// TestDialCutByTheDeadline pins that a server that accepts and never
// greets fails the dial when the context ends, as CodeUnavailable carrying
// the context's error.
func TestDialCutByTheDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	_, err = ftp.New(shortCtx(t), config(ln.Addr().String()))
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want CodeUnavailable with the deadline", err)
	}
}

// TestImplicitTLS runs a write and a read over implicit FTPS.
func TestImplicitTLS(t *testing.T) {
	serverTLS, roots := selfSigned(t)
	_, addr := startServerWith(t, serverTLS, func(s *ftpserver.Settings) {
		// The server wraps only a listener it creates itself.
		s.TLSRequired, s.Listener = ftpserver.ImplicitEncryption, tls.NewListener(s.Listener, serverTLS)
	})
	cfg := config(addr)
	cfg.TLS = &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	cfg.ImplicitTLS = true
	drv := newDriver(t, cfg)
	put(t, drv, "a.txt", "secret")
	if got := readKey(t, drv, "a.txt"); got != "secret" {
		t.Fatalf("read %q", got)
	}
	// Without ServerName the name to verify comes from Addr, as tls.Dialer
	// does it.
	named := cfg
	named.TLS = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if got := readKey(t, newDriver(t, named), "a.txt"); got != "secret" {
		t.Fatalf("read %q without ServerName", got)
	}
	// Implicit TLS to a plain FTP server fails the handshake.
	_, plain := startServer(t, nil)
	wrong := config(plain)
	wrong.TLS, wrong.ImplicitTLS = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, true
	if _, err := ftp.New(shortCtx(t), wrong); err == nil {
		t.Fatal("implicit TLS against a plain server opened")
	}
}

// TestCaseProbe pins the probe's answers on servers that do not fold
// case (the SIZE of the upper-case name is refused) and on a read-only
// account (the probe file cannot be stored): both case-sensitive.
func TestCaseProbe(t *testing.T) {
	for name, hook := range map[string]func(string, string) error{
		"case-sensitive": func(op, name string) error {
			if op == opStat && strings.ToLower(name) != name {
				return os.ErrNotExist
			}
			return nil
		},
		"read-only": on(opOpenFile, ftp.StagingDir+"/tmp/", os.ErrPermission),
	} {
		t.Run(name, func(t *testing.T) {
			f := &faulty{}
			f.m, f.addr = startServer(t, nil)
			f.hook = hook
			f.m.wrapFs = func(inner afero.Fs) afero.Fs { return faultFs{Fs: inner, mu: &f.mu, hook: &f.hook} }
			if newDriver(t, config(f.addr)).Capabilities().CaseInsensitive {
				t.Fatal("reported case-insensitive")
			}
		})
	}
}

// TestCaseProbeOnABrokenConnection pins that a probe cut off by the
// deadline fails New rather than guessing.
func TestCaseProbeOnABrokenConnection(t *testing.T) {
	for name, hook := range map[string]func(string, string) error{
		"store": on(opOpenFile, ftp.StagingDir+"/tmp/", errStall),
		"size": func(op, name string) error {
			if op == opStat && strings.ToLower(name) != name {
				return errStall
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &faulty{}
			f.m, f.addr = startServer(t, nil)
			f.hook = hook
			f.m.wrapFs = func(inner afero.Fs) afero.Fs { return faultFs{Fs: inner, mu: &f.mu, hook: &f.hook} }
			if _, err := ftp.New(shortCtx(t), config(f.addr)); err == nil {
				t.Fatal("New succeeded over a broken probe")
			}
		})
	}
}

// TestStatOfADirectory pins that a directory is not an object.
func TestStatOfADirectory(t *testing.T) {
	f := newFaulty(t, nil)
	put(t, f.drv, "d/k", "x")
	if _, err := f.drv.Stat(t.Context(), "d"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("got %v, want CodeNotFound", err)
	}
}

// TestReadEdges pins Read past the happy path: a closed driver, a file the
// server will not send, a context ended while the body is read, and a
// second Close.
func TestReadEdges(t *testing.T) {
	f := newFaulty(t, nil)
	put(t, f.drv, "k", "0123456789")
	f.set(on(opOpenFile, "/app/k", os.ErrPermission))
	if _, err := f.drv.Read(t.Context(), "k", filesystem.ByteRange{}); err == nil {
		t.Fatal("a refused RETR was read")
	}
	f.set(nil)
	ctx, cancel := context.WithCancel(t.Context())
	rc, err := f.drv.Read(ctx, "k", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := io.ReadAll(rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel: %v, want context.Canceled", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_ = f.drv.Close()
	if _, err := f.drv.Read(t.Context(), "k", filesystem.ByteRange{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("closed driver: %v, want CodeUnavailable", err)
	}
}

// cancellingBody cancels its context on the first read and keeps sending.
type cancellingBody struct{ cancel context.CancelFunc }

func (b cancellingBody) Read(p []byte) (int, error) {
	b.cancel()
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestWriteStopsWhenTheContextEnds pins that an upload stops reading its
// body once the context ends, and stores nothing.
func TestWriteStopsWhenTheContextEnds(t *testing.T) {
	f := newFaulty(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := f.drv.Write(ctx, "k", cancellingBody{cancel}, filesystem.WriteOptions{}); err == nil {
		t.Fatal("a cancelled write succeeded")
	}
	if _, err := os.Stat(filepath.Join(f.m.dir, "app", "k")); err == nil {
		t.Fatal("a cancelled write stored the key")
	}
}

// TestRenameEdges pins rename's refusals on a server that will not rename
// over a file: dst a directory (refused, nothing deleted), the staged file
// gone (refused, the old object kept), a refused delete of the replaced
// object, and a reply of another kind than "exists" after the parents were
// made.
func TestRenameEdges(t *testing.T) {
	exists := errors.New("Cannot create a file when that file already exists.")
	refuseOver := func(f *faulty) func(string, string) error {
		return func(op, name string) error {
			if op != opRename {
				return nil
			}
			if _, err := os.Stat(filepath.Join(f.m.dir, filepath.FromSlash(name))); err == nil {
				return exists
			}
			return nil
		}
	}
	t.Run("dst is a directory", func(t *testing.T) {
		f := newFaulty(t, nil)
		put(t, f.drv, "src", "s")
		put(t, f.drv, "d/inner", "i")
		f.set(refuseOver(f))
		if _, err := f.drv.Move(t.Context(), "src", "d"); filesystem.CodeOf(err) != filesystem.CodePermission {
			t.Fatalf("got %v, want CodePermission", err)
		}
		if got := readKey(t, f.drv, "d/inner"); got != "i" {
			t.Fatalf("the directory lost its file: %q", got)
		}
	})
	t.Run("delete of the replaced object refused", func(t *testing.T) {
		// Deleting the moved-aside old object is cleanup: the replace has
		// happened, so it succeeds and leaves one staged file for
		// SweepAbandoned.
		f := newFaulty(t, nil)
		put(t, f.drv, "doc", "old")
		over := refuseOver(f)
		f.set(func(op, name string) error {
			if op == opRemove && strings.Contains(name, "/"+ftp.StagingDir+"/") {
				return os.ErrPermission
			}
			return over(op, name)
		})
		if _, err := f.drv.Write(t.Context(), "doc", strings.NewReader("new"), filesystem.WriteOptions{}); err != nil {
			t.Fatalf("a replace whose cleanup was refused: %v", err)
		}
		f.set(nil)
		if got := readKey(t, f.drv, "doc"); got != "new" {
			t.Fatalf("doc = %q, want new", got)
		}
		if left := staged(t, f.m); len(left) != 1 {
			t.Fatalf("%d staged files after a refused cleanup, want the old object's one", len(left))
		}
	})
	t.Run("other refusal after mkdir", func(t *testing.T) {
		f := newFaulty(t, nil)
		f.set(on(opRename, "/app/n/k", errors.New("quota")))
		if _, err := f.drv.Write(t.Context(), "n/k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
			t.Fatalf("got %v, want CodePermission", err)
		}
	})
}

// TestDeleteEdges pins that a delete the server refuses fails Delete.
func TestDeleteEdges(t *testing.T) {
	f := newFaulty(t, nil)
	put(t, f.drv, "k", "x")
	f.set(on(opRemove, "/app/k", os.ErrPermission))
	if err := f.drv.Delete(t.Context(), "k"); err == nil {
		t.Fatal("a refused delete succeeded")
	}
}

// TestListEdges pins that a listing of a subdirectory the server refuses
// fails the listing, and that names no clean key holds are skipped.
func TestListEdges(t *testing.T) {
	f := newFaulty(t, nil)
	put(t, f.drv, "a/b", "x")
	if err := os.WriteFile(filepath.Join(f.m.dir, "app", `back\slash`), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := f.drv.List(t.Context(), "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Key != "a/b" {
		t.Fatalf("entries %+v, want only a/b", p.Entries)
	}
	f.set(on(opOpen, "/app/a", os.ErrPermission))
	if _, err := f.drv.List(t.Context(), "", filesystem.ListOptions{Recursive: true}); err == nil {
		t.Fatal("a refused subdirectory listed as empty")
	}
}

// TestDeletePrefixEdges pins DeletePrefix past the happy path: a missing
// directory and a file named like it are left alone without error, the
// root removes everything but the staging directory, and a refused
// removal fails it.
func TestDeletePrefixEdges(t *testing.T) {
	ctx := t.Context()
	f := newFaulty(t, nil)
	put(t, f.drv, "file", "x")
	put(t, f.drv, "d/k", "x")
	if err := f.drv.DeletePrefix(ctx, "missing"); err != nil {
		t.Fatalf("missing: %v", err)
	}
	if err := f.drv.DeletePrefix(ctx, "file"); err != nil {
		t.Fatalf("a file named like the prefix: %v", err)
	}
	f.set(on(opRemove, "/app/file", os.ErrPermission))
	if err := f.drv.DeletePrefix(ctx, ""); err == nil {
		t.Fatal("a refused file removal under the root succeeded")
	}
	f.set(nil)
	put(t, f.drv, "d/k", "x")
	f.set(on(opRemove, "/app/d", os.ErrPermission))
	if err := f.drv.DeletePrefix(ctx, "d"); err == nil {
		t.Fatal("a refused removal succeeded")
	}
	if err := f.drv.DeletePrefix(ctx, ""); err == nil {
		t.Fatal("a refused directory removal under the root succeeded")
	}
	f.set(nil)
	if err := f.drv.DeletePrefix(ctx, ""); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(filepath.Join(f.m.dir, "app"))
	if err != nil || len(left) != 1 || left[0].Name() != ftp.StagingDir {
		t.Fatalf("left %v (%v), want only the staging directory", left, err)
	}
}

// TestBrokenConnections pins that a command cut off by the caller's
// deadline (a hung server; the pool closes the connection) is an error,
// never taken for the server's answer, for every operation.
func TestBrokenConnections(t *testing.T) {
	cases := []struct {
		name string
		hook func(string, string) error
		call func(ctx context.Context, d *ftp.Driver) error
	}{
		{"stat", on(opStat, "/app/k", errStall), func(ctx context.Context, d *ftp.Driver) error { _, err := d.Stat(ctx, "k"); return err }},
		{"delete stat", on(opStat, "/app/k", errStall), func(ctx context.Context, d *ftp.Driver) error { return d.Delete(ctx, "k") }},
		{"list", on(opOpen, "/app", errStall), func(ctx context.Context, d *ftp.Driver) error {
			_, err := d.List(ctx, "", filesystem.ListOptions{})
			return err
		}},
		{"delete prefix", on(opStat, "/app/d", errStall), func(ctx context.Context, d *ftp.Driver) error { return d.DeletePrefix(ctx, "d") }},
		{"delete prefix root", on(opOpen, "/app", errStall), func(ctx context.Context, d *ftp.Driver) error { return d.DeletePrefix(ctx, "") }},
		{"write mkdir", on(opMkdir, "/app/new", errStall), func(ctx context.Context, d *ftp.Driver) error {
			_, err := d.Write(ctx, "new/k", strings.NewReader("x"), filesystem.WriteOptions{})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFaulty(t, nil)
			put(t, f.drv, "k", "x")
			put(t, f.drv, "d/k", "x")
			f.set(c.hook)
			// A fresh driver: the first one's pooled connection would do.
			drv := newDriver(t, config(f.addr))
			if err := c.call(shortCtx(t), drv); err == nil {
				t.Fatal("a call cut off by its deadline succeeded")
			}
		})
	}
}

// nth returns a hook answering err to the n-th op on exactly name.
func nth(n int, op, name string, err error) func(string, string) error {
	seen := 0
	return func(o, p string) error {
		if o != op || p != name {
			return nil
		}
		if seen++; seen == n {
			return err
		}
		return nil
	}
}

// TestMoreBrokenConnections pins the cut-off cases that need a server
// answering some commands first: MDTM after SIZE on a server without MLST,
// the check that the staged file is still there before a replace deletes
// the old object, and the CWD that verifies new parent directories.
func TestMoreBrokenConnections(t *testing.T) {
	cases := []struct {
		name     string
		settings func(*ftpserver.Settings)
		hook     func(f *faulty) func(string, string) error
		call     func(ctx context.Context, d *ftp.Driver) error
	}{
		{"mdtm", func(s *ftpserver.Settings) { s.DisableMLST, s.DisableMLSD = true, true },
			func(*faulty) func(string, string) error { return nth(2, opStat, "/app/k", errStall) },
			func(ctx context.Context, d *ftp.Driver) error { _, err := d.Stat(ctx, "k"); return err }},
		{"cwd", nil,
			func(*faulty) func(string, string) error { return nth(1, opStat, "/app/new", errStall) },
			func(ctx context.Context, d *ftp.Driver) error {
				_, err := d.Write(ctx, "new/k", strings.NewReader("x"), filesystem.WriteOptions{})
				return err
			}},
		{"staged check", nil,
			func(f *faulty) func(string, string) error {
				refused := false
				return func(op, name string) error {
					switch {
					case op == opRename && strings.HasSuffix(name, "/app/k"):
						refused = true
						return errors.New("Cannot create a file when that file already exists.")
					case op == opStat && refused && strings.Contains(name, ftp.StagingDir+"/tmp/"):
						return errStall // the SIZE of the staged file, after the refusal
					}
					return nil
				}
			},
			func(ctx context.Context, d *ftp.Driver) error {
				_, err := d.Write(ctx, "k", strings.NewReader("new"), filesystem.WriteOptions{})
				return err
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFaulty(t, c.settings)
			put(t, f.drv, "k", "x")
			f.set(c.hook(f))
			drv := newDriver(t, config(f.addr))
			if err := c.call(shortCtx(t), drv); err == nil {
				t.Fatal("a call cut off by its deadline succeeded")
			}
		})
	}
}

// TestReadCancelledDuringItsStat pins that a Read whose context ends while
// the server is still answering the stat returns the context's error.
func TestReadCancelledDuringItsStat(t *testing.T) {
	f := newFaulty(t, nil)
	put(t, f.drv, "k", "x")
	f.set(on(opStat, "/app/k", errStall))
	drv := newDriver(t, config(f.addr))
	if _, err := drv.Read(shortCtx(t), "k", filesystem.ByteRange{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the deadline", err)
	}
}

// TestReadAfterClose pins that reading a closed download fails instead of
// reading a socket the pool may have reused.
func TestReadAfterClose(t *testing.T) {
	f := newFaulty(t, nil)
	put(t, f.drv, "k", strings.Repeat("x", 1<<20))
	rc, err := f.drv.Read(t.Context(), "k", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if _, err := rc.Read(make([]byte, 8)); err == nil {
		t.Fatal("a closed download read")
	}
}

// failingFile is a stored file whose reads fail after the first chunk, so
// the server aborts the transfer and answers it with an error reply.
type failingFile struct {
	afero.File
	sent bool
}

func (f *failingFile) Read(p []byte) (int, error) {
	if f.sent {
		return 0, errors.New("disk read failed")
	}
	f.sent = true
	return f.File.Read(p[:min(len(p), 4)])
}

type failingReads struct{ afero.Fs }

func (f failingReads) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	file, err := f.Fs.OpenFile(name, flag, perm)
	if err != nil || flag != os.O_RDONLY || !strings.HasSuffix(name, "/app/k") {
		return file, err
	}
	return &failingFile{File: file}, nil
}

func (f failingReads) Open(name string) (afero.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

// TestTransferAbortedByTheServer pins that a download the server cut short
// (its disk failed mid-file) is an error when it is closed, not a short
// file read as whole.
func TestTransferAbortedByTheServer(t *testing.T) {
	m, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	put(t, drv, "k", "0123456789")
	m.wrapFs = func(inner afero.Fs) afero.Fs { return failingReads{inner} }
	drv = newDriver(t, config(addr))
	rc, err := drv.Read(t.Context(), "k", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := io.ReadAll(rc)
	if cerr := rc.Close(); rerr == nil && cerr == nil {
		t.Fatal("an aborted transfer read and closed cleanly")
	}
}

// TestOpenerSchemes pins the Opener's schemes: ftps sets up TLS (which a
// plain server refuses), and any other scheme is refused.
func TestOpenerSchemes(t *testing.T) {
	_, addr := startServer(t, nil)
	u, err := url.Parse("ftps://" + addr + "/app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ftp.Opener(shortCtx(t), u); err == nil {
		t.Fatal("ftps succeeded against a server without TLS")
	}
	u.Scheme = "http"
	if _, err := ftp.Opener(t.Context(), u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("http: %v, want CodeInvalidArgument", err)
	}
}

// TestReplaceKeepsTheOldObject pins that a replace on a server that will
// not rename over a file never loses the old object when a step fails. The
// driver used to delete the old object before renaming the new one in, so
// a failed rename left the key empty. Each case fails a different step of
// replace (moving the old object aside, the new one in, both the new one in
// and the old one back) and checks where both contents end up.
func TestReplaceKeepsTheOldObject(t *testing.T) {
	quota := errors.New("quota exceeded")
	// refuse fails renames onto /app/doc that the server does not already
	// refuse as "exists" (the moves in and back), the first n times, and
	// every rename into the staging directory (moves aside) when aside.
	refuse := func(f *faulty, n int, aside bool) func(string, string) error {
		over := func(name string) bool {
			_, err := os.Stat(filepath.Join(f.m.dir, filepath.FromSlash(name)))
			return err == nil
		}
		return func(op, name string) error {
			if op != opRename {
				return nil
			}
			switch {
			case over(name):
				return errors.New("Cannot create a file when that file already exists.")
			case aside && strings.Contains(name, "/"+ftp.StagingDir+"/"):
				return quota
			case strings.HasSuffix(name, "/app/doc") && n > 0:
				n--
				return quota
			}
			return nil
		}
	}
	write := func(f *faulty) error {
		_, err := f.drv.Write(t.Context(), "doc", strings.NewReader("new"), filesystem.WriteOptions{})
		return err
	}
	move := func(f *faulty) error {
		_, err := f.drv.Move(t.Context(), "src", "doc")
		return err
	}
	// Moving in is tried three times; the fourth rename onto doc is the
	// move back.
	const movesIn = 3
	for _, c := range []struct {
		name  string
		op    func(*faulty) error
		n     int
		aside bool
	}{
		{"write: moving the new object in refused", write, movesIn, false},
		{"move: moving the new object in refused", move, movesIn, false},
		{"write: moving the old object aside refused", write, 0, true},
		{"move: moving the old object aside refused", move, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFaulty(t, nil)
			put(t, f.drv, "doc", "old")
			put(t, f.drv, "src", "new")
			f.set(refuse(f, c.n, c.aside))
			err := c.op(f)
			if filesystem.CodeOf(err) != filesystem.CodePermission || errors.As(err, new(*ftp.StrandedError)) {
				t.Fatalf("got %v, want a plain CodePermission", err)
			}
			f.set(nil)
			if got := readKey(t, f.drv, "doc"); got != "old" {
				t.Fatalf("doc = %q, want old", got)
			}
			if got := readKey(t, f.drv, "src"); got != "new" {
				t.Fatalf("src = %q, want it unharmed", got)
			}
			if left := staged(t, f.m); len(left) != 0 {
				t.Fatalf("%d staged files left", len(left))
			}
		})
	}

	t.Run("moving in and back both refused", func(t *testing.T) {
		f := newFaulty(t, nil)
		put(t, f.drv, "doc", "old")
		f.set(refuse(f, 2*movesIn, false))
		var se *ftp.StrandedError
		if err := write(f); !errors.As(err, &se) {
			t.Fatalf("got %v, want a *ftp.StrandedError", err)
		}
		f.set(nil)
		for path, want := range map[string]string{se.Staged: "new", se.Previous: "old"} {
			b, err := os.ReadFile(filepath.Join(f.m.dir, filepath.FromSlash(path)))
			if err != nil || string(b) != want {
				t.Errorf("%s holds %q (%v), want %s", path, b, err, want)
			}
		}
		if se.Target != "/app/doc" || !strings.Contains(se.Error(), se.Staged) || !strings.Contains(se.Error(), se.Previous) {
			t.Errorf("error does not name the target and both kept files: %v", se)
		}
	})
}

// TestReplaceCutWhileMovingAside pins that a connection cut while the old
// object is being moved aside loses nothing. The server may still finish
// the rename after the client gave up, so the old content ends up either
// at the key or in the staging directory, never nowhere, and the write
// fails with the context's error rather than reporting success.
func TestReplaceCutWhileMovingAside(t *testing.T) {
	f := newFaulty(t, nil)
	put(t, f.drv, "doc", "old")
	f.set(func(op, name string) error {
		if op != opRename {
			return nil
		}
		if strings.Contains(name, "/"+ftp.StagingDir+"/") {
			return errStall
		}
		if _, err := os.Stat(filepath.Join(f.m.dir, filepath.FromSlash(name))); err == nil {
			return errors.New("Cannot create a file when that file already exists.")
		}
		return nil
	})
	_, err := f.drv.Write(shortCtx(t), "doc", strings.NewReader("new"), filesystem.WriteOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context's deadline", err)
	}
	// Let the stalled rename finish on the server, as a real one may.
	time.Sleep(stallFor + stallFor/2)
	f.set(nil)
	found := false
	if b, err := os.ReadFile(filepath.Join(f.m.dir, "app", "doc")); err == nil && string(b) == "old" {
		found = true
	}
	for _, e := range staged(t, f.m) {
		if b, err := os.ReadFile(filepath.Join(f.m.dir, "app", ftp.StagingDir, "tmp", e.Name())); err == nil && string(b) == "old" {
			found = true
		}
	}
	if !found {
		t.Fatal("the old content is neither at the key nor in the staging directory")
	}
}
