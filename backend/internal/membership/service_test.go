package membership

import (
	"errors"
	"testing"
)

func TestValidateIdentity(t *testing.T) {
	displayName, email, username, err := validateIdentity("  Flow Member  ", "MEMBER@example.test", "member.one")
	if err != nil {
		t.Fatal(err)
	}
	if displayName != "Flow Member" || email != "member@example.test" || username != "member.one" {
		t.Fatalf("unexpected normalized identity: %q %q %q", displayName, email, username)
	}
}

func TestValidateIdentityRejectsUnsafeUsername(t *testing.T) {
	_, _, _, err := validateIdentity("Member", "member@example.test", "../member")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
}

func TestTemporaryPasswordValidation(t *testing.T) {
	if _, err := hashTemporaryPassword("short"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
}
