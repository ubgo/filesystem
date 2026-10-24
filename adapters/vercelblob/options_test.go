package vercelblob_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/vercelblob"
)

// putHeaders records the x-cache-control-max-age header of every PUT the
// fake receives.
func putHeaders(f *fake) *[]string {
	var got []string
	f.on(func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			got = append(got, r.Header.Get("x-cache-control-max-age"))
		}
		return false
	})
	return &got
}

// TestCacheMaxAge proves Config.CacheMaxAge goes on every write and copy
// in seconds, that the default sends none, and that values Vercel refuses
// are refused here.
func TestCacheMaxAge(t *testing.T) {
	ctx := t.Context()
	f := newFake(t, true)
	cfg := config(f, vercelblob.AccessPublic)
	cfg.CacheMaxAge = 5 * time.Minute
	d, _ := filesystem.NewDisk("vercel", newDriver(t, cfg))
	got := putHeaders(f)
	if _, err := d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Copy(ctx, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || (*got)[0] != "300" || (*got)[1] != "300" {
		t.Fatalf("PUT max-age headers = %q, want 300 on both", *got)
	}

	plain, _ := filesystem.NewDisk("vercel", newDriver(t, config(f, vercelblob.AccessPublic)))
	got = putHeaders(f)
	if _, err := plain.WriteBytes(ctx, "c", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0] != "" {
		t.Fatalf("default sent max-age %q", *got)
	}
	for _, bad := range []time.Duration{-time.Minute, 30 * time.Second, 90*time.Second + time.Millisecond} {
		c := config(f, vercelblob.AccessPublic)
		c.CacheMaxAge = bad
		if _, err := vercelblob.New(c); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("CacheMaxAge %s: %v, want CodeInvalidArgument", bad, err)
		}
	}
}

// TestOpener proves a DSN opens a disk with the token from the named (or
// default) variable and the cache age it gives, and refuses credentials
// in the URL, a missing token, a path, a bad access mode or cache age.
func TestOpener(t *testing.T) {
	ctx := t.Context()
	t.Setenv("MY_BLOB_TOKEN", testToken)
	t.Setenv(vercelblob.DefaultTokenEnv, testToken)
	for _, dsn := range []string{"vercelblob://public?token_env=MY_BLOB_TOKEN&cache_max_age=120", "vercelblob://private"} {
		u, _ := url.Parse(dsn)
		drv, err := vercelblob.Opener(ctx, u)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if got := drv.Capabilities().PublicURL; got != (u.Host == "public") {
			t.Errorf("%s: PublicURL capability %v", dsn, got)
		}
	}
	t.Setenv("EMPTY_TOKEN", "")
	for _, dsn := range []string{
		"vercelblob://user:pw@public",
		"vercelblob://public?token_env=EMPTY_TOKEN",
		"vercelblob://public/sub",
		"vercelblob://shared",
		"vercelblob://public?cache_max_age=0",
		"vercelblob://public?cache_max_age=30",
		"vercelblob://public?typo=1",
	} {
		u, _ := url.Parse(dsn)
		if _, err := vercelblob.Opener(ctx, u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", dsn, err)
		}
	}
}
