package main

import (
	"database/sql"
	"fmt"
	"io"

	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	fsgorm "github.com/ubgo/filesystem/contrib/gorm"
)

// openGorm opens GORM on the Store's own *sql.DB, so both see the same
// rows and transactions are not split across pools. The module imports
// no dialector; the app picks its own (gormlite here, gorm.io/driver/
// postgres or mysql elsewhere, each over the same *sql.DB). Never call
// AutoMigrate on the fsgorm models: the tables belong to Store.Migrate.
// ds:def id=opengorm-me6qbh5f
func openGorm(db *sql.DB) (*gorm.DB, error) {
	return gorm.Open(gormlite.OpenDB(db), &gorm.Config{Logger: logger.Discard})
}

// gormGallery lists a post's gallery with GORM: the attachments in
// position order, each with its file, leaving trashed files out as
// Store.Attachments does. InScope comes first, or the query crosses
// tenants.
// ds:def id=gormgallery-27wg6mmp
func gormGallery(db *gorm.DB, scope, owner string) ([]fsgorm.Attachment, error) {
	var out []fsgorm.Attachment
	err := db.Scopes(fsgorm.InScope(scope), fsgorm.WithLiveFile, fsgorm.ByPosition).
		Where(&fsgorm.Attachment{OwnerType: ownerPost, OwnerID: owner, Role: roleGallery}).
		Preload("File").Find(&out).Error
	return out, err
}

// gormLiveFiles lists a scope's live files newest first, each with its
// attachments preloaded: the order of Store.Files.
func gormLiveFiles(db *gorm.DB, scope string) ([]fsgorm.File, error) {
	var out []fsgorm.File
	err := db.Scopes(fsgorm.InScope(scope), fsgorm.Live, fsgorm.NewestFirst).
		Preload("Attachments").Find(&out).Error
	return out, err
}

// readWithGorm prints the gallery and the live files through GORM, and
// hands one row back to the records types with File.Record.
// ds:def id=readwithgorm-fgmhqteb
func readWithGorm(out io.Writer, db *sql.DB) error {
	g, err := openGorm(db)
	if err != nil {
		return err
	}
	gallery, err := gormGallery(g, tenant, postID)
	if err != nil {
		return err
	}
	for _, a := range gallery {
		fmt.Fprintf(out, "gorm gallery: %d %s %d bytes\n", a.Position, a.File.Filename, a.File.Size)
	}
	files, err := gormLiveFiles(g, tenant)
	if err != nil {
		return err
	}
	for _, f := range files {
		rec, err := f.Record()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "gorm live file: %s, %d attachment(s), records.File status %s\n", rec.Filename, len(f.Attachments), rec.Status)
	}
	return nil
}
