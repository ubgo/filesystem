package fsent_test

import (
	"fmt"

	"entgo.io/ent"

	fsent "github.com/ubgo/filesystem/contrib/ent"
)

// File is an application's schema for fs_files: it mixes in FileMixin and
// adds the edge to its attachments. testdata/entcheck/ent/schema holds
// the same two schemas, which TestGenerateBuildAndRead generates, builds
// and runs a client from.
type File struct{ ent.Schema }

// Mixin implements ent.Interface.
func (File) Mixin() []ent.Mixin { return []ent.Mixin{fsent.FileMixin{}} }

// Edges implements ent.Interface.
// ds:def id=file-edges-x7tmb6k5
func (File) Edges() []ent.Edge { return []ent.Edge{fsent.FileAttachments(Attachment.Type)} }

// Attachment is an application's schema for fs_attachments.
type Attachment struct{ ent.Schema }

// Mixin implements ent.Interface.
func (Attachment) Mixin() []ent.Mixin { return []ent.Mixin{fsent.AttachmentMixin{}} }

// Edges implements ent.Interface.
// ds:def id=attachment-edges-5xhw4vbv
func (Attachment) Edges() []ent.Edge { return []ent.Edge{fsent.AttachmentFile(File.Type)} }

// Example lists what the two schemas read: each mixin's columns and the
// edge between them.
// ds:def id=example-7aumthe4
func Example() {
	for _, m := range []ent.Mixin{fsent.FileMixin{}, fsent.AttachmentMixin{}} {
		var cols []string
		for _, f := range m.Fields() {
			cols = append(cols, f.Descriptor().Name)
		}
		fmt.Println(len(cols), "columns:", cols)
	}
	e := Attachment{}.Edges()[0].Descriptor()
	fmt.Println("edge", e.Name, "on", e.Field, "unique:", e.Unique, "required:", e.Required)
	// Output:
	// 21 columns: [id scope disk storage_key filename content_type size sha256 visibility metadata created_at deleted_at uploaded_by status status_detail source_channel source_client source_ip expires_at view_once checksums]
	// 8 columns: [id scope file_id owner_type owner_id role position created_at]
	// edge file on file_id unique: true required: true
}
