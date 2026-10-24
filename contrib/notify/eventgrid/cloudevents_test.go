package eventgrid_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem/contrib/notify"
	"github.com/ubgo/filesystem/contrib/notify/eventgrid"
)

const cloudBatch = `[
 {"specversion":"1.0","id":"c1","source":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","type":"Microsoft.Storage.BlobCreated","subject":"/blobServices/default/containers/uploads/blobs/a.png","time":"2026-10-07T09:00:01Z","data":{"contentLength":7}},
 {"specversion":"1.0","id":"c2","source":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","type":"Microsoft.Storage.BlobTierChanged","subject":"/blobServices/default/containers/uploads/blobs/b.png","time":"2026-10-07T09:00:02Z","data":{}},
 {"specversion":"1.0","id":"c3","source":"/x","type":"Microsoft.EventGrid.SubscriptionValidationEvent","subject":"","data":{"validationCode":"forged"}}
]`

// TestCloudEvents pins the schema and kind seams: a CloudEvents batch
// (and a single event) is decoded with its type in Name, the OPTIONS
// handshake is answered only with the secret, a validation event in that
// schema is never echoed, PassOther passes the tier change, and the
// default Event Grid schema still refuses a CloudEvents body.
func TestCloudEvents(t *testing.T) {
	var got []notify.Event
	wh, err := eventgrid.Handler(eventgrid.Options{
		Authorize: eventgrid.QuerySecret("key", secret), Buckets: notify.Buckets{"acct1/uploads"},
		Handler:     func(_ context.Context, e notify.Event) error { got = append(got, e); return nil },
		CloudEvents: true, PassOther: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(wh)
	defer srv.Close()
	code, body := post(t, srv.URL+"?key="+secret, cloudBatch)
	if code != http.StatusOK || body != "" || len(got) != 2 || got[0].Kind != notify.KindCreated || got[0].Size != 7 || got[0].Account != "acct1" ||
		got[1].Kind != notify.KindOther || got[1].Name != "Microsoft.Storage.BlobTierChanged" {
		t.Fatalf("batch = %d %q, events %+v", code, body, got)
	}
	got = nil
	if code, _ := post(t, srv.URL+"?key="+secret, `{"id":"c9","source":"/subscriptions/s/resourceGroups/g/providers/Microsoft.Storage/storageAccounts/acct1","type":"Microsoft.Storage.BlobDeleted","subject":"/blobServices/default/containers/uploads/blobs/z"}`); code != http.StatusOK || len(got) != 1 || got[0].Kind != notify.KindDeleted {
		t.Fatalf("single = %d %+v", code, got)
	}
	handshake := func(url string) *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodOptions, url, nil)
		req.Header.Set("WebHook-Request-Origin", "eventgrid.azure.net")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	if r := handshake(srv.URL + "?key=" + secret); r.StatusCode != http.StatusOK || r.Header.Get("WebHook-Allowed-Origin") != "eventgrid.azure.net" {
		t.Fatalf("handshake = %d %v", r.StatusCode, r.Header)
	}
	if r := handshake(srv.URL); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("handshake without the secret = %d", r.StatusCode)
	}
	noOrigin, _ := http.NewRequestWithContext(t.Context(), http.MethodOptions, srv.URL+"?key="+secret, nil)
	resp, err := http.DefaultClient.Do(noOrigin)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("WebHook-Allowed-Origin") != "" {
		t.Fatalf("handshake without an origin = %d %v", resp.StatusCode, resp.Header)
	}
	got = nil
	if code, _ := post(t, srv.URL+"?key="+secret, `"neither an event nor a batch"`); code != http.StatusBadRequest || len(got) != 0 {
		t.Fatalf("a body that is no CloudEvent = %d, events %+v", code, got)
	}

	var rejected []notify.Event
	def := newServer(t, func(context.Context, notify.Event) error { return nil }, &rejected)
	if code, _ := post(t, def.URL+"?key="+secret, `{"id":"c9","type":"Microsoft.Storage.BlobDeleted"}`); code != http.StatusBadRequest {
		t.Fatalf("the default schema took a CloudEvent: %d", code)
	}
}
