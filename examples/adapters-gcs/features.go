package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// kmsKeyName is the Cloud KMS key (CMEK) of the encryption step. A key
// name is not a secret, so it may live in configuration and a DSN.
const kmsKeyName = "projects/example-project/locations/eu/keyRings/files/cryptoKeys/media"

// fixture is what the feature steps share: the fake, the signing identity
// and the disk opened by openWithConfig.
type fixture struct {
	srv     *fakestorage.Server
	signing *storage.SignedURLOptions
	disk    *filesystem.Disk
}

// featureStep is one object-store feature step.
type featureStep func(ctx context.Context, out io.Writer, f fixture) error

// runFeatures runs the object-store feature steps in order.
func runFeatures(ctx context.Context, out io.Writer, f fixture) error {
	for _, step := range []featureStep{
		storageClass, bucketVersions, retention, responseContentType, kmsKey, requestOptions, customerKey, bucketLifecycle, bucketReplication, delegatePrefix,
	} {
		if err := step(ctx, out, f); err != nil {
			return err
		}
	}
	return nil
}

// newBucketDisk creates bucket on the fake and opens a disk on it with cfg
// (Client and Bucket are filled in). It returns the driver too, for its
// GCS-only methods (ObjectInfo).
func newBucketDisk(f fixture, bucket string, versioned bool, cfg gcs.Config) (*gcs.Driver, *filesystem.Disk, error) {
	f.srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket, VersioningEnabled: versioned})
	cfg.Client, cfg.Bucket = f.srv.Client(), bucket
	drv, err := gcs.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(bucket, drv)
	return drv, disk, err
}

// storageClass gives every write of a disk a colder class than the
// bucket's default, and reads it back with Stat.
// ds:def id=storageclass-pj8rpapy
func storageClass(ctx context.Context, out io.Writer, f fixture) error {
	_, disk, err := newBucketDisk(f, "cold-store", false, gcs.Config{StorageClass: gcs.ClassNearline})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "logs/2026-09.log", []byte("september"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "logs/2026-09.log")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "storage class: %s\n", e.StorageClass)
	return nil
}

