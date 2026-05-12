// Package push delivers Web Push notifications (RFC 8030) to registered
// browser subscriptions using the VAPID protocol. It is consumed by the
// gateway to surface approval requests, real-time agent replies, and other
// out-of-band events to operators without requiring them to long-poll.
//
// The package is split into three concerns:
//
//   - Service: lifecycle (key generation, subscription registration,
//     fan-out send). Owned by the gateway.
//   - Store: file-backed persistence for subscriptions, mode 0o600 to
//     keep the per-user endpoint URLs (which act like bearer tokens for
//     receiving pushes) off-disk-readable to other accounts.
//   - Sender: the testable seam between the gateway and the actual
//     webpush-go library. Tests substitute an in-memory Sender so the
//     unit suite never hits a real push endpoint.
package push

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"openclaw-go/internal/fileutil"
)

// Subscription is the gateway-side view of a browser's PushSubscription.
// The webpush-go library has its own Subscription type with just
// Endpoint+Keys; we wrap it with ID/Label/CreatedAt so operators can
// manage subscriptions over RPC.
type Subscription struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"` // human-readable hint, e.g. "Alice's iPhone"
	Endpoint  string    `json:"endpoint"`
	P256dhKey string    `json:"p256dh"`
	AuthKey   string    `json:"auth"`
	CreatedAt time.Time `json:"createdAt"`
}

// Sender is the testable seam wrapping the webpush-go library. The
// production impl in webpushSender posts to Endpoint with VAPID auth.
type Sender interface {
	Send(ctx context.Context, sub Subscription, payload []byte) error
}

// keysFile holds the VAPID keypair + contact email. Persisted as
// `${dataDir}/push-keys.json` at mode 0o600. The private key is the
// signing key for VAPID JWTs and MUST NOT be exposed; the public key is
// safe to hand to browsers (they encrypt their subscription against it).
type keysFile struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
	Contact    string `json:"contact"` // RFC 8292 sub claim — usually mailto:owner@example.com
}

// Service combines storage + sender + VAPID keys. Construct one per
// gateway instance via NewService.
type Service struct {
	mu      sync.Mutex
	dataDir string
	keys    keysFile
	subs    map[string]*Subscription
	sender  Sender
}

// NewService loads (or generates+persists) VAPID keys and the subscription
// store from dataDir. The contact email is recorded with the keys so the
// VAPID JWT presents a consistent `sub` claim to push providers.
//
// First-use generation writes the keypair to disk and returns. Operators
// should back up `push-keys.json` — losing the private key means every
// existing browser subscription becomes undeliverable (the endpoints
// won't accept JWTs signed by a different key).
func NewService(dataDir, contact string) (*Service, error) {
	s := &Service{
		dataDir: dataDir,
		subs:    map[string]*Subscription{},
		// sender is set by bindRealSender below once VAPID keys are loaded.
		// Until then any Send call would NPE; that can't happen in practice
		// because bindRealSender runs before NewService returns.
	}
	if err := s.loadKeys(contact); err != nil {
		return nil, err
	}
	if err := s.loadSubscriptions(); err != nil {
		return nil, err
	}
	// Bind the production sender now that keys are loaded. Tests that need
	// an in-memory sender call SetSender after construction to replace it.
	s.bindRealSender()
	return s, nil
}

// SetSender swaps the underlying Sender. Used by tests to substitute an
// in-memory implementation.
func (s *Service) SetSender(sender Sender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sender = sender
}

// PublicKey returns the VAPID public key suitable for handing to a
// browser's PushManager.subscribe({applicationServerKey}) call.
func (s *Service) PublicKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys.PublicKey
}

// Subscribe registers a browser's PushSubscription. Returns the assigned
// ID. Endpoint/p256dh/auth are taken straight from the browser's
// subscription.toJSON(); label is an optional operator-supplied hint.
func (s *Service) Subscribe(endpoint, p256dh, auth, label string) (Subscription, error) {
	if endpoint == "" || p256dh == "" || auth == "" {
		return Subscription{}, errors.New("subscribe: endpoint/p256dh/auth all required")
	}
	id, err := generateID()
	if err != nil {
		return Subscription{}, err
	}
	sub := Subscription{
		ID:        id,
		Label:     label,
		Endpoint:  endpoint,
		P256dhKey: p256dh,
		AuthKey:   auth,
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[id] = &sub
	if err := s.saveSubscriptionsLocked(); err != nil {
		delete(s.subs, id)
		return Subscription{}, err
	}
	return sub, nil
}

// Unsubscribe removes a subscription. Idempotent — removing an unknown
// ID returns no error (matches REST DELETE convention).
func (s *Service) Unsubscribe(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[id]; !ok {
		return nil
	}
	delete(s.subs, id)
	return s.saveSubscriptionsLocked()
}

// List returns all registered subscriptions, redacted-friendly (the raw
// endpoint URL is included because operators sometimes need it to
// manually revoke from the browser's UI; for display contexts callers
// should truncate it).
func (s *Service) List() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, v := range s.subs {
		out = append(out, *v)
	}
	return out
}

