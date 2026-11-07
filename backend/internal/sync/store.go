package sync

import (
	"context"
	"strings"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
)

type correlationContextKey struct{}

// WithCorrelationID carries the API request correlation identifier into the
// mode-transition transaction so the existing append-only audit contract is
// preserved without putting transport metadata in playback command bodies.
func WithCorrelationID(ctx context.Context, value string) context.Context {
	value = strings.TrimSpace(value)
	if value == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationContextKey{}, value)
}

func correlationIDFrom(ctx context.Context, fallback string) string {
	if value, ok := ctx.Value(correlationContextKey{}).(string); ok && value != "" {
		return value
	}
	return fallback
}

type Store interface {
	Load(context.Context) (Snapshot, error)
	Start(context.Context, auth.Principal, Session, *int64) (Result, error)
	Commit(context.Context, *auth.Principal, Expected, Session, EventDraft, *string) (Result, error)
	Recover(context.Context, time.Time) ([]Result, error)
}
