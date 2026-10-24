package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// Values of the GovCloud delegation step.
const (
	govRole   = "arn:aws-us-gov:iam::111122223333:role/uploader"
	govKMSKey = "arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// restoreDays is how long a restored copy of an archived object stays
// readable.
const restoreDays = 7

// objectTags sets tags with a write through RequestOptions, reads them,
// replaces them with SetTags, and shows a copy keeping its source's tags.
// ds:def id=objecttags-u5m8k2ws
func objectTags(ctx context.Context, out io.Writer, client *s3.Client, _ *fakeS3) error {
	disk, err := newBucketDisk(ctx, client, "tagged", fss3.Config{Tagging: true})
	if err != nil {
		return err
	}
	wctx, err := fss3.WithRequestOptions(ctx, disk, "invoices/7.pdf", fss3.RequestOptions{Tags: map[string]string{"tenant": "acme", "class": "billing"}})
	if err != nil {
		return err // an invalid key
	}
	if _, err := disk.WriteBytes(wctx, "invoices/7.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	written, err := disk.Tags(ctx, "invoices/7.pdf")
	if err != nil {
		return err
	}
	if err := disk.SetTags(ctx, "invoices/7.pdf", map[string]string{"tenant": "acme", "retain": "7y"}); err != nil {
		return err
	}
	replaced, err := disk.Tags(ctx, "invoices/7.pdf")
	if err != nil {
		return err
	}
	if _, err := disk.Copy(ctx, "invoices/7.pdf", "archive/7.pdf"); err != nil {
		return err
	}
	copied, err := disk.Tags(ctx, "archive/7.pdf")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "tags on write: %s; after SetTags: %s; the copy keeps: %s\n", tagList(written), tagList(replaced), tagList(copied))
	return nil
}

// tagList prints tags sorted by key.
func tagList(tags map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ",")
}

// legalHold writes a contract under a legal hold on an Object Lock
// bucket: deleting or overwriting it is refused until the hold is
// lifted.
// ds:def id=legalhold-c9r3t6vy
func legalHold(ctx context.Context, out io.Writer, client *s3.Client, fake *fakeS3) error {
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("records"), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
		return err
	}
	fake.objects.enableLock("records") // the fake ignores ObjectLockEnabledForBucket
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: "records", ObjectLock: true})
	if err != nil {
		return err
	}
	if err := drv.CheckBucket(ctx); err != nil {
		return err // ObjectLock declared on a bucket without it: CodeInvalidArgument
	}
	disk, err := filesystem.NewDisk("records", drv)
	if err != nil {
		return err
	}
	wctx, err := fss3.WithRequestOptions(ctx, disk, "contracts/acme.pdf", fss3.RequestOptions{LegalHold: true})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(wctx, "contracts/acme.pdf", []byte("signed"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	hold, err := disk.Hold(ctx, "contracts/acme.pdf")
	if err != nil {
		return err
	}
	delErr := disk.Delete(ctx, "contracts/acme.pdf")
	_, writeErr := disk.WriteBytes(ctx, "contracts/acme.pdf", []byte("forged"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "legal hold: %t; delete: %s; overwrite: %s\n", hold.Legal, filesystem.CodeOf(delErr), filesystem.CodeOf(writeErr))
	if err := disk.SetHold(ctx, "contracts/acme.pdf", filesystem.Hold{}); err != nil { // lift it
		return err
	}
	err = disk.Delete(ctx, "contracts/acme.pdf")
	fmt.Fprintf(out, "hold lifted, delete: %v\n", err)
	return nil
}

// archiveRestore writes into GLACIER: a read fails until a restore,
// asked at the Bulk tier, has finished.
// ds:def id=archiverestore-k7w4n9pd
func archiveRestore(ctx context.Context, out io.Writer, client *s3.Client, fake *fakeS3) error {
	disk, err := newBucketDisk(ctx, client, "cold", fss3.Config{
		Restore: true, RestoreTier: types.TierBulk, StorageClasses: true, StorageClass: types.StorageClassGlacier,
	})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "logs/2019.tar", []byte("old logs"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, err = disk.ReadAll(ctx, "logs/2019.tar")
	fmt.Fprintf(out, "archived read: %s, ErrArchived: %t\n", filesystem.CodeOf(err), errors.Is(err, fss3.ErrArchived))
	if err := disk.Restore(ctx, "logs/2019.tar", filesystem.RestoreOptions{Days: restoreDays}); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "logs/2019.tar")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "restore asked at tier %s: %s\n", fake.objects.restoreTier("/cold/logs/2019.tar"), e.Metadata[fss3.MetaRestore])
	fake.objects.finishRestore("/cold/logs/2019.tar") // hours later on S3
	b, err := disk.ReadAll(ctx, "logs/2019.tar")
	if err != nil {
		return err
	}
	e, err = disk.Stat(ctx, "logs/2019.tar")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "restored: reads %q, %s until %s\n", b, e.Metadata[fss3.MetaRestore], e.Metadata[fss3.MetaRestoreExpiry])
	return nil
}

// govCloudDelegation builds the session policy for a GovCloud bucket
// encrypted with a customer-managed KMS key. The partition comes from the
// role's ARN; the key is listed so the session may use it.
// ds:def id=govclouddelegation-q2h6x8bm
func govCloudDelegation(ctx context.Context, out io.Writer, _ *s3.Client, _ *fakeS3) error {
	stsClient := &fakeSTS{} // sts.NewFromConfig(cfg) in production
	_, err := fss3.DelegatePrefix(ctx, stsClient, fss3.DelegateOptions{
		Role:        govRole, // Partition becomes aws-us-gov
		SessionName: "user-42",
		Bucket:      bucketName,
		Prefix:      "users/42",
		Write:       true,
		Duration:    time.Hour,
		KMSKeyARNs:  []string{govKMSKey},
	})
	if err != nil {
		return err
	}
	var policy struct {
		Statement []struct {
			Action   any
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(aws.ToString(stsClient.last.Policy)), &policy); err != nil {
		return err
	}
	last := policy.Statement[len(policy.Statement)-1]
	fmt.Fprintf(out, "govcloud: objects %s; kms %v on %s\n", policy.Statement[0].Resource[0], last.Action, last.Resource[0])
	_, err = fss3.SessionPolicy(fss3.DelegateOptions{Role: govRole, Bucket: bucketName, Prefix: "users/42", Partition: fss3.PartitionChina})
	fmt.Fprintf(out, "china partition for a govcloud role: %s\n", filesystem.CodeOf(err))
	_, err = fss3.SessionPolicy(fss3.DelegateOptions{Role: govRole, Bucket: bucketName, Prefix: "users/42", KMSKeyARNs: []string{"arn:aws-us-gov:kms:us-gov-west-1:111122223333:alias/media"}})
	fmt.Fprintf(out, "kms alias instead of a key: %s\n", filesystem.CodeOf(err))
	return nil
}
