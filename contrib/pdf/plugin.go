package pdf

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Limits bound what Inspect accepts. Zero fields do not limit.
type Limits struct {
	// MaxPages refuses longer documents.
	MaxPages int
}

// Inspect returns an upload.Inspector that reads every stored PDF and
// refuses it (CodeRejected, which deletes the object) when pdfinfo cannot
// read it (not a PDF, damaged, or locked with a user password) or it has
// more than limits.MaxPages pages. Other types pass untouched. Use it as
// Policy.Inspect: it reads the stored object back, so it covers
// presigned and Uppy uploads through Confirm as well as form uploads.
func (t Tool) Inspect(limits Limits) upload.Inspector {
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		if mediaType(r.ContentType) != ContentType {
			return nil
		}
		rc, err := disk.Read(ctx, r.Key)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		info, err := t.Probe(ctx, rc)
		if err != nil {
			return err
		}
		if limits.MaxPages > 0 && info.Pages > limits.MaxPages {
			return filesystem.Errorf(filesystem.CodeRejected, "the document has more than %d pages", limits.MaxPages)
		}
		return nil
	}
}

// mediaType strips parameters from a content type.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(strings.ToLower(mt))
}

// PluginName is the records plugin's name.
const PluginName = "pdf"

// Metadata keys the plugin sets on PDF files. The document's title and
// author have keys of their own, not records.MetaTitle: that one is the
// application's display heading, and a file's embedded title is only a
// claim by whoever made it.
const (
	MetaPages      = "pages"
	MetaPageWidth  = "page_width_pt"  // the first page, in points
	MetaPageHeight = "page_height_pt" // the first page, in points
	MetaDocTitle   = "doc_title"      // Info.Title, when the file has one
	MetaDocAuthor  = "doc_author"     // Info.Author, when the file has one
	MetaDocCreated = "doc_created"    // Info.Created as RFC 3339, when the file has one
	MetaEncrypted  = "encrypted"      // "true" when the file is encrypted with an owner password
	MetaPreviewKey = "preview_key"    // with WithPreview: the preview's key, on MetaPreviewDisk or else the file's disk
	MetaTextKey    = "text_key"       // with WithText: the text's key, on MetaTextDisk or else the file's disk, when there is text
	// MetaPreviewDisk and MetaTextDisk name the preview's and the text's
	// disk when WithLocation put them on another disk than the file's;
	// absent when they are on the file's own.
	MetaPreviewDisk = "preview_disk"
	MetaTextDisk    = "text_disk"
	MetaTextCut     = "text_truncated" // "true" when the stored text stops short (Extract.Truncated)
	MetaTextOCR     = "text_ocr"       // "true" when the stored text came from a Recognizer
)

// metaTrue is the value of the plugin's boolean keys; a false one is
// absent.
const metaTrue = "true"

// metaKeys are every key the plugin owns. They are cleared only on a
// file the plugin describes (see describe), never on another type: an
// application may keep "pages" or "encrypted" of its own on other files.
var metaKeys = []string{MetaPages, MetaPageWidth, MetaPageHeight, MetaDocTitle, MetaDocAuthor, MetaDocCreated, MetaEncrypted, MetaPreviewKey, MetaPreviewDisk, MetaTextKey, MetaTextDisk, MetaTextCut, MetaTextOCR}

// Names the plugin passes to a Location for each derived object.
const (
	DerivedPreview = "preview"
	DerivedText    = "text"
)

// Location chooses where the plugin stores a derived object of f (name is
// DerivedPreview or DerivedText): the name of a disk of the Store's
// Manager ("" for f's own disk) and a key prefix. The plugin appends a
// random part and the suffix (PreviewSuffix, TextSuffix) to the prefix,
// so every run writes a key no other run writes: a replacement's preview
// never overwrites the current one before the replacement is processed,
// and the deferred deletion of an old one never removes a new one. An
// error fails closed: nothing more is written, what the run already
// wrote is deleted, and the file is marked StatusFailed.
type Location func(f records.File, name string) (disk, prefix string, err error)

