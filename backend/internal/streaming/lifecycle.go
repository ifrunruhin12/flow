package streaming

import "github.com/ifrunruhin12/flow/backend/internal/navidrome"

// Lifecycle composes membership state changes with both process-local security
// components introduced in M2.
type Lifecycle struct {
	Registry    *Registry
	Credentials *navidrome.CredentialCache
}

func (l *Lifecycle) RevokeUser(userID int64) {
	if l == nil {
		return
	}
	if l.Registry != nil {
		l.Registry.BlockAndAbortUser(userID)
	}
	if l.Credentials != nil {
		l.Credentials.Evict(userID)
	}
}

func (l *Lifecycle) AbortUser(userID int64) {
	if l != nil && l.Registry != nil {
		l.Registry.AbortUser(userID)
	}
}

func (l *Lifecycle) ReactivateUser(userID int64) {
	if l != nil && l.Registry != nil {
		l.Registry.ReactivateUser(userID)
	}
}
