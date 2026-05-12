package runtime

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ApprovalStatus is the decision a human/system made on an approval request.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
)

// ApprovalRequest represents a single pending tool-call approval.
type ApprovalRequest struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionId"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Status    ApprovalStatus `json:"status"`
	CreatedAt time.Time      `json:"createdAt"`
	ExpiresAt time.Time      `json:"expiresAt,omitempty"`
	DecidedAt *time.Time     `json:"decidedAt,omitempty"`
}

// ApprovalQueue holds pending approval requests and lets callers await decisions.
type ApprovalQueue struct {
	mu       sync.Mutex
	requests map[string]*approvalEntry
	// onEnqueue, if set, is called (synchronously, without q.mu held) every
	// time a request is added. It exists so callers that wire approvals to
	// out-of-band notifications (hooks, push, channels) can do so without
	// the runtime package depending on hookstore.
	onEnqueue func(req ApprovalRequest)
}

type approvalEntry struct {
	req  *ApprovalRequest
	done chan struct{}
}

// NewApprovalQueue creates an empty queue.
func NewApprovalQueue() *ApprovalQueue {
	return &ApprovalQueue{requests: map[string]*approvalEntry{}}
}

// SetOnEnqueue installs a callback invoked synchronously every time Enqueue
// completes. Passing nil clears the callback. Safe to call from any goroutine.
//
// The callback receives a value-copy of the request (not a pointer), so it
// cannot race against later mutations from Decide/Wait.
func (q *ApprovalQueue) SetOnEnqueue(fn func(req ApprovalRequest)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onEnqueue = fn
}

// Enqueue adds a new request and returns its id.
// If ExpiresAt is zero it is defaulted to 10 minutes from now.
func (q *ApprovalQueue) Enqueue(req *ApprovalRequest) {
	q.mu.Lock()
	if req.ExpiresAt.IsZero() {
		req.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
	}
	q.requests[req.ID] = &approvalEntry{req: req, done: make(chan struct{})}
	cb := q.onEnqueue
	snapshot := *req
	q.mu.Unlock()
	if cb != nil {
		cb(snapshot)
	}
}

// Wait blocks until the request is decided, the context is cancelled, or the
// request expires (when ExpiresAt is non-zero).
func (q *ApprovalQueue) Wait(ctx context.Context, id string) (ApprovalStatus, error) {
	q.mu.Lock()
	entry, ok := q.requests[id]
	q.mu.Unlock()
	if !ok {
		return ApprovalPending, errors.New("approval request not found")
	}

	// Build expiry channel when ExpiresAt is set.
	var expiryCh <-chan time.Time
	if !entry.req.ExpiresAt.IsZero() {
		d := time.Until(entry.req.ExpiresAt)
		if d <= 0 {
			return ApprovalPending, errors.New("approval request expired")
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		expiryCh = timer.C
	}

	select {
	case <-ctx.Done():
		return ApprovalPending, ctx.Err()
	case <-entry.done:
		return entry.req.Status, nil
	case <-expiryCh:
		// Mark as rejected and clean up so the map doesn't grow unboundedly.
		q.mu.Lock()
		if e, ok := q.requests[id]; ok && e.req.Status == ApprovalPending {
			now := time.Now().UTC()
			e.req.Status = ApprovalRejected
			e.req.DecidedAt = &now
			close(e.done)
			delete(q.requests, id)
		}
		q.mu.Unlock()
		return ApprovalRejected, errors.New("approval request expired")
	}
}

// Decide resolves a pending request as approved or rejected.
func (q *ApprovalQueue) Decide(id string, approved bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.requests[id]
	if !ok {
		return errors.New("approval request not found")
	}
	if entry.req.Status != ApprovalPending {
		return errors.New("request already decided")
	}
	now := time.Now().UTC()
	entry.req.DecidedAt = &now
	if approved {
		entry.req.Status = ApprovalApproved
	} else {
		entry.req.Status = ApprovalRejected
	}
	close(entry.done)
	q.pruneLocked()
	return nil
}

// pruneLocked drops decided entries older than 5 minutes AND any pending
// entries whose ExpiresAt has passed. The pending sweep bounds memory even
// when Enqueue is called without a matching Wait — otherwise expired
// requests would linger forever because the expiry timer lives inside Wait.
// Must be called with q.mu held.
func (q *ApprovalQueue) pruneLocked() {
	now := time.Now()
	decidedCutoff := now.Add(-5 * time.Minute)
	for id, e := range q.requests {
		if e.req.Status != ApprovalPending {
			if e.req.DecidedAt != nil && e.req.DecidedAt.Before(decidedCutoff) {
				delete(q.requests, id)
			}
			continue
		}
		if !e.req.ExpiresAt.IsZero() && e.req.ExpiresAt.Before(now) {
			// Pending-and-expired: close the done channel so any late Wait
			// observers unblock with a deterministic Rejected status.
			nowUTC := now.UTC()
			e.req.Status = ApprovalRejected
			e.req.DecidedAt = &nowUTC
			close(e.done)
			delete(q.requests, id)
		}
	}
}

// List returns all pending non-expired requests. Side effect: prunes
// expired-pending and old-decided entries so memory stays bounded even when
// no one is calling Decide.
func (q *ApprovalQueue) List() []*ApprovalRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneLocked()
	out := make([]*ApprovalRequest, 0, len(q.requests))
	for _, e := range q.requests {
		if e.req.Status == ApprovalPending {
			cp := *e.req
			out = append(out, &cp)
		}
	}
	return out
}

// Get returns a single request by id.
func (q *ApprovalQueue) Get(id string) (*ApprovalRequest, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.requests[id]
	if !ok {
		return nil, false
	}
	cp := *e.req
	return &cp, true
}
