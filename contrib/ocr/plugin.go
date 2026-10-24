package ocr

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// PluginName is the records plugin's name.
const PluginName = "ocr"

// MetaTextKey is the metadata key holding the key of an image file's
// text on the file's disk. It is the plugin's own key, not contrib/pdf's
// "text_key", so the two plugins never clear each other's.
const MetaTextKey = "ocr_text_key"

// MetaTextDisk names the text's disk when WithLocation put it on another
// disk than the image's; absent when it is on the image's own.
const MetaTextDisk = "ocr_text_disk"

// DerivedText is the name the plugin passes to a Location for a text.
const DerivedText = "text"

// Location chooses where the plugin stores a derived object of f (name is
// DerivedText): the name of a disk of the Store's Manager ("" for f's
// own disk) and a key prefix. The plugin appends a random part and
// TextSuffix to the prefix, so every run writes a key no other run
// writes: a replacement's text never overwrites the current one before
// the replacement is read, and the deferred deletion of an old text
// never removes a new one. An error fails closed: nothing is written and
// the image is marked StatusFailed.
type Location func(f records.File, name string) (disk, prefix string, err error)

// defaultLocation stores a text beside its image, on the image's disk:
// <image key><TextInfix><random><TextSuffix>.
func defaultLocation(f records.File, _ string) (string, string, error) {
	return "", f.Key + TextInfix, nil
}

// Texts are stored next to their image as
// <image key><TextInfix><random><TextSuffix>, as TextContentType. The
// random part keeps a replacement's text from overwriting the current
// one before the replacement is read.
const (
	TextInfix       = ".ocr-"
	TextSuffix      = ".txt"
	TextContentType = "text/plain; charset=utf-8"
	// textTokenLen is how many characters of rand.Text name a text.
	textTokenLen = 10
)

// StatusDetail is the records status detail of an image the plugin is
// reading; a processing status with another detail belongs to another
// plugin and is left alone. A failure's detail is StatusFailedPrefix and
// the error code ("ocr: unavailable").
const (
	StatusDetail       = "ocr: processing"
	StatusFailedPrefix = "ocr: "
)

// DefaultProcessTimeout bounds one Process run.
const DefaultProcessTimeout = 5 * time.Minute

// settleTimeout bounds the transaction that records how a Process run
// ended. It runs on a context detached from the run's own deadline, so a
// run cut short by WithProcessTimeout still records StatusFailed instead
// of leaving the file in StatusProcessing until Resume; the bound keeps a
// stuck database from holding Process forever in its place.
const settleTimeout = 30 * time.Second

// Plugin is a records plugin that reads the text of every PNG, JPEG and
// GIF file (or of the types WithContentTypes names) when it is recorded
// and when its content is replaced, and stores it beside the file for a search indexer, its key in MetaTextKey
// (none when the image holds no text). Recognition never runs inside the
// upload's transaction: the file is marked records.StatusProcessing in
// it, and Process runs once it commits, outside any transaction and
// under its own timeout, then records the text and marks the file
// StatusReady, or StatusFailed with the reason (a missing program, an
// image the engine cannot read), never failing the upload. The text
// follows the file: deleted after a purge commits and when the content
// is replaced. Construct it with NewPlugin and install it with
// records.WithPlugins.
type Plugin struct {
	tool Recognizer
	// types are the content types the plugin reads; ContentTypes()
	// unless WithContentTypes.
	types     []string
	timeout   time.Duration
	deferWork bool
	// location places texts; defaultLocation unless WithLocation.
	location Location
}

// PluginOption configures a Plugin.
type PluginOption func(*Plugin)

// WithProcessTimeout bounds one Process run; the default is
// DefaultProcessTimeout.
func WithProcessTimeout(d time.Duration) PluginOption { return func(p *Plugin) { p.timeout = d } }

// WithDeferredProcessing leaves recognition to Resume: an upload only
// marks the image processing and returns. Without it (the default, which
// needs no setup) Process runs inline once the upload commits, outside
// the transaction but still inside the Upload call, so the caller waits
// for it: up to WithProcessTimeout per upload, even after its client has
// gone away. With it, schedule Resume (jobs.Func every minute, say).
func WithDeferredProcessing() PluginOption { return func(p *Plugin) { p.deferWork = true } }

