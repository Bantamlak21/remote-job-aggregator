// Package mailer sends the sign-in emails: over SMTP in production, and to the
// log in development (never in production: the log would then hold live
// sign-in links).
package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Message is one email. Text is required; HTML is optional.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer sends messages.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// ErrHeaderInjection is returned when an address or subject holds a line break.
var ErrHeaderInjection = errors.New("mailer: line break in a header value")

// Log prints messages instead of sending them. Development only: config.go refuses it
// outside development, and even in development refuses it alongside an https
// PUBLIC_BASE_URL. It prints the full address and, for a sign-in email, the live token —
// exactly what a developer needs to sign in locally without a mail server, and exactly
// what must never reach a log sink anyone else can read. Never point AUTH_MAILER=log's
// output at a shared, retained or centrally shipped log stream.
type Log struct{ Logger *slog.Logger }

// Send logs the message.
func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("email (not sent: log mailer)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return nil
}

// TLS modes for SMTP.
const (
	TLSStartTLS = "starttls" // connect in clear, upgrade (usually port 587)
	TLSImplicit = "tls"      // TLS from the first byte (usually port 465)
	TLSNone     = "none"     // no encryption: only for a local relay
)

// SMTP sends over an SMTP server.
type SMTP struct {
	Host, Port         string
	Username, Password string
	From               string // "Name <addr>" or "addr"
	TLS                string
	Timeout            time.Duration
	// InsecureSkipVerify is for tests against a self-signed server only.
	InsecureSkipVerify bool
}

// Send delivers m. The whole exchange is bounded by the smaller of ctx and
// Timeout.
func (s SMTP) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("mailer: invalid From: %w", err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil || to.Name != "" {
		return fmt.Errorf("mailer: invalid recipient")
	}
	body, err := build(from, to.Address, m)
	if err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	d := net.Dialer{}
	addr := net.JoinHostPort(s.Host, s.Port)
	var conn net.Conn
	cfg := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: s.InsecureSkipVerify} //nolint:gosec // test-only switch
	if s.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: cfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mailer: connecting: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// A deadline covers a server that goes quiet, but not a cancellation (shutdown):
	// closing the connection when ctx ends unblocks any read or write in progress, so a
	// stalled server cannot hold the process past its shutdown timeout.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: greeting: %w", err)
	}
	defer func() { _ = c.Close() }()

	if s.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("mailer: the server does not offer STARTTLS")
		}
		if err := c.StartTLS(cfg); err != nil {
			return fmt.Errorf("mailer: starttls: %w", err)
		}
	}
	if s.Username != "" {
		if s.TLS == TLSNone {
			return errors.New("mailer: refusing to send a password without encryption")
		}
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mailer: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("mailer: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mailer: writing the message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: finishing the message: %w", err)
	}
	return c.Quit()
}

// build renders the RFC 5322 message: multipart/alternative with quoted-printable
// UTF-8 parts, an encoded subject, and header values checked for line breaks.
func build(from *mail.Address, to string, m Message) ([]byte, error) {
	for _, v := range []string{from.Address, from.Name, to, m.Subject} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, ErrHeaderInjection
		}
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("mailer: message id: %w", err)
	}
	domain := "localhost"
	if at := strings.LastIndexByte(from.Address, '@'); at >= 0 {
		domain = from.Address[at+1:]
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	// The multipart writer picks the boundary; headers go on top.
	var out bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&out, "%s: %s\r\n", k, v) }
	h("From", from.String())
	h("To", (&mail.Address{Address: to}).String())
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", time.Now().UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	part := func(ctype, content string) error {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ctype + `; charset="utf-8"`},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return err
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(content)); err != nil {
			return err
		}
		return qp.Close()
	}
	if err := part("text/plain", m.Text); err != nil {
		return nil, err
	}
	if m.HTML != "" {
		if err := part("text/html", m.HTML); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	h("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	out.WriteString("\r\n")
	out.Write(b.Bytes())
	return out.Bytes(), nil
}
