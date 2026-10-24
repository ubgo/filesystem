package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/sniff"
)

// contentStaging is the directory, under ContentAddressed's dir, where
// uploads wait until their hash names them. Point Sweep at it for uploads
// a crash left there.
const contentStaging = ".incoming"

// ContentAddressing configures ContentAddressed: whose uploads may share
// an object.
type ContentAddressing struct {
	// Owner names who is uploading (a user or tenant ID), from the
	// request's context, where the application's authentication put it.
	// Identical files are shared only within one owner's uploads. Required
	// unless Shared.
	Owner func(ctx context.Context) (string, error)
	// Shared lets every upload under dir share objects, across owners: a
	// deliberate choice, with the consequences ContentAddressed lists.
	Shared bool
}

// ownerDigestLen is how many hex characters of the owner's SHA-256 name
// its directory: key-safe whatever the owner ID holds, and not readable as
// the ID itself.
const ownerDigestLen = 32

// ContentAddressed returns the Key and Settle steps of a policy whose files
// are named by their content, as Git, container registries and backup
// tools store blobs: identical files share one object, keys never
// collide, and a key proves its content. By default sharing stays within
// one owner: "<dir>/<owner digest>/<sha256>.<ext>", the owner coming from
// o.Owner:
//
//	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Owner: tenantFromContext})
//
// With o.Shared every upload under dir shares, "<dir>/<sha256>.<ext>", and
// two consequences follow: deleting one owner's file deletes the object
// every other owner's identical file points to (count references, as
// records' FileBySHA256 allows, before deleting), and an upload is only
// deduplicated against an object stored with the same visibility and
// headers, so one owner's public upload can never make another's private
// file public (a duplicate with other settings is CodeConflict; the check
// applies within one owner too). Neither Owner nor Shared refuses every
// upload with CodeInvalidArgument.
//
// The upload is first written under "<dir>/.incoming/" with a random name
// while the Disk hashes it, then moved to its content key. When that key
// already holds the same content (a duplicate upload) the incoming copy is
// deleted and the existing object reused; two identical uploads racing to
// one key both write the same bytes, so neither can corrupt the other.
func ContentAddressed(dir string, o ContentAddressing) (KeyFunc, Settler) {
	staging := filesystem.JoinKey(dir, contentStaging)
	settle := func(ctx context.Context, disk *filesystem.Disk, r Result) (Result, error) {
		scope, err := o.scope(ctx, dir)
		if err != nil {
			return Result{}, err
		}
		target := filesystem.JoinKey(scope, r.SHA256+sniff.Extension(r.ContentType))
		existing, err := disk.Stat(ctx, target)
		switch {
		case err == nil && existing.Size == r.Size:
			if err := sameSettings(ctx, disk, r.Key, existing); err != nil {
				return Result{}, err
			}
			if err := disk.Delete(ctx, r.Key); err != nil {
				return Result{}, err
			}
		case err == nil || filesystem.CodeOf(err) == filesystem.CodeNotFound:
			if _, err := disk.Move(ctx, r.Key, target); err != nil {
				return Result{}, err
			}
		default:
			return Result{}, err
		}
		r.Key = target
		return r, nil
	}
	return RandomKey(staging), settle
}

// scope is the directory whose uploads share objects: dir when Shared,
// else the owner's directory under it.
func (o ContentAddressing) scope(ctx context.Context, dir string) (string, error) {
	if o.Shared {
		return dir, nil
	}
	if o.Owner == nil {
		return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "content-addressed keys need an Owner, or Shared to share across owners")
	}
	owner, err := o.Owner(ctx)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(owner))
	return filesystem.JoinKey(dir, hex.EncodeToString(sum[:])[:ownerDigestLen]), nil
}

// sameSettings refuses to reuse existing for the upload at key when they
// were stored with other visibility or headers: reusing it would serve
// this upload with the other's settings, or the other with this one's.
func sameSettings(ctx context.Context, disk *filesystem.Disk, key string, existing filesystem.Entry) error {
	incoming, err := disk.Stat(ctx, key)
	if err != nil {
		return err
	}
	if incoming.Visibility != existing.Visibility || incoming.Headers != existing.Headers {
		return filesystem.Errorf(filesystem.CodeConflict, "identical content is already stored under this directory with other visibility or headers")
	}
	return nil
}
