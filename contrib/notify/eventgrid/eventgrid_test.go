package eventgrid_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/notify"
	"github.com/ubgo/filesystem/contrib/notify/eventgrid"
)

const secret = "s3cr3t-value"

func newServer(t *testing.T, h notify.Handler, rejected *[]notify.Event) *httptest.Server {
	t.Helper()
	wh, err := eventgrid.Handler(eventgrid.Options{
		Authorize:  eventgrid.QuerySecret("key", secret),
		Buckets:    notify.Buckets{"acct1/uploads"},
		Handler:    h,
		OnRejected: func(e notify.Event) { *rejected = append(*rejected, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(wh)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const validation = `[{"id":"v1","eventType":"Microsoft.EventGrid.SubscriptionValidationEvent","subject":"","eventTime":"2026-10-07T09:00:00Z","data":{"validationCode":"512d38b6-c7b8-40c8-89fe-f46f9e9622b6"}}]`

const blobs = `[
 {"id":"e1","topic":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","eventType":"Microsoft.Storage.BlobCreated","subject":"/blobServices/default/containers/uploads/blobs/pending/a b.png","eventTime":"2026-10-07T09:00:01Z","data":{"contentLength":42,"eTag":"0x8D"}},
 {"id":"e2","topic":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","eventType":"Microsoft.Storage.BlobDeleted","subject":"/blobServices/default/containers/uploads/blobs/old.txt","eventTime":"2026-10-07T09:00:02Z","data":{}},
 {"id":"e3","topic":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","eventType":"Microsoft.Storage.BlobCreated","subject":"/blobServices/default/containers/other/blobs/x","eventTime":"2026-10-07T09:00:03Z","data":{}},
 {"id":"e4","topic":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","eventType":"Microsoft.Storage.BlobTierChanged","subject":"/blobServices/default/containers/uploads/blobs/y","eventTime":"2026-10-07T09:00:04Z","data":{}}
]`

func TestValidationHandshake(t *testing.T) {
	var rejected []notify.Event
	srv := newServer(t, func(context.Context, notify.Event) error { return nil }, &rejected)
	code, body := post(t, srv.URL+"?key="+secret, validation)
	if code != http.StatusOK || !strings.Contains(body, `"validationResponse":"512d38b6-c7b8-40c8-89fe-f46f9e9622b6"`) {
		t.Fatalf("handshake = %d %s", code, body)
	}
	// The handshake needs the secret too: nobody else can subscribe the
	// endpoint.
	if code, _ := post(t, srv.URL, validation); code != http.StatusUnauthorized {
		t.Fatalf("handshake without the secret = %d", code)
	}
}

func TestBlobEvents(t *testing.T) {
	var got, rejected []notify.Event
	srv := newServer(t, func(_ context.Context, e notify.Event) error { got = append(got, e); return nil }, &rejected)

	for _, url := range []string{srv.URL, srv.URL + "?key=wrong", srv.URL + "?key=" + secret + "x"} {
		if code, _ := post(t, url, blobs); code != http.StatusUnauthorized {
			t.Fatalf("forged delivery to %s = %d", url, code)
		}
	}
	if len(got) != 0 {
		t.Fatalf("a refused delivery reached the handler: %+v", got)
	}

	if code, body := post(t, srv.URL+"?key="+secret, blobs); code != http.StatusOK {
		t.Fatalf("delivery = %d %s", code, body)
	}
	if len(got) != 2 || got[0].Kind != notify.KindCreated || got[0].Key != "pending/a b.png" || got[0].Bucket != "uploads" || got[0].Size != 42 || got[0].Provider != notify.ProviderAzure ||
		got[1].Kind != notify.KindDeleted || got[1].Key != "old.txt" {
		t.Fatalf("events %+v", got)
	}
	if len(rejected) != 1 || rejected[0].Bucket != "other" || got[0].Account != "acct1" || got[0].ID != notify.EventID("acct1", "uploads", "pending/a b.png", "e1") {
		t.Fatalf("rejected %+v", rejected)
	}
}

func TestHandlerErrorAsksForRedelivery(t *testing.T) {
	var rejected []notify.Event
	srv := newServer(t, func(context.Context, notify.Event) error { return errors.New("database down") }, &rejected)
	if code, _ := post(t, srv.URL+"?key="+secret, blobs); code != http.StatusInternalServerError {
		t.Fatalf("failing handler = %d, want 500 so Event Grid retries", code)
	}
}

func TestRequestLimits(t *testing.T) {
	var rejected []notify.Event
	srv := newServer(t, func(context.Context, notify.Event) error { return nil }, &rejected)
	if code, _ := post(t, srv.URL+"?key="+secret, "["+strings.Repeat(" ", eventgrid.DefaultMaxBodyBytes)+"]"); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d", code)
	}
	if code, _ := post(t, srv.URL+"?key="+secret, "{not json"); code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", code)
	}
	resp, err := http.Get(srv.URL + "?key=" + secret)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
}

func TestOptionsRequired(t *testing.T) {
	h := func(context.Context, notify.Event) error { return nil }
	auth := eventgrid.HeaderSecret("X-Secret", secret)
	for name, o := range map[string]eventgrid.Options{
		"no authorize": {Buckets: notify.Buckets{"b"}, Handler: h},
		"no buckets":   {Authorize: auth, Handler: h},
		"no handler":   {Authorize: auth, Buckets: notify.Buckets{"b"}},
	} {
		if _, err := eventgrid.Handler(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// HeaderSecret, and an empty secret never matches.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Secret", secret)
	if auth(r) != nil || eventgrid.HeaderSecret("X-Secret", "")(httptest.NewRequest(http.MethodPost, "/", nil)) == nil {
		t.Fatal("HeaderSecret")
	}
}

// TestSameContainerInAnotherAccount: the allowlist names the account, so
// a container called "uploads" in another storage account is rejected.
func TestSameContainerInAnotherAccount(t *testing.T) {
	var got, rejected []notify.Event
	srv := newServer(t, func(_ context.Context, e notify.Event) error { got = append(got, e); return nil }, &rejected)
	other := strings.ReplaceAll(blobs, "storageAccounts/acct1", "storageAccounts/attacker")
	noTopic := strings.ReplaceAll(blobs, `"topic":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1",`, "")
	for _, body := range []string{other, noTopic} {
		if code, _ := post(t, srv.URL+"?key="+secret, body); code != http.StatusOK {
			t.Fatalf("delivery = %d", code)
		}
	}
	if len(got) != 0 || len(rejected) != 6 {
		t.Fatalf("handled %+v, rejected %d", got, len(rejected))
	}
}

// TestMalformedEvents pins that a validation event without a code is
// refused (400, so no subscription is confirmed) and that blob events
// whose subject is not a blob path, or names no container or no blob,
// are skipped rather than delivered with an empty key.
func TestMalformedEvents(t *testing.T) {
	var got, rejected []notify.Event
	srv := newServer(t, func(_ context.Context, e notify.Event) error { got = append(got, e); return nil }, &rejected)
	noCode := `[{"id":"v1","eventType":"Microsoft.EventGrid.SubscriptionValidationEvent","data":{}}]`
	if code, _ := post(t, srv.URL+"?key="+secret, noCode); code != http.StatusBadRequest {
		t.Fatalf("validation without a code = %d, want 400", code)
	}
	const topic = `"topic":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","eventType":"Microsoft.Storage.BlobCreated"`
	bad := `[` +
		`{"id":"m1",` + topic + `,"subject":"/queueServices/default/queues/q"},` +
		`{"id":"m2",` + topic + `,"subject":"/blobServices/default/containers/uploads"},` +
		`{"id":"m3",` + topic + `,"subject":"/blobServices/default/containers//blobs/x"},` +
		`{"id":"m4",` + topic + `,"subject":"/blobServices/default/containers/uploads/blobs/"}` +
		`]`
	if code, body := post(t, srv.URL+"?key="+secret, bad); code != http.StatusOK {
		t.Fatalf("delivery = %d %s", code, body)
	}
	if len(got) != 0 || len(rejected) != 0 {
		t.Fatalf("malformed subjects delivered %+v, rejected %+v", got, rejected)
	}
}
