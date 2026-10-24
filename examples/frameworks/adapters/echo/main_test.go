package main

import (
	"testing"

	fsecho "github.com/ubgo/filesystem/contrib/echo"
	app "github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
	"github.com/ubgo/filesystem/examples/frameworks/internal/apptest"
)

// TestRoutes walks every route through a real echo server.
func TestRoutes(t *testing.T) {
	ln, base := apptest.Listen(t)
	a, err := app.New(t.Context(), base, fsecho.ScopeOf)
	if err != nil {
		t.Fatal(err)
	}
	apptest.Serve(t, ln, newRouter(a))
	apptest.ExerciseAdapters(t, base)
}
