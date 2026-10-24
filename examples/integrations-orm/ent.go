package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	fsent "github.com/ubgo/filesystem/contrib/ent"
	"github.com/ubgo/filesystem/examples/integrations-orm/ent"
	"github.com/ubgo/filesystem/examples/integrations-orm/ent/attachment"
	"github.com/ubgo/filesystem/examples/integrations-orm/ent/file"
	_ "github.com/ubgo/filesystem/examples/integrations-orm/ent/runtime" // registers the mixins' read-only hooks
)

// openEnt opens the generated ent client on the Store's own *sql.DB.
// Never run client.Schema.Create on it: the tables belong to
// Store.Migrate.
func openEnt(db *sql.DB) *ent.Client {
	return ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
}

// entGallery lists a post's gallery with the generated client: the
// attachments of the scope, owner and role in position order, each with
// its file, keeping only files not in the trash (Store.Attachments).
// ds:def id=entgallery-2fun6t4a
func entGallery(ctx context.Context, client *ent.Client, scope, owner string) ([]*ent.Attachment, error) {
	return client.Attachment.Query().
		Where(
			attachment.Scope(scope),
			attachment.OwnerType(ownerPost), attachment.OwnerID(owner), attachment.Role(roleGallery),
			attachment.HasFileWith(file.DeletedAtIsNil()),
		).
		Order(ent.Asc(attachment.FieldPosition), ent.Asc(attachment.FieldID)).
		WithFile().
		All(ctx)
}

// readWithEnt prints the gallery through ent and shows that the mixins
// refuse a write: a generated Delete fails with fsent.ErrReadOnly.
// ds:def id=readwithent-qkkbw8hc
func readWithEnt(ctx context.Context, out io.Writer, db *sql.DB) error {
	client := openEnt(db)
	gallery, err := entGallery(ctx, client, tenant, postID)
	if err != nil {
		return err
	}
	for _, a := range gallery {
		fmt.Fprintf(out, "ent gallery: %d %s %d bytes\n", a.Position, a.Edges.File.Filename, a.Edges.File.Size)
	}
	_, err = client.File.Delete().Where(file.Scope(tenant)).Exec(ctx)
	fmt.Fprintf(out, "ent delete refused: %t\n", errors.Is(err, fsent.ErrReadOnly))
	n, err := client.File.Query().Where(file.Scope(tenant)).Count(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ent rows after the refused delete: %d\n", n)
	return nil
}
