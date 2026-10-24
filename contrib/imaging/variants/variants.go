// Package variants is a records plugin that serves named image variants of
// stored files ("thumb", "avatar-64"), generated on first request and then
// cached: the derivative layer of an image upload pipeline.
//
// Only names the application declares can be generated, so a client can
// never ask for an arbitrary size (the denial-of-service and injection risk
// of open transform URLs). Each variant is identified by its name and the
// digest of its Transform; changing a transform produces new variants on
// the next request, and Prune removes the outdated ones. Variant rows and
// objects are deleted when their file is trashed (a trashed file's
// thumbnails must stop answering at their URLs) or purged; a restored
// file's variants are generated again.
package variants

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "variants"

// TableNameVariants is the table the plugin owns.
const TableNameVariants = "fs_variants"

// Column names of fs_variants.
const (
	ColID          = "id"
	ColScope       = "scope"
	ColFileID      = "file_id"
	ColName        = "name"
	ColDigest      = "digest"
	ColDisk        = "disk"
	ColKey         = "storage_key"
	ColContentType = "content_type"
	ColSize        = "size"
	ColWidth       = "width"
	ColHeight      = "height"
	ColCreatedAt   = "created_at"
)

// Table and typed columns.
var (
	Table       = sqlb.Table{Name: TableNameVariants, Alias: "v"}
	ID          = sqlb.Col[string](Table, ColID)
	Scope       = sqlb.Col[string](Table, ColScope)
	FileID      = sqlb.Col[string](Table, ColFileID)
	Name        = sqlb.Col[string](Table, ColName)
	Digest      = sqlb.Col[string](Table, ColDigest)
	Disk        = sqlb.Col[string](Table, ColDisk)
	Key         = sqlb.Col[string](Table, ColKey)
	ContentType = sqlb.Col[string](Table, ColContentType)
	Size        = sqlb.Col[int64](Table, ColSize)
	Width       = sqlb.Col[int64](Table, ColWidth)
	Height      = sqlb.Col[int64](Table, ColHeight)
	CreatedAt   = sqlb.Col[time.Time](Table, ColCreatedAt)
)

// IDPrefix starts every variant ID.
const IDPrefix = "var_"

// KeyDir is the directory variant objects are stored under by default,
// next to the originals on the same disk (see WithLocation).
const KeyDir = "variants"

// Location chooses where a generated variant of f is stored (name is the
// variant's declared name): the name of a disk of the Store's Manager
// ("" for f's own disk) and a key prefix. The plugin appends
// <name>-<digest>-<id><ext> to the prefix, where id is the random part of
// the variant's own ID, so every generation still writes a key no other
// generation writes, which the deferred deletions depend on (see
// variantKey). The disk is recorded on the variant's row, so serving,
// Prune, the purge and SweepOrphans find it there. An error fails
// closed: nothing is written and the request fails with it (an eager
// generation marks the file StatusFailed).
type Location func(f records.File, name string) (disk, prefix string, err error)

// defaultLocation stores variants on their file's disk under
// variants/<fileID>/.
func defaultLocation(f records.File, _ string) (string, string, error) {
	return "", KeyDir + "/" + f.ID + "/", nil
}

// Variant is one generated derivative of a file.
type Variant struct {
	ID          string
	Scope       string
	FileID      string
	Name        string
	Digest      string
	Disk        string
	Key         string
	ContentType string
	Size        int64
	Width       int64
	Height      int64
	CreatedAt   time.Time
}

func bindings(v *Variant) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(ID, &v.ID), sqlb.Bind(Scope, &v.Scope), sqlb.Bind(FileID, &v.FileID), sqlb.Bind(Name, &v.Name),
		sqlb.Bind(Digest, &v.Digest), sqlb.Bind(Disk, &v.Disk), sqlb.Bind(Key, &v.Key), sqlb.Bind(ContentType, &v.ContentType),
		sqlb.Bind(Size, &v.Size), sqlb.Bind(Width, &v.Width), sqlb.Bind(Height, &v.Height), sqlb.Bind(CreatedAt, &v.CreatedAt),
	}
}

