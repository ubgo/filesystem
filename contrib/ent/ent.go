// Package fsent gives ent (https://entgo.io) schemas for the records
// tables, fs_files and fs_attachments, as mixins and edges, so an
// application that generates its data layer with ent reads files with the
// generated client and traverses from its own entities to them.
//
// An application declares two schemas of its own and mixes these in:
//
//	type File struct{ ent.Schema }
//	func (File) Mixin() []ent.Mixin { return []ent.Mixin{fsent.FileMixin{}} }
//	func (File) Edges() []ent.Edge  { return []ent.Edge{fsent.FileAttachments(Attachment.Type)} }
//
//	type Attachment struct{ ent.Schema }
//	func (Attachment) Mixin() []ent.Mixin { return []ent.Mixin{fsent.AttachmentMixin{}} }
//	func (Attachment) Edges() []ent.Edge  { return []ent.Edge{fsent.AttachmentFile(File.Type)} }
//
// Read only. Every write goes through records.Store, which runs plugin
// hooks, emits events and keeps stored objects consistent with their
// rows; the mixins install a hook that refuses every ent mutation of
// these tables with ErrReadOnly, so a generated Create or Delete fails
// instead of writing behind the Store's back. Never let ent migrate them
// either: do not run the generated client's Schema.Create against a
// database holding them, since the tables belong to Store.Migrate. The
// package's tests compare every field with the columns Store.Migrate
// creates, on every engine, and generate, build and run a client from
// these mixins.
//
// A Store built with records.WithTablePrefix or records.WithTableNames
// keeps its tables and indexes under other names; give both mixins the
// same mapping in Names (FileMixin{Names: names}) and they follow it. The
// zero value keeps the default names.
//
// The package name is fsent so it does not shadow entgo.io/ent.
package fsent

import (
	"context"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"entgo.io/ent/schema/mixin"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// ErrReadOnly is the error of every ent mutation of fs_files or
// fs_attachments: writes go through records.Store.
var ErrReadOnly = filesystem.Errorf(filesystem.CodePermission, "fsent: the records tables are written only through records.Store")

// Edge names, the names the generated client uses (QueryAttachments,
// QueryFile, WithAttachments, WithFile).
const (
	EdgeAttachments = "attachments"
	EdgeFile        = "file"
)

// mapName is the name of a records table or index under names, the
// mapping the Store was built with; nil keeps the default name.
func mapName(names records.TableNamer, name string) string {
	if names == nil {
		return name
	}
	return names(name)
}

// readOnly is the hook both mixins install.
func readOnly(ent.Mutator) ent.Mutator {
	return ent.MutateFunc(func(context.Context, ent.Mutation) (ent.Value, error) { return nil, ErrReadOnly })
}

// FileMixin makes a schema read fs_files: its table, every column with the
// Store's names and types, and the read-only hook. The ID is the Store's
// file ID (a string).
type FileMixin struct {
	mixin.Schema
	// Names is the records.TableNamer the Store was built with
	// (records.WithTableNames, or the prefix of records.WithTablePrefix),
	// applied to the table and index names; nil keeps the default names.
	// It must be the Store's mapping exactly, or the schema names a table
	// the Store did not create.
	Names records.TableNamer
}

// Fields of fs_files, in the Store's column order.
func (FileMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String(records.ColFileID).Immutable(),
		field.String(records.ColFileScope),
		field.String(records.ColFileDisk),
		field.String(records.ColFileKey),
		field.String(records.ColFileFilename),
		field.String(records.ColFileContentType),
		field.Int64(records.ColFileSize),
		field.String(records.ColFileSHA256),
		field.String(records.ColFileVisibility),
		// metadata is the JSON object records.File.Metadata is stored as.
		field.String(records.ColFileMetadata),
		field.Time(records.ColFileCreatedAt),
		field.Time(records.ColFileDeletedAt).Optional().Nillable(),
		field.String(records.ColFileUploadedBy),
		field.String(records.ColFileStatus),
		field.String(records.ColFileStatusDetail),
		field.String(records.ColFileSourceChannel),
		field.String(records.ColFileSourceClient),
		field.String(records.ColFileSourceIP),
		field.Time(records.ColFileExpiresAt).Optional().Nillable(),
		// view_once is non-zero for a view-once file: 1 before its first
		// view, 2 after.
		field.Int64(records.ColFileViewOnce),
		// checksums is the JSON object records.File.Checksums is stored
		// as, NULL when the file has none.
		field.String(records.ColFileChecksums).Optional().Nillable(),
	}
}

// Indexes mirror the Store's, for readers of the schema; ent never
// creates them (the Store does).
func (m FileMixin) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields(records.ColFileScope, records.ColFileCreatedAt, records.ColFileID).StorageKey(mapName(m.Names, "fs_files_scope_created")),
		index.Fields(records.ColFileScope, records.ColFileSHA256).StorageKey(mapName(m.Names, "fs_files_scope_sha256")),
		index.Fields(records.ColFileScope, records.ColFileStatus).StorageKey(mapName(m.Names, "fs_files_scope_status")),
		index.Fields(records.ColFileDisk, records.ColFileKey).StorageKey(mapName(m.Names, "fs_files_object")),
		index.Fields(records.ColFileExpiresAt).StorageKey(mapName(m.Names, "fs_files_expires")),
	}
}

// Annotations name the table.
func (m FileMixin) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: mapName(m.Names, records.TableNameFiles)}}
}

// Hooks refuse every mutation with ErrReadOnly.
func (FileMixin) Hooks() []ent.Hook { return []ent.Hook{readOnly} }

// AttachmentMixin makes a schema read fs_attachments, like FileMixin.
type AttachmentMixin struct {
	mixin.Schema
	// Names is the Store's table and index mapping, as in FileMixin.
	Names records.TableNamer
}

// Fields of fs_attachments, in the Store's column order. file_id is the
// field AttachmentFile's edge is built on.
func (AttachmentMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String(records.ColAttachmentID).Immutable(),
		field.String(records.ColAttachmentScope),
		field.String(records.ColAttachmentFileID).Immutable(),
		field.String(records.ColAttachmentOwnerType),
		field.String(records.ColAttachmentOwnerID),
		field.String(records.ColAttachmentRole),
		field.Int64(records.ColAttachmentPosition),
		field.Time(records.ColAttachmentCreatedAt),
	}
}

// Indexes mirror the Store's; ent never creates them.
func (m AttachmentMixin) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields(records.ColAttachmentScope, records.ColAttachmentOwnerType, records.ColAttachmentOwnerID, records.ColAttachmentRole, records.ColAttachmentFileID).
			Unique().StorageKey(mapName(m.Names, "fs_attachments_owner")),
		index.Fields(records.ColAttachmentFileID).StorageKey(mapName(m.Names, "fs_attachments_file")),
	}
}

// Annotations name the table.
func (m AttachmentMixin) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: mapName(m.Names, records.TableNameAttachments)}}
}

// Hooks refuse every mutation with ErrReadOnly.
func (AttachmentMixin) Hooks() []ent.Hook { return []ent.Hook{readOnly} }

// FileAttachments is the File schema's edge to its attachments; attachment
// is the application's Attachment schema type (Attachment.Type).
func FileAttachments(attachment any) ent.Edge {
	return edge.To(EdgeAttachments, attachment)
}

// AttachmentFile is the Attachment schema's edge to its file, on the
// file_id column; file is the application's File schema type (File.Type).
func AttachmentFile(file any) ent.Edge {
	return edge.From(EdgeFile, file).Ref(EdgeAttachments).Field(records.ColAttachmentFileID).Unique().Required().Immutable()
}
