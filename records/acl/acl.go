// Package acl is a records plugin for permissions on files and folders:
// grants of a level (read, write, admin) to a subject on one file or on a
// folder, inherited by every file in the folder and its subfolders, and a
// Can check that resolves them. Sharing with named users, team folders
// and "editors of this folder" are grants; who the subjects are (users,
// groups, API keys) is the application's, and so is expanding a user
// into the groups it belongs to: Can takes every subject the caller
// speaks for.
//
// Grants allow by default. WithDeny adds deny grants (Grant.Effect Deny):
// a deny anywhere on a file's path, the file itself, a folder above it or
// a container holding it, beats every allow there, explicit or inherited,
// so "everyone in the team folder except the contractor" is one allow and
// one deny.
//
// Grants on folders need a Hierarchy (the folders plugin is one): without
// it only file grants exist. Grants go with their file when it is purged
// and with their scope (records.ScopePurger); a grant on a deleted folder
// matches nothing, since folder IDs are never reused.
package acl

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// PluginName is the plugin's name in the migration ledger.
const PluginName = "acl"

// TableNameGrants is the table the plugin owns.
const TableNameGrants = "fs_acl_grants"

// Column names of fs_acl_grants.
const (
	ColID        = "id"
	ColScope     = "scope"
	ColSubject   = "subject"
	ColKind      = "resource_kind"
	ColResource  = "resource_id"
	ColLevel     = "level"
	ColCreatedAt = "created_at"
	// ColExpiresAt came with migration 0002: NULL for a grant that never
	// expires.
	ColExpiresAt = "expires_at"
	// ColEffect came with migration 0003: Allow for every grant made
	// before it, and for every grant without WithDeny.
	ColEffect = "effect"
)

// Table and typed columns.
var (
	Table     = sqlb.Table{Name: TableNameGrants, Alias: "g"}
	ID        = sqlb.Col[string](Table, ColID)
	Scope     = sqlb.Col[string](Table, ColScope)
	Subject   = sqlb.Col[string](Table, ColSubject)
	Kind      = sqlb.Col[ResourceKind](Table, ColKind)
	Resource  = sqlb.Col[string](Table, ColResource)
	LevelCol  = sqlb.Col[Level](Table, ColLevel)
	CreatedAt = sqlb.Col[time.Time](Table, ColCreatedAt)
	// ExpiresAt is nil for a grant that never expires; a grant whose time
	// has passed matches nothing (Can, CanFolder) and goes with Prune.
	ExpiresAt = sqlb.Col[*time.Time](Table, ColExpiresAt)
	// EffectCol is the grant's Effect (Allow or Deny).
	EffectCol = sqlb.Col[Effect](Table, ColEffect)
)

// Effect is what a grant does: Allow gives its level, Deny takes it away
// (WithDeny).
type Effect string

// The closed set of effects.
const (
	// Allow gives the grant's level, and every level it includes.
	Allow Effect = "allow"
	// Deny refuses the grant's level, and every level that includes it,
	// whatever allows the subject holds (see Can). Only a Plugin built
	// with WithDeny gives deny grants; every Plugin honours the ones
	// stored.
	Deny Effect = "deny"
)

// EffectValues lists every Effect.
var EffectValues = []Effect{Allow, Deny}

// ResourceKind is what a grant is on: OnFile, OnFolder, or the Kind of a
// Container installed with WithContainers.
type ResourceKind string

// The built-in resource kinds.
const (
	OnFile   ResourceKind = "file"
	OnFolder ResourceKind = "folder"
)

// Level is what a grant allows. The default levels form a ladder, each
// including the ones below it; WithLevels replaces them with an
// application's own.
type Level string

// The default levels, lowest first.
const (
	// Read sees and downloads.
	Read Level = "read"
	// Write also replaces, trashes, restores and changes visibility and
	// text.
	Write Level = "write"
	// Admin also purges and grants.
	Admin Level = "admin"
)

// LevelValues lists every default Level, lowest first.
var LevelValues = []Level{Read, Write, Admin}

// DefaultLevels is the default ladder as LevelDefs: Write includes Read,
// Admin includes Write. It is what a Plugin uses without WithLevels, and a
// starting point for an application that extends the ladder.
func DefaultLevels() []LevelDef {
	return []LevelDef{{Level: Read}, {Level: Write, Includes: []Level{Read}}, {Level: Admin, Includes: []Level{Write}}}
}

