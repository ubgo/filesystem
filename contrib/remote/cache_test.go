package remote_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
	"github.com/ubgo/filesystem/upload"
)

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Example.COM:443/a?b=1#frag": "https://example.com/a?b=1",
		"http://example.com":                 "http://example.com/",
		"http://example.com:8080/x":          "http://example.com:8080/x",
		"https://[::1]:443/":                 "https://[::1]/",
		"https://[2001:DB8::1]:8443/p":       "https://[2001:db8::1]:8443/p",
	} {
		got, err := remote.Canonical(in)
		if err != nil || got != want {
			t.Errorf("Canonical(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ftp://example.com/a", "file:///etc/passwd", "/relative", "https://user:pw@example.com/", "http://", "javascript:alert(1)"} {
		_, err := remote.Canonical(in)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

func TestNewRefusesUnsafePolicies(t *testing.T) {
	disk := newDisk(t)
	_, settle := upload.ContentAddressed("x", upload.ContentAddressing{})
	for name, p := range map[string]upload.Policy{
		"invalid":        {},
		"active content": {MaxSize: 1, Allowed: []string{"image/*"}, AllowActiveContent: true},
		"active type":    {MaxSize: 1, Allowed: []string{"image/*"}, AllowActive: []string{"image/svg+xml"}},
		"settle":         {MaxSize: 1, Allowed: []string{"image/*"}, Settle: settle},
	} {
		if _, err := remote.New(disk, remote.Options{Dir: "remote", Policy: p}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: New = %v, want CodeInvalidArgument", name, err)
		}
	}
}

// The cache owns its Dir: with the disk's root, Sweep would evict the
// application's own media/ and preview/ files.
func TestNewRequiresADir(t *testing.T) {
	disk := newDisk(t)
	for dir, code := range map[string]filesystem.Code{
		"":      filesystem.CodeInvalidArgument,
		" \t":   filesystem.CodeInvalidArgument,
		"/":     filesystem.CodeInvalidKey,
		"..":    filesystem.CodeInvalidKey,
		"../up": filesystem.CodeInvalidKey,
	} {
		if _, err := remote.New(disk, remote.Options{Dir: dir, Policy: imagePolicy()}); filesystem.CodeOf(err) != code {
			t.Errorf("Dir %q: New = %v, want %s", dir, err, code)
		}
	}
}

// A caller that cancels does not fail the fetch for the callers waiting
// on the same URL.
func TestOneCallerCancellingDoesNotFailTheOthers(t *testing.T) {
	release := make(chan struct{})
	srv := serve(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		<-release
		body("image/png", pngBytes)(w, r)
	})
	c, _ := newCache(t)
	first, cancel := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := c.Get(first, srv.URL+"/a.png")
		firstErr <- err
	}()
	for srv.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	secondErr := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), srv.URL+"/a.png")
		secondErr <- err
	}()
	time.Sleep(20 * time.Millisecond) // the second caller joins the flight
	cancel()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", err)
	}
	close(release)
	if err := <-secondErr; err != nil {
		t.Fatalf("the waiting caller failed with the other's cancellation: %v", err)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
}

func TestGetCachesUnderTheURLHash(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	c, disk := newCache(t)
	raw := srv.URL + "/Avatar.PNG?size=2#ignored"
	for range 3 {
		e, err := c.Get(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		if e.ContentType != "image/png" || e.Size != int64(len(pngBytes)) {
			t.Fatalf("entry %+v", e)
		}
		canon, _ := remote.Canonical(raw)
		sum := sha256.Sum256([]byte(canon))
		if path.Base(e.Key) != hex.EncodeToString(sum[:]) || strings.Contains(e.Key, "127.0.0.1") || strings.Contains(strings.ToLower(e.Key), "avatar") {
			t.Fatalf("key %q is not the URL's hash", e.Key)
		}
		if k, _ := c.MediaKey(raw); k != e.Key {
			t.Fatalf("MediaKey %q, Get stored %q", k, e.Key)
		}
		if ok, _ := disk.Exists(ctx, e.Key); !ok {
			t.Fatal("nothing stored")
		}
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("server hit %d times, want 1", n)
	}
}

func TestGetRefusesActiveContentByItsBytes(t *testing.T) {
	ctx := context.Background()
	for name, b := range map[string][]byte{"svg": svgBytes, "html": []byte("<!DOCTYPE html><html><script>alert(1)</script>")} {
		srv := serve(t, "127.0.0.1:0", body("image/png", b))
		c, disk := newCache(t)
		_, err := c.Get(ctx, srv.URL+"/a.png")
		if filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Fatalf("%s served as image/png: %v, want CodeRejected", name, err)
		}
		k, _ := c.MediaKey(srv.URL + "/a.png")
		if ok, _ := disk.Exists(ctx, k); ok {
			t.Fatalf("%s was cached", name)
		}
	}
}

