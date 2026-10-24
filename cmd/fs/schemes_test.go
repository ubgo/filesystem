package main

import (
	"strings"
	"testing"
)

// TestEverySchemeIsRegistered: each scheme the usage text names reaches
// its adapter's opener. Port 1 refuses at once, so an opener that dials
// fails for the connection, never for "no opener registered".
func TestEverySchemeIsRegistered(t *testing.T) {
	for _, dsn := range []string{
		"ftp://127.0.0.1:1/", "ftps://127.0.0.1:1/", "ftps-implicit://127.0.0.1:1/",
		"webdav://127.0.0.1:1/", "webdavs://127.0.0.1:1/",
		"cloudinary://key@cloud?api_secret_env=FS_TEST_UNSET_SECRET", "vercelblob://public?token_env=FS_TEST_UNSET_TOKEN",
	} {
		d, err := newMux().Open(t.Context(), "cli", dsn)
		if err == nil {
			_ = d.Close()
			continue
		}
		if strings.Contains(err.Error(), "no opener registered") {
			t.Errorf("%s: %v", dsn, err)
		}
	}
}
