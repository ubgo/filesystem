// Command entcheck reads, with the client ent generated from the fsent
// mixins, a SQLite database the records Store wrote, and tries one write.
// The test in contrib/ent runs it and compares its output with the Store.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"

	"entcheck/ent"
	"entcheck/ent/attachment"
	"entcheck/ent/file"
	_ "entcheck/ent/runtime" // registers the mixins' read-only hooks

	fsent "github.com/ubgo/filesystem/contrib/ent"
)

func main() {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	files, err := client.File.Query().
		Where(file.Scope("t"), file.DeletedAtIsNil()).
		Order(ent.Desc(file.FieldCreatedAt), ent.Desc(file.FieldID)).
		WithAttachments().All(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range files {
		fmt.Printf("file %s %s %d %s %d\n", f.ID, f.Filename, f.Size, f.Sha256, len(f.Edges.Attachments))
	}
	atts, err := client.Attachment.Query().
		Where(attachment.Scope("t"), attachment.OwnerType("post"), attachment.OwnerID("p1"), attachment.Role("gallery")).
		Order(ent.Asc(attachment.FieldPosition), ent.Asc(attachment.FieldID)).
		WithFile().All(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, a := range atts {
		fmt.Printf("attachment %s %d %s\n", a.ID, a.Position, a.Edges.File.ID)
	}
	_, err = client.File.Delete().Where(file.Scope("t")).Exec(ctx)
	fmt.Println("delete refused:", errors.Is(err, fsent.ErrReadOnly))
}
