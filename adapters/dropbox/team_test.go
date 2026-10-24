package dropbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/dropbox"
)

// lastHeaders is the headers of the fake's latest request.
func lastHeaders(f *fake) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[len(f.headers)-1]
}

// TestPathRootAndSelectUser proves the path-root and select-user headers
// go on API and content requests, that the default sends neither, and
// that both namespace fields together are refused.
func TestPathRootAndSelectUser(t *testing.T) {
	ctx := t.Context()
	f := newFake(t)
	cfg := config(f)
	cfg.RootNamespaceID, cfg.SelectUser = "1234", "dbmid:member"
	d, _ := filesystem.NewDisk("dropbox", newDriver(t, cfg))
	for _, step := range []func() error{
		func() error { _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); return err },
		func() error { _, err := d.Stat(ctx, "k"); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
		h := lastHeaders(f)
		var root map[string]string
		if err := json.Unmarshal([]byte(h.Get("Dropbox-API-Path-Root")), &root); err != nil || root[".tag"] != "root" || root["root"] != "1234" || h.Get("Dropbox-API-Select-User") != "dbmid:member" {
			t.Fatalf("headers = %v", h)
		}
	}
	cfg = config(f)
	cfg.NamespaceID = "77"
	ns, _ := filesystem.NewDisk("dropbox", newDriver(t, cfg))
	if _, err := ns.Stat(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeNotFound && err != nil {
		t.Fatal(err)
	}
	if got := lastHeaders(f).Get("Dropbox-API-Path-Root"); got != `{".tag":"namespace_id","namespace_id":"77"}` {
		t.Errorf("namespace path root = %s", got)
	}
	plain, _ := filesystem.NewDisk("dropbox", newDriver(t, config(f)))
	_, _ = plain.Stat(ctx, "k")
	if h := lastHeaders(f); h.Get("Dropbox-API-Path-Root") != "" || h.Get("Dropbox-API-Select-User") != "" {
		t.Errorf("default sent %v", h)
	}
	cfg = config(f)
	cfg.RootNamespaceID, cfg.NamespaceID = "1", "2"
	if _, err := dropbox.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("both namespaces: %v", err)
	}
}

// TestAuthorizedClient proves the client adds the token and the
// configured headers for the Dropbox hosts only, keeps a caller's own
// Authorization, and that Native still adds nothing.
func TestAuthorizedClient(t *testing.T) {
	f := newFake(t)
	cfg := config(f)
	cfg.SelectUser = "dbmid:member"
	drv := newDriver(t, cfg)
	c := drv.AuthorizedClient()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, f.URL+"/2/users/get_current_account", strings.NewReader("null"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if h := lastHeaders(f); h.Get("Authorization") != "Bearer "+testToken || h.Get("Dropbox-API-Select-User") != "dbmid:member" {
		t.Fatalf("authorized request headers = %v", h)
	}
	other := newFake(t) // another host: nothing may be added
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, other.URL+"/2/x", nil)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if h := lastHeaders(other); h.Get("Authorization") != "" || h.Get("Dropbox-API-Select-User") != "" {
		t.Fatalf("token sent to another host: %v", h)
	}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, f.URL+"/2/x", nil)
	req.Header.Set("Authorization", "Bearer mine")
	if resp, err = c.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	if got := lastHeaders(f).Get("Authorization"); got != "Bearer mine" {
		t.Errorf("caller's Authorization replaced by %q", got)
	}
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, f.URL+"/2/x", nil)
	if resp, err = drv.Native().Do(req); err == nil {
		_ = resp.Body.Close()
	}
	if got := lastHeaders(f).Get("Authorization"); got != "" {
		t.Errorf("Native added Authorization %q", got)
	}
}

// TestAuthorizedClientTokenFailure pins that a failing token source fails
// the request as CodePermission, without sending it, and closes its body.
func TestAuthorizedClientTokenFailure(t *testing.T) {
	f := newFake(t)
	cfg := config(f)
	cfg.Token = dropbox.TokenFunc(func(context.Context) (string, error) { return "", errTokenDown })
	c := newDriver(t, cfg).AuthorizedClient()
	body := &closeRecorder{Reader: strings.NewReader("null")}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, f.URL+"/2/users/get_current_account", body)
	resp, err := c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request sent without a token")
	}
	if filesystem.CodeOf(err) != filesystem.CodePermission || !body.closed {
		t.Fatalf("err = %v (code %s), body closed %t", err, filesystem.CodeOf(err), body.closed)
	}
}

var errTokenDown = errors.New("token service down")

// closeRecorder is a request body that records its Close.
type closeRecorder struct {
	*strings.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }
