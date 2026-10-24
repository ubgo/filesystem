package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Mirror returns a Disk named name that reads from primary and writes to
// primary and then to every mirror. It is how an application moves to a new
// backend without downtime (mirror old to new, backfill, then switch the
// primary) or keeps a warm copy.
//
// Semantics, which callers must accept:
//   - Not atomic across disks. A write succeeds on primary first; if a
//     mirror then fails, the error is returned (CodeUnavailable wrapping the
//     cause) and the object exists on primary only. Writes are overwrites,
//     so retrying converges.
//   - Mirrors receive the bytes by reading the object back from primary,
//     because the caller's body can be consumed only once.
//   - A mirror that folds letter case where the primary does not (local
//     disk on macOS behind a memory or S3 primary) holds one object for
//     keys that differ only in case; the primary holds two. Compare
//     Capabilities().CaseInsensitive before mixing them.
//   - When the primary's driver signs its own URLs (S3, GCS, Azure), signed
//     PUT URLs and presigned parts are refused: a client uploading straight
//     to the backend would bypass the mirrors. A primary that cannot sign
//     uses the disk's URLSigner (WithURLSigner), and those PUT URLs are
//     issued: they name the mirror disk and are served by the application
//     (fshttp.Signed), so the upload is written through the mirror as long
//     as the application resolves that name to the mirror disk. Server-side
//     multipart is allowed and replicated on completion.
//   - The mirror disk's capabilities are primary's, minus PresignPart,
//     PresignPost, PresignHeaders, PresignVisibility (signed PUTs are
//     refused, as above),
//     Append, SetModTime, ConditionalRead, CompareAndSwap, Tags, Hold,
//     Versions, VersionRead, StorageClass, Restore (in-place changes would
//     skip the mirrors) and
//     SweepAbandoned and FindAbandoned
//     (each disk keeps its own leftovers; sweep them directly). MakeDir
//     creates the directory on the primary, then on every mirror.
func Mirror(name string, primary *Disk, mirrors ...*Disk) (*Disk, error) {
	if primary == nil || len(mirrors) == 0 {
		return nil, Errorf(CodeInvalidArgument, "mirror needs a primary and at least one mirror")
	}
	for _, m := range mirrors {
		if m == nil || m == primary {
			return nil, Errorf(CodeInvalidArgument, "mirror disks must be distinct from the primary and non-nil")
		}
	}
	return primary.derive(name, &mirrorDriver{primary: primary.driver, mirrors: mirrors}, primary.publicBase)
}

// mirrorDriver fans mutations out to mirror Disks after the primary driver.
type mirrorDriver struct {
	primary Driver
	mirrors []*Disk
}

// Unwrap returns the primary driver, for As.
func (m *mirrorDriver) Unwrap() Driver { return m.primary }

func (m *mirrorDriver) Capabilities() Capabilities {
	c := m.primary.Capabilities()
	c.PresignPart = false
	// Signed PUTs and forms are refused (see Mirror), so none pins
	// headers or a visibility.
	c.PresignHeaders = false
	c.PresignVisibility = false
	// A form upload goes straight to the primary's backend, bypassing the
	// mirrors, like a signed PUT.
	c.PresignPost = false
	// Operations that change one object in place (appends, times, tags,
	// holds, restores, compare-and-swap writes) would leave the mirrors
	// behind, and versions are the primary's alone: the mirror declares
	// none of them. Conditional reads fall back to a Stat of the primary.
	c.Append, c.SetModTime, c.ConditionalRead, c.CompareAndSwap = false, false, false, false
	c.Tags, c.Hold, c.Versions, c.Restore = false, false, false, false
	c.VersionRead, c.StorageClass = false, false
	// Each underlying disk keeps its own leftovers; sweep the primary and
	// the mirrors directly.
	c.SweepAbandoned, c.FindAbandoned = false, false
	return c
}

// each runs fn on every mirror and joins the failures into one
// CodeUnavailable error naming the mirrors that failed.
func (m *mirrorDriver) each(fn func(*Disk) error) error {
	var errs []error
	for _, d := range m.mirrors {
		if err := fn(d); err != nil {
			errs = append(errs, fmt.Errorf("mirror %s: %w", d.Name(), err))
		}
	}
	if len(errs) > 0 {
		return NewError(CodeUnavailable, errors.Join(errs...))
	}
	return nil
}

// replicate copies key from primary to every mirror.
func (m *mirrorDriver) replicate(ctx context.Context, key string, opts WriteOptions) error {
	return m.each(func(d *Disk) error {
		rc, err := m.primary.Read(ctx, key, ByteRange{})
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		mo := opts
		mo.IfNotExists = false
		if !d.caps.Metadata {
			mo.Metadata = nil
		}
		if !d.caps.Headers {
			mo.Headers = HTTPHeaders{}
		}
		if !d.caps.SetVisibility {
			mo.Visibility = ""
		}
		_, err = d.Write(ctx, key, rc, mo)
		return err
	})
}

func (m *mirrorDriver) Stat(ctx context.Context, key string) (Entry, error) {
	return m.primary.Stat(ctx, key)
}

