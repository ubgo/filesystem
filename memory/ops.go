package memory

import (
	"context"
	"io"
	"maps"
	"time"

	"github.com/ubgo/filesystem"
)

// Append implements filesystem.Appender. The body is read in full before
// the lock is taken, so a failing body appends nothing and two appends
// never interleave.
func (d *Driver) Append(ctx context.Context, key string, body io.Reader, opts filesystem.AppendOptions) (filesystem.Entry, error) {
	data, err := io.ReadAll(&ctxReader{ctx: ctx, r: body})
	if err != nil {
		return filesystem.Entry{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, ok := d.objects[key]
	if opts.MaxSize > 0 && int64(len(cur.dataOrNil())+len(data)) > opts.MaxSize {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeTooLarge, "the object would exceed %d bytes", opts.MaxSize)
	}
	if !ok {
		o := d.newObject(data, "", opts.Visibility, nil, filesystem.HTTPHeaders{})
		d.put(key, o)
		return o.entry(key), nil
	}
	// A new object value, not an update in place: readers already holding
	// the old bytes keep their snapshot.
	o := d.newObject(append(append([]byte(nil), cur.data...), data...), cur.contentType, cur.visibility, cur.metadata, cur.headers)
	o.tags, o.class = cur.tags, cur.class
	d.put(key, o)
	return o.entry(key), nil
}

// SetModTime implements filesystem.ModTimeSetter.
func (d *Driver) SetModTime(ctx context.Context, key string, t time.Time) error {
	return d.update(ctx, key, func(o *object) { o.modTime = t })
}

// Tags implements filesystem.Tagger.
func (d *Driver) Tags(ctx context.Context, key string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	o, ok := d.objects[key]
	if !ok {
		return nil, notFound(key)
	}
	return maps.Clone(o.tags), nil
}

// SetTags implements filesystem.Tagger.
func (d *Driver) SetTags(ctx context.Context, key string, tags map[string]string) error {
	return d.update(ctx, key, func(o *object) { o.tags = maps.Clone(tags) })
}

// update applies fn to a copy of key's object and stores the copy, so a
// reader holding the old value never sees it change.
func (d *Driver) update(ctx context.Context, key string, fn func(*object)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.objects[key]
	if !ok {
		return notFound(key)
	}
	cp := *o
	fn(&cp)
	d.objects[key] = &cp
	return nil
}

// dataOrNil is o's bytes, or none for a missing object.
func (o *object) dataOrNil() []byte {
	if o == nil {
		return nil
	}
	return o.data
}