// bucketVersions writes a key twice on a bucket with Object Versioning,
// reads the older generation and brings it back as the live object.
// ds:def id=bucketversions-qwpnag3j
func bucketVersions(ctx context.Context, out io.Writer, f fixture) error {
	_, disk, err := newBucketDisk(f, "history", true, gcs.Config{Versioned: true})
	if err != nil {
		return err
	}
	for _, body := range []string{"draft", "final"} {
		if _, err := disk.WriteBytes(ctx, "contract.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	vs, err := disk.Versions(ctx, "contract.txt") // newest first; an ID is a generation
	if err != nil {
		return err
	}
	rc, err := disk.ReadVersion(ctx, "contract.txt", vs[1].ID)
	if err != nil {
		return err
	}
	old, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return err
	}
	if err := disk.RestoreVersion(ctx, "contract.txt", vs[1].ID); err != nil {
		return err
	}
	live, err := disk.ReadAll(ctx, "contract.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "versions: %d, older reads %q, after RestoreVersion the live object reads %q\n", len(vs), old, live)
	return nil
}

// retention gives every object a retention date an hour out (RetainFor),
// then extends one object's with SetHold, reading both back.
// ds:def id=retention-b9yuyv4w
func retention(ctx context.Context, out io.Writer, f fixture) error {
	drv, disk, err := newBucketDisk(f, "records", false, gcs.Config{RetainFor: time.Hour})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "contracts/acme.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	info, err := drv.ObjectInfo(ctx, "contracts/acme.pdf")
	if err != nil {
		return err
	}
	until := time.Now().AddDate(0, 0, 30).UTC().Truncate(time.Second)
	if err := disk.SetHold(ctx, "contracts/acme.pdf", filesystem.Hold{RetainUntil: until}); err != nil {
		return err
	}
	h, err := disk.Hold(ctx, "contracts/acme.pdf")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "retention: mode %s on write, extended to 30 days: %t\n", info.RetentionMode, h.RetainUntil.Equal(until))
	return nil
}

// responseContentType signs a download that overrides the stored content
// type, so a file stored as bytes opens as text in the browser.
// ds:def id=responsecontenttype-y8jngg64
func responseContentType(ctx context.Context, out io.Writer, f fixture) error {
	req, err := f.disk.SignedURL(ctx, "export.dat", filesystem.PresignOptions{
		Method:      filesystem.MethodGet,
		Expires:     linkLifetime,
		ContentType: "text/plain", // signed as response-content-type
	})
	if err != nil {
		return err
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET override: response-content-type=%s\n", u.Query().Get("response-content-type"))
	return nil
}

// kmsKey names a Cloud KMS key for every object the disk creates; a
// presigned PUT signs it, so a direct upload is encrypted the same way.
// ds:def id=kmskey-ydr9m4vx
func kmsKey(ctx context.Context, out io.Writer, f fixture) error {
	_, disk, err := newBucketDisk(f, "cmek", false, gcs.Config{KMSKeyName: kmsKeyName, Signing: f.signing})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	put, err := disk.SignedURL(ctx, "direct.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: linkLifetime})
	if err != nil {
		return err
	}
	u, err := url.Parse(put.URL)
	if err != nil {
		return err
	}
	signed := strings.Contains(u.Query().Get("X-Goog-SignedHeaders"), "x-goog-encryption-kms-key-name")
	fmt.Fprintf(out, "kms: presigned PUT signs the key: %t, client sends %s\n", signed, put.Header.Get("X-Goog-Encryption-Kms-Key-Name"))
	return nil
}

// customerKey encrypts with a key the application holds (CSEK): it goes on
// every request, GCS keeps only its hash, and the disk stops signing URLs.
// ds:def id=customerkey-7w6wh6d9
func customerKey(ctx context.Context, out io.Writer, f fixture) error {
	raw := []byte("0123456789abcdef0123456789abcdef") // 32 bytes from the secret manager, never from code
	key, err := gcs.NewCustomerKey(raw)
	if err != nil {
		return err
	}
	_, disk, err := newBucketDisk(f, "sealed", false, gcs.Config{CustomerKey: key, Signing: f.signing})
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
	fmt.Fprintf(out, "csek: read %q, key printed as %v, presign declared: %t\n", got, key, disk.Capabilities().Presign)
	return nil
}

// bucketLifecycle sets the bucket's lifecycle rules (a setup step with
// admin credentials) and reads them back. GCS rules carry one action
// each.
// ds:def id=bucketlifecycle-jwr74z2b
func bucketLifecycle(ctx context.Context, out io.Writer, _ fixture) error {
	client, stop, err := bucketConfigClient(ctx)
	if err != nil {
		return err
	}
	defer stop()
	err = gcs.SetLifecycle(ctx, client, bucketName,
		gcs.LifecycleRule{Action: gcs.LifecycleDelete, AgeDays: 1, Prefixes: []string{"tmp/"}},
		gcs.LifecycleRule{Action: gcs.LifecycleSetStorageClass, StorageClass: "ARCHIVE", AgeDays: 90, Prefixes: []string{"logs/"}},
		gcs.LifecycleRule{Action: gcs.LifecycleAbortUploads, AgeDays: 7},
	)
	if err != nil {
		return err
	}
	rules, err := gcs.Lifecycle(ctx, client, bucketName)
	if err != nil {
		return err
	}
	actions := make([]string, len(rules))
	for i, r := range rules {
		actions[i] = r.Action
	}
	fmt.Fprintf(out, "lifecycle: %s; logs/ move to %s after %d days\n", strings.Join(actions, ","), rules[1].StorageClass, rules[1].AgeDays)
	return nil
}

// bucketReplication turns on turbo replication of a dual-region bucket (a
// setup step) and reads the mode back.
// ds:def id=bucketreplication-ue44rg6x
func bucketReplication(ctx context.Context, out io.Writer, _ fixture) error {
	client, stop, err := bucketConfigClient(ctx)
	if err != nil {
		return err
	}
	defer stop()
	if err := gcs.SetReplication(ctx, client, bucketName, gcs.ReplicationTurbo); err != nil {
		return err
	}
	mode, err := gcs.Replication(ctx, client, bucketName)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "replication: %s\n", mode)
	return nil
}

// delegatePrefix issues a downscoped token that reaches only one user's
// folder, for a client that talks to GCS directly. The root token source
// is the application's own credentials; the exchange goes to Google's STS.
// ds:def id=delegateprefix-ftu27q44
func delegatePrefix(ctx context.Context, out io.Writer, _ fixture) error {
	sts := &fakeSTS{}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: sts}) // only for the stand-in STS
	root := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "root-token", Expiry: time.Now().Add(time.Hour)})
	tok, err := gcs.DelegatePrefix(ctx, root, gcs.DelegateOptions{Bucket: bucketName, Prefix: "users/42", Access: gcs.DelegateCreate})
	if err != nil {
		return err
	}
	var sent struct {
		AccessBoundary struct {
			AccessBoundaryRules []struct{ AvailablePermissions []string }
		}
	}
	if err := json.Unmarshal([]byte(sts.boundary), &sent); err != nil {
		return err
	}
	fmt.Fprintf(out, "delegated %s/%s with %s\n", tok.Bucket, tok.Prefix, strings.Join(sent.AccessBoundary.AccessBoundaryRules[0].AvailablePermissions, ","))
	fmt.Fprintf(out, "token prints as: %s\n", redactExpiry(tok.String()))
	return nil
}

// fakeSTS stands in for Google's token exchange: it records the access
// boundary it was asked for and returns a fixed downscoped token.
type fakeSTS struct{ boundary string }

// RoundTrip implements http.RoundTripper.
func (f *fakeSTS) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	f.boundary = form.Get("options")
	resp := `{"access_token":"downscoped-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(resp)), Request: r}, nil
}

// redactExpiry drops the expiry time from a printed token, which changes
// on every run, so the printed line is stable.
func redactExpiry(s string) string {
	before, _, _ := strings.Cut(s, ", Expires:")
	return before + "}"
}

// bucketConfigClient returns a storage client talking to a stand-in for
// the GCS bucket metadata API, and a function that stops it.
// fake-gcs-server accepts lifecycle and replication updates but keeps
// neither, so the example stores them the way GCS does instead: a PATCH
// merges its fields into the bucket's document, a GET returns it, and the
// SDK's encoding and decoding both run.
func bucketConfigClient(ctx context.Context) (*storage.Client, func(), error) {
	var mu sync.Mutex
	doc := map[string]json.RawMessage{"name": json.RawMessage(`"` + bucketName + `"`)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPatch {
			var patch map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			maps.Copy(doc, patch)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	client, err := storage.NewClient(ctx, option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	return client, func() { _ = client.Close(); srv.Close() }, nil
}
