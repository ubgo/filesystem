package main

import (
	"testing"

	"github.com/gofiber/fiber/v3"

	fsfiber "github.com/ubgo/filesystem/contrib/fiber"
	app "github.com/ubgo/filesystem/examples/frameworks/internal/adapterapp"
	"github.com/ubgo/filesystem/examples/frameworks/internal/apptest"
)

// TestRoutes walks every route through a real fiber server.
func TestRoutes(t *testing.T) {
	ln, base := apptest.Listen(t)
	a, err := app.New(t.Context(), base, fsfiber.ScopeOf)
	if err != nil {
		t.Fatal(err)
	}
	f := newApp(a)
	go func() { _ = f.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = f.Shutdown() })
	apptest.ExerciseAdapters(t, base)
}
