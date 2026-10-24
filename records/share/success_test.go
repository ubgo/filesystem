package share_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/share"
)

// dataEnvelope is an application's success shape, {"kind": …, "data": …}.
type dataEnvelope struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func envelopeWriter(w http.ResponseWriter, _ *http.Request, status int, body share.SuccessBody) {
	kind := "unknown"
	switch body.(type) {
	case share.ListResponse:
		kind = "list"
	case share.DepositResponse:
		kind = "deposit"
	}
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/vnd.app+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(dataEnvelope{Kind: kind, Data: data})
}

// successSetup is a folder holding one file, a collection link to it and
// a request link into it, and a multipart upload body.
func successSetup(t *testing.T) (*suite, share.Share, share.Share, string) {
	t.Helper()
	ctx := t.Context()
	u := newSuite(t, 0)
	dir, err := u.folders.Create(ctx, u.store, "t", folders.RootID, "Reports")
	must(t, err)
	in := u.file(t, "in.txt")
	must(t, u.folders.Put(ctx, u.store, "t", in.ID, dir.ID))
	coll, err := u.plugin.CreateFor(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	must(t, err)
	req, err := u.plugin.CreateRequest(ctx, u.store, "t", share.Target{Kind: folders.KindFolder, Ref: dir.ID}, share.Options{TTL: time.Hour})
	must(t, err)
	return u, coll, req, in.ID
}

func depositBody() (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile(share.FormFile, "doc.txt")
	_, _ = fw.Write([]byte("hello"))
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// TestSuccessWriter (B5): WithRespond answers a collection's listing and
// a request link's upload in the application's envelope, with the
// handler's status and the body's type.
func TestSuccessWriter(t *testing.T) {
	u, coll, req, inID := successSetup(t)
	h := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText), share.WithRespond(envelopeWriter))
	rec := serve(h, http.MethodGet, "/"+coll.Token, nil, nil)
	var env dataEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != http.StatusOK || env.Kind != "list" {
		t.Fatalf("listing = %d %s", rec.Code, rec.Body)
	}
	var listed share.ListResponse
	if err := json.Unmarshal(env.Data, &listed); err != nil || len(listed) != 1 || listed[0].ID != inID {
		t.Fatalf("listing data = %s", env.Data)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the listing lost Cache-Control: no-store")
	}
	body, ct := depositBody()
	rec = serve(h, http.MethodPost, "/"+req.Token, body, http.Header{"Content-Type": {ct}})
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != http.StatusCreated || env.Kind != "deposit" {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
}

// TestDefaultSuccessUnchanged: without WithRespond the listing is a bare
// JSON array and an upload {"id": …}, byte for byte as before.
func TestDefaultSuccessUnchanged(t *testing.T) {
	u, coll, req, inID := successSetup(t)
	h := share.Handler(u.store, u.plugin, share.WithUploads("media", anyText))
	rec := serve(h, http.MethodGet, "/"+coll.Token, nil, nil)
	want := fmt.Sprintf(`[{"id":%q,"filename":"in.txt","content_type":"text/plain; charset=utf-8","size":%d}]`+"\n", inID, len("in.txt"))
	if rec.Code != http.StatusOK || rec.Body.String() != want || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("listing = %d %q, want %q", rec.Code, rec.Body.String(), want)
	}
	body, ct := depositBody()
	rec = serve(h, http.MethodPost, "/"+req.Token, body, http.Header{"Content-Type": {ct}})
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusCreated || len(got) != 1 || got["id"] == "" {
		t.Fatalf("upload = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != fmt.Sprintf(`{"id":%q}`+"\n", got["id"]) {
		t.Fatalf("upload body = %q", rec.Body.String())
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
