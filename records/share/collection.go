package share

import (
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// Target is what a link shares: one file (Kind KindFile, Ref ""), or a
// collection of files, named by a Collection's Kind and a reference only
// that Collection reads (a folder's ID, an owner's role).
type Target struct {
	Kind string
	Ref  string
}

// Collection is a set of files a link can share as a whole: the folders
// plugin's folders, an owner's attachments under a role (Owners), or an
// application's own (an album, a tag). A collection link lists and serves
// the collection's live files at the time of each request, so files added
// later are included and trashed ones are not. Install collections with
// WithCollections.
type Collection interface {
	// Kind names the collection type in links ("folder", "owner"); it
	// must not be KindFile.
	Kind() string
	// Files returns up to limit live files of the collection ref of
	// scope, CodeNotFound when ref names nothing.
	Files(ctx context.Context, s *records.Store, scope, ref string, limit int) ([]records.File, error)
	// Contains reports whether the live file fileID is in the collection.
	Contains(ctx context.Context, s *records.Store, scope, ref, fileID string) (bool, error)
}

// Receiver is a Collection files can be added to, for request links
// (CreateRequest): Accept places a newly uploaded file in the collection,
// inside the upload's transaction.
type Receiver interface {
	Collection
	Accept(ctx context.Context, s *records.Store, scope, ref string, f records.File) error
}

// WithCollections installs the collections links may share. Without any,
// only single-file links exist.
func WithCollections(collections ...Collection) Option {
	return func(p *Plugin) {
		for _, c := range collections {
			p.collections[c.Kind()] = c
		}
	}
}

// MaxListedFiles caps how many files a collection link lists at once.
const MaxListedFiles = 1000

// collection returns the installed collection of kind, or
// CodeInvalidArgument.
func (p *Plugin) collection(kind string) (Collection, error) {
	c, ok := p.collections[kind]
	if !ok || kind == KindFile {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "share: no collection of kind %q is installed (WithCollections)", kind)
	}
	return c, nil
}

// CreateFor makes a link sharing a collection of scope: its holder lists
// the collection's live files (List) and downloads any of them
// (ResolveFile), each download counting toward MaxDownloads. The target
// must exist (CodeNotFound otherwise).
func (p *Plugin) CreateFor(ctx context.Context, s *records.Store, scope string, target Target, o Options) (Share, error) {
	c, err := p.collection(target.Kind)
	if err != nil {
		return Share{}, err
	}
	if _, err := c.Files(ctx, s, scope, target.Ref, 1); err != nil {
		return Share{}, err
	}
	return p.create(ctx, s, scope, "", target, ModeDownload, o)
}

// List returns the live files of a collection link's target, up to
// MaxListedFiles, newest first as the collection orders them, and the
// link; quarantined files are left out. It spends nothing; it fails as Resolve does.
func (p *Plugin) List(ctx context.Context, s *records.Store, token string, opts ...AccessOption) ([]records.File, Share, error) {
	sh, c, err := p.collectionLink(ctx, s, token, ModeDownload, opts)
	if err != nil {
		return nil, Share{}, err
	}
	files, err := c.Files(ctx, s, sh.Scope, sh.Target.Ref, MaxListedFiles)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil, Share{}, errGone()
	}
	// A quarantined file is not listed: its visitors could not download
	// it, and its name is not theirs to see before the scan passes.
	return slices.DeleteFunc(files, records.File.Quarantined), sh, err
}

// ResolveFile spends one download of a collection link and returns its
// file fileID, if the file is live and in the collection now, counting
// its view as Resolve does. A file outside it, expired, or view-once and
// already seen is CodeNotFound with the link's uniform message, and spends
// nothing.
func (p *Plugin) ResolveFile(ctx context.Context, s *records.Store, token, fileID string, opts ...AccessOption) (records.File, error) {
	sh, c, err := p.collectionLink(ctx, s, token, ModeDownload, opts)
	if err != nil {
		return records.File{}, err
	}
	in, err := c.Contains(ctx, s, sh.Scope, sh.Target.Ref, fileID)
	if err != nil && filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return records.File{}, err
	}
	if !in {
		return records.File{}, errGone()
	}
	f, err := p.liveFile(ctx, s, sh.Scope, fileID)
	if err != nil {
		return records.File{}, err
	}
	if err := p.take(ctx, s, sh, f); err != nil {
		return records.File{}, err
	}
	return f, nil
}