// SendAll fans `payload` out to every registered subscription. Errors are
// aggregated — a single broken subscription doesn't stop the rest.
// Caller is responsible for marshalling the payload to JSON (most clients
// expect a JSON object the service worker can pass to showNotification).
func (s *Service) SendAll(ctx context.Context, payload []byte) error {
	s.mu.Lock()
	subs := make([]Subscription, 0, len(s.subs))
	for _, v := range s.subs {
		subs = append(subs, *v)
	}
	sender := s.sender
	s.mu.Unlock()

	var firstErr error
	for _, sub := range subs {
		if err := sender.Send(ctx, sub, payload); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("push to %s: %w", sub.ID, err)
			}
		}
	}
	return firstErr
}

// SendOne pushes a payload to a single subscription by ID. Returns an
// error if the id is unknown OR the Sender reports a failure.
func (s *Service) SendOne(ctx context.Context, id string, payload []byte) error {
	s.mu.Lock()
	sub, ok := s.subs[id]
	sender := s.sender
	subCopy := Subscription{}
	if ok {
		subCopy = *sub
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("subscription not found")
	}
	return sender.Send(ctx, subCopy, payload)
}

func (s *Service) keysPath() string {
	return filepath.Join(s.dataDir, "push-keys.json")
}

func (s *Service) subscriptionsPath() string {
	return filepath.Join(s.dataDir, "push-subscriptions.json")
}

func (s *Service) loadKeys(contact string) error {
	raw, err := os.ReadFile(s.keysPath())
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.keys); err != nil {
			return fmt.Errorf("push: parse %s: %w", s.keysPath(), err)
		}
		// Contact email is allowed to change at runtime via config; keep
		// the persisted keys but update the in-memory contact.
		if contact != "" {
			s.keys.Contact = contact
		}
		return nil
	}
	// First use — generate and persist.
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return fmt.Errorf("push: generate VAPID keys: %w", err)
	}
	s.keys = keysFile{
		PublicKey:  pub,
		PrivateKey: priv,
		Contact:    contact,
	}
	return s.saveKeysLocked()
}

func (s *Service) saveKeysLocked() error {
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.keys, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(s.keysPath(), raw, 0o600)
}

func (s *Service) loadSubscriptions() error {
	raw, err := os.ReadFile(s.subscriptionsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var list []*Subscription
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("push: parse %s: %w", s.subscriptionsPath(), err)
	}
	for _, sub := range list {
		if sub == nil || sub.ID == "" {
			continue
		}
		s.subs[sub.ID] = sub
	}
	return nil
}

func (s *Service) saveSubscriptionsLocked() error {
	list := make([]*Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		list = append(list, sub)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFile(s.subscriptionsPath(), raw, 0o600)
}

// generateID returns a 16-byte hex-encoded random ID. Used for
// subscriptions — collisions are essentially impossible at any realistic
// gateway scale.
func generateID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// keyedSender wraps webpush-go's SendNotificationWithContext with a fixed
// VAPID keypair. The Service constructs one of these at init so
// SendAll/SendOne don't have to thread keys through every call.
type keyedSender struct {
	privateKey string
	publicKey  string
	contact    string
}

func (k keyedSender) Send(ctx context.Context, sub Subscription, payload []byte) error {
	wpSub := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.P256dhKey,
			Auth:   sub.AuthKey,
		},
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, wpSub, &webpush.Options{
		Subscriber:      k.contact,
		VAPIDPublicKey:  k.publicKey,
		VAPIDPrivateKey: k.privateKey,
		TTL:             30,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("push provider returned %d", resp.StatusCode)
	}
	return nil
}

// bindRealSender swaps the placeholder webpushSender for a keyedSender
// bound to the loaded VAPID keys. Called by NewService after key load.
func (s *Service) bindRealSender() {
	s.mu.Lock()
	s.sender = keyedSender{
		privateKey: s.keys.PrivateKey,
		publicKey:  s.keys.PublicKey,
		contact:    s.keys.Contact,
	}
	s.mu.Unlock()
}
