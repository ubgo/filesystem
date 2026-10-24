package gcs

import (
	"context"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// RetentionMode is how firmly an object's retention date holds (GCS object
// retention lock; the bucket must have object retention enabled).
type RetentionMode string

const (
	// RetentionUnlocked: the date can be shortened or removed later by a
	// caller with storage.objects.overrideUnlockedRetention, so a mistake
	// can be undone. The default.
	RetentionUnlocked RetentionMode = "Unlocked"
	// RetentionLocked: WORM compliance; the date can only be extended, and
	// the object cannot be deleted or replaced before it, by anyone.
	RetentionLocked RetentionMode = "Locked"
)

// RetentionModeValues lists every RetentionMode.
var RetentionModeValues = []RetentionMode{RetentionUnlocked, RetentionLocked}

// retention is the retention Write sets on a new object: Config.RetainFor
// from now, or nil when unset.
func (d *Driver) retention() *storage.ObjectRetention {
	if d.cfg.RetainFor <= 0 {
		return nil
	}
	return &storage.ObjectRetention{Mode: string(d.retentionMode()), RetainUntil: d.now().Add(d.cfg.RetainFor)}
}

func (d *Driver) retentionMode() RetentionMode {
	if d.cfg.RetentionMode == "" {
		return RetentionUnlocked
	}
	return d.cfg.RetentionMode
}

// Hold implements filesystem.Holder. Legal is the object's temporary hold;
// RetainUntil its retention date. An event-based hold (set by a bucket
// default for retention that starts at an event) is reported by ObjectInfo,
// not here, because lifting it is that event, not a release of a hold.
func (d *Driver) Hold(ctx context.Context, key string) (filesystem.Hold, error) {
	a, err := d.object(key).Attrs(ctx)
	if err != nil {
		return filesystem.Hold{}, mapErr(err)
	}
	return holdOf(a), nil
}

func holdOf(a *storage.ObjectAttrs) filesystem.Hold {
	h := filesystem.Hold{Legal: a.TemporaryHold}
	if a.Retention != nil {
		h.RetainUntil = a.Retention.RetainUntil
	}
	return h
}

// SetHold implements filesystem.Holder: Legal sets or releases the
// temporary hold, which any bucket supports; RetainUntil sets the
// retention date in Config.RetentionMode, which needs a bucket with object
// retention enabled. The retention is sent only when it changes, with the
// unlocked-retention override so an Unlocked date can be shortened or
// removed; GCS refuses to shorten a Locked one (CodePermission). While
// either protection holds, Delete and overwrites fail with CodePermission.
func (d *Driver) SetHold(ctx context.Context, key string, h filesystem.Hold) error {
	obj := d.object(key)
	a, err := obj.Attrs(ctx)
	if err != nil {
		return mapErr(err)
	}
	update := storage.ObjectAttrsToUpdate{TemporaryHold: h.Legal}
	if cur := holdOf(a).RetainUntil; !cur.Equal(h.RetainUntil) {
		update.Retention = &storage.ObjectRetention{}
		if !h.RetainUntil.IsZero() {
			update.Retention = &storage.ObjectRetention{Mode: string(d.retentionMode()), RetainUntil: h.RetainUntil}
		}
		obj = obj.OverrideUnlockedRetention(true)
	}
	_, err = obj.Update(ctx, update)
	return mapErr(err)
}
