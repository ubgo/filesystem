package s3

import (
	"context"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
)

// ObjectACL is an object's access control list. Set exactly one of Canned
// and Grants. The Disk's Visibility covers public and private; this is for
// the rest. Buckets with Object Ownership set to "bucket owner enforced"
// (the S3 default since 2023) have ACLs disabled and refuse both.
type ObjectACL struct {
	Canned CannedACL
	Grants []Grant
	// AllowPublic permits an ACL that opens the object beyond the
	// account: a public canned ACL (CannedPublicRead,
	// CannedPublicReadWrite, CannedAuthenticatedRead) or a grant to the
	// AllUsers or AuthenticatedUsers group. Off, such an ACL is refused,
	// so making an object readable by anyone is always asked for by name.
	AllowPublic bool
}

// CannedACL is one of S3's canned object ACLs.
type CannedACL string

// The canned object ACLs S3 accepts.
const (
	CannedPrivate                CannedACL = CannedACL(types.ObjectCannedACLPrivate)
	CannedBucketOwnerRead        CannedACL = CannedACL(types.ObjectCannedACLBucketOwnerRead)
	CannedBucketOwnerFullControl CannedACL = CannedACL(types.ObjectCannedACLBucketOwnerFullControl)
	CannedAWSExecRead            CannedACL = CannedACL(types.ObjectCannedACLAwsExecRead)
	// The public ones, refused unless ObjectACL.AllowPublic is set.
	CannedPublicRead        CannedACL = CannedACL(types.ObjectCannedACLPublicRead)
	CannedPublicReadWrite   CannedACL = CannedACL(types.ObjectCannedACLPublicReadWrite)
	CannedAuthenticatedRead CannedACL = CannedACL(types.ObjectCannedACLAuthenticatedRead)
)

// public reports whether c opens the object beyond the account, and
// whether c is a canned ACL S3 knows at all.
func (c CannedACL) public() (public, known bool) {
	switch c {
	case CannedPrivate, CannedBucketOwnerRead, CannedBucketOwnerFullControl, CannedAWSExecRead:
		return false, true
	case CannedPublicRead, CannedPublicReadWrite, CannedAuthenticatedRead:
		return true, true
	}
	return false, false
}

// The predefined groups whose grants open an object beyond the account.
const (
	GroupAllUsers           = "http://acs.amazonaws.com/groups/global/AllUsers"
	GroupAuthenticatedUsers = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
)

// Grant gives one grantee one permission on an object.
type Grant struct {
	Grantee Grantee
	// Permission is "READ", "READ_ACP", "WRITE_ACP" or "FULL_CONTROL"
	// (GrantRead and the other constants).
	Permission string
}

// The permissions an object grant can carry. WRITE applies to buckets
// only.
const (
	GrantRead        = string(types.PermissionRead)
	GrantReadACP     = string(types.PermissionReadAcp)
	GrantWriteACP    = string(types.PermissionWriteAcp)
	GrantFullControl = string(types.PermissionFullControl)
)

// Grantee names who a Grant is for: exactly one of a canonical user ID,
// a predefined group's URI (such as
// "http://acs.amazonaws.com/groups/global/AuthenticatedUsers") or, in the
// regions that still accept it, an account's email address.
type Grantee struct {
	ID    string
	URI   string
	Email string
}

// header renders g as one entry of an x-amz-grant-* header, or false
// when it names no grantee or more than one.
func (g Grantee) header() (string, bool) {
	var parts []string
	for _, f := range []struct{ name, value string }{{"id", g.ID}, {"uri", g.URI}, {"emailAddress", g.Email}} {
		if f.value != "" {
			parts = append(parts, f.name+"="+strconv.Quote(f.value))
		}
	}
	if len(parts) != 1 {
		return "", false
	}
	return parts[0], true
}

// SetObjectACL replaces the ACL of the object at key (PutObjectAcl). key
// is the key as stored in the bucket: for a disk's key, pass
// disk.DriverKey(key), which cleans it and adds a scoped disk's prefix. A
// key that is not already clean, an ACL setting both or neither of Canned
// and Grants, an unknown canned ACL, a grant naming no grantee or several,
// an unknown permission, or a public ACL without AllowPublic is refused
// before any request. Get the client from a disk
// with filesystem.As[*s3.Client].
func SetObjectACL(ctx context.Context, client *s3.Client, bucket, key string, acl ObjectACL) error {
	if clean, err := filesystem.CleanKey(key); err != nil || clean != key {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: object ACL key %q is not a clean key", key)
	}
	if (acl.Canned == "") == (len(acl.Grants) == 0) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: an object ACL sets exactly one of Canned and Grants")
	}
	if acl.Canned != "" {
		public, known := acl.Canned.public()
		if !known {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: %q is not a canned object ACL", acl.Canned)
		}
		if public && !acl.AllowPublic {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: canned ACL %q makes the object public; set AllowPublic to mean it", acl.Canned)
		}
	}
	in := &s3.PutObjectAclInput{Bucket: aws.String(bucket), Key: aws.String(key), ACL: types.ObjectCannedACL(acl.Canned)}
	byPermission := map[string][]string{}
	for _, g := range acl.Grants {
		h, ok := g.Grantee.header()
		if !ok {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: a grant names exactly one of ID, URI and Email")
		}
		if (g.Grantee.URI == GroupAllUsers || g.Grantee.URI == GroupAuthenticatedUsers) && !acl.AllowPublic {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: a grant to %s makes the object public; set AllowPublic to mean it", g.Grantee.URI)
		}
		byPermission[g.Permission] = append(byPermission[g.Permission], h)
	}
	for perm, grantees := range byPermission {
		joined := aws.String(strings.Join(grantees, ", "))
		switch perm {
		case GrantRead:
			in.GrantRead = joined
		case GrantReadACP:
			in.GrantReadACP = joined
		case GrantWriteACP:
			in.GrantWriteACP = joined
		case GrantFullControl:
			in.GrantFullControl = joined
		default:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: object grant permission %q is not one of READ, READ_ACP, WRITE_ACP, FULL_CONTROL", perm)
		}
	}
	_, err := client.PutObjectAcl(ctx, in)
	return mapErr(err)
}

// ObjectGrants reads the object's ACL back as grants (GetObjectAcl); a
// canned ACL comes back expanded into the grants it stands for, the
// owner's FULL_CONTROL included. key is as for SetObjectACL. A grant whose
// grantee Grantee cannot represent is a CodeUnsupported error, never left
// out.
func ObjectGrants(ctx context.Context, client *s3.Client, bucket, key string) ([]Grant, error) {
	if clean, err := filesystem.CleanKey(key); err != nil || clean != key {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "s3: object ACL key %q is not a clean key", key)
	}
	out, err := client.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, mapErr(err)
	}
	grants := make([]Grant, 0, len(out.Grants))
	for _, g := range out.Grants {
		if g.Grantee == nil {
			return nil, filesystem.Errorf(filesystem.CodeUnsupported, "s3: object %q has a %s grant without a grantee", key, g.Permission)
		}
		grantee := Grantee{ID: aws.ToString(g.Grantee.ID), URI: aws.ToString(g.Grantee.URI), Email: aws.ToString(g.Grantee.EmailAddress)}
		if _, ok := grantee.header(); !ok {
			return nil, filesystem.Errorf(filesystem.CodeUnsupported, "s3: object %q has a %s grant whose grantee is not exactly one of an ID, a URI and an email", key, g.Permission)
		}
		grants = append(grants, Grant{Grantee: grantee, Permission: string(g.Permission)})
	}
	return grants, nil
}