// WithLocation stores texts where l says instead of beside their image on
// its disk: a bucket a search indexer reads, or a path it watches. The
// text's disk is recorded in MetaTextDisk when it is not the image's, so
// TextLocation, the purge and a replacement find it. A nil l keeps the
// default.
func WithLocation(l Location) PluginOption {
	return func(p *Plugin) {
		if l != nil {
			p.location = l
		}
	}
}

// Recognizer reads the text in an image. Tool (Tesseract) is the
// built-in one; a cloud service (AWS Textract, Google Vision, Azure
// Document Intelligence) is another, wrapped in a type with this one
// method. It has the same method set as contrib/pdf's Recognizer, so one
// value serves both plugins.
//
// Invariant: the plugin hands it the stored bytes of a file whose type is
// one of the plugin's content types and trusts it to bound its own work
// (input size, pixels, time, output), as Tool does through its limits.
// An error marks the file StatusFailed with the error's code; the upload
// itself never fails.
type Recognizer interface {
	Recognize(ctx context.Context, image io.Reader) (string, error)
}

// WithContentTypes sets the content types the plugin reads, replacing
// ContentTypes() (PNG, JPEG, GIF), which is what Tool reads. Name only
// types the Recognizer reads: TIFF and WebP scans for a cloud OCR
// service, say. Types are matched without case and without parameters.
// An empty list keeps the default.
func WithContentTypes(types ...string) PluginOption {
	return func(p *Plugin) {
		if len(types) == 0 {
			return
		}
		p.types = make([]string, 0, len(types))
		for _, t := range types {
			p.types = append(p.types, mediaType(t))
		}
	}
}

// NewPlugin returns a Plugin running r: a Tool for Tesseract, or any
// other Recognizer. A Tool reads only ContentTypes(); pair another
// engine with WithContentTypes for the types it reads.
func NewPlugin(r Recognizer, opts ...PluginOption) *Plugin {
	p := &Plugin{tool: r, types: ContentTypes(), timeout: DefaultProcessTimeout, location: defaultLocation}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// BeforeCreate implements records.FileHooks: a stale text key from the
// caller's metadata is dropped.
func (p *Plugin) BeforeCreate(_ context.Context, _ *records.Store, f *records.File) error {
	clearKey(f)
	return nil
}

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: the text goes with the file,
// after the purge commits.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	deleteText(ctx, s, f)
	return nil
}

// BeforeReplace implements records.ReplaceHooks: the old text is deleted
// once the replacement commits; the new content is read by Process.
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, current records.File, next *records.File) error {
	clearKey(next)
	deleteText(ctx, s, current)
	return nil
}

// OnEvent implements records.EventHooks: an image recorded or replaced is
// marked processing in the change's own transaction, and Process runs
// once the change commits. A rolled-back change runs nothing. A
// quarantined image is left alone (its bytes are unscanned) until its
// scan passes, whose file.released starts the same work.
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	switch e.Kind {
	case records.EventFileCreated, records.EventFileReplaced, records.EventFileReleased:
	default:
		return nil
	}
	if e.File == nil || !p.reads(*e.File) || e.File.Quarantined() {
		return nil
	}
	if _, err := s.SetStatus(ctx, e.Scope, e.FileID, records.StatusProcessing, StatusDetail); err != nil {
		return err
	}
	if !p.deferWork {
		s.AfterCommit(ctx, func(ctx context.Context) { _ = p.Process(ctx, s, e.Scope, e.FileID) })
	}
	return nil
}

// resumeBatch is how many rows Resume reads per query.
const resumeBatch = 100