// namePattern constrains variant names; they appear in storage keys.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Plugin generates and tracks variants. Construct it with New and install
// it with records.WithPlugins; its methods take the Store explicitly, so
// it holds no reference to one.
type Plugin struct {
	transforms map[string]imaging.Transform
	opts       imaging.Options
	// processor makes the variants; imaging.Default() unless
	// WithProcessor.
	processor imaging.Processor
	// focal locates GravityFocal crops (WithFocal); nil means centred.
	focal   FocalSource
	eager   []string
	onEager func(ctx context.Context, f records.File, name string, err error)
	// headers are the stored HTTP headers per variant name (WithHeaders).
	headers map[string]filesystem.HTTPHeaders
	// densities lists each base's declared factors, 1 first, ascending
	// (WithDensities); densityOf says which declared names are density
	// versions, and of what.
	densities map[string][]float64
	densityOf map[string]density
	// location places variant objects; defaultLocation unless
	// WithLocation.
	location Location
}

// Option configures a Plugin.
type Option func(*Plugin) error

// WithEager generates the named variants as soon as an image is recorded,
// replaced, restored from the trash or made public or private, instead of on its first request, so
// the first visitor of a new photo is not the one who waits. They are
// made after the transaction commits, so a slow resize never holds the
// database; a failure is passed to onError (which may be nil) and the
// variant is made on its first request instead.
func WithEager(onError func(ctx context.Context, f records.File, name string, err error), names ...string) Option {
	return func(p *Plugin) error {
		for _, n := range names {
			if _, ok := p.transforms[n]; !ok {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: eager variant %q is not declared", n)
			}
		}
		p.eager, p.onEager = names, onError
		return nil
	}
}

// WithHeaders stores h as the HTTP headers of the named variants (every
// declared variant when no name is given), served with them by the disk,
// fshttp or a CDN in front of a public disk. Each generation writes a key
// never written before (variantKey), so a cached copy never shows another
// image; but a copy a browser or CDN already holds is served until it
// expires even after the file is made private, trashed or purged, since
// only the object is deleted. So CacheControl "public, max-age=31536000,
// immutable" suits variants of files that stay public; for files that can
// be hidden, use a short max-age or purge the CDN on those events. Headers
// are never inherited from the original, so an
// original stored as an attachment does not make its thumbnails
// downloads. A value with CR or LF is CodeInvalidArgument. The disk must
// have the Headers capability: on one without it every generation of
// these variants fails with CodeUnsupported, as Disk.Write refuses headers
// it cannot store rather than dropping them.
func WithHeaders(h filesystem.HTTPHeaders, names ...string) Option {
	return func(p *Plugin) error {
		for _, v := range []string{h.CacheControl, h.ContentDisposition, h.ContentEncoding} {
			if strings.ContainsAny(v, "\r\n") {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: header values may not contain CR or LF")
			}
		}
		if len(names) == 0 {
			names = slices.Sorted(maps.Keys(p.transforms))
		}
		for _, n := range names {
			if _, ok := p.transforms[n]; !ok {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: headers for undeclared variant %q", n)
			}
			p.headers[n] = h
		}
		return nil
	}
}

// New returns a Plugin for the named transforms. Names must match
// [a-z0-9][a-z0-9_-]* and transforms must be valid.
func New(transforms map[string]imaging.Transform, opts imaging.Options, options ...Option) (*Plugin, error) {
	if len(transforms) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: no transforms declared")
	}
	for name := range transforms {
		if !namePattern.MatchString(name) {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: invalid name %q", name)
		}
	}
	// A copy, since WithDensities declares more: the caller's map is
	// never changed.
	p := &Plugin{
		transforms: maps.Clone(transforms), opts: opts, headers: map[string]filesystem.HTTPHeaders{}, processor: imaging.Default(),
		densities: map[string][]float64{}, densityOf: map[string]density{}, location: defaultLocation,
	}
	for _, o := range options {
		if err := o(p); err != nil {
			return nil, err
		}
	}
	p.inheritHeaders()
	// Transforms are checked once the processor is known, so a format its
	// own encoders add (imaging.DescribedEncoder) is accepted; density
	// versions WithDensities declared are checked with them.
	for _, name := range slices.Sorted(maps.Keys(p.transforms)) {
		if err := p.transforms[name].ValidateFor(p.processor); err != nil {
			return nil, err
		}
	}
	// A processor that can say which formats it writes is asked now, so a
	// WebP variant without the WebP codec fails at start-up rather than
	// on every request.
	if enc, ok := p.processor.(interface{ Encodes(imaging.Format) bool }); ok {
		for name, t := range p.transforms {
			if !enc.Encodes(t.Format) {
				return nil, filesystem.Errorf(filesystem.CodeUnsupported, "variants: %q needs a %s encoder the processor lacks (add its codec module, see imaging.WithEncoder)", name, t.Format)
			}
		}
	}
	return p, nil
}