// defaultLocation stores a derived object beside its file, on the file's
// disk: <file key><PreviewInfix or TextInfix><random><suffix>.
func defaultLocation(f records.File, name string) (string, string, error) {
	if name == DerivedText {
		return "", f.Key + TextInfix, nil
	}
	return "", f.Key + PreviewInfix, nil
}

// Previews are stored next to their PDF as
// <pdf key><PreviewInfix><random><PreviewSuffix>. The random part keeps a
// replacement's preview from overwriting the current one before the
// replacement commits.
const (
	PreviewInfix  = ".preview-"
	PreviewSuffix = ".jpg"
	// derivativeTokenLen is how many characters of rand.Text name a
	// preview or a text.
	derivativeTokenLen = 10
)

// Texts are stored the same way, as <pdf key><TextInfix><random><TextSuffix>.
const (
	TextInfix  = ".text-"
	TextSuffix = ".txt"
)

// Converter turns another document type into a PDF, so the plugin
// describes it like one: page count, preview and text. contrib/office's
// Tool converts word-processing, spreadsheet and presentation files with
// LibreOffice.
type Converter interface {
	// Converts reports whether the converter takes contentType (no
	// parameters, lower case).
	Converts(contentType string) bool
	// ConvertToPDF returns the PDF made from r, a file of contentType;
	// the caller closes it. A file it cannot convert is CodeRejected.
	ConvertToPDF(ctx context.Context, r io.Reader, contentType string) (io.ReadCloser, error)
}

// Plugin is a records plugin that describes every PDF file when it is
// recorded and when its content is replaced: page count, first page size
// and document information, read with pdfinfo inside the upload's
// transaction (a bounded, fast step). With WithPreview, WithText, WithOCR
// or WithConverter it also makes a preview, the text, or the PDF of an
// office file, and that slow work never runs inside the transaction: the
// file is marked records.StatusProcessing in it, and Process runs once it
// commits (records.Store.AfterCommit), outside any transaction and under
// its own timeout, then records what it made and marks the file
// StatusReady, or StatusFailed with the reason, never failing the upload.
// Construct it with NewPlugin and install it with records.WithPlugins.
//
// A file pdfinfo cannot read is recorded without metadata, or refused
// with WithStrict. A missing program is an error either way: every PDF
// upload fails with CodeUnavailable until Poppler is installed, rather
// than silently recording PDFs without metadata.
type Plugin struct {
	tool           Tool
	strict         bool
	preview        bool
	text           bool
	ocr            Recognizer
	converter      Converter
	processTimeout time.Duration
	deferWork      bool
	// location places previews and texts; defaultLocation unless
	// WithLocation.
	location Location
}

// DefaultProcessTimeout bounds one Process run: conversion, preview, text
// and OCR of one file together.
const DefaultProcessTimeout = 10 * time.Minute

// settleTimeout bounds the transaction that records how a Process run
// ended. It runs on a context detached from the run's own deadline, so a
// run cut short by WithProcessTimeout still records StatusFailed instead
// of leaving the file in StatusProcessing until Resume; the bound keeps a
// stuck database from holding Process forever in its place.
const settleTimeout = 30 * time.Second

// StatusDetail is the records status detail of a file the plugin is
// processing; a processing status with another detail belongs to another
// plugin and is left alone. A failure's detail is StatusFailedPrefix and
// the error code ("pdf: unavailable").
const (
	StatusDetail       = "pdf: processing"
	StatusFailedPrefix = "pdf: "
)

// PluginOption configures a Plugin.
type PluginOption func(*Plugin)

// WithStrict refuses a PDF file pdfinfo cannot read (CodeRejected), so a
// damaged or disguised file is never recorded. It covers the step inside
// the upload only: a file that fails later, in Process (an office file
// LibreOffice cannot convert), is recorded and marked StatusFailed.
func WithStrict() PluginOption { return func(p *Plugin) { p.strict = true } }

