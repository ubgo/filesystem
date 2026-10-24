package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
)

// TestRun runs every step and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"write: 29 bytes, sha256 matches an independent hash: true",
		"corrupted body with a declared sha256: invalid_argument",
		"stored version is still the first: true",
		"good body with a declared sha256: stored",
		"truncated body with a size hint: invalid_argument, stored: false",
		"etag: same bytes keep it (memory): true, new bytes change it: true, stat reports the latest: true",
		"content-addressed: same key twice: true, key is the sha256: true, objects: 1",
		"digests: crc32c 79a02f78 (as declared: true), md5 8e8333e47b0d58a90c1c846824684368, sha256 as always: true",
		"corrupted body with a declared crc32c: invalid_argument, stored version is still the first: true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestOtherDigestsAreTheRequestedHashes pins that the digests in
// Entry.Checksums come from the hashers the write named, by comparing
// them with hashes computed here.
func TestOtherDigestsAreTheRequestedHashes(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum([]byte(report))
	h := crc32c()
	_, _ = h.Write([]byte(report))
	want := "digests: crc32c " + hex.EncodeToString(h.Sum(nil)) + " (as declared: true), md5 " + hex.EncodeToString(sum[:])
	if !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
}