// Valid reports whether l is one of the default LevelValues. A Plugin
// with WithLevels checks its own set instead.
func (l Level) Valid() bool {
	for _, v := range LevelValues {
		if v == l {
			return true
		}
	}
	return false
}

// LevelDef defines one level for WithLevels: the level and the levels a
// grant of it also gives (directly; inclusion is transitive). A level
// includes itself.
type LevelDef struct {
	Level    Level
	Includes []Level
}

// namePattern constrains level names and Container kinds: both are stored
// in short columns and compared as plain strings.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Limits.
const (
	// IDPrefix starts every grant ID.
	IDPrefix = "acl_"
	// MaxSubjectBytes is the longest subject, the indexed width of its
	// column.
	MaxSubjectBytes = records.MaxActorBytes
)

// Event kinds the plugin emits through records.Store.Emit: FileID set for
// a grant on a file, Subject the grant's subject, Detail DetailKind,
// DetailResource and DetailLevel.
const (
	EventGranted records.EventKind = PluginName + ".granted"
	EventRevoked records.EventKind = PluginName + ".revoked"
)

// EventKindValues lists every event kind the plugin emits.
var EventKindValues = []records.EventKind{EventGranted, EventRevoked}

// Detail keys of the plugin's events.
const (
	DetailKind     = "kind"
	DetailResource = "resource"
	DetailLevel    = "level"
	// DetailExpiresAt is the grant's ExpiresAt (RFC 3339, UTC), present
	// only for a grant that expires.
	DetailExpiresAt = "expires_at"
	// DetailEffect is Deny for a deny grant; absent for an allow, so the
	// events of a Plugin without WithDeny read as they always have.
	DetailEffect = "effect"
)

// Grant is one permission: Subject may act at Level on the resource.
type Grant struct {
	ID        string
	Scope     string
	Subject   string
	Kind      ResourceKind
	Resource  string
	Level     Level
	CreatedAt time.Time
	// ExpiresAt, when set, ends the grant at that time: "access until
	// Friday". nil never expires. It must be after the grant is given.
	// A deny expires the same way: "no access while the audit runs".
	ExpiresAt *time.Time
	// Effect is Allow or Deny; "" is Allow. Deny needs WithDeny. A
	// subject has one grant per resource, so a deny given where the
	// subject had an allow replaces it, and the other way round.
	Effect Effect
}

func bindings(g *Grant) []sqlb.Binding {
	return []sqlb.Binding{
		sqlb.Bind(ID, &g.ID), sqlb.Bind(Scope, &g.Scope), sqlb.Bind(Subject, &g.Subject), sqlb.Bind(Kind, &g.Kind),
		sqlb.Bind(Resource, &g.Resource), sqlb.Bind(LevelCol, &g.Level), sqlb.Bind(CreatedAt, &g.CreatedAt),
		sqlb.Bind(ExpiresAt, &g.ExpiresAt), sqlb.Bind(EffectCol, &g.Effect),
	}
}

// Hierarchy places files in folders, for grants on folders: the folders
// plugin satisfies it.
type Hierarchy interface {
	// FolderOf returns the folder a file of scope is in ("" for none).
	FolderOf(ctx context.Context, s *records.Store, scope, fileID string) (string, error)
	// Ancestors returns folder id and every folder above it, nearest
	// first; CodeNotFound for an unknown folder.
	Ancestors(ctx context.Context, s *records.Store, scope, id string) ([]string, error)
}

// Container is a kind of resource, other than a file or a folder, that
// holds files and can carry grants: an owner ("everyone granted on post
// 42 may read its gallery"), a share collection, a project the
// application keeps. A grant on one reaches every file the container
// says it holds.
//
// Why it exists: the plugin knows files and, through a Hierarchy,
// folders; anything else that groups files is the application's model,
// so the plugin asks rather than assumes.
type Container interface {
	// Kind is the ResourceKind of grants on this container: a name of
	// [a-z][a-z0-9_]*, at most 64 bytes, neither OnFile nor OnFolder.
	Kind() ResourceKind
	// Of returns the IDs of the containers of this kind that hold the
	// file fileID of scope; none is fine. An error fails the check that
	// asked (Can refuses), so a container that cannot answer never grants.
	Of(ctx context.Context, s *records.Store, scope, fileID string) ([]string, error)
	// Check returns nil when id names a container of this kind in scope,
	// CodeNotFound otherwise; Grant calls it so no grant points at
	// nothing.
	Check(ctx context.Context, s *records.Store, scope, id string) error
}

