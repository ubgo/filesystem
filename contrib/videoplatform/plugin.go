package videoplatform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger and the prefix
// of its event kinds.
const PluginName = "videoplatform"

// TableNameAssets is the table the plugin owns: one row per published
// file.
const TableNameAssets = "fs_video_assets"

// Column names of fs_video_assets.
const (
	ColScope      = "scope"
	ColFileID     = "file_id"
	ColPlatform   = "platform"
	ColAssetID    = "asset_id"
	ColStatus     = "status"
	ColPlaybackID = "playback_id"
	ColHLS        = "hls_url"
	ColThumbnail  = "thumbnail_url"
	ColDuration   = "duration_ms"
	ColDetail     = "detail"
	ColStateAt    = "state_at_ms"
)

// Table and typed columns.
var (
	Table         = sqlb.Table{Name: TableNameAssets, Alias: "va"}
	Scope         = sqlb.Col[string](Table, ColScope)
	FileID        = sqlb.Col[string](Table, ColFileID)
	AssetPlatform = sqlb.Col[string](Table, ColPlatform)
	AssetID       = sqlb.Col[string](Table, ColAssetID)
	AssetStatus   = sqlb.Col[string](Table, ColStatus)
	PlaybackID    = sqlb.Col[string](Table, ColPlaybackID)
	HLS           = sqlb.Col[string](Table, ColHLS)
	Thumbnail     = sqlb.Col[string](Table, ColThumbnail)
	DurationMS    = sqlb.Col[int64](Table, ColDuration)
	Detail        = sqlb.Col[string](Table, ColDetail)
	// StateAt is Asset.At in Unix milliseconds, 0 when unknown.
	StateAt = sqlb.Col[int64](Table, ColStateAt)
)

// Event kinds the plugin emits, inside the transaction that records the
// change: FileID, and Detail with DetailAssetID and DetailPlatform
// (EventFailed adds DetailReason).
const (
	EventPublished records.EventKind = PluginName + ".published"
	EventReady     records.EventKind = PluginName + ".ready"
	EventFailed    records.EventKind = PluginName + ".failed"
)

// EventKindValues lists every event kind the plugin emits.
var EventKindValues = []records.EventKind{EventPublished, EventReady, EventFailed}

// Detail keys of the plugin's events.
const (
	DetailAssetID  = "asset_id"
	DetailPlatform = "platform"
	DetailReason   = "reason"
)

// DefaultSourceTTL is how long the signed URL a platform fetches the
// video from stays valid: long enough for a queue and a slow download.
const DefaultSourceTTL = time.Hour

// Column widths the plugin truncates the platform's text to.
const (
	maxTextChars = 255
	maxLongChars = 1024
)

// Plugin records the assets of files published to one Platform.
// Construct it with New and install it with records.WithPlugins.
type Plugin struct {
	platform  Platform
	sourceTTL time.Duration
	sourceURL func(ctx context.Context, s *records.Store, f records.File) (string, error)
	// createOptions fills Source.Options per file; nil sends none.
	createOptions func(ctx context.Context, s *records.Store, f records.File) (map[string]json.RawMessage, error)
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithSourceTTL sets how long the signed source URL stays valid;
// non-positive values are ignored.
func WithSourceTTL(ttl time.Duration) Option {
	return func(p *Plugin) {
		if ttl > 0 {
			p.sourceTTL = ttl
		}
	}
}

// WithSourceURL replaces how the platform's download URL is made (by
// default Store.URL with the source TTL): the escape hatch for a disk
// that cannot sign URLs, or a CDN in front of the bucket.
func WithSourceURL(fn func(ctx context.Context, s *records.Store, f records.File) (string, error)) Option {
	return func(p *Plugin) { p.sourceURL = fn }
}

// WithCreateOptions gives each published file its own create-request
// fields (Source.Options), merged over the adapter's Config.CreateOptions:
// a per-plan resolution tier or duration cap, a per-creator watermark, a
// per-tenant allowedOrigins list, read from the file row (its scope, its
// metadata) by fn. Nil, the default, adds none, so every file gets the
// Config's request.
//
// Invariant: fn runs before the platform is called; an error from it, or
// a field the adapter reserves (the source URL, the playback policy, the
// passthrough that maps webhooks back to the file), fails Publish with no
// asset created and nothing recorded.
func WithCreateOptions(fn func(ctx context.Context, s *records.Store, f records.File) (map[string]json.RawMessage, error)) Option {
	return func(p *Plugin) { p.createOptions = fn }
}

// New returns a Plugin publishing to platform.
func New(platform Platform, opts ...Option) *Plugin {
	p := &Plugin{platform: platform, sourceTTL: DefaultSourceTTL}
	p.sourceURL = func(ctx context.Context, s *records.Store, f records.File) (string, error) {
		return s.URL(ctx, f, p.sourceTTL)
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(AssetPlatform, t.Short), records.Column(AssetID, t.Key),
			records.Column(AssetStatus, t.Short), records.Column(PlaybackID, t.Text), records.Column(HLS, t.Long), records.Column(Thumbnail, t.Long),
			records.Column(DurationMS, t.Int), records.Column(Detail, t.Text),
		},
		PrimaryKey: []ddl.ColumnRef{Scope, FileID},
		Indexes:    []records.Index{{Name: "fs_video_assets_asset", Unique: true, Columns: []records.IndexColumn{records.Key(AssetPlatform), records.Key(AssetID)}}},
	}).Statements()
	if err != nil {
		return nil, err
	}
	stateAt, err := records.NewDDL(engine).AddColumn(Table, records.Column(StateAt, t.Int).Default(0)).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_video_assets", Statements: st},
		{Owner: PluginName, Name: "0002_video_assets_state_at", Statements: stateAt},
	}, err
}

