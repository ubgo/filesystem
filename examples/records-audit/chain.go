package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/upload"
)

// chainKey keys the hash chain's HMAC. A real app keeps it outside the
// database (a secret manager, a KMS), so someone who can rewrite every
// row still cannot rebuild the chain.
var chainKey = []byte("audit-chain-key-kept-outside-the-db")

// feedPolicy accepts plain text with a random key.
var feedPolicy = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("feed")}

// newChainedPlugin turns the log into a tamper-evident trail and a change
// feed (WithHashChain), and an access log of every read
// (WithAccessLog; sample a fraction in production, reads outnumber
// changes).
// ds:def id=newchainedplugin-t6w2k8qm
func newChainedPlugin() *audit.Plugin {
	return audit.New(
		audit.WithHashChain(chainKey),
		audit.WithAccessLog(audit.SampleRate(1)),
	)
}

// follow reads the change feed from cursor to the end, a page at a time,
// as a search indexer or a sync job would; it returns the kinds read and
// the cursor to store for the next poll.
// ds:def id=follow-c9n4v7hx
func follow(ctx context.Context, s *records.Store, au *audit.Plugin, scope, cursor string) ([]string, string, error) {
	var kinds []string
	for {
		page, err := au.Changes(ctx, s, audit.ChangeQuery{Scope: scope, Cursor: cursor, Limit: 2})
		if err != nil {
			return nil, cursor, err
		}
		for _, e := range page.Entries {
			kinds = append(kinds, fmt.Sprintf("%d:%s", e.Seq, e.Kind))
		}
		if len(page.Entries) == 0 {
			return kinds, page.NextCursor, nil
		}
		cursor = page.NextCursor
	}
}

// verify walks the scope's chain. A conflict wrapping
// audit.ErrChainBroken names the first entry edited, deleted, reordered
// or inserted behind the plugin's back.
// ds:def id=verify-k3p8w5rc
func verify(ctx context.Context, s *records.Store, au *audit.Plugin, scope string) (int64, error) {
	return au.Verify(ctx, s, scope)
}

// chainDemo records changes and a read on a chained log, follows the
// feed, verifies the chain, then edits an entry with SQL and verifies
// again.
func chainDemo(ctx context.Context, w io.Writer, db *sql.DB, disks *filesystem.Manager) error {
	au := newChainedPlugin()
	s, err := newStore(ctx, db, disks, au)
	if err != nil {
		return err
	}
	const scope = "chained"
	ctx = records.WithActor(ctx, "user:dana")
	f, err := s.Upload(ctx, scope, diskName, strings.NewReader("v1"), "notes.txt", feedPolicy)
	if err != nil {
		return err
	}
	if _, err := s.Replace(ctx, scope, f.ID, strings.NewReader("v2"), "", feedPolicy); err != nil {
		return err
	}
	if err := s.Served(ctx, records.Served{Kind: records.ServeStream, Scope: scope, FileID: f.ID, Bytes: 2}); err != nil {
		return err
	}
	if err := s.Trash(ctx, scope, f.ID); err != nil {
		return err
	}
	kinds, cursor, err := follow(ctx, s, au, scope, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "change feed: %s (reads left out)\n", strings.Join(kinds, ", "))
	if err := s.Restore(ctx, scope, f.ID); err != nil {
		return err
	}
	more, _, err := follow(ctx, s, au, scope, cursor)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "next poll from the stored cursor: %s\n", strings.Join(more, ", "))
	n, err := verify(ctx, s, au, scope)
	fmt.Fprintf(w, "verify: %d entries, err=%v\n", n, err)
	if _, err := db.ExecContext(ctx, "UPDATE "+audit.TableNameAudit+" SET "+audit.ColActor+" = 'user:mallory' WHERE "+audit.ColSeq+" = 1 AND "+audit.ColScope+" = ?", scope); err != nil {
		return err
	}
	_, err = verify(ctx, s, au, scope)
	fmt.Fprintf(w, "after an edit with SQL: %s, chain broken %t\n", filesystem.CodeOf(err), errors.Is(err, audit.ErrChainBroken))
	return nil
}
