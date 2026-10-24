package filesystem

import (
	"context"
	"errors"
	"io"
	"time"
)

// Scope returns a Disk named name that sees only the keys under prefix on d:
// key "a.png" on the scoped disk is "<prefix>/a.png" on d. It shares d's
// driver, interceptors, URL signer and settings; a public URL base, if set,
// gains the prefix. Typical use is one scoped disk per tenant or feature.
//
// The scoped disk needs its own name because signed URLs and logs identify
// a disk by name, and its keys differ from d's.
func (d *Disk) Scope(name, prefix string) (*Disk, error) {
	p, err := d.cleanDir(prefix)
	if err != nil {
		return nil, err
	}
	if p == "" {
		return nil, Errorf(CodeInvalidArgument, "empty scope prefix")
	}
	base := d.publicBase
	if base != "" {
		base += "/" + EscapeKey(p)
	}
	return d.derive(name, newScopedDriver(d.driver, p), base)
}

// scopedDriver prefixes every key it passes to inner and strips the prefix
// from every key it returns. It implements every optional interface and
// reports inner's capabilities, so the Disk only calls what inner supports.
type scopedDriver struct {
	inner  Driver
	prefix string
}

func newScopedDriver(inner Driver, prefix string) *scopedDriver {
	return &scopedDriver{inner: inner, prefix: prefix}
}

func (s *scopedDriver) full(key string) string { return JoinKey(s.prefix, key) }

func (s *scopedDriver) strip(e Entry) Entry {
	e.Key = RelativeKey(e.Key, s.prefix)
	return e
}

// Unwrap returns the driver underneath, for As.
func (s *scopedDriver) Unwrap() Driver { return s.inner }

// InnerKey implements KeyMapper: the key inner sees for key.
func (s *scopedDriver) InnerKey(key string) string { return s.full(key) }

// Capabilities are the inner driver's, except SweepAbandoned and
// FindAbandoned: leftovers are backend-wide, and a disk scoped to one
// prefix must neither abort nor report uploads under other prefixes.
func (s *scopedDriver) Capabilities() Capabilities {
	c := s.inner.Capabilities()
	c.SweepAbandoned, c.FindAbandoned = false, false
	return c
}

func (s *scopedDriver) MakeDir(ctx context.Context, dir string) error {
	return s.inner.(DirMaker).MakeDir(ctx, s.full(dir))
}

func (s *scopedDriver) DirExists(ctx context.Context, dir string) (bool, error) {
	return s.inner.(DirMaker).DirExists(ctx, s.full(dir))
}

func (s *scopedDriver) Stat(ctx context.Context, key string) (Entry, error) {
	e, err := s.inner.Stat(ctx, s.full(key))
	return s.strip(e), err
}

func (s *scopedDriver) Read(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error) {
	return s.inner.Read(ctx, s.full(key), rng)
}

func (s *scopedDriver) Write(ctx context.Context, key string, body io.Reader, opts WriteOptions) (Entry, error) {
	e, err := s.inner.Write(ctx, s.full(key), body, opts)
	return s.strip(e), err
}

func (s *scopedDriver) Delete(ctx context.Context, key string) error {
	return s.inner.Delete(ctx, s.full(key))
}

// List passes the cursor through unchanged: it is opaque to callers, and the
// inner driver issued it.
func (s *scopedDriver) List(ctx context.Context, dir string, opts ListOptions) (Page, error) {
	page, err := s.inner.List(ctx, s.full(dir), opts)
	for i := range page.Entries {
		page.Entries[i] = s.strip(page.Entries[i])
	}
	return page, err
}

func (s *scopedDriver) Copy(ctx context.Context, src, dst string) (Entry, error) {
	e, err := s.inner.(Copier).Copy(ctx, s.full(src), s.full(dst))
	return s.strip(e), err
}

func (s *scopedDriver) Move(ctx context.Context, src, dst string) (Entry, error) {
	e, err := s.inner.(Mover).Move(ctx, s.full(src), s.full(dst))
	return s.strip(e), err
}

func (s *scopedDriver) SetVisibility(ctx context.Context, key string, v Visibility) error {
	return s.inner.(VisibilitySetter).SetVisibility(ctx, s.full(key), v)
}

func (s *scopedDriver) DeletePrefix(ctx context.Context, dir string) error {
	return s.inner.(PrefixDeleter).DeletePrefix(ctx, s.full(dir))
}

// DeleteKeys forwards to the driver underneath, reporting failed keys
// relative to the scope again.
func (s *scopedDriver) DeleteKeys(ctx context.Context, keys []string) error {
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = s.full(k)
	}
	err := s.inner.(KeysDeleter).DeleteKeys(ctx, full)
	var batch *BatchError
	if !errors.As(err, &batch) {
		return err
	}
	out := &BatchError{Failures: make([]KeyError, len(batch.Failures))}
	for i, f := range batch.Failures {
		out.Failures[i] = KeyError{Key: RelativeKey(f.Key, s.prefix), Err: f.Err}
	}
	return out
}

