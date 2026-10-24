package jobs

import (
	"context"
	"errors"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// Cursor keeps where a resumable job (Scrub, MigrateDisk) stopped, so the
// next run, in this process or after a restart or a crash, continues
// there instead of starting over. The library keeps no state of its own:
// the application stores the value wherever it likes (a row, a Redis key,
// an object with ObjectCursor). The value is opaque; an empty one means
// "start from the beginning".
type Cursor interface {
	Load(ctx context.Context) (string, error)
	Save(ctx context.Context, value string) error
}

// ObjectCursor is a Cursor kept as the object key on disk: durable across
// restarts with nothing but a disk. A missing object loads as "". Keep
// key outside every prefix a sweep or a sync deletes from.
func ObjectCursor(disk *filesystem.Disk, key string) Cursor {
	return objectCursor{disk: disk, key: key}
}

type objectCursor struct {
	disk *filesystem.Disk
	key  string
}

func (c objectCursor) Load(ctx context.Context) (string, error) {
	b, err := c.disk.ReadAll(ctx, c.key)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return "", nil
	}
	return string(b), err
}

func (c objectCursor) Save(ctx context.Context, value string) error {
	_, err := c.disk.WriteBytes(ctx, c.key, []byte(value), filesystem.WriteOptions{ContentType: "text/plain"})
	return err
}

// defaultBatch is how many files a resumable job handles in one run when
// its options leave Batch at 0: a few minutes of work at most for typical
// file sizes, so a run ends (and saves its cursor) often.
const defaultBatch = 500

// fileRef names one file row across scopes.
type fileRef struct{ scope, id string }

// filesOn returns up to limit files recorded on the named disk, in any
// scope, live or trashed, with an ID after after, in ID order: the walk
// Scrub and MigrateDisk share, resumable from the last ID handled.
func filesOn(ctx context.Context, store *records.Store, disk, after string, limit int) ([]fileRef, error) {
	var r fileRef
	var out []fileRef
	q := sqlb.NewSelect(store.Dialect()).Columns(sqlb.Bind(records.FileScope, &r.scope), sqlb.Bind(records.FileID, &r.id)).
		From(records.TableFiles).Where(sqlb.Eq(records.FileDisk, disk), sqlb.Greater(records.FileID, after)).
		Order(sqlb.OrderBy(records.FileID, sqlb.SortAsc)).Limit(limit)
	err := store.Each(ctx, q, func() error { out = append(out, r); return nil })
	return out, err
}

// batchSize is n, or defaultBatch when n is not positive.
func batchSize(n int) int {
	if n <= 0 {
		return defaultBatch
	}
	return n
}

// saveOnError saves the cursor at the last item handled and returns
// err, joined with the save's failure if any. The save runs even when ctx
// has ended, so a cancelled run still records its progress.
func saveOnError(ctx context.Context, c Cursor, value string, err error) error {
	if serr := c.Save(context.WithoutCancel(ctx), value); serr != nil {
		return errors.Join(err, serr)
	}
	return err
}
