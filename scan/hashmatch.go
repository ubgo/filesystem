package scan

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"strings"
)

// Digest is a fingerprint of a file's content: an exact hash (MD5 and
// SHA-256 are what published hash lists use) or a perceptual hash that
// survives resizing and re-encoding (PDQ, PhotoDNA), computed by a
// Fingerprinter.
type Digest struct {
	// Algorithm names the fingerprint: AlgorithmMD5, AlgorithmSHA256, or
	// the name a perceptual Fingerprinter gives its own.
	Algorithm string
	// Value is the fingerprint, lower-case hex for the exact hashes.
	Value string
}

// The exact-hash algorithms this package computes.
const (
	AlgorithmMD5    = "md5"
	AlgorithmSHA256 = "sha256"
)

// Fingerprinter computes one Digest of r. Perceptual hashing needs an
// image decoder and lives in the application (or a module with its own
// dependencies); it plugs in here.
type Fingerprinter func(ctx context.Context, r io.Reader) (Digest, error)

// MD5 is the Fingerprinter for MD5 lists, which most shared hash lists of
// known abuse and malware material still publish.
func MD5(ctx context.Context, r io.Reader) (Digest, error) {
	return exact(ctx, r, AlgorithmMD5, md5.New())
}

// SHA256 is the Fingerprinter for SHA-256 lists.
func SHA256(ctx context.Context, r io.Reader) (Digest, error) {
	return exact(ctx, r, AlgorithmSHA256, sha256.New())
}

// exact hashes r with h.
func exact(ctx context.Context, r io.Reader, algorithm string, h hash.Hash) (Digest, error) {
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: r}); err != nil {
		return Digest{}, err
	}
	return Digest{Algorithm: algorithm, Value: hex.EncodeToString(h.Sum(nil))}, nil
}

// contextReader stops a read once ctx ends, so hashing a large file
// honours cancellation.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

// Read implements io.Reader.
func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// HashMatcher looks a fingerprint up in a list of known content: a hash
// list loaded in memory (HashSet), a moderation service, or a database. A
// match is a Verdict with Clean false and Signature naming the list
// entry; an error means the lookup did not complete.
type HashMatcher interface {
	Match(ctx context.Context, d Digest) (Verdict, error)
}

// HashMatcherFunc adapts a function to HashMatcher, for a moderation API
// call.
type HashMatcherFunc func(ctx context.Context, d Digest) (Verdict, error)

// Match implements HashMatcher.
func (f HashMatcherFunc) Match(ctx context.Context, d Digest) (Verdict, error) { return f(ctx, d) }

// HashScanner returns a Scanner that fingerprints the stream with f and
// asks m, so a hash list plugs into Inspect like a virus scanner and
// fails closed the same way. Chain it with a malware Scanner through
// upload.Inspectors to run both.
func HashScanner(f Fingerprinter, m HashMatcher) Scanner {
	return ScannerFunc(func(ctx context.Context, r io.Reader) (Verdict, error) {
		d, err := f(ctx, r)
		if err != nil {
			return Verdict{}, err
		}
		return m.Match(ctx, d)
	})
}

// HashSet is a HashMatcher over a fixed list of digests held in memory,
// for lists that are published as files and loaded at start-up. It is
// immutable once built; load a new list into a new HashSet. Exact hashes
// match case-insensitively.
type HashSet struct {
	signature string
	digests   map[Digest]bool
}

// NewHashSet returns a HashSet matching digests, reporting signature
// (the list's name, such as "known-abuse:2026-10") on a match; the matched
// digest is not echoed, because the signature reaches the uploader.
func NewHashSet(signature string, digests ...Digest) *HashSet {
	s := &HashSet{signature: signature, digests: make(map[Digest]bool, len(digests))}
	for _, d := range digests {
		s.digests[normalise(d)] = true
	}
	return s
}

// Match implements HashMatcher.
func (s *HashSet) Match(_ context.Context, d Digest) (Verdict, error) {
	if s.digests[normalise(d)] {
		return Verdict{Signature: s.signature}, nil
	}
	return Verdict{Clean: true}, nil
}

// Len is the number of digests in the set.
func (s *HashSet) Len() int { return len(s.digests) }

// normalise lower-cases a digest so hex from any list matches.
func normalise(d Digest) Digest {
	return Digest{Algorithm: strings.ToLower(d.Algorithm), Value: strings.ToLower(strings.TrimSpace(d.Value))}
}