func (m *mirrorDriver) Read(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error) {
	return m.primary.Read(ctx, key, rng)
}

func (m *mirrorDriver) Write(ctx context.Context, key string, body io.Reader, opts WriteOptions) (Entry, error) {
	e, err := m.primary.Write(ctx, key, body, opts)
	if err != nil {
		return Entry{}, err
	}
	return e, m.replicate(ctx, key, opts)
}

func (m *mirrorDriver) Delete(ctx context.Context, key string) error {
	if err := m.primary.Delete(ctx, key); err != nil {
		return err
	}
	return m.each(func(d *Disk) error { return d.Delete(ctx, key) })
}

func (m *mirrorDriver) List(ctx context.Context, dir string, opts ListOptions) (Page, error) {
	return m.primary.List(ctx, dir, opts)
}

func (m *mirrorDriver) Copy(ctx context.Context, src, dst string) (Entry, error) {
	e, err := m.primary.(Copier).Copy(ctx, src, dst)
	if err != nil {
		return Entry{}, err
	}
	return e, m.each(func(d *Disk) error { _, err := d.Copy(ctx, src, dst); return err })
}

func (m *mirrorDriver) Move(ctx context.Context, src, dst string) (Entry, error) {
	e, err := m.primary.(Mover).Move(ctx, src, dst)
	if err != nil {
		return Entry{}, err
	}
	return e, m.each(func(d *Disk) error { _, err := d.Move(ctx, src, dst); return err })
}

func (m *mirrorDriver) SetVisibility(ctx context.Context, key string, v Visibility) error {
	if err := m.primary.(VisibilitySetter).SetVisibility(ctx, key, v); err != nil {
		return err
	}
	return m.each(func(d *Disk) error {
		if !d.caps.SetVisibility {
			return nil
		}
		return d.SetVisibility(ctx, key, v)
	})
}

func (m *mirrorDriver) DeletePrefix(ctx context.Context, dir string) error {
	if err := m.primary.(PrefixDeleter).DeletePrefix(ctx, dir); err != nil {
		return err
	}
	return m.each(func(d *Disk) error { return d.DeletePrefix(ctx, dir) })
}

// MakeDir creates dir on the primary, then on every mirror through its
// Disk, which on a mirror without directories does nothing (Disk.MakeDir).
func (m *mirrorDriver) MakeDir(ctx context.Context, dir string) error {
	if err := m.primary.(DirMaker).MakeDir(ctx, dir); err != nil {
		return err
	}
	return m.each(func(d *Disk) error { return d.MakeDir(ctx, dir) })
}

// DirExists asks the primary, like every other read.
func (m *mirrorDriver) DirExists(ctx context.Context, dir string) (bool, error) {
	return m.primary.(DirMaker).DirExists(ctx, dir)
}

func (m *mirrorDriver) DeleteKeys(ctx context.Context, keys []string) error {
	if err := m.primary.(KeysDeleter).DeleteKeys(ctx, keys); err != nil {
		return err
	}
	return m.each(func(d *Disk) error { return d.DeleteKeys(ctx, keys) })
}

// presignsMethod reports that Presign signs downloads only (see
// methodPresigner).
func (m *mirrorDriver) presignsMethod(method HTTPMethod) bool { return method == MethodGet }

// Presign signs downloads only; see Mirror.
func (m *mirrorDriver) Presign(ctx context.Context, key string, opts PresignOptions) (SignedRequest, error) {
	if opts.Method == MethodPut {
		return SignedRequest{}, Errorf(CodeUnsupported, "a signed upload to a mirrored disk would bypass its mirrors")
	}
	return m.primary.(Presigner).Presign(ctx, key, opts)
}

func (m *mirrorDriver) PublicURL(ctx context.Context, key string) (string, error) {
	return m.primary.(PublicURLer).PublicURL(ctx, key)
}

func (m *mirrorDriver) CreateMultipart(ctx context.Context, key string, opts WriteOptions) (string, error) {
	return m.primary.(MultipartUploader).CreateMultipart(ctx, key, opts)
}

func (m *mirrorDriver) UploadPart(ctx context.Context, key, id string, n int, body io.Reader, size int64) (Part, error) {
	return m.primary.(MultipartUploader).UploadPart(ctx, key, id, n, body, size)
}

func (m *mirrorDriver) ListParts(ctx context.Context, key, id string) ([]Part, error) {
	return m.primary.(MultipartUploader).ListParts(ctx, key, id)
}

// CompleteMultipart assembles on primary, then replicates the result. The
// write options recorded at CreateMultipart are not available here, so the
// replica takes the assembled object's content type and visibility.
func (m *mirrorDriver) CompleteMultipart(ctx context.Context, key, id string, parts []Part) (Entry, error) {
	e, err := m.primary.(MultipartUploader).CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return Entry{}, err
	}
	return e, m.replicate(ctx, key, WriteOptions{ContentType: e.ContentType, Visibility: e.Visibility, Metadata: e.Metadata})
}

func (m *mirrorDriver) AbortMultipart(ctx context.Context, key, id string) error {
	return m.primary.(MultipartUploader).AbortMultipart(ctx, key, id)
}
