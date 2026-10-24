package office_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/office"
	"github.com/ubgo/filesystem/fshttp"
)

// fakeSOffice writes an executable shell script standing in for soffice.
// The script sees the arguments as soffice would: $in is the document
// (the last argument), $out the --outdir directory, $profile the
// -env:UserInstallation URL.
func fakeSOffice(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "soffice")
	script := `#!/bin/sh
prev=""; out=""; profile=""
for a in "$@"; do
  [ "$prev" = "--outdir" ] && out="$a"
  case "$a" in -env:UserInstallation=*) profile="${a#-env:UserInstallation=}";; esac
  prev="$a"; in="$a"
done
` + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// emptyDir fails the test when dir holds anything: a conversion must
// leave nothing behind.
func emptyDir(t *testing.T, dir string) {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil || len(left) != 0 {
		t.Fatalf("left behind in %s: %v %v", dir, left, err)
	}
}

func TestConvertToPDF_FencesTheRun(t *testing.T) {
	logDir := t.TempDir()
	soffice := fakeSOffice(t, `
printf '%s\n' "$HOME" > `+logDir+`/home
printf '%s\n' "$TMPDIR" > `+logDir+`/tmpdir
printf '%s\n' "$@" > `+logDir+`/args
cp "${profile#file://}/user/registrymodifications.xcu" `+logDir+`/profile
cp "$in" `+logDir+`/input
printf '%%PDF-1.4 converted' > "$out/document.pdf"`)
	work := t.TempDir()
	tool := office.Tool{SOffice: soffice, TempDir: work}
	rc, err := tool.ConvertToPDF(t.Context(), strings.NewReader("the document"), office.DOCX+"; charset=binary")
	if err != nil {
		t.Fatal(err)
	}
	emptyDir(t, work) // the PDF is already unlinked while open
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "%PDF-1.4 converted" {
		t.Fatalf("pdf = %q, %v", body, err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	home, tmp, args := read("home"), read("tmpdir"), read("args")
	if !strings.HasPrefix(home, work) || !strings.HasPrefix(tmp, work) || home == tmp {
		t.Fatalf("HOME %q and TMPDIR %q are not the run's own under %s", home, tmp, work)
	}
	for _, want := range []string{"--headless", "--norestore", "--convert-to\npdf", "-env:UserInstallation=file://" + work} {
		if !strings.Contains(args, want) {
			t.Errorf("arguments lack %q:\n%s", want, args)
		}
	}
	if !strings.HasSuffix(args, ".docx") {
		t.Errorf("the input is not named for its type:\n%s", args)
	}
	profile := read("profile")
	for _, want := range []string{`"DisableMacrosExecution" oor:op="fuse"><value>true`, `"MacroSecurityLevel" oor:op="fuse"><value>3`, `Writer/Content/Update"><prop oor:name="Link" oor:op="fuse"><value>0`, `Calc/Content/Update"><prop oor:name="Link" oor:op="fuse"><value>0`} {
		if !strings.Contains(profile, want) {
			t.Errorf("the profile lacks %s", want)
		}
	}
	if read("input") != "the document" {
		t.Fatal("the document did not reach the program")
	}
}

// TestConvertToPDF_OutputCap pins that a conversion writing without end
// is stopped at MaxOutput, long before its timeout.
func TestConvertToPDF_OutputCap(t *testing.T) {
	tool := office.Tool{SOffice: fakeSOffice(t, `yes 'page' > "$out/document.pdf"`), MaxOutput: 1 << 20, Timeout: 30 * time.Second, TempDir: t.TempDir()}
	start := time.Now()
	if _, err := tool.ConvertToPDF(t.Context(), strings.NewReader("x"), office.XLSX); filesystem.CodeOf(err) != filesystem.CodeTooLarge || time.Since(start) > 10*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
	// A PDF over the cap written before the first check is caught after
	// the run.
	tool.SOffice = fakeSOffice(t, `head -c 2000 /dev/zero > "$out/document.pdf"`)
	tool.MaxOutput = 1000
	if _, err := tool.ConvertToPDF(t.Context(), strings.NewReader("x"), office.XLSX); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("a quick oversized PDF: %v", err)
	}
}

func TestConvertToPDF_Failures(t *testing.T) {
	ctx := t.Context()
	// soffice exits 0 when it cannot load a document; no PDF is a
	// rejection.
	tool := office.Tool{SOffice: fakeSOffice(t, "echo 'Error: source file could not be loaded'"), TempDir: t.TempDir()}
	_, err := tool.ConvertToPDF(ctx, strings.NewReader("x"), office.PPTX)
	var pe *office.ProgramError
	if filesystem.CodeOf(err) != filesystem.CodeRejected || !errors.As(err, &pe) || pe.Program != "soffice" {
		t.Fatalf("no output: %v", err)
	}
	if strings.Contains(fshttp.ClientMessage(err), string(filepath.Separator)) {
		t.Fatalf("the client message names a path: %q", fshttp.ClientMessage(err))
	}
	// A failing run keeps only the start of its messages.
	tool.SOffice = fakeSOffice(t, `i=0; while [ $i -lt 2000 ]; do echo "error line $i" >&2; i=$((i+1)); done; exit 3`)
	if _, err = tool.ConvertToPDF(ctx, strings.NewReader("x"), office.PPTX); !errors.As(err, &pe) || len(pe.Detail) == 0 || len(pe.Detail) > 4<<10 {
		t.Fatalf("failing run: %v", err)
	}
	// Too large an input never reaches the program.
	tool.MaxInput = 4
	if _, err = tool.ConvertToPDF(ctx, strings.NewReader("12345"), office.PPTX); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("large input: %v", err)
	}
	emptyDir(t, tool.TempDir)
	// A missing program is a deployment problem.
	if _, err = (office.Tool{SOffice: "/nonexistent/soffice"}).ConvertToPDF(ctx, strings.NewReader("x"), office.DOCX); !errors.Is(err, office.ErrNoProgram) || filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("missing program: %v", err)
	}
	if _, err = (office.Tool{SOffice: tool.SOffice, TempDir: filepath.Join(t.TempDir(), "missing")}).ConvertToPDF(ctx, strings.NewReader("x"), office.DOCX); err == nil {
		t.Fatal("a missing TempDir was not reported")
	}
}

