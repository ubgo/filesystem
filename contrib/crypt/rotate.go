package crypt

import (
	"bytes"
	"context"
	"io"

	"github.com/ubgo/filesystem"
)

// Rotate re-wraps key's data key under the master key the keyring now
// chooses for it, and nothing else: the data key and every chunk stay as
// they are, so no plaintext is decrypted and the chunks' ciphertext is
// copied byte for byte behind the new header. Run it over every object
// (List the disk, Rotate each key) after the keyring starts choosing a new
// master key; once every object is rotated, the old master key can be
// retired.
//
// Object stores cannot rewrite part of an object, so the object is
// written again through the wrapped driver, keeping its content type,
// visibility, metadata and headers. When the wrapped driver has
// CompareAndSwap, the write applies only if the object is still the one
// read (CodeConflict otherwise: rotate it again); without it, a write that
// lands between the read and the rotation's write is overwritten.
func (d *Driver) Rotate(ctx context.Context, key string) error {
	k, err := filesystem.CleanKey(key)
	if err != nil {
		return err
	}
	st, err := d.inner.Stat(ctx, k)
	if err != nil {
		return err
	}
	rc, err := d.inner.Read(ctx, k, filesystem.ByteRange{})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	hdr, dek, err := d.open(ctx, k, rc)
	if err != nil {
		return err
	}
	hdr.wrapped, err = d.keyring.WrapKey(ctx, k, dek)
	clear(dek)
	if err != nil {
		return err
	}
	encoded, err := hdr.encode()
	if err != nil {
		return err
	}
	opts := filesystem.WriteOptions{
		ContentType: st.ContentType,
		Metadata:    st.Metadata,
		Headers:     st.Headers,
		SizeHint:    st.Size,
	}
	if st.Visibility.Valid() {
		opts.Visibility = st.Visibility
	}
	if d.inner.Capabilities().CompareAndSwap {
		opts.IfMatch = st.ETag
	}
	_, err = d.inner.Write(ctx, k, io.MultiReader(bytes.NewReader(encoded), rc), opts)
	return err
}
