package main

import (
	"bytes"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"github.com/ubgo/filesystem"
	fsotel "github.com/ubgo/filesystem/contrib/otel"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"span filesystem.write",
		"span filesystem.stat ",
		"filesystem.error_code=not_found",
		"metric filesystem.bytes_written filesystem.disk=media = 18",
		"metric filesystem.operations filesystem.disk=media filesystem.operation=write filesystem.outcome=ok = 1",
		"metric filesystem.operation.duration filesystem.disk=media filesystem.error_code=not_found filesystem.operation=stat filesystem.outcome=error count=1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSpansCarryNoKeysUnlessAsked(t *testing.T) {
	tel := newTelemetry()
	d, err := newDisk(tel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(t.Context(), "users/42/secret.png"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatal(err)
	}
	span := tel.Spans.Ended()[0]
	if span.Status().Code != codes.Error {
		t.Errorf("status = %v", span.Status())
	}
	for _, kv := range span.Attributes() {
		if kv.Key == fsotel.AttrKey {
			t.Errorf("key recorded without WithKeys: %v", kv)
		}
	}
	tel = newTelemetry()
	d, err = newDiskWithKeys(tel)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.Stat(t.Context(), "users/42/secret.png")
	if !strings.Contains(attrs(tel.Spans.Ended()[0].Attributes()), "filesystem.key=users/42/secret.png") {
		t.Errorf("WithKeys did not record the key")
	}
}