// Published is a file's asset as recorded.
type Published struct {
	Scope, FileID, Platform string
	Asset                   Asset
}

// row holds the columns of a row that need converting into Published.
type row struct {
	status   string
	ms, atMS int64
}

// published converts r into v's Asset fields.
func (r row) published(v *Published) {
	v.Asset.Status, v.Asset.Duration = Status(r.status), time.Duration(r.ms)*time.Millisecond
	if r.atMS > 0 {
		v.Asset.At = time.UnixMilli(r.atMS).UTC()
	}
}

// bindings binds every column of a row to v and r.
func bindings(v *Published, r *row) []sqlb.Binding {
	status, ms := &r.status, &r.ms
	return []sqlb.Binding{
		sqlb.Bind(Scope, &v.Scope), sqlb.Bind(FileID, &v.FileID), sqlb.Bind(AssetPlatform, &v.Platform), sqlb.Bind(AssetID, &v.Asset.ID),
		sqlb.Bind(AssetStatus, status), sqlb.Bind(PlaybackID, &v.Asset.PlaybackID), sqlb.Bind(HLS, &v.Asset.HLS), sqlb.Bind(Thumbnail, &v.Asset.Thumbnail),
		sqlb.Bind(DurationMS, ms), sqlb.Bind(Detail, &v.Asset.Detail), sqlb.Bind(StateAt, &r.atMS),
	}
}

// read returns the row matching where, CodeNotFound when there is none.
func (p *Plugin) read(ctx context.Context, s *records.Store, where ...sqlb.Predicate) (Published, error) {
	var v Published
	var r row
	if err := s.One(ctx, sqlb.NewSelect(s.Dialect()).Columns(bindings(&v, &r)...).From(Table).Where(where...)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Published{}, filesystem.Errorf(filesystem.CodeNotFound, "videoplatform: no asset recorded")
		}
		return Published{}, err
	}
	r.published(&v)
	return v, nil
}

// Asset returns the asset recorded for a file of scope; CodeNotFound when
// the file was never published (or its asset was dropped).
func (p *Plugin) Asset(ctx context.Context, s *records.Store, scope, fileID string) (Published, error) {
	return p.read(ctx, s, sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
}

// Publish hands a video file of scope to the platform and records its
// asset as preparing, setting the file's status to processing. A file
// already published is CodeExists (Unpublish it first); a file that is
// not a video is CodeInvalidArgument; a quarantined one is
// records.ErrQuarantined, before the platform is called. Object first,
// row second: when recording fails after the platform created the asset,
// the asset is deleted.
func (p *Plugin) Publish(ctx context.Context, s *records.Store, scope, fileID string) (Published, error) {
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return Published{}, err
	}
	if !strings.HasPrefix(f.ContentType, "video/") {
		return Published{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "videoplatform: %s is not a video", f.ContentType)
	}
	// Unscanned bytes are never handed to the platform, whatever
	// WithSourceURL does (Store.URL would refuse them too).
	if f.Quarantined() {
		return Published{}, records.ErrQuarantined
	}
	if _, err := p.Asset(ctx, s, scope, fileID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		if err == nil {
			return Published{}, filesystem.Errorf(filesystem.CodeExists, "videoplatform: file %q is already published", fileID)
		}
		return Published{}, err
	}
	url, err := p.sourceURL(ctx, s, f)
	if err != nil {
		return Published{}, err
	}
	src := Source{URL: url, Passthrough: scope + "/" + fileID}
	if p.createOptions != nil {
		if src.Options, err = p.createOptions(ctx, s, f); err != nil {
			return Published{}, fmt.Errorf("videoplatform: create options for %q: %w", fileID, err)
		}
	}
	asset, err := p.platform.Ingest(ctx, src)
	if err != nil {
		return Published{}, err
	}
	v := Published{Scope: scope, FileID: fileID, Platform: p.platform.Name(), Asset: clean(asset)}
	scheduled := false
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		s.OnRollback(ctx, func(ctx context.Context) { _ = p.platform.Delete(ctx, asset.ID) })
		scheduled = true
		ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(p.assignments(v)...)
		if _, err := s.Exec(ctx, ins); err != nil {
			return err
		}
		return p.report(ctx, s, v, EventPublished)
	})
	if err != nil {
		if !scheduled {
			// The transaction never began (a request cancelled during
			// Ingest), so no rollback will drop the asset: drop it here.
			_ = p.platform.Delete(context.WithoutCancel(ctx), asset.ID)
		}
		return Published{}, err
	}
	return v, nil
}

