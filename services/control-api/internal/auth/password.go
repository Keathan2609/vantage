// Package auth implements password hashing, TOTP, sessions and CSRF defence.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters.
//
// Chosen to cost roughly 100ms on modest hardware: high enough that offline
// cracking of a stolen hash is expensive, low enough that a login request is
// not itself a denial-of-service amplifier. The parameters are encoded into
// every hash, so they can be raised later without invalidating existing
// passwords -- an old hash still verifies under its own recorded parameters.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // 64 MiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// Errors returned by this package.
var (
	ErrPasswordMismatch = errors.New("auth: password does not match")
	ErrHashMalformed    = errors.New("auth: password hash is malformed")
	ErrPasswordWeak     = errors.New("auth: password does not meet policy")
)

// HashPassword derives an Argon2id hash in the standard PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a stored hash in constant time.
func VerifyPassword(password, encoded string) error {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return argonParams{}, nil, nil, ErrHashMalformed
	}
	return p, salt, key, nil
}

// NeedsRehash reports whether a stored hash uses weaker parameters than the
// current policy, so it can be upgraded on the user's next successful login.
func NeedsRehash(encoded string) bool {
	p, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return p.memory < argonMemory || p.time < argonTime || p.threads < argonThreads
}

// Password policy.
//
// Length does most of the work; composition rules mostly push people toward
// predictable substitutions. The floor here is 12 characters with a modest
// variety requirement, plus a block-list of the passwords that actually appear
// in credential-stuffing lists.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 256 // bound the Argon2 input so a huge body cannot burn CPU
)

var commonPasswords = map[string]bool{
	"password":     true,
	"password1":    true,
	"password123":  true,
	"passw0rd":     true,
	"123456789012": true,
	"qwertyuiop12": true,
	"letmein12345": true,
	"welcome12345": true,
	"admin1234567": true,
	"trading12345": true,
	"vantage12345": true,
	"changeme1234": true,
}

// ValidatePassword enforces the password policy, returning a message the user
// can act on.
func ValidatePassword(password, email, displayName string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("%w: passwords must be at least %d characters", ErrPasswordWeak, MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("%w: passwords must be at most %d characters", ErrPasswordWeak, MaxPasswordLength)
	}
	lower := strings.ToLower(password)
	if commonPasswords[lower] {
		return fmt.Errorf("%w: this password is too common", ErrPasswordWeak)
	}
	// A password containing the account's own identifiers is guessable by
	// anyone who knows the account.
	if local := strings.SplitN(strings.ToLower(email), "@", 2)[0]; len(local) >= 4 && strings.Contains(lower, local) {
		return fmt.Errorf("%w: the password must not contain your email address", ErrPasswordWeak)
	}
	if n := strings.ToLower(strings.TrimSpace(displayName)); len(n) >= 4 && strings.Contains(lower, n) {
		return fmt.Errorf("%w: the password must not contain your name", ErrPasswordWeak)
	}

	var hasLetter, hasDigitOrSymbol bool
	for _, r := range password {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r), unicode.IsPunct(r), unicode.IsSymbol(r), unicode.IsSpace(r):
			hasDigitOrSymbol = true
		}
	}
	if !hasLetter || !hasDigitOrSymbol {
		return fmt.Errorf("%w: use a mix of letters and numbers or symbols", ErrPasswordWeak)
	}
	return nil
}