// Plugin keeps grants. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	now       func() time.Time
	hierarchy Hierarchy
	// levelFor maps an action on a target to the level it needs; LevelFor
	// by default (WithLevelFor, WithLevelForTarget).
	levelFor LevelMapper
	// precedence decides a check from its matching grants; DenyOverrides
	// by default (WithPrecedence).
	precedence Precedence
	// perLevel is WithLevelsPerSubject: a subject holds one grant per
	// level on a resource instead of one grant in all.
	perLevel bool
	// includes is the transitive closure of the level definitions: the
	// levels a grant of each level gives, itself included.
	includes map[Level]map[Level]bool
	// containers are WithContainers', by Kind.
	containers map[ResourceKind]Container
	// deny is WithDeny: Grant accepts Effect Deny. Can honours stored
	// deny grants whether or not it is set, so turning it off never turns
	// a refusal into access.
	deny bool
	// err is the first invalid option; every method that reads grants or
	// writes them returns it, so a misconfigured plugin fails closed.
	err error
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithHierarchy enables grants on folders, inherited through h.
func WithHierarchy(h Hierarchy) Option { return func(p *Plugin) { p.hierarchy = h } }

// LevelMapper maps an action on a target to the level Authorizer asks Can
// for. ok false leaves the action to Authorizer's next rule. It runs
// after the target's file is loaded and, on the visibility and text
// routes, after the requested change is read (records.ActionTarget.Change),
// so it can decide on both: "making a file public needs Admin, making it
// private needs Write", "a file under legal hold needs Admin to trash".
// The target is as recordshttp built it and must not be modified.
type LevelMapper func(ctx context.Context, a records.Action, t records.ActionTarget) (Level, bool)

// WithLevelFor replaces LevelFor as the map from an action to the level
// Authorizer asks Can for: "changing visibility needs Admin", "setting
// text needs only Read", or an action mapped to a level of WithLevels.
// ok false leaves the action to Authorizer's next rule. A level fn
// returns that the plugin does not define makes Can fail, which refuses
// the request. A nil fn is an invalid option. A map that needs the
// request, the file or the change asked for takes WithLevelForTarget.
func WithLevelFor(fn func(records.Action) (Level, bool)) Option {
	return func(p *Plugin) {
		if fn == nil {
			p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: nil WithLevelFor"))
			return
		}
		p.levelFor = func(_ context.Context, a records.Action, _ records.ActionTarget) (Level, bool) { return fn(a) }
	}
}

// WithLevelForTarget is WithLevelFor with the request's context and the
// action's target (see LevelMapper). It replaces LevelFor, and
// WithLevelFor's function when both are given (the later option wins). A
// level it returns that the plugin does not define makes Can fail, which
// refuses the request. A nil fn is an invalid option.
func WithLevelForTarget(fn LevelMapper) Option {
	return func(p *Plugin) {
		if fn == nil {
			p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: nil WithLevelForTarget"))
			return
		}
		p.levelFor = fn
	}
}

// WithPrecedence replaces DenyOverrides, the default, as the rule that
// decides a check from the grants that match it (see Precedence):
// MostSpecific for NTFS-style broken inheritance, or the application's
// own. It matters only where deny grants exist (WithDeny, or ones already
// stored); with allows alone every built-in precedence grants what any
// applying allow gives. A nil fn is an invalid option.
func WithPrecedence(fn Precedence) Option {
	return func(p *Plugin) {
		if fn == nil {
			p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: nil WithPrecedence"))
			return
		}
		p.precedence = fn
	}
}

// WithLevelsPerSubject lets a subject hold several grants on one
// resource, one per level: "comment" and "download" on one file when the
// levels of WithLevels do not form a ladder, or an allow of one level
// beside a deny of another. Grant then replaces only the subject's grant
// of the same level; Revoke still removes every grant of the subject
// there, and RevokeLevel one level's.
//
// It adds migration 0004_grants_level_key, which widens the unique key
// from (scope, kind, resource, subject) to include the level: run
// Migrate after turning it on. Without it a subject keeps one grant per
// resource, as it always has. Once migrated, keep the option: a Plugin
// without it still reads and replaces every grant correctly, but the
// database no longer stops two concurrent Grants from leaving a subject
// two levels on one resource.
func WithLevelsPerSubject() Option { return func(p *Plugin) { p.perLevel = true } }