// WithProcessor makes variants with proc instead of imaging.Default(): a
// Pipeline with codec modules (WebP or AVIF output, HEIC input), or
// another backend. A nil proc is CodeInvalidArgument.
func WithProcessor(proc imaging.Processor) Option {
	return func(p *Plugin) error {
		if proc == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: nil processor")
		}
		p.processor = proc
		return nil
	}
}

// WithLocation stores variants where l says instead of under KeyDir on
// their file's disk: thumbnails on a public CDN disk while the originals
// stay private, a cheaper bucket or storage class, or CDN-friendly paths.
// A variant on another disk than its file's stays there when the file
// moves disks (MoveFileDisk moves only those on the file's own). A
// variant inherits its file's visibility only where its own disk can set
// one. A nil l is CodeInvalidArgument.
func WithLocation(l Location) Option {
	return func(p *Plugin) error {
		if l == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: nil location")
		}
		p.location = l
		return nil
	}
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Index names of fs_variants. They are unique in the database (Postgres
// and SQLite share one index namespace per schema), hence the prefix.
const (
	indexFileNameDigest = "fs_variants_file_name_digest"
	// indexByScope is the index HeldBytes reads by.
	indexByScope = "fs_variants_scope"
)

// Migrations implements records.Migrator. Both are built with records.DDL
// from the typed columns the queries use, so the table cannot be created
// under one name or type and read under another, and every statement can
// run again after a crash before the ledger write (MySQL's DDL commits at
// once; DDL guards it there).
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	table, err1 := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(Scope, t.Key), records.Column(FileID, t.Key),
			records.Column(Name, t.Short), records.Column(Digest, t.Short), records.Column(Disk, t.Short),
			records.Column(Key, t.Long), records.Column(ContentType, t.Text), records.Column(Size, t.Int),
			records.Column(Width, t.Int), records.Column(Height, t.Int), records.Column(CreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		Indexes: []records.Index{
			{Name: indexFileNameDigest, Unique: true, Columns: []records.IndexColumn{records.Key(FileID), records.Key(Name), records.Key(Digest)}},
		},
	}).Statements()
	byScope, err2 := records.NewDDL(engine).CreateIndex(Table, records.Index{Name: indexByScope, Columns: []records.IndexColumn{records.Key(Scope)}}).Statements()
	// A build error means the definition above is wrong, never the
	// database; the golden test of the rendered SQL catches it first.
	return []records.Migration{
		{Owner: PluginName, Name: "0001_variants", Statements: table},
		// HeldBytes totals a scope's variants.
		{Owner: PluginName, Name: "0002_variants_scope", Statements: byScope},
	}, errors.Join(err1, err2)
}

// Variant rows name their disk, so a disk rename must reach them.
var _ records.DiskRenamer = (*Plugin)(nil)

// RenameDisk implements records.DiskRenamer: variant rows on the disk
// named from point at to instead, inside Store.RenameDisk's transaction
// (s.Exec joins it), so they move with the files or not at all. Only rows
// change; the objects must already be on to under the same keys.
func (p *Plugin) RenameDisk(ctx context.Context, s *records.Store, from, to string) error {
	_, err := s.Exec(ctx, sqlb.NewUpdate(s.Dialect()).Table(Table).Set(sqlb.Set(Disk, to)).Where(sqlb.Eq(Disk, from)))
	return err
}

