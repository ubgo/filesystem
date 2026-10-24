package upload_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func TestPresignAndConfirm(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, presigning{memory.New()})
	tk, err := upload.Presign(ctx, d, upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 100}, images, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Method != "PUT" || !strings.HasPrefix(tk.URL, "https://bucket.test/") || tk.Headers["Content-Type"][0] != "image/png" || !strings.HasSuffix(tk.Key, ".png") {
		t.Errorf("ticket = %+v", tk)
	}
	// The client uploads; here the bytes are written directly.
	_, _ = d.WriteBytes(ctx, tk.Key, []byte(pngData), filesystem.WriteOptions{})
	res, err := upload.Confirm(ctx, d, tk.Key, "me.png", images)
	if err != nil || res.ContentType != "image/png" || res.Size != int64(len(pngData)) {
		t.Fatalf("Confirm = %+v, %v", res, err)
	}

	for _, req := range []upload.TicketRequest{
		{ContentType: "image/png", Size: 0},
		{ContentType: "image/png", Size: images.MaxSize + 1},
	} {
		_, err := upload.Presign(ctx, d, req, images, time.Minute)
		wantCode(t, err, filesystem.CodeTooLarge)
	}
	_, err = upload.Presign(ctx, d, upload.TicketRequest{ContentType: "text/html", Size: 1}, images, time.Minute)
	wantCode(t, err, filesystem.CodeRejected)
	_, err = upload.Presign(ctx, d, upload.TicketRequest{ContentType: "image/png", Size: 1}, upload.Policy{}, time.Minute)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	badKey := upload.Policy{MaxSize: 10, Allowed: []string{"*/*"}, Key: func(upload.Info) (string, error) { return "", nil }}
	_, err = upload.Presign(ctx, d, upload.TicketRequest{ContentType: "image/png", Size: 1}, badKey, time.Minute)
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = upload.Presign(ctx, newDisk(t, memory.New()), upload.TicketRequest{ContentType: "image/png", Size: 1}, images, time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestConfirmRemovesRefusedObjects(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	_, _ = d.WriteBytes(ctx, "big.png", []byte(pngData), filesystem.WriteOptions{})
	_, err := upload.Confirm(ctx, d, "big.png", "", upload.Policy{MaxSize: 4, Allowed: []string{"image/*"}})
	wantCode(t, err, filesystem.CodeTooLarge)
	_, _ = d.WriteBytes(ctx, "fake.png", []byte(htmlData), filesystem.WriteOptions{})
	_, err = upload.Confirm(ctx, d, "fake.png", "fake.png", images)
	wantCode(t, err, filesystem.CodeRejected)
	if count(t, d) != 0 {
		t.Error("refused objects remain")
	}
	_, err = upload.Confirm(ctx, d, "missing", "", images)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = upload.Confirm(ctx, d, "missing", "", upload.Policy{})
	wantCode(t, err, filesystem.CodeInvalidArgument)

	stuck := newDisk(t, undeletable{memory.New()})
	_, _ = stuck.WriteBytes(ctx, "x", []byte(htmlData), filesystem.WriteOptions{})
	_, err = upload.Confirm(ctx, stuck, "x", "", images)
	if filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), "permission") {
		t.Errorf("failed cleanup not reported: %v", err)
	}
	for _, drv := range []filesystem.Driver{unreadable{memory.New()}, brokenStream{memory.New()}} {
		dd := newDisk(t, drv)
		_, _ = dd.WriteBytes(ctx, "x", []byte(pngData), filesystem.WriteOptions{})
		_, err = upload.Confirm(ctx, dd, "x", "", images)
		wantCode(t, err, filesystem.CodeUnavailable)
	}
}

