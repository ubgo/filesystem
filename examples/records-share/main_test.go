package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records/share"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	gone := "filesystem: not_found: share: link not found or no longer valid"
	for _, want := range []string{
		"token 43 chars, expires in 168h0m0s, 2 downloads",
		`download 1: "contract.txt"`, `download 2: "contract.txt"`, "download 3: not_found",
		"revoked: " + gone, "unknown: " + gone, "file trashed: " + gone,
		`links of the file: 3 (revoked: true, token listed: "")`, "links after purge: 0",
		"password link without a password: permission (true)", "with a wrong one: permission", "with the right one: contract.txt",
		"folder link lists 1 file(s)", "request link: receipt.txt by share:… into Inbox true",
		"second upload past its limit: not_found", "listing through the request link: not_found",
		`folder link stats: 1 use(s), last by "user:carol"`,
		"policy: link opened from another organisation: 403",
		"policy: link opened by a member: 200 <h1>invoice.txt</h1>",
		`policy: password link: 401 <form method="post">`,
		"policy: 12-byte upload through the request link: 413",
		"policy: 5-byte upload through the request link: 201",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestHandlerRedirectsUntilExhausted(t *testing.T) {
	a, err := newApp(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := uploadDoc(t.Context(), a.store, "a.txt", "hello")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := shareFile(t.Context(), a.store, a.share, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMux(a.store, a.share, a.disks, a.signer))
	defer srv.Close()
	for i, want := range []int{http.StatusFound, http.StatusFound, http.StatusNotFound} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+sharePrefix+sh.Token, nil)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("request %d = %d, want %d", i+1, resp.StatusCode, want)
		}
		if want == http.StatusFound && !strings.Contains(resp.Header.Get("Location"), "cd=attachment") {
			t.Fatalf("redirect = %q", resp.Header.Get("Location"))
		}
	}
}

func TestExpiredLinkIsGone(t *testing.T) {
	now := time.Now()
	a, err := newApp(t.Context(), t.TempDir(), share.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	f, err := uploadDoc(t.Context(), a.store, "a.txt", "hello")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := shareFile(t.Context(), a.store, a.share, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(linkTTL)
	if _, err := open(t.Context(), a.store, a.share, sh.Token); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("expired link = %v", err)
	}
}

func TestOnlyTheHashIsStored(t *testing.T) {
	a, err := newApp(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := uploadDoc(t.Context(), a.store, "a.txt", "hello")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := shareFile(t.Context(), a.store, a.share, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	row := a.db.QueryRowContext(t.Context(), "SELECT "+share.ColTokenHash+" FROM "+share.TableNameShares)
	if err := row.Scan(&stored); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(sh.Token))
	if stored != hex.EncodeToString(sum[:]) {
		t.Fatalf("stored %q, want the token's SHA-256", stored)
	}
}

func TestLifetimeIsCapped(t *testing.T) {
	a, err := newApp(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := uploadDoc(t.Context(), a.store, "a.txt", "hello")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.share.Create(t.Context(), a.store, tenant, f.ID, share.Options{TTL: share.MaxLifetime + time.Hour})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Create past MaxLifetime = %v", err)
	}
}

// TestAccessCheckSpendsNothing pins that a visitor the access check
// refuses spends none of the link's downloads: after two refusals a
// member still gets both.
func TestAccessCheckSpendsNothing(t *testing.T) {
	a, err := newApp(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.db.Close() })
	h := newPolicyHandler(a.store, a.share)
	f, err := uploadDoc(t.Context(), a.store, "a.txt", "x")
	if err != nil {
		t.Fatal(err)
	}
	link, err := shareFile(t.Context(), a.store, a.share, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, _ := visit(h, http.MethodGet, link.Token, "globex", nil, ""); code != http.StatusForbidden {
			t.Fatalf("outsider = %d, want 403", code)
		}
	}
	for i := range linkDownloads {
		if code, _ := visit(h, http.MethodGet, link.Token, tenant, nil, ""); code != http.StatusOK {
			t.Fatalf("member download %d = %d, want 200", i+1, code)
		}
	}
	if code, _ := visit(h, http.MethodGet, link.Token, tenant, nil, ""); code != http.StatusNotFound {
		t.Fatalf("download past the limit = %d, want 404", code)
	}
}
