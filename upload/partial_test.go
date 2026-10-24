package upload_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func partialResponse(t *testing.T, rec *httptest.ResponseRecorder) upload.Response {
	t.Helper()
	var r upload.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v in %s", err, rec.Body)
	}
	return r
}

func TestPartialUploads(t *testing.T) {
	small := upload.Policy{MaxSize: 64, Allowed: []string{"image/*"}}
	var seen []upload.Result
	opts := upload.HandlerOptions{MaxFiles: 4, Partial: true, OnStored: func(_ context.Context, _ *http.Request, files []upload.Result) error {
		seen = files
		return nil
	}}

	disk := newDisk(t, memory.New())
	h := upload.Handler(disk, small, opts)
	body, ct := multipartBody(t, []formFile{
		{"file", "a.png", pngData},
		{"file", "../../evil.png", htmlData},
		{"file", "big.png", pngData + strings.Repeat("x", 100)},
		{"file", "b.png", pngData},
	}, nil)
	rec := post(t, h, body, ct)
	got := partialResponse(t, rec)
	if rec.Code != http.StatusOK || len(got.Files) != 2 || len(got.Failed) != 2 || len(seen) != 2 {
		t.Fatalf("mixed = %d %+v", rec.Code, got)
	}
	if f := got.Failed[0]; f.Filename != "evil.png" || f.Code != filesystem.CodeRejected || !strings.Contains(f.Error, "text/html") {
		t.Fatalf("refused file = %+v", f)
	}
	if f := got.Failed[1]; f.Filename != "big.png" || f.Code != filesystem.CodeTooLarge {
		t.Fatalf("large file = %+v", f)
	}
	if n := count(t, disk); n != 2 {
		t.Fatalf("%d objects, want the 2 stored", n)
	}

	// All good: 201 without failures.
	body, ct = multipartBody(t, []formFile{{"file", "a.png", pngData}}, nil)
	if rec := post(t, h, body, ct); rec.Code != http.StatusCreated || partialResponse(t, rec).Failed != nil {
		t.Fatalf("all stored = %d %s", rec.Code, rec.Body)
	}

	// All bad: the first file's status, with every failure listed.
	body, ct = multipartBody(t, []formFile{{"file", "x.html", htmlData}, {"file", "y.html", htmlData}}, nil)
	rec = post(t, h, body, ct)
	if got := partialResponse(t, rec); rec.Code != http.StatusUnsupportedMediaType || len(got.Files) != 0 || len(got.Failed) != 2 {
		t.Fatalf("all refused = %d %+v", rec.Code, got)
	}

	// Failed files count towards MaxFiles; the request then fails whole.
	before := count(t, disk)
	body, ct = multipartBody(t, []formFile{{"file", "1.html", htmlData}, {"file", "2.png", pngData}, {"file", "3.html", htmlData}, {"file", "4.png", pngData}, {"file", "5.png", pngData}}, nil)
	if rec := post(t, h, body, ct); rec.Code != http.StatusBadRequest || count(t, disk) != before {
		t.Fatalf("over MaxFiles = %d, %d objects (was %d)", rec.Code, count(t, disk), before)
	}

	// The request cap is a request failure even with Partial.
	capped := upload.Handler(disk, small, upload.HandlerOptions{MaxFiles: 1, Partial: true})
	body, ct = multipartBody(t, []formFile{{"file", "a.png", pngData}}, map[string]string{"padding": strings.Repeat("p", 80<<10)})
	if rec := post(t, capped, body, ct); rec.Code != http.StatusRequestEntityTooLarge || count(t, disk) != before {
		t.Fatalf("over the request cap = %d", rec.Code)
	}

	// A failing OnStored deletes what Partial stored.
	failing := upload.Handler(disk, small, upload.HandlerOptions{MaxFiles: 2, Partial: true, OnStored: func(context.Context, *http.Request, []upload.Result) error {
		return errors.New("db down")
	}})
	body, ct = multipartBody(t, []formFile{{"file", "a.png", pngData}, {"file", "x.html", htmlData}}, nil)
	if rec := post(t, failing, body, ct); rec.Code != http.StatusInternalServerError || count(t, disk) != before {
		t.Fatalf("failing OnStored = %d, %d objects", rec.Code, count(t, disk))
	}
}
