package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"op":"write","key":"reports/q3.txt","attempts":1,"bytes":18`,
		`"op":"stat","key":"reports/q3.txt","attempts":2`,
		`"target":"archive/q3.txt"`,
		`"code":"not_found"`,
		`"media.stat.ok": 1`,
		`"media.stat.not_found": 1`,
		`bytes_written {"media": 18}`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %s:\n%s", want, out.String())
		}
	}
	// An invalid key is refused before the interceptors run, so it is
	// neither logged nor counted.
	if strings.Contains(out.String(), `"op":"delete"`) {
		t.Errorf("invalid key reached the log:\n%s", out.String())
	}
}

func TestQuietLog(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	d, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithInterceptors(newQuietLog(logger)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	_, _ = d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	_, _ = d.Stat(ctx, "missing.txt")
	_ = d.SetVisibility(ctx, "missing.txt", filesystem.VisibilityPublic)
	got := out.String()
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "op=write") {
		t.Errorf("success not at Info:\n%s", got)
	}
	if strings.Contains(got, "op=stat") {
		t.Errorf("not-found lookup logged at Info:\n%s", got)
	}
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "op=set_visibility") {
		t.Errorf("failed mutation not at Warn:\n%s", got)
	}
}
