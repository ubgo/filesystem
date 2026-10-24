package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
)

// exportKey is the object the extra-digest step writes.
const exportKey = "exports/q3.csv"

// crc32c is the Castagnoli CRC GCS reports; any hash.Hash plugs in the
// same way.
// ds:def id=crc32c-m3v7q9tk
func crc32c() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }

// otherDigests writes with a CRC32C the sender declared and an MD5
// computed only, both in the same pass as the SHA-256; then writes a body
// whose CRC32C does not match the declared one.
// ds:def id=otherdigests-x8p2w5rn
func otherDigests(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	h := crc32c()
	_, _ = io.WriteString(h, report)
	declared := fmt.Sprintf("%x", h.Sum(nil)) // what the sender computed
	e, err := disk.Write(ctx, exportKey, strings.NewReader(report), filesystem.WriteOptions{
		Checksums: []filesystem.Checksum{
			{Name: "crc32c", New: crc32c, Want: declared}, // verified like SHA256
			{Name: "md5", New: md5.New},                   // computed only
		},
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "digests: crc32c %s (as declared: %t), md5 %s, sha256 as always: %t\n",
		e.Checksums["crc32c"], e.Checksums["crc32c"] == declared, e.Checksums["md5"], e.SHA256 == digest(report))
	_, err = disk.Write(ctx, exportKey, strings.NewReader(strings.Replace(report, "340", "999", 1)), filesystem.WriteOptions{
		Checksums: []filesystem.Checksum{{Name: "crc32c", New: crc32c, Want: declared}},
	})
	b, rerr := disk.ReadAll(ctx, exportKey)
	if rerr != nil {
		return rerr
	}
	fmt.Fprintf(out, "corrupted body with a declared crc32c: %s, stored version is still the first: %t\n", filesystem.CodeOf(err), string(b) == report)
	return nil
}
