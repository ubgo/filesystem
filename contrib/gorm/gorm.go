// Package fsgorm maps the records tables (fs_files, fs_attachments) to
// GORM models, so an application that reads its data with GORM can list,
// join and preload files without a second data layer.
//
// Read only. Every write goes through records.Store: it runs plugin hooks
// (quota, versions, folders), emits the events audit logs and webhooks
// hear, and keeps a stored object consistent with its row. Never Create,
// Save, Update or Delete these models, and never AutoMigrate them: the
// tables belong to Store.Migrate and its ledger, and AutoMigrate would
// alter columns the Store relies on. The package's tests compare every
// model field with the columns Store.Migrate creates, on every engine.
//
// A Store built with records.WithTablePrefix or records.WithTableNames
// keeps its tables under other names. Open GORM with
// NamingStrategy: fsgorm.NewNamer(base, names), names being the Store's
// records.TableNamer, and the models, preloads, joins and WithLiveFile
// name those tables; any other naming strategy keeps the default names.
//
// The package name is fsgorm so it does not shadow gorm.io/gorm.
package fsgorm

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// File is a row of fs_files. Field meanings are records.File's; Metadata
// holds the JSON object records.File.Metadata is stored as (Record
// decodes it). DeletedAt is a plain *time.Time, not gorm.DeletedAt, so
// GORM never turns a Delete into a soft delete behind the Store's back;
// filter trashed files with Live.
type File struct {
	ID           string     `gorm:"column:id;primaryKey"`
	Scope        string     `gorm:"column:scope"`
	Disk         string     `gorm:"column:disk"`
	StorageKey   string     `gorm:"column:storage_key"`
	Filename     string     `gorm:"column:filename"`
	ContentType  string     `gorm:"column:content_type"`
	Size         int64      `gorm:"column:size"`
	SHA256       string     `gorm:"column:sha256"`
	Visibility   string     `gorm:"column:visibility"`
	Metadata     string     `gorm:"column:metadata"`
	CreatedAt    time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	DeletedAt    *time.Time `gorm:"column:deleted_at"`
	UploadedBy   string     `gorm:"column:uploaded_by"`
	Status       string     `gorm:"column:status"`
	StatusDetail string     `gorm:"column:status_detail"`
	// SourceChannel, SourceClient and SourceIP are records.File.Source.
	SourceChannel string     `gorm:"column:source_channel"`
	SourceClient  string     `gorm:"column:source_client"`
	SourceIP      string     `gorm:"column:source_ip"`
	ExpiresAt     *time.Time `gorm:"column:expires_at"`
	// ViewOnce is non-zero for a view-once file (records.File.ViewOnce): 1
	// before its first view, 2 after.
	ViewOnce int64 `gorm:"column:view_once"`
	// Checksums holds the JSON object records.File.Checksums is stored
	// as; nil when the file has none.
	Checksums *string `gorm:"column:checksums"`
	// Attachments are the file's attachments, loaded with
	// Preload("Attachments").
	Attachments []Attachment `gorm:"foreignKey:FileID;references:ID"`
}

// TableName implements gorm's schema.TablerWithNamer:
// records.TableNameFiles, mapped by the records.TableNamer a Namer
// carries.
func (File) TableName(namer schema.Namer) string { return tableName(namer, records.TableNameFiles) }

// Record converts the row to a records.File, so the application can hand
// it to the Store (Store.URL, Store.DownloadURL). Metadata that is not a
// JSON object (a hand-edited row) is CodeInvalidArgument rather than
// silently dropped.
func (f File) Record() (records.File, error) {
	var meta map[string]string
	if f.Metadata != "" && f.Metadata != "{}" {
		if err := json.Unmarshal([]byte(f.Metadata), &meta); err != nil {
			return records.File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "fsgorm: file %q has metadata that is not a JSON object of strings", f.ID)
		}
	}
	var sums map[string]string
	if f.Checksums != nil {
		if err := json.Unmarshal([]byte(*f.Checksums), &sums); err != nil {
			return records.File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "fsgorm: file %q has checksums that are not a JSON object of strings", f.ID)
		}
	}
	return records.File{
		ID: f.ID, Scope: f.Scope, Disk: f.Disk, Key: f.StorageKey, Filename: f.Filename, ContentType: f.ContentType,
		Size: f.Size, SHA256: f.SHA256, Visibility: filesystem.Visibility(f.Visibility), Metadata: meta,
		CreatedAt: f.CreatedAt.UTC(), DeletedAt: utc(f.DeletedAt), UploadedBy: f.UploadedBy,
		Status: records.Status(f.Status), StatusDetail: f.StatusDetail,
		Source:    records.Source{Channel: f.SourceChannel, Client: f.SourceClient, IP: f.SourceIP},
		ExpiresAt: utc(f.ExpiresAt), ViewOnce: f.ViewOnce != 0, Checksums: sums,
	}, nil
}

// Attachment is a row of fs_attachments. File is the attached file,
// loaded with Preload("File").
type Attachment struct {
	ID        string    `gorm:"column:id;primaryKey"`
	Scope     string    `gorm:"column:scope"`
	FileID    string    `gorm:"column:file_id"`
	OwnerType string    `gorm:"column:owner_type"`
	OwnerID   string    `gorm:"column:owner_id"`
	Role      string    `gorm:"column:role"`
	Position  int64     `gorm:"column:position"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime:false"`
	File      *File     `gorm:"foreignKey:FileID;references:ID"`
}

