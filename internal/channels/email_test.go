package channels

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/smtp"
	"strings"
	"testing"
)

// fakeEmailDialer captures every SMTP verb and the body bytes so tests can
// assert what would have gone over the wire without needing a real SMTP
// server. Each Dial returns a fresh fakeEmailClient.
type fakeEmailDialer struct {
	clients  []*fakeEmailClient
	failDial error
}

func (f *fakeEmailDialer) Dial(_ context.Context, _ string, _ bool, host string) (EmailClient, error) {
	if f.failDial != nil {
		return nil, f.failDial
	}
	c := &fakeEmailClient{host: host, body: &bytes.Buffer{}, hasStartTLS: true}
	f.clients = append(f.clients, c)
	return c, nil
}

type fakeEmailClient struct {
	host          string
	hasStartTLS   bool
	starttlsCalls int
	authCalls     int
	mailFrom      string
	rcptTo        string
	body          *bytes.Buffer
	quitCalls     int
	failAuth      bool
}

func (f *fakeEmailClient) StartTLS(_ *tls.Config) error {
	f.starttlsCalls++
	return nil
}
func (f *fakeEmailClient) Auth(_ smtp.Auth) error {
	f.authCalls++
	if f.failAuth {
		return errors.New("bad credentials")
	}
	return nil
}
func (f *fakeEmailClient) Mail(from string) error      { f.mailFrom = from; return nil }
func (f *fakeEmailClient) Rcpt(to string) error        { f.rcptTo = to; return nil }
func (f *fakeEmailClient) Data() (writerCloser, error) { return &nopWriteCloser{f.body}, nil }
func (f *fakeEmailClient) Quit() error                 { f.quitCalls++; return nil }
func (f *fakeEmailClient) Extension(ext string) (bool, string) {
	if ext == "STARTTLS" {
		return f.hasStartTLS, ""
	}
	return false, ""
}

type nopWriteCloser struct{ *bytes.Buffer }

func (n *nopWriteCloser) Close() error { return nil }

