package upload_test

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// TestSignVisibility pins R-I14: with Policy.SignVisibility the server's
// Visibility is signed into every ticket (PUT and form) without the client
// asking; a client asking for another one is refused before signing;
// public still needs AllowPublic; and without the switch a policy's
// Visibility signs nothing, as before.
func TestSignVisibility(t *testing.T) {
	ctx := t.Context()
	var put, post filesystem.Visibility
	d := newDisk(t, aclPresigning{presigning: presigning{memory.New()}, put: &put, post: &post})
	png := func(v filesystem.Visibility) upload.TicketRequest {
		return upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 10, Visibility: v}
	}
	public := images
	public.Visibility, public.AllowPublic, public.SignVisibility = filesystem.VisibilityPublic, true, true
	publicForm := public
	publicForm.PostForm = true
	private := images
	private.Visibility, private.SignVisibility = filesystem.VisibilityPrivate, true
	legacy := images
	legacy.Visibility, legacy.AllowPublic = filesystem.VisibilityPublic, true

	for _, c := range []struct {
		name string
		p    upload.Policy
		ask  filesystem.Visibility
		want filesystem.Visibility
		code filesystem.Code
	}{
		{"server signs public unasked", public, "", filesystem.VisibilityPublic, ""},
		{"server signs public in a form", publicForm, "", filesystem.VisibilityPublic, ""},
		{"server signs private unasked", private, "", filesystem.VisibilityPrivate, ""},
		{"matching request passes", public, filesystem.VisibilityPublic, filesystem.VisibilityPublic, ""},
		{"client cannot downgrade", public, filesystem.VisibilityPrivate, "", filesystem.CodePermission},
		{"client cannot publish", private, filesystem.VisibilityPublic, "", filesystem.CodePermission},
		{"default: policy visibility signs nothing", legacy, "", "", ""},
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
			case c.code == "" && got != c.want:
				t.Fatalf("the driver pinned %q, want %q", got, c.want)
			}
		})
	}

	t.Run("validate", func(t *testing.T) {
		noValue := images
		noValue.SignVisibility = true
		wantCode(t, noValue.Validate(), filesystem.CodeInvalidArgument)
		noAllow := public
		noAllow.AllowPublic = false
		wantCode(t, noAllow.Validate(), filesystem.CodeInvalidArgument)
		put = "unset"
		_, err := upload.Presign(ctx, d, png(""), noAllow, time.Minute)
		wantCode(t, err, filesystem.CodeInvalidArgument)
		if put != "unset" {
			t.Fatal("a public ticket without AllowPublic reached the driver")
		}
	})
}
