package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app: two workspaces of one organisation share
// the organisation's plan, on top of their own (unlimited) workspace
// counters.
const (
	// orgID is the organisation both workspaces belong to.
	orgID = "initech"
	// northWS and southWS are the organisation's workspaces (scopes).
	northWS = "initech-north"
	southWS = "initech-south"
)

// orgPlan is the organisation's cap over all of its workspaces.
var orgPlan = quota.Limit{Bytes: 20}

// orgOf is the app's directory of which organisation owns a workspace; a
// real app reads its own table here.
var orgOf = map[string]string{northWS: orgID, southWS: orgID}

// orgDimensions charges every file to its organisation's counter (with
// the organisation's limit) and to a content-class counter of its
// workspace (counted only). A workspace without an organisation is an
// error, which refuses the upload rather than leaving it uncounted.
// ds:def id=orgdimensions-q7m2x9ka
func orgDimensions(_ context.Context, f records.File) ([]quota.Dimension, error) {
	org, ok := orgOf[f.Scope]
	if !ok {
		return nil, filesystem.Errorf(filesystem.CodePermission, "workspace %q has no organisation", f.Scope)
	}
	class, _, _ := strings.Cut(f.ContentType, "/")
	return []quota.Dimension{
		{Key: "org:" + org, Limit: orgPlan},
		{Key: "type:" + f.Scope + ":" + class},
	}, nil
}

// newOrgStore installs the quota plugin with WithDimensions. The
// workspaces themselves are unlimited: the organisation's counter is the
// cap.
// ds:def id=neworgstore-p4v8c2nd
func newOrgStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *quota.Plugin, error) {
	unlimited := func(context.Context, string) (quota.Limit, error) { return quota.Limit{}, nil }
	q, err := quota.New(unlimited, quota.WithDimensions(orgDimensions))
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, q, nil
}

// dimensionLine formats one counter for an admin page.
// ds:def id=dimensionline-h3w6t9rb
func dimensionLine(ctx context.Context, s *records.Store, q *quota.Plugin, key string) (string, error) {
	u, err := q.DimensionUsage(ctx, s, key)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: %d bytes, %d files", key, u.Bytes, u.Files), nil
}

// orgCounters runs the organisation-wide cap on a Store of its own (its
// own disk and database, so the first half's scopes, which have no
// organisation, are not recounted): two workspaces fill the organisation's
// 20 bytes together, a third upload passes the workspace's (unlimited)
// counter but not the organisation's, and RecountDimensions rebuilds the
// counters from the files.
func orgCounters(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "orgs.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	s, q, err := newOrgStore(ctx, db, disks)
	if err != nil {
		return err
	}
	if _, err := uploadWithRetry(ctx, s, northWS, "twelve bytes"); err != nil {
		return err
	}
	if _, err := uploadWithRetry(ctx, s, southWS, "six by"); err != nil {
		return err
	}
	_, err = uploadText(ctx, s, southWS, "five!")
	fmt.Fprintf(w, "org over its plan:     %s\n", filesystem.CodeOf(err))
	show := func() error {
		for _, key := range []string{"org:" + orgID, "type:" + northWS + ":text"} {
			line, err := dimensionLine(ctx, s, q, key)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "  %s\n", line)
		}
		return nil
	}
	if err := show(); err != nil {
		return err
	}
	// A manual repair lost the counters: RecountDimensions rebuilds them
	// from fs_files, asking orgDimensions again for every file.
	if _, err := s.Conn(ctx).ExecContext(ctx, "DELETE FROM "+quota.TableNameDimensionUsage); err != nil {
		return err
	}
	if err := q.RecountDimensions(ctx, s); err != nil {
		return err
	}
	fmt.Fprintln(w, "rows deleted, then RecountDimensions:")
	return show()
}
