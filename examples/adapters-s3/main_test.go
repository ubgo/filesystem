package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program against a fresh fake S3 and checks the
// lines the guide quotes.
func TestRun(t *testing.T) {
	srv, fake := startFakeS3()
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", fakeAccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", fakeSecretKey)
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	var out bytes.Buffer
	if err := run(t.Context(), &out, fake); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"capabilities: [copy,delete-prefix,delete-keys,presign,multipart,presign-part,presign-post,metadata,content-type,headers,presign-headers,checksum-sha256]",
		`stat: reports/q3.csv text/csv owner=finance disposition=attachment; filename="q3.csv"`,
		`signed GET: 200 "month,total"`,
		"signed PUT: 200, stored 19 bytes as text/plain",
		"multipart: 1 part(s), videos/clip.bin is 23 bytes",
		"after deletes: 0 keys under tmp/",
		"public write: unsupported",
		"conditional write: unsupported",
		"via DSN: reports/q3.csv is 20 bytes",
		"storage class: STANDARD_IA, then GLACIER_IR",
		`versions: 2, newest latest=true, oldest reads "draft"`,
		"signed GET with a type override: 200 text/plain",
		"checksum sent: x-amz-checksum-sha256=true, payload hash is the digest=true",
		"wrong digest: invalid_argument",
		"stored crc32c: mnG7TA==, presign declared: false",
		"kms: tenants/acme/logo.txt -> aws:kms " + acmeKMSKey,
		"kms: tenants/globex/logo.txt -> aws:kms " + globexKMSKey,
		"kms: shared/terms.txt -> aws:kms " + defaultKMSKey,
		`sse-c: read "for your eyes only" with algorithm AES256, key printed as s3.CustomerKey{[redacted]}, presign declared: false`,
		"lifecycle: tmp,uploads,logs; logs move to GLACIER after 90 days",
		"replication: contracts-dr contracts/ -> arn:aws:s3:::history-eu-west-1, deletes replicated: false",
		"delegated: arn:aws:s3:::media/users/42/*",
		`credentials print as: s3.ScopedCredentials{AccessKeyID: "ASIAROLEEXAMPLE", SecretAccessKey: [REDACTED], SessionToken: [REDACTED], Bucket: "media", Prefix: "users/42"}`,
		"byo bucket: STS saw external ID ext-7f3c9a; PUT signed by ASIAROLEEXAMPLE with a session token: true",
		`role provider prints as: s3.RoleCredentials{RoleARN: "arn:aws:iam::444455556666:role/app-storage", SessionName: "tenant-acme", [REDACTED]}`,
		`tags on write: class=billing,tenant=acme; after SetTags: retain=7y,tenant=acme; the copy keeps: retain=7y,tenant=acme`,
		`legal hold: true; delete: permission; overwrite: permission`,
		`hold lifted, delete: <nil>`,
		`archived read: conflict, ErrArchived: true`,
		`restore asked at tier Bulk: ongoing`,
		`restored: reads "old logs", restored until 2040-12-21T00:00:00Z`,
		`govcloud: objects arn:aws-us-gov:s3:::media/users/42/*; kms [kms:Decrypt kms:GenerateDataKey] on arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab`,
		`china partition for a govcloud role: invalid_argument`,
		`kms alias instead of a key: invalid_argument`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
