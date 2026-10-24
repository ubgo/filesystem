package gcs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// bucketStore is a fake GCS JSON API for bucket metadata: a PATCH merges
// its fields into the bucket's document and a GET returns it, as GCS
// keeps bucket attributes, so the SDK's encoding and decoding both run.
// fake-gcs-server does not keep lifecycle rules or the replication mode,
// which is why these tests use this instead.
type bucketStore struct {
	mu   sync.Mutex
	docs map[string]map[string]json.RawMessage
}

func (b *bucketStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/storage/v1/b/")
	b.mu.Lock()
	defer b.mu.Unlock()
	doc, ok := b.docs[name]
	if !ok {
		doc = map[string]json.RawMessage{"name": json.RawMessage(`"` + name + `"`)}
		b.docs[name] = doc
	}
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
}

func bucketClient(t *testing.T) (*storage.Client, *bucketStore) {
	t.Helper()
	store := &bucketStore{docs: map[string]map[string]json.RawMessage{}}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)
	client, err := storage.NewClient(t.Context(), option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return client, store
}

// TestLifecycleReadBackNeverWidens proves a stored rule with any condition
// LifecycleRule cannot represent is an error naming it, never a rule with
// the condition dropped. The reviewer's case: "delete noncurrent versions
// older than 30 days" (isLive false) read back as "delete everything older
// than 30 days".
func TestLifecycleReadBackNeverWidens(t *testing.T) {
	ctx := t.Context()
	client, store := bucketClient(t)
	for name, condition := range map[string]string{
		"noncurrent only":   `{"age":30,"isLive":false}`,
		"live only":         `{"age":30,"isLive":true}`,
		"newer versions":    `{"age":30,"numNewerVersions":3}`,
		"storage classes":   `{"age":30,"matchesStorageClass":["STANDARD"]}`,
		"suffixes":          `{"age":30,"matchesSuffix":[".tmp"]}`,
		"created before":    `{"age":30,"createdBefore":"2026-01-01"}`,
		"noncurrent days":   `{"age":30,"daysSinceNoncurrentTime":7}`,
		"custom time":       `{"age":30,"daysSinceCustomTime":7}`,
		"noncurrent before": `{"age":30,"noncurrentTimeBefore":"2026-01-01"}`,
		"no age":            `{"matchesPrefix":["a/"]}`,
	} {
		store.mu.Lock()
		store.docs["b"] = map[string]json.RawMessage{"name": json.RawMessage(`"b"`), "lifecycle": json.RawMessage(
			`{"rule":[{"action":{"type":"Delete"},"condition":{"age":1,"matchesPrefix":["tmp/"]}},{"action":{"type":"Delete"},"condition":` + condition + `}]}`)}
		store.mu.Unlock()
		rules, err := gcs.Lifecycle(ctx, client, "b")
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported || !strings.Contains(err.Error(), "rule 1") {
			t.Errorf("%s: read back %+v, %v", name, rules, err)
		}
	}
}

// TestLifecycleRoundTrip proves SetLifecycle stores rules that Lifecycle
// reads back unchanged, and refuses rules GCS would reject or that would
// fire on every object.
func TestLifecycleRoundTrip(t *testing.T) {
	ctx := t.Context()
	client, _ := bucketClient(t)
	want := []gcs.LifecycleRule{
		{Action: gcs.LifecycleDelete, AgeDays: 1, Prefixes: []string{"tmp/"}},
		{Action: gcs.LifecycleSetStorageClass, StorageClass: "ARCHIVE", AgeDays: 90, Prefixes: []string{"logs/", "audit/"}},
		{Action: gcs.LifecycleAbortUploads, AgeDays: 7},
	}
	if err := gcs.SetLifecycle(ctx, client, "b", want...); err != nil {
		t.Fatal(err)
	}
	got, err := gcs.Lifecycle(ctx, client, "b")
	if err != nil || !slices.EqualFunc(got, want, func(a, b gcs.LifecycleRule) bool {
		return a.Action == b.Action && a.StorageClass == b.StorageClass && a.AgeDays == b.AgeDays && slices.Equal(a.Prefixes, b.Prefixes)
	}) {
		t.Fatalf("read back %+v, %v", got, err)
	}
	for _, bad := range [][]gcs.LifecycleRule{
		nil,
		{{Action: "Archive", AgeDays: 1}},
		{{Action: gcs.LifecycleDelete}},
		{{Action: gcs.LifecycleSetStorageClass, AgeDays: 1}},
		{{Action: gcs.LifecycleDelete, AgeDays: 1, StorageClass: "ARCHIVE"}},
	} {
		if err := gcs.SetLifecycle(ctx, client, "b", bad...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("SetLifecycle(%+v) = %v", bad, err)
		}
	}
}

