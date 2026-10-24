package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of the second half of the demo, not of the library.
const (
	// ownerPost is the owner type files are attached to in the usage demo.
	ownerPost = "post"
	// roleCover is the role of a post's cover image.
	roleCover = "cover"
	// blobDir is where the content-addressed policy stores its objects.
	blobDir = "blobs"
	// opaqueIDBytes is how many random bytes the custom ID generator
	// appends to the prefix: 16 bytes, as many as the default generator.
	opaqueIDBytes = 16
)

// opaqueID is an ID generator for records.WithIDs: the prefix the Store
// asks for (fil_, att_) and 32 random hex characters. A real app might
// plug in github.com/ubgo/publicid or UUIDs instead; the IDs only have to
// be unique, start with the prefix and fit in 191 bytes.
// ds:def id=opaqueid-r8kz2m4q
func opaqueID(prefix string) string {
	b := make([]byte, opaqueIDBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return prefix + hex.EncodeToString(b)
}

// newFullStore is newStore with the options the second half needs: the
// custom ID generator, the owner types files may be attached to, and
// WithSourceIP, which keeps the client IP of each upload (personal data,
// so off unless the app asks).
// ds:def id=newfullstore-p3v7hx2c
func newFullStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithIDs(opaqueID),
		records.WithOwnerTypes(ownerPost),
		records.WithSourceIP(),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// requestContext is what an app's middleware does once per request: name
// the actor (File.UploadedBy) and where the request came from
// (File.Source). The library stores the values and never interprets them.
// ds:def id=requestcontext-b6wq9n3t
func requestContext(ctx context.Context, user, ip string) context.Context {
	ctx = records.WithActor(ctx, user)
	return records.WithSource(ctx, records.Source{Channel: "ios", Client: "app/3.2", IP: ip})
}

// describe sets the alt text, caption and title of a file. All three are
// replaced; "" removes one. The rest of the metadata is kept.
// ds:def id=describe-f4nq8w2z
func describe(ctx context.Context, s *records.Store, f records.File) (records.File, error) {
	return s.SetText(ctx, f.Scope, f.ID, records.Text{
		Alt:     "Bar chart of revenue against costs",
		Caption: "Q3 revenue.\nSource: finance export.",
		Title:   "Q3 report",
	})
}

// uploadDedup uploads body with records.Dedup: when a live file of the
// same scope, disk and visibility already has the same SHA-256, the new
// file is recorded on that file's object and the copy just uploaded is
// deleted after the commit.
// ds:def id=uploaddedup-x2m9c7kd
func uploadDedup(ctx context.Context, s *records.Store, filename, body string) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader(body), filename, notesPolicy, records.Dedup())
}

// tenantFromContext is the owner of a content-addressed upload: here the
// fixed demo tenant; a real app reads it from its authentication.
func tenantFromContext(context.Context) (string, error) { return tenant, nil }

// blobPolicy names each upload by its content: "blobs/<owner digest>/
// <sha256>.txt". Identical uploads of one owner share one key, and a key
// proves its content. The disk is deduplicated even without Dedup.
// ds:def id=blobpolicy-k7t3w9qe
func blobPolicy() upload.Policy {
	p := upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}}
	p.Key, p.Settle = upload.ContentAddressed(blobDir, upload.ContentAddressing{Owner: tenantFromContext})
	return p
}

// errInUse is purgeIfUnused's refusal: the file is still attached.
var errInUse = filesystem.Errorf(filesystem.CodeConflict, "the file is still used")

// purgeIfUnused deletes a file for good only when nothing uses it:
// UsagesOf lists its attachments (owner and role), newest first, so one
// row is enough to know. Purge itself would delete the attachments with
// the file; this check is the app's "used in 3 posts, detach first".
// ds:def id=purgeifunused-h9c4r2vn
func purgeIfUnused(ctx context.Context, s *records.Store, f records.File) error {
	used, err := s.UsagesOf(ctx, records.UsageQuery{Scope: f.Scope, FileID: f.ID, Limit: 1})
	if err != nil {
		return err
	}
	if len(used.Usages) > 0 {
		a := used.Usages[0]
		return fmt.Errorf("%w: %s %s as %s", errInUse, a.Owner.Type, a.Owner.ID, a.Role)
	}
	return s.Purge(ctx, f.Scope, f.ID)
}

