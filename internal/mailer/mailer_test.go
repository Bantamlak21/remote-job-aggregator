package mailer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- an in-process SMTP server ------------------------------------------------

type smtpSession struct {
	tls        bool
	commands   []string
	user, pass string
	from       string
	rcpts      []string
	data       string // the message as received, dot-unstuffed, lines joined with CRLF
	delivered  bool
}

// fakeSMTP is just enough SMTP to test a client: EHLO, STARTTLS, AUTH PLAIN, MAIL,
// RCPT, DATA, QUIT. It records everything a client sent.
type fakeSMTP struct {
	ln     net.Listener
	tlsCfg *tls.Config

	// behaviour
	advertiseSTARTTLS bool
	implicitTLS       bool
	wantUser          string // when set, AUTH is offered and this login is required
	wantPass          string
	rejectRcpt        bool
	hangAt            string // "greeting" or a command verb the server never answers

	done chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	// guarded by mu
	conns    int
	sessions []*smtpSession
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake-smtp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startSMTP(t *testing.T, configure func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	s := &fakeSMTP{done: make(chan struct{})}
	s.tlsCfg = &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, MinVersion: tls.VersionTLS12}
	if configure != nil {
		configure(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.handle(c) }()
		}
	}()
	t.Cleanup(func() {
		close(s.done)
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeSMTP) port() string {
	_, p, _ := net.SplitHostPort(s.ln.Addr().String())
	return p
}

func (s *fakeSMTP) client(tlsMode string) SMTP {
	return SMTP{Host: "127.0.0.1", Port: s.port(), From: "Sign-in <no-reply@jobs.example.com>", TLS: tlsMode, Timeout: 5 * time.Second, InsecureSkipVerify: true}
}

func (s *fakeSMTP) update(sess *smtpSession, f func(*smtpSession)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(sess)
}

// snapshot returns copies of the sessions and the number of connections.
func (s *fakeSMTP) snapshot() (int, []smtpSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]smtpSession, len(s.sessions))
	for i, x := range s.sessions {
		out[i] = *x
		out[i].commands = append([]string(nil), x.commands...)
		out[i].rcpts = append([]string(nil), x.rcpts...)
	}
	return s.conns, out
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	sess := &smtpSession{}
	s.mu.Lock()
	s.conns++
	s.sessions = append(s.sessions, sess)
	s.mu.Unlock()

	if s.implicitTLS {
		tc := tls.Server(conn, s.tlsCfg)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
		s.update(sess, func(x *smtpSession) { x.tls = true })
	}
	r := bufio.NewReader(conn)
	reply := func(format string, a ...any) { _, _ = fmt.Fprintf(conn, format+"\r\n", a...) }

	if s.hangAt == "greeting" {
		<-s.done
		return
	}
	reply("220 fake.example ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		logged := line
		if verb == "AUTH" {
			logged = "AUTH" // the credentials are checked below, not stored as a command
		}
		s.update(sess, func(x *smtpSession) { x.commands = append(x.commands, logged) })
		if s.hangAt == verb {
			<-s.done
			return
		}
		switch verb {
		case "EHLO", "HELO":
			reply("250-fake.example")
			if s.advertiseSTARTTLS && !sess.tlsNow(s) {
				reply("250-STARTTLS")
			}
			if s.wantUser != "" {
				reply("250-AUTH PLAIN")
			}
			reply("250 8BITMIME")
		case "STARTTLS":
			reply("220 go ahead")
			tc := tls.Server(conn, s.tlsCfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			r = bufio.NewReader(tc)
			s.update(sess, func(x *smtpSession) { x.tls = true })
		case "AUTH":
			parts := strings.Fields(line)
			if len(parts) != 3 || parts[1] != "PLAIN" {
				reply("504 only AUTH PLAIN with an initial response")
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(parts[2])
			fields := strings.Split(string(raw), "\x00")
			if err != nil || len(fields) != 3 {
				reply("501 bad AUTH data")
				continue
			}
			s.update(sess, func(x *smtpSession) { x.user, x.pass = fields[1], fields[2] })
			if fields[1] == s.wantUser && fields[2] == s.wantPass {
				reply("235 authenticated")
			} else {
				reply("535 bad credentials")
			}
		case "MAIL":
			if s.wantUser != "" && !sess.authedNow(s) {
				reply("530 authentication required")
				continue
			}
			s.update(sess, func(x *smtpSession) { x.from = between(line, '<', '>') })
			reply("250 ok")
		case "RCPT":
			if s.rejectRcpt {
				reply("550 no such user")
				continue
			}
			s.update(sess, func(x *smtpSession) { x.rcpts = append(x.rcpts, between(line, '<', '>')) })
			reply("250 ok")
		case "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			var lines []string
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				l = strings.TrimRight(l, "\r\n")
				if l == "." {
					break
				}
				lines = append(lines, strings.TrimPrefix(l, ".")) // un-stuff the leading dot (RFC 5321 4.5.2)
			}
			s.update(sess, func(x *smtpSession) { x.data = strings.Join(lines, "\r\n"); x.delivered = true })
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		case "RSET", "NOOP":
			reply("250 ok")
		default:
			reply("502 command not implemented")
		}
	}
}

