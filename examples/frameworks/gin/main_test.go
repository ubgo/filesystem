package main

import (
	"testing"

	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
	"github.com/ubgo/filesystem/examples/frameworks/internal/apptest"
)

// TestRoutes drives every mounted endpoint through a real gin server.
func TestRoutes(t *testing.T) {
	ln, base := apptest.Listen(t)
	a := apptest.NewApp(t, base)
	r := newRouter(a.Handlers)
	r.GET(app.PathInfo+"/*key", fileInfo(a.Disks.Media))
	apptest.Serve(t, ln, r)
	apptest.Exercise(t, base, a)
	apptest.ExerciseInfo(t, base, a)
}