// Resume runs Process for every file, in any scope, the plugin marked
// processing and recorded before olderThan, and returns how many it ran
// and their failures joined (each is also on its file's status). It
// finishes what a crash interrupted (Process runs in memory after the
// commit) and, with WithDeferredProcessing, does all the work. Schedule
// it from the application, with jobs.Func. Pick olderThan past
// WithProcessTimeout so a run still in progress elsewhere is left alone;
// with WithDeferredProcessing use the current time. A file replaced or
// processed meanwhile is protected as in Process.
func (p *Plugin) Resume(ctx context.Context, s *records.Store, olderThan time.Time) (int, error) {
	var ran int
	var errs []error
	after := ""
	for {
		var ref struct{ scope, id string }
		var batch []struct{ scope, id string }
		q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(records.FileScope, &ref.scope), sqlb.Bind(records.FileID, &ref.id)).
			From(records.TableFiles).
			Where(sqlb.Eq(records.FileStatus, records.StatusProcessing), sqlb.Eq(records.FileStatusDetail, StatusDetail),
				sqlb.Less(records.FileCreatedAt, olderThan.UTC()), sqlb.Greater(records.FileID, after)).
			Order(sqlb.OrderBy(records.FileID, sqlb.SortAsc)).Limit(resumeBatch)
		if err := s.Each(ctx, q, func() error { batch = append(batch, ref); return nil }); err != nil {
			return ran, errors.Join(append(errs, err)...)
		}
		for _, f := range batch {
			ran++
			if err := p.Process(ctx, s, f.scope, f.id); err != nil {
				errs = append(errs, err)
			}
			after = f.id
		}
		if len(batch) < resumeBatch || ctx.Err() != nil {
			return ran, errors.Join(errs...)
		}
	}
}

// TextKey returns the key of f's text; ok is false when there is none.
// The text is on f's disk unless WithLocation chose another:
// TextLocation returns both.
func TextKey(f records.File) (string, bool) {
	key := f.Metadata[MetaTextKey]
	return key, key != ""
}

// TextLocation returns the disk and key of f's text: the disk is
// MetaTextDisk when WithLocation put it elsewhere, else f's own. ok is
// false when there is no text.
func TextLocation(f records.File) (disk, key string, ok bool) {
	key, ok = TextKey(f)
	if !ok {
		return "", "", false
	}
	disk = f.Metadata[MetaTextDisk]
	if disk == "" {
		disk = f.Disk
	}
	return disk, key, true
}

// Process reads the text of the image id of scope, stores it beside the
// file and records its key, then marks the file StatusReady; a failure
// marks it StatusFailed with StatusFailedPrefix and the error code, and
// is returned. An image without text, or one the engine cannot read, is
// recorded without text. The plugin runs it after each upload commits;
// call it from a job to retry or backfill. Content replaced while it ran
// is left to the replacement's own run: the text it made is deleted. A
// quarantined image is refused with records.ErrQuarantined, unread.
func (p *Plugin) Process(ctx context.Context, s *records.Store, scope, id string) error {
	run, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	end, cancelEnd := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancelEnd()
	f, err := s.File(run, scope, id)
	if err != nil {
		return err
	}
	// Unscanned bytes are never handed to the engine; the release runs
	// Process again.
	if f.Quarantined() {
		return records.ErrQuarantined
	}
	if f.DeletedAt != nil || !p.reads(f) {
		return p.settle(end, s, f, nil, nil)
	}
	t, err := p.read(run, s, f)
	if filesystem.CodeOf(err) == filesystem.CodeRejected {
		t, err = &stored{}, nil // unreadable: recorded without text
	}
	if err != nil {
		return errors.Join(err, p.settle(end, s, f, nil, err))
	}
	return p.settle(end, s, f, t, nil)
}

// stored is the outcome of one read: the text's disk, the disk's name and
// its key, all zero when the image holds no text.
type stored struct {
	disk     *filesystem.Disk
	diskName string
	key      string
}

// read recognizes f's image and stores the text where the Location puts
// it, returning a key "" when there is no text. A Location error fails
// closed, before anything is written.
func (p *Plugin) read(ctx context.Context, s *records.Store, f records.File) (*stored, error) {
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return nil, err
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	text, err := p.tool.Recognize(ctx, rc)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return &stored{}, nil
	}
	diskName, prefix, err := p.location(f, DerivedText)
	if err != nil {
		return nil, err
	}
	if diskName == "" {
		diskName = f.Disk
	}
	dst, err := s.Disks().Disk(diskName)
	if err != nil {
		return nil, err
	}
	key := prefix + strings.ToLower(rand.Text()[:textTokenLen]) + TextSuffix
	if _, err := dst.WriteBytes(ctx, key, []byte(text), filesystem.WriteOptions{ContentType: TextContentType}); err != nil {
		return nil, err
	}
	return &stored{disk: dst, diskName: diskName, key: key}, nil
}

