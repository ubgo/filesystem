package fsgorm_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ncruces/go-sqlite3/gormlite"
	_ "modernc.org/sqlite"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/ubgo/filesystem"
	fsgorm "github.com/ubgo/filesystem/contrib/gorm"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// galleryOf lists a post's gallery with GORM: the attachments in position
// order, each with its file, leaving trashed files out as
// Store.Attachments does. Writes went through the Store; this only reads.
// ds:def id=galleryof-8cd932hh
func galleryOf(db *gorm.DB, scope, postID string) ([]fsgorm.Attachment, error) {
	var out []fsgorm.Attachment
	err := db.Scopes(fsgorm.InScope(scope), fsgorm.WithLiveFile, fsgorm.ByPosition).
		Where(&fsgorm.Attachment{OwnerType: "post", OwnerID: postID, Role: "gallery"}).
		Preload("File").Find(&out).Error
	return out, err
}

// Example shows a Store writing files and GORM reading them over the same
// database: the app's own GORM models can join and preload fs_ rows.
func Example() {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1) // one in-memory database for both
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	s, err := records.New(db, sqlb.SQLiteDialect{}, mgr, records.WithOwnerTypes("post"))
	if err != nil {
		panic(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		panic(err)
	}
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}
	post := records.Owner{Type: "post", ID: "p1"}
	for _, name := range []string{"cover.txt", "draft.txt", "map.txt"} {
		f, err := s.Upload(ctx, "acme", "media", strings.NewReader(name), name, policy)
		if err != nil {
			panic(err)
		}
		if _, err := s.Attach(ctx, "acme", f.ID, post, "gallery"); err != nil {
			panic(err)
		}
		if name == "draft.txt" {
			if err := s.Trash(ctx, "acme", f.ID); err != nil { // left out of the gallery
				panic(err)
			}
		}
	}

	g, err := gorm.Open(gormlite.OpenDB(db), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		panic(err)
	}
	gallery, err := galleryOf(g, "acme", "p1")
	if err != nil {
		panic(err)
	}
	for _, a := range gallery {
		fmt.Println(a.Position, a.File.Filename, a.File.Size, "bytes")
	}
	// Output:
	// 1 cover.txt 9 bytes
	// 3 map.txt 7 bytes
}
