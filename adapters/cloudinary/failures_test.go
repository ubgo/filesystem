package cloudinary_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/cloudinary"
)

// match selects the requests an intercept answers.
type match func(r *http.Request) bool

func isAction(action string) match {
	return func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/raw/"+action) }
}

// isAdmin matches the Admin API requests with method m.
func isAdmin(m string) match {
	return func(r *http.Request) bool { return r.Method == m && strings.Contains(r.URL.Path, "/resources/raw/") }
}

// every matches every request.
func every(*http.Request) bool { return true }

// answer is an intercept answering the requests m matches with status.
func answer(m match, status int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !m(r) {
			return false
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"injected"}}`)
		return true
	}
}

func seeded(t *testing.T, keys ...string) (*cloudinary.Driver, *fake) {
	t.Helper()
	f := newFake(t)
	drv := newDriver(t, config(f, cloudinary.TypeAuthenticated))
	for _, k := range keys {
		if _, err := drv.Write(context.Background(), k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return drv, f
}

// TestConfigEdges pins that an empty secret prints as empty (not as a
// redacted one) and that a plain-http delivery host off the loopback
// interface is refused.
func TestConfigEdges(t *testing.T) {
	if s := (cloudinary.Config{}).String(); !strings.Contains(s, `APISecret: ""`) {
		t.Fatalf("String() = %s, want the empty secret shown as empty", s)
	}
	cfg := cloudinary.Config{CloudName: testCloud, APIKey: testKey, APISecret: testSecret, DeliveryURL: "http://res.example.com"}
	if _, err := cloudinary.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("plain-http delivery host: %v, want CodeInvalidArgument", err)
	}
}

// TestStatusFallbacks pins that a 5xx carries its Retry-After and that an
// unknown status is CodeUnknown.
func TestStatusFallbacks(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t)
	f.on(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "6")
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	_, err := drv.Stat(ctx, "x")
	if d, ok := filesystem.RetryAfterOf(err); filesystem.CodeOf(err) != filesystem.CodeUnavailable || !ok || d != 6*time.Second {
		t.Fatalf("502: %v (retry %v), want CodeUnavailable after 6s", err, d)
	}
	f.on(answer(every, http.StatusTeapot))
	if _, err := drv.Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodeUnknown {
		t.Fatalf("418: %v, want CodeUnknown", err)
	}
}

// TestTransportFailures pins that a dropped connection is CodeUnavailable
// for the Upload API, the Admin API and a write, and that a request whose
// context ended returns the context's own error, unclassified.
func TestTransportFailures(t *testing.T) {
	drv, f := seeded(t, "k")
	f.on(func(w http.ResponseWriter, r *http.Request) bool { hangUp(w, r); return true })
	ctx := context.Background()
	if _, err := drv.Stat(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("admin: %v, want CodeUnavailable", err)
	}
	if err := drv.Delete(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("upload API: %v, want CodeUnavailable", err)
	}
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("write: %v, want CodeUnavailable", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	var fe *filesystem.Error
	if _, err := drv.Stat(cctx, "k"); !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
		t.Fatalf("cancelled: %v, want the context error itself, unclassified", err)
	}
}

// TestWriteToAServerThatIsGone pins that a write whose request never
// connects fails as CodeUnavailable, its form writer stopping at the
// first field rather than blocking.
func TestWriteToAServerThatIsGone(t *testing.T) {
	drv, f := seeded(t)
	f.Close()
	if _, err := drv.Write(context.Background(), "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("got %v, want CodeUnavailable", err)
	}
}

// TestReadWhenTheServerIgnoresRange pins the client-side cut of a 200 that
// answers a ranged read with the whole file, and that a body cut short
// before the offset is CodeUnavailable.
func TestReadWhenTheServerIgnoresRange(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, "r")
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !isAction("download")(r) {
			return false
		}
		_, _ = io.WriteString(w, "0123456789")
		return true
	})
	for _, c := range []struct {
		rng  filesystem.ByteRange
		want string
	}{
		{filesystem.ByteRange{Offset: 2, Length: 3}, "234"},
		{filesystem.ByteRange{Offset: 7}, "789"},
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
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !isAction("download")(r) {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(100))
		_, _ = io.WriteString(w, "01234")
		return true
	})
	if _, err := drv.Read(ctx, "r", filesystem.ByteRange{Offset: 50}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("truncated answer: %v, want CodeUnavailable", err)
	}
}

// TestRefusedRename pins that a refused rename fails Move.
func TestRefusedRename(t *testing.T) {
	drv, f := seeded(t, "s")
	f.on(answer(isAction("rename"), http.StatusForbidden))
	if _, err := drv.Move(context.Background(), "s", "d"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("got %v, want CodePermission", err)
	}
}

// TestDeleteKeysFailures pins that a failed batch reports each of its keys
// in a BatchError while the other batches are still tried, and that the
// context's end stops DeleteKeys with the context's error alone.
func TestDeleteKeysFailures(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t)
	keys := make([]string, cloudinary.DeleteBatch+1)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}
	batches := 0
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !isAdmin(http.MethodDelete)(r) {
			return false
		}
		if batches++; batches == 1 {
			return answer(every, http.StatusInternalServerError)(w, r)
		}
		return false
	})
	err := drv.DeleteKeys(ctx, keys)
	var be *filesystem.BatchError
	if !errors.As(err, &be) || len(be.Failures) != cloudinary.DeleteBatch || batches != 2 {
		t.Fatalf("got %v after %d batches, want %d failed keys and both batches tried", err, batches, cloudinary.DeleteBatch)
	}
	cctx, cancel := context.WithCancel(ctx)
	batches = 0
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		batches++
		cancel()
		return answer(every, http.StatusInternalServerError)(w, r)
	})
	if err := drv.DeleteKeys(cctx, keys); !errors.Is(err, context.Canceled) || errors.As(err, &be) || batches != 1 {
		t.Fatalf("cancelled: %v after %d batches, want context.Canceled alone after the first", err, batches)
	}
}

// TestPrefixAndListFailures pins that a refused prefix delete and a
// refused listing page are returned, and that a listed public ID with an
// incomplete escape (not one this driver wrote) is skipped.
func TestPrefixAndListFailures(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t)
	f.on(answer(isAdmin(http.MethodDelete), http.StatusUnauthorized))
	if err := drv.DeletePrefix(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("prefix delete: %v, want CodePermission", err)
	}
	f.on(answer(isAdmin(http.MethodGet), statusRateLimited))
	if _, err := drv.List(ctx, "", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("listing: %v, want CodeUnavailable", err)
	}
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !isAdmin(http.MethodGet)(r) {
			return false
		}
		_, _ = io.WriteString(w, `{"resources":[{"public_id":"bad~4"},{"public_id":"ok","bytes":2}]}`)
		return true
	})
	p, err := drv.List(ctx, "", filesystem.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Key != "ok" {
		t.Fatalf("entries %+v, want only ok", p.Entries)
	}
}

// statusRateLimited is the Admin API's hourly-limit status.
const statusRateLimited = 420

// TestControlCharacterInCloudName pins that a cloud name New lets through
// but no URL can hold (a control character) fails every operation with
// CodeInvalidArgument before any request is sent.
func TestControlCharacterInCloudName(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	cfg := config(f, cloudinary.TypeAuthenticated)
	cfg.CloudName = "bad\x7fname"
	drv := newDriver(t, cfg)
	ops := map[string]error{}
	_, ops["stat"] = drv.Stat(ctx, "k")
	_, ops["read"] = drv.Read(ctx, "k", filesystem.ByteRange{})
	_, ops["write"] = drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{})
	ops["delete"] = drv.Delete(ctx, "k")
	for op, err := range ops {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", op, err)
		}
	}
	if f.adminCalls != 0 {
		t.Fatalf("%d requests reached the server", f.adminCalls)
	}
}
