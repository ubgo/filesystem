package upload_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

type uppyClient struct {
	t *testing.T
	h http.Handler
}

func (c uppyClient) call(method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, httptest.NewRequestWithContext(c.t.Context(), method, path, strings.NewReader(body)))
	return rec
}

func TestUppyFlow(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, presigning{memory.New()})
	c := uppyClient{t, upload.Uppy(d, images, upload.UppyOptions{})}

	rec := c.call(http.MethodPost, "/multipart", `{"filename":"big.png","type":"image/png"}`)
	var created struct{ UploadID, Key string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.UploadID == "" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	q := "?key=" + created.Key
	rec = c.call(http.MethodGet, "/multipart/"+created.UploadID+"/1"+q, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"url":"https://bucket.test/`) {
		t.Errorf("sign part = %d %s", rec.Code, rec.Body)
	}
	// The browser uploads the part to the signed URL; here it goes straight
	// to the disk.
	part, err := d.UploadPart(ctx, created.Key, created.UploadID, 1, reader(pngData), int64(len(pngData)))
	if err != nil {
		t.Fatal(err)
	}
	rec = c.call(http.MethodGet, "/multipart/"+created.UploadID+q, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"PartNumber":1`) {
		t.Errorf("list parts = %d %s", rec.Code, rec.Body)
	}
	body := fmt.Sprintf(`{"parts":[{"PartNumber":1,"ETag":"\"%s\""}]}`, part.ETag)
	rec = c.call(http.MethodPost, "/multipart/"+created.UploadID+"/complete"+q, body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"location":"https://cdn.test/`+created.Key) {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body)
	}
	if got, _ := d.ReadAll(ctx, created.Key); string(got) != pngData {
		t.Error("assembled object differs")
	}
}

