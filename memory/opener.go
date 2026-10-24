package memory

import (
	"context"
	"net/url"

	"github.com/ubgo/filesystem"
)

// Scheme is the DSN scheme of the memory driver: "mem://". The host part is
// ignored, so "mem://cache" is a readable way to name one.
const Scheme = "mem"

// Opener opens a fresh memory driver for every DSN. It accepts no
// parameters beyond the Mux's own.
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u); err != nil {
		return nil, err
	}
	return New(), nil
}
