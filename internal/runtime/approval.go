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
	DecidedAt *time.Time     `json:"decidedAt,omitempty"`
}

// ApprovalQueue holds pending approval requests and lets callers await decisions.
type ApprovalQueue struct {
	mu       sync.Mutex
	requests map[string]*approvalEntry
}

type approvalEntry struct {
	req  *ApprovalRequest
	done chan struct{}
}

// NewApprovalQueue creates an empty queue.
func NewApprovalQueue() *ApprovalQueue {
	return &ApprovalQueue{requests: map[string]*approvalEntry{}}
}

// Enqueue adds a new request and returns its id.
func (q *ApprovalQueue) Enqueue(req *ApprovalRequest) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.requests[req.ID] = &approvalEntry{req: req, done: make(chan struct{})}
}

// Wait blocks until the request is decided or the context is cancelled.
func (q *ApprovalQueue) Wait(ctx context.Context, id string) (ApprovalStatus, error) {
	q.mu.Lock()
	entry, ok := q.requests[id]
	q.mu.Unlock()
	if !ok {
		return ApprovalPending, errors.New("approval request not found")
	}
	select {
	case <-ctx.Done():
		return ApprovalPending, ctx.Err()
	case <-entry.done:
		return entry.req.Status, nil
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
	return nil
}

// List returns all pending requests.
func (q *ApprovalQueue) List() []*ApprovalRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
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
