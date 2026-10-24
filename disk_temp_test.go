package filesystem_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// understated reports every object as one byte, as an object that grew
// after its Stat would look.
type understated struct{ *memory.Driver }

func (u understated) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := u.Driver.Stat(ctx, key)
	e.Size = 1
	return e, err
}

// midStreamFailure fails its reads part-way.
type midStreamFailure struct{ *memory.Driver }

func (midStreamFailure) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return io.NopCloser(io.MultiReader(strings.NewReader("partial"), iotestErr{})), nil
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// leftovers lists the files in dir.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestTempFile(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	d := mustDisk(t, "media", memory.New())
	_, _ = d.WriteBytes(ctx, "videos/clip.MP4", []byte("movie bytes"), filesystem.WriteOptions{})
	_, _ = d.WriteBytes(ctx, "odd/name.tar-gz", []byte("x"), filesystem.WriteOptions{})
	_, _ = d.WriteBytes(ctx, "odd/name.averyveryverylongext", []byte("x"), filesystem.WriteOptions{})

	p, err := d.TempFile(ctx, "videos/clip.MP4", filesystem.TempFileOptions{Dir: dir, MaxSize: 11})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	info, _ := os.Stat(p)
	if string(got) != "movie bytes" || filepath.Dir(p) != dir || !strings.HasSuffix(p, ".MP4") || !strings.HasPrefix(filepath.Base(p), "filesystem-") {
		t.Errorf("temp file %s holds %q", p, got)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("temp file mode %v is readable by others", info.Mode())
	}
	// Extensions that are not short and alphanumeric are dropped.
	for _, key := range []string{"odd/name.tar-gz", "odd/name.averyveryverylongext"} {
		p, err := d.TempFile(ctx, key, filesystem.TempFileOptions{Dir: dir})
		if err != nil || filepath.Ext(p) != "" {
			t.Errorf("%s: %s, %v; want no extension", key, p, err)
		}
	}
	// The default directory is the system one.
	p, err = d.TempFile(ctx, "videos/clip.MP4", filesystem.TempFileOptions{})
	if err != nil || filepath.Dir(p) != filepath.Clean(os.TempDir()) {
		t.Errorf("default dir: %s, %v", p, err)
	}
	_ = os.Remove(p)
}

func TestTempFileFailuresLeaveNothing(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name   string
		driver filesystem.Driver
		key    string
		opts   func(dir string) filesystem.TempFileOptions
		code   filesystem.Code
	}{
		{"missing", memory.New(), "nope", func(d string) filesystem.TempFileOptions { return filesystem.TempFileOptions{Dir: d} }, filesystem.CodeNotFound},
		{"too large by stat", memory.New(), "a.bin", func(d string) filesystem.TempFileOptions { return filesystem.TempFileOptions{Dir: d, MaxSize: 3} }, filesystem.CodeTooLarge},
		{"grew after stat", understated{memory.New()}, "a.bin", func(d string) filesystem.TempFileOptions { return filesystem.TempFileOptions{Dir: d, MaxSize: 3} }, filesystem.CodeTooLarge},
		{"stream breaks", midStreamFailure{memory.New()}, "a.bin", func(d string) filesystem.TempFileOptions { return filesystem.TempFileOptions{Dir: d} }, filesystem.CodeUnknown},
		{"read fails", unreadable{newBasic()}, "a.bin", func(d string) filesystem.TempFileOptions { return filesystem.TempFileOptions{Dir: d} }, filesystem.CodePermission},
		{"no such directory", memory.New(), "a.bin", func(d string) filesystem.TempFileOptions {
			return filesystem.TempFileOptions{Dir: filepath.Join(d, "missing")}
		}, filesystem.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			d := mustDisk(t, "media", c.driver)
			_, _ = d.WriteBytes(ctx, "a.bin", []byte("0123456789"), filesystem.WriteOptions{})
			p, err := d.TempFile(ctx, c.key, c.opts(dir))
			var fe *filesystem.Error
			if p != "" || filesystem.CodeOf(err) != c.code || !errors.As(err, &fe) || fe.Disk != "media" || fe.Op == "" {
				t.Errorf("path %q, err %v; want code %s with op and disk", p, err, c.code)
			}
			if left := leftovers(t, dir); len(left) != 0 {
				t.Errorf("left %v", left)
			}
		})
	}
}