func TestUppyRefusals(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, presigning{memory.New()})
	deny := errors.New("not yours")
	c := uppyClient{t, upload.Uppy(d, images, upload.UppyOptions{Authorize: func(_ *http.Request, key string) error {
		if strings.HasPrefix(key, "other/") {
			return deny
		}
		return nil
	}})}
	status := func(method, path, body string) int { return c.call(method, path, body).Code }
	if s := status(http.MethodPost, "/multipart", `{"filename":"x.html","type":"text/html"}`); s != 415 {
		t.Errorf("create with a refused type = %d", s)
	}
	if s := status(http.MethodPost, "/multipart", `nope`); s != 400 {
		t.Errorf("bad create body = %d", s)
	}
	if s := status(http.MethodGet, "/multipart/1/1", ""); s != 400 {
		t.Errorf("missing key = %d", s)
	}
	if s := status(http.MethodGet, "/multipart/1/1?key=other/x", ""); s != 403 {
		t.Errorf("unauthorized = %d", s)
	}
	if s := status(http.MethodGet, "/multipart/1/abc?key=k", ""); s != 400 {
		t.Errorf("bad part number = %d", s)
	}
	if s := status(http.MethodGet, "/multipart/1/0?key=k", ""); s != 400 {
		t.Errorf("part 0 = %d", s)
	}
	if s := status(http.MethodGet, "/multipart/999?key=k", ""); s != 404 {
		t.Errorf("list unknown upload = %d", s)
	}
	if s := status(http.MethodPost, "/multipart/999/complete?key=k", `{"parts":[{"PartNumber":1,"ETag":"x"}]}`); s != 404 {
		t.Errorf("complete unknown upload = %d", s)
	}
	if s := status(http.MethodPost, "/multipart/999/complete?key=k", `{`); s != 400 {
		t.Errorf("bad complete body = %d", s)
	}

	// Declared image/png but the assembled bytes are HTML: refused and removed.
	rec := c.call(http.MethodPost, "/multipart", `{"filename":"x.png","type":"image/png"}`)
	var created struct{ UploadID, Key string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	part, _ := d.UploadPart(ctx, created.Key, created.UploadID, 1, reader(htmlData), int64(len(htmlData)))
	rec = c.call(http.MethodPost, "/multipart/"+created.UploadID+"/complete?key="+created.Key, fmt.Sprintf(`{"parts":[{"PartNumber":1,"ETag":"%s"}]}`, part.ETag))
	if rec.Code != 415 {
		t.Errorf("disguised upload = %d %s", rec.Code, rec.Body)
	}
	if ok, _ := d.Exists(ctx, created.Key); ok {
		t.Error("disguised upload kept")
	}

	// Abort.
	rec = c.call(http.MethodPost, "/multipart", `{"filename":"y.png","type":"image/png"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if s := status(http.MethodDelete, "/multipart/"+created.UploadID+"?key="+created.Key, ""); s != 204 {
		t.Errorf("abort = %d", s)
	}
}

func TestUppyCapabilitiesAndPolicy(t *testing.T) {
	plain := uppyClient{t, upload.Uppy(newDisk(t, memory.New()), images, upload.UppyOptions{})}
	if rec := plain.call(http.MethodPost, "/multipart", `{}`); rec.Code != 501 {
		t.Errorf("disk without part signing = %d", rec.Code)
	}
	bad := uppyClient{t, upload.Uppy(newDisk(t, presigning{memory.New()}), upload.Policy{}, upload.UppyOptions{})}
	if rec := bad.call(http.MethodPost, "/multipart", `{}`); rec.Code != 500 {
		t.Errorf("invalid policy = %d", rec.Code)
	}
	keyFail := upload.Policy{MaxSize: 10, Allowed: []string{"*/*"}, Key: func(upload.Info) (string, error) { return "..", nil }}
	kc := uppyClient{t, upload.Uppy(newDisk(t, presigning{memory.New()}), keyFail, upload.UppyOptions{})}
	if rec := kc.call(http.MethodPost, "/multipart", `{"type":"image/png"}`); rec.Code != 400 {
		t.Errorf("bad key = %d", rec.Code)
	}
	ro, _ := filesystem.NewDisk("ro", presigning{memory.New()}, filesystem.WithInterceptors(filesystem.ReadOnly()))
	rc := uppyClient{t, upload.Uppy(ro, images, upload.UppyOptions{})}
	if rec := rc.call(http.MethodPost, "/multipart", `{"type":"image/png"}`); rec.Code != 403 {
		t.Errorf("create on read-only disk = %d", rec.Code)
	}
	if rec := rc.call(http.MethodDelete, "/multipart/1?key=k", ""); rec.Code != 403 {
		t.Errorf("abort on read-only disk = %d", rec.Code)
	}
}

// With a Settle step the completed upload answers with the key the file
// was settled to, not the key the client uploaded to.
func TestUppyCompleteAnswersTheSettledKey(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, presigning{memory.New()})
	p := images
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
	c := uppyClient{t, upload.Uppy(d, p, upload.UppyOptions{})}
	rec := c.call(http.MethodPost, "/multipart", `{"filename":"big.png","type":"image/png"}`)
	var created struct{ UploadID, Key string }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	part, err := d.UploadPart(ctx, created.Key, created.UploadID, 1, reader(pngData), int64(len(pngData)))
	if err != nil {
		t.Fatal(err)
	}
	rec = c.call(http.MethodPost, "/multipart/"+created.UploadID+"/complete?key="+created.Key, fmt.Sprintf(`{"parts":[{"PartNumber":1,"ETag":"\"%s\""}]}`, part.ETag))
	var done struct{ Key, Location string }
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil || !strings.HasPrefix(done.Key, "blobs/") || strings.Contains(done.Key, ".incoming") || !strings.HasSuffix(done.Location, done.Key) {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body)
	}
	if ok, _ := d.Exists(ctx, done.Key); !ok {
		t.Error("the answered key holds nothing")
	}
}
