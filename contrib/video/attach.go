package video

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"maps"
	"regexp"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// HLSLadder names an HLS ladder made outside the upload's transaction
// (in a job, by Tool.HLS or EncodeHLS), for AttachHLS: where it is and
// which content of the file it was encoded from.
type HLSLadder struct {
	// Disk is the name of the disk holding it, and Dir its directory with
	// the trailing slash: NewHLSLadder's answer, so a fresh directory
	// under the prefix the plugin's Location gives the file.
	Disk, Dir string
	// SourceSHA256 is the SHA-256 the file's row had when the ladder's
	// source was read (records.File.SHA256); AttachHLS refuses the ladder
	// if the file's content was replaced since.
	SourceSHA256 string
}

// ladderToken is the shape of the random last part of a ladder's
// directory: what NewHLSLadder and the inline encode write.
var ladderToken = regexp.MustCompile(`^[a-z0-9]{1,64}/$`)

// NewHLSLadder returns a fresh place for f's ladder, where WithHLS would
// put it: the Location's disk (the file's own by default) and a new
// directory under its prefix. Writing into it is the caller's part (see
// Writer); nothing is stored until AttachHLS records it. A Location error
// is returned as is.
func (p *Plugin) NewHLSLadder(f records.File) (HLSLadder, error) {
	disk, prefix, err := p.location(f, DerivedHLS)
	if err != nil {
		return HLSLadder{}, err
	}
	if disk == "" {
		disk = f.Disk
	}
	return HLSLadder{Disk: disk, Dir: prefix + strings.ToLower(rand.Text()[:posterTokenLen]) + "/", SourceSHA256: f.SHA256}, nil
}

// Writer returns the HLSWriter storing each file of the ladder under
// l.Dir on l.Disk, for Tool.HLS.
func (l HLSLadder) Writer(s *records.Store) (HLSWriter, error) {
	dst, err := s.Disks().Disk(l.Disk)
	if err != nil {
		return nil, err
	}
	w := &hlsWrite{dst: dst, disk: l.Disk, dir: l.Dir}
	return w.write, nil
}

// EncodeHLS encodes the file's current content as an HLS ladder with
// opts, outside any transaction, and records it with AttachHLS: the path
// for videos too long to encode inside the upload (WithHLS), run from a
// job. A ladder whose encode or attach fails is deleted at once, best
// effort (what fails to go is an orphan SweepOrphans removes). The file
// must be a live, released video; it returns the updated file.
func (p *Plugin) EncodeHLS(ctx context.Context, s *records.Store, scope, fileID string, opts HLS) (records.File, error) {
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return records.File{}, err
	}
	if err := attachable(f); err != nil {
		return records.File{}, err
	}
	l, err := p.NewHLSLadder(f)
	if err != nil {
		return records.File{}, err
	}
	src, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return records.File{}, err
	}
	dst, err := s.Disks().Disk(l.Disk)
	if err != nil {
		return records.File{}, err
	}
	rc, err := src.Read(ctx, f.Key)
	if err != nil {
		return records.File{}, err
	}
	defer func() { _ = rc.Close() }()
	w := &hlsWrite{dst: dst, disk: l.Disk, dir: l.Dir}
	if err := p.tool.HLS(ctx, rc, opts, w.write); err != nil {
		w.discard(context.WithoutCancel(ctx))
		return records.File{}, err
	}
	out, err := p.AttachHLS(ctx, s, scope, fileID, l)
	if err != nil {
		w.discard(context.WithoutCancel(ctx))
		return records.File{}, err
	}
	return out, nil
}

// attachable refuses a file a ladder may not be attached to: trashed,
// not a video, or quarantined (its bytes are unscanned; WithHLS encodes
// one on release).
func attachable(f records.File) error {
	switch {
	case f.DeletedAt != nil:
		return filesystem.Errorf(filesystem.CodeNotFound, "video: file %q is in the trash", f.ID)
	case !IsVideo(f.ContentType):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: file %q is %s, not a video", f.ID, f.ContentType)
	case f.Quarantined():
		return filesystem.Errorf(filesystem.CodeConflict, "video: file %q is quarantined", f.ID)
	}
	return nil
}

