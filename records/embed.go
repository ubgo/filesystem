package records

import (
	"encoding/json"
	"time"

	"github.com/ubgo/filesystem"
)

// EmbedVersion is the version of the JSON Embed writes. Unembed reads
// every version up to it and refuses a newer one (written by a newer
// library), so an owner row is never read wrongly after a downgrade.
const EmbedVersion = 1

// embedded is the JSON form of a file on an owner row. Field names are
// part of the stored format: add fields, never rename one, and bump
// EmbedVersion when a field's meaning changes.
type embedded struct {
	V           int                   `json:"v"`
	ID          string                `json:"id"`
	Scope       string                `json:"scope"`
	Disk        string                `json:"disk"`
	Key         string                `json:"key"`
	Filename    string                `json:"filename"`
	ContentType string                `json:"content_type"`
	Size        int64                 `json:"size"`
	SHA256      string                `json:"sha256,omitempty"`
	Visibility  filesystem.Visibility `json:"visibility"`
	Metadata    map[string]string     `json:"metadata,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
}

// Embed returns f as JSON to store on the row that owns it (a user's
// avatar column, a post's cover), so a page can render the file (its
// URL through Store.URL, its name, size and type) without reading
// fs_files. It holds what a reader needs and carries EmbedVersion. The
// copy goes stale when the file changes: keep it in sync with an
// EventHooks plugin that re-embeds on file.replaced,
// file.visibility_changed, file.moved and file.released (whose release
// hooks may set metadata), and clears it on file.trashed and
// file.purged. fs_files stays the source of truth.
func Embed(f File) (json.RawMessage, error) {
	return json.Marshal(embedded{
		V: EmbedVersion, ID: f.ID, Scope: f.Scope, Disk: f.Disk, Key: f.Key, Filename: f.Filename, ContentType: f.ContentType,
		Size: f.Size, SHA256: f.SHA256, Visibility: f.Visibility, Metadata: f.Metadata, CreatedAt: f.CreatedAt.UTC(),
	})
}

// Unembed reads JSON written by Embed back into a File (only the fields
// Embed keeps). JSON that is not an embedded file, has no version, or has
// no file ID is CodeInvalidArgument; a version newer than EmbedVersion is
// CodeUnsupported.
func Unembed(raw json.RawMessage) (File, error) {
	var e embedded
	if err := json.Unmarshal(raw, &e); err != nil {
		return File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: not an embedded file: %v", err)
	}
	switch {
	case e.V < 1 || e.ID == "":
		return File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "records: an embedded file needs a version and an ID")
	case e.V > EmbedVersion:
		return File{}, filesystem.Errorf(filesystem.CodeUnsupported, "records: embedded file version %d is newer than %d", e.V, EmbedVersion)
	}
	return File{
		ID: e.ID, Scope: e.Scope, Disk: e.Disk, Key: e.Key, Filename: e.Filename, ContentType: e.ContentType,
		Size: e.Size, SHA256: e.SHA256, Visibility: e.Visibility, Metadata: e.Metadata, CreatedAt: e.CreatedAt.UTC(),
	}, nil
}
