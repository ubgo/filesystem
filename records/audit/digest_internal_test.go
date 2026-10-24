package audit

import (
	"testing"
	"time"

	"github.com/ubgo/filesystem/records"
)

// TestLegacyHashFormatUnchanged pins the hash of an entry and a chain head
// with no key ID and no source to values computed by an independent
// HMAC-SHA256 over the original encoding, so chains written before key IDs
// and sources existed keep verifying. A v2 row must hash differently.
func TestLegacyHashFormatUnchanged(t *testing.T) {
	key := []byte("golden-key")
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	r := row{seq: 7, prev: "prevhash", id: "aud_1", scope: "t", at: at, actor: "user:alice", kind: "file.created", fileID: "fil_1"}
	if got := digest(r, key); got != "4e44f6548a291a6ac472fd718b3a72f8bba5a1d01404b3dcb1a52ab009a3a3b0" {
		t.Fatalf("legacy digest = %s", got)
	}
	c := chainHead{scope: "t", seq: 7, head: "headhash", anchorAt: at, since: at.Add(-time.Hour)}
	if got := seal(c, key); got != "29e977590921023b539a4cd8e807a5f00270232a81b96115ade5b853603f19d4" {
		t.Fatalf("legacy seal = %s", got)
	}
	withKey, withSource := r, r
	withKey.keyID = "k1"
	withSource.source = records.Source{Channel: "web"}
	c2 := c
	c2.keyID = "k1"
	otherKey := withKey
	otherKey.keyID = "k2"
	otherSource := withSource
	otherSource.source.Client = "app/2"
	if digest(otherKey, key) == digest(withKey, key) || digest(otherSource, key) == digest(withSource, key) {
		t.Fatal("the key ID or source is not part of the hash")
	}
	if digest(withKey, key) == digest(r, key) || digest(withSource, key) == digest(r, key) || seal(c2, key) == seal(c, key) {
		t.Fatal("a key ID or source does not change the hash")
	}
}
