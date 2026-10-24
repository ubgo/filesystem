package folders

import (
	"context"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// KindFolder is the collection kind of a folder, for links that share a
// folder: install it with share.WithCollections(plugin.Collection()). Its
// Ref is the folder's ID.
const KindFolder = "folder"

// Collection returns the folders as a collection links can share, and
// receive files into (share.Collection and share.Receiver, satisfied
// without importing share).
func (p *Plugin) Collection() *Collection { return &Collection{p: p} }

// Collection is the folder collection of a Plugin (see Plugin.Collection).
type Collection struct{ p *Plugin }

// Kind names the folder collection.
func (c *Collection) Kind() string { return KindFolder }

// Files returns up to limit live files directly in folder ref of scope,
// newest first. A shared folder covers its own files, not its
// subfolders'; share each subfolder that should be reachable. The root
// cannot be shared (CodeInvalidArgument): it would expose the scope.
func (c *Collection) Files(ctx context.Context, s *records.Store, scope, ref string, limit int) ([]records.File, error) {
	if ref == RootID {
		return nil, errRootShared()
	}
	page, err := c.p.Files(ctx, s, ref, records.FileQuery{Scope: scope, Limit: limit})
	return page.Files, err
}

// Contains reports whether the file fileID of scope is directly in folder
// ref.
func (c *Collection) Contains(ctx context.Context, s *records.Store, scope, ref, fileID string) (bool, error) {
	if ref == RootID {
		return false, errRootShared()
	}
	in, err := c.p.FolderOf(ctx, s, scope, fileID)
	return err == nil && in == ref, err
}

// Accept puts a file uploaded through a request link into folder ref.
func (c *Collection) Accept(ctx context.Context, s *records.Store, scope, ref string, f records.File) error {
	if ref == RootID {
		return errRootShared()
	}
	return c.p.Put(ctx, s, scope, f.ID, ref)
}

func errRootShared() error {
	return filesystem.Errorf(filesystem.CodeInvalidArgument, "folders: the root cannot be shared")
}

// Ancestors returns folder id of scope and every folder above it, nearest
// first ([] for the root): the chain a permission on a folder is
// inherited through (acl.Hierarchy, with FolderOf).
func (p *Plugin) Ancestors(ctx context.Context, s *records.Store, scope, id string) ([]string, error) {
	path, err := p.Path(ctx, s, scope, id)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(path))
	for i, f := range path {
		out[len(path)-1-i] = f.ID
	}
	return out, nil
}
