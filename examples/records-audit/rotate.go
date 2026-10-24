package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
)

// Chain keys of the rotation demo. A real app keeps them in a secret
// manager or a KMS, never in code.
var (
	// oldChainKey is the key the log was chained with before key IDs.
	oldChainKey = []byte("audit-chain-key-of-2025-kept-in-kms")
	// newChainKey is the key new entries are hashed with from October on.
	newChainKey = []byte("audit-chain-key-of-2026-10-in-kms")
)

// rotationKeyID names newChainKey in the entries it hashes.
const rotationKeyID = "2026-10"

// rotatedRing hashes new entries with newChainKey under its ID and still
// verifies entries chained with WithHashChain(oldChainKey), which carry
// the ID "". Back a Keyring with a KMS to rotate without a deploy.
// ds:def id=rotatedring-m5c9x2hq
func rotatedRing() audit.Keyring {
	return audit.StaticKeyring{
		CurrentID: rotationKeyID,
		Keys: map[string][]byte{
			"":            oldChainKey,
			rotationKeyID: newChainKey,
		},
	}
}

// newRotatedPlugin is the plugin after the rotation: the chain keyed by
// rotatedRing, and every entry logging the request's channel and client
// (WithSource). The IP is personal data and needs WithSourceIP as well.
// ds:def id=newrotatedplugin-v2k7r4wn
func newRotatedPlugin() *audit.Plugin {
	return audit.New(audit.WithHashChainKeyring(rotatedRing()), audit.WithSource())
}

// rotateDemo chains one entry under the single old key, then opens the
// Store again with the keyring and WithSource, records a change made from
// the iOS app, and verifies the whole chain across the rotation.
func rotateDemo(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "rotate.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newDisks()
	if err != nil {
		return err
	}
	const scope = "rotating"
	ctx = records.WithActor(ctx, "user:erin")
	before, err := newStore(ctx, db, disks, audit.New(audit.WithHashChain(oldChainKey)))
	if err != nil {
		return err
	}
	f, err := before.Upload(ctx, scope, diskName, strings.NewReader("v1"), "plan.txt", feedPolicy)
	if err != nil {
		return err
	}
	au := newRotatedPlugin()
	s, err := newStore(ctx, db, disks, au)
	if err != nil {
		return err
	}
	ios := records.WithSource(ctx, records.Source{Channel: "ios", Client: "app/3.2", IP: "203.0.113.7"})
	if _, err := s.Replace(ios, scope, f.ID, strings.NewReader("v2"), "", feedPolicy); err != nil {
		return err
	}
	page, err := au.Changes(ctx, s, audit.ChangeQuery{Scope: scope})
	if err != nil {
		return err
	}
	for _, e := range page.Entries {
		fmt.Fprintf(w, "rotated chain: %d:%s key=%q source=%q %q ip=%q\n", e.Seq, e.Kind, e.KeyID, e.Source.Channel, e.Source.Client, e.Source.IP)
	}
	n, err := verify(ctx, s, au, scope)
	fmt.Fprintf(w, "verify across the rotation: %d entries, err=%v\n", n, err)
	return nil
}
