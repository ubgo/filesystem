package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`upload: key="" err=filesystem: rejected: scan: the file is infected (Eicar-Test-Signature)`,
		"scanner down, fail closed: filesystem: unavailable: scanner answered 404 Not Found",
		"alert: unscanned upload",
		"scanner down, fail open: stored=true err=<nil>",
		"hash list: 1 digest(s)",
		"moderated upload: stored=false err=filesystem: rejected: scan: the file is infected (removed-by-moderation)",
		"moderated upload: stored=true err=<nil>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestInfectedFileIsNotKept checks the promise behind Inspect: a refused
// file is deleted, not merely reported.
func TestInfectedFileIsNotKept(t *testing.T) {
	svc := httptestServer(t)
	disk := filesystemtest.NewFake(t, diskName)
	p := policyWith(HTTPScanner{URL: svc.URL + scanPath, Client: svc.Client()})
	_, err := upload.Receive(t.Context(), disk.Disk, strings.NewReader(eicar), "e.txt", p)
	if filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("err = %v", err)
	}
	disk.AssertEmpty()
}

// httptestServer serves the simulated scanning service for one test.
func httptestServer(t *testing.T) *httptest.Server {
	svc := httptest.NewServer(fakeService())
	t.Cleanup(svc.Close)
	return svc
}

// TestLoadHashListRefusesBadLines: a list in the wrong format fails at
// load instead of silently matching nothing.
func TestLoadHashListRefusesBadLines(t *testing.T) {
	for _, list := range []string{"not-hex\n", "abcd\n", strings.Repeat("a", 32) + "\n"} {
		if _, err := loadHashList(strings.NewReader(list)); err == nil {
			t.Errorf("%q accepted", list)
		}
	}
	got, err := loadHashList(strings.NewReader("\n# comment\n" + strings.Repeat("A", 64) + "\n"))
	if err != nil || len(got) != 1 || got[0].Algorithm != scan.AlgorithmSHA256 {
		t.Errorf("valid list = %v, %v", got, err)
	}
}