// TableName implements gorm's schema.TablerWithNamer:
// records.TableNameAttachments, mapped by the records.TableNamer a Namer
// carries.
func (Attachment) TableName(namer schema.Namer) string {
	return tableName(namer, records.TableNameAttachments)
}

// Namer is a GORM naming strategy that names the records tables as a
// Store built with records.WithTableNames or records.WithTablePrefix
// does, and names every other table as the application's own strategy
// does. Set it as gorm.Config.NamingStrategy. GORM caches a model's
// schema per *gorm.DB, so two databases opened with different Namers
// keep their own names.
//
// Invariant: the records tables never take the base strategy's
// TablePrefix or pluralisation, as before; only the TableNamer maps them.
type Namer struct {
	base  schema.Namer
	names records.TableNamer
}

// NewNamer returns a Namer over base (schema.NamingStrategy{} when nil)
// mapping the records tables with names, the Store's mapping (nil keeps
// the default names).
func NewNamer(base schema.Namer, names records.TableNamer) Namer {
	if base == nil {
		base = schema.NamingStrategy{}
	}
	return Namer{base: base, names: names}
}

// tableName is name mapped by the TableNamer namer carries, or name for
// any other naming strategy.
func tableName(namer schema.Namer, name string) string {
	if n, ok := namer.(Namer); ok && n.names != nil {
		return n.names(name)
	}
	return name
}

// The methods of schema.Namer, delegated to the base strategy.

// TableName names a model of the application's own.
func (n Namer) TableName(table string) string { return n.strategy().TableName(table) }

// SchemaName delegates to the base strategy.
func (n Namer) SchemaName(table string) string { return n.strategy().SchemaName(table) }

// ColumnName delegates to the base strategy.
func (n Namer) ColumnName(table, column string) string {
	return n.strategy().ColumnName(table, column)
}

// JoinTableName delegates to the base strategy.
func (n Namer) JoinTableName(joinTable string) string {
	return n.strategy().JoinTableName(joinTable)
}

// RelationshipFKName delegates to the base strategy.
func (n Namer) RelationshipFKName(rel schema.Relationship) string {
	return n.strategy().RelationshipFKName(rel)
}

// CheckerName delegates to the base strategy.
func (n Namer) CheckerName(table, column string) string {
	return n.strategy().CheckerName(table, column)
}

// IndexName delegates to the base strategy.
func (n Namer) IndexName(table, column string) string { return n.strategy().IndexName(table, column) }

// UniqueName delegates to the base strategy.
func (n Namer) UniqueName(table, column string) string {
	return n.strategy().UniqueName(table, column)
}

// strategy is the base strategy; a zero Namer (not built with NewNamer)
// behaves as schema.NamingStrategy{}.
func (n Namer) strategy() schema.Namer {
	if n.base == nil {
		return schema.NamingStrategy{}
	}
	return n.base
}

// col names a column of the query's own table (clause.CurrentTable), so
// the scopes below stay unambiguous in a query that joins the other
// fs_ table.
func col(name string) clause.Column { return clause.Column{Table: clause.CurrentTable, Name: name} }

// InScope keeps the rows of one scope. Every query over these tables
// should start with it, as every Store query does: without it a listing
// crosses tenants. It works on File and Attachment queries alike.
func InScope(scope string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where(clause.Eq{Column: col(records.ColFileScope), Value: scope})
	}
}

// Live keeps files that are not in the trash, as Store.Files does. Use it
// on File queries, and in Preload("File", Live).
func Live(db *gorm.DB) *gorm.DB {
	return db.Where(clause.Eq{Column: col(records.ColFileDeletedAt), Value: nil})
}

// NewestFirst orders files as Store.Files does, newest first with the ID
// breaking ties, so pages are stable.
func NewestFirst(db *gorm.DB) *gorm.DB {
	return db.Order(clause.OrderByColumn{Column: col(records.ColFileCreatedAt), Desc: true}).
		Order(clause.OrderByColumn{Column: col(records.ColFileID), Desc: true})
}

// ByPosition orders attachments as Store.Attachments does.
func ByPosition(db *gorm.DB) *gorm.DB {
	return db.Order(clause.OrderByColumn{Column: col(records.ColAttachmentPosition)}).
		Order(clause.OrderByColumn{Column: col(records.ColAttachmentID)})
}

// WithLiveFile keeps attachments whose file is not in the trash, as
// Store.Attachments does, with an EXISTS on fs_files (a join would add the
// file's columns to the attachment's row). Use it on Attachment queries;
// Preload("File") still loads the file itself.
func WithLiveFile(db *gorm.DB) *gorm.DB {
	f, a := tableName(db.NamingStrategy, records.TableNameFiles), tableName(db.NamingStrategy, records.TableNameAttachments)
	return db.Where("EXISTS (SELECT 1 FROM " + f + " WHERE " + f + "." + records.ColFileID + " = " + a + "." + records.ColAttachmentFileID +
		" AND " + f + "." + records.ColFileScope + " = " + a + "." + records.ColAttachmentScope +
		" AND " + f + "." + records.ColFileDeletedAt + " IS NULL)")
}

// utc returns t in UTC, or nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
