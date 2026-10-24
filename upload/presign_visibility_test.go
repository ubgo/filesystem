package upload_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// aclPresigning is a presigning driver whose PUTs and forms pin a
// visibility, as S3 and GCS do in ACL mode; it records what it pinned.
type aclPresigning struct {
	presigning
	put  *filesystem.Visibility
	post *filesystem.Visibility
}

func (a aclPresigning) Capabilities() filesystem.Capabilities {
	c := a.presigning.Capabilities()
	c.PresignPost, c.PresignVisibility = true, true
	return c
}

func (a aclPresigning) Presign(ctx context.Context, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	*a.put = o.Visibility
	return a.presigning.Presign(ctx, key, o)
}

func (a aclPresigning) PresignPost(_ context.Context, p filesystem.PostPolicy) (filesystem.PresignedPost, error) {
	*a.post = p.Visibility
	return filesystem.PresignedPost{URL: "https://bucket.test/", Fields: map[string]string{filesystem.PostFieldKey: p.Key}}, nil
}

func TestPresignVisibility(t *testing.T) {
	ctx := t.Context()
	var put, post filesystem.Visibility
	d := newDisk(t, aclPresigning{presigning: presigning{memory.New()}, put: &put, post: &post})
	png := func(v filesystem.Visibility) upload.TicketRequest {
		return upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 10, Visibility: v}
	}
	allow := images
	allow.AllowPublic = true
	forms := allow
	forms.PostForm = true

	for _, c := range []struct {
		name string
		p    upload.Policy
		ask  filesystem.Visibility
		code filesystem.Code
	}{
		{"default unchanged: nothing asked, nothing signed", images, "", ""},
		{"private needs no allow", images, filesystem.VisibilityPrivate, ""},
		{"public refused without AllowPublic", images, filesystem.VisibilityPublic, filesystem.CodePermission},
		{"public allowed and signed", allow, filesystem.VisibilityPublic, ""},
		{"public allowed and pinned in a form", forms, filesystem.VisibilityPublic, ""},
		{"unknown value", allow, "world", filesystem.CodeInvalidArgument},
	} {
		t.Run(c.name, func(t *testing.T) {
			put, post = "unset", "unset"
			_, err := upload.Presign(ctx, d, png(c.ask), c.p, time.Minute)
			wantCode(t, err, c.code)
			got := put
			if c.p.PostForm {
				got = post
			}
			switch {
			case c.code != "" && (put != "unset" || post != "unset"):
				t.Fatal("a refused ticket reached the driver")
			case c.code == "" && got != c.ask:
				t.Fatalf("the driver pinned %q, want %q", got, c.ask)
			}
		})
	}

	// A disk that cannot pin a visibility refuses rather than issue a
	// ticket that would store the object some other way.
	_, err := upload.Presign(ctx, newDisk(t, presigning{memory.New()}), png(filesystem.VisibilityPublic), allow, time.Minute)
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestPresignHandlerVisibility(t *testing.T) {
	var put, post filesystem.Visibility
	d := newDisk(t, aclPresigning{presigning: presigning{memory.New()}, put: &put, post: &post})
	ask := func(h http.Handler) int {
		body, _ := json.Marshal(upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 10, Visibility: filesystem.VisibilityPublic})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
		return rec.Code
	}
	if code := ask(upload.PresignHandler(d, images, time.Minute)); code != http.StatusForbidden {
		t.Errorf("public ticket without AllowPublic: %d, want 403", code)
	}
	allow := images
	allow.AllowPublic = true
	if code := ask(upload.PresignHandler(d, allow, time.Minute)); code != http.StatusOK || put != filesystem.VisibilityPublic {
		t.Errorf("public ticket with AllowPublic: %d, pinned %q", code, put)
	}
}
