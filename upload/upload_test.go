package upload_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
)

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"photo.png":              "photo.png",
		"../../etc/passwd":       "passwd",
		`C:\Users\me\cv.pdf`:     "cv.pdf",
		"dir/":                   "dir",
		"  spaced  .txt ":        "spaced  .txt",
		"bad\x00name\n.txt":      "badname.txt",
		"":                       "file",
		"..":                     "file",
		"/":                      "file",
		strings.Repeat("é", 200): strings.Repeat("é", 127),
		"ünïcödé ファイル.jpg":       "ünïcödé ファイル.jpg",
	}
	for in, want := range cases {
		if got := upload.SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPolicyAccepts(t *testing.T) {
	p := upload.Policy{MaxSize: 1, Allowed: []string{"image/*", "text/*"}, Denied: []string{"image/gif"}}
	for typ, ok := range map[string]bool{
		"image/png":                 true,
		"image/gif":                 false,
		"application/pdf":           false,
		sniff.SVG:                   false,
		"text/html; charset=utf-8":  false,
		"text/plain; charset=utf-8": true,
	} {
		if err := p.Accepts(typ); (err == nil) != ok {
			t.Errorf("Accepts(%q) = %v, want ok=%v", typ, err, ok)
		} else if err != nil {
			wantCode(t, err, filesystem.CodeRejected)
		}
	}
	p.AllowActiveContent = true
	if err := p.Accepts(sniff.SVG); err != nil {
		t.Errorf("AllowActiveContent did not lift the default denials: %v", err)
	}
	// Every +xml type is active (XHTML script runs in any of them), not
	// only the ones the default list names.
	wide := upload.Policy{MaxSize: 1, Allowed: []string{"application/*"}}
	for _, typ := range []string{"application/rss+xml", "application/atom+xml", "application/xslt+xml"} {
		if err := wide.Accepts(typ); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("Accepts(%q) = %v, want rejected", typ, err)
		}
	}
	wide.AllowActiveContent = true
	if err := wide.Accepts("application/rss+xml"); err != nil {
		t.Errorf("AllowActiveContent did not lift the +xml rule: %v", err)
	}
	// AllowActive exempts the named types only.
	narrow := upload.Policy{MaxSize: 1, Allowed: []string{"image/*", "application/*"}, AllowActive: []string{sniff.SVG}}
	if err := narrow.Accepts(sniff.SVG); err != nil {
		t.Errorf("AllowActive did not exempt SVG: %v", err)
	}
	for _, typ := range []string{"application/rss+xml", "application/xhtml+xml"} {
		if err := narrow.Accepts(typ); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("AllowActive SVG let %q through: %v", typ, err)
		}
	}
}

func TestRandomKey(t *testing.T) {
	k, err := upload.RandomKey("avatars")(upload.Info{ContentType: "image/png"})
	if err != nil || !strings.HasPrefix(k, "avatars/") || !strings.HasSuffix(k, ".png") || strings.ToLower(k) != k {
		t.Errorf("RandomKey = %q, %v", k, err)
	}
	k2, _ := upload.RandomKey("")(upload.Info{ContentType: "application/x-unknown"})
	if strings.Contains(k2, "/") || strings.Contains(k2, ".") || k2 == k {
		t.Errorf("RandomKey without dir or extension = %q", k2)
	}
}

func TestReceive(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t, memory.New())
	res, err := upload.Receive(ctx, d, reader(pngData), "../me.png", images)
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType != "image/png" || res.Filename != "me.png" || res.Size != int64(len(pngData)) || len(res.SHA256) != 64 || !strings.HasSuffix(res.Key, ".png") {
		t.Errorf("result = %+v", res)
	}
	if got, _ := d.ReadAll(ctx, res.Key); string(got) != pngData {
		t.Error("stored bytes differ")
	}
	// The client's name cannot change the detected type.
	_, err = upload.Receive(ctx, d, reader(htmlData), "innocent.png", images)
	wantCode(t, err, filesystem.CodeRejected)
	_, err = upload.Receive(ctx, d, reader(svgData), "logo.svg", upload.Policy{MaxSize: 100, Allowed: []string{"image/*"}})
	wantCode(t, err, filesystem.CodeRejected)

	small := upload.Policy{MaxSize: 10, Allowed: []string{"image/*"}, Key: func(upload.Info) (string, error) { return "fixed.png", nil }}
	_, err = upload.Receive(ctx, d, reader(pngData), "x.png", small)
	wantCode(t, err, filesystem.CodeTooLarge)
	if ok, _ := d.Exists(ctx, "fixed.png"); ok {
		t.Error("oversize upload stored")
	}
	keyErr := errors.New("no user in context")
	_, err = upload.Receive(ctx, d, reader(pngData), "x.png", upload.Policy{MaxSize: 100, Allowed: []string{"image/*"}, Key: func(upload.Info) (string, error) { return "", keyErr }})
	if !errors.Is(err, keyErr) {
		t.Errorf("key func error = %v", err)
	}
	_, err = upload.Receive(ctx, d, reader(pngData), "x.png", upload.Policy{MaxSize: 100, Allowed: []string{"image/*"}, Key: func(upload.Info) (string, error) { return "../x", nil }})
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = upload.Receive(ctx, d, &failing{}, "x.png", images)
	wantCode(t, err, filesystem.CodeUnavailable)
	for _, p := range []upload.Policy{{Allowed: []string{"*/*"}}, {MaxSize: 1}, {MaxSize: 1, Allowed: []string{"*/*"}, Visibility: "hidden"}} {
		_, err := upload.Receive(ctx, d, reader("x"), "x", p)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	pub := upload.Policy{MaxSize: 100, Allowed: []string{"image/*"}, Visibility: filesystem.VisibilityPublic}
	res, err = upload.Receive(ctx, d, reader(pngData), "p.png", pub)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, res.Key); e.Visibility != filesystem.VisibilityPublic {
		t.Errorf("policy visibility ignored: %q", e.Visibility)
	}
}

