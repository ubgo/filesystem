package records

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// ServeKind names how a read handed a file, or something derived from it,
// to a client.
type ServeKind string

// The closed set of serve kinds. They share the "access." prefix, which no
// EventKind uses, so a log that keeps both (the audit plugin) can tell
// reads from changes.
const (
	// ServeLink is a URL to a file issued by Store.URL or DownloadURL:
	// recordshttp's file route and listings, and any application code.
	ServeLink ServeKind = "access.link"
	// ServeShare is a file handed out through a share link (the share
	// plugin's handler); Subject is the link's ID.
	ServeShare ServeKind = "access.share"
	// ServeVariant is a URL to a derived image (the imaging variants
	// handler, URL and Srcset); Subject is the variant's name.
	ServeVariant ServeKind = "access.variant"
	// ServeStream is bytes the application streamed itself, Bytes being
	// what it wrote.
	ServeStream ServeKind = "access.stream"
)

// ServeKindValues lists every built-in ServeKind.
var ServeKindValues = []ServeKind{ServeLink, ServeShare, ServeVariant, ServeStream}

// servePrefix starts every ServeKind, built-in or not, so a log that keeps
// reads and changes together tells them apart: no EventKind starts with it.
const servePrefix = "access."

// customServeKindPattern is what follows servePrefix in a kind a plugin or
// the application reports for its own reads: "<name>.<action>", the shape
// of a plugin's event kinds, so "access.zip.download" or
// "access.pdf.page" never collides with a built-in kind (which has one
// segment after the prefix).
var customServeKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

// Valid reports whether k is a kind Store.Served accepts: one of
// ServeKindValues, or a custom kind "access.<name>.<action>" (each part
// [a-z][a-z0-9_]*) of at most MaxEventKindBytes, for reads the built-in
// kinds do not name (a zip download, a rendered PDF page). Why custom
// kinds keep the "access." prefix: the audit plugin stores reads beside
// changes and tells them apart by it.
func (k ServeKind) Valid() bool {
	if slices.Contains(ServeKindValues, k) {
		return true
	}
	rest, ok := strings.CutPrefix(string(k), servePrefix)
	return ok && len(k) <= MaxEventKindBytes && customServeKindPattern.MatchString(rest)
}

// LinkOption describes a link Store.URL or DownloadURL issues, for the
// Served report they make.
type LinkOption func(*Served)

// ServedAs reports the link as kind with subject (ServeShare and the link
// ID, ServeVariant and the variant name) instead of ServeLink.
func ServedAs(kind ServeKind, subject string) LinkOption {
	return func(sv *Served) { sv.Kind, sv.Subject = kind, subject }
}

// Served is one read: what was handed out, to which scope, and how many
// bytes it can carry. Bytes is the size of the object a link points at
// (an upper bound: a client may not follow the link, or fetch a range),
// or, for ServeStream, the bytes actually written. A public URL never
// expires and can be fetched any number of times without the application
// seeing it, so for public files Served counts links issued; the bytes
// actually fetched are in the CDN's or the backend's access logs.
type Served struct {
	Kind   ServeKind
	Scope  string
	FileID string
	// Subject is the share link ID (ServeShare) or variant name
	// (ServeVariant); "" otherwise.
	Subject string
	Bytes   int64
	// At is stamped by Store.Served from the Store's clock.
	At time.Time
}

// ServeHooks is a plugin told of every read a handler reports with
// Store.Served: egress metering, an access log. OnServed runs outside any
// transaction of the Store's (a read changes nothing), so a hook that
// writes more than one statement opens its own with Store.Tx.
type ServeHooks interface {
	OnServed(ctx context.Context, s *Store, sv Served) error
}

// Served reports a read to every installed ServeHooks plugin, in install
// order. Store.URL and DownloadURL call it for every link they issue, so
// handlers built on them are metered without calling it; call it yourself
// only for bytes the application streams (ServeStream), before writing
// them, and refuse the request when it fails, so nothing is served that
// metering or an access log did not see. Every
// hook runs even when an earlier one fails, and their errors are joined:
// one plugin's failure must not hide the read from another. Kind must be
// valid (ServeKind.Valid: a built-in kind, or a custom
// "access.<name>.<action>") and Bytes not negative (CodeInvalidArgument).
// A read of a file that is quarantined now (read from the database, not
// from the caller's File) is refused with ErrQuarantined before any hook
// runs. With no ServeHooks plugin installed it does nothing else.
func (s *Store) Served(ctx context.Context, sv Served) error {
	if !sv.Kind.Valid() || sv.Bytes < 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: served %q with %d bytes is not a valid read", sv.Kind, sv.Bytes)
	}
	// Every link the library issues passes here, so this one check keeps
	// a quarantined file from being handed out by any path, whatever File
	// value the caller holds.
	if err := s.checkServable(ctx, sv.Scope, sv.FileID); err != nil {
		return err
	}
	sv.At = s.clock()
	var errs []error
	for _, p := range s.plugins {
		if h, ok := p.(ServeHooks); ok {
			if err := h.OnServed(ctx, s, sv); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
