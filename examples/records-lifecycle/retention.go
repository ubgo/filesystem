package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/versions"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions about the second half, not the library's.
const (
	// ownerChat is the owner type chat messages attach files to.
	ownerChat = "chat"
	// chatRetention is how long chat attachments are kept.
	chatRetention = 90 * 24 * time.Hour
	// storyLife is how long a story is visible.
	storyLife = 24 * time.Hour
)

// textPolicy accepts plain text with a random key.
var textPolicy = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("t")}

// newRetentionStore declares how long chat attachments are kept, and
// keeps old versions of replaced files (the versions plugin), to show a
// takedown removing them with the file.
// ds:def id=newretentionstore-k4m9w2tc
func newRetentionStore(ctx context.Context, db *sql.DB, clock *Clock) (*records.Store, *versions.Plugin, error) {
	disks, err := newDisks()
	if err != nil {
		return nil, nil, err
	}
	vp, err := versions.New()
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser, ownerChat),
		records.WithClock(clock.Now),
		records.WithRetention(records.Retention{OwnerType: ownerChat, MaxAge: chatRetention}),
		records.WithPlugins(vp),
	)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, vp, nil
}

// applyRetention previews the sweep, then runs it. The dry run returns
// exactly what a real run would do and changes nothing: log or show it
// before switching a new rule on.
// ds:def id=applyretention-p7v3n8qx
func applyRetention(ctx context.Context, s *records.Store, now time.Time) (preview, done records.RetentionReport, err error) {
	if preview, err = s.SweepRetention(ctx, now, true); err != nil {
		return preview, done, err
	}
	done, err = s.SweepRetention(ctx, now, false)
	return preview, done, err
}

// postStory uploads a file that disappears after storyLife: Viewed
// refuses it once expired, and SweepExpired purges it.
// ds:def id=poststory-w2c8t5hm
func postStory(ctx context.Context, s *records.Store, now time.Time) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader("sunset"), "story.txt", textPolicy, records.Expires(now.Add(storyLife)))
}

// sendViewOnce uploads a file that can be viewed exactly once.
// ds:def id=sendviewonce-r6h2k9vc
func sendViewOnce(ctx context.Context, s *records.Store) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader("secret"), "once.txt", textPolicy, records.ViewOnce())
}

// serve is what a route does before it hands out a temporary or
// view-once file: ask Viewed, which counts the view, and refuse on false.
// ds:def id=serve-n3q7w4kz
func serve(ctx context.Context, s *records.Store, id string) string {
	ok, err := s.Viewed(ctx, tenant, id)
	switch {
	case err != nil:
		return string(filesystem.CodeOf(err))
	case !ok:
		return "404"
	}
	return "200"
}

// takedown removes a file for good on a legal or abuse request: Purge
// runs every plugin's BeforePurge in the same transaction, so its kept
// versions (and image variants, shares, grants, locks) go with it, and the
// objects are deleted after the commit.
// ds:def id=takedown-x5t9c2mw
func takedown(ctx context.Context, s *records.Store, id string) error {
	return s.Purge(records.WithActor(ctx, "admin:trust-and-safety"), tenant, id)
}

// retentionDemo runs retention, temporary and view-once files and a
// takedown on a second Store over the same database.
func retentionDemo(ctx context.Context, w io.Writer, db *sql.DB, clock *Clock) error {
	s, vp, err := newRetentionStore(ctx, db, clock)
	if err != nil {
		return err
	}
	old, err := s.Upload(ctx, tenant, diskName, strings.NewReader("old chat photo"), "old.txt", textPolicy)
	if err != nil {
		return err
	}
	if _, err := s.Attach(ctx, tenant, old.ID, records.Owner{Type: ownerChat, ID: "c1"}, "attachment"); err != nil {
		return err
	}
	shared, err := s.Upload(ctx, tenant, diskName, strings.NewReader("logo"), "logo.txt", textPolicy)
	if err != nil {
		return err
	}
	for _, o := range []records.Owner{{Type: ownerChat, ID: "c1"}, {Type: ownerUser, ID: "u1"}} {
		if _, err := s.Attach(ctx, tenant, shared.ID, o, "attachment"); err != nil {
			return err
		}
	}
	later := clock.Now().Add(chatRetention + time.Hour)
	preview, done, err := applyRetention(ctx, s, later)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "retention dry run: detach %d, purge %d; real run: detached %d, purged %d\n",
		len(preview.Detached), len(preview.Purged), len(done.Detached), len(done.Purged))
	_, errOld := s.File(ctx, tenant, old.ID)
	_, errShared := s.File(ctx, tenant, shared.ID)
	fmt.Fprintf(w, "old chat photo: %s; logo still used by a user: %t\n", filesystem.CodeOf(errOld), errShared == nil)

	story, err := postStory(ctx, s, clock.Now())
	if err != nil {
		return err
	}
	once, err := sendViewOnce(ctx, s)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "story now: %s; view-once first: %s, second: %s\n", serve(ctx, s, story.ID), serve(ctx, s, once.ID), serve(ctx, s, once.ID))
	n, err := s.SweepExpired(ctx, clock.Now().Add(storyLife))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "a day later, SweepExpired purged %d (the story and the viewed note)\n", n)

	doc, err := s.Upload(ctx, tenant, diskName, strings.NewReader("v1"), "leak.txt", textPolicy)
	if err != nil {
		return err
	}
	if _, err := s.Replace(ctx, tenant, doc.ID, strings.NewReader("v2"), "", textPolicy); err != nil {
		return err
	}
	kept, err := vp.Versions(ctx, s, tenant, doc.ID)
	if err != nil {
		return err
	}
	if err := takedown(ctx, s, doc.ID); err != nil {
		return err
	}
	left, err := vp.Versions(ctx, s, tenant, doc.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "takedown: %d kept version(s) before, %d after\n", len(kept), len(left))
	return nil
}
