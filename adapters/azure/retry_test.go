package azure_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// TestRetryAfter proves a 429 or 503 carrying Retry-After reports the wait
// on the error, and that one without it reports none. The server answers
// every request with the failure, so no Azurite is needed.
func TestRetryAfter(t *testing.T) {
	for _, c := range []struct {
		status int
		header string
		want   time.Duration
		ok     bool
	}{
		{http.StatusServiceUnavailable, "4", 4 * time.Second, true},
		{http.StatusTooManyRequests, "1", time.Second, true},
		{http.StatusServiceUnavailable, "", 0, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if c.header != "" {
				w.Header().Set("Retry-After", c.header)
			}
			w.Header().Set("x-ms-error-code", "ServerBusy")
			w.WriteHeader(c.status)
			_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>ServerBusy</Code><Message>busy</Message></Error>`)
		}))
		cred, _ := azblob.NewSharedKeyCredential("acct", "a2V5")
		client, err := azblob.NewClientWithSharedKeyCredential(srv.URL+"/acct/", cred, &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
		if err != nil {
			t.Fatal(err)
		}
		drv, err := azure.New(azure.Config{Client: client, Container: "c"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = drv.Stat(t.Context(), "k")
		srv.Close()
		wantCode(t, err, filesystem.CodeUnavailable)
		if got, ok := filesystem.RetryAfterOf(err); ok != c.ok || got != c.want {
			t.Errorf("%d Retry-After %q => %v, %t; want %v", c.status, c.header, got, ok, c.want)
		}
	}
}
