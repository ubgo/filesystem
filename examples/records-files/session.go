package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// sortScope is the tenant of the sort demo, apart from the files the
// earlier steps recorded under tenant.
const sortScope = "globex"

// session is the app's own signed-in user, kept under its own context
// key by its authentication middleware. The Store never sees this type.
type session struct{ UserID string }

// sessionKey is the context key of session.
type sessionKey struct{}

// withSession is what the app's middleware does once per request.
func withSession(ctx context.Context, s session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// newSessionStore names the actor from the app's own session type with
// records.WithActorFunc, so no middleware has to copy the user into
// records.WithActor. An empty answer falls back to records.WithActor,
// which the library itself sets for share-link uploads.
// ds:def id=newsessionstore-m8x3k7wd
func newSessionStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithActorFunc(func(ctx context.Context) string {
			if u, ok := ctx.Value(sessionKey{}).(session); ok {
				return "user:" + u.UserID
			}
			return ""
		}),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// listSorted walks every page of a scope in one order: a drive's sort
// menu. Every order is a keyset, so the pages stay stable while files are
// added; a cursor works only with the order that issued it.
// ds:def id=listsorted-q2v9n6tp
func listSorted(ctx context.Context, s *records.Store, scope string, order records.FileOrder) ([]string, error) {
	var names []string
	q := records.FileQuery{Scope: scope, Limit: 2, Order: order}
	for {
		page, err := s.Files(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, f := range page.Files {
			names = append(names, fmt.Sprintf("%s(%d)", f.Filename, f.Size))
		}
		if page.NextCursor == "" {
			return names, nil
		}
		q.Cursor = page.NextCursor
	}
}

// sessionAndSort uploads three files as the session's user, then lists
// them in each name and size order.
func sessionAndSort(ctx context.Context, w io.Writer, db *sql.DB, disks *filesystem.Manager) error {
	s, err := newSessionStore(ctx, db, disks)
	if err != nil {
		return err
	}
	signedIn := withSession(ctx, session{UserID: "42"})
	for _, f := range []struct{ name, body string }{
		{"budget.txt", "abc"}, {"Agenda.txt", "ten bytes!"}, {"notes.txt", "x"},
	} {
		up, err := s.Upload(signedIn, sortScope, diskName, strings.NewReader(f.body), f.name, notesPolicy)
		if err != nil {
			return err
		}
		if f.name == "budget.txt" {
			fmt.Fprintf(w, "session actor: uploaded by %s\n", up.UploadedBy)
		}
	}
	shared, err := s.Upload(records.WithActor(ctx, "share:lnk_1"), sortScope, diskName, strings.NewReader("drop"), "dropped.txt", notesPolicy)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "no session: uploaded by %s\n", shared.UploadedBy)
	if err := s.Purge(ctx, sortScope, shared.ID); err != nil {
		return err
	}
	for _, o := range []records.FileOrder{records.OrderName, records.OrderNameDesc, records.OrderLargest, records.OrderSmallest} {
		names, err := listSorted(ctx, s, sortScope, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "order %-10s %s\n", o+":", strings.Join(names, " "))
	}
	page, err := s.Files(ctx, records.FileQuery{Scope: sortScope, Limit: 1, Order: records.OrderName})
	if err != nil {
		return err
	}
	_, err = s.Files(ctx, records.FileQuery{Scope: sortScope, Limit: 1, Order: records.OrderLargest, Cursor: page.NextCursor})
	fmt.Fprintf(w, "name cursor with the largest order: %s\n", filesystem.CodeOf(err))
	return nil
}
