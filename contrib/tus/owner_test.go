package tus_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	fstus "github.com/ubgo/filesystem/contrib/tus"
)

const metaOwner = "owner"

// TestAuthorizeUpload pins the per-upload seam: StampMetadata writes the
// owner over what the client claims, AuthorizeUpload sees the stored
// owner on PATCH and HEAD and lets the owner continue while another user
// is refused 403, and a stamp error refuses the creation.
func TestAuthorizeUpload(t *testing.T) {
	var seen []fstus.UploadRequest
	cfg := fstus.Config{
		StampMetadata: func(_ context.Context, h http.Header) (map[string]string, error) {
			if h.Get("X-User") == "" {
				return nil, errors.New("signed out")
			}
			return map[string]string{metaOwner: h.Get("X-User")}, nil
		},
		AuthorizeUpload: func(_ context.Context, r fstus.UploadRequest) error {
			seen = append(seen, r)
			if r.UploadID != "" && r.Metadata[metaOwner] != r.Header.Get("X-User") {
				return errors.New("not yours")
			}
			return nil
		},
	}
	e := setup(t, cfg)
	alice := map[string]string{"X-User": "alice"}
	loc, resp := e.create(t, len(pngData), map[string]string{fstus.MetaFiletype: "image/png", metaOwner: "mallory"}, alice)
	if resp.StatusCode != http.StatusCreated || len(seen) != 1 || seen[0].UploadID != "" || seen[0].Metadata[metaOwner] != "mallory" {
		t.Fatalf("create = %d, seen %+v", resp.StatusCode, seen)
	}
	id := loc[strings.LastIndexByte(loc, '/')+1:]
	bob := map[string]string{"X-User": "bob", "Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}
	if r := e.do(t, http.MethodPatch, loc, strings.NewReader(pngData), bob); r.StatusCode != http.StatusForbidden {
		t.Fatalf("another user's PATCH = %d", r.StatusCode)
	}
	if r := e.do(t, http.MethodHead, loc, nil, map[string]string{"X-User": "mallory"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("the claimed owner's HEAD = %d: the client's metadata won over the stamp", r.StatusCode)
	}
	last := seen[len(seen)-1]
	if last.UploadID != id || last.Metadata[metaOwner] != "alice" || last.Method != http.MethodHead {
		t.Fatalf("AuthorizeUpload saw %+v", last)
	}
	e.always = alice
	if final := e.send(t, loc, pngData); final.StatusCode != http.StatusNoContent {
		t.Fatalf("the owner's upload = %d", final.StatusCode)
	}
	if _, resp := e.create(t, len(pngData), map[string]string{fstus.MetaFiletype: "image/png"}, map[string]string{"X-User": ""}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a refused stamp = %d", resp.StatusCode)
	}
	e.always = nil
	if r := e.do(t, http.MethodHead, e.srv.URL+"/uploads/unknownid", nil, alice); r.StatusCode != http.StatusForbidden {
		t.Fatalf("an unknown upload = %d", r.StatusCode)
	}
}

// TestUploadsWithoutMetadata pins that an upload created with no
// metadata at all still gets the stamp, and that AuthorizeUpload is
// handed an empty map, never nil, for an upload that stored none.
func TestUploadsWithoutMetadata(t *testing.T) {
	stamped := setup(t, fstus.Config{
		StampMetadata: func(context.Context, http.Header) (map[string]string, error) {
			return map[string]string{metaOwner: "alice"}, nil
		},
	})
	bare := map[string]string{"Upload-Length": strconv.Itoa(len(pngData))} // no Upload-Metadata header at all
	if resp := stamped.do(t, http.MethodPost, stamped.srv.URL+"/uploads/", nil, bare); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create without metadata = %d", resp.StatusCode)
	}

	var seen []fstus.UploadRequest
	plain := setup(t, fstus.Config{
		AuthorizeUpload: func(_ context.Context, r fstus.UploadRequest) error {
			seen = append(seen, r)
			return nil
		},
	})
	resp := plain.do(t, http.MethodPost, plain.srv.URL+"/uploads/", nil, bare)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	if r := plain.do(t, http.MethodHead, loc, nil, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("HEAD = %d", r.StatusCode)
	}
	if last := seen[len(seen)-1]; last.Metadata == nil || len(last.Metadata) != 0 {
		t.Fatalf("AuthorizeUpload saw metadata %#v", last.Metadata)
	}
}
