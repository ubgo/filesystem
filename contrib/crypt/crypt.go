// Package crypt encrypts objects before they reach a backend, so the
// storage provider, a leaked bucket or a stolen disk image holds only
// ciphertext. It is a wrapper Driver (ADR-0007): wrap any driver, put the
// result in filesystem.NewDisk, and every byte written through the Disk is
// encrypted with a fresh data key, which a Keyring wraps under a master key
// that can differ per tenant and live in a KMS.
//
// Objects are encrypted in 64 KiB chunks of AES-256-GCM (see format.go for
// the layout), so ranged reads, and the streaming and resumable serving
// built on them, decrypt only the chunks they touch. Each chunk is bound
// to its position, its object and its storage key: a reordered, swapped,
// moved or truncated object fails to decrypt with ErrTampered instead of
// returning wrong bytes.
//
// The wrapper declares only the capabilities it can honour on ciphertext.
// Backend-signed and public URLs, browser uploads, multipart, append,
// server-side copy and move (the storage key is authenticated), version
// reads, backend checksums and conditional reads are not forwarded; the
// Disk falls back where it can (copy and move stream through and
// re-encrypt) and refuses the rest with CodeUnsupported.
//
// What stays visible to the backend: keys, sizes (plaintext size plus a
// fixed overhead), modification times, content types, metadata, headers
// and tags. Size and ETag: Stat and List report the plaintext size; the
// ETag is the ciphertext's, which changes on every write even of the same
// bytes, and works for compare-and-swap.
package crypt

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/ubgo/filesystem"
)

// Driver encrypts and decrypts objects on the way to and from the driver
// it wraps. Build it with New.
type Driver struct {
	inner   filesystem.Driver
	keyring Keyring
}

// New wraps inner so every object is encrypted with a data key that
// keyring wraps. inner must hold only objects this package wrote: Stat,
// List and Read refuse others with ErrTampered.
func New(inner filesystem.Driver, keyring Keyring) (*Driver, error) {
	if inner == nil || keyring == nil {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "crypt: a driver and a keyring are required")
	}
	return &Driver{inner: inner, keyring: keyring}, nil
}

// String describes the driver without its keyring's secrets.
func (d *Driver) String() string { return "crypt.Driver{redacted}" }

// GoString implements fmt.GoStringer so %#v redacts too.
func (d *Driver) GoString() string { return d.String() }

// LogValue implements slog.LogValuer so structured logs redact too.
func (d *Driver) LogValue() slog.Value { return slog.StringValue(d.String()) }

// Unwrap returns the wrapped driver, for filesystem.As.
func (d *Driver) Unwrap() filesystem.Driver { return d.inner }

// forwarded are the capabilities that do not depend on the bytes stored,
// so the wrapped driver's answer holds for ciphertext.
func forwarded(c filesystem.Capabilities) filesystem.Capabilities {
	return filesystem.Capabilities{
		SetVisibility:    c.SetVisibility,
		DeletePrefix:     c.DeletePrefix,
		DeleteKeys:       c.DeleteKeys,
		SweepAbandoned:   c.SweepAbandoned,
		FindAbandoned:    c.FindAbandoned,
		Dirs:             c.Dirs,
		SetModTime:       c.SetModTime,
		Tags:             c.Tags,
		Hold:             c.Hold,
		StorageClass:     c.StorageClass,
		Restore:          c.Restore,
		Metadata:         c.Metadata,
		ContentType:      c.ContentType,
		Headers:          c.Headers,
		ConditionalWrite: c.ConditionalWrite,
		CompareAndSwap:   c.CompareAndSwap,
		CaseInsensitive:  c.CaseInsensitive,
	}
}

// Capabilities implements filesystem.Driver: the wrapped driver's,
// narrowed to what holds for ciphertext.
func (d *Driver) Capabilities() filesystem.Capabilities { return forwarded(d.inner.Capabilities()) }

// Stat implements filesystem.Driver, reporting the plaintext size.
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := d.inner.Stat(ctx, key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	return plainEntry(e)
}

// plainEntry converts a stored entry to what the caller wrote.
func plainEntry(e filesystem.Entry) (filesystem.Entry, error) {
	if e.IsDir {
		return e, nil
	}
	size, err := PlaintextSize(e.Size)
	if err != nil {
		return filesystem.Entry{}, fmt.Errorf("%s: %w", e.Key, err)
	}
	e.Size = size
	// A digest the backend reports is the ciphertext's.
	e.SHA256 = ""
	return e, nil
}

// List implements filesystem.Driver, reporting plaintext sizes.
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	p, err := d.inner.List(ctx, dir, opts)
	if err != nil {
		return filesystem.Page{}, err
	}
	for i, e := range p.Entries {
		if p.Entries[i], err = plainEntry(e); err != nil {
			return filesystem.Page{}, err
		}
	}
	return p, nil
}

