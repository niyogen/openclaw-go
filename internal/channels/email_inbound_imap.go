package channels

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// IMAPFetcher implements EmailFetcher against a real IMAP server using
// emersion's imapclient. The fetcher reconnects transparently if the
// underlying TCP connection drops between calls.
//
// Concurrency: the poller invokes Connect / FetchNew / Close in serial
// from a single goroutine, so the internal *Client doesn't need a mutex
// beyond what guards the lifecycle bit (connected flag).
type IMAPFetcher struct {
	addr     string // "imap.example.com:993"
	username string
	password string
	mailbox  string // typically "INBOX"
	useTLS   bool   // 993 = implicit TLS; 143 = STARTTLS-or-plain

	mu        sync.Mutex
	client    *imapclient.Client
	connected bool
}

// NewIMAPFetcher constructs a fetcher. Use port 993 with useTLS=true for
// the standard IMAPS workflow; port 143 with useTLS=false issues plain
// IMAP and is suitable only for local test servers. Mailbox defaults to
// "INBOX" when empty.
func NewIMAPFetcher(host string, port int, useTLS bool, username, password, mailbox string) *IMAPFetcher {
	if port == 0 {
		if useTLS {
			port = 993
		} else {
			port = 143
		}
	}
	if strings.TrimSpace(mailbox) == "" {
		mailbox = "INBOX"
	}
	return &IMAPFetcher{
		addr:     fmt.Sprintf("%s:%d", host, port),
		username: username,
		password: password,
		mailbox:  mailbox,
		useTLS:   useTLS,
	}
}

func (f *IMAPFetcher) Connect(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connected {
		return nil
	}

	var c *imapclient.Client
	var err error
	if f.useTLS {
		// ServerName is the bare host (no port) — strip if present.
		host := f.addr
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		c, err = imapclient.DialTLS(f.addr, &imapclient.Options{
			TLSConfig: &tls.Config{ServerName: host},
		})
	} else {
		// Plain unencrypted IMAP. Intended for local test servers only;
		// production IMAP should use port 993 with useTLS=true.
		// STARTTLS-on-143 is not currently supported — the v2 imapclient
		// requires a working TLS handshake which most local test servers
		// (including imapmemserver) don't provide.
		c, err = imapclient.DialInsecure(f.addr, nil)
	}
	if err != nil {
		return fmt.Errorf("imap dial %s: %w", f.addr, err)
	}

	if err := c.Login(f.username, f.password).Wait(); err != nil {
		c.Close()
		return fmt.Errorf("imap login: %w", err)
	}
	if _, err := c.Select(f.mailbox, nil).Wait(); err != nil {
		c.Logout()
		c.Close()
		return fmt.Errorf("imap select %q: %w", f.mailbox, err)
	}
	f.client = c
	f.connected = true
	return nil
}

func (f *IMAPFetcher) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected || f.client == nil {
		return nil
	}
	// Logout is best-effort — even if it fails (e.g., server dropped us)
	// the close below releases local resources.
	_ = f.client.Logout().Wait()
	err := f.client.Close()
	f.client = nil
	f.connected = false
	return err
}

// FetchNew returns all unread (\Seen flag absent) messages and marks them
// seen on the server. Connection failures cause Close + reset so the next
// caller triggers a fresh Connect.
func (f *IMAPFetcher) FetchNew(ctx context.Context) ([]EmailMessage, error) {
	f.mu.Lock()
	c := f.client
	connected := f.connected
	f.mu.Unlock()
	if !connected || c == nil {
		return nil, fmt.Errorf("imap fetcher not connected")
	}

	searchData, err := c.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		f.dropConnection()
		return nil, fmt.Errorf("imap search unseen: %w", err)
	}
	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}

	uidSet := imap.UIDSetNum(uids...)
	fetchOpts := &imap.FetchOptions{
		UID:      true,
		Envelope: true,
		BodySection: []*imap.FetchItemBodySection{{
			Specifier: imap.PartSpecifierText,
			Peek:      true, // don't auto-mark seen — we set the flag ourselves below
		}},
	}
	msgs, err := c.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		f.dropConnection()
		return nil, fmt.Errorf("imap fetch: %w", err)
	}

	out := make([]EmailMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, emailMessageFromBuffer(m))
	}

	// Mark all fetched messages as seen so we don't redeliver on the next
	// tick. Errors here are non-fatal — the next FetchNew will return
	// the same UIDs and the dedupe burden falls on the caller's handler.
	_, _ = c.Store(uidSet, &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil).Collect()

	return out, nil
}

func (f *IMAPFetcher) dropConnection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client != nil {
		_ = f.client.Close()
	}
	f.client = nil
	f.connected = false
}

// emailMessageFromBuffer extracts the fields we care about from emersion's
// FetchMessageBuffer. Tolerates missing parts — a malformed message
// becomes an empty EmailMessage rather than a hard error.
func emailMessageFromBuffer(m *imapclient.FetchMessageBuffer) EmailMessage {
	out := EmailMessage{UID: uint32(m.UID)}
	if m.Envelope != nil {
		out.Subject = m.Envelope.Subject
		if len(m.Envelope.From) > 0 {
			a := m.Envelope.From[0]
			out.From = a.Mailbox + "@" + a.Host
		}
	}
	// Body — first text section, if any.
	for _, sec := range m.BodySection {
		if len(sec.Bytes) > 0 {
			out.Body = string(sec.Bytes)
			break
		}
	}
	return out
}