// WithLevels replaces the default ladder (DefaultLevels) with the
// application's levels: "comment" that includes "view", a "view" that
// cannot download, an "upload" level for drop-box folders that includes
// nothing else. A grant of a level gives every level it includes,
// transitively; levels need not form a ladder. Names follow
// [a-z][a-z0-9_]*, at most 64 bytes; each is defined once, and every
// included level must be defined. Map actions to them with WithLevelFor.
// Grants of a level no longer defined match nothing.
func WithLevels(defs ...LevelDef) Option {
	return func(p *Plugin) {
		includes, err := closure(defs)
		if err != nil {
			p.fail(err)
			return
		}
		p.includes = includes
	}
}

// WithContainers lets grants be given on the containers' kinds (Grant
// with Kind c.Kind()), and makes Can honour them for every file a
// container holds. Each kind must be valid and unique.
func WithContainers(cs ...Container) Option {
	return func(p *Plugin) {
		for _, c := range cs {
			if c == nil {
				p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: nil Container"))
				return
			}
			k := c.Kind()
			if !namePattern.MatchString(string(k)) || k == OnFile || k == OnFolder || p.containers[k] != nil {
				p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: container kind %q must match %s, not be %q or %q, and be unique", k, namePattern, OnFile, OnFolder))
				return
			}
			p.containers[k] = c
		}
	}
}

// WithDeny lets Grant give deny grants (Grant.Effect Deny): a subject
// refused a level on a file, a folder or a container, whatever it is
// allowed there or above. Precedence by default is the safe one
// (DenyOverrides): a deny that applies to the request wins over every
// allow, explicit or inherited, on the same resource or a broader one (a
// folder above, a container), and over an allow on a narrower one too (a
// file allow does not reopen a folder deny). WithPrecedence changes that,
// for instance to MostSpecific. A deny of a level refuses that level and
// every level that includes it: denying Write leaves Read and refuses
// Write and Admin. An expired deny refuses nothing. Without WithDeny a
// deny grant is CodeInvalidArgument, and the plugin behaves as it always
// has; deny grants already stored are honoured either way.
func WithDeny() Option { return func(p *Plugin) { p.deny = true } }

// fail keeps the first invalid option's error.
func (p *Plugin) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}

// closure checks level definitions and returns, for each level, every
// level a grant of it gives.
func closure(defs []LevelDef) (map[Level]map[Level]bool, error) {
	direct := map[Level][]Level{}
	for _, d := range defs {
		if !namePattern.MatchString(string(d.Level)) {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: level %q must match %s", d.Level, namePattern)
		}
		if _, dup := direct[d.Level]; dup {
			return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: level %q defined twice", d.Level)
		}
		direct[d.Level] = d.Includes
	}
	if len(direct) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: WithLevels needs at least one level")
	}
	out := map[Level]map[Level]bool{}
	for l := range direct {
		seen := map[Level]bool{}
		stack := []Level{l}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[cur] {
				continue
			}
			inc, ok := direct[cur]
			if !ok {
				return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: level %q includes %q, which is not defined", l, cur)
			}
			seen[cur] = true
			stack = append(stack, inc...)
		}
		out[l] = seen
	}
	return out, nil
}

// defined reports whether l is one of the plugin's levels.
func (p *Plugin) defined(l Level) bool { return p.includes[l] != nil }

// grants reports whether a grant of held gives want.
func (p *Plugin) grants(held, want Level) bool { return p.includes[held][want] }

// Levels returns the plugin's levels (the default ladder, or WithLevels'),
// sorted by name.
func (p *Plugin) Levels() []Level { return slices.Sorted(maps.Keys(p.includes)) }

// WithClock gives this plugin its own clock, for tests. Without it the
// plugin reads records.Store.Now, so records.WithClock rules the plugin
// and the Store alike; times are kept in UTC at microsecond precision
// either way.
func WithClock(now func() time.Time) Option { return func(p *Plugin) { p.now = now } }

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{
		levelFor:   func(_ context.Context, a records.Action, _ records.ActionTarget) (Level, bool) { return LevelFor(a) },
		precedence: DenyOverrides,
		containers: map[ResourceKind]Container{},
	}
	p.includes, _ = closure(DefaultLevels()) // the default ladder is valid
	for _, o := range opts {
		o(p)
	}
	return p
}