// The plugin reports what it holds to usage plugins.
var _ records.HeldCounter = (*Plugin)(nil)

// HeldBytes implements records.HeldCounter: the total size of the scope's
// variants, so a usage recount (quota.Recount) includes them.
func (p *Plugin) HeldBytes(ctx context.Context, s *records.Store, scope string) (int64, error) {
	var n int64
	sum := sqlb.Coalesce(sqlb.Sum(Size), sqlb.Param[int64](0))
	if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(sum, &n)).From(Table).Where(sqlb.Eq(Scope, scope))); err != nil {
		return 0, err
	}
	return n, nil
}

// BeforeCreate implements records.FileHooks; variants need nothing at
// creation (they are generated lazily).
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: it deletes the file's variant
// rows in the purge transaction and their objects after it commits.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	return p.dropAll(ctx, s, f.Scope, f.ID)
}

// OnEvent implements records.EventHooks. A quarantined file gets no eager
// variants until file.released. A replaced file's variants show
// its old content, and a file made private must not keep public
// thumbnails (or keep them on the public disk it left), so in both cases
// they are dropped and generated again on the next request, from the
// file as it is now. A trashed file's variants are dropped too: their
// objects may be public, and a file in the trash must not keep answering
// through its thumbnails. Restoring it makes them again on request (or at
// once, with WithEager).
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	switch e.Kind {
	case records.EventFileTrashed:
		return p.dropAll(ctx, s, e.Scope, e.FileID)
	case records.EventFileReplaced, records.EventFileVisibilityChanged, p.focalEvent():
		if err := p.dropAll(ctx, s, e.Scope, e.FileID); err != nil {
			return err
		}
	case records.EventFileCreated, records.EventFileRestored, records.EventFileReleased:
	default:
		return nil
	}
	// A quarantined file's bytes are unscanned: nothing is generated from
	// them until its scan passes, whose file.released starts the eager
	// variants instead.
	if len(p.eager) == 0 || e.File == nil || !isImage(*e.File) || e.File.Quarantined() {
		return nil
	}
	// Mark the file processing in the change's own transaction, so no
	// reader sees it ready before its eager variants exist.
	if _, err := s.SetStatus(ctx, e.Scope, e.FileID, records.StatusProcessing, progressDetail(len(p.eager))); err != nil {
		return err
	}
	s.AfterCommit(ctx, func(ctx context.Context) { p.warm(ctx, s, e.Scope, e.FileID) })
	return nil
}

// focalEvent is the FocalSource's change event, or a kind no event has.
func (p *Plugin) focalEvent() records.EventKind {
	if p.focal == nil {
		return ""
	}
	return p.focal.FocalChanged()
}

// warm generates the eager variants after the change committed, passing
// each failure to the WithEager handler. Generate already recorded the
// outcome in the file's status and events.
func (p *Plugin) warm(ctx context.Context, s *records.Store, scope, fileID string) {
	_ = p.run(ctx, s, scope, fileID, p.eager, p.onEager)
}

// dropAll deletes a file's variant rows, and their objects once the
// transaction commits.
func (p *Plugin) dropAll(ctx context.Context, s *records.Store, scope, fileID string) error {
	list, err := p.list(ctx, s, scope, fileID)
	if err != nil {
		return err
	}
	del := sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
	if _, err := s.Exec(ctx, del); err != nil {
		return err
	}
	if err := release(ctx, s, list); err != nil {
		return err
	}
	deleteObjects(ctx, s, list)
	return nil
}

// deleteObjects deletes variant objects after the transaction commits.
// Failures go to the Store's object error handler; a deleted row never
// comes back, so nothing points at an object left behind.
func deleteObjects(ctx context.Context, s *records.Store, list []Variant) {
	for _, v := range list {
		s.DeleteObjectAfterCommit(ctx, records.File{ID: v.FileID, Scope: v.Scope, Disk: v.Disk, Key: v.Key})
	}
}

func (p *Plugin) list(ctx context.Context, s *records.Store, scope, fileID string) ([]Variant, error) {
	var v Variant
	var out []Variant
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&v)...).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
	err := s.Each(ctx, q, func() error { out = append(out, v); return nil })
	return out, err
}

