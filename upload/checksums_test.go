package upload_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"hash"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// crc32c is a Castagnoli CRC32, the digest GCS reports.
func crc32c() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }

// digest is the lowercase hex digest of s under h.
func digest(h hash.Hash, s string) string {
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// shaReporting is a memory disk whose Stat reports a SHA-256, as S3 with
// checksums does, so Confirm would otherwise read only the head.
type shaReporting struct{ *memory.Driver }

func (s shaReporting) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := s.Driver.Stat(ctx, key)
	e.SHA256 = strings.Repeat("ab", 32)
	return e, err
}

// TestPolicyChecksums pins R-I16: Policy.Checksums reaches Result.Checksums
// through Receive (computed by Disk.Write) and through Confirm (computed
// from the stored object, also when the backend reports a SHA-256); a Want
// that does not match refuses the upload and leaves nothing stored; a bad
// list is refused by Validate; and the default computes nothing and keeps
// the JSON response unchanged.
func TestPolicyChecksums(t *testing.T) {
	ctx := t.Context()
	wantMD5, wantCRC := digest(md5.New(), pngData), digest(crc32c(), pngData)
	sums := images
	sums.Checksums = []filesystem.Checksum{{Name: "md5", New: md5.New}, {Name: "crc32c", New: crc32c}}
	check := func(t *testing.T, res upload.Result) {
		t.Helper()
		if res.Checksums["md5"] != wantMD5 || res.Checksums["crc32c"] != wantCRC || len(res.Checksums) != 2 {
			t.Fatalf("checksums %v, want md5 %s crc32c %s", res.Checksums, wantMD5, wantCRC)
		}
	}

	t.Run("receive", func(t *testing.T) {
		res, err := upload.Receive(ctx, newDisk(t, memory.New()), strings.NewReader(pngData), "a.png", sums)
		if err != nil {
			t.Fatal(err)
		}
		check(t, res)
		b, _ := json.Marshal(res)
		if !strings.Contains(string(b), `"checksums":{`) {
			t.Fatalf("JSON lost the checksums: %s", b)
		}
	})
	for name, drv := range map[string]filesystem.Driver{"confirm": memory.New(), "confirm on a backend reporting SHA-256": shaReporting{memory.New()}} {
		t.Run(name, func(t *testing.T) {
			d := newDisk(t, drv)
			if _, err := d.WriteBytes(ctx, "direct.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			res, err := upload.Confirm(ctx, d, "direct.png", "a.png", sums)
			if err != nil {
				t.Fatal(err)
			}
			check(t, res)
		})
	}

	mismatch := images
	mismatch.Checksums = []filesystem.Checksum{{Name: "md5", New: md5.New, Want: strings.Repeat("0", 32)}}
	t.Run("receive refuses a mismatched Want", func(t *testing.T) {
		d := newDisk(t, memory.New())
		_, err := upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", mismatch)
		wantCode(t, err, filesystem.CodeInvalidArgument)
		if n := countKeys(t, d); n != 0 {
			t.Fatalf("%d objects stored after a mismatch", n)
		}
	})
	t.Run("confirm deletes a mismatched Want", func(t *testing.T) {
		d := newDisk(t, memory.New())
		if _, err := d.WriteBytes(ctx, "direct.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		_, err := upload.Confirm(ctx, d, "direct.png", "a.png", mismatch)
		wantCode(t, err, filesystem.CodeRejected)
		if n := countKeys(t, d); n != 0 {
			t.Fatalf("%d objects left after a mismatch", n)
		}
	})
	t.Run("confirm accepts a matching Want in any case", func(t *testing.T) {
		d := newDisk(t, memory.New())
		if _, err := d.WriteBytes(ctx, "direct.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		match := images
		match.Checksums = []filesystem.Checksum{{Name: "md5", New: md5.New, Want: strings.ToUpper(wantMD5)}}
		if _, err := upload.Confirm(ctx, d, "direct.png", "a.png", match); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("validate refuses a bad list", func(t *testing.T) {
		bad := images
		bad.Checksums = []filesystem.Checksum{{Name: "md5", New: md5.New}, {Name: "md5", New: md5.New}}
		wantCode(t, bad.Validate(), filesystem.CodeInvalidArgument)
		bad.Checksums = []filesystem.Checksum{{Name: "MD5", New: md5.New}}
		wantCode(t, bad.Validate(), filesystem.CodeInvalidArgument)
	})
	t.Run("default computes nothing", func(t *testing.T) {
		d := newDisk(t, memory.New())
		res, err := upload.Receive(ctx, d, strings.NewReader(pngData), "a.png", images)
		if err != nil || res.Checksums != nil {
			t.Fatalf("Receive = %v, %v", res.Checksums, err)
		}
		if b, _ := json.Marshal(res); strings.Contains(string(b), "checksums") {
			t.Fatalf("default JSON changed: %s", b)
		}
		res, err = upload.Confirm(ctx, d, res.Key, "a.png", images)
		if err != nil || res.Checksums != nil {
			t.Fatalf("Confirm = %v, %v", res.Checksums, err)
		}
	})
}