// Delete implements filesystem.Driver.
func (d *Driver) Delete(ctx context.Context, key string) error { return d.inner.Delete(ctx, key) }

// Write implements filesystem.Driver: a fresh data key and object ID, the
// data key wrapped by the keyring, then the body sealed chunk by chunk
// while it streams to the wrapped driver.
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	dek := make([]byte, DataKeySize)
	if _, err := rand.Read(dek); err != nil {
		return filesystem.Entry{}, err
	}
	defer clear(dek)
	hdr := header{chunkSize: ChunkSize}
	if _, err := rand.Read(hdr.objectID[:]); err != nil {
		return filesystem.Entry{}, err
	}
	wrapped, err := d.keyring.WrapKey(ctx, key, dek)
	if err != nil {
		return filesystem.Entry{}, err
	}
	hdr.wrapped = wrapped
	encoded, err := hdr.encode()
	if err != nil {
		return filesystem.Entry{}, err
	}
	aead, err := newAEAD(dek)
	if err != nil {
		return filesystem.Entry{}, err
	}
	s := newSealer(body, aead, hdr, encoded, key)
	// The Disk has already checked the plaintext against SHA256 and
	// MaxSize; the backend sees ciphertext, so the digest cannot go to it
	// and the size hint is the ciphertext's.
	opts.SHA256 = ""
	opts.MaxSize = 0
	if opts.SizeHint > 0 {
		opts.SizeHint = CiphertextSize(opts.SizeHint)
	}
	e, err := d.inner.Write(ctx, key, s, opts)
	if err != nil {
		return filesystem.Entry{}, err
	}
	e.Size = s.read
	e.SHA256 = ""
	return e, nil
}

// open reads and unwraps the header of key from r.
func (d *Driver) open(ctx context.Context, key string, r io.Reader) (header, []byte, error) {
	b := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return header{}, nil, tampered("shorter than a header")
		}
		return header{}, nil, err
	}
	hdr, err := decodeHeader(b)
	if err != nil {
		return header{}, nil, err
	}
	dek, err := d.keyring.UnwrapKey(ctx, key, hdr.wrapped)
	if err != nil {
		return header{}, nil, err
	}
	return hdr, dek, nil
}

// Read implements filesystem.Driver. A whole read is one request to the
// wrapped driver. A ranged read is three (the header, a Stat for the
// size, and the chunks covering the range) and decrypts only those
// chunks.
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if rng.Offset == 0 && rng.Length == 0 {
		rc, err := d.inner.Read(ctx, key, filesystem.ByteRange{})
		if err != nil {
			return nil, err
		}
		hdr, dek, err := d.open(ctx, key, rc)
		if err != nil {
			_ = rc.Close()
			return nil, err
		}
		return d.opener(rc, hdr, dek, key, 0, -1, false, 0, -1)
	}
	hrc, err := d.inner.Read(ctx, key, filesystem.ByteRange{Length: HeaderSize})
	if err != nil {
		return nil, err
	}
	hdr, dek, err := d.open(ctx, key, hrc)
	_ = hrc.Close()
	if err != nil {
		return nil, err
	}
	st, err := d.inner.Stat(ctx, key)
	if err != nil {
		clear(dek)
		return nil, err
	}
	size, err := PlaintextSize(st.Size)
	if err != nil {
		clear(dek)
		return nil, err
	}
	end := size
	if rng.Length > 0 {
		end = min(size, rng.Offset+rng.Length)
	}
	if rng.Offset >= end {
		clear(dek)
		return io.NopCloser(&emptyReader{}), nil
	}
	first, last := rng.Offset/ChunkSize, (end-1)/ChunkSize
	finalChunk := chunkCount(size) - 1
	const sealed = ChunkSize + TagSize
	rc, err := d.inner.Read(ctx, key, filesystem.ByteRange{Offset: HeaderSize + first*sealed, Length: (last - first + 1) * sealed})
	if err != nil {
		clear(dek)
		return nil, err
	}
	return d.opener(rc, hdr, dek, key, uint64(first), last, last == finalChunk, rng.Offset-first*ChunkSize, end-rng.Offset)
}

// emptyReader is a read past the end of an object.
type emptyReader struct{}

// Read implements io.Reader.
func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// opener builds the decrypting reader, clearing the data key once the
// cipher holds it.
func (d *Driver) opener(rc io.ReadCloser, hdr header, dek []byte, key string, first uint64, last int64, lastIsFinal bool, skip, limit int64) (io.ReadCloser, error) {
	aead, err := newAEAD(dek)
	clear(dek)
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	return &opener{
		src: rc, br: newChunkReader(rc), aead: aead, prefix: hdr.aadPrefix(), key: key,
		index: first, last: last, lastIsFinal: lastIsFinal, skip: skip, limit: limit,
		sealed: make([]byte, ChunkSize+TagSize),
	}, nil
}

