package azure_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// TestSetCORS sets the recommended direct-upload rule on Azurite, reads it
// back, and checks a browser's preflight for a SAS upload is allowed from
// that origin only.
func TestSetCORS(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	for _, bad := range [][]azure.CORSRule{nil, {{AllowedMethods: []string{"GET"}}}, {{AllowedOrigins: []string{"https://x"}}}} {
		if err := azure.SetCORS(ctx, client, bad...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("SetCORS(%v) = %v", bad, err)
		}
	}
	const origin = "https://app.example.com"
	if err := azure.SetCORS(ctx, client, azure.DirectUploadCORS(origin)); err != nil {
		t.Fatal(err)
	}
	props, err := client.ServiceClient().GetProperties(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(props.CORS) != 1 || *props.CORS[0].AllowedOrigins != origin || !strings.Contains(*props.CORS[0].AllowedMethods, "PUT") || *props.CORS[0].MaxAgeInSeconds != int32(time.Hour/time.Second) {
		t.Fatalf("stored CORS = %+v", props.CORS)
	}
	d := newDisk(t, client, cred)
	put, err := d.SignedURL(ctx, "cors.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	preflight := func(from string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodOptions, put.URL, nil)
		req.Header.Set("Origin", from)
		req.Header.Set("Access-Control-Request-Method", http.MethodPut)
		req.Header.Set("Access-Control-Request-Headers", "content-type,x-ms-blob-type")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.Header.Get("Access-Control-Allow-Origin") == "" {
			return 0
		}
		return resp.StatusCode
	}
	if got := preflight(origin); got != http.StatusOK {
		t.Errorf("preflight from %s = %d", origin, got)
	}
	if got := preflight("https://evil.example.net"); got == http.StatusOK {
		t.Error("preflight from another origin allowed")
	}
}
