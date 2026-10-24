package upload_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

var anyFile = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}}

func TestMinSize(t *testing.T) {
	ctx := t.Context()
	for _, bad := range []upload.Policy{{MaxSize: 10, MinSize: -1, Allowed: []string{"*/*"}}, {MaxSize: 10, MinSize: 11, Allowed: []string{"*/*"}}} {
		wantCode(t, bad.Validate(), filesystem.CodeInvalidArgument)
	}
	p := anyFile
	p.MinSize = 1
	d := newDisk(t, presigning{memory.New()})
	_, err := upload.Receive(ctx, d, strings.NewReader(""), "empty.txt", p)
	wantCode(t, err, filesystem.CodeRejected)
	if !strings.Contains(err.Error(), "0 bytes, under the minimum of 1") || count(t, d) != 0 {
		t.Errorf("empty upload: %v, %d stored", err, count(t, d))
	}
	if _, err := upload.Receive(ctx, d, strings.NewReader("x"), "one.txt", p); err != nil {
		t.Errorf("one byte: %v", err)
	}
	p.MinSize = 100
	_, err = upload.Presign(ctx, d, upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 99}, p, time.Minute)
	wantCode(t, err, filesystem.CodeRejected)
	_, _ = d.WriteBytes(ctx, "small.png", []byte(pngData), filesystem.WriteOptions{})
	_, err = upload.Confirm(ctx, d, "small.png", "small.png", p)
	wantCode(t, err, filesystem.CodeRejected)
	if ok, _ := d.Exists(ctx, "small.png"); ok {
		t.Error("undersized direct upload kept")
	}
}

func TestConfirmExpected(t *testing.T) {
	ctx := t.Context()
	sum := sha256.Sum256([]byte(pngData))
	digest := hex.EncodeToString(sum[:])
	for _, c := range []struct {
		name string
		want upload.Expected
		code filesystem.Code
	}{
		{"matching size and digest", upload.Expected{Size: int64(len(pngData)), SHA256: strings.ToUpper(digest)}, ""},
		{"nothing declared", upload.Expected{}, ""},
		{"truncated", upload.Expected{Size: int64(len(pngData)) + 1}, filesystem.CodeRejected},
		{"other bytes", upload.Expected{SHA256: strings.Repeat("0", 64)}, filesystem.CodeRejected},
		{"malformed digest", upload.Expected{SHA256: "abc"}, filesystem.CodeInvalidArgument},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Twice: once where the backend reports the hash, once where
			// the server computes it.
			for _, drv := range []filesystem.Driver{memory.New(), unhashed{memory.New()}} {
				d := newDisk(t, drv)
				_, _ = d.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{})
				_, err := upload.ConfirmExpected(ctx, d, "a.png", "a.png", images, c.want)
				if filesystem.CodeOf(err) != c.code {
					t.Errorf("%T: %v, want %q", drv, err, c.code)
				}
				kept, _ := d.Exists(ctx, "a.png")
				if kept != (c.code != filesystem.CodeRejected) {
					t.Errorf("%T: kept=%v", drv, kept)
				}
			}
		})
	}
	_, err := upload.Presign(ctx, newDisk(t, presigning{memory.New()}), upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10, SHA256: "xyz"}, images, time.Minute)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if _, err := upload.Presign(ctx, newDisk(t, presigning{memory.New()}), upload.TicketRequest{Filename: "a.png", ContentType: "image/png", Size: 10, SHA256: digest}, images, time.Minute); err != nil {
		t.Errorf("valid digest: %v", err)
	}
}

// unhashed reports no SHA-256 from Stat, as most object stores do.
type unhashed struct{ *memory.Driver }

func (u unhashed) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := u.Driver.Stat(ctx, key)
	e.SHA256 = ""
	return e, err
}

// trackedBody records whether anything read it.
type trackedBody struct {
	io.Reader
	read bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	b.read = true
	return b.Reader.Read(p)
}

func (*trackedBody) Close() error { return nil }

func TestHandlerResolve(t *testing.T) {
	d := newDisk(t, memory.New())
	plans := func(r *http.Request, base upload.Policy) (upload.Policy, error) {
		switch r.Header.Get("X-Plan") {
		case "free":
			base.MaxSize = 8
		case "throttled":
			return upload.Policy{}, filesystem.NewError(filesystem.CodeUnavailable, errors.New("too many uploads")).WithRetryAfter(30 * time.Second)
		case "broken":
			return upload.Policy{MaxSize: 1}, nil // allows no type: invalid
		}
		return base, nil
	}
	h := upload.Handler(d, anyFile, upload.HandlerOptions{Resolve: plans})
	send := func(plan, data string) (*httptest.ResponseRecorder, *trackedBody) {
		body, ct := multipartBody(t, []formFile{{"file", "a.bin", data}}, nil)
		tracked := &trackedBody{Reader: body}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/upload", tracked)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Plan", plan)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, tracked
	}
	if rec, _ := send("paid", "0123456789"); rec.Code != http.StatusCreated {
		t.Errorf("paid = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := send("free", "0123456789"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("free = %d %s", rec.Code, rec.Body)
	}
	rec, tracked := send("throttled", "0123456789")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" || errorBody(t, rec).Error != http.StatusText(http.StatusServiceUnavailable) || tracked.read {
		t.Errorf("throttled = %d %v %s, body read %v", rec.Code, rec.Header(), rec.Body, tracked.read)
	}
	// A resolved policy that fails Validate is the server's fault: 500 with
	// the status text only, nothing about the configuration.
	if rec, _ := send("broken", "x"); rec.Code != http.StatusInternalServerError || errorBody(t, rec) != (upload.ErrorResponse{Error: http.StatusText(http.StatusInternalServerError), Code: filesystem.CodeUnknown}) {
		t.Errorf("invalid resolved policy = %d %s", rec.Code, rec.Body)
	}
}

