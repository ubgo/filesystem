package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/ubgo/filesystem/examples/frameworks/internal/app"
	"github.com/ubgo/filesystem/examples/frameworks/internal/apptest"
)

// TestRoutes drives every mounted endpoint through a real fiber server.
func TestRoutes(t *testing.T) {
	ln, base := apptest.Listen(t)
	a := apptest.NewApp(t, base)
	f := newRouter(a.Handlers)
	f.Get(app.PathInfo+"/*", fileInfo(a.Disks.Media))
	go func() { _ = f.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = f.Shutdown() })
	apptest.Exercise(t, base, a)
	apptest.ExerciseInfo(t, base, a)
	t.Run("upload over fiber's default body limit", func(t *testing.T) {
		big := strings.Repeat("a", fiber.DefaultBodyLimit+1)
		body, ct := apptest.Form(t, "big.txt", big)
		status, out, _ := apptest.Do(t, http.MethodPost, base+app.PathUpload, http.Header{"Content-Type": {ct}}, body)
		if status != http.StatusCreated {
			t.Errorf("upload of %d bytes = %d %s", len(big), status, out)
		}
	})
}