// clock is WithClock's time when set, else the Store's (records.WithClock),
// in UTC at microsecond precision, the precision every engine keeps.
func (p *Plugin) clock(s *records.Store) time.Time {
	if p.now == nil {
		return s.Now()
	}
	return p.now().UTC().Truncate(time.Microsecond)
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Migrations implements records.Migrator.
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(Scope, t.Key), records.Column(Subject, t.Key), records.Column(Kind, t.Short),
			records.Column(Resource, t.Key), records.Column(LevelCol, t.Short), records.Column(CreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		Indexes: []records.Index{
			{Name: indexResource, Unique: true, Columns: []records.IndexColumn{records.Key(Scope), records.Key(Kind), records.Key(Resource), records.Key(Subject)}},
			{Name: "fs_acl_grants_subject", Columns: []records.IndexColumn{records.Key(Scope), records.Key(Subject)}},
		},
	}).Statements()
	more, err2 := records.NewDDL(engine).AddColumn(Table, records.Column(ExpiresAt, t.Timestamp)).Statements()
	effect, err3 := records.NewDDL(engine).AddColumn(Table, records.Column(EffectCol, t.Short).Default(Allow)).Statements()
	ms := []records.Migration{
		{Owner: PluginName, Name: "0001_grants", Statements: st},
		{Owner: PluginName, Name: "0002_grants_expiry", Statements: more},
		{Owner: PluginName, Name: "0003_grants_effect", Statements: effect},
	}
	var err4 error
	if p.perLevel {
		// The new key first, the old one after, so the table always has a
		// unique key. Every row the old key allowed is unique under the
		// wider one, so the new index always builds.
		var key []string
		key, err4 = records.NewDDL(engine).
			CreateIndex(Table, records.Index{Name: indexResourceLevel, Unique: true, Columns: []records.IndexColumn{
				records.Key(Scope), records.Key(Kind), records.Key(Resource), records.Key(Subject), records.Key(LevelCol),
			}}).
			DropIndex(Table, indexResource).Statements()
		ms = append(ms, records.Migration{Owner: PluginName, Name: "0004_grants_level_key", Statements: key})
	}
	return ms, errors.Join(p.err, err, err2, err3, err4)
}

// Index names of fs_acl_grants: the unique key of migration 0001, and the
// wider one WithLevelsPerSubject's migration 0004 replaces it with.
const (
	indexResource      = "fs_acl_grants_resource"
	indexResourceLevel = "fs_acl_grants_resource_level"
)

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(context.Context, *records.Store, *records.File) error { return nil }

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: a purged file's grants go too.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, f.Scope), sqlb.Eq(Kind, OnFile), sqlb.Eq(Resource, f.ID)))
	return err
}

// PurgeScope implements records.ScopePurger: every grant of the scope goes.
func (p *Plugin) PurgeScope(ctx context.Context, s *records.Store, scope string) error {
	_, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope)))
	return err
}

