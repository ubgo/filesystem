package ftp_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
)

// TestConformanceWithoutMLST runs the suite against a server without MLST
// and MLSD (RFC 3659), as many older servers are: Stat falls back to SIZE
// and MDTM, type checks to SIZE then CWD, and listings to LIST.
func TestConformanceWithoutMLST(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		m, addr := startServer(t, nil)
		m.settings.DisableMLST, m.settings.DisableMLSD = true, true
		d, err := filesystem.NewDisk("ftp", newDriver(t, config(addr)))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}
