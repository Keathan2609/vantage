package auth

import (
	"encoding/base32"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	const pw = "correct horse battery staple 42"
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, pw) {
		t.Fatal("the password appears inside its own hash")
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash is not argon2id: %q", hash)
	}
	if err := VerifyPassword(pw, hash); err != nil {
		t.Errorf("correct password did not verify: %v", err)
	}
	if err := VerifyPassword("wrong password entirely", hash); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("wrong password: err = %v, want ErrPasswordMismatch", err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same password here")
	b, _ := HashPassword("same password here")
	if a == b {
		t.Fatal("identical passwords produced identical hashes: the salt is not random")
	}
	// Both must still verify.
	if err := VerifyPassword("same password here", a); err != nil {
		t.Error(err)
	}
	if err := VerifyPassword("same password here", b); err != nil {
		t.Error(err)
	}
}

func TestMalformedHashIsRejected(t *testing.T) {
	for _, h := range []string{
		"", "notahash", "$argon2id$", "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=99$m=65536,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=19$bogus$c2FsdA$a2V5",
	} {
		if err := VerifyPassword("x", h); !errors.Is(err, ErrHashMalformed) {
			t.Errorf("hash %q: err = %v, want ErrHashMalformed", h, err)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		name, password string
		wantErr        bool
	}{
		{"good", "quiet-gold-tide-9174", false},
		{"too short", "short1!", true},
		{"letters only", "abcdefghijklmnop", true},
		{"digits only", "1234567890123456", true},
		{"common", "password123", true},
		{"contains email local part", "trader.one-secure-9", true},
		{"contains display name", "Marlowe-secure-42xx", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidatePassword(c.password, "trader.one@example.com", "Marlowe")
			if c.wantErr && err == nil {
				t.Errorf("expected %q to be rejected", c.password)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected %q to be accepted, got %v", c.password, err)
			}
		})
	}
}

func TestPasswordLengthIsBounded(t *testing.T) {
	// An unbounded password is an easy CPU-exhaustion vector: Argon2 over a
	// megabyte of input, on an unauthenticated endpoint, at request rate.
	long := strings.Repeat("a1", MaxPasswordLength)
	if err := ValidatePassword(long, "u@example.com", "User"); err == nil {
		t.Error("an over-long password must be rejected before hashing")
	}
}

func TestNeedsRehash(t *testing.T) {
	current, _ := HashPassword("some password 123")
	if NeedsRehash(current) {
		t.Error("a freshly generated hash should not need rehashing")
	}
	weak := "$argon2id$v=19$m=1024,t=1,p=1$c2FsdHNhbHRzYWx0c2E$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5"
	if !NeedsRehash(weak) {
		t.Error("a hash with weaker parameters should be flagged for rehashing")
	}
	if !NeedsRehash("garbage") {
		t.Error("an unparseable hash should be flagged")
	}
}

// TestTOTPAgainstRFC6238Vectors checks the implementation against the published
// test vectors, using the RFC's SHA-1 seed. Getting this wrong would either
// lock every user out or accept codes that are not theirs.
func TestTOTPAgainstRFC6238Vectors(t *testing.T) {
	// RFC 6238 Appendix B seed for SHA-1: ASCII "12345678901234567890".
	seed := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))

	cases := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		got, err := TOTPCode(seed, time.Unix(c.unix, 0).UTC())
		if err != nil {
			t.Fatalf("TOTPCode: %v", err)
		}
		if got != c.want {
			t.Errorf("TOTPCode(t=%d) = %s, want %s", c.unix, got, c.want)
		}
	}
}

func TestVerifyTOTPWindow(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	now := time.Date(2025, 7, 8, 12, 0, 30, 0, time.UTC)
	code, _ := TOTPCode(secret, now)

	if !VerifyTOTP(secret, code, now) {
		t.Error("the current code must verify")
	}
	// One period of drift either way is tolerated.
	if !VerifyTOTP(secret, code, now.Add(30*time.Second)) {
		t.Error("a code one period old must still verify")
	}
	if !VerifyTOTP(secret, code, now.Add(-30*time.Second)) {
		t.Error("a code one period early must verify")
	}
	// Beyond the window it must not.
	if VerifyTOTP(secret, code, now.Add(5*time.Minute)) {
		t.Error("a five-minute-old code must be refused")
	}
	if VerifyTOTP(secret, code, now.Add(-5*time.Minute)) {
		t.Error("a code from five minutes in the future must be refused")
	}
}

func TestVerifyTOTPRejectsMalformedInput(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Now().UTC()
	for _, code := range []string{"", "12345", "1234567", "abcdef", "  ", "000000"} {
		if code == "000000" {
			// Vanishingly unlikely to be the real code; if it is, skip.
			real, _ := TOTPCode(secret, now)
			if real == "000000" {
				continue
			}
		}
		if VerifyTOTP(secret, code, now) {
			t.Errorf("code %q must not verify", code)
		}
	}
	// A wrong secret must not verify a valid-looking code.
	other, _ := GenerateTOTPSecret()
	code, _ := TOTPCode(other, now)
	if VerifyTOTP(secret, code, now) {
		t.Error("a code from a different secret must not verify")
	}
}

func TestProvisioningURI(t *testing.T) {
	uri := TOTPProvisioningURI("JBSWY3DPEHPK3PXP", "trader@example.com", "Vantage")
	for _, want := range []string{"otpauth://totp/", "secret=JBSWY3DPEHPK3PXP", "issuer=Vantage", "digits=6", "period=30"} {
		if !strings.Contains(uri, want) {
			t.Errorf("provisioning URI %q is missing %q", uri, want)
		}
	}
}

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes(10)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("got %d codes, want 10", len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate recovery code %q", c)
		}
		seen[c] = true
		// Ambiguous characters are excluded so codes can be read aloud or
		// copied from paper without transcription errors.
		for _, bad := range []string{"I", "L", "O", "0", "1", "U"} {
			if strings.Contains(c, bad) {
				t.Errorf("code %q contains ambiguous character %q", c, bad)
			}
		}
	}
	if _, err := GenerateRecoveryCodes(0); err == nil {
		t.Error("zero codes must be refused")
	}
	if _, err := GenerateRecoveryCodes(100); err == nil {
		t.Error("an unreasonable count must be refused")
	}
}