// Grant gives subject level on a live file, a folder or a container of
// scope, replacing the grant it had there (its level and expiry; with
// WithLevelsPerSubject only its grant of the same level). A
// folder needs WithHierarchy, a container's kind WithContainers. An
// unknown file, folder or container is CodeNotFound; a bad subject, kind
// or level, or an ExpiresAt not after now, is CodeInvalidArgument.
func (p *Plugin) Grant(ctx context.Context, s *records.Store, scope string, g Grant) (Grant, error) {
	if p.err != nil {
		return Grant{}, p.err
	}
	if g.Subject == "" || len(g.Subject) > MaxSubjectBytes || !p.defined(g.Level) {
		return Grant{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: a grant needs a subject of 1 to %d bytes and a level of %v", MaxSubjectBytes, p.Levels())
	}
	switch g.Effect {
	case "", Allow:
		g.Effect = Allow
	case Deny:
		if !p.deny {
			return Grant{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: deny grants need WithDeny")
		}
	default:
		return Grant{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: unknown effect %q", g.Effect)
	}
	now := p.clock(s)
	if g.ExpiresAt != nil {
		at := g.ExpiresAt.UTC().Truncate(time.Microsecond)
		if !at.After(now) {
			return Grant{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: a grant's ExpiresAt must be after now")
		}
		g.ExpiresAt = &at
	}
	if err := p.checkResource(ctx, s, scope, g.Kind, g.Resource); err != nil {
		return Grant{}, err
	}
	g.ID, g.Scope, g.CreatedAt = s.NewID(IDPrefix), scope, now
	err := s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		replaced := p.at(scope, g.Kind, g.Resource, g.Subject)
		if p.perLevel {
			replaced = append(replaced, sqlb.Eq(LevelCol, g.Level))
		}
		if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(replaced...)); err != nil {
			return err
		}
		ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(
			sqlb.Set(ID, g.ID), sqlb.Set(Scope, g.Scope), sqlb.Set(Subject, g.Subject), sqlb.Set(Kind, g.Kind),
			sqlb.Set(Resource, g.Resource), sqlb.Set(LevelCol, g.Level), sqlb.Set(CreatedAt, g.CreatedAt),
			sqlb.Set(ExpiresAt, g.ExpiresAt), sqlb.Set(EffectCol, g.Effect),
		)
		if _, err := s.Exec(ctx, ins); err != nil {
			if records.IsUniqueViolation(err) {
				return filesystem.NewError(filesystem.CodeUnavailable, err) // a concurrent grant of the same; retry
			}
			return err
		}
		return p.emit(ctx, s, EventGranted, g)
	})
	if err != nil {
		return Grant{}, err
	}
	return g, nil
}

// Revoke removes subject's grants on a resource of scope (one, unless
// WithLevelsPerSubject gave it several), with an EventRevoked for each;
// revoking a grant that does not exist does nothing.
func (p *Plugin) Revoke(ctx context.Context, s *records.Store, scope string, kind ResourceKind, resource, subject string) error {
	return p.revoke(ctx, s, p.at(scope, kind, resource, subject))
}

// RevokeLevel removes subject's grant of level on a resource of scope,
// leaving its other levels there (WithLevelsPerSubject); revoking a grant
// that does not exist does nothing.
func (p *Plugin) RevokeLevel(ctx context.Context, s *records.Store, scope string, kind ResourceKind, resource, subject string, level Level) error {
	return p.revoke(ctx, s, append(p.at(scope, kind, resource, subject), sqlb.Eq(LevelCol, level)))
}

// revoke deletes the grants where matches, emitting EventRevoked for each.
func (p *Plugin) revoke(ctx context.Context, s *records.Store, where []sqlb.Predicate) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		gs, err := p.list(ctx, s, where...)
		if err != nil {
			return err
		}
		for _, g := range gs {
			if _, err := s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(ID, g.ID))); err != nil {
				return err
			}
			if err := p.emit(ctx, s, EventRevoked, g); err != nil {
				return err
			}
		}
		return nil
	})
}

// Grants lists the grants on one file or folder of scope, by subject: the
// "shared with" panel.
func (p *Plugin) Grants(ctx context.Context, s *records.Store, scope string, kind ResourceKind, resource string) ([]Grant, error) {
	return p.list(ctx, s, sqlb.Eq(Scope, scope), sqlb.Eq(Kind, kind), sqlb.Eq(Resource, resource))
}

// GrantsOf lists every grant of subject in scope: the "shared with me"
// view.
func (p *Plugin) GrantsOf(ctx context.Context, s *records.Store, scope, subject string) ([]Grant, error) {
	return p.list(ctx, s, sqlb.Eq(Scope, scope), sqlb.Eq(Subject, subject))
}

func (p *Plugin) list(ctx context.Context, s *records.Store, where ...sqlb.Predicate) ([]Grant, error) {
	var g Grant
	var out []Grant
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&g)...).From(Table).Where(where...).
		Order(sqlb.OrderBy(Subject, sqlb.SortAsc), sqlb.OrderBy(Kind, sqlb.SortAsc), sqlb.OrderBy(Resource, sqlb.SortAsc))
	err := s.Each(ctx, q, func() error {
		g.CreatedAt = g.CreatedAt.UTC()
		if g.ExpiresAt != nil {
			at := g.ExpiresAt.UTC()
			g.ExpiresAt = &at
		}
		out = append(out, g)
		return nil
	})
	return out, err
}