// Unpublish drops a file's asset: its row now, and the asset on the
// platform once that commits. The file's status is left as it is. A file
// never published is CodeNotFound.
func (p *Plugin) Unpublish(ctx context.Context, s *records.Store, scope, fileID string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		v, err := p.Asset(ctx, s, scope, fileID)
		if err != nil {
			return err
		}
		return p.drop(ctx, s, v)
	})
}

// drop deletes v's row and, after commit, its asset.
func (p *Plugin) drop(ctx context.Context, s *records.Store, v Published) error {
	if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, v.Scope), sqlb.Eq(FileID, v.FileID))); err != nil {
		return err
	}
	s.AfterCommit(ctx, func(ctx context.Context) { _ = p.platform.Delete(ctx, v.Asset.ID) })
	return nil
}

// Sync asks the platform for a file's asset and records what it says: the
// fallback for a webhook that never arrived. It returns the asset as
// recorded.
func (p *Plugin) Sync(ctx context.Context, s *records.Store, scope, fileID string) (Published, error) {
	v, err := p.Asset(ctx, s, scope, fileID)
	if err != nil {
		return Published{}, err
	}
	asset, err := p.platform.Asset(ctx, v.Asset.ID)
	if err != nil {
		return Published{}, err
	}
	return p.apply(ctx, s, asset)
}

// apply records asset's state on the row of the file it belongs to and
// sets the file's status to match, in one transaction. Changes are
// monotonic (admit), so a replayed, late or undated report cannot move an
// asset back, and an event is emitted only when the status changes. An
// undated state keeps the recorded date. An asset no row knows is
// CodeNotFound.
func (p *Plugin) apply(ctx context.Context, s *records.Store, asset Asset) (Published, error) {
	var out Published
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		v, err := p.read(ctx, s, sqlb.Eq(AssetPlatform, p.platform.Name()), sqlb.Eq(AssetID, asset.ID))
		if err != nil {
			return err
		}
		out = v
		next := clean(asset)
		if !admit(v.Asset, next) {
			return nil
		}
		if next.At.IsZero() {
			next.At = v.Asset.At
		}
		if next == v.Asset {
			return nil
		}
		prev := v.Asset.Status
		v.Asset = next
		upd := sqlb.NewUpdate(s.Dialect()).Table(Table).Set(p.assignments(v)[2:]...).Where(sqlb.Eq(Scope, v.Scope), sqlb.Eq(FileID, v.FileID))
		if _, err := s.Exec(ctx, upd); err != nil {
			return err
		}
		out = v
		if next.Status == prev {
			return nil // the row is refreshed; the state is not news
		}
		switch next.Status {
		case StatusReady:
			return p.report(ctx, s, v, EventReady)
		case StatusErrored, StatusDeleted:
			return p.report(ctx, s, v, EventFailed)
		}
		return nil
	})
	return out, err
}

// admit decides whether next, a state the platform reported, may replace
// cur, the recorded one, so that a replayed, late or undated report can
// never move an asset back:
//   - nothing leaves StatusDeleted;
//   - preparing never replaces a later state;
//   - when both are dated, an older next is refused, and on equal times
//     (two events in one millisecond) only a further state (errored over
//     ready) wins;
//   - an undated next (a webhook whose time did not parse, Sync on a
//     platform whose asset carries none) may not leave a terminal state
//     except further along (errored to deleted); the same state only
//     refreshes the row, since apply emits on a status change alone.
func admit(cur, next Asset) bool {
	switch {
	case cur.Status == StatusDeleted:
		return false
	case next.Status == StatusPreparing && cur.Status != StatusPreparing:
		return false
	case !next.At.IsZero() && !cur.At.IsZero():
		if next.At.Equal(cur.At) {
			return next.Status.rank() > cur.Status.rank()
		}
		return next.At.After(cur.At)
	case cur.Status.terminal():
		return next.Status.rank() >= cur.Status.rank()
	}
	return true
}

