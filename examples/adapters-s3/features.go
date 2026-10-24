package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// KMS keys of the encryption steps. A KMS key ID is not a secret (S3
// returns it on every HEAD), so it may live in configuration and a DSN.
const (
	defaultKMSKey = "arn:aws:kms:eu-west-1:111122223333:key/default-key"
	acmeKMSKey    = "arn:aws:kms:eu-west-1:111122223333:key/acme-key"
	globexKMSKey  = "arn:aws:kms:eu-west-1:111122223333:key/globex-key"
)

// Values of the fake STS the role steps use. In production they come from
// STS; the external ID is generated per tenant and stored with it.
const (
	roleKeyID      = "ASIAROLEEXAMPLE"
	customerRole   = "arn:aws:iam::444455556666:role/app-storage"
	tenantExternal = "ext-7f3c9a"
	uploaderRole   = "arn:aws:iam::111122223333:role/uploader"
)

// featureStep is one object-store feature step. Each opens the disk it
// needs on the shared client and reads the fake's request log where the
// fake accepts a feature without acting on it.
type featureStep func(ctx context.Context, out io.Writer, client *s3.Client, fake *fakeS3) error

// runFeatures runs the object-store feature steps in order.
func runFeatures(ctx context.Context, out io.Writer, client *s3.Client, fake *fakeS3) error {
	for _, step := range []featureStep{
		storageClasses, bucketVersions, responseContentType, checksumVerified, storedChecksum,
		kmsKeyPerTenant, customerKey, bucketLifecycle, bucketReplication, delegatePrefix, bringYourOwnBucket,
		objectTags, legalHold, archiveRestore, govCloudDelegation,
	} {
		if err := step(ctx, out, client, fake); err != nil {
			return err
		}
	}
	return nil
}

// newBucketDisk creates bucket on the fake and opens a disk on it with cfg
// (Client and Bucket are filled in).
func newBucketDisk(ctx context.Context, client *s3.Client, bucket string, cfg fss3.Config) (*filesystem.Disk, error) {
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return nil, err
	}
	cfg.Client, cfg.Bucket = client, bucket
	drv, err := fss3.New(cfg)
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(bucket, drv)
}

// storageClasses writes into a class, then moves the object to another
// with SetStorageClass (a copy onto itself), reading the class back with
// Stat each time.
// ds:def id=storageclasses-pfs8dyd7
func storageClasses(ctx context.Context, out io.Writer, client *s3.Client, _ *fakeS3) error {
	disk, err := newBucketDisk(ctx, client, "cold-store", fss3.Config{StorageClasses: true, StorageClass: types.StorageClassStandardIa})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "logs/2026-09.log", []byte("september"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	first, err := disk.Stat(ctx, "logs/2026-09.log")
	if err != nil {
		return err
	}
	if err := disk.SetStorageClass(ctx, "logs/2026-09.log", string(types.StorageClassGlacierIr)); err != nil {
		return err
	}
	now, err := disk.Stat(ctx, "logs/2026-09.log")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "storage class: %s, then %s\n", first.StorageClass, now.StorageClass)
	return nil
}

// bucketVersions turns versioning on, proves it with CheckBucket (which
// the Opener runs for versioning=true), writes a key twice and reads the
// older version back.
// ds:def id=bucketversions-5pccvywq
func bucketVersions(ctx context.Context, out io.Writer, client *s3.Client, _ *fakeS3) error {
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("history")}); err != nil {
		return err
	}
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("history"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		return err
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: "history", Versioning: true})
	if err != nil {
		return err
	}
	if err := drv.CheckBucket(ctx); err != nil {
		return err // Versioning declared on a bucket without it: CodeInvalidArgument
	}
	disk, err := filesystem.NewDisk("history", drv)
	if err != nil {
		return err
	}
	for _, body := range []string{"draft", "final"} {
		if _, err := disk.WriteBytes(ctx, "contract.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	vs, err := disk.Versions(ctx, "contract.txt") // newest first
	if err != nil {
		return err
	}
	rc, err := disk.ReadVersion(ctx, "contract.txt", vs[len(vs)-1].ID)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	old, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "versions: %d, newest latest=%t, oldest reads %q\n", len(vs), vs[0].Latest, old)
	return nil
}

