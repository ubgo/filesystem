package main

import (
	"testing"

	"github.com/ubgo/filesystem/examples/frameworks/internal/apptest"
)

// TestRoutes drives every mounted endpoint through a real chi server.
func TestRoutes(t *testing.T) {
	ln, base := apptest.Listen(t)
	a := apptest.NewApp(t, base)
	apptest.Serve(t, ln, newRouter(a.Handlers))
	apptest.Exercise(t, base, a)
}
