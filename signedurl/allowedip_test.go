package signedurl_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

// TestAllowedIPIsSigned proves an IP-restricted URL carries its range under
// the signature: Verify returns it, and changing, widening or dropping it
// invalidates the URL. Through a Disk, the signer is accepted for
// AllowedIP because it reports RestrictsIP.
func TestAllowedIPIsSigned(t *testing.T) {
	at := now
	s := signer(t, &at, current)
	if !s.RestrictsIP() {
		t.Fatal("signer does not restrict IPs")
	}
	d, err := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(s))
	if err != nil {
		t.Fatal(err)
	}
	req, err := d.SignedURL(t.Context(), "a.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, AllowedIP: "203.0.113.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	disk, key, q := parse(t, req.URL)
	c, err := s.Verify(disk, key, filesystem.MethodGet, q)
	if err != nil || c.AllowedIP.String() != "203.0.113.0/24" {
		t.Fatalf("verify = %v, %v", c.AllowedIP, err)
	}
	for name, mutate := range map[string]func(url.Values){
		"widened":   func(v url.Values) { v.Set(signedurl.ParamAllowedIP, "0.0.0.0/0") },
		"moved":     func(v url.Values) { v.Set(signedurl.ParamAllowedIP, "198.51.100.0/24") },
		"dropped":   func(v url.Values) { v.Del(signedurl.ParamAllowedIP) },
		"malformed": func(v url.Values) { v.Set(signedurl.ParamAllowedIP, "203.0.113.1/24") },
	} {
		tampered := url.Values{}
		for k, v := range q {
			tampered[k] = append([]string(nil), v...)
		}
		mutate(tampered)
		if _, err := s.Verify(disk, key, filesystem.MethodGet, tampered); filesystem.CodeOf(err) != filesystem.CodePermission {
			t.Errorf("%s range: %v", name, err)
		}
	}
	// An unrestricted URL cannot be turned into a restricted one either:
	// adding a range changes what was signed.
	plain, err := d.SignedURL(t.Context(), "a.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, _, pq := parse(t, plain.URL)
	pq.Set(signedurl.ParamAllowedIP, "203.0.113.0/24")
	if _, err := s.Verify(disk, key, filesystem.MethodGet, pq); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("range added to an unrestricted URL: %v", err)
	}
}
