package tus_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"

	"github.com/ubgo/filesystem"
	fstus "github.com/ubgo/filesystem/contrib/tus"
	"github.com/ubgo/filesystem/memory"
)

// preflight sends a CORS preflight for a PATCH carrying Upload-Session.
func (e *env) preflight(t *testing.T, origin string) *http.Response {
	t.Helper()
	return e.do(t, http.MethodOptions, e.srv.URL+"/uploads/", nil, map[string]string{
		"Origin": origin, "Access-Control-Request-Method": http.MethodPatch,
		"Access-Control-Request-Headers": "Upload-Session, Upload-Offset",
	})
}

// TestCors_DefaultAllowsUploadSession pins B6: without Config.Cors the
// preflight allows Upload-Session, which upload sessions send with every
// request, and keeps tusd's own headers and any origin.
func TestCors_DefaultAllowsUploadSession(t *testing.T) {
	e := setup(t, fstus.Config{})
	resp := e.preflight(t, "https://app.test")
	allowed := resp.Header.Get("Access-Control-Allow-Headers")
	if resp.StatusCode != http.StatusOK || !strings.Contains(allowed, fstus.HeaderUploadSession) || !strings.Contains(allowed, "Upload-Offset") {
		t.Fatalf("preflight = %d, allowed headers %q", resp.StatusCode, allowed)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.test" {
		t.Fatalf("allowed origin = %q", got)
	}
	if strings.Contains(handler.DefaultCorsConfig.AllowHeaders, fstus.HeaderUploadSession) {
		t.Fatal("DefaultCors changed tusd's own default")
	}
}

// TestCors_CustomOriginsHonoured pins B6: a Config.Cors built from
// DefaultCors with a restricted AllowOrigin refuses other origins and
// still allows Upload-Session from the allowed one.
func TestCors_CustomOriginsHonoured(t *testing.T) {
	cors := fstus.DefaultCors()
	cors.AllowOrigin = regexp.MustCompile(`^https://app\.test$`)
	cors.AllowCredentials = true
	e := setup(t, fstus.Config{Cors: cors})
	if resp := e.preflight(t, "https://evil.test"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign origin got %d", resp.StatusCode)
	}
	resp := e.preflight(t, "https://app.test")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), fstus.HeaderUploadSession) || resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("allowed origin: %d %v", resp.StatusCode, resp.Header)
	}
}

// countingStore is a filestore that counts the uploads it creates.
type countingStore struct {
	filestore.FileStore
	created atomic.Int32
}

func (s *countingStore) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	s.created.Add(1)
	return s.FileStore.NewUpload(ctx, info)
}

// UseIn installs s itself, not the embedded filestore, so the composer
// calls the counting NewUpload.
func (s *countingStore) UseIn(c *handler.StoreComposer) {
	s.FileStore.UseIn(c)
	c.UseCore(s)
}

// countingLocker is a memorylocker that counts the locks it hands out.
type countingLocker struct {
	*memorylocker.MemoryLocker
	locks atomic.Int32
}

func (l *countingLocker) NewLock(id string) (handler.Lock, error) {
	l.locks.Add(1)
	return l.MemoryLocker.NewLock(id)
}

// TestStore_CustomStoreAndLocker pins B7: Config.Store keeps the sessions
// instead of a filestore in TempDir, Config.Locker locks them, and the
// upload completes through both.
func TestStore_CustomStoreAndLocker(t *testing.T) {
	dir := t.TempDir()
	store := &countingStore{FileStore: filestore.New(dir)}
	locker := &countingLocker{MemoryLocker: memorylocker.New()}
	e := setup(t, fstus.Config{Store: store, Locker: locker})
	loc, resp := e.create(t, len(pngData), map[string]string{fstus.MetaFilename: "me.png"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	if n := sessions(t, dir); n == 0 {
		t.Fatal("the custom store holds no session")
	}
	final := e.send(t, loc, pngData)
	if final.StatusCode != http.StatusNoContent || final.Header.Get(fstus.HeaderKey) == "" {
		t.Fatalf("final = %d", final.StatusCode)
	}
	if store.created.Load() != 1 || locker.locks.Load() == 0 {
		t.Fatalf("store created %d, locker locked %d", store.created.Load(), locker.locks.Load())
	}
	if n := sessions(t, e.tmp); n != 0 {
		t.Fatalf("%d files in TempDir; the custom store was bypassed", n)
	}
	if n := sessions(t, dir); n != 0 {
		t.Fatalf("%d session files left in the custom store", n)
	}
}

// TestStore_Validation: a Store needs a Locker (a lock local to one
// instance would not guard a shared store), and TempDir may be left out
// only when Store is set.
func TestStore_Validation(t *testing.T) {
	disk, _ := filesystem.NewDisk("media", memory.New())
	base := fstus.Config{Disk: disk, Policy: images, BasePath: "/uploads/"}
	cfg := base
	cfg.Store = filestore.New(t.TempDir())
	if _, err := fstus.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Store without Locker: %v", err)
	}
	cfg.Locker = memorylocker.New()
	if _, err := fstus.New(cfg); err != nil {
		t.Fatalf("Store with Locker and no TempDir: %v", err)
	}
	if _, err := fstus.New(base); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("neither TempDir nor Store: %v", err)
	}
}

// TestDefault_Locks pins B7's default: without Config.Locker each upload
// is locked with tusd's filelocker in TempDir while a request works on
// it, so two PATCHes never write one upload at once. The lock file
// (<id>.lock) exists while a PATCH is still receiving its body.
func TestDefault_Locks(t *testing.T) {
	e := setup(t, fstus.Config{})
	loc, resp := e.create(t, len(pngData), nil, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	lockFile := filepath.Join(e.tmp, path.Base(loc)+".lock")
	body, feed := io.Pipe()
	done := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, loc, body)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", "0")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- nil
			return
		}
		_ = r.Body.Close()
		done <- r
	}()
	if _, err := feed.Write([]byte(pngData[:4])); err != nil {
		t.Fatal(err)
	}
	locked := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(lockFile); err == nil {
			locked = true
			break
		}
	}
	_ = feed.Close()
	if r := <-done; r == nil || r.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH = %v", r)
	}
	if !locked {
		t.Fatal("no lock was held while the PATCH ran")
	}
	if _, err := os.Stat(lockFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the lock outlived the request: %v", err)
	}
}

// TestSweep_RemovesLeftoverLocks: a crash can leave the filelocker's
// .lock and .stop files of a session behind; Sweep deletes them with the
// abandoned session and leaves a live session's alone.
func TestSweep_RemovesLeftoverLocks(t *testing.T) {
	e := setup(t, fstus.Config{})
	now := time.Now()
	_, oldID := e.abandon(t)
	_, freshID := e.abandon(t)
	age(t, e.tmp, oldID, now.Add(-48*time.Hour))
	for _, id := range []string{oldID, freshID} {
		for _, suffix := range []string{".lock", ".stop"} {
			if err := os.WriteFile(filepath.Join(e.tmp, id+suffix), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := fstus.Sweep(t.Context(), e.tmp, now.Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	for _, suffix := range []string{".lock", ".stop"} {
		if exists(e.tmp, oldID+suffix) {
			t.Errorf("the swept session's %s file survived", suffix)
		}
		if !exists(e.tmp, freshID+suffix) {
			t.Errorf("the live session's %s file was swept", suffix)
		}
	}
}