// Get returns the named variant of a file, generating and storing it on
// first request. A trashed file has no variants (CodeNotFound), even one
// generated before it was trashed, and neither has a view-once or expired
// file (see linkable): the returned key could be streamed without
// spending the view. Concurrent first requests are safe:
// both may generate, one row wins, and the loser returns the winner's row.
// A Trash, Replace or SetVisibility that lands while a variant is being
// generated never leaves it behind: after a trash the call fails with
// CodeNotFound and deletes the object it wrote; after a change it
// generates again from the file as it now is (CodeUnavailable if the
// file keeps changing).
func (p *Plugin) Get(ctx context.Context, s *records.Store, scope, fileID, name string) (Variant, error) {
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return Variant{}, err
	}
	if err := linkable(s, f); err != nil {
		return Variant{}, err
	}
	return p.variant(ctx, s, f, name)
}

// maxGenerateAttempts bounds how many times variant generates when the
// file keeps changing (replaced, made public or private) while it works:
// each change makes the previous result stale, and a file rewritten that
// often is better served by the next request than by a loop.
const maxGenerateAttempts = 3

// Sentinels that end the recording transaction without being reported.
var (
	// errFileChanged: the file's content, disk or visibility changed
	// after it was loaded, so the generated variant is stale.
	errFileChanged = errors.New("variants: file changed during generation")
	// errLostRace: a concurrent request recorded the same variant first.
	errLostRace = errors.New("variants: variant recorded concurrently")
)

// variant is Get for a file already loaded. The trash check comes before
// the lookup, so a variant row that outlived its file's trashing is never
// served. A file that changes while its variant is generated is loaded
// again and the variant generated from it (see record).
func (p *Plugin) variant(ctx context.Context, s *records.Store, f records.File, name string) (Variant, error) {
	return p.variantIn(ctx, s, f, name, "")
}

// variantIn is variant in another output format than the declared one
// (format "" keeps it): a negotiated sibling (negotiate.go). A sibling is
// a variant of the same name whose transform differs only in its format,
// so it has its own digest, row and object.
func (p *Plugin) variantIn(ctx context.Context, s *records.Store, f records.File, name string, format imaging.Format) (Variant, error) {
	t, ok := p.transforms[name]
	if !ok {
		return Variant{}, filesystem.Errorf(filesystem.CodeNotFound, "variants: no variant named %q", name)
	}
	if format != "" {
		t.Format = format
	}
	for attempt := 1; ; attempt++ {
		v, err := p.generate(ctx, s, f, name, t)
		if !errors.Is(err, errFileChanged) {
			return v, err
		}
		if attempt == maxGenerateAttempts {
			return Variant{}, filesystem.Errorf(filesystem.CodeUnavailable, "variants: file %q changed %d times while %q was generated", f.ID, attempt, name)
		}
		if f, err = s.File(ctx, f.Scope, f.ID); err != nil {
			return Variant{}, err
		}
	}
}