func TestEmailSendHappyPath(t *testing.T) {
	dialer := &fakeEmailDialer{}
	ch := NewEmailChannel("smtp.example.com", 587, "bot@example.com", "pw", "bot@example.com")
	ch.dialer = dialer

	err := ch.Send(context.Background(), OutboundMessage{
		SessionID: "s1",
		Target:    "user@example.com",
		Message:   "Daily summary\nAll quiet.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dialer.clients) != 1 {
		t.Fatalf("expected 1 dial, got %d", len(dialer.clients))
	}
	c := dialer.clients[0]
	if c.starttlsCalls != 1 {
		t.Fatalf("STARTTLS calls: got %d want 1 (server advertised it)", c.starttlsCalls)
	}
	if c.authCalls != 1 {
		t.Fatalf("Auth calls: got %d want 1", c.authCalls)
	}
	if c.mailFrom != "bot@example.com" {
		t.Fatalf("MAIL FROM: %q", c.mailFrom)
	}
	if c.rcptTo != "user@example.com" {
		t.Fatalf("RCPT TO: %q", c.rcptTo)
	}
	body := c.body.String()
	if !strings.Contains(body, "Subject: [s1] Daily summary") {
		t.Fatalf("subject missing or wrong: %q", body)
	}
	if !strings.Contains(body, "All quiet.") {
		t.Fatalf("body content missing: %q", body)
	}
	if !strings.Contains(body, "From: bot@example.com") {
		t.Fatal("From header missing")
	}
}

func TestEmailDisabledWhenHostEmpty(t *testing.T) {
	ch := NewEmailChannel("", 587, "bot@example.com", "pw", "")
	// Disabled channels return nil — same behaviour as Discord/Slack/etc.
	if err := ch.Send(context.Background(), OutboundMessage{Target: "x@y.com", Message: "hi"}); err != nil {
		t.Fatalf("expected nil error for disabled channel, got %v", err)
	}
}

func TestEmailMissingRecipient(t *testing.T) {
	ch := NewEmailChannel("smtp.example.com", 587, "bot@example.com", "pw", "")
	ch.dialer = &fakeEmailDialer{}
	if err := ch.Send(context.Background(), OutboundMessage{Message: "hi"}); err == nil {
		t.Fatal("expected error for missing recipient")
	}
}

func TestEmailNoSTARTTLSWhenServerLacks(t *testing.T) {
	dialer := &fakeEmailDialer{}
	ch := NewEmailChannel("smtp.example.com", 587, "bot@example.com", "pw", "")
	ch.dialer = dialer

	// Override clients factory by pre-allocating one without STARTTLS support.
	// Simulate by intercepting Dial through a wrapper.
	wrappingDialer := &fakeEmailDialerNoTLS{}
	ch.dialer = wrappingDialer

	if err := ch.Send(context.Background(), OutboundMessage{Target: "u@e.com", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(wrappingDialer.clients) != 1 || wrappingDialer.clients[0].starttlsCalls != 0 {
		t.Fatalf("STARTTLS should not have been called; calls=%d", wrappingDialer.clients[0].starttlsCalls)
	}
}

type fakeEmailDialerNoTLS struct {
	clients []*fakeEmailClient
}

func (f *fakeEmailDialerNoTLS) Dial(_ context.Context, _ string, _ bool, host string) (EmailClient, error) {
	c := &fakeEmailClient{host: host, body: &bytes.Buffer{}, hasStartTLS: false}
	f.clients = append(f.clients, c)
	return c, nil
}

func TestEmailImplicitTLSOnPort465(t *testing.T) {
	// Port 465 means useTLS=true and we should NOT then negotiate STARTTLS.
	var capturedUseTLS bool
	dialer := &assertingDialer{onDial: func(useTLS bool) { capturedUseTLS = useTLS }}
	ch := NewEmailChannel("smtp.example.com", 465, "bot@example.com", "pw", "")
	ch.dialer = dialer

	if err := ch.Send(context.Background(), OutboundMessage{Target: "u@e.com", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !capturedUseTLS {
		t.Fatal("port 465 must dial with implicit TLS")
	}
	if dialer.client.starttlsCalls != 0 {
		t.Fatal("STARTTLS must not run on implicit-TLS connections")
	}
}

type assertingDialer struct {
	onDial func(useTLS bool)
	client *fakeEmailClient
}

func (a *assertingDialer) Dial(_ context.Context, _ string, useTLS bool, host string) (EmailClient, error) {
	a.onDial(useTLS)
	a.client = &fakeEmailClient{host: host, body: &bytes.Buffer{}, hasStartTLS: true}
	return a.client, nil
}

func TestEmailAuthFailurePropagates(t *testing.T) {
	dialer := &fakeEmailDialer{}
	ch := NewEmailChannel("smtp.example.com", 587, "bot@example.com", "wrong", "")
	ch.dialer = dialer

	// Patch: set failAuth on the next fake client by intercepting via a custom
	// dialer.
	failing := &failingAuthDialer{}
	ch.dialer = failing
	err := ch.Send(context.Background(), OutboundMessage{Target: "u@e.com", Message: "hi"})
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

type failingAuthDialer struct{}

func (failingAuthDialer) Dial(_ context.Context, _ string, _ bool, host string) (EmailClient, error) {
	return &fakeEmailClient{host: host, body: &bytes.Buffer{}, hasStartTLS: true, failAuth: true}, nil
}

func TestEmailDialErrorPropagates(t *testing.T) {
	// Dial errors must reach the caller as a wrapped error — silent fallback
	// to "no send" would hide network/DNS issues from operators.
	failing := failingDialDialer{err: errors.New("connection refused")}
	ch := NewEmailChannel("smtp.unreachable", 587, "u", "p", "")
	ch.dialer = failing

	err := ch.Send(context.Background(), OutboundMessage{Target: "u@e.com", Message: "hi"})
	if err == nil {
		t.Fatal("expected dial error to propagate")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error should wrap dial error: %v", err)
	}
}

type failingDialDialer struct{ err error }

func (f failingDialDialer) Dial(_ context.Context, _ string, _ bool, _ string) (EmailClient, error) {
	return nil, f.err
}

func TestBuildEmailSubjectTruncates(t *testing.T) {
	long := strings.Repeat("a", 200)
	subj := buildEmailSubject(OutboundMessage{SessionID: "sx", Message: long})
	if !strings.HasPrefix(subj, "[sx] ") {
		t.Fatalf("subject prefix wrong: %q", subj)
	}
	if len(subj) > 80 { // [sx]  + 60 + ellipsis byte sequence
		t.Fatalf("subject too long: %d %q", len(subj), subj)
	}
	if !strings.HasSuffix(subj, "…") {
		t.Fatalf("expected ellipsis suffix: %q", subj)
	}
}

func TestBuildEmailSubjectFallback(t *testing.T) {
	subj := buildEmailSubject(OutboundMessage{Message: "  "})
	if !strings.Contains(subj, "openclaw notification") {
		t.Fatalf("fallback subject wrong: %q", subj)
	}
}
