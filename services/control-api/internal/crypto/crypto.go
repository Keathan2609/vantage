// Package crypto provides authenticated encryption for secrets held at rest.
//
// Vantage does not implement cryptographic primitives. This package composes
// AES-256-GCM from the standard library with a versioned key, and nothing else.
//
// What it protects today: TOTP secrets. What it is designed to protect later:
// broker credentials, which must never be readable from a database dump alone.
//
// Key versioning exists so keys can be rotated without a flag day. Every
// ciphertext records the version of the key that produced it, so old data stays
// readable while new data is written under the new key. The key itself lives
// outside the database — in development an environment variable, in any real
// deployment a KMS — so that database access alone does not yield plaintext.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Errors returned by this package.
var (
	ErrKeyVersionUnknown = errors.New("crypto: no key registered for ciphertext version")
	ErrCiphertextInvalid = errors.New("crypto: ciphertext is malformed")
	ErrDecryptionFailed  = errors.New("crypto: decryption failed")
)

// version1Header is the 5-byte prefix on every ciphertext: a format byte
// followed by a big-endian key version.
const formatVersion byte = 0x01

// Keyring holds the active encryption key plus any retired keys still needed to
// read old ciphertext.
type Keyring struct {
	activeVersion int
	keys          map[int][]byte
}

// NewKeyring builds a keyring with one active key.
func NewKeyring(activeVersion int, activeKey []byte) (*Keyring, error) {
	if activeVersion < 1 {
		return nil, errors.New("crypto: key version must be positive")
	}
	if len(activeKey) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(activeKey))
	}
	k := make([]byte, 32)
	copy(k, activeKey)
	return &Keyring{
		activeVersion: activeVersion,
		keys:          map[int][]byte{activeVersion: k},
	}, nil
}

// AddRetiredKey registers an older key so existing ciphertext stays readable
// after rotation.
func (r *Keyring) AddRetiredKey(version int, key []byte) error {
	if version == r.activeVersion {
		return errors.New("crypto: cannot register the active version as retired")
	}
	if len(key) != 32 {
		return fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	k := make([]byte, 32)
	copy(k, key)
	r.keys[version] = k
	return nil
}

// ActiveVersion returns the version new ciphertext is written under.
func (r *Keyring) ActiveVersion() int { return r.activeVersion }

// Encrypt seals plaintext under the active key.
//
// associatedData is authenticated but not encrypted. Callers bind ciphertext to
// its context with it — for example the user ID that owns a TOTP secret — so a
// ciphertext copied from one row to another fails to decrypt rather than
// silently authenticating the wrong principal.
func (r *Keyring) Encrypt(plaintext, associatedData []byte) ([]byte, error) {
	key, ok := r.keys[r.activeVersion]
	if !ok {
		return nil, ErrKeyVersionUnknown
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: read nonce: %w", err)
	}

	header := make([]byte, 5)
	header[0] = formatVersion
	binary.BigEndian.PutUint32(header[1:], uint32(r.activeVersion))

	// The header is authenticated alongside the caller's associated data, so
	// the recorded key version cannot be altered without detection.
	aad := append(append([]byte{}, header...), associatedData...)
	sealed := gcm.Seal(nil, nonce, plaintext, aad)

	out := make([]byte, 0, len(header)+len(nonce)+len(sealed))
	out = append(out, header...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Decrypt opens a ciphertext produced by Encrypt.
func (r *Keyring) Decrypt(ciphertext, associatedData []byte) ([]byte, error) {
	if len(ciphertext) < 5 {
		return nil, ErrCiphertextInvalid
	}
	if ciphertext[0] != formatVersion {
		return nil, fmt.Errorf("%w: unknown format byte %#x", ErrCiphertextInvalid, ciphertext[0])
	}
	version := int(binary.BigEndian.Uint32(ciphertext[1:5]))
	key, ok := r.keys[version]
	if !ok {
		return nil, fmt.Errorf("%w: version %d", ErrKeyVersionUnknown, version)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < 5+gcm.NonceSize() {
		return nil, ErrCiphertextInvalid
	}
	header := ciphertext[:5]
	nonce := ciphertext[5 : 5+gcm.NonceSize()]
	sealed := ciphertext[5+gcm.NonceSize():]

	aad := append(append([]byte{}, header...), associatedData...)
	plaintext, err := gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		// The underlying error is deliberately not wrapped: distinguishing
		// "wrong key" from "tampered ciphertext" is an oracle.
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}

// KeyVersionOf reports which key version sealed a ciphertext, without
// decrypting it. Used by rotation tooling to find rows still on an old key.
func KeyVersionOf(ciphertext []byte) (int, error) {
	if len(ciphertext) < 5 || ciphertext[0] != formatVersion {
		return 0, ErrCiphertextInvalid
	}
	return int(binary.BigEndian.Uint32(ciphertext[1:5])), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return gcm, nil
}

// HashRecoveryCode hashes an MFA recovery code for storage.
//
// Recovery codes are high-entropy values generated by Vantage, not
// user-chosen passwords, so a fast keyed hash is appropriate: there is nothing
// to brute-force in a 160-bit random string. Passwords use Argon2id instead
// (see internal/auth).
func HashRecoveryCode(key []byte, code string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(code))
	return base64.RawStdEncoding.EncodeToString(m.Sum(nil))
}

// VerifyRecoveryCode compares a candidate code against a stored hash in
// constant time.
func VerifyRecoveryCode(key []byte, code, storedHash string) bool {
	candidate := HashRecoveryCode(key, code)
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(storedHash)) == 1
}

// ConstantTimeEqualString compares two strings without leaking their contents
// through timing. Used for service tokens and session identifiers.
func ConstantTimeEqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RandomToken returns a URL-safe random token with n bytes of entropy.
func RandomToken(n int) (string, error) {
	if n < 16 {
		return "", errors.New("crypto: tokens must carry at least 128 bits of entropy")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("crypto: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SHA256Hex returns the hex-encoded SHA-256 of b. Used for request hashing in
// idempotency handling, where the value is a fingerprint rather than a secret.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}
