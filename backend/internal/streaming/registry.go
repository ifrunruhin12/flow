package streaming

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrUserBlocked    = errors.New("streaming is blocked for this user")
	ErrSessionRevoked = errors.New("streaming session was revoked")
	ErrStreamLimit    = errors.New("concurrent stream limit reached")
	errStreamClosed   = errors.New("stream registration closed")
)

type registryEntry struct {
	id        uint64
	userID    int64
	sessionID string
	cancel    context.CancelCauseFunc
}

// Registry is the process-wide active-stream registry. Registration and user
// blocking share one mutex, so a suspension racing with registration has only
// two outcomes: registration is rejected, or the newly registered context is
// included in the suspension's cancellation set.
type Registry struct {
	mu        sync.Mutex
	maximum   int
	nextID    uint64
	blocked   map[int64]struct{}
	entries   map[uint64]*registryEntry
	byUser    map[int64]map[uint64]*registryEntry
	bySession map[string]map[uint64]*registryEntry
}

func NewRegistry(maximumPerUser int) *Registry {
	if maximumPerUser < 1 {
		maximumPerUser = 1
	}
	return &Registry{
		maximum:   maximumPerUser,
		blocked:   make(map[int64]struct{}),
		entries:   make(map[uint64]*registryEntry),
		byUser:    make(map[int64]map[uint64]*registryEntry),
		bySession: make(map[string]map[uint64]*registryEntry),
	}
}

type Registration struct {
	registry *Registry
	entry    *registryEntry
	ctx      context.Context
	once     sync.Once
}

func (r *Registry) Register(parent context.Context, userID int64, sessionID string) (*Registration, error) {
	if parent == nil || userID < 1 || sessionID == "" {
		return nil, ErrSessionRevoked
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, denied := r.blocked[userID]; denied {
		return nil, ErrUserBlocked
	}
	if len(r.byUser[userID]) >= r.maximum {
		return nil, ErrStreamLimit
	}

	r.nextID++
	ctx, cancel := context.WithCancelCause(parent)
	entry := &registryEntry{id: r.nextID, userID: userID, sessionID: sessionID, cancel: cancel}
	r.entries[entry.id] = entry
	if r.byUser[userID] == nil {
		r.byUser[userID] = make(map[uint64]*registryEntry)
	}
	r.byUser[userID][entry.id] = entry
	if r.bySession[sessionID] == nil {
		r.bySession[sessionID] = make(map[uint64]*registryEntry)
	}
	r.bySession[sessionID][entry.id] = entry
	return &Registration{registry: r, entry: entry, ctx: ctx}, nil
}

func (r *Registration) Context() context.Context {
	return r.ctx
}

func (r *Registration) Close() {
	if r == nil || r.registry == nil || r.entry == nil {
		return
	}
	r.once.Do(func() {
		r.registry.remove(r.entry.id)
		r.entry.cancel(errStreamClosed)
	})
}

func (r *Registry) remove(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[id]
	if !ok {
		return
	}
	r.removeLocked(entry)
}

func (r *Registry) removeLocked(entry *registryEntry) {
	delete(r.entries, entry.id)
	if items := r.byUser[entry.userID]; items != nil {
		delete(items, entry.id)
		if len(items) == 0 {
			delete(r.byUser, entry.userID)
		}
	}
	if items := r.bySession[entry.sessionID]; items != nil {
		delete(items, entry.id)
		if len(items) == 0 {
			delete(r.bySession, entry.sessionID)
		}
	}
}

// BlockAndAbortUser is used after suspension or deletion commits. The block is
// persistent until ReactivateUser explicitly removes it.
func (r *Registry) BlockAndAbortUser(userID int64) {
	r.mu.Lock()
	r.blocked[userID] = struct{}{}
	entries := r.detachUserLocked(userID, "")
	r.mu.Unlock()
	cancelEntries(entries, ErrUserBlocked)
}

// AbortUser cancels current streams without blocking future registration. It
// is used when all sessions are revoked by a password reset while the account
// itself remains active.
func (r *Registry) AbortUser(userID int64) {
	r.mu.Lock()
	entries := r.detachUserLocked(userID, "")
	r.mu.Unlock()
	cancelEntries(entries, ErrSessionRevoked)
}

func (r *Registry) ReactivateUser(userID int64) {
	r.mu.Lock()
	delete(r.blocked, userID)
	r.mu.Unlock()
}

func (r *Registry) AbortSession(sessionID string) {
	r.mu.Lock()
	items := r.bySession[sessionID]
	entries := make([]*registryEntry, 0, len(items))
	for _, entry := range items {
		entries = append(entries, entry)
		r.removeLocked(entry)
	}
	r.mu.Unlock()
	cancelEntries(entries, ErrSessionRevoked)
}

func (r *Registry) AbortUserExceptSession(userID int64, sessionID string) {
	r.mu.Lock()
	entries := r.detachUserLocked(userID, sessionID)
	r.mu.Unlock()
	cancelEntries(entries, ErrSessionRevoked)
}

func (r *Registry) AbortAll() {
	r.mu.Lock()
	entries := make([]*registryEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
		r.removeLocked(entry)
	}
	r.mu.Unlock()
	cancelEntries(entries, ErrSessionRevoked)
}

func (r *Registry) detachUserLocked(userID int64, exceptSession string) []*registryEntry {
	items := r.byUser[userID]
	entries := make([]*registryEntry, 0, len(items))
	for _, entry := range items {
		if exceptSession != "" && entry.sessionID == exceptSession {
			continue
		}
		entries = append(entries, entry)
		r.removeLocked(entry)
	}
	return entries
}

func cancelEntries(entries []*registryEntry, cause error) {
	for _, entry := range entries {
		entry.cancel(cause)
	}
}

func (r *Registry) ActiveForUser(userID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byUser[userID])
}

func (r *Registry) IsBlocked(userID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, blocked := r.blocked[userID]
	return blocked
}