// embedOnPost is the JSON a post row would keep in its cover column, so a
// feed renders the cover without reading fs_files; Unembed reads it back.
// ds:def id=embedonpost-q5j8m2xw
func embedOnPost(f records.File) (records.File, json.RawMessage, error) {
	raw, err := records.Embed(f)
	if err != nil {
		return records.File{}, nil, err
	}
	back, err := records.Unembed(raw)
	return back, raw, err
}

// extras runs the second half of the demo on a Store made by newFullStore.
func extras(ctx context.Context, w io.Writer, s *records.Store) error {
	ctx = requestContext(ctx, "user:bob", "203.0.113.7")
	report, err := recordExisting(ctx, s, "exports/q4.txt", []byte("revenue,costs\n12,5\n"))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "id %s… (%d chars) key %s name %q\n", report.ID[:4], len(report.ID), report.Key, report.Filename)
	fmt.Fprintf(w, "uploaded by %s via %s %s from %s, created_at set %t\n", report.UploadedBy, report.Source.Channel, report.Source.Client,
		report.Source.IP, !report.CreatedAt.IsZero())
	described, err := describe(ctx, s, report)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "alt %q title %q source %q\n", described.Text().Alt, described.Text().Title, described.Metadata["source"])
	if err := dedupDemo(ctx, w, s); err != nil {
		return err
	}
	if err := usageDemo(ctx, w, s, described); err != nil {
		return err
	}
	back, raw, err := embedOnPost(described)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "embedded %d bytes, v1=%t, back %q\n", len(raw), strings.HasPrefix(string(raw), `{"v":1,`), back.Filename)
	return nil
}

// dedupDemo uploads the same body twice with Dedup and twice under the
// content-addressed policy, and counts the references to each object.
func dedupDemo(ctx context.Context, w io.Writer, s *records.Store) error {
	a, err := uploadDedup(ctx, s, "a.txt", "same bytes")
	if err != nil {
		return err
	}
	b, err := uploadDedup(ctx, s, "b.txt", "same bytes")
	if err != nil {
		return err
	}
	refs, err := s.References(ctx, diskName, a.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "dedup: two rows, same key %t, %d references\n", a.ID != b.ID && a.Key == b.Key, refs)
	if err := s.Purge(ctx, tenant, a.ID); err != nil {
		return err
	}
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return err
	}
	kept, err := disk.Exists(ctx, b.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "purged one: object kept %t\n", kept)
	c, err := s.Upload(ctx, tenant, diskName, strings.NewReader("hello"), "c.txt", blobPolicy())
	if err != nil {
		return err
	}
	d, err := s.Upload(ctx, tenant, diskName, strings.NewReader("hello"), "d.txt", blobPolicy())
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "content key under %s/: %t, shared %t\n", blobDir, strings.HasPrefix(c.Key, blobDir+"/"), c.Key == d.Key)
	return nil
}

// usageDemo attaches a file to a post, shows the refusal to purge it, and
// purges it once detached.
func usageDemo(ctx context.Context, w io.Writer, s *records.Store, f records.File) error {
	att, err := s.Attach(ctx, f.Scope, f.ID, records.Owner{Type: ownerPost, ID: "42"}, roleCover)
	if err != nil {
		return err
	}
	if err := purgeIfUnused(ctx, s, f); err != nil {
		fmt.Fprintf(w, "purge refused: %v (%s)\n", err, filesystem.CodeOf(err))
	}
	if err := s.Detach(ctx, f.Scope, att.ID); err != nil {
		return err
	}
	if err := purgeIfUnused(ctx, s, f); err != nil {
		return err
	}
	fmt.Fprintln(w, "detached, then purged")
	return nil
}
