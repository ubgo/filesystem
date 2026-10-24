package upload

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/sniff"
)

// Result describes a stored upload.
type Result struct {
	// Key is where the file is stored on the disk.
	Key string `json:"key"`
	// Filename is the client's file name, made safe, for display.
	Filename string `json:"filename"`
	// ContentType is the type detected from the content.
	ContentType string `json:"content_type"`
	// Size is the stored size in bytes.
	Size int64 `json:"size"`
	// SHA256 is the hex digest of the stored bytes, for deduplication and
	// integrity checks.
	SHA256 string `json:"sha256"`
	// Path is the client's relative path made safe (SafePath), for folder
	// uploads: "photos/2024/a.jpg"; the bare file name when the client sent
	// no directories. Display only, unless FolderKey builds keys from it.
	Path string `json:"path,omitempty"`
	// Checksums holds the digests Policy.Checksums asked for, by
	// filesystem.Checksum.Name, in lowercase hex; nil when the policy
	// asked for none, so a response without them is unchanged.
	Checksums map[string]string `json:"checksums,omitempty"`
}

// Receive stores one file from body on disk under policy p: it detects the
// type from the first bytes, refuses it with CodeRejected when the policy
// does not accept it, chooses the key with the policy's KeyFunc, and streams
// the rest with the policy's size cap (CodeTooLarge, nothing stored).
// filename is the client's name and is used for display only.
func Receive(ctx context.Context, disk *filesystem.Disk, body io.Reader, filename string, p Policy) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	head := make([]byte, sniff.HeadSize)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Result{}, err
	}
	head = head[:n]
	name, relPath := SafeFilename(filename), SafePath(filename)
	contentType := p.detect(head, name)
	if err := p.Accepts(contentType); err != nil {
		return Result{}, err
	}
	if err := p.AcceptsName(name, contentType); err != nil {
		return Result{}, err
	}
	var stream io.Reader = io.MultiReader(bytes.NewReader(head), body)
	if p.Process != nil {
		info, err := p.process(ctx, Info{Filename: name, ContentType: contentType}, &stream)
		if err != nil {
			return Result{}, err
		}
		name, contentType = info.Filename, info.ContentType
		relPath = path.Join(path.Dir(relPath), name)
	}
	info := Info{Filename: name, ContentType: contentType, Path: relPath}
	if p.Progress != nil {
		stream = &progressReader{r: stream, ctx: ctx, info: info, report: p.Progress}
	}
	key, err := p.key(ctx, info)
	if err != nil {
		return Result{}, err
	}
	headers, err := p.headers(info)
	if err != nil {
		return Result{}, err
	}
	key, conditional, err := p.claim(ctx, disk, key)
	if err != nil {
		return Result{}, err
	}
	e, err := disk.Write(ctx, key, stream, filesystem.WriteOptions{
		ContentType: contentType,
		Visibility:  p.Visibility,
		MaxSize:     p.MaxSize,
		Headers:     headers,
		IfNotExists: conditional,
		Checksums:   p.Checksums,
	})
	if err != nil {
		return Result{}, err
	}
	if err := p.checkMinSize(e.Size); err != nil {
		return Result{}, reject(ctx, disk, e.Key, err)
	}
	res, err := p.inspect(ctx, disk, Result{Key: e.Key, Filename: name, Path: relPath, ContentType: contentType, Size: e.Size, SHA256: e.SHA256, Checksums: e.Checksums})
	if err != nil {
		return Result{}, err
	}
	return p.settle(ctx, disk, res)
}

// process runs p.Process on *stream and checks what it produced against
// the policy again, replacing *stream with the processed body.
func (p Policy) process(ctx context.Context, in Info, stream *io.Reader) (Info, error) {
	out, info, err := p.Process(ctx, in, *stream)
	if err != nil {
		return Info{}, err
	}
	info.Filename = SafeFilename(orDefault(info.Filename, in.Filename))
	if info.ContentType == "" {
		info.ContentType = in.ContentType
	}
	if err := p.Accepts(info.ContentType); err != nil {
		return Info{}, err
	}
	*stream = out
	return info, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// randomName is 128 random bits as lowercase base32 (crypto/rand.Text, which
// cannot fail). Lowercase keeps keys stable on case-insensitive filesystems.
func randomName() string { return strings.ToLower(rand.Text()) }