func TestPresignHandler(t *testing.T) {
	d := newDisk(t, presigning{memory.New()})
	h := upload.PresignHandler(d, images, time.Minute)
	call := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/presign", strings.NewReader(body)))
		return rec
	}
	rec := call(http.MethodPost, `{"filename":"a.png","content_type":"image/png","size":10}`)
	var tk upload.Ticket
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &tk) != nil || tk.Key == "" {
		t.Fatalf("presign = %d %s", rec.Code, rec.Body)
	}
	for body, want := range map[string]int{
		`{"content_type":"image/png","size":10,"extra":1}`: 400,
		`not json`: 400,
		`{"filename":"a.html","content_type":"text/html","size":10}`: 415,
	} {
		if rec := call(http.MethodPost, body); rec.Code != want {
			t.Errorf("%s = %d, want %d", body, rec.Code, want)
		}
	}
	if rec := call(http.MethodGet, ""); rec.Code != 405 {
		t.Errorf("GET = %d", rec.Code)
	}
}

// A presigned PUT may store whatever Content-Type the client sent when the
// backend cannot pin it (Azure SAS); Confirm refuses a declared type that
// is not what the bytes are, so the object is never served as that type.
func TestConfirmRefusesMismatchedStoredType(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	_, _ = d.WriteBytes(ctx, "u/a.png", []byte(pngData), filesystem.WriteOptions{ContentType: "text/html"})
	_, err := upload.Confirm(ctx, d, "u/a.png", "a.png", upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}})
	wantCode(t, err, filesystem.CodeRejected)
	msg := err.Error()
	if !strings.Contains(msg, "text/html") || !strings.Contains(msg, "image/png") || strings.Contains(msg, "u/a.png") {
		t.Errorf("message %q", msg)
	}
	if count(t, d) != 0 {
		t.Error("mismatched object kept")
	}

	// Parameters and case do not matter.
	_, _ = d.WriteBytes(ctx, "u/b.txt", []byte("plain words"), filesystem.WriteOptions{ContentType: "Text/Plain; charset=UTF-8"})
	text := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}}
	if res, err := upload.Confirm(ctx, d, "u/b.txt", "b.txt", text); err != nil || res.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("matching type with charset: %+v, %v", res, err)
	}
	// A backend that stores no type has nothing to compare.
	plain := newDisk(t, untyped{memory.New()})
	_, _ = plain.WriteBytes(ctx, "c.png", []byte(pngData), filesystem.WriteOptions{})
	if _, err := upload.Confirm(ctx, plain, "c.png", "c.png", images); err != nil {
		t.Errorf("untyped backend: %v", err)
	}
}

// untyped is a memory driver that stores no content type.
type untyped struct{ *memory.Driver }

func (u untyped) Capabilities() filesystem.Capabilities {
	c := u.Driver.Capabilities()
	c.ContentType = false
	return c
}

func (u untyped) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := u.Driver.Stat(ctx, key)
	e.ContentType = ""
	return e, err
}

// A stored type mime cannot parse still compares by its type.
func TestConfirmMalformedStoredType(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	_, _ = d.WriteBytes(ctx, "a.png", []byte(pngData), filesystem.WriteOptions{ContentType: "IMAGE/PNG; broken"})
	if _, err := upload.Confirm(ctx, d, "a.png", "a.png", images); err != nil {
		t.Errorf("malformed matching type: %v", err)
	}
	_, _ = d.WriteBytes(ctx, "b.png", []byte(pngData), filesystem.WriteOptions{ContentType: "text/html; broken"})
	_, err := upload.Confirm(ctx, d, "b.png", "b.png", images)
	wantCode(t, err, filesystem.CodeRejected)
}