func TestHandlerMaxTotal(t *testing.T) {
	d := newDisk(t, memory.New())
	post3 := func(opts upload.HandlerOptions, p upload.Policy, sizes ...int) *httptest.ResponseRecorder {
		var files []formFile
		for i, n := range sizes {
			files = append(files, formFile{"file", string(rune('a'+i)) + ".bin", strings.Repeat("x", n)})
		}
		body, ct := multipartBody(t, files, nil)
		return post(t, upload.Handler(d, p, opts), body, ct)
	}
	all := upload.HandlerOptions{MaxFiles: 3, MaxTotal: 100}
	rec := post3(all, anyFile, 60, 60)
	if e := errorBody(t, rec); rec.Code != http.StatusRequestEntityTooLarge || e.Error != "the files of this request exceed 100 bytes in total" || count(t, d) != 0 {
		t.Errorf("over total = %d %s, %d stored", rec.Code, rec.Body, count(t, d))
	}
	partial := all
	partial.Partial = true
	rec = post3(partial, anyFile, 60, 60, 40)
	var resp upload.Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || len(resp.Files) != 2 || len(resp.Failed) != 1 || resp.Failed[0].Code != filesystem.CodeTooLarge {
		t.Errorf("partial = %d %s", rec.Code, rec.Body)
	}
	// Nothing left of the budget, or less than MinSize, refuses at once.
	if rec := post3(all, anyFile, 100, 1); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("exhausted budget = %d %s", rec.Code, rec.Body)
	}
	minimum := anyFile
	minimum.MinSize = 10
	if rec := post3(all, minimum, 95, 10); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("budget under MinSize = %d %s", rec.Code, rec.Body)
	}
	if rec := post3(upload.HandlerOptions{MaxTotal: -1}, anyFile, 1); rec.Code != http.StatusInternalServerError {
		t.Errorf("negative MaxTotal = %d", rec.Code)
	}
	// The request cap still answers as the request's failure.
	// The junk field comes first; the cap (MaxTotal plus 64 KiB of form
	// overhead) then trips half-way through b, while b is still within
	// what the total leaves it.
	wide := upload.HandlerOptions{MaxFiles: 3, MaxTotal: 60_000}
	junk, ct := multipartBody(t, []formFile{{"file", "a.bin", strings.Repeat("x", 30_000)}, {"file", "b.bin", strings.Repeat("y", 29_000)}}, map[string]string{"junk": strings.Repeat("j", 80_000)})
	rec = post(t, upload.Handler(d, anyFile, wide), junk, ct)
	if e := errorBody(t, rec); rec.Code != http.StatusRequestEntityTooLarge || e.Error != "http: request body too large" {
		t.Errorf("request cap = %d %s", rec.Code, rec.Body)
	}
}

func TestUppyAndPresignHandlerResolve(t *testing.T) {
	d := newDisk(t, presigning{memory.New()})
	refuse := func(r *http.Request, base upload.Policy) (upload.Policy, error) {
		if r.Header.Get("X-User") == "" {
			return upload.Policy{}, filesystem.Errorf(filesystem.CodePermission, "sign in first")
		}
		base.Key = upload.RandomKey("users/" + r.Header.Get("X-User"))
		return base, nil
	}
	uppyH := upload.Uppy(d, images, upload.UppyOptions{Resolve: refuse})
	presignH := upload.PresignHandlerWith(d, images, time.Minute, upload.PresignHandlerOptions{Resolve: refuse})
	for name, c := range map[string]struct {
		h          http.Handler
		path, body string
	}{
		"uppy":    {uppyH, "/multipart", `{"filename":"a.png","type":"image/png"}`},
		"presign": {presignH, "/", `{"filename":"a.png","content_type":"image/png","size":10}`},
	} {
		for _, user := range []string{"", "u1"} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, c.path, strings.NewReader(c.body))
			if user != "" {
				req.Header.Set("X-User", user)
			}
			rec := httptest.NewRecorder()
			c.h.ServeHTTP(rec, req)
			switch {
			case user == "" && rec.Code != http.StatusForbidden:
				t.Errorf("%s anonymous = %d %s", name, rec.Code, rec.Body)
			case user != "" && (rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"key":"users/u1/`)):
				t.Errorf("%s as u1 = %d %s", name, rec.Code, rec.Body)
			}
		}
	}
}

func TestMisconfiguredResolverIsAServerError(t *testing.T) {
	d := newDisk(t, presigning{memory.New()})
	broken := func(*http.Request, upload.Policy) (upload.Policy, error) {
		return upload.Policy{MaxSize: 10}, nil // allows no type: invalid
	}
	want := upload.ErrorResponse{Error: http.StatusText(http.StatusInternalServerError), Code: filesystem.CodeUnknown}
	for name, c := range map[string]struct {
		h          http.Handler
		path, body string
	}{
		"uppy":    {upload.Uppy(d, images, upload.UppyOptions{Resolve: broken}), "/multipart", `{"filename":"a.png","type":"image/png"}`},
		"presign": {upload.PresignHandlerWith(d, images, time.Minute, upload.PresignHandlerOptions{Resolve: broken}), "/", `{"filename":"a.png","content_type":"image/png","size":10}`},
	} {
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, c.path, strings.NewReader(c.body)))
		if rec.Code != http.StatusInternalServerError || errorBody(t, rec) != want || strings.Contains(rec.Body.String(), "policy") {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body)
		}
	}
}
