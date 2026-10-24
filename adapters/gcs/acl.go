package gcs

import (
	"context"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
)

// ACLGrant gives one entity one role on an object. Entity is GCS's
// grantee string: storage.AllUsers, storage.AllAuthenticatedUsers,
// "user-<email>", "group-<email>", "domain-<domain>" or
// "project-<team>-<number>". Role is storage.RoleReader or
// storage.RoleOwner (an object has no writer role). The Disk's Visibility
// covers public and private; this is for the rest. A bucket with uniform
// bucket-level access (the GCS default for new buckets) has object ACLs
// disabled and refuses them.
type ACLGrant struct {
	Entity storage.ACLEntity
	Role   storage.ACLRole
}

// ObjectACL is the list SetObjectACL replaces an object's ACL with.
type ObjectACL struct {
	Grants []ACLGrant
	// AllowPublic permits a grant to storage.AllUsers or
	// storage.AllAuthenticatedUsers, which opens the object to anyone (with
	// a Google account). Off, such a grant is refused, so making an object
	// public is always asked for by name.
	AllowPublic bool
}

// SetObjectACL replaces the ACL of the object at key with acl.Grants. key is
// the name as stored in the bucket: for a disk's key, pass
// disk.DriverKey(key), which cleans it and adds a scoped disk's prefix. A
// key that is not already clean, no grants, an empty entity, a role other
// than reader or owner, or a public grant without AllowPublic is refused
// before any request. Get the
// client from a disk with filesystem.As[*storage.Client].
func SetObjectACL(ctx context.Context, client *storage.Client, bucket, key string, acl ObjectACL) error {
	if err := checkACLKey(key); err != nil {
		return err
	}
	grants := acl.Grants
	if len(grants) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: SetObjectACL needs at least one grant")
	}
	rules := make([]storage.ACLRule, 0, len(grants))
	for _, g := range grants {
		if g.Entity == "" || (g.Role != storage.RoleReader && g.Role != storage.RoleOwner) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: object grant %q %q needs an entity and the READER or OWNER role", g.Entity, g.Role)
		}
		if (g.Entity == storage.AllUsers || g.Entity == storage.AllAuthenticatedUsers) && !acl.AllowPublic {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: a grant to %s makes the object public; set AllowPublic to mean it", g.Entity)
		}
		rules = append(rules, storage.ACLRule{Entity: g.Entity, Role: g.Role})
	}
	_, err := client.Bucket(bucket).Object(key).Update(ctx, storage.ObjectAttrsToUpdate{ACL: rules})
	return mapErr(err)
}

// ObjectGrants reads the object's ACL back. key is as for SetObjectACL.
// Every entry is returned as stored; an entry SetObjectACL would refuse
// (another role, a public grant) is still reported, so nothing is hidden.
func ObjectGrants(ctx context.Context, client *storage.Client, bucket, key string) ([]ACLGrant, error) {
	if err := checkACLKey(key); err != nil {
		return nil, err
	}
	attrs, err := client.Bucket(bucket).Object(key).Attrs(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	grants := make([]ACLGrant, 0, len(attrs.ACL))
	for _, r := range attrs.ACL {
		grants = append(grants, ACLGrant{Entity: r.Entity, Role: r.Role})
	}
	return grants, nil
}

// checkACLKey refuses a key that cleaning would change, so an adapter
// function never sends a traversal or an unnormalised name to GCS.
func checkACLKey(key string) error {
	if clean, err := filesystem.CleanKey(key); err != nil || clean != key {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "gcs: object ACL key %q is not a clean key", key)
	}
	return nil
}

// PredefinedACL is a GCS predefined object ACL that keeps an object
// private to the project or the bucket's owner, for Config.PrivateACL.
// The values are the JSON API's names; a presigned upload signs the XML
// API's equivalent (see xml). Public ACLs are not among them: public
// objects always get publicRead, and opening an object further is
// SetObjectACL's job, behind its AllowPublic.
type PredefinedACL string

const (
	// ACLPrivate: the object's owner alone. The default.
	ACLPrivate PredefinedACL = "private"
	// ACLProjectPrivate: the owner, plus the project's owners, editors and
	// viewers by role.
	ACLProjectPrivate PredefinedACL = "projectPrivate"
	// ACLBucketOwnerRead: the owner, plus read for the bucket's owner.
	ACLBucketOwnerRead PredefinedACL = "bucketOwnerRead"
	// ACLBucketOwnerFullControl: the owner, plus full control for the
	// bucket's owner. Set it when this disk writes into a bucket another
	// project owns, so the owner can manage what it receives.
	ACLBucketOwnerFullControl PredefinedACL = "bucketOwnerFullControl"
)

// PrivateACLValues lists every PredefinedACL Config.PrivateACL accepts.
var PrivateACLValues = []PredefinedACL{ACLPrivate, ACLProjectPrivate, ACLBucketOwnerRead, ACLBucketOwnerFullControl}

// xml is the XML API canned ACL a presigned PUT signs as x-goog-acl, and a
// form pins as its acl field, for a; the two APIs spell the same ACLs
// differently. Only values of PrivateACLValues reach it (New refuses the
// rest).
func (a PredefinedACL) xml() string {
	switch a {
	case ACLProjectPrivate:
		return "project-private"
	case ACLBucketOwnerRead:
		return "bucket-owner-read"
	case ACLBucketOwnerFullControl:
		return "bucket-owner-full-control"
	}
	return "private"
}

// privateACL is the predefined ACL private objects get: Config.PrivateACL,
// or ACLPrivate.
func (d *Driver) privateACL() PredefinedACL {
	if d.cfg.PrivateACL != "" {
		return d.cfg.PrivateACL
	}
	return ACLPrivate
}
