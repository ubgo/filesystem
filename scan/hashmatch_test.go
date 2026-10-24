package scan_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

func TestFingerprints(t *testing.T) {
	ctx := t.Context()
	for _, c := range []struct {
		f    scan.Fingerprinter
		want scan.Digest
	}{
		{scan.MD5, scan.Digest{Algorithm: scan.AlgorithmMD5, Value: "5d41402abc4b2a76b9719d911017c592"}},
		{scan.SHA256, scan.Digest{Algorithm: scan.AlgorithmSHA256, Value: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"}},
	} {
		got, err := c.f(ctx, strings.NewReader("hello"))
		if err != nil || got != c.want {
			t.Errorf("got %+v, %v; want %+v", got, err, c.want)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := scan.MD5(canceled, strings.NewReader("hello")); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestHashSet(t *testing.T) {
	set := scan.NewHashSet("known-bad", scan.Digest{Algorithm: "MD5", Value: " 5D41402ABC4B2A76B9719D911017C592 "})
	if set.Len() != 1 {
		t.Fatalf("len %d", set.Len())
	}
	v, err := set.Match(t.Context(), scan.Digest{Algorithm: scan.AlgorithmMD5, Value: "5d41402abc4b2a76b9719d911017c592"})
	if err != nil || v.Clean || v.Signature != "known-bad" {
		t.Fatalf("listed digest: %+v, %v", v, err)
	}
	// The same value under another algorithm is a different fingerprint.
	if v, _ := set.Match(t.Context(), scan.Digest{Algorithm: scan.AlgorithmSHA256, Value: "5d41402abc4b2a76b9719d911017c592"}); !v.Clean {
		t.Fatal("matched across algorithms")
	}
}

// A hash list and a perceptual matcher plug into an upload policy through
// the public API, refusing listed content and passing the rest; a matcher
// that cannot answer fails closed.
func TestHashScannerInPolicy(t *testing.T) {
	ctx := t.Context()
	disk, err := filesystem.NewDisk("m", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	list := scan.NewHashSet("known-bad", scan.Digest{Algorithm: scan.AlgorithmSHA256, Value: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"})
	// A second matcher: a stand-in for a perceptual-hash service, which
	// matches by distance rather than equality. Here: any digest whose
	// "perceptual" value shares a prefix with a known one.
	perceptual := func(_ context.Context, r io.Reader) (scan.Digest, error) {
		b, err := io.ReadAll(r)
		if err != nil {
			return scan.Digest{}, err
		}
		return scan.Digest{Algorithm: "toy-phash", Value: string(b[:min(len(b), 4)])}, nil
	}
	near := scan.HashMatcherFunc(func(_ context.Context, d scan.Digest) (scan.Verdict, error) {
		if strings.HasPrefix(d.Value, "hel") {
			return scan.Verdict{Signature: "near-duplicate"}, nil
		}
		return scan.Verdict{Clean: true}, nil
	})
	down := scan.HashMatcherFunc(func(context.Context, scan.Digest) (scan.Verdict, error) {
		return scan.Verdict{}, errors.New("connection refused")
	})
	for name, c := range map[string]struct {
		inspect upload.Inspector
		body    string
		want    filesystem.Code
	}{
		"listed":           {scan.Inspect(scan.HashScanner(scan.SHA256, list)), "hello", filesystem.CodeRejected},
		"not listed":       {scan.Inspect(scan.HashScanner(scan.SHA256, list)), "hello!", ""},
		"near duplicate":   {scan.Inspect(scan.HashScanner(perceptual, near)), "help me", filesystem.CodeRejected},
		"perceptual clean": {scan.Inspect(scan.HashScanner(perceptual, near)), "goodbye", ""},
		"matcher down":     {scan.Inspect(scan.HashScanner(scan.MD5, down)), "anything", filesystem.CodeUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := disk.WriteBytes(ctx, "u", []byte(c.body), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			err := c.inspect(ctx, disk, upload.Result{Key: "u"})
			if filesystem.CodeOf(err) != c.want && !(c.want == "" && err == nil) {
				t.Fatalf("got %v, want code %q", err, c.want)
			}
		})
	}
}