// responseContentType signs a download that overrides the stored content
// type, so a file stored as bytes opens as text in the browser.
// ds:def id=responsecontenttype-j6nkwu93
func responseContentType(ctx context.Context, out io.Writer, client *s3.Client, _ *fakeS3) error {
	disk, err := newBucketDisk(ctx, client, "exports", fss3.Config{})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "export.dat", []byte("id,name\n1,ada\n"), filesystem.WriteOptions{ContentType: "application/octet-stream"}); err != nil {
		return err
	}
	req, err := disk.SignedURL(ctx, "export.dat", filesystem.PresignOptions{
		Method:      filesystem.MethodGet,
		Expires:     linkLifetime,
		ContentType: "text/plain", // signed as response-content-type
	})
	if err != nil {
		return err
	}
	resp, err := httpGet(ctx, req.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET with a type override: %d %s\n", resp.StatusCode, resp.Header.Get("Content-Type"))
	return nil
}

// checksumVerified writes with a known SHA-256 and size, so the driver
// sends the digest for S3 to verify, and shows a wrong digest refused.
// ds:def id=checksumverified-k2gmgkhf
func checksumVerified(ctx context.Context, out io.Writer, client *s3.Client, fake *fakeS3) error {
	disk, err := newBucketDisk(ctx, client, "invoices", fss3.Config{})
	if err != nil {
		return err
	}
	body := []byte("invoice 42: 120.00 EUR\n")
	sum := sha256.Sum256(body)
	if _, err := disk.WriteBytes(ctx, "42.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(sum[:]), SizeHint: int64(len(body))}); err != nil {
		return err
	}
	sent := fake.lastSent(http.MethodPut, "/invoices/42.txt", "PutObject")
	fmt.Fprintf(out, "checksum sent: x-amz-checksum-sha256=%t, payload hash is the digest=%t\n",
		sent.Get("X-Amz-Checksum-Sha256") != "", sent.Get("X-Amz-Content-Sha256") == hex.EncodeToString(sum[:]))
	wrong := sha256.Sum256([]byte("another body"))
	_, err = disk.WriteBytes(ctx, "43.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(wrong[:]), SizeHint: int64(len(body))})
	fmt.Fprintf(out, "wrong digest: %s\n", filesystem.CodeOf(err))
	return nil
}

// storedChecksum has S3 store a CRC32C checksum with every write and reads
// it back from Stat.
// ds:def id=storedchecksum-jzbg22kx
func storedChecksum(ctx context.Context, out io.Writer, client *s3.Client, _ *fakeS3) error {
	disk, err := newBucketDisk(ctx, client, "crc-store", fss3.Config{ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "a.txt")
	if err != nil {
		return err
	}
	stored := e.Metadata[fss3.MetaChecksumPrefix+"crc32c"]
	fmt.Fprintf(out, "stored crc32c: %s, presign declared: %t\n", stored, disk.Capabilities().Presign)
	return nil
}

// kmsKeyPerTenant encrypts each tenant's files with its own KMS key on one
// shared disk, and everything else with the disk's default key.
// ds:def id=kmskeypertenant-kspf4xgd
func kmsKeyPerTenant(ctx context.Context, out io.Writer, client *s3.Client, fake *fakeS3) error {
	keys := map[string]string{"acme": acmeKMSKey, "globex": globexKMSKey} // loaded at start-up
	disk, err := newBucketDisk(ctx, client, "app-files", fss3.Config{
		SSEKMSKeyID: defaultKMSKey, // the disk's key, for keys outside any tenant
		SSEKMSKeyFor: func(key string) string { // the key as the driver sees it, under the scope's prefix
			tenant, _, _ := strings.Cut(strings.TrimPrefix(key, "tenants/"), "/")
			return keys[tenant] // "" falls back to SSEKMSKeyID
		},
	})
	if err != nil {
		return err
	}
	for _, tenant := range []string{"acme", "globex"} {
		scoped, err := disk.Scope(tenant, "tenants/"+tenant)
		if err != nil {
			return err
		}
		if _, err := scoped.WriteBytes(ctx, "logo.txt", []byte(tenant), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	if _, err := disk.WriteBytes(ctx, "shared/terms.txt", []byte("terms"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	for _, path := range []string{"/app-files/tenants/acme/logo.txt", "/app-files/tenants/globex/logo.txt", "/app-files/shared/terms.txt"} {
		h := fake.lastSent(http.MethodPut, path, "PutObject")
		fmt.Fprintf(out, "kms: %s -> %s %s\n", strings.TrimPrefix(path, "/app-files/"), h.Get("X-Amz-Server-Side-Encryption"), h.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"))
	}
	return nil
}

// customerKey encrypts with a key the application holds (SSE-C): it goes
// on every request, S3 never stores it, and the disk stops signing URLs.
// ds:def id=customerkey-z2trfajm
func customerKey(ctx context.Context, out io.Writer, client *s3.Client, fake *fakeS3) error {
	raw := []byte("0123456789abcdef0123456789abcdef") // 32 bytes from the secret manager, never from code
	key, err := fss3.NewCustomerKey(raw)
	if err != nil {
		return err
	}
	disk, err := newBucketDisk(ctx, client, "sealed", fss3.Config{SSECustomerKey: key})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "secret.txt", []byte("for your eyes only"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "secret.txt")
	if err != nil {
		return err
	}
	read := fake.lastSent(http.MethodGet, "/sealed/secret.txt", "GetObject")
	fmt.Fprintf(out, "sse-c: read %q with algorithm %s, key printed as %v, presign declared: %t\n",
		got, read.Get("X-Amz-Server-Side-Encryption-Customer-Algorithm"), key, disk.Capabilities().Presign)
	return nil
}

// bucketLifecycle sets the bucket's lifecycle rules (a setup step with
// admin credentials) and reads them back.
// ds:def id=bucketlifecycle-kf7c8yf7
func bucketLifecycle(ctx context.Context, out io.Writer, client *s3.Client, _ *fakeS3) error {
	err := fss3.SetLifecycle(ctx, client, bucketName,
		fss3.LifecycleRule{ID: "tmp", Prefix: "tmp/", ExpireAfterDays: 1},
		fss3.LifecycleRule{ID: "uploads", AbortIncompleteUploadsAfterDays: 7},
		fss3.LifecycleRule{ID: "logs", Prefix: "logs/", Transitions: []fss3.LifecycleTransition{{AfterDays: 90, StorageClass: "GLACIER"}}},
	)
	if err != nil {
		return err
	}
	rules, err := fss3.Lifecycle(ctx, client, bucketName)
	if err != nil {
		return err
	}
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	fmt.Fprintf(out, "lifecycle: %s; logs move to %s after %d days\n", strings.Join(ids, ","), rules[2].Transitions[0].StorageClass, rules[2].Transitions[0].AfterDays)
	return nil
}

// bucketReplication sets cross-region replication of one prefix (a setup
// step; S3 needs versioning on both buckets) and reads it back.
// ds:def id=bucketreplication-57t5vubn
func bucketReplication(ctx context.Context, out io.Writer, client *s3.Client, _ *fakeS3) error {
	err := fss3.SetReplication(ctx, client, "history", fss3.ReplicationConfig{
		Role: "arn:aws:iam::111122223333:role/s3-replication",
		Rules: []fss3.ReplicationRule{{
			ID: "contracts-dr", Priority: 1, Prefix: "contracts/",
			DestinationBucket: "arn:aws:s3:::history-eu-west-1",
			StorageClass:      "STANDARD_IA",
			ReplicateDeletes:  false, // a backup, not a mirror
		}},
	})
	if err != nil {
		return err
	}
	cfg, err := fss3.Replication(ctx, client, "history")
	if err != nil {
		return err
	}
	r := cfg.Rules[0]
	fmt.Fprintf(out, "replication: %s %s -> %s, deletes replicated: %t\n", r.ID, r.Prefix, r.DestinationBucket, r.ReplicateDeletes)
	return nil
}

// delegatePrefix issues temporary credentials that reach only one user's
// folder, for a client that uploads to S3 directly.
// ds:def id=delegateprefix-jaey2apq
func delegatePrefix(ctx context.Context, out io.Writer, _ *s3.Client, _ *fakeS3) error {
	stsClient := &fakeSTS{} // sts.NewFromConfig(cfg) in production
	creds, err := fss3.DelegatePrefix(ctx, stsClient, fss3.DelegateOptions{
		Role:        uploaderRole,
		SessionName: "user-42",
		Bucket:      bucketName,
		Prefix:      "users/42",
		Write:       true,
		Duration:    time.Hour,
	})
	if err != nil {
		return err
	}
	var policy struct{ Statement []struct{ Resource []string } }
	if err := json.Unmarshal([]byte(aws.ToString(stsClient.last.Policy)), &policy); err != nil {
		return err
	}
	fmt.Fprintf(out, "delegated: %s\n", policy.Statement[0].Resource[0])
	fmt.Fprintf(out, "credentials print as: %v\n", redactExpiry(creds.String()))
	return nil
}

// bringYourOwnBucket works through a role in the customer's account: the
// application assumes it with an external ID, and every S3 request is
// signed with the role's temporary credentials.
// ds:def id=bringyourownbucket-9tmvyaps
func bringYourOwnBucket(ctx context.Context, out io.Writer, _ *s3.Client, fake *fakeS3) error {
	stsClient := &fakeSTS{} // the application's own STS client in production
	creds, err := fss3.RoleCredentials(stsClient, fss3.RoleOptions{
		RoleARN:     customerRole,
		ExternalID:  tenantExternal, // generated per tenant by the application
		SessionName: "tenant-acme",
	})
	if err != nil {
		return err
	}
	client := s3.New(s3.Options{Region: fakeRegion, BaseEndpoint: aws.String(fake.endpoint), UsePathStyle: true, Credentials: creds})
	disk, err := newBucketDisk(ctx, client, "acme-files", fss3.Config{})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "report.txt", []byte("q3"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	put := fake.lastSent(http.MethodPut, "/acme-files/report.txt", "PutObject")
	fmt.Fprintf(out, "byo bucket: STS saw external ID %s; PUT signed by %s with a session token: %t\n",
		aws.ToString(stsClient.last.ExternalId), signingKey(put), put.Get("X-Amz-Security-Token") != "")
	fmt.Fprintf(out, "role provider prints as: %v\n", creds)
	return nil
}

// fakeSTS stands in for STS: it records the AssumeRole input and returns
// fixed temporary credentials valid for an hour.
type fakeSTS struct{ last *sts.AssumeRoleInput }

// AssumeRole implements fss3.AssumeRoler.
func (f *fakeSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.last = in
	exp := time.Now().Add(time.Hour)
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{
		AccessKeyId: aws.String(roleKeyID), SecretAccessKey: aws.String("role-secret"), SessionToken: aws.String("role-token"), Expiration: &exp,
	}}, nil
}

// signingKey returns the access key ID a SigV4 Authorization header names.
func signingKey(h http.Header) string {
	_, rest, _ := strings.Cut(h.Get("Authorization"), "Credential=")
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// redactExpiry drops the expiry time from printed credentials, which
// changes on every run, so the printed line is stable.
func redactExpiry(s string) string {
	before, _, _ := strings.Cut(s, ", Expires:")
	return before + "}"
}

// httpGet fetches rawURL with a plain HTTP client, as a browser would.
func httpGet(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp, resp.Body.Close()
}