// generate returns the recorded variant of f, or makes, stores and records
// it from f as loaded.
func (p *Plugin) generate(ctx context.Context, s *records.Store, f records.File, name string, t imaging.Transform) (Variant, error) {
	// Every path that makes a variant comes here, with f as loaded (and
	// loaded again after a change): unscanned bytes are never decoded.
	if f.Quarantined() {
		return Variant{}, records.ErrQuarantined
	}
	scope, fileID := f.Scope, f.ID
	if f.DeletedAt != nil {
		return Variant{}, trashed(fileID)
	}
	// The digest names the declared transform: the per-file focal point
	// joins only the Process call, and a change of it drops the rows
	// (OnEvent), so it never needs its own identity.
	digest := t.Digest()
	used := p.focalOf(f)
	t = withFocal(used, t)
	if v, err := p.find(ctx, s, scope, fileID, name, digest); err == nil {
		return v, nil
	} else if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return Variant{}, err
	}
	if !strings.HasPrefix(f.ContentType, "image/") {
		return Variant{}, filesystem.Errorf(filesystem.CodeRejected, "variants: file %q is %s, not an image", fileID, f.ContentType)
	}
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return Variant{}, err
	}
	// A density version is made no larger than its source supports; the
	// digest stays the declared transform's, as with a focal point.
	if d, ok := p.densityOf[name]; ok {
		if t, err = p.capDensity(ctx, disk, f.Key, t, d); err != nil {
			return Variant{}, err
		}
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return Variant{}, err
	}
	res, err := p.processor.Process(rc, t, p.opts)
	_ = rc.Close()
	if err != nil {
		return Variant{}, err
	}
	diskName, prefix, err := p.location(f, name)
	if err != nil {
		return Variant{}, err
	}
	if diskName == "" {
		diskName = f.Disk
	}
	dst, err := s.Disks().Disk(diskName)
	if err != nil {
		return Variant{}, err
	}
	id := strings.ToLower(rand.Text())
	_, ext := p.formatInfo(t.Format)
	v := Variant{
		ID: IDPrefix + id, Scope: scope, FileID: fileID, Name: name, Digest: digest,
		Disk: diskName, Key: variantKey(prefix, name, digest, id, ext),
		ContentType: res.ContentType, Size: int64(len(res.Data)), Width: int64(res.Width), Height: int64(res.Height),
		// The Store's clock (records.WithClock), so one clock rules the
		// rows of every plugin.
		CreatedAt: s.Now().UTC().Truncate(time.Microsecond),
	}
	// Headers come from WithHeaders alone, never from the original.
	wo := filesystem.WriteOptions{ContentType: v.ContentType, Headers: p.headers[name]}
	if dst.Capabilities().SetVisibility {
		wo.Visibility = f.Visibility
	}
	if _, err := dst.Write(ctx, v.Key, bytes.NewReader(res.Data), wo); err != nil {
		return Variant{}, err
	}
	err = p.record(ctx, s, f, v, used)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, errLostRace):
		// The winner recorded its own key; ours is an orphan.
		p.discard(ctx, s, v)
		return p.find(ctx, s, scope, fileID, name, digest)
	}
	p.discard(ctx, s, v)
	return Variant{}, err
}

// variantKey is where one generated variant is stored:
// <prefix><name>-<digest>-<id><ext> (the prefix is variants/<fileID>/ by
// default, see Location), where id is the random part
// of the variant's own ID. Every generation therefore writes a key no
// other generation, earlier or concurrent, ever writes, which is what
// makes the deferred deletions safe: a drop deletes, after its commit,
// exactly the objects of the rows it dropped, and a generation that loses
// (to a concurrent request, a trash, a change of the file) deletes only
// the object it wrote itself. A key derived from the file's state instead
// would be rewritten by a Get landing between a drop's commit and its
// deletion, and the deletion would remove the new object under a live
// row. It also means a key never shows two different images, so it can be
// cached for as long as the variant exists (see WithHeaders for what a
// CDN keeps after that). The digest stays in the key so objects are
// recognisable by transform.
func variantKey(prefix, name, digest, id, extension string) string {
	return prefix + name + "-" + digest + "-" + id + extension
}

// formatInfo is f's MIME type and extension: the processor's own answer
// when it can give one (*imaging.Pipeline's FormatInfo, which also names
// the formats its DescribedEncoders add), else the package's table.
func (p *Plugin) formatInfo(f imaging.Format) (contentType, extension string) {
	if info, ok := p.processor.(interface {
		FormatInfo(imaging.Format) (string, string, bool)
	}); ok {
		if ct, ext, ok := info.FormatInfo(f); ok {
			return ct, ext
		}
	}
	t := imaging.Transform{Format: f}
	return t.ContentType(), t.Extension()
}

