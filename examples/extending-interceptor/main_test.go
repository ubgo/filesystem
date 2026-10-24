package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"write legal/contract.pdf: filesystem: write media:legal/contract.pdf: read_only: legal/ is frozen",
		"write drafts/contract.txt: <nil>",
		"move into legal/: filesystem: move media:drafts/contract.txt: read_only: legal/ is frozen",
		"after 69 bytes: filesystem: write media:bulk/4.txt: quota_exceeded: disk media used its 64 bytes",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestFreezeCoversBatchesAndSignedUploads pins that Freeze refuses every
// path that changes a frozen key, not only Write and Delete: a batch
// delete, a signed PUT and a prefix delete all come back CodeReadOnly,
// while a signed GET still works.
// ds:def id=testfreezecoversbatchesandsigneduploads-3sahqzwj
func TestFreezeCoversBatchesAndSignedUploads(t *testing.T) {
	ctx := t.Context()
	d, err := filesystem.NewDisk("d", memory.New(), filesystem.WithInterceptors(Freeze(frozenDir)), filesystem.WithURLSigner(signer{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteKeys(ctx, []string{"a.txt", "legal/b.txt"}); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("batch delete = %v", err)
	}
	if _, err := d.SignedURL(ctx, "legal/b.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("signed PUT = %v", err)
	}
	if _, err := d.SignedURL(ctx, "legal/b.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err != nil {
		t.Errorf("signed GET = %v", err)
	}
	if err := d.DeletePrefix(ctx, "legal"); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("delete prefix = %v", err)
	}
}

type signer struct{}

func (signer) SignURL(_ context.Context, _, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: o.Method, URL: "https://app.test/" + key}, nil
}

// TestReportSlow drives ReportSlow with a clock that advances one second
// per reading, so every operation takes exactly one second and the
// threshold decides deterministically which ones are reported.
// ds:def id=testreportslow-v2j3tvmf
func TestReportSlow(t *testing.T) {
	clock := time.Unix(0, 0)
	now := func() time.Time { clock = clock.Add(time.Second); return clock }
	var got []SlowOp
	d, err := filesystem.NewDisk("d", memory.New(), filesystem.WithInterceptors(ReportSlow(slowThreshold, now, func(s SlowOp) { got = append(got, s) })))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.Stat(t.Context(), "k")
	if len(got) != 1 || got[0].Kind != filesystem.OpStat || got[0].Attempts != 1 || got[0].Took != time.Second {
		t.Errorf("reports = %+v", got)
	}
}