// WithPreview also stores a JPEG of the first page (Tool.MaxSide pixels
// on its longest side) next to each PDF, after the upload commits,
// records its key in MetaPreviewKey, and deletes it with the file (after
// the purge commits) or when the file's content is replaced.
func WithPreview() PluginOption { return func(p *Plugin) { p.preview = true } }

// WithText also stores each file's text (Tool.Text, at most Tool.MaxText
// bytes) next to it as a text/plain object for a search indexer to read,
// after the upload commits, records its key in MetaTextKey (none when
// the text holds no letter or digit), and deletes it with the file or
// when its content is replaced, like a preview.
func WithText() PluginOption { return func(p *Plugin) { p.text = true } }

// WithOCR is WithText reading a PDF without a text layer through rec (see
// Tool.Text). Each scanned page is a render and a recognition, and a
// cloud recognizer is billed per page: Tool.MaxOCRPages bounds both.
func WithOCR(rec Recognizer) PluginOption {
	return func(p *Plugin) { p.text, p.ocr = true, rec }
}

// WithConverter also describes the files c converts, through the PDF it
// makes of each, after the upload commits: page count, document
// information, and with WithPreview and WithText a preview and the text.
// The PDF itself is not kept.
func WithConverter(c Converter) PluginOption { return func(p *Plugin) { p.converter = c } }

// WithProcessTimeout bounds one Process run; the default is
// DefaultProcessTimeout. A run cut short marks the file StatusFailed.
func WithProcessTimeout(d time.Duration) PluginOption {
	return func(p *Plugin) { p.processTimeout = d }
}

// WithDeferredProcessing leaves the slow work to Resume: an upload only
// marks the file processing and returns. Without it (the default, which
// needs no setup) Process runs inline once the upload commits, outside
// the transaction but still inside the Upload call, so the caller waits
// for it: up to WithProcessTimeout per upload, even after its client has
// gone away. With it, schedule Resume (jobs.Func every minute, say).
func WithDeferredProcessing() PluginOption { return func(p *Plugin) { p.deferWork = true } }

// WithLocation stores previews and texts where l says instead of beside
// their file on its disk: a public CDN disk for previews while the
// documents stay private, a search bucket for the texts, or CDN-friendly
// paths. A derived object's disk is recorded in MetaPreviewDisk or
// MetaTextDisk when it is not the file's, so PreviewLocation,
// TextLocation, the purge and a replacement find it. A nil l keeps the
// default.
func WithLocation(l Location) PluginOption {
	return func(p *Plugin) {
		if l != nil {
			p.location = l
		}
	}
}

// NewPlugin returns a Plugin running t.
func NewPlugin(t Tool, opts ...PluginOption) *Plugin {
	p := &Plugin{tool: t, processTimeout: DefaultProcessTimeout, location: defaultLocation}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// BeforeCreate implements records.FileHooks: the preview and text keys a
// caller put in f's metadata are dropped (clearRefs), then the document
// information is read, inside the transaction.
func (p *Plugin) BeforeCreate(ctx context.Context, s *records.Store, f *records.File) error {
	clearRefs(f)
	return p.describe(ctx, s, f)
}

// refKeys name the plugin's own objects: the purge deletes what they name,
// OwnedObjects keeps it from the orphan sweep and PreviewLocation and
// TextLocation hand it out. Only the plugin may set them, so they are
// dropped from a new file of any type, while every other key of a type
// the plugin does not describe stays the application's (see describe).
var refKeys = []string{MetaPreviewKey, MetaPreviewDisk, MetaTextKey, MetaTextDisk}

// clearRefs removes refKeys from f's metadata, copying the map, which the
// caller may share.
func clearRefs(f *records.File) {
	if !slices.ContainsFunc(refKeys, func(k string) bool { _, ok := f.Metadata[k]; return ok }) {
		return
	}
	meta := maps.Clone(f.Metadata)
	for _, k := range refKeys {
		delete(meta, k)
	}
	if len(meta) == 0 {
		meta = nil
	}
	f.Metadata = meta
}

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: the preview and the text go
// with the file, after the purge commits.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	deleteDerivatives(ctx, s, f)
	return nil
}

