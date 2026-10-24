package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem/filesystemtest"
)

// postAvatar sends file as user's avatar through the handler, the way a
// browser's form post would, and returns the response.
// ds:def id=testing-postavatar-2tdsmydn
func postAvatar(t *testing.T, a *Avatars, user string, file filesystemtest.UploadFile) *httptest.ResponseRecorder {
	t.Helper()
	req := filesystemtest.NewUploadRequest(t, "/avatar", nil, file)
	req.Header.Set(headerUser, user)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

// TestAvatarUploadOverHTTP builds the multipart request and a real image
// in code: no fixture files, no browser.
// ds:def id=testing-uploadoverhttp-e85bbpak
func TestAvatarUploadOverHTTP(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	img := filesystemtest.FakeImage(t, filesystemtest.PNG, 64, 64)
	rec := postAvatar(t, NewAvatars(disk.Disk), "u1", filesystemtest.UploadFile{Field: avatarField, Name: "me.png", Content: img})
	if rec.Code != http.StatusOK || rec.Body.String() != "avatars/u1\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	disk.AssertExists("avatars/u1")
}

// TestAvatarRefusalsOverHTTP checks each refusal's status, and that
// nothing is stored for any of them.
// ds:def id=testing-refusalsoverhttp-eauurr9s
func TestAvatarRefusalsOverHTTP(t *testing.T) {
	disk := filesystemtest.NewFake(t, mediaDisk)
	a := NewAvatars(disk.Disk)
	tooBig := filesystemtest.PadTo(t, filesystemtest.FakeImage(t, filesystemtest.JPEG, 32, 32), maxAvatarSize+1)
	for name, tc := range map[string]struct {
		file filesystemtest.UploadFile
		want int
	}{
		"one byte over the limit": {filesystemtest.UploadFile{Field: avatarField, Name: "big.jpg", Content: tooBig}, http.StatusRequestEntityTooLarge},
		"not an image":            {filesystemtest.UploadFile{Field: avatarField, Name: "me.png", Content: filesystemtest.FakeBytes(1024)}, http.StatusUnsupportedMediaType},
		"wrong field":             {filesystemtest.UploadFile{Field: "file", Name: "me.png", Content: filesystemtest.FakeImage(t, filesystemtest.GIF, 8, 8)}, http.StatusBadRequest},
	} {
		if rec := postAvatar(t, a, "u1", tc.file); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, tc.want, rec.Body)
		}
	}
	disk.AssertEmpty()
}