func (x *smtpSession) tlsNow(s *fakeSMTP) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return x.tls
}

func (x *smtpSession) authedNow(s *fakeSMTP) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return x.user == s.wantUser && x.pass == s.wantPass
}

func between(s string, open, closeCh byte) string {
	i, j := strings.IndexByte(s, open), strings.LastIndexByte(s, closeCh)
	if i < 0 || j < i {
		return ""
	}
	return s[i+1 : j]
}

func delivered(t *testing.T, s *fakeSMTP) smtpSession {
	t.Helper()
	_, sessions := s.snapshot()
	var got []smtpSession
	for _, x := range sessions {
		if x.delivered {
			got = append(got, x)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d messages delivered, want 1 (sessions: %+v)", len(got), sessions)
	}
	return got[0]
}

func sample() Message {
	return Message{
		To:      "alice@example.com",
		Subject: "Your sign-in link for Remote jobs",
		Text:    "Open this link to sign in:\r\n\r\nhttps://jobs.example.com/auth/verify#token=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\n\r\nIt works once.\r\n",
		HTML:    `<p>Open <a href="https://jobs.example.com/auth/verify#token=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA">Sign in</a></p>`,
	}
}

// ---- delivery -----------------------------------------------------------------

func TestSMTP_StartTLSDeliversTheMessageOverTLS(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.advertiseSTARTTLS = true })
	if err := srv.client(TLSStartTLS).Send(context.Background(), sample()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := delivered(t, srv)
	if !got.tls {
		t.Error("the message was delivered without TLS")
	}
	if got.from != "no-reply@jobs.example.com" {
		t.Errorf("envelope sender = %q, want the bare From address", got.from)
	}
	if len(got.rcpts) != 1 || got.rcpts[0] != "alice@example.com" {
		t.Errorf("envelope recipients = %v, want [alice@example.com]", got.rcpts)
	}
	// STARTTLS happens before MAIL FROM, and EHLO is repeated over the encrypted link.
	var order []string
	for _, c := range got.commands {
		order = append(order, strings.ToUpper(strings.SplitN(c, " ", 2)[0]))
	}
	want := []string{"EHLO", "STARTTLS", "EHLO", "MAIL", "RCPT", "DATA", "QUIT"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("command sequence = %v, want %v", order, want)
	}
}

func TestSMTP_PlainNoneDeliversWithoutTLS(t *testing.T) {
	srv := startSMTP(t, nil)
	if err := srv.client(TLSNone).Send(context.Background(), sample()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := delivered(t, srv)
	if got.tls {
		t.Error("TLS was used in mode none")
	}
	for _, c := range got.commands {
		if strings.EqualFold(c, "STARTTLS") {
			t.Error("the client sent STARTTLS in mode none")
		}
	}
}

func TestSMTP_ImplicitTLSDelivers(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.implicitTLS = true })
	if err := srv.client(TLSImplicit).Send(context.Background(), sample()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := delivered(t, srv); !got.tls {
		t.Error("the message was not delivered over TLS")
	}
}

