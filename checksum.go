package filesystem

import (
	"encoding/hex"
	"hash"
	"regexp"
)

// Checksum asks Disk.Write for one more digest of the body, computed while
// it streams, beside the SHA-256 it always computes (Entry.SHA256), and
// optionally verified: MD5 for an existing checksum
// column, CRC32C to compare with what GCS reports, BLAKE3 or xxHash for
// fast content addressing. The library names no algorithm and imports no
// hash package for it: the caller brings the hash.Hash.
//
// Why a list of hashers rather than a fixed set: every application keeps
// the digest its other systems already use, and adding one should not
// need a release of this library. SHA-256 stays the one the Disk always
// computes, so Entry.SHA256, upload.Result.SHA256 and the records schema
// keep their meaning.
type Checksum struct {
	// Name labels the digest in Entry.Checksums ("md5", "crc32c"):
	// lowercase letters, digits and dashes, 1 to 32 characters, unique
	// within one write.
	Name string
	// New returns a fresh hasher; it is called once per write attempt, so
	// a retried write starts from zero. Required.
	New func() hash.Hash
	// Want, when set, is the hex digest the body must have (any case).
	// A body that ends with another one fails the write with
	// CodeInvalidArgument before the driver commits, exactly as
	// WriteOptions.SHA256 does, so a corrupted body never replaces what
	// was stored. Its length must match New's Size.
	Want string
}

// checksumName is the shape of Checksum.Name: short, lowercase, safe as a
// map key, a metadata suffix or a column name.
var checksumName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidateChecksums checks list the way Disk.Write does before it reads a
// byte: every Name is 1 to 32 lowercase letters, digits or dashes and
// unique, every New is set, and every Want is hex of New's size. Layers
// that compute the same digests themselves (upload.Confirm reading an
// assembled object) call it so a bad list fails the same way on every
// path, with CodeInvalidArgument.
func ValidateChecksums(list []Checksum) error {
	_, err := prepareChecksums(list)
	return err
}

// prepareChecksums validates list and returns a copy with every Want in
// lowercase hex, leaving the caller's slice untouched.
func prepareChecksums(list []Checksum) ([]Checksum, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make([]Checksum, len(list))
	seen := make(map[string]bool, len(list))
	for i, c := range list {
		switch {
		case !checksumName.MatchString(c.Name):
			return nil, Errorf(CodeInvalidArgument, "checksum name %q is not 1 to 32 lowercase letters, digits or dashes", c.Name)
		case seen[c.Name]:
			return nil, Errorf(CodeInvalidArgument, "checksum %q is listed twice", c.Name)
		case c.New == nil:
			return nil, Errorf(CodeInvalidArgument, "checksum %q has no New", c.Name)
		}
		seen[c.Name] = true
		if c.Want != "" {
			sum, err := hex.DecodeString(c.Want)
			if size := c.New().Size(); err != nil || len(sum) != size {
				return nil, Errorf(CodeInvalidArgument, "checksum %q Want is not %d hex characters", c.Name, 2*size)
			}
			c.Want = hex.EncodeToString(sum)
		}
		out[i] = c
	}
	return out, nil
}

// runningChecksum is one Checksum being computed for one write attempt.
type runningChecksum struct {
	name string
	want string
	hash hash.Hash
}

// startChecksums opens a fresh hasher for every checksum of an attempt.
func startChecksums(list []Checksum) []runningChecksum {
	if len(list) == 0 {
		return nil
	}
	out := make([]runningChecksum, len(list))
	for i, c := range list {
		out[i] = runningChecksum{name: c.Name, want: c.Want, hash: c.New()}
	}
	return out
}