// presignsMethod forwards the inner driver's per-method signing (see
// methodPresigner), so scoping a mirrored or cached disk keeps its limits.
func (s *scopedDriver) presignsMethod(m HTTPMethod) bool { return driverPresigns(s.inner, m) }

func (s *scopedDriver) Presign(ctx context.Context, key string, opts PresignOptions) (SignedRequest, error) {
	return s.inner.(Presigner).Presign(ctx, s.full(key), opts)
}

// PresignPost prefixes the key or the key prefix, so a scoped form can
// never upload outside the scope.
func (s *scopedDriver) PresignPost(ctx context.Context, p PostPolicy) (PresignedPost, error) {
	if p.Key != "" {
		p.Key = s.full(p.Key)
	} else {
		p.KeyPrefix = s.full(p.KeyPrefix)
	}
	return s.inner.(PostPresigner).PresignPost(ctx, p)
}

func (s *scopedDriver) PublicURL(ctx context.Context, key string) (string, error) {
	return s.inner.(PublicURLer).PublicURL(ctx, s.full(key))
}

func (s *scopedDriver) CreateMultipart(ctx context.Context, key string, opts WriteOptions) (string, error) {
	return s.inner.(MultipartUploader).CreateMultipart(ctx, s.full(key), opts)
}

func (s *scopedDriver) UploadPart(ctx context.Context, key, id string, n int, body io.Reader, size int64) (Part, error) {
	return s.inner.(MultipartUploader).UploadPart(ctx, s.full(key), id, n, body, size)
}

func (s *scopedDriver) PresignPart(ctx context.Context, key, id string, n int, expires time.Duration) (SignedRequest, error) {
	return s.inner.(PartPresigner).PresignPart(ctx, s.full(key), id, n, expires)
}

func (s *scopedDriver) ListParts(ctx context.Context, key, id string) ([]Part, error) {
	return s.inner.(MultipartUploader).ListParts(ctx, s.full(key), id)
}

func (s *scopedDriver) CompleteMultipart(ctx context.Context, key, id string, parts []Part) (Entry, error) {
	e, err := s.inner.(MultipartUploader).CompleteMultipart(ctx, s.full(key), id, parts)
	return s.strip(e), err
}

func (s *scopedDriver) AbortMultipart(ctx context.Context, key, id string) error {
	return s.inner.(MultipartUploader).AbortMultipart(ctx, s.full(key), id)
}

func (s *scopedDriver) Append(ctx context.Context, key string, body io.Reader, opts AppendOptions) (Entry, error) {
	e, err := s.inner.(Appender).Append(ctx, s.full(key), body, opts)
	return s.strip(e), err
}

func (s *scopedDriver) SetModTime(ctx context.Context, key string, t time.Time) error {
	return s.inner.(ModTimeSetter).SetModTime(ctx, s.full(key), t)
}

func (s *scopedDriver) ReadIf(ctx context.Context, key string, opts ReadOptions) (io.ReadCloser, Entry, error) {
	rc, e, err := s.inner.(ConditionalReader).ReadIf(ctx, s.full(key), opts)
	return rc, s.strip(e), err
}

func (s *scopedDriver) Tags(ctx context.Context, key string) (map[string]string, error) {
	return s.inner.(Tagger).Tags(ctx, s.full(key))
}

func (s *scopedDriver) SetTags(ctx context.Context, key string, tags map[string]string) error {
	return s.inner.(Tagger).SetTags(ctx, s.full(key), tags)
}

func (s *scopedDriver) Hold(ctx context.Context, key string) (Hold, error) {
	return s.inner.(Holder).Hold(ctx, s.full(key))
}

func (s *scopedDriver) SetHold(ctx context.Context, key string, h Hold) error {
	return s.inner.(Holder).SetHold(ctx, s.full(key), h)
}

func (s *scopedDriver) Versions(ctx context.Context, key string) ([]Version, error) {
	return s.inner.(VersionLister).Versions(ctx, s.full(key))
}

func (s *scopedDriver) ReadVersion(ctx context.Context, key, id string) (io.ReadCloser, error) {
	return s.inner.(VersionReader).ReadVersion(ctx, s.full(key), id)
}

func (s *scopedDriver) RestoreVersion(ctx context.Context, key, id string) error {
	return s.inner.(VersionReader).RestoreVersion(ctx, s.full(key), id)
}

func (s *scopedDriver) SetStorageClass(ctx context.Context, key, class string) error {
	return s.inner.(StorageClasser).SetStorageClass(ctx, s.full(key), class)
}

func (s *scopedDriver) Restore(ctx context.Context, key string, opts RestoreOptions) error {
	return s.inner.(Restorer).Restore(ctx, s.full(key), opts)
}