func TestSMTP_AuthPlainSendsTheCredentialsOverTLS(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.advertiseSTARTTLS = true; s.wantUser = "apikey"; s.wantPass = "s3cret pass" })
	c := srv.client(TLSStartTLS)
	c.Username, c.Password = "apikey", "s3cret pass"
	if err := c.Send(context.Background(), sample()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := delivered(t, srv)
	if got.user != "apikey" || got.pass != "s3cret pass" {
		t.Errorf("server saw login %q/%q", got.user, got.pass)
	}
	if !got.tls {
		t.Error("credentials were sent without TLS")
	}
}

func TestSMTP_WrongPasswordIsAnErrorAndNothingIsDelivered(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.advertiseSTARTTLS = true; s.wantUser = "apikey"; s.wantPass = "right" })
	c := srv.client(TLSStartTLS)
	c.Username, c.Password = "apikey", "wrong"
	err := c.Send(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("Send with a bad password = %v, want an auth error", err)
	}
	_, sessions := srv.snapshot()
	for _, s := range sessions {
		if s.delivered {
			t.Error("a message was delivered after a failed login")
		}
	}
}

func TestSMTP_NeverSendsAPasswordWithoutEncryption(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.wantUser = "apikey"; s.wantPass = "pw" })
	c := srv.client(TLSNone)
	c.Username, c.Password = "apikey", "pw"
	err := c.Send(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "without encryption") {
		t.Fatalf("Send = %v, want a refusal to send a password in clear", err)
	}
	_, sessions := srv.snapshot()
	for _, s := range sessions {
		if s.pass != "" || s.delivered {
			t.Errorf("the password reached the server or a message was sent: %+v", s)
		}
	}
}

func TestSMTP_StartTLSRequiredWhenNotOffered(t *testing.T) {
	srv := startSMTP(t, nil) // does not advertise STARTTLS
	err := srv.client(TLSStartTLS).Send(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("Send = %v, want an error that STARTTLS is missing (no silent downgrade)", err)
	}
	_, sessions := srv.snapshot()
	for _, s := range sessions {
		if s.delivered || s.from != "" {
			t.Errorf("the client went on without TLS: %+v", s)
		}
	}
}

func TestSMTP_CertificateIsVerifiedUnlessTestSwitchIsOn(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.advertiseSTARTTLS = true })
	c := srv.client(TLSStartTLS)
	c.InsecureSkipVerify = false // the default: a self-signed certificate must be refused
	err := c.Send(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "starttls") {
		t.Fatalf("Send to a self-signed server = %v, want a certificate error", err)
	}
	_, sessions := srv.snapshot()
	for _, s := range sessions {
		if s.delivered {
			t.Error("a message went to a server whose certificate was not verified")
		}
	}
	// Implicit TLS too.
	srv2 := startSMTP(t, func(s *fakeSMTP) { s.implicitTLS = true })
	c2 := srv2.client(TLSImplicit)
	c2.InsecureSkipVerify = false
	if err := c2.Send(context.Background(), sample()); err == nil {
		t.Error("implicit TLS accepted a self-signed certificate")
	}
}

func TestSMTP_RecipientRejectedIsAnError(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.rejectRcpt = true })
	err := srv.client(TLSNone).Send(context.Background(), sample())
	if err == nil || !strings.Contains(err.Error(), "RCPT TO") {
		t.Fatalf("Send = %v, want a RCPT TO error", err)
	}
}

func TestSMTP_InvalidFromIsRefusedBeforeConnecting(t *testing.T) {
	srv := startSMTP(t, nil)
	for _, from := range []string{"", "not an address", "a@b.com\r\nBcc: x@y.com"} {
		c := srv.client(TLSNone)
		c.From = from
		if err := c.Send(context.Background(), sample()); err == nil {
			t.Errorf("Send with From %q succeeded", from)
		}
	}
	if n, _ := srv.snapshot(); n != 0 {
		t.Errorf("%d connections were made for an invalid From", n)
	}
}

func TestSMTP_ConnectionRefusedIsAnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close() // nothing listens on this port now
	c := SMTP{Host: "127.0.0.1", Port: port, From: "a@example.com", TLS: TLSNone, Timeout: 2 * time.Second}
	if err := c.Send(context.Background(), sample()); err == nil || !strings.Contains(err.Error(), "connecting") {
		t.Errorf("Send = %v, want a connecting error", err)
	}
}

// ---- header injection ---------------------------------------------------------

func TestSMTP_LineBreaksInRecipientAreRefusedBeforeConnecting(t *testing.T) {
	srv := startSMTP(t, nil)
	for name, to := range map[string]string{
		"CRLF Bcc":        "victim@example.com\r\nBcc: attacker@evil.example",
		"LF Bcc":          "victim@example.com\nBcc: attacker@evil.example",
		"CR only":         "victim@example.com\rBcc: attacker@evil.example",
		"CRLF in local":   "vic\r\ntim@example.com",
		"display name":    "Eve <eve@evil.example>",
		"two recipients":  "a@example.com, b@example.com",
		"empty":           "",
		"NUL":             "vic\x00tim@example.com",
		"CRLF after name": "Eve\r\nBcc: x@y.z <eve@evil.example>",
	} {
		m := sample()
		m.To = to
		if err := srv.client(TLSNone).Send(context.Background(), m); err == nil {
			t.Errorf("%s: Send to %q succeeded, want an error", name, to)
		}
	}
	if n, _ := srv.snapshot(); n != 0 {
		t.Errorf("%d connections were opened for refused recipients, want 0", n)
	}
}

func TestSMTP_LineBreaksInSubjectAreRefusedBeforeConnecting(t *testing.T) {
	srv := startSMTP(t, nil)
	for name, subject := range map[string]string{
		"CRLF Bcc": "Hi\r\nBcc: attacker@evil.example",
		"LF Bcc":   "Hi\nBcc: attacker@evil.example",
		"CR only":  "Hi\rBcc: attacker@evil.example",
		"CRLF end": "Hi\r\n",
	} {
		m := sample()
		m.Subject = subject
		err := srv.client(TLSNone).Send(context.Background(), m)
		if !errors.Is(err, ErrHeaderInjection) {
			t.Errorf("%s: Send = %v, want ErrHeaderInjection", name, err)
		}
	}
	if n, _ := srv.snapshot(); n != 0 {
		t.Errorf("%d connections were opened for refused subjects, want 0", n)
	}
}

func TestBuild_RefusesLineBreaksInEveryHeaderValue(t *testing.T) {
	ok := &mail.Address{Name: "Jobs", Address: "no-reply@example.com"}
	for name, tc := range map[string]struct {
		from *mail.Address
		to   string
		subj string
	}{
		"from name":    {&mail.Address{Name: "Jobs\r\nBcc: x@y.z", Address: "a@example.com"}, "b@example.com", "s"},
		"from address": {&mail.Address{Address: "a@example.com\nBcc: x@y.z"}, "b@example.com", "s"},
		"to":           {ok, "b@example.com\r\nBcc: x@y.z", "s"},
		"subject":      {ok, "b@example.com", "s\r\nBcc: x@y.z"},
	} {
		if _, err := build(tc.from, tc.to, Message{Subject: tc.subj, Text: "t"}); !errors.Is(err, ErrHeaderInjection) {
			t.Errorf("%s: build = %v, want ErrHeaderInjection", name, err)
		}
	}
}

