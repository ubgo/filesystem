// Package vision tags image files with what they show ("dog", "beach",
// "invoice"), so an application can search and group pictures nobody
// tagged by hand. A Labeler names what an image shows; the google
// package is one, calling Google Cloud Vision's label detection over
// plain REST. Plugin writes the labels of every image recorded in an
// opted-in scope as tags through records/tags, after the upload commits,
// and Plugin.Label does the same for one file on demand (a backfill, or a
// worker taking the call off the request).
//
// Labelling is opt-in per scope and off for every scope by default,
// because a hosted labeler bills each image (see the guide for Google's
// prices). It is a module of its own because it calls a vendor's
// service; the core never does.
package vision

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/tags"
)

// Label is one thing a Labeler sees in an image.
type Label struct {
	// Name is what it is, in the labeler's words ("Golden retriever").
	Name string
	// Score is the labeler's confidence, from 0 to 1.
	Score float64
}

// Labeler names what an image shows. It reads the image from r and
// returns its labels in any order. It must bound its own work: the image
// size it accepts, the time it waits, the response it reads. The google
// package is one; a fake, a self-hosted model or another vendor is
// another.
type Labeler interface {
	Labels(ctx context.Context, image io.Reader) ([]Label, error)
}

// Defaults used when an Option does not set them.
const (
	// DefaultMinScore drops labels the labeler is less sure of: below it,
	// labels are mostly noise ("Font", "Rectangle").
	DefaultMinScore = 0.7
	// DefaultMaxLabels bounds the tags one image gets.
	DefaultMaxLabels = 10
	// DefaultMaxInput bounds the bytes of an image read for the labeler;
	// Google's REST API takes about 7.5 MiB once base64-encoded.
	DefaultMaxInput int64 = 7 << 20
)

// DefaultContentTypes returns the image types labelled when
// WithContentTypes is not used: the formats web labelers accept.
func DefaultContentTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

// PluginName is the records plugin's name.
const PluginName = "vision"

// LabelSink receives the labels the Plugin keeps for a file: Add stores
// names and returns every label the file now has, Tags returns them
// without adding any. *tags.Plugin is the default sink (labels become
// tags); another one sends them to a search index, a metadata key or a
// moderation table. Names arrive prefixed (WithPrefix), best scored
// first, at most WithMaxLabels of them, each a valid tag name (tags.Check)
// so a sink can switch back to tags without surprises. A sink error is
// Label's error; after an upload it goes to WithErrorHandler.
type LabelSink interface {
	Add(ctx context.Context, s *records.Store, scope, fileID string, names ...string) ([]string, error)
	Tags(ctx context.Context, s *records.Store, scope, fileID string) ([]string, error)
}

// The tags plugin is the default sink.
var _ LabelSink = (*tags.Plugin)(nil)

