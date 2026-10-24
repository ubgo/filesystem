// Package schema is what an application writes to read the records
// tables with ent: two schemas of its own that mix in fsent. The test in
// contrib/ent generates a client from it, builds it and runs main.go.
package schema

import (
	"entgo.io/ent"

	fsent "github.com/ubgo/filesystem/contrib/ent"
)

// File reads fs_files. The fields come from fsent.FileMixin rather than
// being written out here, so the schema follows the records table when a
// release adds a column.
// ds:def id=schemas-fjjwqp9j span=+15
type File struct{ ent.Schema }

// Mixin implements ent.Interface.
func (File) Mixin() []ent.Mixin { return []ent.Mixin{fsent.FileMixin{}} }

// Edges implements ent.Interface.
func (File) Edges() []ent.Edge { return []ent.Edge{fsent.FileAttachments(Attachment.Type)} }

// Attachment reads fs_attachments, with its fields from
// fsent.AttachmentMixin for the same reason as File.
type Attachment struct{ ent.Schema }

// Mixin implements ent.Interface.
func (Attachment) Mixin() []ent.Mixin { return []ent.Mixin{fsent.AttachmentMixin{}} }

// Edges implements ent.Interface.
func (Attachment) Edges() []ent.Edge { return []ent.Edge{fsent.AttachmentFile(File.Type)} }