// TestReplicationRoundTrip proves both modes are stored and read back,
// that a bucket never set reads as no mode, and that an unknown mode is
// refused.
func TestReplicationRoundTrip(t *testing.T) {
	ctx := t.Context()
	client, _ := bucketClient(t)
	if mode, err := gcs.Replication(ctx, client, "b"); err != nil || mode != "" {
		t.Fatalf("unset = %q, %v", mode, err)
	}
	for _, mode := range []gcs.ReplicationMode{gcs.ReplicationTurbo, gcs.ReplicationDefault} {
		if err := gcs.SetReplication(ctx, client, "b", mode); err != nil {
			t.Fatal(err)
		}
		if got, err := gcs.Replication(ctx, client, "b"); err != nil || got != mode {
			t.Errorf("set %q, read %q, %v", mode, got, err)
		}
	}
	if err := gcs.SetReplication(ctx, client, "b", "FAST"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("unknown mode: %v", err)
	}
}

// TestObjectACL proves SetObjectACL replaces an object's ACL on
// fake-gcs-server and ObjectACL reads it back, and that bad grants and
// unclean keys are refused before any request.
func TestObjectACL(t *testing.T) {
	ctx := t.Context()
	client, srv, _, _ := proxied(t)
	bucket := newBucket(t, srv)
	srv.CreateObject(fakestorage.Object{ObjectAttrs: fakestorage.ObjectAttrs{BucketName: bucket, Name: "docs/a.pdf"}, Content: []byte("pdf")})
	want := []gcs.ACLGrant{
		{Entity: "user-partner@example.com", Role: storage.RoleReader},
		{Entity: storage.AllAuthenticatedUsers, Role: storage.RoleReader},
		{Entity: "group-ops@example.com", Role: storage.RoleOwner},
	}
	if err := gcs.SetObjectACL(ctx, client, bucket, "docs/a.pdf", gcs.ObjectACL{Grants: want, AllowPublic: true}); err != nil {
		t.Fatal(err)
	}
	got, err := gcs.ObjectGrants(ctx, client, bucket, "docs/a.pdf")
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("read back %+v, %v", got, err)
	}
	for name, c := range map[string]struct {
		key    string
		grants []gcs.ACLGrant
	}{
		"no grants":   {"docs/a.pdf", nil},
		"no entity":   {"docs/a.pdf", []gcs.ACLGrant{{Role: storage.RoleReader}}},
		"writer role": {"docs/a.pdf", []gcs.ACLGrant{{Entity: storage.AllUsers, Role: storage.RoleWriter}}},
		"traversal":   {"../a.pdf", want},
		"unclean":     {"docs//a.pdf", want},
		"all users":   {"docs/a.pdf", []gcs.ACLGrant{{Entity: storage.AllUsers, Role: storage.RoleReader}}},
		"authn users": {"docs/a.pdf", []gcs.ACLGrant{{Entity: storage.AllAuthenticatedUsers, Role: storage.RoleReader}}},
	} {
		if err := gcs.SetObjectACL(ctx, client, bucket, c.key, gcs.ObjectACL{Grants: c.grants}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := gcs.ObjectGrants(ctx, client, bucket, "../a.pdf"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("ObjectACL on a traversal key: %v", err)
	}
}

// fakeSTS answers Google's token exchange, recording the access boundary
// it was asked for.
type fakeSTS struct {
	mu       sync.Mutex
	boundary string
}

func (f *fakeSTS) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	f.boundary = form.Get("options")
	f.mu.Unlock()
	resp := `{"access_token":"downscoped-secret-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(resp)), Request: r}, nil
}

// TestDelegatePrefix proves the access boundary sent to STS allows exactly
// the prefix (objects named prefix/… and listings under prefix/), that
// write access is opt-in, that prefixes able to break out of the
// condition are refused, and that the token never prints.
func TestDelegatePrefix(t *testing.T) {
	f := &fakeSTS{}
	ctx := t.Context()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: f})
	root := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "root-token", Expiry: time.Now().Add(time.Hour)})
	o := gcs.DelegateOptions{Bucket: "media", Prefix: "users/42"}
	tok, err := gcs.DelegatePrefix(ctx, root, o)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		AccessBoundary struct {
			AccessBoundaryRules []struct {
				AvailableResource     string
				AvailablePermissions  []string
				AvailabilityCondition struct {
					Expression string
				}
			}
		}
	}
	f.mu.Lock()
	boundary := f.boundary
	f.mu.Unlock()
	if err := json.Unmarshal([]byte(boundary), &sent); err != nil || len(sent.AccessBoundary.AccessBoundaryRules) != 1 {
		t.Fatalf("boundary %s (%v)", boundary, err)
	}
	rule := sent.AccessBoundary.AccessBoundaryRules[0]
	wantExpr := "resource.name.startsWith('projects/_/buckets/media/objects/users/42/') || api.getAttribute('storage.googleapis.com/objectListPrefix', '').startsWith('users/42/')"
	if rule.AvailableResource != "//storage.googleapis.com/projects/_/buckets/media" || !slices.Equal(rule.AvailablePermissions, []string{"inRole:roles/storage.objectViewer"}) || rule.AvailabilityCondition.Expression != wantExpr {
		t.Errorf("rule %+v", rule)
	}
	if tok.AccessToken != "downscoped-secret-token" || tok.Prefix != "users/42" || tok.Expires.IsZero() {
		t.Errorf("token %v", tok)
	}
	if got, _ := tok.TokenSource().Token(); got.AccessToken != tok.AccessToken || !got.Expiry.Equal(tok.Expires) {
		t.Errorf("token source %+v", got)
	}

	// No level ever keeps objectAdmin, which can change ACLs and IAM.
	for access, roles := range map[gcs.DelegateAccess][]string{
		gcs.DelegateRead:      {"inRole:roles/storage.objectViewer"},
		gcs.DelegateCreate:    {"inRole:roles/storage.objectViewer", "inRole:roles/storage.objectCreator"},
		gcs.DelegateReadWrite: {"inRole:roles/storage.objectUser"},
	} {
		w, err := gcs.PrefixBoundary(o.Bucket, o.Prefix, access)
		if err != nil || !slices.Equal(w.AvailablePermissions, roles) || w.Condition.Expression != wantExpr {
			t.Errorf("access %d boundary %+v, %v", access, w, err)
		}
	}
	if _, err := gcs.PrefixBoundary(o.Bucket, o.Prefix, gcs.DelegateReadWrite+1); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("unknown access level: %v", err)
	}
	o.Access = gcs.DelegateReadWrite
	for name, bad := range map[string]gcs.DelegateOptions{
		"empty prefix":    {Bucket: "media"},
		"quote":           {Bucket: "media", Prefix: "users/42') || true || ('"},
		"backslash":       {Bucket: "media", Prefix: `users\42`},
		"traversal":       {Bucket: "media", Prefix: "users/../admin"},
		"trailing slash":  {Bucket: "media", Prefix: "users/42/"},
		"bucket with key": {Bucket: "media/users", Prefix: "42"},
		"empty bucket":    {Prefix: "users/42"},
	} {
		if _, err := gcs.DelegatePrefix(ctx, root, bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := gcs.DelegatePrefix(ctx, nil, o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("nil root: %v", err)
	}

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("issued", "token", tok)
	printed := fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok) + logged.String() + fmt.Sprintf("%+v", struct{ T gcs.ScopedToken }{tok})
	if strings.Contains(printed, "downscoped-secret-token") || !strings.Contains(printed, "users/42") {
		t.Errorf("printed token: %s", printed)
	}
}
