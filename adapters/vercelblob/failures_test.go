package vercelblob_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/vercelblob"
)

// match selects the requests an intercept answers.
type match func(r *http.Request) bool

// isDelete matches the API's delete request; isPut its PUT; isBlobGet a
// read on the blob host; isList a listing.
func isDelete(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete")
}
func isPut(r *http.Request) bool { return r.Method == http.MethodPut }
func isBlobGet(r *http.Request) bool {
	return r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/")
}
func isList(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/") && !r.URL.Query().Has("url")
}

// answer is an intercept answering the requests m matches with status and
// an API error code.
func answer(m match, status int, code string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !m(r) {
			return false
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":"`+code+`"}}`)
		return true
	}
}

// seeded returns a private-store driver over a fake holding the keys.
func seeded(t *testing.T, keys ...string) (*vercelblob.Driver, *fake) {
	t.Helper()
	f := newFake(t, false)
	drv := newDriver(t, config(f, vercelblob.AccessPrivate))
	for _, k := range keys {
		if _, err := drv.Write(context.Background(), k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return drv, f
}

// TestConfigEdges pins that an empty token prints as empty (not as a
// redacted secret), that https endpoints are accepted, and that a
// plain-http blob host off the loopback interface is refused.
func TestConfigEdges(t *testing.T) {
	if s := (vercelblob.Config{}).String(); !strings.Contains(s, `Token: ""`) {
		t.Fatalf("String() = %s, want an empty token shown as empty", s)
	}
	cfg := vercelblob.Config{Token: testToken, Access: vercelblob.AccessPublic, APIURL: "https://api.example.com"}
	if _, err := vercelblob.New(cfg); err != nil {
		t.Fatalf("https API: %v", err)
	}
	cfg.BlobURL = "http://blob.example.com"
	if _, err := vercelblob.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("plain-http blob host: %v, want CodeInvalidArgument", err)
	}
}

// TestRetryAfterAndUnknownStatus pins that a 5xx carries the Retry-After
// it was sent with and that an unknown status with no code is CodeUnknown.
func TestRetryAfterAndUnknownStatus(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t)
	f.on(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	_, err := drv.Stat(ctx, "x")
	if d, ok := filesystem.RetryAfterOf(err); filesystem.CodeOf(err) != filesystem.CodeUnavailable || !ok || d != 3*time.Second {
		t.Fatalf("503: %v (retry %v), want CodeUnavailable after 3s", err, d)
	}
	f.on(answer(func(*http.Request) bool { return true }, http.StatusTeapot, ""))
	if _, err := drv.Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodeUnknown {
		t.Fatalf("418: %v, want CodeUnknown", err)
	}
}

// hangUp drops the connection without answering.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// TestTransportFailures pins that a dropped connection is CodeUnavailable
// (for the API and for a read on the blob host), and that a request whose
// context ended returns the context's own error, unclassified.
func TestTransportFailures(t *testing.T) {
	drv, f := seeded(t, "k")
	f.on(func(w http.ResponseWriter, r *http.Request) bool { hangUp(w, r); return true })
	if _, err := drv.Stat(context.Background(), "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("dropped stat: %v, want CodeUnavailable", err)
	}
	if _, err := drv.Read(context.Background(), "k", filesystem.ByteRange{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("dropped read: %v, want CodeUnavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var fe *filesystem.Error
	if _, err := drv.Stat(ctx, "k"); !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
		t.Fatalf("cancelled: %v, want the context error itself, unclassified", err)
	}
}

// TestReadWhenTheHostIgnoresRange pins the client-side cut of a 200 that
// answers a ranged read with the whole blob, and that a body cut short
// before the offset is CodeUnavailable.
func TestReadWhenTheHostIgnoresRange(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, "r")
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !isBlobGet(r) {
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
		if !isBlobGet(r) {
			return false
		}
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "01234")
		return true
	})
	if _, err := drv.Read(ctx, "r", filesystem.ByteRange{Offset: 50}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("truncated answer: %v, want CodeUnavailable", err)
	}
}

// TestCompareAndSwapOfAMissingBlob pins that a swap the API refuses as
// not_found (the blob is gone) is CodeConflict, as every driver reports a
// failed swap.
func TestCompareAndSwapOfAMissingBlob(t *testing.T) {
	drv, f := seeded(t)
	f.on(answer(isPut, http.StatusNotFound, "not_found"))
	if _, err := drv.Write(context.Background(), "k", strings.NewReader("x"), filesystem.WriteOptions{IfMatch: "e1"}); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Fatalf("got %v, want CodeConflict", err)
	}
}

// TestCopyEdges pins Copy onto itself (the blob, no request to copy) and a
// refused copy.
func TestCopyEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, "s")
	puts := 0
	f.on(func(_ http.ResponseWriter, r *http.Request) bool {
		if isPut(r) {
			puts++
		}
		return false
	})
	if e, err := drv.Copy(ctx, "s", "s"); err != nil || e.Key != "s" || puts != 0 {
		t.Fatalf("copy onto itself: %+v, %v, %d PUTs", e, err, puts)
	}
	f.on(answer(isPut, http.StatusForbidden, "forbidden"))
	if _, err := drv.Copy(ctx, "s", "d"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused copy: %v, want CodePermission", err)
	}
}

// TestDeleteEdges pins that a delete the API answers not_found is a
// success, that DeleteKeys reports every key of a failed batch in a
// BatchError while still trying the other batches, and that it stops with
// the context's error once the context ends.
func TestDeleteEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, "k")
	f.on(answer(isDelete, http.StatusNotFound, "not_found"))
	if err := drv.Delete(ctx, "k"); err != nil {
		t.Fatalf("not_found delete: %v, want success", err)
	}
	keys := make([]string, vercelblob.DeleteBatch+1)
	for i := range keys {
		keys[i] = "k" + strings.Repeat("x", i)
	}
	batch := 0
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !isDelete(r) {
			return false
		}
		if batch++; batch == 1 {
			return answer(isDelete, http.StatusInternalServerError, "")(w, r)
		}
		return false
	})
	err := drv.DeleteKeys(ctx, keys)
	var be *filesystem.BatchError
	if !errors.As(err, &be) || len(be.Failures) != vercelblob.DeleteBatch || batch != 2 {
		t.Fatalf("got %v after %d batches, want %d failed keys and both batches tried", err, batch, vercelblob.DeleteBatch)
	}
	cctx, cancel := context.WithCancel(ctx)
	batch = 0
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		batch++
		cancel()
		return answer(isDelete, http.StatusInternalServerError, "")(w, r)
	})
	if err := drv.DeleteKeys(cctx, keys); !errors.Is(err, context.Canceled) || errors.As(err, &be) || batch != 1 {
		t.Fatalf("cancelled: %v after %d batches, want context.Canceled alone after the first", err, batch)
	}
}

// TestListFailure pins that a failing page fails the listing.
func TestListFailure(t *testing.T) {
	drv, f := seeded(t)
	f.on(answer(isList, http.StatusInternalServerError, "service_unavailable"))
	if _, err := drv.List(context.Background(), "", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("got %v, want CodeUnavailable", err)
	}
}
