// Command recipe-email-attachments sends stored files by email: an invoice
// PDF small enough to attach is streamed from the disk straight into the
// MIME message, base64-encoded on the way, never held in memory whole; an
// export too large for a mailbox is replaced by a signed download link
// that expires after a week. The messages go through net/smtp to an
// in-process SMTP server, so the program needs no mail account.
//
//	go run ./recipe-email-attachments            (from the examples directory)
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

// Decisions of this app, not of the library.
const (
	// maxAttachBytes is the largest file sent as an attachment. Base64
	// adds a third, and many providers refuse messages over 25 MB, so
	// 10 MiB of file stays well inside every common limit.
	maxAttachBytes = 10 << 20
	// linkLifetime is how long a download link in an email works: the
	// longest a signed URL may live (filesystem.MaxPresignExpiry).
	linkLifetime = 7 * 24 * time.Hour
	// downloadBase is where the app's fshttp.Signed handler serves links.
	downloadBase = "https://files.example.com/dl"
	// sender is the From address of every message.
	sender = "billing@example.com"
	// base64Line is the longest encoded line MIME allows (RFC 2045).
	base64Line = 76
)

// demoKey signs the download links. A real app loads its keys from a
// secret store and rotates them; see signedurl.
var demoKey = signedurl.Key{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}

// File is a stored file to send: its key on the disk, and the name the
// recipient sees, which the app keeps apart from the key (a records row's
// Filename, or the app's own column).
type File struct {
	Key      string
	Filename string
}

// attachment is a file small enough to attach, with the type its bytes
// were stored as.
type attachment struct {
	File
	ContentType string
}

// Message is one email to compose.
type Message struct {
	To      string
	Subject string
	Body    string
	Files   []File
}

// plan decides, per file, whether it is attached or linked: a file up to
// maxAttachBytes is attached with the type it was stored as; a larger one
// gets a signed download link appended to the body. The size and type
// come from Stat, never from the file name.
// ds:def id=plan-rsfyhv5a
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

// compose writes msg as a MIME message to w: the headers, a text part,
// then each attachment streamed from the disk. Every header value a user
// can influence is parsed or encoded, never pasted in.
// ds:def id=compose-bqznmsaa
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

// attachFile streams one stored file into the message as a base64 part.
// mime.FormatMediaType encodes a non-ASCII file name (RFC 2231), so
// "Résumé.pdf" arrives intact.
// ds:def id=attachfile-bp2kg62w
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

// lineBreaker inserts CRLF every base64Line bytes, as MIME requires of
// base64 bodies.
type lineBreaker struct {
	w   io.Writer
	col int
}

func (l *lineBreaker) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := min(base64Line-l.col, len(p))
		m, err := l.w.Write(p[:chunk])
		n += m
		if err != nil {
			return n, err
		}
		l.col += chunk
		p = p[chunk:]
		if l.col == base64Line {
			if _, err := io.WriteString(l.w, "\r\n"); err != nil {
				return n, err
			}
			l.col = 0
		}
	}
	return n, nil
}

// send delivers msg through an SMTP server with net/smtp. The message is
// composed straight into the DATA stream, so a 10 MiB attachment flows
// from the disk to the server without being buffered.
// ds:def id=send-kzn9836x
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

// openDisk is a memory disk with a URL signer, holding an invoice and a
// large export. In an app it is the disk the files already live on.
func openDisk(ctx context.Context) (*filesystem.Disk, error) {
	signer, err := signedurl.New(downloadBase, []signedurl.Key{demoKey})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk("files", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	if _, err := disk.WriteBytes(ctx, "invoices/2026/0042.pdf", []byte("%PDF-1.7\n% invoice 42\n"), filesystem.WriteOptions{}); err != nil {
		return nil, err
	}
	export := strings.Repeat("id,amount\n", (maxAttachBytes/10)+1)
	_, err = disk.WriteBytes(ctx, "exports/u1/all-orders.csv", []byte(export), filesystem.WriteOptions{ContentType: "text/csv"})
	return disk, err
}

// run sends one message with both files to the SMTP server at addr and
// prints what it did.
func run(ctx context.Context, out io.Writer, addr string) error {
	disk, err := openDisk(ctx)
	if err != nil {
		return err
	}
	msg := Message{
		To:      "ada@example.org",
		Subject: "Your invoice and export",
		Body:    "Hello Ada,\r\n\r\nYour invoice is attached.",
		Files: []File{
			{Key: "invoices/2026/0042.pdf", Filename: "Facture n°42.pdf"},
			{Key: "exports/u1/all-orders.csv", Filename: "all-orders.csv"},
		},
	}
	if err := send(ctx, addr, disk, msg); err != nil {
		return err
	}
	fmt.Fprintf(out, "sent to %s through %s\n", msg.To, addr)
	return nil
}

func main() {
	srv, err := startSMTP(os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()
	if err := run(context.Background(), os.Stdout, srv.Addr().String()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	<-srv.done
}

// smtpServer is a minimal in-process SMTP server that accepts one message
// and reports what it received: enough for the demo and the test, not a
// mail server.
type smtpServer struct {
	net.Listener
	done chan struct{}
	out  io.Writer
	// Last is the last message received, in wire form.
	Last string
}

// startSMTP listens on a random local port and serves one connection.
func startSMTP(out io.Writer) (*smtpServer, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &smtpServer{Listener: l, done: make(chan struct{}), out: out}
	go s.serve()
	return s, nil
}

func (s *smtpServer) serve() {
	defer close(s.done)
	conn, err := s.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	tp := textproto.NewConn(conn)
	reply := func(line string) { _ = tp.PrintfLine("%s", line) }
	reply("220 localhost demo SMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.Fields(line + " x")[0]); cmd {
		case "EHLO", "HELO":
			reply("250 localhost")
		case "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			body, err := io.ReadAll(tp.DotReader())
			if err != nil {
				return
			}
			s.Last = string(body)
			fmt.Fprintf(s.out, "server received %d bytes:\n%s", len(body), body)
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}