// report sets the file's records status for v and emits kind.
func (p *Plugin) report(ctx context.Context, s *records.Store, v Published, kind records.EventKind) error {
	st, detail := records.StatusProcessing, ""
	switch v.Asset.Status {
	case StatusReady:
		st = records.StatusReady
	case StatusErrored, StatusDeleted:
		st, detail = records.StatusFailed, truncate("video platform: "+v.Asset.Detail, records.MaxStatusDetailChars)
	}
	if _, err := s.SetStatus(ctx, v.Scope, v.FileID, st, detail); err != nil {
		return err
	}
	e := records.Event{Kind: kind, Scope: v.Scope, FileID: v.FileID, Detail: map[string]string{DetailAssetID: v.Asset.ID, DetailPlatform: v.Platform}}
	if kind == EventFailed {
		e.Detail[DetailReason] = v.Asset.Detail
	}
	return s.Emit(ctx, p, e)
}

// assignments are every column of v's row, the key columns first.
func (p *Plugin) assignments(v Published) []sqlb.Assignment {
	return []sqlb.Assignment{
		sqlb.Set(Scope, v.Scope), sqlb.Set(FileID, v.FileID),
		sqlb.Set(AssetPlatform, v.Platform), sqlb.Set(AssetID, v.Asset.ID), sqlb.Set(AssetStatus, string(v.Asset.Status)),
		sqlb.Set(PlaybackID, v.Asset.PlaybackID), sqlb.Set(HLS, v.Asset.HLS), sqlb.Set(Thumbnail, v.Asset.Thumbnail),
		sqlb.Set(DurationMS, v.Asset.Duration.Milliseconds()), sqlb.Set(Detail, v.Asset.Detail), sqlb.Set(StateAt, stateAtMS(v.Asset.At)),
	}
}

// stateAtMS is at in Unix milliseconds, 0 for the zero time.
func stateAtMS(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}

// clean makes a platform's asset fit the row: an unknown status becomes
// errored, text is cut to its column and stripped of control characters,
// sub-millisecond durations and times are dropped (the row keeps
// milliseconds), and Passthrough is not stored.
func clean(a Asset) Asset {
	if !slices.Contains(StatusValues, a.Status) {
		a.Detail, a.Status = "unknown status "+string(a.Status), StatusErrored
	}
	a.PlaybackID, a.Detail = truncate(a.PlaybackID, maxTextChars), truncate(a.Detail, maxTextChars)
	a.HLS, a.Thumbnail = truncate(a.HLS, maxLongChars), truncate(a.Thumbnail, maxLongChars)
	a.Duration = a.Duration.Truncate(time.Millisecond)
	if !a.At.IsZero() {
		a.At = a.At.Truncate(time.Millisecond).UTC()
	}
	a.Passthrough = ""
	return a
}

// truncate drops control characters and invalid UTF-8 from v and cuts it
// to n characters.
func truncate(v string, n int) string {
	v = strings.ToValidUTF8(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v), "")
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	return string([]rune(v)[:n])
}

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: the file's asset goes with
// it, on the platform after the purge commits.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	return p.dropIfAny(ctx, s, f)
}

// BeforeReplace implements records.ReplaceHooks: the asset shows the old
// content, so it is dropped; publish the file again for the new one.
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, current records.File, _ *records.File) error {
	return p.dropIfAny(ctx, s, current)
}

// PurgeScope implements records.ScopePurger: rows of the scope left
// behind go too, and their assets after commit.
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	var left []Published
	var v Published
	var r row
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&v, &r)...).From(Table).Where(sqlb.Eq(Scope, scope))
	if err := s.Each(ctx, q, func() error { r.published(&v); left = append(left, v); return nil }); err != nil {
		return err
	}
	for _, v := range left {
		if err := p.drop(ctx, s, v); err != nil {
			return err
		}
	}
	return nil
}

// dropIfAny drops f's asset when it has one.
func (p *Plugin) dropIfAny(ctx context.Context, s *records.Store, f records.File) error {
	v, err := p.Asset(ctx, s, f.Scope, f.ID)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		return nil
	case err != nil:
		return err
	}
	return p.drop(ctx, s, v)
}
