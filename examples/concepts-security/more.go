package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/sniff"
	"github.com/ubgo/filesystem/upload"
)

// demoSigningSecret is a fixed, long-enough secret for the redaction step;
// it must never appear in the output.
var demoSigningSecret = bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)

// extensionFromType stores files whose names lie about their type, or
// have no extension at all. RandomKey names each stored file with the
// extension of the type detected from its bytes, so the key's extension
// is always the truth, whatever the client called the file.
// ds:def id=extensionfromtype-6mq54e6t
func extensionFromType(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	p := images
	p.Key = upload.RandomKey("avatars")
	for _, name := range []string{"photo", "photo.gif", "photo.png.exe.jpg"} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(pngBytes), name, p)
		if err != nil {
			fmt.Fprintf(out, "named %q: %s\n", name, filesystem.CodeOf(err))
			continue
		}
		fmt.Fprintf(out, "named %q: stored with extension %s\n", name, path.Ext(res.Key))
	}
	return nil
}

// mimeHelpers shows the type tables in sniff. They answer the same on
// every machine, unlike mime.TypeByExtension, which reads system files.
// None of them decides what a file is; Detect does that from the bytes.
// ds:def id=mimehelpers-9cmqcw4g
func mimeHelpers(out io.Writer) {
	fmt.Fprintf(out, "TypeByExtension(report.PDF) = %s\n", sniff.TypeByExtension("report.PDF"))
	fmt.Fprintf(out, "Extension(image/jpeg) = %s\n", sniff.Extension("image/jpeg"))
	fmt.Fprintf(out, "Matches(image/png, image/*) = %t\n", sniff.Matches("image/png", "image/*"))
	fmt.Fprintf(out, "MatchesAny(application/pdf, [image/* text/*]) = %t\n",
		sniff.MatchesAny("application/pdf", []string{"image/*", "text/*"}))
	fmt.Fprintf(out, "IsActive(application/xhtml+xml) = %t\n", sniff.IsActive("application/xhtml+xml"))
}

// redaction prints a signing key and a signer every way a log line or an
// error message might, and opens a disk from a DSN that carries a
// password. None of the outputs contains the secret.
// ds:def id=redaction-2vb5zj87
func redaction(ctx context.Context, out io.Writer) error {
	key := signedurl.Key{ID: "2026-10", Secret: demoSigningSecret}
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{key})
	if err != nil {
		return err
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("signer ready", "key", key, "signer", signer)
	printed := fmt.Sprintf("%v %+v %#v %s", key, key, key, key) + logs.String()
	fmt.Fprintf(out, "key printed: %v\n", key)
	fmt.Fprintf(out, "secret in any print or log: %t\n", strings.Contains(printed, string(demoSigningSecret)))
	mux := filesystem.NewMux()
	if err := mux.Register("mem", memory.Opener); err != nil {
		return err
	}
	_, err = mux.Open(ctx, "media", "mem://bucket?password=hunter2&prefix=../x")
	fmt.Fprintf(out, "DSN error: %v (password shown: %t)\n", err, strings.Contains(fmt.Sprint(err), "hunter2"))
	return nil
}