func TestSweep(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at := now.Add(-48 * time.Hour)
	d := newDisk(t, memory.New(memory.WithClock(func() time.Time { return at })))
	_, _ = d.WriteBytes(ctx, "tmp/old-1", []byte("x"), filesystem.WriteOptions{})
	_, _ = d.WriteBytes(ctx, "tmp/a/old-2", []byte("x"), filesystem.WriteOptions{})
	_, _ = d.WriteBytes(ctx, "kept/old", []byte("x"), filesystem.WriteOptions{})
	at = now
	_, _ = d.WriteBytes(ctx, "tmp/new", []byte("x"), filesystem.WriteOptions{})
	n, err := upload.Sweep(ctx, d, "tmp", now.Add(-24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	for k, want := range map[string]bool{"tmp/old-1": false, "tmp/a/old-2": false, "tmp/new": true, "kept/old": true} {
		if ok, _ := d.Exists(ctx, k); ok != want {
			t.Errorf("%s exists = %v", k, ok)
		}
	}
	_, err = upload.Sweep(ctx, d, "/", now)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = upload.Sweep(ctx, d, "../x", now)
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = upload.Sweep(ctx, newDisk(t, unlistable{memory.New()}), "tmp", now)
	wantCode(t, err, filesystem.CodeUnavailable)
	bad := newDisk(t, undeletable{memory.New()})
	_, _ = bad.WriteBytes(ctx, "tmp/x", []byte("x"), filesystem.WriteOptions{})
	_, err = upload.Sweep(ctx, bad, "tmp", now.Add(time.Hour*1000000))
	wantCode(t, err, filesystem.CodePermission)
}

// undated is a memory driver whose Stat and List report no modification
// time, as some backends do for some objects.
type undated struct{ *memory.Driver }

func (u undated) List(ctx context.Context, dir string, o filesystem.ListOptions) (filesystem.Page, error) {
	page, err := u.Driver.List(ctx, dir, o)
	for i := range page.Entries {
		page.Entries[i].ModTime = time.Time{}
	}
	return page, err
}

func TestSweepKeepsUndatedObjects(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, undated{memory.New()})
	_, _ = disk.WriteBytes(ctx, "incoming/a", []byte("x"), filesystem.WriteOptions{})
	n, err := upload.Sweep(ctx, disk, "incoming", time.Now().Add(time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("Sweep deleted %d objects of unknown age, %v", n, err)
	}
	if ok, _ := disk.Exists(ctx, "incoming/a"); !ok {
		t.Fatal("an undated object was deleted")
	}
}

// TestFindStaleReportsWhatSweepDeletes pins that the dry run selects
// exactly the objects Sweep would delete (old, dated, under dir), deletes
// none of them, and refuses the root as Sweep does.
func TestFindStaleReportsWhatSweepDeletes(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at := now.Add(-48 * time.Hour)
	d := newDisk(t, memory.New(memory.WithClock(func() time.Time { return at })))
	for _, k := range []string{"tmp/old-1", "tmp/a/old-2", "kept/old"} {
		if _, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	at = now
	if _, err := d.WriteBytes(ctx, "tmp/new", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	var found []string
	n, err := upload.FindStale(ctx, d, "tmp", cutoff, func(e filesystem.Entry) { found = append(found, e.Key) })
	if err != nil || n != 2 || len(found) != 2 {
		t.Fatalf("FindStale = %d, %v, found %v", n, err, found)
	}
	for _, k := range []string{"tmp/old-1", "tmp/a/old-2", "tmp/new", "kept/old"} {
		if ok, _ := d.Exists(ctx, k); !ok {
			t.Errorf("dry run deleted %s", k)
		}
	}
	swept, err := upload.Sweep(ctx, d, "tmp", cutoff)
	if err != nil || swept != n {
		t.Errorf("Sweep deleted %d, %v; the dry run reported %d", swept, err, n)
	}
	_, err = upload.FindStale(ctx, d, "/", now, func(filesystem.Entry) {})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}