// Plugin labels image files and writes the labels as tags. Construct it
// with NewPlugin. Installed with records.WithPlugins (beside the tags
// plugin), it labels every image recorded in a scope the scopes function
// accepts, once the upload has committed; not installed, it labels only
// what Label is called for.
type Plugin struct {
	labeler Labeler
	// tags is where labels go: the tags plugin, or any LabelSink.
	tags      LabelSink
	scopes    func(scope string) bool
	minScore  float64
	maxLabels int
	maxInput  int64
	prefix    string
	types     []string
	onError   func(ctx context.Context, f records.File, err error)
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithMinScore keeps only labels scored at least min (0 to 1); the
// default is DefaultMinScore.
func WithMinScore(min float64) Option { return func(p *Plugin) { p.minScore = min } }

// WithMaxLabels keeps at most n labels per image, the best scored first;
// the default is DefaultMaxLabels. A file holds at most tags.MaxPerFile
// tags in all.
func WithMaxLabels(n int) Option { return func(p *Plugin) { p.maxLabels = n } }

// WithMaxInput bounds the bytes of an image sent to the labeler; a larger
// file is CodeTooLarge and is not sent, so it costs nothing. The default
// is DefaultMaxInput.
func WithMaxInput(n int64) Option { return func(p *Plugin) { p.maxInput = n } }

// WithPrefix puts prefix before every label ("auto:" makes "auto:dog"),
// so labels never mix with the tags people type and can be filtered or
// removed as a set. The default is none.
func WithPrefix(prefix string) Option { return func(p *Plugin) { p.prefix = prefix } }

// WithContentTypes replaces DefaultContentTypes: the types labelled.
func WithContentTypes(types ...string) Option { return func(p *Plugin) { p.types = types } }

// WithErrorHandler receives the failures of labelling after an upload
// commits (the upload has succeeded by then, so there is no caller to
// return them to). The default logs them with slog.Default at error level.
func WithErrorHandler(fn func(ctx context.Context, f records.File, err error)) Option {
	return func(p *Plugin) { p.onError = fn }
}

// NewPlugin returns a Plugin labelling with labeler and writing through
// t: the tags plugin (which must be installed in the Store), or another
// LabelSink. scopes says which scopes are labelled; nil labels none.
func NewPlugin(labeler Labeler, t LabelSink, scopes func(scope string) bool, opts ...Option) *Plugin {
	p := &Plugin{
		labeler: labeler, tags: t, scopes: scopes,
		minScore: DefaultMinScore, maxLabels: DefaultMaxLabels, maxInput: DefaultMaxInput,
		types: DefaultContentTypes(), onError: logError,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// logError is the default error handler.
func logError(ctx context.Context, f records.File, err error) {
	slog.Default().ErrorContext(ctx, "vision: labelling failed", "scope", f.Scope, "file", f.ID, "error", err)
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks: an image of an opted-in scope
// is labelled once the transaction commits, never for a rolled-back
// upload. A failure goes to the error handler. A quarantined image is not
// sent to the labeler until its scan passes (OnEvent).
func (p *Plugin) AfterCreate(ctx context.Context, s *records.Store, f records.File) error {
	if f.Quarantined() {
		return nil
	}
	p.labelAfterCommit(ctx, s, f)
	return nil
}

// OnEvent implements records.EventHooks: a quarantined image is labelled
// when its scan passes (file.released), as AfterCreate labels any other.
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == records.EventFileReleased && e.File != nil {
		p.labelAfterCommit(ctx, s, *e.File)
	}
	return nil
}

// labelAfterCommit labels f once the transaction in ctx commits, if the
// plugin wants it; a failure goes to the error handler.
func (p *Plugin) labelAfterCommit(ctx context.Context, s *records.Store, f records.File) {
	if !p.wants(f) {
		return
	}
	s.AfterCommit(ctx, func(ctx context.Context) {
		if _, err := p.Label(ctx, s, f.Scope, f.ID); err != nil {
			p.onError(ctx, f, err)
		}
	})
}

// BeforePurge implements records.FileHooks; the tags plugin removes the
// file's tags.
func (p *Plugin) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// Label labels the file id of scope now and adds the labels as tags,
// returning the file's tags. It refuses (CodeInvalidArgument) a scope not
// opted in or a type not labelled, and (CodeTooLarge) a file over the
// input bound, and (records.ErrQuarantined) a quarantined file, before
// the labeler is called. A label that is not a valid tag (too long once
// prefixed) is skipped.
func (p *Plugin) Label(ctx context.Context, s *records.Store, scope, id string) ([]string, error) {
	f, err := s.File(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	// Unscanned bytes are never sent to the labeler (an outside service).
	if f.Quarantined() {
		return nil, records.ErrQuarantined
	}
	if !p.optedIn(scope) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vision: scope %q is not opted in to labelling", scope)
	}
	if !p.labels(f.ContentType) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "vision: %s files are not labelled", mediaType(f.ContentType))
	}
	if f.Size > p.maxInput {
		return nil, filesystem.Errorf(filesystem.CodeTooLarge, "vision: larger than %d bytes", p.maxInput)
	}
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return nil, err
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	labels, err := p.labeler.Labels(ctx, io.LimitReader(rc, p.maxInput))
	if err != nil {
		return nil, err
	}
	names := p.choose(labels)
	if len(names) == 0 {
		return p.tags.Tags(ctx, s, scope, id)
	}
	return p.tags.Add(ctx, s, scope, id, names...)
}

// choose keeps the labels scored at least minScore, best first, as at
// most maxLabels prefixed tag names, skipping any that is not a valid tag
// or repeats one already kept (ignoring case, as tags are usually
// lowercased).
func (p *Plugin) choose(labels []Label) []string {
	labels = slices.DeleteFunc(slices.Clone(labels), func(l Label) bool { return l.Score < p.minScore })
	slices.SortStableFunc(labels, func(a, b Label) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	var names []string
	for _, l := range labels {
		if len(names) == p.maxLabels {
			break
		}
		name := p.prefix + strings.TrimSpace(l.Name)
		if _, err := tags.Check(name); err != nil || slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, name) }) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// wants reports whether f is labelled after its upload.
func (p *Plugin) wants(f records.File) bool {
	return p.optedIn(f.Scope) && p.labels(f.ContentType)
}

func (p *Plugin) optedIn(scope string) bool { return p.scopes != nil && p.scopes(scope) }

func (p *Plugin) labels(contentType string) bool {
	mt := mediaType(contentType)
	return slices.ContainsFunc(p.types, func(t string) bool { return mediaType(t) == mt })
}

// mediaType strips parameters from a content type and lowercases it.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(strings.ToLower(mt))
}
