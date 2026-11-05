package auth

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword(encoded, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword() = %v, %v", ok, err)
	}
	ok, err = VerifyPassword(encoded, "incorrect password")
	if err != nil || ok {
		t.Fatalf("VerifyPassword(wrong) = %v, %v", ok, err)
	}
}

func TestValidatePasswordBounds(t *testing.T) {
	if err := ValidatePassword("too-short"); err == nil {
		t.Fatal("ValidatePassword() expected an error")
	}
}
