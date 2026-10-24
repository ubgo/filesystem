package s3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// configStore is a fake S3 endpoint for bucket and object configuration:
// it keeps the body of every PUT to a subresource (?lifecycle,
// ?replication, ?acl) and returns it on the matching GET, the way S3
// stores a configuration document, so the SDK's encoding and decoding
// both run. A GET of a subresource never PUT answers S3's "no such
// configuration" error.
type configStore struct {
	mu      sync.Mutex
	docs    map[string][]byte
	headers map[string]http.Header
}

// notFoundCodes are S3's answers for a missing configuration.
var notFoundCodes = map[string]string{"lifecycle": "NoSuchLifecycleConfiguration", "replication": "ReplicationConfigurationNotFoundError", "acl": "NoSuchKey"}

func (c *configStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var sub string
	for name := range notFoundCodes {
		if q.Has(name) {
			sub = name
		}
	}
	id := strings.TrimSuffix(r.URL.Path, "/") + "?" + sub
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		c.docs[id], c.headers[id] = b, r.Header.Clone()
	case http.MethodGet:
		b, ok := c.docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", notFoundCodes[sub])
			return
		}
		_, _ = w.Write(b)
	}
}

// newConfigClient returns an S3 client talking to a fresh configStore.
func newConfigClient(t *testing.T) (*s3.Client, *configStore) {
	t.Helper()
	store := &configStore{docs: map[string][]byte{}, headers: map[string]http.Header{}}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
	})
	return client, store
}

// TestLifecycleRoundTrip proves SetLifecycle stores rules that Lifecycle
// reads back unchanged (expiry, multipart cleanup, transitions, per
// prefix and bucket-wide), that a bucket without rules reads as none, and
// that rules without an action or with bad values are refused.
func TestLifecycleRoundTrip(t *testing.T) {
	ctx := t.Context()
	client, _ := newConfigClient(t)
	if rules, err := fss3.Lifecycle(ctx, client, "b"); err != nil || rules != nil {
		t.Fatalf("no configuration = %v, %v", rules, err)
	}
	want := []fss3.LifecycleRule{
		{ID: "tmp", Prefix: "tmp/", ExpireAfterDays: 1},
		{ID: "uploads", AbortIncompleteUploadsAfterDays: 7},
		{ID: "archive", Prefix: "logs/", ExpireAfterDays: 365, Transitions: []fss3.LifecycleTransition{{AfterDays: 30, StorageClass: "STANDARD_IA"}, {AfterDays: 90, StorageClass: "GLACIER"}}},
		{ID: "paused", Prefix: "old/", ExpireAfterDays: 30, Disabled: true},
	}
	if err := fss3.SetLifecycle(ctx, client, "b", want...); err != nil {
		t.Fatal(err)
	}
	got, err := fss3.Lifecycle(ctx, client, "b")
	if err != nil || !slices.EqualFunc(got, want, lifecycleEqual) {
		t.Fatalf("read back %+v, %v\nwant %+v", got, err, want)
	}
	for _, bad := range [][]fss3.LifecycleRule{
		nil,
		{{ID: "nothing"}},
		{{ID: "negative", ExpireAfterDays: -1}},
		{{ID: "classless", Transitions: []fss3.LifecycleTransition{{AfterDays: 1}}}},
	} {
		if err := fss3.SetLifecycle(ctx, client, "b", bad...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("SetLifecycle(%+v) = %v", bad, err)
		}
	}
}

