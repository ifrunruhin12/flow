package streaming

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegistryBlockAndRegistrationRaceCannotEscape(t *testing.T) {
	for attempt := 0; attempt < 500; attempt++ {
		registry := NewRegistry(2)
		start := make(chan struct{})
		var registration *Registration
		var registerErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			registration, registerErr = registry.Register(context.Background(), 7, "session")
		}()
		go func() {
			defer wait.Done()
			<-start
			registry.BlockAndAbortUser(7)
		}()
		close(start)
		wait.Wait()

		if registerErr != nil {
			if !errors.Is(registerErr, ErrUserBlocked) {
				t.Fatalf("attempt %d: Register() error = %v", attempt, registerErr)
			}
		} else {
			select {
			case <-registration.Context().Done():
				if !errors.Is(context.Cause(registration.Context()), ErrUserBlocked) {
					t.Fatalf("attempt %d: cancellation cause = %v", attempt, context.Cause(registration.Context()))
				}
			case <-time.After(time.Second):
				t.Fatalf("attempt %d: registration escaped user block", attempt)
			}
			registration.Close()
		}
		if got := registry.ActiveForUser(7); got != 0 {
			t.Fatalf("attempt %d: active streams = %d", attempt, got)
		}
		if !registry.IsBlocked(7) {
			t.Fatalf("attempt %d: user is not blocked", attempt)
		}
	}
}

func TestRegistrySessionAndUserRevocation(t *testing.T) {
	registry := NewRegistry(4)
	first, _ := registry.Register(context.Background(), 9, "first")
	second, _ := registry.Register(context.Background(), 9, "second")
	third, _ := registry.Register(context.Background(), 10, "first")

	registry.AbortSession("first")
	assertCanceledWith(t, first.Context(), ErrSessionRevoked)
	assertCanceledWith(t, third.Context(), ErrSessionRevoked)
	if err := second.Context().Err(); err != nil {
		t.Fatalf("unrelated session canceled: %v", err)
	}

	registry.AbortUserExceptSession(9, "second")
	if err := second.Context().Err(); err != nil {
		t.Fatalf("preserved session canceled: %v", err)
	}
	second.Close()
}

func TestRegistryLimitAndReactivation(t *testing.T) {
	registry := NewRegistry(1)
	registration, err := registry.Register(context.Background(), 4, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(context.Background(), 4, "two"); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("second Register() error = %v", err)
	}
	registry.BlockAndAbortUser(4)
	assertCanceledWith(t, registration.Context(), ErrUserBlocked)
	if _, err := registry.Register(context.Background(), 4, "three"); !errors.Is(err, ErrUserBlocked) {
		t.Fatalf("blocked Register() error = %v", err)
	}
	registry.ReactivateUser(4)
	reactivated, err := registry.Register(context.Background(), 4, "four")
	if err != nil {
		t.Fatalf("reactivated Register() error = %v", err)
	}
	reactivated.Close()
}

func assertCanceledWith(t *testing.T, ctx context.Context, expected error) {
	t.Helper()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), expected) {
			t.Fatalf("cancellation cause = %v, want %v", context.Cause(ctx), expected)
		}
	case <-time.After(time.Second):
		t.Fatalf("context was not canceled with %v", expected)
	}
}
