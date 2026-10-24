package records_test

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// checksumPolicy is a permissive upload policy that asks for an MD5.
func checksumPolicy() upload.Policy {
	return upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}, Checksums: []filesystem.Checksum{{Name: "md5", New: md5.New}}}
}

// TestFileChecksums pins R-I16 in records: the digests
// upload.Policy.Checksums asks for land on the file row and read back
// (Upload and Replace), a replacement without them clears them, a file
// without any stores NULL and reads nil (the default), and NewFile's are
// stored lowercase.
func TestFileChecksums(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f, err := e.store.Upload(ctx, "t", "media", strings.NewReader("hello"), "a.txt", checksumPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if f.Checksums["md5"] != md5Hex("hello") || len(f.Checksums) != 1 {
		t.Fatalf("uploaded checksums %v", f.Checksums)
	}
	got, err := e.store.File(ctx, "t", f.ID)
	if err != nil || got.Checksums["md5"] != md5Hex("hello") {
		t.Fatalf("read back %v, %v", got.Checksums, err)
	}
	r, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader("bye"), "", checksumPolicy())
	if err != nil || r.Checksums["md5"] != md5Hex("bye") {
		t.Fatalf("replaced checksums %v, %v", r.Checksums, err)
	}
	plain := checksumPolicy()
	plain.Checksums = nil
	r, err = e.store.Replace(ctx, "t", f.ID, strings.NewReader("again"), "", plain)
	if err != nil || r.Checksums != nil {
		t.Fatalf("a replacement without checksums kept %v, %v", r.Checksums, err)
	}
	if got, _ := e.store.File(ctx, "t", f.ID); got.Checksums != nil {
		t.Fatalf("stale checksums after the replacement: %v", got.Checksums)
	}

	none, err := e.store.Upload(ctx, "t", "media", strings.NewReader("x"), "b.txt", plain)
	if err != nil || none.Checksums != nil {
		t.Fatalf("default upload checksums %v, %v", none.Checksums, err)
	}
	var stored *string
	if err := e.store.One(ctx, sqlb.NewSelect(e.store.Dialect()).Columns(sqlb.Bind(records.FileChecksums, &stored)).From(records.TableFiles).
		Where(sqlb.Eq(records.FileID, none.ID))); err != nil || stored != nil {
		t.Fatalf("a file without checksums stores %v, %v", stored, err)
	}

	if _, err := e.disk.WriteBytes(ctx, "c.txt", []byte("c"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "c.txt", Checksums: map[string]string{"crc32c": "ABCDEF01"}})
	if err != nil || c.Checksums["crc32c"] != "abcdef01" {
		t.Fatalf("CreateFile checksums %v, %v", c.Checksums, err)
	}
}

// TestFileChecksumsRefused: names and values outside the rule are
// CodeInvalidArgument, on CreateFile and on ReplaceObject, and nothing is
// recorded.
func TestFileChecksumsRefused(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	if _, err := e.disk.WriteBytes(ctx, "b.png", []byte(pngData), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	many := map[string]string{}
	for i := range records.MaxChecksums + 1 {
		many[string(rune('a'+i))] = "00"
	}
	for name, sums := range map[string]map[string]string{
		"upper name": {"MD5": "00"},
		"empty name": {"": "00"},
		"not hex":    {"md5": "zz"},
		"odd hex":    {"md5": "abc"},
		"empty":      {"md5": ""},
		"too long":   {"sha": strings.Repeat("00", records.MaxChecksumBytes+1)},
		"too many":   many,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "b.png", Checksums: sums})
			wantCode(t, err, filesystem.CodeInvalidArgument)
			_, err = e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "b.png", Checksums: sums})
			wantCode(t, err, filesystem.CodeInvalidArgument)
		})
	}
	page, err := e.store.Files(ctx, records.FileQuery{Scope: "t"})
	if err != nil || len(page.Files) != 1 || page.Files[0].Key != "a.png" {
		t.Fatalf("a refused checksum recorded something: %v %v", page.Files, err)
	}
}