// TestLifecycleReadBackNeverWidens proves a stored rule using anything
// LifecycleRule cannot represent is an error naming it, never a rule with
// that part dropped. The reviewer's case: a tag-filtered one-day expiry
// read back as a prefix-less rule would, written back, expire the whole
// bucket after a day. Each stored configuration is written as S3 returns
// it, beside a plain rule that alone would read back fine.
func TestLifecycleReadBackNeverWidens(t *testing.T) {
	ctx := t.Context()
	client, store := newConfigClient(t)
	const plain = `<Rule><ID>plain</ID><Filter><Prefix>tmp/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`
	for name, rule := range map[string]string{
		"tag filter":            `<Rule><ID>tagged</ID><Filter><Tag><Key>temp</Key><Value>true</Value></Tag></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`,
		"and filter":            `<Rule><ID>tagged</ID><Filter><And><Prefix>a/</Prefix><Tag><Key>temp</Key><Value>true</Value></Tag></And></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`,
		"size filter":           `<Rule><ID>tagged</ID><Filter><ObjectSizeGreaterThan>1000</ObjectSizeGreaterThan></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`,
		"noncurrent expiry":     `<Rule><ID>tagged</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>30</NoncurrentDays></NoncurrentVersionExpiration></Rule>`,
		"noncurrent transition": `<Rule><ID>tagged</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><NoncurrentVersionTransition><NoncurrentDays>30</NoncurrentDays><StorageClass>GLACIER</StorageClass></NoncurrentVersionTransition></Rule>`,
		"delete markers":        `<Rule><ID>tagged</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`,
		"expiry date":           `<Rule><ID>tagged</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><Expiration><Date>2027-01-01T00:00:00Z</Date></Expiration></Rule>`,
		"transition date":       `<Rule><ID>tagged</ID><Filter><Prefix></Prefix></Filter><Status>Enabled</Status><Transition><Date>2027-01-01T00:00:00Z</Date><StorageClass>GLACIER</StorageClass></Transition></Rule>`,
	} {
		store.mu.Lock()
		store.docs["/b?lifecycle"] = []byte(`<LifecycleConfiguration>` + plain + rule + `</LifecycleConfiguration>`)
		store.mu.Unlock()
		rules, err := fss3.Lifecycle(ctx, client, "b")
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), `"tagged"`) {
			t.Errorf("%s: read back %+v, %v", name, rules, err)
		}
	}
	// The reviewer's exact pair: a tagged expiry and a disabled rule.
	store.mu.Lock()
	store.docs["/b?lifecycle"] = []byte(`<LifecycleConfiguration><Rule><ID>off</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`)
	store.mu.Unlock()
	rules, err := fss3.Lifecycle(ctx, client, "b")
	if err != nil || len(rules) != 1 || !rules[0].Disabled || rules[0].Prefix != "logs/" {
		t.Fatalf("disabled rule read back %+v, %v", rules, err)
	}
	if err := fss3.SetLifecycle(ctx, client, "b", rules...); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	written := string(store.docs["/b?lifecycle"])
	store.mu.Unlock()
	if !strings.Contains(written, "<Status>Disabled</Status>") || strings.Contains(written, "<Status>Enabled</Status>") {
		t.Errorf("disabled rule written back as %s", written)
	}
}

func lifecycleEqual(a, b fss3.LifecycleRule) bool {
	return a.Disabled == b.Disabled && a.ID == b.ID && a.Prefix == b.Prefix && a.ExpireAfterDays == b.ExpireAfterDays &&
		a.AbortIncompleteUploadsAfterDays == b.AbortIncompleteUploadsAfterDays && slices.Equal(a.Transitions, b.Transitions)
}

// TestReplicationRoundTrip proves SetReplication stores a cross-region
// configuration that Replication reads back unchanged, that a bucket
// without one reads as the zero value, and that a configuration without
// a role, rules or a destination is refused.
func TestReplicationRoundTrip(t *testing.T) {
	ctx := t.Context()
	client, _ := newConfigClient(t)
	if got, err := fss3.Replication(ctx, client, "b"); err != nil || got.Role != "" || got.Rules != nil {
		t.Fatalf("no configuration = %+v, %v", got, err)
	}
	want := fss3.ReplicationConfig{Role: "arn:aws:iam::123456789012:role/replicate", Rules: []fss3.ReplicationRule{
		{ID: "all", Priority: 1, DestinationBucket: "arn:aws:s3:::b-eu", StorageClass: "STANDARD_IA", ReplicateDeletes: true},
		{ID: "media", Priority: 2, Prefix: "media/", DestinationBucket: "arn:aws:s3:::b-backup"},
		{ID: "paused", Priority: 3, Prefix: "old/", DestinationBucket: "arn:aws:s3:::b-backup", Disabled: true},
	}}
	if err := fss3.SetReplication(ctx, client, "b", want); err != nil {
		t.Fatal(err)
	}
	got, err := fss3.Replication(ctx, client, "b")
	if err != nil || got.Role != want.Role || !slices.Equal(got.Rules, want.Rules) {
		t.Fatalf("read back %+v, %v", got, err)
	}
	for _, bad := range []fss3.ReplicationConfig{
		{Rules: want.Rules},
		{Role: want.Role},
		{Role: want.Role, Rules: []fss3.ReplicationRule{{ID: "nowhere"}}},
	} {
		if err := fss3.SetReplication(ctx, client, "b", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("SetReplication(%+v) = %v", bad, err)
		}
	}
}