func TestGetGoesThroughTheSSRFGuard(t *testing.T) {
	ctx := context.Background()
	internal := serve(t, "[::1]:0", body("image/png", pngBytes))
	c, _ := newCache(t)
	_, err := c.Get(ctx, internal.URL+"/a.png")
	wantCode(t, err, filesystem.CodeInvalidArgument)

	// A public server redirecting to the internal one is refused too.
	redirect := serve(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/a.png", http.StatusFound)
	})
	_, err = c.Get(ctx, redirect.URL+"/a.png")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if redirect.hits.Load() != 1 || internal.hits.Load() != 0 {
		t.Fatalf("redirect hits %d, internal hits %d; the internal server was reached", redirect.hits.Load(), internal.hits.Load())
	}

	// Without AllowAddr even the "public" test server is loopback.
	public := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	strict, _ := newCache(t, func(o *remote.Options) { o.Fetch = upload.FetchOptions{} })
	_, err = strict.Get(ctx, public.URL+"/a.png")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if public.hits.Load() != 0 {
		t.Fatal("the guard let a loopback request through")
	}
}

func TestGetCapsTheSize(t *testing.T) {
	big := append([]byte(nil), pngBytes...)
	big = append(big, make([]byte, 2<<20)...)
	srv := serve(t, "127.0.0.1:0", body("image/png", big))
	c, _ := newCache(t)
	_, err := c.Get(context.Background(), srv.URL+"/big.png")
	wantCode(t, err, filesystem.CodeTooLarge)
}

func TestConcurrentMissesFetchOnce(t *testing.T) {
	srv := serve(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		body("image/png", pngBytes)(w, r)
	})
	c, _ := newCache(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := c.Get(context.Background(), srv.URL+"/a.png")
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("8 concurrent misses fetched %d times, want 1", n)
	}
}

func TestOpenAndPurge(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	c, disk := newCache(t)
	raw := srv.URL + "/a.png"
	rc, e, err := c.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if err := c.Purge(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if ok, _ := disk.Exists(ctx, e.Key); ok {
		t.Fatal("Purge left the media")
	}
	if err := c.Purge(ctx, raw); err != nil {
		t.Fatalf("purging what is not cached: %v", err)
	}
	if _, err := c.Get(ctx, raw); err != nil || srv.hits.Load() != 2 {
		t.Fatalf("after Purge, Get = %v with %d hits; want a refetch", err, srv.hits.Load())
	}
	// Open fetches again when the entry vanished between Stat and Read.
	if err := disk.Delete(ctx, e.Key); err != nil {
		t.Fatal(err)
	}
	if rc, _, err := c.Open(ctx, raw); err != nil {
		t.Fatal(err)
	} else {
		_ = rc.Close()
	}
}

// Fetches nobody waits for cannot pile up: past MaxConcurrentFetches a new
// fetch is refused, while a request for a URL being fetched still joins.
func TestConcurrentFetchesAreCapped(t *testing.T) {
	release := make(chan struct{})
	srv := serve(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		<-release
		body("image/png", pngBytes)(w, r)
	})
	defer close(release)
	c, _ := newCache(t, func(o *remote.Options) { o.MaxConcurrentFetches = 2 })
	for _, name := range []string{"a", "b"} {
		go func() { _, _ = c.Get(context.Background(), srv.URL+"/"+name+".png") }()
	}
	for srv.hits.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	// Bounded, so a missing cap fails here instead of hanging.
	third, cancelThird := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelThird()
	_, err := c.Get(third, srv.URL+"/c.png")
	if errors.Is(err, context.DeadlineExceeded) || srv.hits.Load() != 2 {
		t.Fatalf("a third fetch over the cap was started and waited for: %v", err)
	}
	wantCode(t, err, filesystem.CodeUnavailable)
	join, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Get(join, srv.URL+"/a.png"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("joining a running fetch: %v, want to wait for it", err)
	}
}

// A fetch whose every caller left is cancelled upstream and frees its slot.
func TestAbandonedFetchIsCancelled(t *testing.T) {
	upstreamGone := make(chan struct{})
	srv := serve(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow.png" {
			<-r.Context().Done()
			close(upstreamGone)
			return
		}
		body("image/png", pngBytes)(w, r)
	})
	c, _ := newCache(t, func(o *remote.Options) { o.MaxConcurrentFetches = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, srv.URL+"/slow.png")
		done <- err
	}()
	for srv.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	select {
	case <-upstreamGone:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned fetch kept its connection open")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := c.Get(context.Background(), srv.URL+"/next.png")
		if err == nil {
			break
		}
		if filesystem.CodeOf(err) != filesystem.CodeUnavailable || time.Now().After(deadline) {
			t.Fatalf("after the abandoned fetch: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