// Only a declared type that would make a browser render the bytes as
// something they are not is refused; aliases and generic labels pass, so
// good uploads are never deleted.
func TestConfirmStoredTypeAliases(t *testing.T) {
	ctx := t.Context()
	wav := "RIFF\x24\x00\x00\x00WAVEfmt " + strings.Repeat("\x00", 40)
	zip := "PK\x03\x04" + strings.Repeat("\x00", 40)
	anyType := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"*/*"}}
	for _, c := range []struct {
		name, declared, body string
		refused              bool
	}{
		{"a.wav", "audio/wav", wav, false},
		{"a.zip", "application/x-zip-compressed", zip, false},
		{"a.png", "application/octet-stream", pngData, false},
		{"b.png", "text/html", pngData, true},
		{"c.png", "image/svg+xml", pngData, true},
		{"d.png", "text/plain", pngData, true},
		{"e.png", "application/javascript", pngData, true},
		{"f.png", "application/rss+xml", pngData, true},
		{"g.txt", "text/html", "just words", true},
	} {
		d := newDisk(t, memory.New())
		_, _ = d.WriteBytes(ctx, c.name, []byte(c.body), filesystem.WriteOptions{ContentType: c.declared})
		_, err := upload.Confirm(ctx, d, c.name, c.name, anyType)
		if refused := filesystem.CodeOf(err) == filesystem.CodeRejected; refused != c.refused || (!c.refused && err != nil) {
			t.Errorf("%s declared %s: err %v, want refused=%v", c.name, c.declared, err, c.refused)
		}
		if kept := count(t, d) == 1; kept == c.refused {
			t.Errorf("%s: object kept=%v", c.name, kept)
		}
	}
}

// Local disk derives its type from the extension; it is not a declaration,
// so it is not compared.
func TestConfirmIgnoresDerivedTypes(t *testing.T) {
	ctx := t.Context()
	drv, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := newDisk(t, drv)
	t.Cleanup(func() { _ = d.Close() })
	_, _ = d.WriteBytes(ctx, "notes.txt", []byte(pngData), filesystem.WriteOptions{})
	if e, _ := d.Stat(ctx, "notes.txt"); !strings.HasPrefix(e.ContentType, "text/plain") {
		t.Fatalf("local derived %q", e.ContentType)
	}
	if _, err := upload.Confirm(ctx, d, "notes.txt", "notes.png", images); err != nil {
		t.Errorf("derived type compared: %v", err)
	}
}

// postingDisk presigns POST forms the way an S3-like backend would,
// without signing, and records the policy.
type postingDisk struct {
	*memory.Driver
	last *filesystem.PostPolicy
}

func (p postingDisk) Capabilities() filesystem.Capabilities {
	c := p.Driver.Capabilities()
	c.PresignPost = true
	return c
}

func (p postingDisk) PresignPost(_ context.Context, pol filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	*p.last = pol
	return filesystem.PresignedPost{URL: "https://bucket.test/", Fields: map[string]string{filesystem.PostFieldKey: pol.Key, filesystem.PostFieldContentType: pol.ContentType, "policy": "p"}, ExpiresAt: time.Unix(0, 0).Add(pol.Expires)}, nil
}

func TestPresignPostForm(t *testing.T) {
	ctx := t.Context()
	var last filesystem.PostPolicy
	d := newDisk(t, postingDisk{Driver: memory.New(), last: &last})
	p := images
	p.PostForm = true
	p.Headers = upload.StaticHeaders(filesystem.HTTPHeaders{CacheControl: "no-store"})
	tk, err := upload.Presign(ctx, d, upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 1234}, p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Method != http.MethodPost || tk.Headers != nil || tk.Fields[filesystem.PostFieldKey] != tk.Key || tk.Fields["policy"] != "p" {
		t.Errorf("ticket %+v", tk)
	}
	// The backend is told to accept exactly the declared size and type.
	if last.Key != tk.Key || last.MinSize != 1234 || last.MaxSize != 1234 || last.ContentType != "image/png" || last.Headers.CacheControl != "no-store" || last.Expires != time.Minute {
		t.Errorf("policy %+v", last)
	}
	b, _ := json.Marshal(tk)
	if !strings.Contains(string(b), `"fields":{`) {
		t.Errorf("JSON %s", b)
	}
	// A disk that cannot sign forms refuses.
	_, err = upload.Presign(ctx, newDisk(t, presigning{memory.New()}), upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 10}, p, time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
}
