package records

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
)

// Source is where a file came from: the channel it arrived through, the
// client that sent it, and the client's IP address. The library never
// interprets the values; the application chooses them (channel "web",
// "ios", "api", "import"; client a user agent or an API key's name).
type Source struct {
	// Channel is up to MaxSourceChannelBytes.
	Channel string
	// Client is up to MaxSourceClientBytes.
	Client string
	// IP is up to MaxSourceIPBytes, and kept only when the Store was made
	// WithSourceIP: an IP address is personal data, so storing it is the
	// application's explicit choice.
	IP string
}

// Limits of Source, the widths of their columns.
const (
	MaxSourceChannelBytes = 64
	MaxSourceClientBytes  = 255
	// MaxSourceIPBytes fits any IPv6 address with a zone.
	MaxSourceIPBytes = 64
)

type sourceKey struct{}

// WithSource returns ctx carrying the source of the files recorded with
// it. Set it once per request, beside WithActor, in the middleware that
// knows the channel and client; CreateFile and Upload record it as
// File.Source unless NewFile.Source is set.
func WithSource(ctx context.Context, src Source) context.Context {
	return context.WithValue(ctx, sourceKey{}, src)
}

// SourceFrom returns the source set with WithSource, or the zero Source.
func SourceFrom(ctx context.Context) Source {
	src, _ := ctx.Value(sourceKey{}).(Source)
	return src
}

// WithSourceIP keeps Source.IP when a file is recorded. Without it the IP
// is dropped before the row is written, whatever the context or NewFile
// carries, so a Store never holds addresses its application did not
// decide to keep.
func WithSourceIP() Option {
	return func(s *Store) error { s.keepSourceIP = true; return nil }
}

// sourceOf is the source a new file records: in when set, else the
// context's, without the IP unless WithSourceIP.
func (s *Store) sourceOf(ctx context.Context, in Source) Source {
	src := in
	if src == (Source{}) {
		src = SourceFrom(ctx)
	}
	if !s.keepSourceIP {
		src.IP = ""
	}
	return src
}

// checkSource refuses a source wider than its columns, rather than
// cutting it short.
func checkSource(src Source) error {
	if len(src.Channel) > MaxSourceChannelBytes || len(src.Client) > MaxSourceClientBytes || len(src.IP) > MaxSourceIPBytes {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "records: a source is at most %d bytes of channel, %d of client and %d of IP",
			MaxSourceChannelBytes, MaxSourceClientBytes, MaxSourceIPBytes)
	}
	return nil
}

// utcPtr returns t in UTC at the Store's microsecond precision, or nil.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC().Truncate(time.Microsecond)
	return &u
}
