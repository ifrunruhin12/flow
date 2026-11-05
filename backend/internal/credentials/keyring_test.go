package credentials

import (
	"bytes"
	"testing"
)

func TestEncryptDecryptAndAssociatedData(t *testing.T) {
	keyring, err := New(2, map[int][]byte{1: bytes.Repeat([]byte{1}, 32), 2: bytes.Repeat([]byte{2}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := keyring.Encrypt("member", "navidrome-password", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if envelope.KeyVersion != 2 {
		t.Fatalf("KeyVersion = %d", envelope.KeyVersion)
	}
	plaintext, err := keyring.Decrypt("member", "navidrome-password", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "secret" {
		t.Fatalf("plaintext = %q", plaintext)
	}
	if _, err := keyring.Decrypt("other-member", "navidrome-password", envelope); err == nil {
		t.Fatal("Decrypt() accepted ciphertext under different associated data")
	}
}

func TestNewRejectsMissingActiveVersion(t *testing.T) {
	if _, err := New(2, map[int][]byte{1: bytes.Repeat([]byte{1}, 32)}); err == nil {
		t.Fatal("New() expected an error")
	}
}
