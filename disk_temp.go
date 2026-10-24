package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
)

// TempFileOptions configure Disk.TempFile. The zero value writes to the
// system temporary directory with no size limit.
type TempFileOptions struct {
	// Dir is the directory the file is created in; "" means os.TempDir.
	Dir string
	// MaxSize, when positive, refuses an object larger than this many bytes
	// with CodeTooLarge, checked from Stat before anything is written and
	// again while copying, so an object that grew in between cannot fill
	// the disk.
	MaxSize int64
}

// tempFilePattern names temporary files: a recognisable prefix, a random
// part (os.CreateTemp's "*"), then the key's extension.
const tempFilePattern = "filesystem-*"

// maxTempExtension bounds the extension copied from the key, so a key with
// a long dotted tail cannot produce an unwieldy name.
const maxTempExtension = 16

// TempFile copies key to a new file on local disk and returns its path,
// for tools that need a path rather than a stream (ffmpeg, libvips,
// pdftotext). The file keeps the key's extension when it is short and
// alphanumeric, since such tools often go by it, and is readable by the
// current user only.
//
// The caller owns the file and must remove it (defer os.Remove(path)).
// On any error nothing is left behind: a failed or refused copy removes
// its partial file. Errors carry the read operation, this disk and the key.
func (d *Disk) TempFile(ctx context.Context, key string, opts TempFileOptions) (string, error) {
	e, err := d.Stat(ctx, key)
	if err != nil {
		return "", err
	}
	tooLarge := annotate(Errorf(CodeTooLarge, "object exceeds %d bytes", opts.MaxSize), OpRead, d.name, e.Key)
	if opts.MaxSize > 0 && e.Size > opts.MaxSize {
		return "", tooLarge
	}
	body, err := d.Read(ctx, e.Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	f, err := os.CreateTemp(opts.Dir, tempFilePattern+tempExtension(e.Key))
	if err != nil {
		return "", annotate(err, OpRead, d.name, e.Key)
	}
	var src io.Reader = body
	if opts.MaxSize > 0 {
		src = io.LimitReader(body, opts.MaxSize+1)
	}
	n, err := io.Copy(f, src)
	err = errors.Join(err, f.Close())
	if err == nil && opts.MaxSize > 0 && n > opts.MaxSize {
		err = tooLarge
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", annotate(err, OpRead, d.name, e.Key)
	}
	return f.Name(), nil
}

// tempExtension is key's extension when it is short and made of letters
// and digits only, else "".
func tempExtension(key string) string {
	ext := path.Ext(key)
	if len(ext) < 2 || len(ext) > maxTempExtension {
		return ""
	}
	for _, c := range ext[1:] {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return ""
		}
	}
	return ext
}
