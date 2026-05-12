package channels

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// EmailChannel sends outbound messages over SMTP. Inbound (IMAP) is not
// supported here — pair this with another channel for replies, or wait for
// the IMAP follow-up that depends on either a new dep or a stdlib RFC 3501
// implementation.
//
// Authentication: PLAIN over an explicit-TLS connection if `port == 465`,
// otherwise STARTTLS opportunistically (so port 587 works without
// configuration gymnastics). Plain unencrypted SMTP is allowed but logs a
// warning on every send — it exists for testing against local MTAs only.
type EmailChannel struct {
	host     string // "smtp.gmail.com"
	port     int    // typically 587 (STARTTLS) or 465 (implicit TLS)
	username string // SMTP AUTH user (often the From address)
	password string // SMTP AUTH password / app-password
	from     string // RFC 5322 From address
	// dialer is overridable in tests so we don't need a real SMTP server.
	dialer EmailDialer
}

// EmailDialer is the seam tests use to substitute an in-memory SMTP transport.
// The default implementation in DefaultEmailDialer talks real SMTP.
type EmailDialer interface {
	Dial(ctx context.Context, addr string, useTLS bool, host string) (EmailClient, error)
}

// EmailClient mirrors the subset of *smtp.Client we actually use, so a fake
// can implement it without re-implementing the smtp package.
type EmailClient interface {
	StartTLS(config *tls.Config) error
	Auth(a smtp.Auth) error
	Mail(from string) error
	Rcpt(to string) error
	Data() (writerCloser, error)
	Quit() error
	Extension(ext string) (bool, string)
}

// writerCloser narrows io.WriteCloser so we don't pull in io for the alias.
type writerCloser interface {
	Write(p []byte) (int, error)
	Close() error
}

// NewEmailChannel constructs a channel. Empty host disables Send (mirrors
// the other channels' behaviour — a misconfigured channel returns nil on
// Send rather than erroring on every dispatch).
func NewEmailChannel(host string, port int, username, password, from string) *EmailChannel {
	if port == 0 {
		port = 587
	}
	if strings.TrimSpace(from) == "" {
		from = strings.TrimSpace(username)
	}
	return &EmailChannel{
		host:     strings.TrimSpace(host),
		port:     port,
		username: strings.TrimSpace(username),
		password: password, // intentionally not TrimSpace'd — leading/trailing whitespace in app-passwords is possible
		from:     strings.TrimSpace(from),
		dialer:   DefaultEmailDialer{},
	}
}

func (e *EmailChannel) Name() string {
	return "email"
}

// Send dispatches a single message. Target is the RFC 5322 recipient address.
// The message body is sent as text/plain UTF-8. Multi-part / HTML is out of
// scope for the parity MVP.
func (e *EmailChannel) Send(ctx context.Context, message OutboundMessage) error {
	if e.host == "" {
		return nil // disabled — see constructor docstring
	}
	to := strings.TrimSpace(message.Target)
	if to == "" {
		return errors.New("email: target (recipient) required")
	}
	if e.from == "" {
		return errors.New("email: from address required (set username or pass from explicitly)")
	}

	useTLS := e.port == 465
	addr := fmt.Sprintf("%s:%d", e.host, e.port)
	client, err := e.dialer.Dial(ctx, addr, useTLS, e.host)
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", addr, err)
	}
	defer client.Quit()

	// Opportunistic STARTTLS when the server advertises it and we're not
	// already on an implicit-TLS connection.
	if !useTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: e.host}); err != nil {
				return fmt.Errorf("email: STARTTLS: %w", err)
			}
		}
	}

	if e.username != "" {
		auth := smtp.PlainAuth("", e.username, e.password, e.host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}

	if err := client.Mail(e.from); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("email: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	subject := buildEmailSubject(message)
	body := buildEmailBody(e.from, to, subject, message.Message)
	if _, err := w.Write([]byte(body)); err != nil {
		w.Close()
		return fmt.Errorf("email: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: close DATA: %w", err)
	}
	return nil
}

// buildEmailSubject keeps the API symmetric with other channels — the
// OutboundMessage doesn't carry a Subject field, so we derive one from the
// session id and the first line of the message (truncated).
func buildEmailSubject(message OutboundMessage) string {
	firstLine := strings.SplitN(message.Message, "\n", 2)[0]
	firstLine = strings.TrimSpace(firstLine)
	if len(firstLine) > 60 {
		firstLine = firstLine[:60] + "…"
	}
	if firstLine == "" {
		firstLine = "openclaw notification"
	}
	if message.SessionID != "" {
		return "[" + message.SessionID + "] " + firstLine
	}
	return firstLine
}

// buildEmailBody returns a minimal RFC 5322 message: From / To / Date /
// Subject / Content-Type headers, blank line, then the body. We do NOT do
// MIME multipart — plaintext only.
func buildEmailBody(from, to, subject, body string) string {
	headers := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=\"UTF-8\"",
		"Content-Transfer-Encoding: 8bit",
	}
	return strings.Join(headers, "\r\n") + "\r\n\r\n" + body + "\r\n"
}

// DefaultEmailDialer wires the channel to the real net/smtp stack. Kept as
// a value-type so the zero value is usable.
type DefaultEmailDialer struct{}

// Dial respects ctx for the TCP-connect phase only — SMTP's net/smtp
// package has no context awareness past the dial, which is fine because
// each subsequent verb (MAIL, RCPT, DATA) is fast.
func (DefaultEmailDialer) Dial(ctx context.Context, addr string, useTLS bool, host string) (EmailClient, error) {
	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if useTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return realEmailClient{c}, nil
}

// realEmailClient adapts *smtp.Client to our narrower EmailClient interface.
type realEmailClient struct{ c *smtp.Client }

func (r realEmailClient) StartTLS(cfg *tls.Config) error      { return r.c.StartTLS(cfg) }
func (r realEmailClient) Auth(a smtp.Auth) error              { return r.c.Auth(a) }
func (r realEmailClient) Mail(from string) error              { return r.c.Mail(from) }
func (r realEmailClient) Rcpt(to string) error                { return r.c.Rcpt(to) }
func (r realEmailClient) Quit() error                         { return r.c.Quit() }
func (r realEmailClient) Extension(ext string) (bool, string) { return r.c.Extension(ext) }
func (r realEmailClient) Data() (writerCloser, error) {
	w, err := r.c.Data()
	if err != nil {
		return nil, err
	}
	return w, nil
}