func TestConverts(t *testing.T) {
	var tool office.Tool
	for _, ct := range office.DefaultContentTypes() {
		if !tool.Converts(strings.ToUpper(ct) + "; charset=binary") {
			t.Errorf("does not convert %s", ct)
		}
	}
	if tool.Converts("application/msword") || tool.Converts("application/pdf") {
		t.Error("converts a type outside the defaults")
	}
	tool.ContentTypes = []string{"Application/MSWord"}
	if !tool.Converts("application/msword") || tool.Converts(office.DOCX) {
		t.Error("ContentTypes does not replace the defaults")
	}
}

// docx builds a minimal Word document saying text.
func docx(t *testing.T, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":         `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestConvertToPDF_LibreOffice converts a real document with the
// installed LibreOffice.
func TestConvertToPDF_LibreOffice(t *testing.T) {
	if _, err := exec.LookPath(office.DefaultSOffice); err != nil {
		t.Skip("soffice is not installed (LibreOffice: brew install --cask libreoffice, apt install libreoffice-core libreoffice-writer)")
	}
	work := t.TempDir()
	rc, err := office.Tool{TempDir: work}.ConvertToPDF(t.Context(), bytes.NewReader(docx(t, "Quarterly results")), office.DOCX)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil || !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatalf("not a PDF: %.20q, %v", body, err)
	}
	emptyDir(t, work)
}

// TestConvertToPDF_PassesOnlyAnAllowlistedEnvironment pins that soffice
// sees PATH, its run's HOME and the variables of Tool.Env, never the
// application's secrets, and that Env cannot move HOME out of the run.
func TestConvertToPDF_PassesOnlyAnAllowlistedEnvironment(t *testing.T) {
	t.Setenv("FS_TEST_SECRET", "planted-secret")
	log := filepath.Join(t.TempDir(), "env")
	work := t.TempDir()
	tool := office.Tool{
		SOffice: fakeSOffice(t, `env > `+log+`; printf '%%PDF' > "$out/document.pdf"`),
		Env:     []string{"FS_TEST_EXTRA=passed", "HOME=/"},
		TempDir: work,
	}
	rc, err := tool.ConvertToPDF(t.Context(), strings.NewReader("x"), office.DOCX)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	b, err := os.ReadFile(log)
	env := string(b)
	if err != nil || strings.Contains(env, "planted-secret") || !strings.Contains(env, "PATH=") || !strings.Contains(env, "FS_TEST_EXTRA=passed") || !strings.Contains(env, "HOME="+work) || strings.Contains(env, "HOME=/\n") {
		t.Fatalf("environment:\n%s\n%v", env, err)
	}
}
