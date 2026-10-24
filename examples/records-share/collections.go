package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/sqlb"
)

// linkPassword protects the password demo's link; a real user picks it.
const linkPassword = "correct horse battery staple"

// newFolderStore installs the folders plugin and the share plugin with
// folders as a collection, so a link can share, or receive files into, a
// whole folder.
// ds:def id=newfolderstore-v8m2k5wc
func newFolderStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *folders.Plugin, *share.Plugin, error) {
	fp := folders.New()
	sp := share.New(share.WithCollections(fp.Collection(), share.Owners{}))
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(fp, sp))
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, nil, err
	}
	return s, fp, sp, nil
}

// protect makes a link that also needs a password. Only a PBKDF2 hash of
// the password is stored.
// ds:def id=protect-c3w7t9nk
func protect(ctx context.Context, s *records.Store, p *share.Plugin, fileID string) (share.Share, error) {
	return p.Create(ctx, s, tenant, fileID, share.Options{TTL: linkTTL, Password: linkPassword})
}

// openProtected resolves a password link: without share.WithPassword, or
// with a wrong one, it is share.ErrPassword and spends nothing.
// ds:def id=openprotected-r9n4x2hv
func openProtected(ctx context.Context, s *records.Store, p *share.Plugin, token, password string) (records.File, error) {
	return p.Resolve(ctx, s, token, share.WithPassword(password))
}

// shareFolder makes a link to a folder's own files (not its subfolders'),
// read at each request, so files added later are included.
// ds:def id=sharefolder-p5k8w3tm
func shareFolder(ctx context.Context, s *records.Store, p *share.Plugin, folderID string) (share.Share, error) {
	return p.CreateFor(ctx, s, tenant, share.Target{Kind: folders.KindFolder, Ref: folderID}, share.Options{TTL: linkTTL})
}

// requestFiles makes an upload-only link into a folder: its holder can
// add files, at most maxFiles, and see nothing.
// ds:def id=requestfiles-h6c2v8qn
func requestFiles(ctx context.Context, s *records.Store, p *share.Plugin, folderID string, maxFiles int64) (share.Share, error) {
	return p.CreateRequest(ctx, s, tenant, share.Target{Kind: folders.KindFolder, Ref: folderID}, share.Options{TTL: linkTTL, MaxDownloads: maxFiles})
}

// deposit is what the request link's handler does with one upload: store
// it under the app's policy, record it in the link's scope, put it in the
// folder and count one use, in one transaction.
// ds:def id=deposit-t2q7m4wk
func deposit(ctx context.Context, s *records.Store, p *share.Plugin, token, name, body string) (records.File, error) {
	return p.Deposit(ctx, s, token, diskName, strings.NewReader(body), name, docPolicy)
}

// collectionsDemo runs passwords, folder links, file requests and
// statistics on a second Store over the same database.
func collectionsDemo(ctx context.Context, w io.Writer, db *sql.DB, disks *filesystem.Manager) error {
	s, fp, sp, err := newFolderStore(ctx, db, disks)
	if err != nil {
		return err
	}
	contract, err := uploadDoc(ctx, s, "contract.txt", "terms")
	if err != nil {
		return err
	}
	locked, err := protect(ctx, s, sp, contract.ID)
	if err != nil {
		return err
	}
	_, err = sp.Resolve(ctx, s, locked.Token)
	fmt.Fprintf(w, "password link without a password: %s (%t)\n", filesystem.CodeOf(err), errors.Is(err, share.ErrPassword))
	_, err = openProtected(ctx, s, sp, locked.Token, "guess")
	fmt.Fprintf(w, "with a wrong one: %s\n", filesystem.CodeOf(err))
	f, err := openProtected(ctx, s, sp, locked.Token, linkPassword)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "with the right one: %s\n", f.Filename)

	trip, err := fp.Create(ctx, s, tenant, "", "Trip")
	if err != nil {
		return err
	}
	beach, err := uploadDoc(ctx, s, "beach.txt", "sand")
	if err != nil {
		return err
	}
	if err := fp.Put(ctx, s, tenant, beach.ID, trip.ID); err != nil {
		return err
	}
	link, err := shareFolder(ctx, s, sp, trip.ID)
	if err != nil {
		return err
	}
	listed, _, err := sp.List(ctx, s, link.Token)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "folder link lists %d file(s)\n", len(listed))
	inbox, err := fp.Create(ctx, s, tenant, "", "Inbox")
	if err != nil {
		return err
	}
	req, err := requestFiles(ctx, s, sp, inbox.ID, 1)
	if err != nil {
		return err
	}
	got, err := deposit(ctx, s, sp, req.Token, "receipt.txt", "paid")
	if err != nil {
		return err
	}
	where, err := fp.FolderOf(ctx, s, tenant, got.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "request link: %s by %s… into Inbox %t\n", got.Filename, got.UploadedBy[:len("share:")], where == inbox.ID)
	_, err = deposit(ctx, s, sp, req.Token, "second.txt", "again")
	fmt.Fprintf(w, "second upload past its limit: %s\n", filesystem.CodeOf(err))
	_, _, err = sp.List(ctx, s, req.Token)
	fmt.Fprintf(w, "listing through the request link: %s\n", filesystem.CodeOf(err))
	if _, err := sp.Resolve(records.WithActor(ctx, "user:carol"), s, link.Token); err == nil {
		return errors.New("a folder link resolved as a file link")
	}
	if _, err := sp.ResolveFile(records.WithActor(ctx, "user:carol"), s, link.Token, beach.ID); err != nil {
		return err
	}
	st, err := sp.Stats(ctx, s, tenant, link.ID, 10)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "folder link stats: %d use(s), last by %q\n", st.Uses, st.Recent[0].Viewer)
	return nil
}
