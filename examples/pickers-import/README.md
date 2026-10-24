# Cloud picker import

This program runs a stand-in for the Google Drive API and an application on in-process servers, and uses `github.com/ubgo/filesystem/contrib/pickers`: the "browser" posts the file ID and access token the Drive Picker returned, and the application imports the file through `upload.Receive`, so the upload policy decides what is stored. The guide [Cloud picker imports](../../docs/plugins/pickers.md) explains each step.

## Run it

```sh
cd examples/pickers-import
go run .
go test .
```

It needs no Google account and no environment variables. The SSRF guard refuses loopback, so the demo allows `127.0.0.1` with `AllowAddr`; a real deployment leaves it unset.

## What it does

```text
POST /import/drive -> 200 {"filename":"invoice-2026-10.pdf","key":"b7ipfamv2aihsu2twcjbfcomnc.pdf","size":28,"type":"application/pdf"}
box import: contract.pdf application/pdf 29 bytes
box import with token "exp…": permission
```

The key is random per run. The last two lines come from `source.go`, a `pickers.Source` written for a vendor the package does not ship (a Box-like API served in-process): the right token imports, a refused one is `permission`.