// BeforeRelease implements records.ReleaseHooks: a file recorded or
// replaced while quarantined was not read then (see describe), so its
// information is read when its scan passes; file.released then starts
// Process (OnEvent).
func (p *Plugin) BeforeRelease(ctx context.Context, s *records.Store, f *records.File) error {
	return p.describe(ctx, s, f)
}

// BeforeReplace implements records.ReplaceHooks: the new content's
// information is read, and the old preview and text are deleted once the
// replacement commits.
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, current records.File, next *records.File) error {
	if p.owns(current) && !p.owns(*next) {
		dropStale(next)
	}
	if err := p.describe(ctx, s, next); err != nil {
		return err
	}
	deleteDerivatives(ctx, s, current)
	return nil
}

// OnEvent implements records.EventHooks: a file recorded or replaced with
// slow work to do is marked processing in the change's own transaction,
// so no reader sees it ready before its preview and text exist, and
// Process runs once the change commits. A rolled-back change runs
// nothing. A quarantined file is left alone (its bytes are unscanned)
// until its scan passes, whose file.released starts the same work.
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	switch e.Kind {
	case records.EventFileCreated, records.EventFileReplaced, records.EventFileReleased:
	default:
		return nil
	}
	if e.File == nil || e.File.Quarantined() || !p.deferred(*e.File) {
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

// deferred reports whether f has work for Process: a PDF the plugin
// described, when a preview or the text is wanted, or a file the
// converter takes.
func (p *Plugin) deferred(f records.File) bool {
	if p.converts(f) {
		return true
	}
	_, described := Of(f)
	return described && mediaType(f.ContentType) == ContentType && (p.preview || p.text)
}

// owns reports whether the plugin describes f: a PDF, or a type the
// converter takes. Only on such a file does it set or clear its keys.
func (p *Plugin) owns(f records.File) bool {
	return mediaType(f.ContentType) == ContentType || p.converts(f)
}

// dropStale removes every key the plugin wrote from next, whose content
// (no longer a type the plugin describes) replaces a file it described,
// so they do not outlive it. It copies the metadata map, which the
// caller may share.
func dropStale(next *records.File) {
	meta := maps.Clone(next.Metadata)
	for _, k := range metaKeys {
		delete(meta, k)
	}
	if len(meta) == 0 {
		meta = nil
	}
	next.Metadata = meta
}

// converts reports whether f is a type the converter takes.
func (p *Plugin) converts(f records.File) bool {
	mt := mediaType(f.ContentType)
	return mt != ContentType && p.converter != nil && p.converter.Converts(mt)
}

// Process does the slow work for the file id of scope: converts it, makes
// its preview and text, stores them beside it and records them on its
// row, then marks it StatusReady; a failure marks it StatusFailed with
// StatusFailedPrefix and the error code, and is returned. The plugin runs
// it after each upload commits; call it from a job to retry a failed file
// or to backfill files recorded before the plugin was installed. It runs
// under WithProcessTimeout and outside any transaction, so it never holds
// one open while programs run. Content replaced while it ran is left to
// the replacement's own run: what it made is deleted. A quarantined file
// is refused with records.ErrQuarantined, unread.
func (p *Plugin) Process(ctx context.Context, s *records.Store, scope, id string) error {
	run, cancel := context.WithTimeout(ctx, p.processTimeout)
	defer cancel()
	end, cancelEnd := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancelEnd()
	f, err := s.File(run, scope, id)
	if err != nil {
		return err
	}
	// Unscanned bytes are never converted or read; the release runs
	// Process again.
	if f.Quarantined() {
		return records.ErrQuarantined
	}
	if f.DeletedAt != nil || !p.deferred(f) {
		return p.settle(end, s, f, nil, nil)
	}
	d, err := p.derive(run, s, f)
	if err != nil {
		return errors.Join(err, p.settle(end, s, f, nil, err))
	}
	return p.settle(end, s, f, &d, nil)
}

// derived is what Process made for a file.
type derived struct {
	info     Info
	preview  stored // the preview, key "" for none
	text     stored // the text, key "" for none
	textCut  bool
	textOCR  bool
	converts bool
}

// stored is one derived object: its disk, the disk's name and its key.
type stored struct {
	disk     *filesystem.Disk
	diskName string
	key      string
}

// derive converts f when it is not a PDF, runs the programs and stores
// the preview and the text, outside any transaction. On an error it
// deletes what it stored, so the caller has nothing to discard.
func (p *Plugin) derive(ctx context.Context, s *records.Store, f records.File) (derived, error) {
	d := derived{converts: p.converts(f)}
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return d, err
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return d, err
	}
	defer func() { _ = rc.Close() }()
	var src io.Reader = rc
	if d.converts {
		converted, err := p.converter.ConvertToPDF(ctx, rc, mediaType(f.ContentType))
		if err != nil {
			return d, err
		}
		defer func() { _ = converted.Close() }()
		src = converted
	}
	var preview []byte
	var text Extract
	err = p.tool.withWorkFile(src, func(path string) error {
		var err error
		if d.info, err = p.tool.infoPath(ctx, path); err != nil {
			return err
		}
		if p.preview {
			if preview, err = p.tool.previewPath(ctx, path); err != nil {
				return err
			}
		}
		if p.text {
			text, err = p.tool.textPath(ctx, path, d.info.Pages, p.ocr)
		}
		return err
	})
	if err != nil {
		return d, err
	}
	if preview != nil {
		if d.preview, err = p.storeDerivative(ctx, s, f, DerivedPreview, PreviewSuffix, PreviewContentType, preview); err != nil {
			return d, err
		}
	}
	if hasText(text.Text) {
		if d.text, err = p.storeDerivative(ctx, s, f, DerivedText, TextSuffix, TextContentType, []byte(text.Text)); err != nil {
			discard(ctx, d)
			return derived{}, err
		}
		d.textCut, d.textOCR = text.Truncated, text.OCR
	}
	return d, nil
}

