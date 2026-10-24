package cloudinary_test

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/cloudinary"
)

// TestResourceType proves ResourceImage stores, stats, reads, lists and
// deletes image resources (every request names "image", none "raw"), that
// a read and PublicURL name the key's extension as the format, and that
// the default stays raw with no format.
func TestResourceType(t *testing.T) {
	ctx := t.Context()
	f := newFake(t)
	cfg := config(f, cloudinary.TypeUpload)
	cfg.ResourceType = cloudinary.ResourceImage
	d, _ := filesystem.NewDisk("cloudinary", newDriver(t, cfg))
	if _, err := d.WriteBytes(ctx, "photos/a.jpg", []byte("jpeg"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.res["photos/a.jpg"].rtype != "image" {
		t.Fatalf("stored as %q", f.res["photos/a.jpg"].rtype)
	}
	if _, err := d.Stat(ctx, "photos/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if b, err := d.ReadAll(ctx, "photos/a.jpg"); err != nil || string(b) != "jpeg" {
		t.Fatalf("read %q, %v", b, err)
	}
	if got := f.downloads[len(f.downloads)-1].Get("format"); got != "jpg" {
		t.Errorf("read format %q, want jpg", got)
	}
	if page, err := d.ListPage(ctx, "photos", filesystem.ListOptions{}); err != nil || len(page.Entries) != 1 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	if u, err := d.PublicURL(ctx, "photos/a.jpg"); err != nil || u != "https://res.example.com/demo/image/upload/photos/a.jpg.jpg" {
		t.Errorf("PublicURL = %q, %v", u, err)
	}
	if err := d.Delete(ctx, "photos/a.jpg"); err != nil || len(f.res) != 0 {
		t.Fatalf("delete = %v, left %d", err, len(f.res))
	}
	// No extension, or one that cannot be a format: no format is named.
	for _, key := range []string{"photos/noext", "photos/odd.j+g"} {
		if u, err := d.PublicURL(ctx, key); err != nil || strings.HasSuffix(u, ".j+g") || strings.Count(u[strings.LastIndex(u, "/"):], ".") > 1 {
			t.Errorf("PublicURL(%q) = %q, %v", key, u, err)
		}
	}

	raw, _ := filesystem.NewDisk("cloudinary", newDriver(t, config(f, cloudinary.TypeAuthenticated)))
	if _, err := raw.WriteBytes(ctx, "a.jpg", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ReadAll(ctx, "a.jpg"); err != nil {
		t.Fatal(err)
	}
	if f.res["a.jpg"].rtype != "raw" || f.downloads[len(f.downloads)-1].Has("format") {
		t.Errorf("default stored as %q with format %q", f.res["a.jpg"].rtype, f.downloads[len(f.downloads)-1].Get("format"))
	}
}

// TestTypePrivate proves the private delivery type is accepted and sent.
func TestTypePrivate(t *testing.T) {
	ctx := t.Context()
	f := newFake(t)
	d, _ := filesystem.NewDisk("cloudinary", newDriver(t, config(f, cloudinary.TypePrivate)))
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.res["k"].typ != "private" || d.Capabilities().PublicURL {
		t.Fatalf("type %q, PublicURL %v", f.res["k"].typ, d.Capabilities().PublicURL)
	}
	if b, err := d.ReadAll(ctx, "k"); err != nil || string(b) != "x" {
		t.Fatalf("read %q, %v", b, err)
	}
}

// TestClock proves request timestamps and a read's expiry come from
// Config.Clock: a clock two hours behind makes the fake refuse the read
// as expired.
func TestClock(t *testing.T) {
	ctx := t.Context()
	f := newFake(t)
	at := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	cfg := config(f, cloudinary.TypeAuthenticated)
	cfg.Clock = func() time.Time { return at }
	d, _ := filesystem.NewDisk("cloudinary", newDriver(t, cfg))
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadAll(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("read with a clock two hours behind = %v, want the expiry refused", err)
	}
	p := f.downloads[len(f.downloads)-1]
	if p.Get("timestamp") != strconv.FormatInt(at.Unix(), 10) || p.Get("expires_at") != strconv.FormatInt(at.Add(cloudinary.DownloadExpiry).Unix(), 10) {
		t.Errorf("timestamp %s, expires_at %s; want the clock's", p.Get("timestamp"), p.Get("expires_at"))
	}
}

// TestOpener proves a DSN opens a disk with the secret from the
// environment, and that a secret in the URL, a missing or empty secret
// variable, a path or an unknown parameter are refused.
func TestOpener(t *testing.T) {
	ctx := t.Context()
	t.Setenv("CLD_SECRET", testSecret)
	t.Setenv("CLD_EMPTY", "")
	u, _ := url.Parse("cloudinary://" + testKey + "@" + testCloud + "?api_secret_env=CLD_SECRET&type=upload&resource_type=image")
	drv, err := cloudinary.Opener(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	c := drv.(*cloudinary.Driver)
	if got, err := c.PublicURL(ctx, "a.png"); err != nil || !strings.HasSuffix(got, "/"+testCloud+"/image/upload/a.png.png") {
		t.Errorf("PublicURL from the DSN = %q, %v", got, err)
	}
	for _, dsn := range []string{
		"cloudinary://" + testKey + ":" + testSecret + "@" + testCloud + "?api_secret_env=CLD_SECRET",
		"cloudinary://" + testKey + "@" + testCloud,
		"cloudinary://" + testKey + "@" + testCloud + "?api_secret_env=CLD_EMPTY",
		"cloudinary://" + testKey + "@" + testCloud + "/sub?api_secret_env=CLD_SECRET",
		"cloudinary://" + testCloud + "?api_secret_env=CLD_SECRET",
		"cloudinary://" + testKey + "@" + testCloud + "?api_secret_env=CLD_SECRET&typo=1",
		"cloudinary://" + testKey + "@" + testCloud + "?api_secret_env=CLD_SECRET&resource_type=auto",
	} {
		u, _ := url.Parse(dsn)
		if _, err := cloudinary.Opener(ctx, u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", dsn, err)
		}
	}
}
