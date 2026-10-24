// Command records-quota-seats shows per-uploader limits of the quota
// plugin: a team workspace on a 100-byte plan where each member's seat
// may store 40 bytes, so one member cannot fill the workspace for
// everyone. It prints each member's usage beside the workspace's, a
// refusal that names the member, a replacement charged to the file's
// uploader rather than to whoever replaced it, and a recount.
//
//	go run ./records-quota-seats
//
// main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk files are stored on.
	diskName = "media"
	// team is the workspace, the records scope.
	team = "team-acme"
	// alice and bob are members, recorded as File.UploadedBy.
	alice = "user:alice"
	bob   = "user:bob"
	// maxUpload caps one upload.
	maxUpload = 1 << 20
)

// Plans of the demo app. Zero is unlimited.
var (
	// teamPlan is what the whole workspace may store.
	teamPlan = quota.Limit{Bytes: 100}
	// seatPlan is what each member may store within it.
	seatPlan = quota.Limit{Bytes: 40, Files: 5}
)

// workspaceLimit is the quota.LimitFunc: the workspace's plan.
func workspaceLimit(context.Context, string) (quota.Limit, error) { return teamPlan, nil }

// seatLimit is the quota.UploaderLimitFunc: every member gets a seat;
// uploads made without an actor (a background import) are not limited
// per uploader, only by the workspace.
// ds:def id=seatlimit-r3vq8w2m
func seatLimit(_ context.Context, _, uploader string) (quota.Limit, error) {
	if uploader == "" {
		return quota.Limit{}, nil
	}
	return seatPlan, nil
}

var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("files")}

// newStore installs the quota plugin with per-uploader limits and creates
// every table.
// ds:def id=newstore-seats-6m2kq9xa
func newStore(ctx context.Context, db *sql.DB) (*records.Store, *quota.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	q, err := quota.New(workspaceLimit, quota.WithUploaderLimits(seatLimit))
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

// uploadAs stores body as member's file: the actor in the context becomes
// File.UploadedBy, which is the uploader the quota charges.
func uploadAs(ctx context.Context, s *records.Store, member, body string) (records.File, error) {
	return s.Upload(records.WithActor(ctx, member), team, diskName, strings.NewReader(body), "note.txt", textPolicy)
}

// seatUsage is the line a members page shows: each member's usage of
// their seat, and the workspace's of its plan.
// ds:def id=seatusage-p4n7c2tz
func seatUsage(ctx context.Context, s *records.Store, q *quota.Plugin) (string, error) {
	var parts []string
	for _, m := range []string{alice, bob} {
		u, err := q.UploaderUsage(ctx, s, team, m)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s %d/%d", strings.TrimPrefix(m, "user:"), u.Bytes, seatPlan.Bytes))
	}
	u, err := q.Usage(ctx, s, team)
	if err != nil {
		return "", err
	}
	return strings.Join(parts, ", ") + fmt.Sprintf(", team %d/%d bytes", u.Bytes, teamPlan.Bytes), nil
}

func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	s, q, err := newStore(ctx, db)
	if err != nil {
		return err
	}
	show := func(step string) error {
		line, err := seatUsage(ctx, s, q)
		fmt.Fprintf(w, "%-24s %s\n", step+":", line)
		return err
	}
	a, err := uploadAs(ctx, s, alice, strings.Repeat("a", 30))
	if err != nil {
		return err
	}
	if _, err := uploadAs(ctx, s, bob, strings.Repeat("b", 25)); err != nil {
		return err
	}
	if err := show("two uploads"); err != nil {
		return err
	}
	_, err = uploadAs(ctx, s, alice, strings.Repeat("a", 15))
	fmt.Fprintf(w, "alice past her seat:     %v\n", err)
	// Bob edits Alice's file: the growth is charged to Alice, the uploader.
	if _, err := s.Replace(records.WithActor(ctx, bob), team, a.ID, strings.NewReader(strings.Repeat("e", 38)), "", textPolicy); err != nil {
		return err
	}
	if err := show("bob grew alice's file"); err != nil {
		return err
	}
	u, err := q.Recount(ctx, s, team)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "recount:                 team %d bytes, %d files\n", u.Bytes, u.Files)
	return show("after the recount")
}

func main() {
	dir, err := os.MkdirTemp("", "records-quota-seats")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