// TestReplicationReadBackNeverWidens proves a stored rule using anything
// ReplicationRule cannot represent is an error naming it, never a rule
// with that part dropped.
func TestReplicationReadBackNeverWidens(t *testing.T) {
	ctx := t.Context()
	client, store := newConfigClient(t)
	const dest = `<Destination><Bucket>arn:aws:s3:::d</Bucket></Destination>`
	const deletes = `<DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication>`
	for name, rule := range map[string]string{
		"tag filter":       `<Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Priority>1</Priority>` + deletes + dest,
		"older schema":     `<Prefix>a/</Prefix>` + dest,
		"existing objects": `<Filter><Prefix></Prefix></Filter><Priority>1</Priority>` + deletes + `<ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication>` + dest,
		"replica sync":     `<Filter><Prefix></Prefix></Filter><Priority>1</Priority>` + deletes + `<SourceSelectionCriteria><ReplicaModifications><Status>Enabled</Status></ReplicaModifications></SourceSelectionCriteria>` + dest,
		"encryption":       `<Filter><Prefix></Prefix></Filter><Priority>1</Priority>` + deletes + `<Destination><Bucket>arn:aws:s3:::d</Bucket><EncryptionConfiguration><ReplicaKmsKeyID>k</ReplicaKmsKeyID></EncryptionConfiguration></Destination>`,
		"account":          `<Filter><Prefix></Prefix></Filter><Priority>1</Priority>` + deletes + `<Destination><Bucket>arn:aws:s3:::d</Bucket><Account>1</Account></Destination>`,
		"no delete status": `<Filter><Prefix></Prefix></Filter><Priority>1</Priority>` + dest,
	} {
		store.mu.Lock()
		store.docs["/b?replication"] = []byte(`<ReplicationConfiguration><Role>r</Role><Rule><ID>odd</ID><Status>Enabled</Status>` + rule + `</Rule></ReplicationConfiguration>`)
		store.mu.Unlock()
		got, err := fss3.Replication(ctx, client, "b")
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), `"odd"`) {
			t.Errorf("%s: read back %+v, %v", name, got, err)
		}
	}
}

