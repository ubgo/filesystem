package main

import (
	"testing"

	fsgin "github.com/ubgo/filesystem/contrib/gin"
	app "github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
	"github.com/ubgo/filesystem/examples/frameworks/internal/apptest"
)

// TestRoutes walks every route through a real gin server.
func TestRoutes(t *testing.T) {
	ln, base := apptest.Listen(t)
	a, err := app.New(t.Context(), base, fsgin.ScopeOf)
	if err != nil {
		t.Fatal(err)
	}
	apptest.Serve(t, ln, newRouter(a))
	apptest.ExerciseAdapters(t, base)
}