// settle records the end of a Process run on the file's row, in one
// transaction with the row locked: what d made (nil for nothing) and the
// status, StatusReady, or StatusFailed for failure. When the file was
// replaced, trashed or purged meanwhile, what d made is deleted and the
// row is left alone. A status set by another plugin is never touched.
func (p *Plugin) settle(ctx context.Context, s *records.Store, f records.File, d *derived, failure error) error {
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
		if d != nil {
			if err := p.record(ctx, s, cur, *d); err != nil {
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
	if err != nil && d != nil {
		discard(ctx, *d)
	}
	if errors.Is(err, errStale) {
		return nil
	}
	return err
}

// ownStatus reports whether f's status is one the plugin set: processing
// with StatusDetail, or failed with StatusFailedPrefix. Only those does
// settle change, so a retry clears the plugin's own failure.
func ownStatus(f records.File) bool {
	switch f.Status {
	case records.StatusProcessing:
		return f.StatusDetail == StatusDetail
	case records.StatusFailed:
		return strings.HasPrefix(f.StatusDetail, StatusFailedPrefix)
	}
	return false
}

// errStale ends a settle whose file changed while Process ran.
var errStale = errors.New("pdf: the file changed while it was processed")

// record writes what d made into cur's metadata, inside settle's
// transaction: the information of a converted file, and the keys of the
// new preview and text. Older ones (an earlier run's) are deleted after
// the commit.
func (p *Plugin) record(ctx context.Context, s *records.Store, cur records.File, d derived) error {
	meta := maps.Clone(cur.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	deleteDerivatives(ctx, s, cur)
	for _, k := range []string{MetaPreviewKey, MetaPreviewDisk, MetaTextKey, MetaTextDisk, MetaTextCut, MetaTextOCR} {
		delete(meta, k)
	}
	if d.converts {
		setInfo(meta, d.info)
	}
	setStored(meta, cur, d.preview, MetaPreviewKey, MetaPreviewDisk)
	if d.text.key != "" {
		setStored(meta, cur, d.text, MetaTextKey, MetaTextDisk)
		setTrue(meta, MetaTextCut, d.textCut)
		setTrue(meta, MetaTextOCR, d.textOCR)
	}
	text, _ := json.Marshal(meta) // a map of strings cannot fail to marshal
	upd := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileMetadata, string(text))).
		Where(sqlb.Eq(records.FileScope, cur.Scope), sqlb.Eq(records.FileID, cur.ID))
	_, err := s.Exec(ctx, upd)
	return err
}

// setStored records o's key in meta under keyMeta, and its disk under
// diskMeta when it is not cur's own; an o with no key records nothing.
func setStored(meta map[string]string, cur records.File, o stored, keyMeta, diskMeta string) {
	if o.key == "" {
		return
	}
	meta[keyMeta] = o.key
	if o.diskName != cur.Disk {
		meta[diskMeta] = o.diskName
	}
}

// discard deletes the objects d stored; it runs when they were not
// recorded, so nothing names them.
func discard(ctx context.Context, d derived) {
	for _, o := range []stored{d.preview, d.text} {
		if o.key != "" {
			_ = o.disk.Delete(context.WithoutCancel(ctx), o.key)
		}
	}
}

// deleteDerivatives deletes f's preview and text after the transaction in
// ctx commits, each on the disk it was stored on.
func deleteDerivatives(ctx context.Context, s *records.Store, f records.File) {
	for _, loc := range []func(records.File) (string, string, bool){PreviewLocation, TextLocation} {
		if disk, key, ok := loc(f); ok {
			releaseAfterCommit(ctx, s, records.File{Disk: disk, Key: key})
		}
	}
}

// describe sets a PDF's document information from its object with
// pdfinfo, replacing whatever the plugin set before; the preview and text
// keys go too, since they describe the content f had. A converted type
// has its keys cleared here and gets its information in Process. Any
// other type is left untouched, keys and all: "pages" or "encrypted" on
// an application's own file are the application's (except the preview
// and text keys, which BeforeCreate drops from every new file; see
// clearRefs). It copies the
// metadata map before changing it, because the caller's map may be
// shared.
func (p *Plugin) describe(ctx context.Context, s *records.Store, f *records.File) error {
	if !p.owns(*f) {
		return nil
	}
	meta := maps.Clone(f.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	for _, k := range metaKeys {
		delete(meta, k)
	}
	defer func() {
		if len(meta) == 0 {
			meta = nil
		}
		f.Metadata = meta
	}()
	// A quarantined file's bytes are unscanned and are not read: it is
	// described on release (BeforeRelease).
	if mediaType(f.ContentType) != ContentType || f.Quarantined() {
		return nil
	}
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	var info Info
	err = p.tool.withWorkFile(rc, func(path string) error {
		var err error
		info, err = p.tool.infoPath(ctx, path)
		return err
	})
	if err != nil {
		if !p.strict && filesystem.CodeOf(err) == filesystem.CodeRejected {
			return nil
		}
		return err
	}
	setInfo(meta, info)
	return nil
}

// setInfo records info in meta.
func setInfo(meta map[string]string, info Info) {
	meta[MetaPages] = strconv.Itoa(info.Pages)
	meta[MetaPageWidth] = strconv.FormatFloat(info.Width, 'f', -1, 64)
	meta[MetaPageHeight] = strconv.FormatFloat(info.Height, 'f', -1, 64)
	for k, v := range map[string]string{MetaDocTitle: info.Title, MetaDocAuthor: info.Author} {
		if v != "" {
			meta[k] = v
		}
	}
	if !info.Created.IsZero() {
		meta[MetaDocCreated] = info.Created.Format(time.RFC3339)
	}
	setTrue(meta, MetaEncrypted, info.Encrypted)
}

// setTrue sets key to metaTrue when on; a false flag is absent.
func setTrue(meta map[string]string, key string, on bool) {
	if on {
		meta[key] = metaTrue
	}
}

// storeDerivative writes body where the Location puts f's derived object
// name: the prefix it returns plus <random><suffix>. The random part
// keeps a replacement's derivative from overwriting the current one
// before the replacement is processed. A Location error fails closed,
// before anything is written.
func (p *Plugin) storeDerivative(ctx context.Context, s *records.Store, f records.File, name, suffix, contentType string, body []byte) (stored, error) {
	diskName, prefix, err := p.location(f, name)
	if err != nil {
		return stored{}, err
	}
	if diskName == "" {
		diskName = f.Disk
	}
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return stored{}, err
	}
	key := prefix + strings.ToLower(rand.Text()[:derivativeTokenLen]) + suffix
	if _, err := disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{ContentType: contentType}); err != nil {
		return stored{}, err
	}
	return stored{disk: disk, diskName: diskName, key: key}, nil
}

