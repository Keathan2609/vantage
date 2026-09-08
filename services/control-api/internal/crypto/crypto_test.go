package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestEncryptDecryptRoundTrip(t *testing.T) {
	r, err := NewKeyring(1, key(0x11))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	secret := []byte("JBSWY3DPEHPK3PXP")
	aad := []byte("user:0f8d")

	ct, err := r.Encrypt(secret, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Contains(ct, secret) {
		t.Fatal("plaintext appears in ciphertext")
	}

	got, err := r.Decrypt(ct, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Errorf("round trip = %q, want %q", got, secret)
	}
}

func TestCiphertextIsNonDeterministic(t *testing.T) {
	r, _ := NewKeyring(1, key(0x11))
	a, _ := r.Encrypt([]byte("same"), nil)
	b, _ := r.Encrypt([]byte("same"), nil)
	if bytes.Equal(a, b) {
		t.Error("encrypting the same plaintext twice produced identical ciphertext")
	}
}

func TestAssociatedDataBindsCiphertextToContext(t *testing.T) {
	// A TOTP secret lifted from one user's row and pasted into another's must
	// not decrypt: otherwise a database writer could transplant a credential.
	r, _ := NewKeyring(1, key(0x11))
	ct, _ := r.Encrypt([]byte("secret"), []byte("user:alice"))

	if _, err := r.Decrypt(ct, []byte("user:mallory")); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("decryption under the wrong context: err = %v, want failure", err)
	}
	if _, err := r.Decrypt(ct, nil); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("decryption with missing context: err = %v, want failure", err)
	}
}

func TestTamperedCiphertextIsRejected(t *testing.T) {
	r, _ := NewKeyring(1, key(0x11))
	ct, _ := r.Encrypt([]byte("secret value"), []byte("ctx"))

	for _, pos := range []int{5, len(ct) / 2, len(ct) - 1} {
		mutated := append([]byte{}, ct...)
		mutated[pos] ^= 0xFF
		if _, err := r.Decrypt(mutated, []byte("ctx")); err == nil {
			t.Errorf("tampering at byte %d was not detected", pos)
		}
	}

	// The recorded key version is authenticated too.
	mutated := append([]byte{}, ct...)
	mutated[4] = 0x09
	if _, err := r.Decrypt(mutated, []byte("ctx")); err == nil {
		t.Error("altering the key version was not detected")
	}
}

func TestWrongKeyFails(t *testing.T) {
	a, _ := NewKeyring(1, key(0x11))
	b, _ := NewKeyring(1, key(0x22))
	ct, _ := a.Encrypt([]byte("secret"), nil)
	if _, err := b.Decrypt(ct, nil); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("err = %v, want ErrDecryptionFailed", err)
	}
}

func TestKeyRotationKeepsOldCiphertextReadable(t *testing.T) {
	old, _ := NewKeyring(1, key(0x11))
	ct, _ := old.Encrypt([]byte("enrolled-secret"), []byte("ctx"))

	// Rotate: version 2 becomes active, version 1 is retained for reads.
	rotated, _ := NewKeyring(2, key(0x22))
	if err := rotated.AddRetiredKey(1, key(0x11)); err != nil {
		t.Fatalf("AddRetiredKey: %v", err)
	}

	got, err := rotated.Decrypt(ct, []byte("ctx"))
	if err != nil {
		t.Fatalf("old ciphertext unreadable after rotation: %v", err)
	}
	if string(got) != "enrolled-secret" {
		t.Errorf("got %q", got)
	}

	// New writes use the new version.
	fresh, _ := rotated.Encrypt([]byte("new"), []byte("ctx"))
	if v, err := KeyVersionOf(fresh); err != nil || v != 2 {
		t.Errorf("new ciphertext version = %d (%v), want 2", v, err)
	}
	if v, _ := KeyVersionOf(ct); v != 1 {
		t.Errorf("old ciphertext version = %d, want 1", v)
	}
}

func TestUnknownKeyVersionIsReported(t *testing.T) {
	a, _ := NewKeyring(7, key(0x11))
	ct, _ := a.Encrypt([]byte("x"), nil)
	b, _ := NewKeyring(1, key(0x11))
	if _, err := b.Decrypt(ct, nil); !errors.Is(err, ErrKeyVersionUnknown) {
		t.Fatalf("err = %v, want ErrKeyVersionUnknown", err)
	}
}

func TestMalformedCiphertext(t *testing.T) {
	r, _ := NewKeyring(1, key(0x11))
	for _, ct := range [][]byte{nil, {}, {0x01}, {0x02, 0, 0, 0, 1}, make([]byte, 8)} {
		if _, err := r.Decrypt(ct, nil); err == nil {
			t.Errorf("malformed ciphertext %v was accepted", ct)
		}
	}
}

func TestRecoveryCodeHashing(t *testing.T) {
	k := key(0x33)
	h := HashRecoveryCode(k, "ABCD-EFGH-IJKL")
	if strings.Contains(h, "ABCD") {
		t.Fatal("recovery code appears in its own hash")
	}
	if !VerifyRecoveryCode(k, "ABCD-EFGH-IJKL", h) {
		t.Error("valid recovery code did not verify")
	}
	if VerifyRecoveryCode(k, "WRONG-CODE-HERE", h) {
		t.Error("invalid recovery code verified")
	}
	if VerifyRecoveryCode(key(0x44), "ABCD-EFGH-IJKL", h) {
		t.Error("recovery code verified under the wrong key")
	}
}

func TestRandomTokenEntropyFloor(t *testing.T) {
	if _, err := RandomToken(8); err == nil {
		t.Error("tokens below 128 bits of entropy must be refused")
	}
	a, err := RandomToken(32)
	if err != nil {
		t.Fatalf("RandomToken: %v", err)
	}
	b, _ := RandomToken(32)
	if a == b {
		t.Error("two random tokens collided")
	}
}

func TestKeyValidation(t *testing.T) {
	if _, err := NewKeyring(1, []byte("short")); err == nil {
		t.Error("short keys must be refused")
	}
	if _, err := NewKeyring(0, key(0x11)); err == nil {
		t.Error("key version 0 must be refused")
	}
}