// record inserts v's row, but only while f is still as it was loaded. The
// generation ran outside any transaction, and in that time the file may
// have been trashed (its variants dropped, so a row inserted now would
// outlive the drop), replaced (the variant shows the old content) or made
// public or private (the object has the wrong visibility, or sits on the
// disk the file left). So the transaction first locks the file's row, with
// an UPDATE that assigns a column to itself (the one row lock SQLite,
// Postgres and MySQL share; SELECT … FOR UPDATE is not portable), then
// reads the row again: a Trash, Replace or SetVisibility that changed it
// committed before the lock was granted, and one that has not started yet
// waits for this commit and then drops the new row with the others.
func (p *Plugin) record(ctx context.Context, s *records.Store, f records.File, v Variant, used focalPoint) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		lock := sqlb.NewUpdate(s.Dialect()).Table(records.TableFiles).Set(sqlb.SetExpr(records.FileKey, records.FileKey)).
			Where(sqlb.Eq(records.FileScope, f.Scope), sqlb.Eq(records.FileID, f.ID))
		if _, err := s.Exec(ctx, lock); err != nil {
			return err
		}
		cur, err := s.File(ctx, f.Scope, f.ID)
		switch {
		case err != nil:
			return err
		case cur.DeletedAt != nil:
			return trashed(f.ID)
		case cur.Key != f.Key || cur.Disk != f.Disk || cur.Visibility != f.Visibility || p.focalOf(cur) != used:
			return errFileChanged
		}
		ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(
			sqlb.Set(ID, v.ID), sqlb.Set(Scope, v.Scope), sqlb.Set(FileID, v.FileID), sqlb.Set(Name, v.Name),
			sqlb.Set(Digest, v.Digest), sqlb.Set(Disk, v.Disk), sqlb.Set(Key, v.Key), sqlb.Set(ContentType, v.ContentType),
			sqlb.Set(Size, v.Size), sqlb.Set(Width, v.Width), sqlb.Set(Height, v.Height), sqlb.Set(CreatedAt, v.CreatedAt),
		)
		if _, err := s.Exec(ctx, ins); err != nil {
			if records.IsUniqueViolation(err) {
				// Returned rather than caught: a failed INSERT aborts a
				// Postgres transaction, so find runs after the rollback.
				return errLostRace
			}
			return err
		}
		// Usage plugins (quota) count the variant with its row; a refusal
		// (quota_exceeded) rolls the row back and generate deletes the
		// object.
		return s.Hold(ctx, held(v))
	})
}

// held is the records.HeldObject for v.
func held(v Variant) records.HeldObject {
	return records.HeldObject{Plugin: PluginName, Scope: v.Scope, FileID: v.FileID, Disk: v.Disk, Key: v.Key, Size: v.Size}
}

// release tells usage plugins the variants in list are gone; call it in
// the transaction that deletes their rows.
func release(ctx context.Context, s *records.Store, list []Variant) error {
	for _, v := range list {
		if err := s.Release(ctx, held(v)); err != nil {
			return err
		}
	}
	return nil
}

// discard deletes the object of a variant whose row was not recorded, so a
// trashed or changed file leaves no bytes behind (a public thumbnail of a
// file that is now private or in the trash above all). No row can name
// the object: its key is unique to this generation (variantKey) and its
// insert did not commit, so the deletion needs no check. A deletion
// failure goes to the Store's object error handler.
func (p *Plugin) discard(ctx context.Context, s *records.Store, v Variant) {
	s.DeleteObjectAfterCommit(ctx, records.File{ID: v.FileID, Scope: v.Scope, Disk: v.Disk, Key: v.Key})
}

// trashed is the error for a variant of a file in the trash.
func trashed(fileID string) error {
	return filesystem.Errorf(filesystem.CodeNotFound, "variants: file %q is in the trash", fileID)
}

func (p *Plugin) find(ctx context.Context, s *records.Store, scope, fileID, name, digest string) (Variant, error) {
	var v Variant
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&v)...).From(Table).
		Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID), sqlb.Eq(Name, name), sqlb.Eq(Digest, digest))
	if err := s.One(ctx, q); err != nil {
		if errors.Is(err, sqlb.ErrNoRows) {
			return Variant{}, filesystem.Errorf(filesystem.CodeNotFound, "variants: %q of %q not generated", name, fileID)
		}
		return Variant{}, err
	}
	return v, nil
}

// The plugin stores objects of its own, so it must answer SweepOrphans.
var _ records.ObjectOwner = (*Plugin)(nil)

