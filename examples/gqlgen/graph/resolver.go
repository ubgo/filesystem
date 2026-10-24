// Package graph is the example's GraphQL layer: the schema in *.graphqls,
// the code gqlgen generates from it (generated.go), and the resolvers,
// which store every Upload through contrib/gqlgen under the app's policy.
package graph

//go:generate go tool gqlgen generate

import (
	"context"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

// Resolver holds what the resolvers need: the disk plain uploads go to,
// the records store, the name of the disk records are stored on, and the
// one policy every upload goes through.
type Resolver struct {
	Media    *filesystem.Disk
	Store    *records.Store
	DiskName string
	Policy   upload.Policy
	// MaxFiles caps the files of one uploadFiles list.
	MaxFiles int
}

// Decisions of this example, not of the library.
const (
	// Tenant is the one scope every file of the example lives in.
	Tenant = "demo"
	// OwnerUser is the owner type avatars are attached to.
	OwnerUser = "user"
	// RoleAvatar is the role of a user's avatar.
	RoleAvatar = "avatar"
)

// errSignedOut refuses a resolver that needs a user, as permission.
var errSignedOut = filesystem.Errorf(filesystem.CodePermission, "sign in first")

// signedIn returns the signed-in user as an owner, from the actor the
// server's middleware recorded with records.WithActor.
func signedIn(ctx context.Context) (records.Owner, error) {
	user := records.ActorFrom(ctx)
	if user == "" {
		return records.Owner{}, errSignedOut
	}
	return records.Owner{Type: OwnerUser, ID: user}, nil
}
