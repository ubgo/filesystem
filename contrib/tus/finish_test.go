package tus_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"

	"github.com/ubgo/filesystem"
	fstus "github.com/ubgo/filesystem/contrib/tus"
)

// errFinished is what failingStore answers when the finished session is
// read back.
var errFinished = errors.New("session store unavailable")

// failingStore is a filestore whose finished sessions cannot be read
// back: GetUpload of a complete upload fails (failGet), or its reader
// does (failRead). A shared Config.Store (s3store, Redis-backed) can fail
// this way between the last PATCH and the promotion.
type failingStore struct {
	filestore.FileStore
	failGet, failRead bool
}

func (s *failingStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	up, err := s.FileStore.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := up.GetInfo(ctx)
	if err != nil {
		return nil, err
	}
	if info.Size > 0 && info.Offset == info.Size {
		if s.failGet {
			return nil, errFinished
		}
		if s.failRead {
			return failingReader{up}, nil
		}
	}
	return up, nil
}

// UseIn installs s itself, so the composer calls the failing GetUpload.
func (s *failingStore) UseIn(c *handler.StoreComposer) {
	s.FileStore.UseIn(c)
	c.UseCore(s)
}

// AsTerminatableUpload unwraps failingReader: filestore's own version
// accepts only its own upload type.
func (s *failingStore) AsTerminatableUpload(up handler.Upload) handler.TerminatableUpload {
	if f, ok := up.(failingReader); ok {
		up = f.Upload
	}
	return s.FileStore.AsTerminatableUpload(up)
}

type failingReader struct{ handler.Upload }

func (failingReader) GetReader(context.Context) (io.ReadCloser, error) { return nil, errFinished }

// TestFinish_StoreFailure: when a custom Store cannot hand back the
// finished session (the session or its bytes), the final PATCH fails and
// nothing is stored on the disk.
func TestFinish_StoreFailure(t *testing.T) {
	for name, store := range map[string]*failingStore{
		"get upload": {failGet: true},
		"get reader": {failRead: true},
	} {
		t.Run(name, func(t *testing.T) {
			store.FileStore = filestore.New(t.TempDir())
			e := setup(t, fstus.Config{Store: store, Locker: memorylocker.New()})
			loc, resp := e.create(t, len(pngData), map[string]string{fstus.MetaFilename: "me.png"}, nil)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("create = %d", resp.StatusCode)
			}
			half := len(pngData) / 2
			h := map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}
			if r := e.do(t, http.MethodPatch, loc, strings.NewReader(pngData[:half]), h); r.StatusCode != http.StatusNoContent {
				t.Fatalf("first chunk = %d", r.StatusCode)
			}
			h["Upload-Offset"] = strconv.Itoa(half)
			final := e.do(t, http.MethodPatch, loc, strings.NewReader(pngData[half:]), h)
			if final.StatusCode < 500 || final.Header.Get(fstus.HeaderKey) != "" {
				t.Fatalf("final = %d, key %q; want a server error and nothing stored", final.StatusCode, final.Header.Get(fstus.HeaderKey))
			}
			for entry, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
				t.Fatalf("stored from an unreadable session: %v %v", entry.Key, err)
			}
		})
	}
}