// Can reports whether any of subjects (a user and the groups the caller
// says it is in) holds a grant that gives level on the file fileID of
// scope: a grant on the file itself, on the folder it is in or any folder
// above (WithHierarchy), or on a container that holds it
// (WithContainers). Any grant that gives the level is enough, unless a
// deny grant applies (WithDeny): a deny of the level, or of a level it
// includes, on any of those resources refuses, whatever allows there are.
// WithPrecedence replaces that rule. An expired grant, allow or deny,
// does nothing. An unknown file is CodeNotFound; a level the plugin does
// not define is CodeInvalidArgument, and so is a plugin built with an
// invalid option; a Precedence's error is returned as it is.
func (p *Plugin) Can(ctx context.Context, s *records.Store, scope string, subjects []string, fileID string, level Level) (bool, error) {
	if err := p.checkLevel(level); err != nil {
		return false, err
	}
	if _, err := s.File(ctx, scope, fileID); err != nil {
		return false, err
	}
	resources := []sqlb.Predicate{sqlb.And(sqlb.Eq(Kind, OnFile), sqlb.Eq(Resource, fileID))}
	depth := depths{{OnFile, fileID}: 0}
	if p.hierarchy != nil {
		folder, err := p.hierarchy.FolderOf(ctx, s, scope, fileID)
		if err != nil {
			return false, err
		}
		if folder != "" {
			chain, err := p.hierarchy.Ancestors(ctx, s, scope, folder)
			if err != nil {
				return false, err
			}
			resources = append(resources, sqlb.And(sqlb.Eq(Kind, OnFolder), sqlb.In(Resource, chain...)))
			depth.folders(chain, 1)
		}
	}
	for kind, c := range p.containers {
		ids, err := c.Of(ctx, s, scope, fileID)
		if err != nil {
			return false, err
		}
		if len(ids) > 0 {
			resources = append(resources, sqlb.And(sqlb.Eq(Kind, kind), sqlb.In(Resource, ids...)))
			for _, id := range ids {
				depth[resourceRef{kind, id}] = ContainerDepth
			}
		}
	}
	return p.holds(ctx, s, scope, subjects, resources, depth, level)
}

// checkLevel refuses a level the plugin does not define, and every check
// of a plugin built with an invalid option.
func (p *Plugin) checkLevel(level Level) error {
	if p.err != nil {
		return p.err
	}
	if !p.defined(level) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: unknown level %q", level)
	}
	return nil
}

// CanFolder is Can for a folder: grants on the folder or any folder above.
// It needs WithHierarchy.
func (p *Plugin) CanFolder(ctx context.Context, s *records.Store, scope string, subjects []string, folderID string, level Level) (bool, error) {
	if err := p.checkLevel(level); err != nil {
		return false, err
	}
	if p.hierarchy == nil {
		return false, errNoHierarchy()
	}
	chain, err := p.hierarchy.Ancestors(ctx, s, scope, folderID)
	if err != nil {
		return false, err
	}
	depth := depths{}
	depth.folders(chain, 0)
	return p.holds(ctx, s, scope, subjects, []sqlb.Predicate{sqlb.And(sqlb.Eq(Kind, OnFolder), sqlb.In(Resource, chain...))}, depth, level)
}

// resourceRef names one resource a grant can be on.
type resourceRef struct {
	kind ResourceKind
	id   string
}

// depths is Match.Depth of every resource on a check's path.
type depths map[resourceRef]int

// folders records chain, nearest first, at depths from first up. A folder
// already recorded keeps its depth, the nearer one.
func (d depths) folders(chain []string, first int) {
	for i, id := range chain {
		if _, ok := d[resourceRef{OnFolder, id}]; !ok {
			d[resourceRef{OnFolder, id}] = first + i
		}
	}
}

