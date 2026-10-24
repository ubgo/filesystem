package upload

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
)

// Data URI syntax (RFC 2397) as FromDataURI accepts it.
const (
	dataURIScheme = "data:"
	base64Marker  = ";base64"
)

// FromBase64 stores a file sent as standard base64 text (RFC 4648 with
// padding; line breaks are ignored), as JSON APIs and mobile clients often
// send small files. It decodes while streaming and is Receive in every
// other respect: the type comes from the decoded bytes, the policy's size
// cap counts decoded bytes, nothing refused is stored. Text that is not
// valid base64 is CodeInvalidArgument. filename is for display only.
func FromBase64(ctx context.Context, disk *filesystem.Disk, encoded, filename string, p Policy) (Result, error) {
	body := &decodeErrors{r: base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))}
	return Receive(ctx, disk, body, filename, p)
}

// FromDataURI stores a file sent as a base64 data URI,
// "data:image/png;base64,iVBOR…", as a canvas export or a pasted image
// arrives. The media type the URI declares is ignored: the type is
// detected from the decoded bytes, as for every upload. Only base64 data
// URIs are accepted; anything else is CodeInvalidArgument. See FromBase64.
func FromDataURI(ctx context.Context, disk *filesystem.Disk, uri, filename string, p Policy) (Result, error) {
	if len(uri) < len(dataURIScheme) || !strings.EqualFold(uri[:len(dataURIScheme)], dataURIScheme) {
		return Result{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "not a data URI")
	}
	meta, payload, ok := strings.Cut(uri[len(dataURIScheme):], ",")
	if !ok || !strings.HasSuffix(strings.ToLower(meta), base64Marker) {
		return Result{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "only base64 data URIs are accepted")
	}
	return FromBase64(ctx, disk, payload, filename, p)
}

// decodeErrors classifies a decoding failure as the client's input, so it
// surfaces as CodeInvalidArgument rather than an unknown error.
type decodeErrors struct{ r io.Reader }

func (d *decodeErrors) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid base64: %v", err)
	}
	return n, err
}

// Promote moves an upload kept in a cache area to its permanent key, the
// second step of two-phase uploads: store the file under cacheDir as soon
// as it arrives (Receive with a RandomKey(cacheDir) policy), let the form
// be redisplayed or the record be validated, then promote it, possibly to
// another disk. Uploads that are never promoted are left for Sweep on
// cacheDir.
//
// key usually comes back from the client, so it must lie under cacheDir
// on cache, or the call fails with CodePermission and nothing moves: a
// client cannot promote, and so take over, any other object. cacheDir must
// not be the root (CodeInvalidArgument). Within one disk this is a Move;
// across disks a copy, then the cached object's deletion
// (filesystem.MoveAcross).
func Promote(ctx context.Context, cache *filesystem.Disk, cacheDir, key string, store *filesystem.Disk, storeKey string) (filesystem.Entry, error) {
	dir, err := filesystem.CleanDir(cacheDir)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if dir == "" {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "the cache area cannot be the root of disk %s", cache.Name())
	}
	k, err := filesystem.CleanKey(key)
	if err != nil {
		return filesystem.Entry{}, err
	}
	if !filesystem.KeyInDir(k, dir) {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodePermission, "the key is not a cached upload")
	}
	return filesystem.MoveAcross(ctx, cache, k, store, storeKey)
}
