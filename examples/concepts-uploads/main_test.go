package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// TestRun checks every upload path's guarantee the uploads page states.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`receive: key uploads/<random>.png name "Holiday Photo.PNG" type image/png size 29`,
		"receive html named photo.png: rejected",
		"receive over MaxSize: too_large, objects stored: 1",
		"form partial=false: 415, stored 0, recorded 0",
		`body {"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}`,
		"form partial=true: 200, stored 2, recorded 2",
		`"failed":[{"filename":"page.html","error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}]`,
		`direct: PUT 200, confirm "" type "image/png", object kept: true`,
		`direct: PUT 200, confirm "rejected" type "", object kept: false`,
		"presign declared text/html: rejected",
		`uppy on memory: 501 {"error":"Not Implemented","code":"unsupported"}`,
		"from loopback: filesystem: invalid_argument: address is not on the public internet",
		`from allowed server: "scan.pdf" application/pdf 24 bytes`,
		"PublicAddr(169.254.169.254): false",
		"file URL: invalid_argument",
		"processed: 5 bytes",
		"inspected: rejected, objects stored: 1",
		"swept 1, left 1",
		"sweep the root: invalid_argument",
		"sharded: 2 directory levels under photos/",
		"collision rename: drive/rename/photo.png, drive/rename/photo (1).png",
		"collision fail: drive/fail/photo.png, exists",
		"base64: image/png, 29 bytes",
		"data URI declared text/plain: stored as image/png",
		"bad base64: invalid_argument",
		"empty file: rejected",
		"two files over MaxTotal 60: 413",
		"body of 8388608 bytes: 413 after reading under 131072",
		`plan free: 413 retry-after=""`,
		`plan paid: 201 retry-after=""`,
		`plan spammer: 503 retry-after="60"`,
		`declared sha256, sent 29 bytes: confirm "", kept true`,
		`declared sha256, sent 29 bytes: confirm "rejected", kept false`,
		"ticket for 24h0m0s: <nil>",
		"ticket past MaxPresignExpiry: invalid_argument",
		"promoted to avatars/u1.png, still in cache: false",
		"promote a key outside the cache: permission",
		"would remove: multipart upload of videos/big.mp4",
		"swept 1 abandoned upload(s)",
		`key from the request, tenant "acme": 201 tenants/acme/<random>.png`,
		`key from the request, tenant "globex": 201 tenants/globex/<random>.png`,
		`key from the request, tenant "": 403 (objects stored in all: 2)`,
		`through the proxy: "q3.pdf" application/pdf; proxy asked for addresses, never the name: true; User-Agent sent: true`,
		`metadata address through the proxy: invalid_argument, proxy asked: false`,
		"envelope respond=false: upload 201 files[1], records 201 files[1], share listing 200 [1]",
		"public ticket: signs x-amz-acl true, client sends X-Amz-Acl: public-read",
		"public ticket without AllowPublic: permission",
		"ticket asking for nothing: signs x-amz-acl false",
		"public ticket on a bucket with ACLs off: unsupported",
		"envelope respond=true: upload 201 data.files[1], records 201 data.files[1], share listing 200 data[1]",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestAllowPublicIsWhatAllows pins that the policy switch alone decides
// whether a client may ask for a public object: on the same ACL bucket
// the same request is signed with AllowPublic and refused with
// permission, before anything is signed, without it.
func TestAllowPublicIsWhatAllows(t *testing.T) {
	disk, err := newACLBucket(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{true, false} {
		p := avatars
		p.AllowPublic = allow
		ticket, err := publicTicket(t.Context(), disk, p)
		switch {
		case allow && (err != nil || !signsACL(ticket)):
			t.Errorf("AllowPublic: ticket %+v, err %v; want a ticket signing x-amz-acl", ticket, err)
		case !allow && filesystem.CodeOf(err) != filesystem.CodePermission:
			t.Errorf("without AllowPublic: err %v, want permission", err)
		}
	}
}