// Meta is the metadata the plugin recorded for a PDF file.
type Meta struct {
	Pages                 int
	PageWidth, PageHeight float64 // the first page, in points
	Title, Author         string
	Created               time.Time
	Encrypted             bool
}

// Of returns the metadata recorded for f; ok is false when there is none
// (not a PDF, unreadable, or recorded before the plugin was installed).
func Of(f records.File) (m Meta, ok bool) {
	pages, err := strconv.Atoi(f.Metadata[MetaPages])
	if err != nil {
		return Meta{}, false
	}
	w, _ := strconv.ParseFloat(f.Metadata[MetaPageWidth], 64)
	h, _ := strconv.ParseFloat(f.Metadata[MetaPageHeight], 64)
	created, _ := time.Parse(time.RFC3339, f.Metadata[MetaDocCreated])
	return Meta{
		Pages: pages, PageWidth: w, PageHeight: h,
		Title: f.Metadata[MetaDocTitle], Author: f.Metadata[MetaDocAuthor],
		Created: created, Encrypted: f.Metadata[MetaEncrypted] == metaTrue,
	}, true
}

// PreviewKey returns the key of f's preview (WithPreview); ok is false
// when there is none. The preview is on f's disk unless WithLocation
// chose another: PreviewLocation returns both.
func PreviewKey(f records.File) (string, bool) {
	key := f.Metadata[MetaPreviewKey]
	return key, key != ""
}

// TextKey returns the key of f's text (WithText); ok is false when there
// is none. The text is on f's disk unless WithLocation chose another:
// TextLocation returns both.
func TextKey(f records.File) (string, bool) {
	key := f.Metadata[MetaTextKey]
	return key, key != ""
}

// PreviewLocation returns the disk and key of f's preview: the disk is
// MetaPreviewDisk when WithLocation put it elsewhere, else f's own. ok is
// false when there is no preview.
func PreviewLocation(f records.File) (disk, key string, ok bool) {
	return location(f, MetaPreviewKey, MetaPreviewDisk)
}

// TextLocation returns the disk and key of f's text, as PreviewLocation.
func TextLocation(f records.File) (disk, key string, ok bool) {
	return location(f, MetaTextKey, MetaTextDisk)
}

// location reads a derived object's key and disk from f's metadata.
func location(f records.File, keyMeta, diskMeta string) (disk, key string, ok bool) {
	key = f.Metadata[keyMeta]
	if key == "" {
		return "", "", false
	}
	disk = f.Metadata[diskMeta]
	if disk == "" {
		disk = f.Disk
	}
	return disk, key, true
}