// CheckFile is ResolveFile without spending a download: it reports
// whether fileID of a collection link is usable now, for a HEAD.
func (p *Plugin) CheckFile(ctx context.Context, s *records.Store, token, fileID string, opts ...AccessOption) (records.File, error) {
	sh, c, err := p.collectionLink(ctx, s, token, ModeDownload, opts)
	if err != nil {
		return records.File{}, err
	}
	if in, err := c.Contains(ctx, s, sh.Scope, sh.Target.Ref, fileID); err != nil || !in {
		if err != nil && filesystem.CodeOf(err) != filesystem.CodeNotFound {
			return records.File{}, err
		}
		return records.File{}, errGone()
	}
	return p.liveFile(ctx, s, sh.Scope, fileID)
}

// collectionLink is usable for a link to an installed collection.
func (p *Plugin) collectionLink(ctx context.Context, s *records.Store, token, mode string, opts []AccessOption) (Share, Collection, error) {
	sh, err := p.usable(ctx, s, token, mode, opts)
	if err != nil {
		return Share{}, nil, err
	}
	c, ok := p.collections[sh.Target.Kind]
	if !ok || sh.Target.Kind == KindFile {
		return Share{}, nil, errGone()
	}
	return sh, c, nil
}

// Collections lists the collection and request links of scope to target,
// newest first, without tokens.
func (p *Plugin) Collections(ctx context.Context, s *records.Store, scope string, target Target) ([]Share, error) {
	return p.list(ctx, s, sqlb.Eq(Scope, scope), sqlb.Eq(TargetKind, target.Kind), sqlb.Eq(TargetRef, target.Ref))
}

// KindOwner is the Kind of Owners.
const KindOwner = "owner"

// Owners is the Collection of an owner's files under a role (a post's
// gallery, a ticket's attachments), in their position order. Its Ref is
// OwnerRef's. It is a Receiver: a request link attaches each upload to
// the owner under the role.
type Owners struct{}

// OwnerRef is the Target.Ref of owner's files under role, for Owners.
func OwnerRef(owner records.Owner, role string) string {
	return url.PathEscape(owner.Type) + ownerRefSep + url.PathEscape(owner.ID) + ownerRefSep + url.PathEscape(role)
}

// ownerRefSep joins the path-escaped owner type, owner ID and role of an
// OwnerRef; escaping keeps it out of the parts, so a ref always splits
// into exactly ownerRefParts.
const (
	ownerRefSep   = "/"
	ownerRefParts = 3
)

func parseOwnerRef(ref string) (records.Owner, string, error) {
	parts := strings.Split(ref, ownerRefSep)
	if len(parts) == ownerRefParts {
		t, err1 := url.PathUnescape(parts[0])
		id, err2 := url.PathUnescape(parts[1])
		role, err3 := url.PathUnescape(parts[2])
		if err1 == nil && err2 == nil && err3 == nil {
			return records.Owner{Type: t, ID: id}, role, nil
		}
	}
	return records.Owner{}, "", filesystem.Errorf(filesystem.CodeInvalidArgument, "share: %q is not an OwnerRef", ref)
}

// Kind implements Collection.
func (Owners) Kind() string { return KindOwner }

// Files implements Collection: the owner's live files under the role, in
// position order. An owner with none is an empty collection, not an
// unknown one: the library does not know the application's rows.
func (Owners) Files(ctx context.Context, s *records.Store, scope, ref string, limit int) ([]records.File, error) {
	owner, role, err := parseOwnerRef(ref)
	if err != nil {
		return nil, err
	}
	page, err := s.AttachmentsPage(ctx, records.AttachmentQuery{Scope: scope, Owner: owner, Role: role, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]records.File, 0, len(page.Files))
	for _, af := range page.Files {
		out = append(out, af.File)
	}
	return out, nil
}

// Contains implements Collection.
func (Owners) Contains(ctx context.Context, s *records.Store, scope, ref, fileID string) (bool, error) {
	owner, role, err := parseOwnerRef(ref)
	if err != nil {
		return false, err
	}
	attached, err := s.AttachedTo(scope, owner, role)
	if err != nil {
		return false, err
	}
	page, err := s.Files(ctx, records.FileQuery{Scope: scope, Limit: 1, Filters: []sqlb.Predicate{attached, sqlb.Eq(records.FileID, fileID)}})
	return len(page.Files) == 1, err
}

// Accept implements Receiver: it attaches f to the owner under the role.
func (Owners) Accept(ctx context.Context, s *records.Store, scope, ref string, f records.File) error {
	owner, role, err := parseOwnerRef(ref)
	if err != nil {
		return err
	}
	_, err = s.Attach(ctx, scope, f.ID, owner, role)
	return err
}
