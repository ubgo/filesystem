package gcs

import (
	"cmp"
	"context"
	"io"
	"slices"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/ubgo/filesystem"
)

// Versions implements filesystem.VersionLister from the object's
// generations, newest first. A bucket keeps noncurrent generations only
// with Object Versioning on, which is why the capability is declared by
// Config.Versioned rather than assumed. GCS has no delete markers: after a
// Delete every version is noncurrent and none is Latest. A key with no
// generation at all is CodeNotFound. Version.ID is the generation number.
func (d *Driver) Versions(ctx context.Context, key string) ([]filesystem.Version, error) {
	// StartOffset and EndOffset bound the listing to exactly key, so a
	// bucket with many keys sharing its prefix is not walked.
	q := &storage.Query{Versions: true, StartOffset: key, EndOffset: key + "\x00"}
	var out []filesystem.Version
	it := d.bucket.Objects(ctx, q)
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, filesystem.Version{
			ID:      strconv.FormatInt(a.Generation, 10),
			Size:    a.Size,
			ModTime: a.Updated,
			ETag:    strings.Trim(a.Etag, `"`),
			Latest:  a.Deleted.IsZero(),
		})
	}
	if len(out) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "gcs: %q has no versions", key)
	}
	slices.SortFunc(out, func(a, b filesystem.Version) int {
		ga, _ := strconv.ParseInt(a.ID, 10, 64)
		gb, _ := strconv.ParseInt(b.ID, 10, 64)
		return cmp.Compare(gb, ga)
	})
	return out, nil
}

// generation parses a Version.ID. An ID that is not a generation number
// names no version GCS could hold: CodeNotFound.
func generation(id string) (int64, error) {
	g, err := strconv.ParseInt(id, 10, 64)
	if err != nil || g <= 0 {
		return 0, filesystem.Errorf(filesystem.CodeNotFound, "gcs: no version %q (versions are generation numbers)", id)
	}
	return g, nil
}

// ReadVersion implements filesystem.VersionReader: one stored version of
// key (a Version.ID from Versions), current or noncurrent. A version that
// does not exist is CodeNotFound.
func (d *Driver) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	g, err := generation(id)
	if err != nil {
		return nil, err
	}
	r, err := d.object(key).Generation(g).NewReader(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	return r, nil
}

// RestoreVersion implements filesystem.VersionReader: it makes version id
// of key the current one again, by rewriting that generation over the live
// object inside GCS; the version being replaced becomes noncurrent, so
// nothing is lost. It works after a Delete too (the key comes back). The
// restored object is a new generation, so it gets Config.RetainFor.
func (d *Driver) RestoreVersion(ctx context.Context, key, id string) error {
	g, err := generation(id)
	if err != nil {
		return err
	}
	call, err := d.callSettings(ctx, key)
	if err != nil {
		return err
	}
	c := d.object(key).CopierFrom(d.object(key).Generation(g))
	c.DestinationKMSKeyName = d.kmsKey(call, key)
	c.Retention = d.retention()
	_, err = c.Run(ctx)
	return mapErr(err)
}