func TestSMTP_BodyCannotSmuggleSMTPCommands(t *testing.T) {
	srv := startSMTP(t, nil)
	m := sample()
	m.Text = "hi\r\n.\r\nMAIL FROM:<evil@evil.example>\r\nRCPT TO:<victim@example.org>\r\nDATA\r\nsmuggled\r\n.\r\nQUIT\r\n"
	if err := srv.client(TLSNone).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	got := delivered(t, srv) // exactly one message, to exactly one recipient
	if len(got.rcpts) != 1 || got.rcpts[0] != "alice@example.com" || got.from != "no-reply@jobs.example.com" {
		t.Errorf("envelope = %q -> %v; the body changed it", got.from, got.rcpts)
	}
	for _, c := range got.commands {
		if strings.Contains(c, "evil") || strings.Contains(c, "victim") {
			t.Errorf("the server saw a command from the body: %q", c)
		}
	}
	text := parseMessage(t, got.data).text
	if text != m.Text {
		t.Errorf("body did not round-trip:\n got %q\nwant %q", text, m.Text)
	}
}

// ---- message format -----------------------------------------------------------

type parsed struct {
	header     mail.Header
	text, html string
	contentTyp string
}

func parseMessage(t *testing.T, raw string) parsed {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("the delivered data is not a valid RFC 5322 message: %v\n%s", err, raw)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, %v, want multipart/alternative", msg.Header.Get("Content-Type"), err)
	}
	out := parsed{header: msg.Header, contentTyp: mt}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading a part: %v", err)
		}
		if enc := p.Header.Get("Content-Transfer-Encoding"); enc != "quoted-printable" {
			t.Errorf("part encoding = %q, want quoted-printable", enc)
		}
		body, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatalf("decoding a part: %v", err)
		}
		ctype, cparams, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		if cparams["charset"] != "utf-8" {
			t.Errorf("part %s charset = %q, want utf-8", ctype, cparams["charset"])
		}
		switch ctype {
		case "text/plain":
			out.text = string(body)
		case "text/html":
			out.html = string(body)
		default:
			t.Errorf("unexpected part type %q", ctype)
		}
	}
	return out
}

func TestSMTP_MessageIsBuiltCorrectly(t *testing.T) {
	srv := startSMTP(t, nil)
	m := sample()
	m.Subject = "Vérifiez votre connexion à Remote jobs"
	m.Text += "Réponse: café = 5 €. A line that is long enough to need a soft line break in quoted-printable: " + strings.Repeat("word ", 30) + "\r\n.starts with a dot\r\n"
	if err := srv.client(TLSNone).Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	got := parseMessage(t, delivered(t, srv).data)

	if got.text != m.Text {
		t.Errorf("text part did not round-trip:\n got %q\nwant %q", got.text, m.Text)
	}
	if got.html != m.HTML {
		t.Errorf("html part did not round-trip:\n got %q\nwant %q", got.html, m.HTML)
	}
	h := got.header
	from, err := mail.ParseAddress(h.Get("From"))
	if err != nil || from.Address != "no-reply@jobs.example.com" || from.Name != "Sign-in" {
		t.Errorf("From = %q, %v", h.Get("From"), err)
	}
	to, err := h.AddressList("To")
	if err != nil || len(to) != 1 || to[0].Address != "alice@example.com" {
		t.Errorf("To = %v, %v", h["To"], err)
	}
	if len(h["To"]) != 1 || len(h["Subject"]) != 1 || len(h["From"]) != 1 {
		t.Errorf("a header is repeated: %v", h)
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
	if err != nil || subj != m.Subject {
		t.Errorf("Subject decodes to %q, %v, want %q", subj, err, m.Subject)
	}
	if strings.ContainsAny(h.Get("Subject"), "é") {
		t.Errorf("the raw Subject header holds non-ASCII: %q", h.Get("Subject"))
	}
	if h.Get("MIME-Version") != "1.0" {
		t.Errorf("MIME-Version = %q", h.Get("MIME-Version"))
	}
	if h.Get("Auto-Submitted") != "auto-generated" {
		t.Errorf("Auto-Submitted = %q, want auto-generated (so auto-replies are not sent)", h.Get("Auto-Submitted"))
	}
	if id := h.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@jobs.example.com>") || len(id) < 30 {
		t.Errorf("Message-ID = %q, want <random@from-domain>", id)
	}
	if d, err := h.Date(); err != nil || time.Since(d) > time.Minute || time.Until(d) > time.Minute {
		t.Errorf("Date = %q, %v, want now", h.Get("Date"), err)
	}
	// Text comes before HTML: mail clients show the last part they can render.
	if got.text == "" || got.html == "" {
		t.Error("a part is missing")
	}
}

