package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"strings"

	"entgo.io/ent/dialect/entsql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/ncruces/go-sqlite3/gormlite"

	"github.com/ubgo/filesystem"
	fsent "github.com/ubgo/filesystem/contrib/ent"
	fsgorm "github.com/ubgo/filesystem/contrib/gorm"
	fssqlc "github.com/ubgo/filesystem/contrib/sqlc"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// tablePrefix is this app's house rule for table names: every records
// table and index is app_fs_… instead of fs_…, so a second Store can live
// beside the default one in the same database.
const tablePrefix = "app_"

// appNames is the mapping WithTablePrefix(tablePrefix) applies, given to
// the ORMs so they read the tables the prefixed Store writes.
func appNames(name string) string { return tablePrefix + name }

// defaultName matches a records name that kept its default fs_ form; a
// mapped app_fs_… name does not match, since "_" before "fs_" is part of
// the word.
var defaultName = regexp.MustCompile(`\bfs_`)

// openPrefixedStore opens a Store whose every table and index carries
// tablePrefix, and migrates it: app_fs_files, app_fs_attachments and the
// ledger app_fs_migrations, beside whatever fs_ tables the database has.
// ds:def id=openprefixedstore-g4t8w2nc
func openPrefixedStore(ctx context.Context, db *sql.DB) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, mgr, records.WithTablePrefix(tablePrefix))
	if err != nil {
		return nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// prefixedReaders gives each data layer the same mapping as the Store:
// GORM through fsgorm.NewNamer, sqlc through the mapped schema and
// queries it generates from, ent through the mixins' Names.
// ds:def id=prefixedreaders-u6m3x9kd
func prefixedReaders(db *sql.DB) (g *gorm.DB, sqlcQueries string, entTable string, err error) {
	g, err = gorm.Open(gormlite.OpenDB(db), &gorm.Config{Logger: logger.Discard, NamingStrategy: fsgorm.NewNamer(nil, appNames)})
	if err != nil {
		return nil, "", "", err
	}
	sqlcQueries, err = fssqlc.QueriesWith(fssqlc.EngineSQLite, appNames)
	if err != nil {
		return nil, "", "", err
	}
	for _, a := range (fsent.FileMixin{Names: appNames}).Annotations() {
		if t, ok := a.(entsql.Annotation); ok {
			entTable = t.Table
		}
	}
	return g, sqlcQueries, entTable, nil
}

// tablePrefixes runs a prefixed Store beside the default one in the same
// database and reads its rows with GORM, then shows the names sqlc and
// ent are given, and a mapping Migrate refuses.
func tablePrefixes(ctx context.Context, out io.Writer, db *sql.DB) error {
	store, err := openPrefixedStore(ctx, db)
	if err != nil {
		return err
	}
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}
	if _, err := store.Upload(ctx, tenant, diskName, strings.NewReader("prefixed"), "notes.txt", policy); err != nil {
		return err
	}
	fmt.Fprintf(out, "prefix: the Store writes %s\n", store.TableName(records.TableNameFiles))
	g, queries, entTable, err := prefixedReaders(db)
	if err != nil {
		return err
	}
	var mapped, plain []fsgorm.File
	if err := g.Scopes(fsgorm.InScope(tenant), fsgorm.Live).Find(&mapped).Error; err != nil {
		return err
	}
	unmapped, err := openGorm(db)
	if err != nil {
		return err
	}
	if err := unmapped.Scopes(fsgorm.InScope(tenant), fsgorm.Live).Find(&plain).Error; err != nil {
		return err
	}
	fmt.Fprintf(out, "prefix: gorm reads %d live file(s) from %s, the default namer %d from %s\n", len(mapped), store.TableName(records.TableNameFiles), len(plain), records.TableNameFiles)
	fmt.Fprintf(out, "prefix: sqlc queries name %s: %t, any fs_ name left: %t\n", appNames(records.TableNameFiles), strings.Contains(queries, appNames(records.TableNameFiles)), defaultName.MatchString(queries))
	fmt.Fprintf(out, "prefix: ent schema table %s\n", entTable)
	clash, err := records.New(db, sqlb.SQLiteDialect{}, store.Disks(), records.WithTableNames(func(string) string { return "files" }))
	if err != nil {
		return err
	}
	_, err = clash.Migrate(ctx)
	fmt.Fprintf(out, "prefix: a mapping giving two names one table: %s\n", filesystem.CodeOf(err))
	return nil
}

// entThenPrefixes is run's last two steps: the ent reader, then the
// prefixed Store beside the default one.
func entThenPrefixes(ctx context.Context, out io.Writer, db *sql.DB) error {
	if err := readWithEnt(ctx, out, db); err != nil {
		return err
	}
	return tablePrefixes(ctx, out, db)
}
