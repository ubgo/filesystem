package main

import (
	"testing"

	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
	"github.com/ubgo/filesystem/examples/frameworks/internal/apptest"
)

// TestRoutes drives every mounted endpoint through a real echo server.
func TestRoutes(t *testing.T) {
	ln, base := apptest.Listen(t)
	a := apptest.NewApp(t, base)
	e := newRouter(a.Handlers)
	e.GET(app.PathInfo+"/*", fileInfo(a.Disks.Media))
	apptest.Serve(t, ln, e)
	apptest.Exercise(t, base, a)
	apptest.ExerciseInfo(t, base, a)
}
