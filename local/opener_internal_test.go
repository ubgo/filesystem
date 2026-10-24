package local

import (
	"net/url"
	"testing"

	"github.com/ubgo/filesystem"
)

// TestDSNDirDriveLetters pins how a DSN becomes a directory on each OS,
// so the Windows spelling is checked on every machine that runs the tests.
func TestDSNDirDriveLetters(t *testing.T) {
	cases := []struct {
		dsn, goos, want string
	}{
		{"file:///C:/app/uploads", goosWindows, "C:/app/uploads"},
		{"file:///d:/data", goosWindows, "d:/data"},
		{"file:///C:", goosWindows, "C:/"},
		{"file:C:", goosWindows, "C:/"},
		{"file:///C:", "linux", "/C:"},
		{"file:C:/app/uploads", goosWindows, "C:/app/uploads"},
		{"file:///C%3A/app/my%20uploads", goosWindows, "C:/app/my uploads"},
		{"file:uploads/relative", goosWindows, "uploads/relative"},
		{"file:///var/app", goosWindows, "/var/app"},
		{"file:///1:/x", goosWindows, "/1:/x"},
		{"file:///C:/app/uploads", "linux", "/C:/app/uploads"},
		{"file:///var/app", "linux", "/var/app"},
		{"file:///var/app", "darwin", "/var/app"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.dsn)
		if err != nil {
			t.Fatalf("parse %q: %v", c.dsn, err)
		}
		got, err := dsnDir(u, c.goos)
		if err != nil || got != c.want {
			t.Errorf("dsnDir(%q, %s) = %q, %v; want %q", c.dsn, c.goos, got, err, c.want)
		}
	}
	// The drive in the host position, and UNC paths, are refused.
	for _, dsn := range []string{"file://C:/app", "file://server/share/x", "file:"} {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse %q: %v", dsn, err)
		}
		if _, err := dsnDir(u, goosWindows); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("dsnDir(%q) = %v, want invalid_argument", dsn, err)
		}
	}
}