func TestBuild_OnlyTextWhenThereIsNoHTMLAndLinesAreCRLF(t *testing.T) {
	from, _ := mail.ParseAddress("a@example.com")
	raw, err := build(from, "b@example.com", Message{Subject: "s", Text: "plain only"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("text/html")) {
		t.Error("an html part was written for an empty HTML")
	}
	// Every line ends in CRLF: a bare LF in an SMTP stream is a smuggling vector.
	if bytes.Count(raw, []byte("\n")) != bytes.Count(raw, []byte("\r\n")) {
		t.Errorf("the message has bare LF line endings:\n%q", raw)
	}
	if !bytes.Contains(raw, []byte("\r\n\r\n")) {
		t.Error("no blank line between headers and body")
	}
	p := parseMessage(t, string(raw))
	if p.text != "plain only" || p.html != "" {
		t.Errorf("parts = %q / %q", p.text, p.html)
	}
}

func TestBuild_MessageIDsAreUnique(t *testing.T) {
	from, _ := mail.ParseAddress("a@example.com")
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		raw, err := build(from, "b@example.com", Message{Subject: "s", Text: "t"})
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := mail.ReadMessage(bytes.NewReader(raw))
		id := msg.Header.Get("Message-ID")
		if seen[id] {
			t.Fatalf("Message-ID %q repeated", id)
		}
		seen[id] = true
	}
}

// ---- timeouts -----------------------------------------------------------------

func TestSMTP_TimeoutBoundsASilentServer(t *testing.T) {
	for _, hang := range []string{"greeting", "MAIL", "DATA"} {
		t.Run(hang, func(t *testing.T) {
			srv := startSMTP(t, func(s *fakeSMTP) { s.hangAt = hang })
			c := srv.client(TLSNone)
			c.Timeout = 300 * time.Millisecond
			start := time.Now()
			err := c.Send(context.Background(), sample())
			if err == nil {
				t.Fatal("Send to a server that stops answering succeeded")
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("Send took %v with a 300ms timeout", d)
			}
		})
	}
}

func TestSMTP_ContextShorterThanTimeoutWins(t *testing.T) {
	srv := startSMTP(t, func(s *fakeSMTP) { s.hangAt = "greeting" })
	c := srv.client(TLSNone)
	c.Timeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Send(ctx, sample()); err == nil {
		t.Fatal("Send succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Send took %v although the context expired after 200ms", d)
	}
}

func TestSMTP_CancelledContextFailsFast(t *testing.T) {
	srv := startSMTP(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.client(TLSNone).Send(ctx, sample()); err == nil {
		t.Error("Send with a cancelled context succeeded")
	}
	_, sessions := srv.snapshot()
	for _, s := range sessions {
		if s.delivered {
			t.Error("a message was delivered on a cancelled context")
		}
	}
}

func TestSMTP_ZeroTimeoutUsesTheDefaultAndStillDelivers(t *testing.T) {
	srv := startSMTP(t, nil)
	c := srv.client(TLSNone)
	c.Timeout = 0
	if err := c.Send(context.Background(), sample()); err != nil {
		t.Errorf("Send with the default timeout: %v", err)
	}
}

// ---- log mailer ---------------------------------------------------------------

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestLog_PrintsTheMessageAndSendsNothing(t *testing.T) {
	buf := &lockedBuf{}
	var m Mailer = Log{Logger: slog.New(slog.NewTextHandler(buf, nil))}
	msg := sample()
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"alice@example.com", "Your sign-in link", "auth/verify#token=AAAA", "not sent"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output lacks %q: %s", want, out)
		}
	}
}

func TestLog_IgnoresCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (Log{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Send(ctx, sample()); err != nil {
		t.Errorf("Send = %v", err)
	}
}

// Both implementations satisfy the interface.
var (
	_ Mailer = SMTP{}
	_ Mailer = Log{}
)
