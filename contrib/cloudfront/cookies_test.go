package cloudfront_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/cloudfront"
)

// TestCookieOptions pins the cookie seams: the default stays Path "/" and
// SameSite Lax; a chosen path and SameSite reach every cookie, which stay
// Secure and HttpOnly; a malformed path and the browser-default SameSite
// are refused at New.
func TestCookieOptions(t *testing.T) {
	check := func(s *cloudfront.Signer, path string, same http.SameSite) {
		t.Helper()
		cookies, _, err := s.SignCookies(context.Background(), "media", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cookies {
			if c.Path != path || c.SameSite != same || !c.Secure || !c.HttpOnly {
				t.Fatalf("cookie %s: path %q samesite %d secure %v httponly %v", c.Name, c.Path, c.SameSite, c.Secure, c.HttpOnly)
			}
		}
	}
	check(newSigner(t, nil), "/", http.SameSiteLaxMode)
	check(newSigner(t, func(c *cloudfront.Config) {
		c.CookiePath, c.CookieSameSite = "/media/", http.SameSiteNoneMode
	}), "/media/", http.SameSiteNoneMode)
	for name, mut := range map[string]func(*cloudfront.Config){
		"relative path":    func(c *cloudfront.Config) { c.CookiePath = "media" },
		"attribute inject": func(c *cloudfront.Config) { c.CookiePath = "/; Domain=evil.test" },
		"browser default":  func(c *cloudfront.Config) { c.CookieSameSite = http.SameSiteDefaultMode },
	} {
		cfg := cloudfront.Config{BaseURL: base, KeyPairID: keyPairID, PrivateKey: testKey}
		mut(&cfg)
		if _, err := cloudfront.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestCookiePathFor pins the per-call Path seam (R-I22): the function sees
// the cleaned prefix and its Path reaches every cookie, so two grants can
// coexist; a "" return keeps CookiePath; an error or an unsafe Path fails
// the call with no cookie; and without the function every call gets
// CookiePath as before.
func TestCookiePathFor(t *testing.T) {
	var asked []string
	s := newSigner(t, func(c *cloudfront.Config) {
		c.CookiePath = "/media/"
		c.CookiePathFor = func(prefix string) (string, error) {
			asked = append(asked, prefix)
			switch prefix {
			case "ws/a":
				return "/ws/a/", nil
			case "bad":
				return "/x; Domain=evil.test", nil
			case "boom":
				return "", errors.New("no path for this prefix")
			}
			return "", nil
		}
	})
	paths := func(prefix string) (string, error) {
		cookies, _, err := s.SignCookies(context.Background(), prefix, time.Hour)
		if err != nil {
			return "", err
		}
		for _, c := range cookies[1:] {
			if c.Path != cookies[0].Path {
				t.Fatalf("cookies of one grant disagree on Path: %q, %q", c.Path, cookies[0].Path)
			}
		}
		return cookies[0].Path, nil
	}
	if p, err := paths("/ws/a/"); err != nil || p != "/ws/a/" {
		t.Errorf("custom path %q, %v", p, err)
	}
	if p, err := paths("other"); err != nil || p != "/media/" {
		t.Errorf("a \"\" return gave %q, %v", p, err)
	}
	if _, err := paths("bad"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("an injected path: %v", err)
	}
	if cookies, _, err := s.SignCookies(context.Background(), "boom", time.Hour); err == nil || cookies != nil {
		t.Errorf("a CookiePathFor error issued %d cookies, err %v", len(cookies), err)
	}
	if len(asked) == 0 || asked[0] != "ws/a" {
		t.Errorf("CookiePathFor was asked %q, want the cleaned prefix first", asked)
	}
	plain := newSigner(t, func(c *cloudfront.Config) { c.CookiePath = "/media/" })
	cookies, _, err := plain.SignCookies(context.Background(), "ws/a", time.Hour)
	if err != nil || cookies[0].Path != "/media/" {
		t.Errorf("without CookiePathFor: %v", err)
	}
}
