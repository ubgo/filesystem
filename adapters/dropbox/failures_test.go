package dropbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/dropbox"
)

// TestConfigWithoutTokenPrintsNone pins that a Config with no token source
// says so rather than claiming a redacted one.
func TestConfigWithoutTokenPrintsNone(t *testing.T) {
	if s := (dropbox.Config{Root: "/r"}).String(); !strings.Contains(s, "Token: none") {
		t.Fatalf("String() = %s", s)
	}
}

// TestLocalhostEndpointIsAllowed pins that plain http is accepted for the
// name localhost (not only a loopback IP).
func TestLocalhostEndpointIsAllowed(t *testing.T) {
	f := newFake(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.URL, "http://"))
	cfg := config(f)
	cfg.APIURL = "http://localhost:" + port + "/2"
	drv := newDriver(t, cfg)
	if _, err := drv.Stat(context.Background(), "x"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("stat through localhost: %v, want CodeNotFound", err)
	}
}

// TestUnknownConflictAndRetryAfter pins the fallbacks of the error mapping:
// a 409 with an unrecognised summary is CodeInvalidArgument, and a
// Retry-After that is not a positive number of seconds is ignored.
func TestUnknownConflictAndRetryAfter(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	f.on("files/get_metadata", f.failWith(http.StatusConflict, "path/something_new/"))
	if _, err := drv.Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("unknown 409: %v, want CodeInvalidArgument", err)
	}
	for _, ra := range []string{"", "soon", "-3"} {
		f.on("files/get_metadata", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", ra)
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := drv.Stat(ctx, "x")
		var fe *filesystem.Error
		if !errors.As(err, &fe) || fe.Code != filesystem.CodeUnavailable || fe.RetryAfter != 0 {
			t.Fatalf("Retry-After %q: %#v, want CodeUnavailable with no delay", ra, err)
		}
	}
	f.on("files/get_metadata", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	var fe *filesystem.Error
	if _, err := drv.Stat(ctx, "x"); !errors.As(err, &fe) || fe.RetryAfter != 7*time.Second {
		t.Fatalf("Retry-After 7: %v", err)
	}
}

// TestTransportFailures pins that a server that cannot be reached is
// CodeUnavailable, and a request whose context ended returns the
// context's error rather than a classified one.
func TestTransportFailures(t *testing.T) {
	f := newFake(t)
	drv := newDriver(t, config(f))
	f.Close()
	if _, err := drv.Stat(context.Background(), "x"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("server down: %v, want CodeUnavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var fe *filesystem.Error
	if _, err := drv.Stat(ctx, "x"); !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
		t.Fatalf("cancelled: %v, want the context error itself, unclassified", err)
	}
}

// TestStatOfAFolderIsNotFound pins that a folder is not an object.
func TestStatOfAFolderIsNotFound(t *testing.T) {
	ctx := context.Background()
	drv := newDriver(t, config(newFake(t)))
	if _, err := drv.Write(ctx, "dir/f", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Stat(ctx, "dir"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("stat of a folder: %v, want CodeNotFound", err)
	}
}

// TestReadWithAFailingToken pins that Read reports a failing token source
// as CodePermission, as every other call does.
func TestReadWithAFailingToken(t *testing.T) {
	cfg := config(newFake(t))
	cfg.Token = dropbox.TokenFunc(func(context.Context) (string, error) { return "", errors.New("revoked") })
	if _, err := newDriver(t, cfg).Read(context.Background(), "x", filesystem.ByteRange{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("got %v, want CodePermission", err)
	}
}

// TestReadWhenTheServerIgnoresRange pins the client-side cut: a 200 with
// the whole file answering a ranged read is trimmed to the range, with and
// without a length.
func TestReadWhenTheServerIgnoresRange(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	f.on("files/download", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("0123456789")) })
	for _, c := range []struct {
		rng  filesystem.ByteRange
		want string
	}{
		{filesystem.ByteRange{Offset: 2, Length: 3}, "234"},
		{filesystem.ByteRange{Offset: 7}, "789"},
		{filesystem.ByteRange{Offset: 20}, ""},
	} {
		rc, err := drv.Read(ctx, "r", c.rng)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != c.want {
			t.Errorf("Read(%+v) = %q, want %q", c.rng, got, c.want)
		}
	}
}

// TestReadOfATruncatedAnswer pins that a body cut short while the driver
// skips to the range's offset is CodeUnavailable, not a short read.
func TestReadOfATruncatedAnswer(t *testing.T) {
	f := newFake(t)
	drv := newDriver(t, config(f))
	f.on("files/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(100))
		_, _ = w.Write([]byte("01234"))
	})
	if _, err := drv.Read(context.Background(), "r", filesystem.ByteRange{Offset: 50}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("got %v, want CodeUnavailable", err)
	}
}

// TestUploadSessionFailures pins that a failure at every request of an
// upload session (start, its answer, append, finish, its answer) fails the
// write and commits nothing.
func TestUploadSessionFailures(t *testing.T) {
	ctx := context.Background()
	body := strings.Repeat("0123456789", 3) + "tail"
	for name, setup := range map[string]func(f *fake){
		"start":        func(f *fake) { f.on("files/upload_session/start", f.failWith(http.StatusServiceUnavailable, "")) },
		"start answer": func(f *fake) { f.on("files/upload_session/start", garbage) },
		"append": func(f *fake) {
			f.on("files/upload_session/append_v2", f.failWith(http.StatusConflict, "lookup_failed/not_found/"))
		},
		"finish": func(f *fake) {
			f.on("files/upload_session/finish", f.failWith(http.StatusConflict, "path/conflict/file/"))
		},
		"finish answer": func(f *fake) { f.on("files/upload_session/finish", garbage) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			cfg := config(f)
			cfg.ChunkSize = 10
			drv := newDriver(t, cfg)
			setup(f)
			if _, err := drv.Write(ctx, "big", strings.NewReader(body), filesystem.WriteOptions{}); err == nil {
				t.Fatal("write succeeded")
			}
			if len(f.files) != 0 {
				t.Fatalf("a failed write committed %d files", len(f.files))
			}
		})
	}
}

