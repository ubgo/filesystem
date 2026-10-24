package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
)

// The permission model of a document tool, the app's own rather than the
// plugin's read < write < admin ladder.
const (
	view    acl.Level = "view"
	comment acl.Level = "comment"
	edit    acl.Level = "edit"
	dropbox acl.Level = "dropbox" // upload only: sees nothing

	// onProject is the kind of grants on the app's projects.
	onProject acl.ResourceKind = "project"
	carol                      = "user:carol"
	dave                       = "user:dave"
	// grantLife is how long Carol's grant lasts: "access until Friday".
	grantLife = 72 * time.Hour
)

// projects is the app's own project table, here a map from a project ID
// to the files it holds. It is an acl.Container, so grants can sit on a
// project and reach every file in it.
type projects struct {
	mu    sync.Mutex
	files map[string][]string
}

// Kind implements acl.Container.
func (*projects) Kind() acl.ResourceKind { return onProject }

// Of names the projects that hold fileID.
func (p *projects) Of(_ context.Context, _ *records.Store, _, fileID string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for id, files := range p.files {
		if slices.Contains(files, fileID) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Check refuses a grant on a project that does not exist.
func (p *projects) Check(_ context.Context, _ *records.Store, _, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.files[id]; !ok {
		return filesystem.Errorf(filesystem.CodeNotFound, "no project %q", id)
	}
	return nil
}

// newCustomACL builds the plugin with the app's levels, the map from a
// records action to them, and projects as containers. clock is the
// plugin's time, so the demo can step past a grant's expiry.
// ds:def id=newcustomacl-r7d2m9xk
func newCustomACL(ps *projects, clock func() time.Time) *acl.Plugin {
	return acl.New(
		acl.WithLevels(
			acl.LevelDef{Level: view},
			acl.LevelDef{Level: comment, Includes: []acl.Level{view}},
			acl.LevelDef{Level: edit, Includes: []acl.Level{comment}},
			acl.LevelDef{Level: dropbox},
		),
		acl.WithLevelFor(func(a records.Action) (acl.Level, bool) {
			switch a {
			case records.ActionRead:
				return view, true
			case records.ActionSetText:
				return comment, true
			case records.ActionReplace, records.ActionTrash, records.ActionRestore, records.ActionSetVisibility, records.ActionPurge:
				return edit, true
			}
			return "", false // the owner routes go to the app's own rule
		}),
		acl.WithContainers(ps),
		acl.WithClock(clock),
	)
}

// grantUntil gives Carol comment access to a whole project until a time,
// and Dave the upload-only level on it.
// ds:def id=grantuntil-b4q8w2nf
func grantUntil(ctx context.Context, s *records.Store, ap *acl.Plugin, project string, until time.Time) error {
	if _, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: carol, Kind: onProject, Resource: project, Level: comment, ExpiresAt: &until}); err != nil {
		return err
	}
	_, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: dave, Kind: onProject, Resource: project, Level: dropbox})
	return err
}

// customModel runs the app's own permission model on a Store of its own:
// levels that are not a ladder, a grant on a project, a grant that
// expires, and Prune.
func customModel(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "custom.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	ps := &projects{files: map[string][]string{}}
	ap := newCustomACL(ps, clock)
	s, err := storeWith(ctx, db, ap)
	if err != nil {
		return err
	}
	spec, err := record(ctx, s, "spec.txt", "the spec")
	if err != nil {
		return err
	}
	ps.files["prj_42"] = []string{spec.ID}
	if err := grantUntil(ctx, s, ap, "prj_42", now.Add(grantLife)); err != nil {
		return err
	}
	for _, c := range []struct {
		who   string
		level acl.Level
	}{{carol, view}, {carol, comment}, {carol, edit}, {dave, dropbox}, {dave, view}} {
		ok, err := ap.Can(ctx, s, tenant, []string{c.who}, spec.ID, c.level)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%-10s %-7s spec.txt %t\n", c.who, c.level, ok)
	}
	_, err = ap.Grant(ctx, s, tenant, acl.Grant{Subject: carol, Kind: onProject, Resource: "prj_404", Level: view})
	fmt.Fprintf(w, "grant on a missing project: %s\n", filesystem.CodeOf(err))
	_, err = ap.Can(ctx, s, tenant, []string{carol}, spec.ID, acl.Admin)
	fmt.Fprintf(w, "check for a level the model lacks: %s\n", filesystem.CodeOf(err))
	if err := readOverHTTP(w, s, ap, spec.ID, "before expiry"); err != nil {
		return err
	}
	now = now.Add(grantLife + time.Hour)
	ok, err := ap.Can(ctx, s, tenant, []string{carol}, spec.ID, view)
	if err != nil {
		return err
	}
	pruned, err := ap.Prune(ctx, s, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "after %s, carol views spec.txt: %t; pruned %d expired grant(s)\n", grantLife+time.Hour, ok, pruned)
	return readOverHTTP(w, s, ap, spec.ID, "after expiry")
}

// readOverHTTP asks the records routes for a file as Carol: acl.Authorizer
// maps the read to view through WithLevelFor.
func readOverHTTP(w io.Writer, s *records.Store, ap *acl.Plugin, fileID, when string) error {
	h, err := newHandler(s, ap)
	if err != nil {
		return err
	}
	req := httptest.NewRequest(http.MethodGet, "/files/"+fileID, nil)
	req.Header.Set(header, carol)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Fprintf(w, "GET /files/{spec} as %s %s: %d\n", carol, when, rec.Code)
	return nil
}