// TestObjectACL proves explicit grants travel as S3's grant headers, one
// header per permission listing every grantee, that a canned ACL is sent
// as such, and that bad ACLs and unclean keys are refused before any
// request. The read side is checked against the configStore's echo of a
// stored AccessControlPolicy.
func TestObjectACL(t *testing.T) {
	ctx := t.Context()
	client, store := newConfigClient(t)
	acl := fss3.ObjectACL{Grants: []fss3.Grant{
		{Grantee: fss3.Grantee{ID: "partner-canonical-id"}, Permission: fss3.GrantRead},
		{Grantee: fss3.Grantee{URI: fss3.GroupAuthenticatedUsers}, Permission: fss3.GrantRead},
		{Grantee: fss3.Grantee{ID: "auditor"}, Permission: fss3.GrantReadACP},
		{Grantee: fss3.Grantee{Email: "ops@example.com"}, Permission: fss3.GrantFullControl},
	}, AllowPublic: true}
	if err := fss3.SetObjectACL(ctx, client, "b", "docs/a.pdf", acl); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	h := store.headers["/b/docs/a.pdf?acl"]
	store.mu.Unlock()
	for header, want := range map[string]string{
		"X-Amz-Grant-Read":         `id="partner-canonical-id", uri="http://acs.amazonaws.com/groups/global/AuthenticatedUsers"`,
		"X-Amz-Grant-Read-Acp":     `id="auditor"`,
		"X-Amz-Grant-Full-Control": `emailAddress="ops@example.com"`,
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if h.Get("X-Amz-Acl") != "" || h.Get("X-Amz-Grant-Write-Acp") != "" {
		t.Errorf("unexpected headers %v", h)
	}
	if err := fss3.SetObjectACL(ctx, client, "b", "docs/b.pdf", fss3.ObjectACL{Canned: fss3.CannedBucketOwnerFullControl}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	canned := store.headers["/b/docs/b.pdf?acl"].Get("X-Amz-Acl")
	store.docs["/b/docs/c.pdf?acl"] = []byte(`<AccessControlPolicy><Owner><ID>owner</ID></Owner><AccessControlList>` +
		`<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>owner</ID></Grantee><Permission>FULL_CONTROL</Permission></Grant>` +
		`<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant>` +
		`</AccessControlList></AccessControlPolicy>`)
	store.mu.Unlock()
	if canned != "bucket-owner-full-control" {
		t.Errorf("canned ACL header %q", canned)
	}
	grants, err := fss3.ObjectGrants(ctx, client, "b", "docs/c.pdf")
	want := []fss3.Grant{{Grantee: fss3.Grantee{ID: "owner"}, Permission: fss3.GrantFullControl}, {Grantee: fss3.Grantee{URI: "http://acs.amazonaws.com/groups/global/AllUsers"}, Permission: fss3.GrantRead}}
	if err != nil || !slices.Equal(grants, want) {
		t.Errorf("ObjectGrants = %+v, %v", grants, err)
	}
	for name, c := range map[string]struct {
		key string
		acl fss3.ObjectACL
	}{
		"both":             {"a", fss3.ObjectACL{Canned: fss3.CannedPrivate, Grants: acl.Grants, AllowPublic: true}},
		"public canned":    {"a", fss3.ObjectACL{Canned: fss3.CannedPublicRead}},
		"public rw canned": {"a", fss3.ObjectACL{Canned: fss3.CannedPublicReadWrite}},
		"authn canned":     {"a", fss3.ObjectACL{Canned: fss3.CannedAuthenticatedRead}},
		"unknown canned":   {"a", fss3.ObjectACL{Canned: "everyone"}},
		"all users grant":  {"a", fss3.ObjectACL{Grants: []fss3.Grant{{Grantee: fss3.Grantee{URI: fss3.GroupAllUsers}, Permission: fss3.GrantRead}}}},
		"authn grant":      {"a", fss3.ObjectACL{Grants: []fss3.Grant{{Grantee: fss3.Grantee{URI: fss3.GroupAuthenticatedUsers}, Permission: fss3.GrantRead}}}},
		"neither":          {"a", fss3.ObjectACL{}},
		"two grantees":     {"a", fss3.ObjectACL{Grants: []fss3.Grant{{Grantee: fss3.Grantee{ID: "x", URI: "y"}, Permission: fss3.GrantRead}}}},
		"no grantee":       {"a", fss3.ObjectACL{Grants: []fss3.Grant{{Permission: fss3.GrantRead}}}},
		"write permission": {"a", fss3.ObjectACL{Grants: []fss3.Grant{{Grantee: fss3.Grantee{ID: "x"}, Permission: "WRITE"}}}},
		"traversal":        {"../a", fss3.ObjectACL{Canned: fss3.CannedPrivate}},
		"unclean":          {"/a//b", fss3.ObjectACL{Canned: fss3.CannedPrivate}},
	} {
		if err := fss3.SetObjectACL(ctx, client, "b", c.key, c.acl); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := fss3.SetObjectACL(ctx, client, "b", "docs/p.pdf", fss3.ObjectACL{Canned: fss3.CannedPublicRead, AllowPublic: true}); err != nil {
		t.Errorf("public canned ACL with AllowPublic: %v", err)
	}
	store.mu.Lock()
	store.docs["/b/docs/d.pdf?acl"] = []byte(`<AccessControlPolicy><AccessControlList><Grant><Permission>READ</Permission></Grant></AccessControlList></AccessControlPolicy>`)
	store.mu.Unlock()
	if g, err := fss3.ObjectGrants(ctx, client, "b", "docs/d.pdf"); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("grant without a grantee read back as %+v, %v", g, err)
	}
	if _, err := fss3.ObjectGrants(ctx, client, "b", "../x"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("ObjectGrants on a traversal key: %v", err)
	}
}

// fakeSTS records the AssumeRole input and returns fixed credentials.
type fakeSTS struct{ last *sts.AssumeRoleInput }

func (f *fakeSTS) AssumeRole(_ context.Context, in *sts.AssumeRoleInput, _ ...func(*sts.Options)) (*sts.AssumeRoleOutput, error) {
	f.last = in
	exp := time.Date(2026, 10, 7, 13, 0, 0, 0, time.UTC)
	return &sts.AssumeRoleOutput{Credentials: &ststypes.Credentials{AccessKeyId: aws.String("ASIAEXAMPLE"), SecretAccessKey: aws.String("very-secret-key"), SessionToken: aws.String("very-secret-token"), Expiration: &exp}}, nil
}

// iamPolicy is the shape of PrefixPolicy's output.
type iamPolicy struct {
	Statement []struct {
		Effect    string
		Action    []string
		Resource  []string
		Condition map[string]map[string][]string
	}
}

// TestDelegatePrefix proves the session policy allows exactly the prefix
// (objects under prefix/ and listings under prefix/, nothing else), that
// write access is opt-in, that wildcard or unclean prefixes and bad
// durations are refused, and that the credentials never print their
// secret or token.
func TestDelegatePrefix(t *testing.T) {
	ctx := t.Context()
	f := &fakeSTS{}
	o := fss3.DelegateOptions{Role: "arn:aws:iam::1:role/uploader", SessionName: "user-42", Bucket: "media", Prefix: "users/42", Duration: time.Hour}
	creds, err := fss3.DelegatePrefix(ctx, f, o)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt32(f.last.DurationSeconds) != 3600 || aws.ToString(f.last.RoleArn) != o.Role || aws.ToString(f.last.RoleSessionName) != "user-42" {
		t.Errorf("AssumeRole input %+v", f.last)
	}
	var doc iamPolicy
	if err := json.Unmarshal([]byte(aws.ToString(f.last.Policy)), &doc); err != nil || len(doc.Statement) != 2 {
		t.Fatalf("policy %s (%v)", aws.ToString(f.last.Policy), err)
	}
	objects, listing := doc.Statement[0], doc.Statement[1]
	if !slices.Equal(objects.Resource, []string{"arn:aws:s3:::media/users/42/*"}) || !slices.Equal(objects.Action, []string{"s3:GetObject"}) || objects.Effect != "Allow" {
		t.Errorf("object statement %+v", objects)
	}
	if !slices.Equal(listing.Resource, []string{"arn:aws:s3:::media"}) || !slices.Equal(listing.Action, []string{"s3:ListBucket"}) ||
		!slices.Equal(listing.Condition["StringLike"]["s3:prefix"], []string{"users/42/*"}) {
		t.Errorf("listing statement %+v", listing)
	}
	if creds.AccessKeyID != "ASIAEXAMPLE" || creds.SessionToken != "very-secret-token" || creds.Prefix != "users/42" || creds.Expires.IsZero() {
		t.Errorf("credentials %v", creds)
	}
	got, _ := creds.Provider().Retrieve(ctx)
	if !got.CanExpire || !got.Expires.Equal(creds.Expires) || got.SecretAccessKey != creds.SecretAccessKey {
		t.Errorf("provider %+v", got)
	}

	o.Write = true
	if _, err := fss3.DelegatePrefix(ctx, f, o); err != nil {
		t.Fatal(err)
	}
	doc = iamPolicy{}
	_ = json.Unmarshal([]byte(aws.ToString(f.last.Policy)), &doc)
	if !slices.Contains(doc.Statement[0].Action, "s3:PutObject") || !slices.Contains(doc.Statement[0].Action, "s3:DeleteObject") || !slices.Equal(doc.Statement[0].Resource, []string{"arn:aws:s3:::media/users/42/*"}) {
		t.Errorf("write statement %+v", doc.Statement[0])
	}

	for name, mutate := range map[string]func(*fss3.DelegateOptions){
		"empty prefix":     func(o *fss3.DelegateOptions) { o.Prefix = "" },
		"wildcard":         func(o *fss3.DelegateOptions) { o.Prefix = "users/*" },
		"single wildcard":  func(o *fss3.DelegateOptions) { o.Prefix = "users/4?" },
		"policy variable":  func(o *fss3.DelegateOptions) { o.Prefix = "users/${aws:username}" },
		"traversal":        func(o *fss3.DelegateOptions) { o.Prefix = "users/../admin" },
		"trailing slash":   func(o *fss3.DelegateOptions) { o.Prefix = "users/42/" },
		"bucket with path": func(o *fss3.DelegateOptions) { o.Bucket = "media/users" },
		"too short":        func(o *fss3.DelegateOptions) { o.Duration = time.Minute },
		"too long":         func(o *fss3.DelegateOptions) { o.Duration = 13 * time.Hour },
		"no role":          func(o *fss3.DelegateOptions) { o.Role = "" },
		"no session name":  func(o *fss3.DelegateOptions) { o.SessionName = "" },
	} {
		bad := o
		mutate(&bad)
		if _, err := fss3.DelegatePrefix(ctx, f, bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("issued", "creds", creds)
	printed := fmt.Sprintf("%v %+v %#v %s", creds, creds, creds, creds) + logged.String() + fmt.Sprintf("%+v", struct{ C fss3.ScopedCredentials }{creds})
	for _, secret := range []string{"very-secret-key", "very-secret-token"} {
		if strings.Contains(printed, secret) {
			t.Errorf("printed credentials leak %q: %s", secret, printed)
		}
	}
	if !strings.Contains(printed, "ASIAEXAMPLE") || !strings.Contains(printed, "users/42") {
		t.Errorf("printed credentials lack the key ID or scope: %s", printed)
	}
}