// TestSessionFinishConflictIsCompareAndSwap pins that a compare-and-swap
// write too long for one request reports a stale rev as CodeConflict.
func TestSessionFinishConflictIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	cfg := config(newFake(t))
	cfg.ChunkSize = 10
	drv := newDriver(t, cfg)
	if _, err := drv.Write(ctx, "k", strings.NewReader("v1"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	long := strings.NewReader(strings.Repeat("x", 35))
	if _, err := drv.Write(ctx, "k", long, filesystem.WriteOptions{IfMatch: "stale"}); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Fatalf("got %v, want CodeConflict", err)
	}
}

// TestDeleteFailures pins Delete's edges: a failing stat is returned, and
// a file that vanishes between the stat and the delete is a success.
func TestDeleteFailures(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.on("files/delete_v2", f.failWith(http.StatusConflict, "path_lookup/not_found/"))
	if err := drv.Delete(ctx, "k"); err != nil {
		t.Fatalf("vanished file: %v, want success", err)
	}
	f.on("files/get_metadata", f.failWith(http.StatusServiceUnavailable, ""))
	if err := drv.Delete(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing stat: %v, want CodeUnavailable", err)
	}
}

// TestRelocateEdges pins Copy and Move onto the same key (a no-op returning
// the object), a copy onto the same key in another case (the same file on
// a case-insensitive store), a failing cleanup of the replaced dst, and
// the failure of the relocation itself.
func TestRelocateEdges(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	if _, err := drv.Write(ctx, "a", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if e, err := drv.Move(ctx, "a", "a"); err != nil || e.Key != "a" {
		t.Fatalf("move onto itself: %+v, %v", e, err)
	}
	if e, err := drv.Copy(ctx, "a", "A"); err != nil || e.Size != 1 || f.requests["files/copy_v2"] != 0 {
		t.Fatalf("copy onto another case: %+v, %v, %d copies", e, err, f.requests["files/copy_v2"])
	}
	if _, err := drv.Write(ctx, "b", strings.NewReader("y"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Deleting the moved-aside old dst is cleanup: its failure leaves one
	// transient file behind but does not fail the copy, which did happen.
	f.on("files/delete_v2", f.failWith(http.StatusForbidden, ""))
	if e, err := drv.Copy(ctx, "a", "b"); err != nil || e.Size != 1 {
		t.Fatalf("copy with a failing cleanup: %+v, %v", e, err)
	}
	if left := f.swapFiles(); len(left) != 1 {
		t.Fatalf("transient files after a failed cleanup: %v, want one", left)
	}
	f.on("files/move_v2", f.failWith(http.StatusConflict, "insufficient_space/"))
	if _, err := drv.Move(ctx, "a", "c"); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("failing move: %v, want CodeQuotaExceeded", err)
	}
}

// TestListFailures pins that a failing first page or a failing
// continuation fails the listing, and that a listed path outside the root
// (which the API should never send) is dropped, not mapped to a key.
func TestListFailures(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	for _, k := range []string{"d/1", "d/2", "d/3", "d/4", "d/5"} {
		if _, err := drv.Write(ctx, k, strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f.on("files/list_folder/continue", f.failWith(http.StatusInternalServerError, ""))
	if _, err := drv.List(ctx, "d", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing continuation: %v, want CodeUnavailable", err)
	}
	f.on("files/list_folder", f.failWith(http.StatusUnauthorized, ""))
	if _, err := drv.List(ctx, "d", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing first page: %v, want CodePermission", err)
	}
	f.on("files/list_folder", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"entries":[{".tag":"file","path_display":"/Elsewhere/x"},{".tag":"file","path_display":"/Apps"},{".tag":"file","path_display":"/Apps/Test/d/ok"}],"has_more":false}`))
	})
	p, err := drv.List(ctx, "d", filesystem.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Key != "d/ok" {
		t.Fatalf("entries %+v, want only d/ok", p.Entries)
	}
}

// TestDeletePrefixEdges pins DeletePrefix's edges: a missing folder and a
// folder that vanishes before the delete are successes, a failing lookup
// is returned.
func TestDeletePrefixEdges(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	if err := drv.DeletePrefix(ctx, "missing"); err != nil {
		t.Fatalf("missing folder: %v", err)
	}
	if _, err := drv.Write(ctx, "d/x", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.on("files/delete_v2", f.failWith(http.StatusConflict, "path_lookup/not_found/"))
	if err := drv.DeletePrefix(ctx, "d"); err != nil {
		t.Fatalf("vanished folder: %v", err)
	}
	f.on("files/get_metadata", f.failWith(http.StatusTooManyRequests, ""))
	if err := drv.DeletePrefix(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing lookup: %v, want CodeUnavailable", err)
	}
}

// TestRelocateKeepsOldDestination pins that a copy or move onto an
// existing file never loses that file when a step fails: Dropbox cannot
// overwrite, and the driver used to delete dst before copying, so a failed
// copy left nothing. Each case fails a different step and checks dst still
// holds its old content, src is unharmed, and no transient file remains
// (except where a rollback also failed, when the old content must be at the
// transient name the error reports).
func TestRelocateKeepsOldDestination(t *testing.T) {
	const srcPath, dstPath = "/apps/test/src", "/apps/test/dst"
	isSwap := func(p string) bool { return strings.Contains(p, strings.ToLower(dropbox.SwapPrefix)) }
	cases := []struct {
		name     string
		op       func(*dropbox.Driver, context.Context) error
		endpoint string
		fail     func(from, to string) bool
	}{
		{"copy fails", copyOp, "files/copy_v2", func(string, string) bool { return true }},
		// The first swap-file-to-dst move is the copy's swap-in; the second
		// is the rollback of the backup, which must succeed.
		{"copy swap-in fails", copyOp, "files/move_v2", firstOnly(func(from, to string) bool { return isSwap(from) && to == dstPath })},
		{"move fails", moveOp, "files/move_v2", func(from, to string) bool { return from == srcPath && to == dstPath }},
		{"moving dst aside fails", moveOp, "files/move_v2", func(from, _ string) bool { return from == dstPath }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFake(t)
			drv := newDriver(t, config(f))
			seed(t, drv, "src", "new")
			seed(t, drv, "dst", "old")
			f.on(c.endpoint, f.failRelocationsWhen(c.endpoint, c.fail))
			if err := c.op(drv, ctx); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
				t.Fatalf("err = %v, want CodeQuotaExceeded", err)
			}
			if got := content(t, drv, "dst"); got != "old" {
				t.Errorf("dst = %q after a failed %s, want the old content", got, c.name)
			}
			if got := content(t, drv, "src"); got != "new" {
				t.Errorf("src = %q, want it unharmed", got)
			}
			if left := f.swapFiles(); len(left) != 0 {
				t.Errorf("transient files left: %v", left)
			}
		})
	}

	// When the swap-in and its rollback both fail, the old content is not
	// lost either: it stays at the transient name, which the error names.
	ctx := context.Background()
	f := newFake(t)
	drv := newDriver(t, config(f))
	seed(t, drv, "src", "new")
	seed(t, drv, "dst", "old")
	f.on("files/move_v2", f.failRelocationsWhen("files/move_v2", func(_, to string) bool { return to == dstPath }))
	_, err := drv.Move(ctx, "src", "dst")
	if filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded || !strings.Contains(err.Error(), dropbox.SwapPrefix) {
		t.Fatalf("err = %v, want CodeQuotaExceeded naming the kept file", err)
	}
	left := f.swapFiles()
	if len(left) != 1 {
		t.Fatalf("transient files: %v, want the kept old dst", left)
	}
	if got := content(t, drv, strings.TrimPrefix(left[0], "/apps/test/")); got != "old" {
		t.Errorf("kept file = %q, want the old content", got)
	}
}

// firstOnly matches what match matches, the first time only.
func firstOnly(match func(from, to string) bool) func(from, to string) bool {
	done := false
	return func(from, to string) bool {
		if done || !match(from, to) {
			return false
		}
		done = true
		return true
	}
}

func copyOp(d *dropbox.Driver, ctx context.Context) error {
	_, err := d.Copy(ctx, "src", "dst")
	return err
}

func moveOp(d *dropbox.Driver, ctx context.Context) error {
	_, err := d.Move(ctx, "src", "dst")
	return err
}

func seed(t *testing.T, d *dropbox.Driver, key, body string) {
	t.Helper()
	if _, err := d.Write(context.Background(), key, strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func content(t *testing.T, d *dropbox.Driver, key string) string {
	t.Helper()
	r, err := d.Read(context.Background(), key, filesystem.ByteRange{})
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRelocateDestinationPaths covers the destination checks relocate
// makes before choosing a path: a case-only Move renames in place (and its
// failure is returned), a failing stat of dst other than not_found fails
// the operation without touching anything, and a replace of a nested key
// stages its transient files in the key's own folder.
func TestRelocateDestinationPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("case-only move", func(t *testing.T) {
		f := newFake(t)
		drv := newDriver(t, config(f))
		seed(t, drv, "a", "x")
		if e, err := drv.Move(ctx, "a", "A"); err != nil || e.Key != "A" {
			t.Fatalf("case-only move: %+v, %v", e, err)
		}
		f.on("files/move_v2", f.failWith(http.StatusConflict, "insufficient_space/"))
		if _, err := drv.Move(ctx, "A", "a"); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
			t.Fatalf("failing case-only move: %v, want CodeQuotaExceeded", err)
		}
	})

	t.Run("stat of dst fails", func(t *testing.T) {
		f := newFake(t)
		drv := newDriver(t, config(f))
		seed(t, drv, "src", "new")
		seed(t, drv, "dst", "old")
		// get_metadata answers for src as the fake does, and refuses dst.
		f.on("files/get_metadata", func(w http.ResponseWriter, r *http.Request) {
			var arg struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&arg)
			file := f.files[strings.ToLower(arg.Path)]
			if strings.HasSuffix(arg.Path, "/dst") || file == nil {
				f.fail(w, http.StatusForbidden, "")
				return
			}
			_ = json.NewEncoder(w).Encode(f.meta(file))
		})
		for name, op := range map[string]func(*dropbox.Driver, context.Context) error{"copy": copyOp, "move": moveOp} {
			if err := op(drv, ctx); filesystem.CodeOf(err) != filesystem.CodePermission {
				t.Fatalf("%s with a failing stat of dst: %v, want CodePermission", name, err)
			}
		}
		if n := f.requests["files/copy_v2"] + f.requests["files/move_v2"]; n != 0 {
			t.Fatalf("%d relocations after a failing stat of dst, want none", n)
		}
	})

	t.Run("nested key", func(t *testing.T) {
		f := newFake(t)
		drv := newDriver(t, config(f))
		seed(t, drv, "src", "new")
		seed(t, drv, "d/dst", "old")
		var staged []string
		f.on("files/copy_v2", func(w http.ResponseWriter, r *http.Request) {
			var arg struct {
				From string `json:"from_path"`
				To   string `json:"to_path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&arg)
			staged = append(staged, arg.To)
			f.relocate(w, "files/copy_v2", arg.From, arg.To)
		})
		if _, err := drv.Copy(ctx, "src", "d/dst"); err != nil {
			t.Fatal(err)
		}
		if len(staged) != 1 || !strings.HasPrefix(staged[0], "/Apps/Test/d/"+dropbox.SwapPrefix) {
			t.Fatalf("copy staged at %v, want one file in /Apps/Test/d/", staged)
		}
		if got := content(t, drv, "d/dst"); got != "new" {
			t.Fatalf("d/dst = %q, want new", got)
		}
	})
}
