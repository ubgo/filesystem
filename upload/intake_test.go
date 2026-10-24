package upload_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

func TestFromBase64AndDataURI(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	encoded := base64.StdEncoding.EncodeToString([]byte(pngData))
	// Wrapped lines, as some clients send them, decode the same.
	wrapped := encoded[:10] + "\r\n" + encoded[10:]
	for name, call := range map[string]func() (upload.Result, error){
		"base64": func() (upload.Result, error) { return upload.FromBase64(ctx, d, wrapped, "pic.png", images) },
		"data URI": func() (upload.Result, error) {
			return upload.FromDataURI(ctx, d, "data:image/png;base64,"+encoded, "pic.png", images)
		},
		"upper-case URI": func() (upload.Result, error) {
			return upload.FromDataURI(ctx, d, "DATA:;BASE64,"+encoded, "pic.png", images)
		},
		"lying data URI": func() (upload.Result, error) {
			return upload.FromDataURI(ctx, d, "data:text/plain;base64,"+encoded, "pic.png", images)
		},
	} {
		res, err := call()
		if err != nil || res.ContentType != "image/png" || res.Size != int64(len(pngData)) {
			t.Errorf("%s: %+v, %v", name, res, err)
			continue
		}
		if got, _ := d.ReadAll(ctx, res.Key); string(got) != pngData {
			t.Errorf("%s: stored %q", name, got)
		}
	}
	html := base64.StdEncoding.EncodeToString([]byte(htmlData))
	for name, c := range map[string]struct {
		call func() (upload.Result, error)
		code filesystem.Code
	}{
		"not base64": {func() (upload.Result, error) { return upload.FromBase64(ctx, d, "!!!!"+encoded, "a.png", images) }, filesystem.CodeInvalidArgument},
		"corrupt mid-way": {func() (upload.Result, error) {
			return upload.FromBase64(ctx, d, encoded[:20]+"*"+encoded[21:], "a.png", images)
		}, filesystem.CodeInvalidArgument},
		"not a data URI": {func() (upload.Result, error) { return upload.FromDataURI(ctx, d, "http://x/a.png", "a.png", images) }, filesystem.CodeInvalidArgument},
		"too short":      {func() (upload.Result, error) { return upload.FromDataURI(ctx, d, "dat", "a.png", images) }, filesystem.CodeInvalidArgument},
		"percent-encoded": {func() (upload.Result, error) {
			return upload.FromDataURI(ctx, d, "data:image/png,%89PNG", "a.png", images)
		}, filesystem.CodeInvalidArgument},
		"no comma": {func() (upload.Result, error) {
			return upload.FromDataURI(ctx, d, "data:image/png;base64", "a.png", images)
		}, filesystem.CodeInvalidArgument},
		"refused type": {func() (upload.Result, error) { return upload.FromBase64(ctx, d, html, "a.html", images) }, filesystem.CodeRejected},
		"over the size cap": {func() (upload.Result, error) {
			return upload.FromBase64(ctx, d, encoded, "a.png", upload.Policy{MaxSize: 8, Allowed: []string{"image/*"}})
		}, filesystem.CodeTooLarge},
	} {
		_, err := c.call()
		if filesystem.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if n := count(t, d); n != 4 {
		t.Errorf("%d objects stored, want only the 4 accepted", n)
	}
}

func TestPromote(t *testing.T) {
	ctx := t.Context()
	cache := newDisk(t, memory.New())
	store, err := filesystem.NewDisk("store", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	cachePolicy := images
	cachePolicy.Key = upload.RandomKey("cache")
	res, err := upload.Receive(ctx, cache, strings.NewReader(pngData), "a.png", cachePolicy)
	if err != nil {
		t.Fatal(err)
	}
	// Across disks: copied, then the cached object is gone.
	e, err := upload.Promote(ctx, cache, "cache", res.Key, store, "avatars/u1.png")
	if err != nil || e.Key != "avatars/u1.png" {
		t.Fatalf("promote = %+v, %v", e, err)
	}
	if got, _ := store.ReadAll(ctx, "avatars/u1.png"); string(got) != pngData {
		t.Error("promoted bytes differ")
	}
	if ok, _ := cache.Exists(ctx, res.Key); ok {
		t.Error("cached object left behind")
	}
	// Within one disk it is a move.
	res, _ = upload.Receive(ctx, cache, strings.NewReader(pngData), "b.png", cachePolicy)
	if _, err := upload.Promote(ctx, cache, "/cache/", res.Key, cache, "kept/b.png"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := cache.Exists(ctx, "kept/b.png"); !ok {
		t.Error("same-disk promote lost the object")
	}
	// A key outside the cache area is refused and nothing moves.
	_, _ = cache.WriteBytes(ctx, "private/secret.png", []byte(pngData), filesystem.WriteOptions{})
	for _, key := range []string{"private/secret.png", "cache", "cachex/a.png", "cache/../private/secret.png"} {
		_, err := upload.Promote(ctx, cache, "cache", key, store, "stolen.png")
		if code := filesystem.CodeOf(err); code != filesystem.CodePermission && code != filesystem.CodeInvalidKey {
			t.Errorf("promote %q: %v", key, err)
		}
	}
	if ok, _ := store.Exists(ctx, "stolen.png"); ok {
		t.Error("an object outside the cache area was promoted")
	}
	for _, dir := range []string{"", "/", "."} {
		_, err := upload.Promote(ctx, cache, dir, "private/secret.png", store, "x.png")
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	_, err = upload.Promote(ctx, cache, "a/../..", "cache/x", store, "x.png")
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = upload.Promote(ctx, cache, "cache", "cache/missing.png", store, "x.png")
	wantCode(t, err, filesystem.CodeNotFound)
}