// OwnedObjects implements records.ObjectOwner, so Store.SweepOrphans never
// deletes a variant object: no fs_files row names one, but its fs_variants
// row does. It reports which of keys, all on the disk named disk, are
// variants.
func (p *Plugin) OwnedObjects(ctx context.Context, s *records.Store, disk string, keys []string) (map[string]bool, error) {
	owned := map[string]bool{}
	if len(keys) == 0 {
		return owned, nil
	}
	var k string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(Key, &k)).From(Table).Where(sqlb.Eq(Disk, disk), sqlb.In(Key, keys...))
	err := s.Each(ctx, q, func() error { owned[k] = true; return nil })
	if err != nil {
		return nil, err
	}
	return owned, nil
}

// URL returns a link to the named variant, generating it if needed: public
// when the file is public and the disk can build one, otherwise signed for
// ttl. A view-once or expired file has none (CodeNotFound).
func (p *Plugin) URL(ctx context.Context, s *records.Store, scope, fileID, name string, ttl time.Duration) (string, error) {
	return p.urlIn(ctx, s, scope, fileID, name, "", ttl)
}

// linkable refuses a link to a variant of an ephemeral file, the rule of
// every route that serves a file (records.Store.Viewed): an expired file
// is not served, and a view-once file is served once, through the route
// that counts its view, so its variants are never linked (a thumbnail
// would show it without spending the view). Both are CodeNotFound, as for
// a trashed file. (A quarantined file is refused by generate, which
// every variant path reaches.) Expiry is judged by the Store's clock
// (records.WithClock), the one the Store itself serves files by.
func linkable(s *records.Store, f records.File) error {
	if f.ViewOnce || f.Expired(s.Now()) {
		return filesystem.Errorf(filesystem.CodeNotFound, "variants: file %q is ephemeral; its variants are not linked", f.ID)
	}
	return nil
}

// urlIn is URL for the variant in format ("" is the declared one).
func (p *Plugin) urlIn(ctx context.Context, s *records.Store, scope, fileID, name string, format imaging.Format, ttl time.Duration) (string, error) {
	f, v, err := p.servable(ctx, s, scope, fileID, name, format)
	if err != nil {
		return "", err
	}
	return variantURL(ctx, s, f, v, ttl)
}

// servable loads scope's file fileID, checks it may be handed out
// (linkable) and returns it with its variant name in format, generated
// when missing.
func (p *Plugin) servable(ctx context.Context, s *records.Store, scope, fileID, name string, format imaging.Format) (records.File, Variant, error) {
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return records.File{}, Variant{}, err
	}
	if err := linkable(s, f); err != nil {
		return records.File{}, Variant{}, err
	}
	v, err := p.variantIn(ctx, s, f, name, format)
	if err != nil {
		return records.File{}, Variant{}, err
	}
	return f, v, nil
}

// variantURL links to variant v of f. Store.URL reports it as served: a
// ServeVariant of f, with the variant's name and its own size, and issues
// no link it could not report.
func variantURL(ctx context.Context, s *records.Store, f records.File, v Variant, ttl time.Duration) (string, error) {
	link := records.File{ID: f.ID, Scope: f.Scope, Disk: v.Disk, Key: v.Key, ContentType: v.ContentType, Visibility: f.Visibility, Size: v.Size}
	return s.URL(ctx, link, ttl, records.ServedAs(records.ServeVariant, v.Name))
}

// Prune deletes the variants whose digest no longer matches their declared
// transform (after a transform changed) or whose name is no longer
// declared, and returns how many it removed.
func (p *Plugin) Prune(ctx context.Context, s *records.Store) (int, error) {
	var v Variant
	var stale []Variant
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&v)...).From(Table)
	err := s.Each(ctx, q, func() error {
		if t, ok := p.transforms[v.Name]; !ok || !p.sibling(t, v.Digest) {
			stale = append(stale, v)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		for _, sv := range stale {
			if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(ID, sv.ID))); err != nil {
				return err
			}
		}
		if err := release(ctx, s, stale); err != nil {
			return err
		}
		deleteObjects(ctx, s, stale)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(stale), nil
}
