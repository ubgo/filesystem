package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

// removedListName is the signature a match reports. It reaches the
// uploader, so it names the list, never the matched digest.
const removedListName = "removed-by-moderation"

// loadHashList reads a hash list in the format moderation tools export:
// one lower- or upper-case hex SHA-256 per line, blank lines and lines
// starting with # ignored. A line that is not a SHA-256 is refused, so a
// list in the wrong format fails at start-up instead of matching nothing.
// ds:def id=loadhashlist-jz8z8r2p
func loadHashList(r io.Reader) ([]scan.Digest, error) {
	var list []scan.Digest
	lines := bufio.NewScanner(r)
	for n := 1; lines.Scan(); n++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if b, err := hex.DecodeString(line); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("hash list line %d: not a SHA-256: %q", n, line)
		}
		list = append(list, scan.Digest{Algorithm: scan.AlgorithmSHA256, Value: line})
	}
	return list, lines.Err()
}

// moderatedPolicy is policyWith plus a hash list: every upload is scanned
// for malware first, then fingerprinted and looked up in the list of files
// the moderators removed. Either refusal deletes the file.
// ds:def id=moderatedpolicy-734sh2n5
func moderatedPolicy(malware scan.Scanner, removed *scan.HashSet) upload.Policy {
	p := policyWith(malware)
	p.Inspect = upload.Inspectors(
		scan.Inspect(malware),
		scan.Inspect(scan.HashScanner(scan.SHA256, removed)),
	)
	return p
}

// moderation loads a list holding the digest of one removed file, then
// uploads that file again and a file that is not on the list.
func moderation(ctx context.Context, out io.Writer, disk *filesystem.Disk, malware scan.Scanner) error {
	removedFile := "a meme the moderators took down\n"
	sum := sha256.Sum256([]byte(removedFile))
	list, err := loadHashList(strings.NewReader("# exported 2026-10-08\n" + strings.ToUpper(hex.EncodeToString(sum[:])) + "\n"))
	if err != nil {
		return err
	}
	removed := scan.NewHashSet(removedListName, list...)
	fmt.Fprintf(out, "hash list: %d digest(s)\n", removed.Len())
	for _, body := range []string{removedFile, "a new post\n"} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(body), "post.txt", moderatedPolicy(malware, removed))
		fmt.Fprintf(out, "moderated upload: stored=%t err=%v\n", res.Key != "", err)
	}
	return nil
}
