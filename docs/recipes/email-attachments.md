# Email attachments

Apps mail stored files all the time: the invoice PDF after a payment, the signed contract, the weekly report, the export a user asked for. The library has no mailer, on purpose: every app already has one (SMTP, SES, Postmark, SendGrid), and what they all need from storage is the same two things, which this recipe shows: a stream of the file's bytes to attach without loading it into memory, and, for a file too large for a mailbox, a download link that keeps working long enough to be clicked.

The program is [examples/recipe-email-attachments](../../examples/recipe-email-attachments). Every code block below is copied from it, and the transcript is from running it.

## What you get

- A file up to 10 MiB is attached: streamed from the disk with `Disk.Read`, base64-encoded in 76-character lines into the MIME message, with the type the bytes were stored as.
- A larger file is replaced by a signed download link in the text, valid for seven days, that downloads under the file's real name.
- The name the recipient sees is the user's original file name, kept apart from the storage key, and encoded so non-ASCII names (`Facture n°42.pdf`) arrive intact.
- The message is composed straight into the SMTP `DATA` stream, so nothing is buffered whole, whatever the file's size.

## Before you start

You need the core module `github.com/ubgo/filesystem` and a disk that can produce signed URLs: S3, GCS and Azure sign natively; a local or memory disk signs with `filesystem.WithURLSigner` and a `signedurl.Signer`, served by `fshttp.Signed` ([serving files](../concepts/serving-files.md)). The example uses a memory disk with a signer and an in-process SMTP server, so it needs no mail account.

Attach or link? Attach when the file is small and the recipient needs it offline or in their records (invoices, receipts, tickets). Link when the file is large, sensitive (a link can expire and be audited; an attachment lives in every mailbox it reaches forever), or likely to change. Many providers cap a whole message at 25 MB, and base64 makes a file a third larger, so 10 MiB is a safe ceiling for attachments.

## Step 1: Decide per file, from what is stored

<!-- ds:block id=plan-rsfyhv5a -->
**plan** · [`examples/recipe-email-attachments/main.go:80-102`](../../examples/recipe-email-attachments/main.go#L80-L102)

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

`Stat` gives the stored size and type. The size decides attach or link; the type becomes the attachment's `Content-Type`. Both come from what the library recorded at write time (detected from the bytes), never from the file name, so a file named `.pdf` that is not a PDF is not announced as one.

The link is `Disk.SignedURL` with the longest lifetime a signed URL may have, `filesystem.MaxPresignExpiry` (seven days, the S3 limit). `ContentDisposition` makes the browser save it under the user's file name instead of the key; on S3 and GCS it is signed into the URL as `response-content-disposition`, and `signedurl` signs it as `cd`, so the recipient cannot change it.

## Step 2: Write the message

<!-- ds:block id=compose-bqznmsaa -->
**compose** · [`examples/recipe-email-attachments/main.go:108-133`](../../examples/recipe-email-attachments/main.go#L108-L133)

```go
func compose(ctx context.Context, w io.Writer, disk *filesystem.Disk, msg Message) error {
	to, err := mail.ParseAddress(msg.To) // refuses CR, LF and anything else that is not one address
	if err != nil {
		return filesystem.NewError(filesystem.CodeInvalidArgument, err)
	}
	attach, body, err := plan(ctx, disk, msg)
	if err != nil {
		return err
	}
	mw := multipart.NewWriter(w)
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%s\r\n\r\n",
		sender, to.Address, mime.QEncoding.Encode("utf-8", msg.Subject), mw.Boundary())
	text, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(text, body+"\r\n"); err != nil {
		return err
	}
	for _, a := range attach {
		if err := attachFile(ctx, mw, disk, a); err != nil {
			return err
		}
	}
	return mw.Close()
}
```
<!-- /ds:block hash=50c077 -->

The recipient is parsed with `net/mail`, so an address carrying a CR or LF (an attempt to add a `Bcc:` header) is refused before anything is written; the subject goes through `mime.QEncoding`, which encodes such characters. `multipart.Writer` chooses a random boundary and writes each part's headers.

## Step 3: Stream the file into the message

<!-- ds:block id=attachfile-bp2kg62w -->
**attachFile** · [`examples/recipe-email-attachments/main.go:139-162`](../../examples/recipe-email-attachments/main.go#L139-L162)

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

`io.Copy` moves the file from `Disk.Read` through a base64 encoder into the part 32 KiB at a time. MIME requires base64 bodies in lines of at most 76 characters, which the `lineBreaker` in the example inserts. `mime.FormatMediaType` writes `filename*=utf-8''Facture%20n%C2%B042.pdf` (RFC 2231) for a name outside ASCII, which every current mail client decodes.

## Step 4: Send

<!-- ds:block id=send-kzn9836x -->
**send** · [`examples/recipe-email-attachments/main.go:196-220`](../../examples/recipe-email-attachments/main.go#L196-L220)

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

`smtp.Client.Data` returns a writer into the open `DATA` command, and `compose` writes into it, so the bytes flow from the disk to the mail server as they are read. With a provider's HTTP API instead of SMTP, the same idea applies where the API accepts a stream (a multipart request body built with `io.Pipe`); where it needs the whole attachment as a base64 string in JSON, read it with `Disk.ReadAll` and keep `maxAttachBytes` low.

## Try it

```sh
cd examples
go run ./recipe-email-attachments
```

The in-process server prints the message it received:

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

Check: the 10 MiB export became a signed link with an expiry (`exp`) and a fixed download name (`cd`); the invoice is attached under its original name, decoded by any mail client as `Facture n°42.pdf`. `go test ./recipe-email-attachments` parses the message with `net/mail`, decodes the attachment and compares it with the stored bytes, and checks that a 200 KB attachment is encoded in lines of at most 76 characters and round-trips exactly.

## Pitfalls

- **Reading the whole file into memory.** `ReadAll` of a 10 MiB file per email, in a worker sending hundreds, is gigabytes of garbage. Stream from `Disk.Read` wherever the mailer accepts a writer or a reader.
- **A link that outlives its purpose, or not long enough.** People open email days later; a 15-minute link is useless. Seven days is the most a signed URL allows; for longer, link to a page of your app that checks the user and issues a fresh signed URL when clicked.
- **Trusting the file name.** Use the stored type from `Stat`, and the original name from your records (`records.File.Filename`), never the key: keys are random or structured, and a user does not want `fil_k5x5h3.pdf`.
- **Header injection.** A file name or subject containing CR or LF could add headers. `mime.FormatMediaType` and `mime.QEncoding` encode such values; never write a user's string into a header with `fmt` alone.
- **Sending a file the recipient may not see.** The mailer runs with the app's rights, not the user's. Check access the way your download handler does before attaching a file to an email someone else asked for.
- **Retries send twice.** If the SMTP server accepted the message but the connection dropped before `250`, a retry delivers it again. Record a sent marker per message (an idempotency key in your outbox table) before retrying.

## Related

- [Private downloads](private-downloads.md): signed URLs and the handler that serves them.
- [Exports](exports.md): building the large file this recipe links to.
- [Serving files](../concepts/serving-files.md): `fshttp.Signed`, download names, `Content-Disposition`.
- [Disks and drivers](../concepts/disks-and-drivers.md): `Read`, `Stat` and the rest of the Disk API.
