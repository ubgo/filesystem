package acl_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/acl"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/upload"
)

var errHookDown = errors.New("event consumer down")

// revokeRefuser is an event hook that fails every EventRevoked, as an
// outbox whose write fails would.
type revokeRefuser struct{}

func (revokeRefuser) Name() string { return "revoke_refuser" }
func (revokeRefuser) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	if e.Kind == acl.EventRevoked {
		return errHookDown
	}
	return nil
}

// TestRevokeEventFailureRollsBack: when the EventRevoked of a revoke
// fails, Revoke returns that failure and the grant is still there, since
// the delete and its event commit together or not at all.
func TestRevokeEventFailureRollsBack(t *testing.T) {
	ctx := t.Context()
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	ap := acl.New()
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(ap, revokeRefuser{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := s.Upload(ctx, "t", "media", strings.NewReader("a"), "a.txt", upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Grant(ctx, s, "t", acl.Grant{Subject: "u", Kind: acl.OnFile, Resource: f.ID, Level: acl.Read}); err != nil {
		t.Fatal(err)
	}
	if err := ap.Revoke(ctx, s, "t", acl.OnFile, f.ID, "u"); !errors.Is(err, errHookDown) {
		t.Fatalf("Revoke with a failing event = %v", err)
	}
	if ok, err := ap.Can(ctx, s, "t", []string{"u"}, f.ID, acl.Read); err != nil || !ok {
		t.Fatalf("after the failed revoke Can = %v, %v; the grant must remain", ok, err)
	}
}

// TestMostSpecificContainers: MostSpecific counts a grant on a container
// as farther than every folder, so a folder grant at any depth decides
// over it, and a container grant decides only when no folder or file
// grant applies.
func TestMostSpecificContainers(t *testing.T) {
	allow := acl.Grant{ID: "a", Effect: acl.Allow}
	deny := acl.Grant{ID: "d", Effect: acl.Deny}
	cases := []struct {
		name    string
		matches []acl.Match
		want    bool
	}{
		{"container allow alone", []acl.Match{{Grant: allow, Depth: acl.ContainerDepth, Applies: true}}, true},
		{"container deny alone", []acl.Match{{Grant: deny, Depth: acl.ContainerDepth, Applies: true}}, false},
		{"deep folder allow over container deny", []acl.Match{
			{Grant: deny, Depth: acl.ContainerDepth, Applies: true}, {Grant: allow, Depth: 40, Applies: true},
		}, true},
		{"folder deny over container allow", []acl.Match{
			{Grant: allow, Depth: acl.ContainerDepth, Applies: true}, {Grant: deny, Depth: 3, Applies: true},
		}, false},
		{"container grants together, deny wins", []acl.Match{
			{Grant: allow, Depth: acl.ContainerDepth, Applies: true}, {Grant: deny, Depth: acl.ContainerDepth, Applies: true},
		}, false},
	}
	for _, c := range cases {
		if got, err := acl.MostSpecific(t.Context(), acl.Read, c.matches); err != nil || got != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}
