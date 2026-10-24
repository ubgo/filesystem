package share

import (
	"context"
	"errors"
	"io"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// CreateRequest makes a file request link: its holder uploads files into
// the target collection of scope (a folder, an owner's role) and can see
// nothing in it. MaxDownloads caps the uploads. The target's Collection
// must be a Receiver.
func (p *Plugin) CreateRequest(ctx context.Context, s *records.Store, scope string, target Target, o Options) (Share, error) {
	c, err := p.collection(target.Kind)
	if err != nil {
		return Share{}, err
	}
	if _, ok := c.(Receiver); !ok {
		return Share{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "share: collection %q cannot receive files", target.Kind)
	}
	if _, err := c.Files(ctx, s, scope, target.Ref, 1); err != nil {
		return Share{}, err
	}
	return p.create(ctx, s, scope, "", target, ModeRequest, o)
}

// Deposit uploads body through a request link with token: it stores and
// records the file in the link's scope on the named disk under policy
// (records.Store.Upload, so quota and every FileHooks plugin apply to the
// owning scope), and in the transaction that records the file it places
// it in the target (Receiver.Accept) and counts one use. The body is
// received before that transaction begins, so a slow client holds no
// database connection; anything failing rolls the row back and deletes
// the stored object. The file's UploadedBy is the actor in ctx, else
// "share:" and the link's ID, so uploads through a link are told apart.
// With WithQuarantinedDeposits the file is recorded quarantined.
// It fails as Resolve does for an unusable link, before reading body.
func (p *Plugin) Deposit(ctx context.Context, s *records.Store, token, disk string, body io.Reader, filename string, policy upload.Policy, opts ...AccessOption) (records.File, error) {
	sh, c, err := p.collectionLink(ctx, s, token, ModeRequest, opts)
	if err != nil {
		return records.File{}, err
	}
	recv, ok := c.(Receiver)
	if !ok {
		return records.File{}, errGone()
	}
	if s.Actor(ctx) == "" {
		ctx = records.WithActor(ctx, uploaderPrefix+sh.ID)
	}
	pending := &pendingDeposit{link: sh, recv: recv}
	var uploadOpts []records.UploadOption
	if p.quarantineDeposits {
		uploadOpts = append(uploadOpts, records.Quarantine())
	}
	f, err := s.Upload(context.WithValue(ctx, depositKey{}, pending), sh.Scope, disk, body, filename, policy, uploadOpts...)
	if err != nil || pending.placed {
		return f, err
	}
	// Only p's AfterCreate places the file: p is not installed in s.
	return records.File{}, errors.Join(errNotInstalled, s.Purge(ctx, f.Scope, f.ID))
}

// errNotInstalled is Deposit's error when its Plugin is not installed in
// the Store, so nothing placed the uploaded file or counted the use.
var errNotInstalled = filesystem.Errorf(filesystem.CodeInvalidArgument, "share: Deposit needs the plugin installed in the Store (records.WithPlugins)")

// pendingDeposit is a Deposit under way, carried in the context of its
// Store.Upload: the plugin's AfterCreate places the first file that
// upload records (placed) and spends link, inside the upload's own
// transaction.
type pendingDeposit struct {
	link   Share
	recv   Receiver
	placed bool
}

// depositKey is the context key of a pendingDeposit.
type depositKey struct{}

// uploaderPrefix starts the UploadedBy of a file deposited through a
// request link without an actor.
const uploaderPrefix = "share:"
