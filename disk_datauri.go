package filesystem

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"strings"

	"github.com/ubgo/filesystem/sniff"
)

// dataURICharset is the only media type parameter DataURI keeps.
const dataURICharset = "charset"

// DataURI returns key as an RFC 2397 data URI, "data:<type>;base64,<bytes>",
// for inlining a tiny file (an icon, a placeholder image) into HTML, CSS or
// JSON without a second request.
//
// It refuses, before reading a byte, anything Stat reports larger than
// maxBytes (CodeTooLarge) and stored active content (HTML, SVG, XML,
// JavaScript; CodeRejected), because a data URI of an active type runs
// script wherever it is embedded. The read itself stops at maxBytes + 1,
// since a backend's reported size can be stale, and the bytes are sniffed:
// content that is active whatever its stored type is refused too. The type
// is the stored one (only its charset parameter is kept), else the sniffed
// one. maxBytes must be positive (CodeInvalidArgument); the caller chooses
// it, since what counts as tiny depends on where the URI is embedded, and
// the base64 output is a third larger than the file.
func (d *Disk) DataURI(ctx context.Context, key string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		return "", annotate(Errorf(CodeInvalidArgument, "data URI limit %d is not positive", maxBytes), OpRead, d.name, key)
	}
	e, err := d.Stat(ctx, key)
	if err != nil {
		return "", err
	}
	if e.Size > maxBytes {
		return "", annotate(Errorf(CodeTooLarge, "object is %d bytes, more than the data URI limit %d", e.Size, maxBytes), OpRead, d.name, e.Key)
	}
	if sniff.IsActive(e.ContentType) {
		return "", annotate(Errorf(CodeRejected, "active content type %q cannot be a data URI", e.ContentType), OpRead, d.name, e.Key)
	}
	rc, err := d.Read(ctx, e.Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return "", annotate(err, OpRead, d.name, e.Key)
	}
	if int64(len(body)) > maxBytes {
		return "", annotate(Errorf(CodeTooLarge, "object is more than the data URI limit %d bytes", maxBytes), OpRead, d.name, e.Key)
	}
	detected := sniff.DetectWith(d.detector, body, e.Key)
	if sniff.IsActive(detected) {
		return "", annotate(Errorf(CodeRejected, "content is %s, which cannot be a data URI", detected), OpRead, d.name, e.Key)
	}
	return "data:" + dataURIType(e.ContentType, detected) + ";base64," + base64.StdEncoding.EncodeToString(body), nil
}

// dataURIType is stored as a bare media type plus a token charset, or
// detected when stored is empty or malformed, so no parameter value can
// inject a "," or ";" into the URI.
func dataURIType(stored, detected string) string {
	mt, params, err := mime.ParseMediaType(stored)
	if err != nil {
		mt, params, _ = mime.ParseMediaType(detected)
	}
	if mt == "" {
		return sniff.OctetStream
	}
	if cs := params[dataURICharset]; cs != "" && strings.Trim(cs, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.:") == "" {
		return mt + ";" + dataURICharset + "=" + cs
	}
	return mt
}