// holds reads every unexpired grant of one of subjects on one of the
// resources (the request's whole path) and lets the Precedence decide:
// DenyOverrides by default, so a deny on any of them wins over an allow
// on any other. No subjects is no access, without asking.
func (p *Plugin) holds(ctx context.Context, s *records.Store, scope string, subjects []string, resources []sqlb.Predicate, depth depths, level Level) (bool, error) {
	if len(subjects) == 0 {
		return false, nil
	}
	var g Grant
	var matches []Match
	now := p.clock(s)
	q := sqlb.NewSelect(s.Dialect()).Columns(bindings(&g)...).From(Table).
		Where(sqlb.Eq(Scope, scope), sqlb.In(Subject, subjects...), sqlb.Or(resources...),
			sqlb.Or(sqlb.IsNull(ExpiresAt), sqlb.Greater(ExpiresAt, &now)))
	err := s.Each(ctx, q, func() error {
		m := Match{Grant: g, Depth: depth[resourceRef{g.Kind, g.Resource}]}
		m.Grant.CreatedAt = m.Grant.CreatedAt.UTC()
		if m.Grant.ExpiresAt != nil {
			at := m.Grant.ExpiresAt.UTC()
			m.Grant.ExpiresAt = &at
		}
		if m.Denies() {
			// Deny, or an effect this code does not know: failing closed,
			// an unknown effect refuses as a deny does.
			m.Applies = p.refuses(g.Level, level)
		} else {
			m.Applies = p.grants(g.Level, level)
		}
		matches = append(matches, m)
		return nil
	})
	if err != nil {
		return false, err
	}
	// Nearest first, then by ID: a precedence sees the same order on every
	// engine.
	slices.SortFunc(matches, func(a, b Match) int {
		if c := cmpDepth(a.Depth, b.Depth); c != 0 {
			return c
		}
		return strings.Compare(a.Grant.ID, b.Grant.ID)
	})
	ok, err := p.precedence(ctx, level, matches)
	if err != nil {
		return false, err // an error never grants, whatever came with it
	}
	return ok, nil
}

// cmpDepth orders depths nearest first, ContainerDepth after every folder.
func cmpDepth(a, b int) int {
	if a == ContainerDepth {
		a = math.MaxInt
	}
	if b == ContainerDepth {
		b = math.MaxInt
	}
	return cmp.Compare(a, b)
}

// refuses reports whether a deny of held refuses a request for want: it
// does when a grant of want would give held, so denying Write refuses
// Write and Admin and leaves Read. A deny of a level the plugin no longer
// defines refuses everything, failing closed.
func (p *Plugin) refuses(held, want Level) bool {
	if !p.defined(held) {
		return true
	}
	return p.grants(want, held)
}

// checkResource refuses a grant on what does not exist.
func (p *Plugin) checkResource(ctx context.Context, s *records.Store, scope string, kind ResourceKind, id string) error {
	switch kind {
	case OnFile:
		f, err := s.File(ctx, scope, id)
		if err == nil && f.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeNotFound, "acl: file %q is in the trash", id)
		}
		return err
	case OnFolder:
		if p.hierarchy == nil {
			return errNoHierarchy()
		}
		if id == "" {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: grants on the root are grants on the scope; give them in the application")
		}
		_, err := p.hierarchy.Ancestors(ctx, s, scope, id)
		return err
	}
	if c, ok := p.containers[kind]; ok {
		if id == "" {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: a grant on a %s needs its ID", kind)
		}
		return c.Check(ctx, s, scope, id)
	}
	return filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: unknown resource kind %q", kind)
}

func errNoHierarchy() error {
	return filesystem.Errorf(filesystem.CodeInvalidArgument, "acl: grants on folders need WithHierarchy")
}

func (p *Plugin) at(scope string, kind ResourceKind, resource, subject string) []sqlb.Predicate {
	return []sqlb.Predicate{sqlb.Eq(Scope, scope), sqlb.Eq(Kind, kind), sqlb.Eq(Resource, resource), sqlb.Eq(Subject, subject)}
}

func (p *Plugin) emit(ctx context.Context, s *records.Store, kind records.EventKind, g Grant) error {
	e := records.Event{Kind: kind, Scope: g.Scope, Subject: g.Subject, Detail: map[string]string{
		DetailKind: string(g.Kind), DetailResource: g.Resource, DetailLevel: string(g.Level),
	}}
	if g.Effect == Deny {
		e.Detail[DetailEffect] = string(Deny)
	}
	if g.ExpiresAt != nil {
		e.Detail[DetailExpiresAt] = g.ExpiresAt.Format(time.RFC3339Nano)
	}
	if g.Kind == OnFile {
		e.FileID = g.Resource
	}
	return s.Emit(ctx, p, e)
}

// Prune deletes the grants that expired before cutoff, in every scope, and
// returns how many it deleted. An expired grant already gives nothing;
// keeping it until cutoff lets Grants still show it. Schedule it (the
// library starts no background work). It emits no event.
func (p *Plugin) Prune(ctx context.Context, s *records.Store, cutoff time.Time) (int64, error) {
	at := cutoff.UTC()
	return s.Exec(ctx, sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.IsNotNull(ExpiresAt), sqlb.Less(ExpiresAt, &at)))
}
