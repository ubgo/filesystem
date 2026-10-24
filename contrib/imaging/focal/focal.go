// Package focal is a records plugin that stores a focal point on a file
// (the face in a portrait, the product in a scene) as two fractions in its
// metadata, so variants declared with imaging.GravityFocal crop around it.
// Install it with records.WithPlugins and pass it to variants.WithFocal;
// changing a point emits EventChanged, on which the variants plugin drops
// the file's variants so they are made again around the new point.
//
// A point can also be set when a file is recorded, through
// records.NewFile.Metadata under MetaX and MetaY (an app that runs face
// detection at upload, say).
package focal

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"strconv"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// PluginName is the plugin's name.
const PluginName = "focal"

// Metadata keys of the point, as decimal fractions of the upright image.
const (
	MetaX = "focal_x"
	MetaY = "focal_y"
)

// EventChanged is emitted when a file's focal point is set or cleared.
const EventChanged records.EventKind = PluginName + ".changed"

// Keys of EventChanged's Detail: the new point, or empty strings when it
// was cleared.
const (
	DetailX = "x"
	DetailY = "y"
)

// Plugin stores focal points. It has no table and no migration.
type Plugin struct{}

// New returns the plugin.
func New() *Plugin { return &Plugin{} }

// Name implements records.Plugin.
func (*Plugin) Name() string { return PluginName }

// The plugin is a variants.FocalSource.
var _ variants.FocalSource = (*Plugin)(nil)

// Focal implements variants.FocalSource with Of.
func (*Plugin) Focal(f records.File) (imaging.Focal, bool) { return Of(f) }

// FocalChanged implements variants.FocalSource.
func (*Plugin) FocalChanged() records.EventKind { return EventChanged }

// Of returns f's focal point; ok is false when it has none or the stored
// values are not fractions within 0..1.
func Of(f records.File) (imaging.Focal, bool) {
	x, errX := strconv.ParseFloat(f.Metadata[MetaX], 64)
	y, errY := strconv.ParseFloat(f.Metadata[MetaY], 64)
	pt := imaging.Focal{X: x, Y: y}
	if errX != nil || errY != nil || !pt.Valid() {
		return imaging.Focal{}, false
	}
	return pt, true
}

// Set stores pt as the focal point of a live file, or clears it when pt
// is nil, and emits EventChanged with the updated file in the same
// transaction (joining the one in ctx, if any). Setting the point the file
// already has changes nothing and emits nothing. The file's row is locked
// first, so a concurrent replace or second Set cannot lose the other's
// metadata. A point outside 0..1 is CodeInvalidArgument; an unknown or
// trashed file is CodeNotFound.
func (p *Plugin) Set(ctx context.Context, s *records.Store, scope, fileID string, pt *imaging.Focal) (records.File, error) {
	if pt != nil && !pt.Valid() {
		return records.File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "focal: point %v is not within 0..1", *pt)
	}
	var out records.File
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		lock := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.SetExpr(records.FileKey, records.FileKey)).
			Where(sqlb.Eq(records.FileScope, scope), sqlb.Eq(records.FileID, fileID))
		if _, err := s.Exec(ctx, lock); err != nil {
			return err
		}
		f, err := s.File(ctx, scope, fileID)
		if err != nil {
			return err
		}
		if f.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeNotFound, "focal: file %q is in the trash", fileID)
		}
		out = f
		meta := maps.Clone(f.Metadata)
		if meta == nil {
			meta = map[string]string{}
		}
		detail := map[string]string{DetailX: "", DetailY: ""}
		delete(meta, MetaX)
		delete(meta, MetaY)
		if pt != nil {
			detail[DetailX] = strconv.FormatFloat(pt.X, 'g', -1, 64)
			detail[DetailY] = strconv.FormatFloat(pt.Y, 'g', -1, 64)
			meta[MetaX], meta[MetaY] = detail[DetailX], detail[DetailY]
		}
		if maps.Equal(meta, f.Metadata) { // nil and empty compare equal
			return nil
		}
		text, _ := json.Marshal(meta) // a map of strings cannot fail to marshal
		upd := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileMetadata, string(text))).
			Where(sqlb.Eq(records.FileScope, scope), sqlb.Eq(records.FileID, fileID))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
		if len(meta) == 0 {
			meta = nil
		}
		out.Metadata = meta
		return s.Emit(ctx, p, records.Event{Kind: EventChanged, Scope: scope, FileID: fileID, File: &out, Detail: detail})
	})
	if err != nil {
		return records.File{}, err
	}
	return out, nil
}
