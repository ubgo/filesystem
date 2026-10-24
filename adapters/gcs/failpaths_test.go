package gcs_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

// TestLockedRetentionMode proves a disk configured with RetentionLocked
// writes its RetainFor retention in that mode, not the Unlocked default.
func TestLockedRetentionMode(t *testing.T) {
	ctx := t.Context()
	drv, d, p := advanced(t, gcs.Config{RetainFor: time.Hour, RetentionMode: gcs.RetentionLocked}, false)
	if _, err := d.WriteBytes(ctx, "kept.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := drv.ObjectInfo(ctx, "kept.txt")
	if err != nil || info.RetentionMode != gcs.RetentionLocked {
		t.Fatalf("retention after a write with RetentionLocked: %+v, %v", info, err)
	}
	if b := strings.Join(p.bodies, ""); strings.Contains(b, `"mode":"Unlocked"`) {
		t.Errorf("a Locked disk sent an Unlocked retention: %s", b)
	}
}

// TestObjectReadFailures proves the reads in front of Hold, ObjectInfo,
// ObjectGrants, Versions, RestoreVersion and a compare-and-swap write
// report their failure with its code: a missing object as CodeNotFound, an
// unclean key as invalid, a server failure as CodeUnavailable, never as an
// empty answer or a write that skipped its precondition.
func TestObjectReadFailures(t *testing.T) {
	ctx := t.Context()
	drv, d, p := advanced(t, gcs.Config{Versioned: true}, true)
	_, err := d.Hold(ctx, "missing.txt")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = drv.ObjectInfo(ctx, "../escape.txt")
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = gcs.ObjectGrants(ctx, drv.Native(), "adv", "missing.txt")
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, d.RestoreVersion(ctx, "missing.txt", "not-a-generation"), filesystem.CodeNotFound)

	p.fail(http.MethodGet, "versions=true", http.StatusServiceUnavailable)
	_, err = d.Versions(ctx, "doc.txt")
	wantCode(t, err, filesystem.CodeUnavailable)

	e, err := d.WriteBytes(ctx, "swap.txt", []byte("one"), filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.fail(http.MethodGet, "/o/swap.txt", http.StatusServiceUnavailable)
	_, err = d.WriteBytes(ctx, "swap.txt", []byte("two"), filesystem.WriteOptions{IfMatch: e.ETag})
	wantCode(t, err, filesystem.CodeUnavailable)
	if b, err := d.ReadAll(ctx, "swap.txt"); err != nil || string(b) != "one" {
		t.Errorf("after the failed swap the object holds %q, %v", b, err)
	}
}

// TestBucketAttrsFailures proves Lifecycle and Replication report a
// refused bucket read as CodePermission rather than as no rules or no
// mode, which a caller could write back over the real configuration.
func TestBucketAttrsFailures(t *testing.T) {
	ctx := t.Context()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"denied"}}`))
	}))
	t.Cleanup(srv.Close)
	client, err := storage.NewClient(ctx, option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	rules, err := gcs.Lifecycle(ctx, client, "b")
	if filesystem.CodeOf(err) != filesystem.CodePermission || rules != nil {
		t.Errorf("Lifecycle = %+v, %v", rules, err)
	}
	mode, err := gcs.Replication(ctx, client, "b")
	if filesystem.CodeOf(err) != filesystem.CodePermission || mode != "" {
		t.Errorf("Replication = %q, %v", mode, err)
	}
}

// failingRoot is a root token source that cannot issue a token.
type failingRoot struct{}

func (failingRoot) Token() (*oauth2.Token, error) { return nil, errors.New("root credentials expired") }

// TestDelegatePrefixExchangeFails proves a root credential that cannot
// produce a token fails the delegation with CodePermission and issues no
// token, rather than a ScopedToken with an empty access token.
func TestDelegatePrefixExchangeFails(t *testing.T) {
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, &http.Client{Transport: &fakeSTS{}})
	tok, err := gcs.DelegatePrefix(ctx, failingRoot{}, gcs.DelegateOptions{Bucket: "media", Prefix: "users/42"})
	if filesystem.CodeOf(err) != filesystem.CodePermission || tok.AccessToken != "" || !strings.Contains(err.Error(), "root credentials expired") {
		t.Errorf("DelegatePrefix with a failing root = %v, %v", tok, err)
	}
}
