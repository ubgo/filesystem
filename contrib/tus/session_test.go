package tus_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	fstus "github.com/ubgo/filesystem/contrib/tus"
	"github.com/ubgo/filesystem/upload"
)

// sessionEnv is a tus handler whose uploads are resolved through upload
// sessions on a clock the test moves.
func sessionEnv(t *testing.T) (*env, *upload.Sessions, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	m, err := upload.NewSessions(&upload.MemorySessions{}, upload.SessionsOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return setup(t, fstus.Config{Resolve: m.Resolver()}), m, &now
}

// TestSessionAppliesToTus: an upload session's prefix and limits apply to
// tus uploads: checked against what the client declares at creation, and
// on the bytes at the final PATCH, where an expired session stops the
// upload and deletes it.
func TestSessionAppliesToTus(t *testing.T) {
	ctx := t.Context()
	e, m, now := sessionEnv(t)
	is, err := m.Issue(ctx, images, upload.SessionRequest{Prefix: "users/42", MaxSize: int64(len(pngData)), Allowed: []string{"image/png"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	png := map[string]string{fstus.MetaFilename: "me.png", fstus.MetaFiletype: "image/png"}
	if _, r := e.create(t, len(pngData), png, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no session = %d", r.StatusCode)
	}
	e.always = map[string]string{upload.HeaderUploadSession: is.Token}
	loc, r := e.create(t, len(pngData), png, nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", r.StatusCode)
	}
	final := e.send(t, loc, pngData)
	if key := final.Header.Get(fstus.HeaderKey); final.StatusCode != http.StatusNoContent || !strings.HasPrefix(key, "users/42/") {
		t.Fatalf("final = %d, key %q", final.StatusCode, key)
	}

	if _, r := e.create(t, len(pngData)+1, png, nil); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("declared over the session's size = %d", r.StatusCode)
	}
	if _, r := e.create(t, 10, map[string]string{fstus.MetaFiletype: "image/gif"}, nil); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("declared a type outside the session = %d", r.StatusCode)
	}
	// Declared a PNG, sent GIF bytes: refused on the bytes at the end.
	gif := "GIF89a" + strings.Repeat("x", len(pngData)-6)
	loc, _ = e.create(t, len(gif), png, nil)
	if r := e.send(t, loc, gif); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("GIF bytes under a PNG-only session = %d", r.StatusCode)
	}

	// The session expires mid-upload: the final PATCH is refused and the
	// upload deleted.
	loc, _ = e.create(t, len(pngData), png, nil)
	*now = now.Add(time.Hour)
	if r := e.send(t, loc, pngData); r.StatusCode != http.StatusForbidden {
		t.Errorf("final PATCH after expiry = %d", r.StatusCode)
	}
	if n := sessions(t, e.tmp); n != 0 {
		t.Errorf("%d session files left", n)
	}
	var stored int
	for _, err := range e.disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		stored++
	}
	if stored != 1 {
		t.Errorf("%d objects stored, want the one accepted upload", stored)
	}
}

// TestResolveMisconfigured: a Resolve returning a policy that fails
// Validate is the server's mistake: 500 with the status text only.
func TestResolveMisconfigured(t *testing.T) {
	broken := func(*http.Request, upload.Policy) (upload.Policy, error) { return upload.Policy{}, nil }
	e := setup(t, fstus.Config{Resolve: broken})
	if _, r := e.create(t, 10, nil, nil); r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("misconfigured create = %d", r.StatusCode)
	}
}

// TestResolveSeesTheRequest: the request a Resolver gets carries the
// method, path, headers and remote address of the tus request.
func TestResolveSeesTheRequest(t *testing.T) {
	var seen []string
	e := setup(t, fstus.Config{Resolve: func(r *http.Request, p upload.Policy) (upload.Policy, error) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-User")+" "+boolString(r.RemoteAddr != ""))
		return p, nil
	}})
	e.always = map[string]string{"X-User": "u1"}
	loc, _ := e.create(t, len(pngData), map[string]string{fstus.MetaFiletype: "image/png"}, nil)
	if r := e.send(t, loc, pngData); r.StatusCode != http.StatusNoContent {
		t.Fatalf("final = %d", r.StatusCode)
	}
	if len(seen) != 2 || seen[0] != "POST /uploads/ u1 true" || !strings.HasPrefix(seen[1], "PATCH /uploads/") || !strings.HasSuffix(seen[1], " u1 true") {
		t.Fatalf("Resolve saw %q", seen)
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
