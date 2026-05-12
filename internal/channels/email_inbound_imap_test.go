package channels

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// startMemoryIMAP brings up an in-memory IMAP server with one user + an
// INBOX, returns the bound address. The server is registered for teardown
// via t.Cleanup so each test gets a fresh listener.
//
// This is the integration seam — the real IMAPFetcher (not a fake) talks
// to this server over a real TCP socket and exercises the full client
// stack: dial, login, select, search, fetch, store-seen.
func startMemoryIMAP(t *testing.T, username, password string) string {
	t.Helper()

	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(username, password)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create inbox: %v", err)
	}
	memServer.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		// InsecureAuth lets the in-memory server accept LOGIN over a plain
		// (non-TLS) connection. Production servers should NEVER set this —
		// it bypasses the standard "TLS required for credentials" guard.
		InsecureAuth: true,
		NewSession: func(_ *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// appendTestMessage uses imapclient (the SAME client our IMAPFetcher uses)
// to APPEND a message to the server's INBOX so the fetcher can pick it up.
func appendTestMessage(t *testing.T, addr, username, password, raw string) {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("appendTestMessage dial: %v", err)
	}
	defer c.Close()
	if err := c.Login(username, password).Wait(); err != nil {
		t.Fatalf("appendTestMessage login: %v", err)
	}
	cmd := c.Append("INBOX", int64(len(raw)), nil)
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatalf("append write: %v", err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("append wait: %v", err)
	}
	_ = c.Logout().Wait()
}

func TestIMAPFetcherAgainstMemoryServer(t *testing.T) {
	const username = "bot"
	const password = "pw"
	addr := startMemoryIMAP(t, username, password)

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	// Drop one message into INBOX before the fetcher connects.
	rfc5322 := "From: alice@example.com\r\n" +
		"To: bot@example.com\r\n" +
		"Subject: hello-imap-fetcher\r\n" +
		"Date: Mon, 12 May 2026 13:00:00 +0000\r\n" +
		"\r\n" +
		"this is the body line\r\n"
	appendTestMessage(t, addr, username, password, rfc5322)

	f := NewIMAPFetcher(host, port, false, username, password, "INBOX")
	t.Cleanup(func() { _ = f.Close() })

	ctx := context.Background()
	if err := f.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	msgs, err := f.FetchNew(ctx)
	if err != nil {
		t.Fatalf("FetchNew: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 unseen message, got %d", len(msgs))
	}
	got := msgs[0]
	if got.From != "alice@example.com" {
		t.Errorf("from: got %q want alice@example.com", got.From)
	}
	if got.Subject != "hello-imap-fetcher" {
		t.Errorf("subject: got %q", got.Subject)
	}
	if !strings.Contains(got.Body, "this is the body line") {
		t.Errorf("body should contain the appended text; got %q", got.Body)
	}
	if got.UID == 0 {
		t.Error("expected non-zero UID")
	}

	// Second FetchNew should be empty — the prior call marked the message Seen.
	msgs2, err := f.FetchNew(ctx)
	if err != nil {
		t.Fatalf("second FetchNew: %v", err)
	}
	if len(msgs2) != 0 {
		t.Fatalf("expected 0 unseen on second fetch (Seen flag set), got %d", len(msgs2))
	}
}

func TestIMAPFetcherEndToEndThroughPoller(t *testing.T) {
	// Same setup as the unit test but routed through the EmailInboundPoller
	// so we prove the seams compose correctly: fetcher → poller → InboundMessage.
	const username = "bot"
	const password = "pw"
	addr := startMemoryIMAP(t, username, password)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	raw := "From: bob@example.com\r\n" +
		"Subject: poller-test\r\n" +
		"Date: Mon, 12 May 2026 13:01:00 +0000\r\n" +
		"\r\n" +
		"poller-body\r\n"
	appendTestMessage(t, addr, username, password, raw)

	f := NewIMAPFetcher(host, port, false, username, password, "INBOX")
	poller := NewEmailInboundPoller(f, 30*time.Second)

	got := make(chan InboundMessage, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poller.Start(ctx, func(_ context.Context, m InboundMessage) error {
		select {
		case got <- m:
		default:
		}
		return nil
	}, nil)

	select {
	case m := <-got:
		if m.SessionID != "email:bob@example.com" {
			t.Errorf("session id: %q", m.SessionID)
		}
		if !strings.Contains(m.Message, "poller-body") {
			t.Errorf("message body: %q", m.Message)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("poller did not deliver the message within 4s")
	}
}
