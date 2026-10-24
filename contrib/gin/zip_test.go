package fsgin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ubgo/filesystem"
	fsgin "github.com/ubgo/filesystem/contrib/gin"
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

// zipEngine serves Zip of one object on d at /zip, behind recovery.
func zipEngine(t *testing.T, d *filesystem.Disk, recovery gin.HandlerFunc) *gin.Engine {
	t.Helper()
	if _, err := d.WriteBytes(t.Context(), "a.txt", []byte("quarterly numbers\n"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if recovery != nil {
		r.Use(recovery)
	}
	r.GET("/zip", func(c *gin.Context) {
		fsgin.Zip(c, d, "a.zip", []fshttp.ZipFile{{Key: "a.txt"}}, fshttp.ZipOptions{})
	})
	return r
}

// TestZip_PassesOtherPanicsOn pins that Zip turns only ServeZip's abort
// into a closed connection: any other panic reaches the application's
// recovery with its own value.
func TestZip_PassesOtherPanicsOn(t *testing.T) {
	d, err := filesystem.NewDisk("media", panickingDriver{memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	var recovered any
	r := zipEngine(t, d, gin.CustomRecovery(func(c *gin.Context, rec any) {
		recovered = rec
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/zip", nil))
	if recovered != panicValue {
		t.Fatalf("recovery got %v, want %q", recovered, panicValue)
	}
}

// brokenDriver is a memory driver whose reads fail after four bytes, as
// a backend that drops the connection mid-stream.
type brokenDriver struct{ *memory.Driver }

func (d brokenDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := d.Driver.Read(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, 4), errReader{}), rc}, nil
}

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// TestZip_ReAbortsWhereTheConnectionCannotBeHijacked covers a writer
// without Hijack (HTTP/2, a recorder): Zip cannot close the connection
// itself, so it re-raises http.ErrAbortHandler for the server to abort
// the stream.
func TestZip_ReAbortsWhereTheConnectionCannotBeHijacked(t *testing.T) {
	d, err := filesystem.NewDisk("media", brokenDriver{memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	r := zipEngine(t, d, nil)
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("panic = %v, want http.ErrAbortHandler", rec)
		}
	}()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/zip", nil))
	t.Fatal("a failed zip on an unhijackable writer returned normally")
}
