package filesystem_test

import (
	"encoding/hex"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// crc32c and crc64ecma stand in for whatever digest an application keeps
// (MD5 for an existing column, CRC32C for GCS, BLAKE3): the Disk names none.
func crc32c() hash.Hash    { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }
func crc64ecma() hash.Hash { return crc64.New(crc64.MakeTable(crc64.ECMA)) }

func digest(h hash.Hash, s string) string {
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func TestWriteChecksums(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("d", memory.New())
	const body = "hello, checksums"
	sums := []filesystem.Checksum{{Name: "crc32c", New: crc32c}, {Name: "crc64", New: crc64ecma}}
	e, err := d.WriteBytes(ctx, "a.txt", []byte(body), filesystem.WriteOptions{Checksums: sums})
	if err != nil {
		t.Fatal(err)
	}
	if e.Checksums["crc32c"] != digest(crc32c(), body) || e.Checksums["crc64"] != digest(crc64ecma(), body) || len(e.Checksums) != 2 {
		t.Errorf("Checksums = %v", e.Checksums)
	}
	if e.SHA256 == "" {
		t.Error("SHA-256 is no longer computed beside the extra digests")
	}

	// A declared digest that holds stores the body; any case is accepted.
	ok := []filesystem.Checksum{{Name: "crc32c", New: crc32c, Want: strings.ToUpper(digest(crc32c(), "new body"))}}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("new body"), filesystem.WriteOptions{Checksums: ok}); err != nil {
		t.Fatalf("matching Want: %v", err)
	}
	// One that does not refuses the write and keeps what was stored.
	bad := []filesystem.Checksum{{Name: "crc32c", New: crc32c, Want: digest(crc32c(), "something else")}}
	_, err = d.WriteBytes(ctx, "a.txt", []byte("corrupted"), filesystem.WriteOptions{Checksums: bad})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "crc32c") {
		t.Fatalf("mismatch = %v", err)
	}
	if got, _ := d.ReadAll(ctx, "a.txt"); string(got) != "new body" {
		t.Errorf("a refused write replaced the object: %q", got)
	}
	// The caller's slice is not rewritten.
	if ok[0].Want != strings.ToUpper(digest(crc32c(), "new body")) {
		t.Error("Write normalised the caller's Checksum in place")
	}

	// Default unchanged: no extra digests.
	e, err = d.WriteBytes(ctx, "b.txt", []byte(body), filesystem.WriteOptions{})
	if err != nil || e.Checksums != nil || e.SHA256 == "" {
		t.Errorf("default = %+v, %v", e, err)
	}
}

func TestWriteChecksumsRefusesMalformed(t *testing.T) {
	d, _ := filesystem.NewDisk("d", memory.New())
	for name, list := range map[string][]filesystem.Checksum{
		"empty name":   {{Name: "", New: crc32c}},
		"upper name":   {{Name: "CRC", New: crc32c}},
		"twice":        {{Name: "crc", New: crc32c}, {Name: "crc", New: crc32c}},
		"no hasher":    {{Name: "crc"}},
		"short want":   {{Name: "crc", New: crc32c, Want: "ab"}},
		"hexless want": {{Name: "crc", New: crc32c, Want: "zzzzzzzz"}},
	} {
		_, err := d.WriteBytes(t.Context(), "x", []byte("x"), filesystem.WriteOptions{Checksums: list})
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
		// ValidateChecksums refuses what Write refuses, before any byte.
		if err := filesystem.ValidateChecksums(list); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("ValidateChecksums %s: %v", name, err)
		}
		if exists, _ := d.Exists(t.Context(), "x"); exists {
			t.Fatalf("%s: stored despite the refusal", name)
		}
	}
	for name, list := range map[string][]filesystem.Checksum{
		"nil":       nil,
		"two":       {{Name: "crc32c", New: crc32c}, {Name: "crc64", New: crc64ecma}},
		"want case": {{Name: "crc", New: crc32c, Want: strings.ToUpper(digest(crc32c(), "x"))}},
	} {
		if err := filesystem.ValidateChecksums(list); err != nil {
			t.Errorf("ValidateChecksums %s: %v", name, err)
		}
	}
	_, err := d.CreateMultipart(t.Context(), "m", filesystem.WriteOptions{Checksums: []filesystem.Checksum{{Name: "crc", New: crc32c}}})
	if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), "Checksums") {
		t.Errorf("multipart = %v", err)
	}
}