// AttachHLS records l as the file's HLS ladder, so it follows the rules
// of one WithHLS made: MetaHLSKey (and MetaHLSDisk off the file's disk)
// name its master playlist, the purge deletes it after the purge
// commits, a replacement of the content deletes it, MoveFile moves it,
// and SweepOrphans keeps it while the row names it. A ladder the file
// had before is deleted once this commits. It runs in a transaction
// (joining the one in ctx), with the file's row locked first, and
// returns the updated file; attaching the ladder the file already has
// changes nothing.
//
// Invariant: only a ladder in a place NewHLSLadder could have given the
// file is accepted (the Location's disk, one fresh directory under its
// prefix), because the purge deletes everything under the directory: a
// ladder elsewhere is CodeInvalidArgument, so attaching can never make a
// purge delete objects that are not the ladder's. The master playlist
// must exist (CodeNotFound), the file must be a live, released video,
// and l.SourceSHA256 must be the file's current SHA-256 when it has one
// (CodeConflict: the content was replaced while the ladder was encoded,
// so the ladder shows the old video). A rollback of the transaction
// deletes the ladder, as it would an inline one.
func (p *Plugin) AttachHLS(ctx context.Context, s *records.Store, scope, fileID string, l HLSLadder) (records.File, error) {
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
		if err := attachable(f); err != nil {
			return err
		}
		if f.SHA256 != "" && l.SourceSHA256 != f.SHA256 {
			return filesystem.Errorf(filesystem.CodeConflict, "video: file %q was replaced after its ladder's source was read", fileID)
		}
		if err := p.checkLadderPlace(f, l); err != nil {
			return err
		}
		master := l.Dir + HLSMaster
		if d, k, ok := HLSLocation(f); ok && d == l.Disk && k == master {
			out = f
			return nil
		}
		dst, err := s.Disks().Disk(l.Disk)
		if err != nil {
			return err
		}
		if _, err := dst.Stat(ctx, master); err != nil {
			return err
		}
		// Object first, row second: the ladder exists, and a rollback
		// deletes it, as it does an inline one.
		s.OnRollback(ctx, func(ctx context.Context) {
			if keys, err := dirKeys(ctx, dst, l.Dir); err == nil {
				_ = dst.DeleteKeys(ctx, keys)
			}
		})
		if d, k, ok := HLSLocation(f); ok {
			releaseDirAfterCommit(ctx, s, d, hlsDir(k))
		}
		meta := maps.Clone(f.Metadata)
		if meta == nil {
			meta = map[string]string{}
		}
		meta[MetaHLSKey] = master
		delete(meta, MetaHLSDisk)
		if l.Disk != f.Disk {
			meta[MetaHLSDisk] = l.Disk
		}
		text, _ := json.Marshal(meta) // a map of strings cannot fail to marshal
		upd := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileMetadata, string(text))).
			Where(sqlb.Eq(records.FileScope, scope), sqlb.Eq(records.FileID, fileID))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
		f.Metadata = meta
		out = f
		return nil
	})
	if err != nil {
		return records.File{}, err
	}
	return out, nil
}

// checkLadderPlace refuses a ladder that is not where NewHLSLadder would
// put one for f: on the Location's disk, in one directory directly under
// its prefix whose name is a token (ladderToken).
func (p *Plugin) checkLadderPlace(f records.File, l HLSLadder) error {
	disk, prefix, err := p.location(f, DerivedHLS)
	if err != nil {
		return err
	}
	if disk == "" {
		disk = f.Disk
	}
	token, ok := strings.CutPrefix(l.Dir, prefix)
	if l.Disk != disk || !ok || !ladderToken.MatchString(token) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: ladder %s:%s is not a directory the plugin's Location gives file %q", l.Disk, l.Dir, f.ID)
	}
	return nil
}