// settle records the end of a Process run in one transaction with the
// row locked: the text's key and disk when t is not nil (none for a key
// "") and the status. When the file
// was replaced, trashed or purged meanwhile, the text is deleted and the
// row left alone. A status another plugin set is never touched.
func (p *Plugin) settle(ctx context.Context, s *records.Store, f records.File, t *stored, failure error) error {
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		lock := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.SetExpr(records.FileKey, records.FileKey)).
			Where(sqlb.Eq(records.FileScope, f.Scope), sqlb.Eq(records.FileID, f.ID))
		if _, err := s.Exec(ctx, lock); err != nil {
			return err
		}
		cur, err := s.File(ctx, f.Scope, f.ID)
		if filesystem.CodeOf(err) == filesystem.CodeNotFound {
			return errStale
		}
		if err != nil {
			return err
		}
		if cur.Key != f.Key || cur.SHA256 != f.SHA256 || cur.DeletedAt != nil {
			return errStale
		}
		if t != nil {
			deleteText(ctx, s, cur)
			meta := maps.Clone(cur.Metadata)
			if meta == nil {
				meta = map[string]string{}
			}
			delete(meta, MetaTextKey)
			delete(meta, MetaTextDisk)
			if t.key != "" {
				meta[MetaTextKey] = t.key
				if t.diskName != cur.Disk {
					meta[MetaTextDisk] = t.diskName
				}
			}
			text, _ := json.Marshal(meta) // a map of strings cannot fail to marshal
			upd := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileMetadata, string(text))).
				Where(sqlb.Eq(records.FileScope, cur.Scope), sqlb.Eq(records.FileID, cur.ID))
			if _, err := s.Exec(ctx, upd); err != nil {
				return err
			}
		}
		if !ownStatus(cur) {
			return nil
		}
		st, detail := records.StatusReady, ""
		if failure != nil {
			st, detail = records.StatusFailed, StatusFailedPrefix+string(filesystem.CodeOf(failure))
		}
		_, err = s.SetStatus(ctx, f.Scope, f.ID, st, detail)
		return err
	})
	if err != nil && t != nil && t.key != "" {
		_ = t.disk.Delete(context.WithoutCancel(ctx), t.key)
	}
	if errors.Is(err, errStale) {
		return nil
	}
	return err
}

// errStale ends a settle whose file changed while Process ran.
var errStale = errors.New("ocr: the file changed while it was read")

// ownStatus reports whether f's status is one the plugin set: processing
// with StatusDetail, or failed with StatusFailedPrefix.
func ownStatus(f records.File) bool {
	switch f.Status {
	case records.StatusProcessing:
		return f.StatusDetail == StatusDetail
	case records.StatusFailed:
		return strings.HasPrefix(f.StatusDetail, StatusFailedPrefix)
	}
	return false
}

// reads reports whether f is one of the content types p reads.
func (p *Plugin) reads(f records.File) bool {
	return slices.Contains(p.types, mediaType(f.ContentType))
}

// mediaType is ct without parameters, trimmed and lower-cased.
func mediaType(ct string) string {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.TrimSpace(strings.ToLower(mt))
}

// clearKey drops MetaTextKey and MetaTextDisk from f, copying the map,
// which the caller may share.
func clearKey(f *records.File) {
	_, hasKey := f.Metadata[MetaTextKey]
	_, hasDisk := f.Metadata[MetaTextDisk]
	if !hasKey && !hasDisk {
		return
	}
	meta := maps.Clone(f.Metadata)
	delete(meta, MetaTextKey)
	delete(meta, MetaTextDisk)
	if len(meta) == 0 {
		meta = nil
	}
	f.Metadata = meta
}

// deleteText deletes f's text after the transaction in ctx commits.
func deleteText(ctx context.Context, s *records.Store, f records.File) {
	if disk, key, ok := TextLocation(f); ok {
		releaseAfterCommit(ctx, s, records.File{Disk: disk, Key: key})
	}
}
