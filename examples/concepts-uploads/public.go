package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/upload"
)

// The bucket of the public-upload step. Presigning is local computation,
// so the endpoint is never contacted and the keys are placeholders.
const (
	avatarBucket   = "avatars"
	avatarRegion   = "us-east-1"
	avatarEndpoint = "https://s3.example.test"
	// avatarSize is the size the client declares for its avatar.
	avatarSize = 48213
)

// avatars is a policy whose tickets may ask for a public object: an
// avatar is read by URL, so its upload signs public-read and nothing has
// to call SetVisibility afterwards. Only the server can set AllowPublic.
// ds:def id=avatarpolicy-v3k8m2qx
var avatars = upload.Policy{
	MaxSize:     2 << 20,
	Allowed:     []string{"image/png", "image/jpeg", "image/webp"},
	Key:         upload.RandomKey("avatars"),
	AllowPublic: true,
}

// newACLBucket opens an S3 disk in ACL mode, which has the
// PresignVisibility capability; acl false is a bucket with ACLs off
// (bucket owner enforced), which cannot pin a visibility.
func newACLBucket(acl bool) (*filesystem.Disk, error) {
	client := s3.New(s3.Options{
		Region: avatarRegion, BaseEndpoint: aws.String(avatarEndpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "example-secret", ""),
	})
	mode := fss3.VisibilityNone
	if acl {
		mode = fss3.VisibilityACL
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: avatarBucket, Visibility: mode})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(avatarBucket, drv)
}

// publicTicket presigns an avatar upload that lands public: the client
// asks with TicketRequest.Visibility, the policy allows it, and the PUT
// signs x-amz-acl, which the client sends from Ticket.Headers.
// ds:def id=publicticket-n6w9t4hb
func publicTicket(ctx context.Context, disk *filesystem.Disk, p upload.Policy) (upload.Ticket, error) {
	req := upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: avatarSize, Visibility: filesystem.VisibilityPublic}
	return upload.Presign(ctx, disk, req, p, ticketLifetime)
}

// signsACL reports whether a ticket's URL signs the x-amz-acl header.
func signsACL(t upload.Ticket) bool {
	u, err := url.Parse(t.URL)
	return err == nil && strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "x-amz-acl")
}

// publicUploads presigns a public avatar, then the same ticket under a
// policy without AllowPublic and on a bucket with ACLs off.
func publicUploads(ctx context.Context, out io.Writer) error {
	disk, err := newACLBucket(true)
	if err != nil {
		return err
	}
	t, err := publicTicket(ctx, disk, avatars)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "public ticket: signs x-amz-acl %t, client sends X-Amz-Acl: %s\n", signsACL(t), http.Header(t.Headers).Get("X-Amz-Acl"))
	private := avatars
	private.AllowPublic = false
	_, err = publicTicket(ctx, disk, private)
	fmt.Fprintf(out, "public ticket without AllowPublic: %s\n", filesystem.CodeOf(err))
	t, err = upload.Presign(ctx, disk, upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: avatarSize}, private, ticketLifetime)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ticket asking for nothing: signs x-amz-acl %t\n", signsACL(t))
	off, err := newACLBucket(false)
	if err != nil {
		return err
	}
	_, err = publicTicket(ctx, off, avatars)
	fmt.Fprintf(out, "public ticket on a bucket with ACLs off: %s\n", filesystem.CodeOf(err))
	return nil
}
