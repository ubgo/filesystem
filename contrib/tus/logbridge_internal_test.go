package tus

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	xslog "golang.org/x/exp/slog"
)

type secret string

func (secret) LogValue() xslog.Value { return xslog.StringValue("redacted") }

// TestLogBridge proves the x/exp to stdlib conversion keeps levels,
// attributes, groups and LogValuers.
func TestLogBridge(t *testing.T) {
	var buf bytes.Buffer
	std := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	l := tusdLogger(std).With("id", "u1").WithGroup("req")
	l.Info("dropped")
	l.Warn("kept", "token", secret("hunter2"), xslog.Group("peer", "host", "a"))
	out := buf.String()
	for _, want := range []string{"msg=kept", "id=u1", "req.token=redacted", "req.peer.host=a"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "dropped") || strings.Contains(out, "hunter2") {
		t.Errorf("log %q", out)
	}
	if !l.Enabled(context.Background(), xslog.LevelError) || l.Enabled(context.Background(), xslog.LevelInfo) {
		t.Error("levels not passed through")
	}
}
