package filesystem_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// verifying is a memory driver declaring ChecksumSHA256 that records the
// digest it was given to send to its backend.
type verifying struct {
	*memory.Driver
	got *string
}

func (v verifying) Capabilities() filesystem.Capabilities {
	c := v.Driver.Capabilities()
	c.ChecksumSHA256 = true
	return c
}

func (v verifying) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	*v.got = o.SHA256
	return v.Driver.Write(ctx, key, body, o)
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestWriteChecksum(t *testing.T) {
	ctx := t.Context()
	var sent string
	for name, driver := range map[string]filesystem.Driver{"memory": memory.New(), "buggy": swallower{newBasic()}, "verifying": verifying{memory.New(), &sent}} {
		d := mustDisk(t, "d", driver)
		_, _ = d.WriteBytes(ctx, "k", []byte("previous"), filesystem.WriteOptions{})
		// A stream, as an upload is: the digest is only known at its end.
		write := func(body, digest string) error {
			_, err := d.Write(ctx, "k", io.MultiReader(strings.NewReader(body)), filesystem.WriteOptions{SHA256: digest})
			return err
		}
		if err := write("corrupted", digestOf("intended")); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: mismatch = %v", name, err)
		}
		got, _ := d.ReadAll(ctx, "k")
		if name != "buggy" && string(got) != "previous" {
			t.Errorf("%s: a mismatched body replaced the object: %q", name, got)
		}
		if name == "buggy" {
			if ok, _ := d.Exists(ctx, "k"); ok {
				t.Errorf("buggy: mismatched object kept")
			}
		}
		if err := write("intended", strings.ToUpper(digestOf("intended"))); err != nil {
			t.Errorf("%s: matching digest in upper case: %v", name, err)
		}
		if got, _ := d.ReadAll(ctx, "k"); string(got) != "intended" {
			t.Errorf("%s: stored %q", name, got)
		}
		for _, bad := range []string{"abc", strings.Repeat("z", 64), digestOf("x")[:62]} {
			if err := write("intended", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
				t.Errorf("%s: malformed %q = %v", name, bad, err)
			}
		}
	}
	// A verifying driver is handed the digest, lower case, for its backend.
	if sent != digestOf("intended") {
		t.Errorf("driver got %q", sent)
	}
	d := mustDisk(t, "v", verifying{memory.New(), &sent})
	if !strings.Contains(d.Capabilities().String(), "checksum-sha256") {
		t.Errorf("caps %v", d.Capabilities())
	}
	_, err := d.CreateMultipart(ctx, "big", filesystem.WriteOptions{SHA256: digestOf("x")})
	wantCode(t, err, filesystem.CodeUnsupported)
}