// capability returns inner as the optional interface T, or CodeUnsupported
// when it does not implement it (a caller that skipped Capabilities).
func capability[T any](inner filesystem.Driver) (T, error) {
	x, ok := inner.(T)
	if !ok {
		return x, filesystem.Errorf(filesystem.CodeUnsupported, "crypt: the wrapped driver does not implement %T", (*T)(nil))
	}
	return x, nil
}

// Forwarded optional interfaces: each runs on the wrapped driver, whose
// capability the Disk has checked through Capabilities.

// SetVisibility implements filesystem.VisibilitySetter.
func (d *Driver) SetVisibility(ctx context.Context, key string, v filesystem.Visibility) error {
	x, err := capability[filesystem.VisibilitySetter](d.inner)
	if err != nil {
		return err
	}
	return x.SetVisibility(ctx, key, v)
}

// DeletePrefix implements filesystem.PrefixDeleter.
func (d *Driver) DeletePrefix(ctx context.Context, dir string) error {
	x, err := capability[filesystem.PrefixDeleter](d.inner)
	if err != nil {
		return err
	}
	return x.DeletePrefix(ctx, dir)
}

// DeleteKeys implements filesystem.KeysDeleter.
func (d *Driver) DeleteKeys(ctx context.Context, keys []string) error {
	x, err := capability[filesystem.KeysDeleter](d.inner)
	if err != nil {
		return err
	}
	return x.DeleteKeys(ctx, keys)
}

// SweepAbandoned implements filesystem.AbandonedSweeper.
func (d *Driver) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	x, err := capability[filesystem.AbandonedSweeper](d.inner)
	if err != nil {
		return 0, err
	}
	return x.SweepAbandoned(ctx, before)
}

// FindAbandoned implements filesystem.AbandonedFinder: the wrapped
// driver's leftovers, whose keys are this disk's (crypt keeps keys).
func (d *Driver) FindAbandoned(ctx context.Context, before time.Time) ([]filesystem.Abandoned, error) {
	x, err := capability[filesystem.AbandonedFinder](d.inner)
	if err != nil {
		return nil, err
	}
	return x.FindAbandoned(ctx, before)
}

// MakeDir implements filesystem.DirMaker: directories hold no bytes, so
// the wrapped driver's answer holds unchanged.
func (d *Driver) MakeDir(ctx context.Context, dir string) error {
	x, err := capability[filesystem.DirMaker](d.inner)
	if err != nil {
		return err
	}
	return x.MakeDir(ctx, dir)
}

// DirExists implements filesystem.DirMaker.
func (d *Driver) DirExists(ctx context.Context, dir string) (bool, error) {
	x, err := capability[filesystem.DirMaker](d.inner)
	if err != nil {
		return false, err
	}
	return x.DirExists(ctx, dir)
}

// SetModTime implements filesystem.ModTimeSetter.
func (d *Driver) SetModTime(ctx context.Context, key string, t time.Time) error {
	x, err := capability[filesystem.ModTimeSetter](d.inner)
	if err != nil {
		return err
	}
	return x.SetModTime(ctx, key, t)
}

// Tags implements filesystem.Tagger.
func (d *Driver) Tags(ctx context.Context, key string) (map[string]string, error) {
	x, err := capability[filesystem.Tagger](d.inner)
	if err != nil {
		return nil, err
	}
	return x.Tags(ctx, key)
}

// SetTags implements filesystem.Tagger.
func (d *Driver) SetTags(ctx context.Context, key string, tags map[string]string) error {
	x, err := capability[filesystem.Tagger](d.inner)
	if err != nil {
		return err
	}
	return x.SetTags(ctx, key, tags)
}

// Hold implements filesystem.Holder.
func (d *Driver) Hold(ctx context.Context, key string) (filesystem.Hold, error) {
	x, err := capability[filesystem.Holder](d.inner)
	if err != nil {
		return filesystem.Hold{}, err
	}
	return x.Hold(ctx, key)
}

// SetHold implements filesystem.Holder.
func (d *Driver) SetHold(ctx context.Context, key string, h filesystem.Hold) error {
	x, err := capability[filesystem.Holder](d.inner)
	if err != nil {
		return err
	}
	return x.SetHold(ctx, key, h)
}

// SetStorageClass implements filesystem.StorageClasser.
func (d *Driver) SetStorageClass(ctx context.Context, key, class string) error {
	x, err := capability[filesystem.StorageClasser](d.inner)
	if err != nil {
		return err
	}
	return x.SetStorageClass(ctx, key, class)
}

// Restore implements filesystem.Restorer.
func (d *Driver) Restore(ctx context.Context, key string, opts filesystem.RestoreOptions) error {
	x, err := capability[filesystem.Restorer](d.inner)
	if err != nil {
		return err
	}
	return x.Restore(ctx, key, opts)
}
