# Email attachments: stream stored files into a message, or link them

Mailing stored files: an invoice PDF small enough to attach is streamed from the disk into the MIME message with `Disk.Read`, base64-encoded in 76-character lines, under the user's original file name and the type its bytes were stored as; an export larger than 10 MiB becomes a signed download link, valid for seven days, in the message text. The message is written straight into `net/smtp`'s `DATA` stream, to an SMTP server that runs in the same process, so nothing is buffered whole and no mail account is needed. It uses the core `filesystem` package (`Stat`, `Read`, `SignedURL`, a memory disk) and `signedurl`. The guide is [Email attachments](../../docs/recipes/email-attachments.md).

## Run it

```sh
cd examples
go run ./recipe-email-attachments
go test ./recipe-email-attachments
```

It needs nothing outside the process: the disk is in memory, and the SMTP server listens on a random local port, accepts one message, prints it and exits.

## What it does

The server prints the message it received (the boundary, the expiry and the signature differ per run):

```text
server received 911 bytes:
From: billing@example.com
To: ada@example.org
Subject: Your invoice and export
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary=f5cab1b30853977102a81734c45b3d9522a09045486fd594792f6ebf036e

--f5cab1b30853977102a81734c45b3d9522a09045486fd594792f6ebf036e
Content-Type: text/plain; charset=utf-8

Hello Ada,

Your invoice is attached.

all-orders.csv is too large to attach. Download it within 7 days:
https://files.example.com/dl/files/exports/u1/all-orders.csv?cd=attachment%3B+filename%3Dall-orders.csv&exp=1792069704&kid=k1&m=GET&sig=c4d65295e932095a03fd72eaab8ad5397bcd6a3d16304aace19d003f96d61d62

--f5cab1b30853977102a81734c45b3d9522a09045486fd594792f6ebf036e
Content-Disposition: attachment; filename*=utf-8''Facture%20n%C2%B042.pdf
Content-Transfer-Encoding: base64
Content-Type: application/pdf

JVBERi0xLjcKJSBpbnZvaWNlIDQyCg==

--f5cab1b30853977102a81734c45b3d9522a09045486fd594792f6ebf036e--
sent to ada@example.org through 127.0.0.1:60217
```

1. `Stat` finds the invoice small enough and the export too large.
2. The export becomes a signed link with a seven-day expiry and a fixed download name.
3. The invoice is attached under `Facture n°42.pdf`, encoded as RFC 2231 so the accented name survives.

## The code

Attach or link, per file:

<!-- ds:block id=plan-rsfyhv5a -->
**plan** · [`examples/recipe-email-attachments/main.go:80-102`](main.go#L80-L102)

```go
func plan(ctx context.Context, disk *filesystem.Disk, msg Message) ([]attachment, string, error) {
	var attach []attachment
	body := msg.Body
	for _, f := range msg.Files {
		e, err := disk.Stat(ctx, f.Key)
		if err != nil {
			return nil, "", err
		}
		if e.Size <= maxAttachBytes {
			attach = append(attach, attachment{File: f, ContentType: e.ContentType})
			continue
		}
		link, err := disk.SignedURL(ctx, f.Key, filesystem.PresignOptions{
			Method: filesystem.MethodGet, Expires: linkLifetime,
			ContentDisposition: mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}),
		})
		if err != nil {
			return nil, "", err
		}
		body += fmt.Sprintf("\r\n\r\n%s is too large to attach. Download it within 7 days:\r\n%s", f.Filename, link.URL)
	}
	return attach, body, nil
}
```
<!-- /ds:block hash=a4325e -->

Stream one file into the message:

<!-- ds:block id=attachfile-bp2kg62w -->
**attachFile** · [`examples/recipe-email-attachments/main.go:139-162`](main.go#L139-L162)

```go
func attachFile(ctx context.Context, mw *multipart.Writer, disk *filesystem.Disk, a attachment) error {
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {a.ContentType},
		"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})},
		"Content-Transfer-Encoding": {"base64"},
	})
	if err != nil {
		return err
	}
	rc, err := disk.Read(ctx, a.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	enc := base64.NewEncoder(base64.StdEncoding, &lineBreaker{w: part})
	if _, err := io.Copy(enc, rc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err = io.WriteString(part, "\r\n")
	return err
}
```
<!-- /ds:block hash=b6fd86 -->

Send through SMTP, composing into the `DATA` stream:

<!-- ds:block id=send-kzn9836x -->
**send** · [`examples/recipe-email-attachments/main.go:196-220`](main.go#L196-L220)

```go
func send(ctx context.Context, addr string, disk *filesystem.Disk, msg Message) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.Mail(sender); err != nil {
		return err
	}
	if err := c.Rcpt(msg.To); err != nil {
		return err
	}
	data, err := c.Data()
	if err != nil {
		return err
	}
	if err := compose(ctx, data, disk, msg); err != nil {
		_ = data.Close()
		return err
	}
	if err := data.Close(); err != nil {
		return err
	}
	return c.Quit()
}
```
<!-- /ds:block hash=85e3fe -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The attach-or-link decision, the MIME composer with its base64 line breaker, the SMTP send, and the in-process SMTP server. |
| `main_test.go` | Parses the sent message and checks the attachment's bytes, name and type and the link's signature; a 200 KB attachment's line lengths and round trip; a recipient that tries to inject a header. |

## Next

- The guide: [Email attachments](../../docs/recipes/email-attachments.md).
- [recipe-private-downloads](../recipe-private-downloads): the handler that serves the signed links.
- [recipe-exports](../recipe-exports): building the large file the link points to.
