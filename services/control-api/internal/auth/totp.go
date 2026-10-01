package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"
)

// TOTP implements RFC 6238 time-based one-time passwords.
//
// The algorithm is implemented directly from the RFC because it is small,
// fully specified and easy to test against the published vectors -- the
// standard library provides HMAC-SHA1, which is the only primitive involved.
// No cryptographic primitive is implemented here.
const (
	totpDigits    = 6
	totpPeriod    = 30 * time.Second
	totpSecretLen = 20 // 160 bits, the RFC 4226 recommendation

	// totpSkew is how many periods either side of "now" are accepted. One
	// period tolerates ordinary clock drift and a user typing slowly; more
	// than that widens the window an attacker has to replay a stolen code.
	totpSkew = 1
)

// GenerateTOTPSecret produces a new base32-encoded shared secret.
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, totpSecretLen)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("auth: generate totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// TOTPCode computes the code for a secret at an instant.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	counter := uint64(t.UTC().Unix()) / uint64(totpPeriod.Seconds())
	return hotp(key, counter), nil
}

// VerifyTOTP checks a submitted code against the secret, allowing for drift.
//
// Comparison is constant-time and covers the whole accepted window, so the
// number of periods tried is not observable through response timing.
func VerifyTOTP(secret, code string, now time.Time) bool {
	key, err := decodeSecret(secret)
	if err != nil {
		return false
	}
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}

	counter := uint64(now.UTC().Unix()) / uint64(totpPeriod.Seconds())
	var matched bool
	for i := -totpSkew; i <= totpSkew; i++ {
		c := counter
		if i < 0 {
			c -= uint64(-i)
		} else {
			c += uint64(i)
		}
		candidate := hotp(key, c)
		// Accumulate rather than returning early, so every branch does the
		// same work regardless of which period matched.
		if constantTimeEqual(candidate, code) {
			matched = true
		}
	}
	return matched
}

// TOTPProvisioningURI returns the otpauth:// URI an authenticator app scans.
// The secret is embedded in the URI, so it must be shown once at enrolment and
// never logged, stored in plaintext, or returned by any other endpoint.
func TOTPProvisioningURI(secret, accountName, issuer string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprintf("%d", totpDigits))
	v.Set("period", fmt.Sprintf("%d", int(totpPeriod.Seconds())))

	label := url.PathEscape(issuer + ":" + accountName)
	return "otpauth://totp/" + label + "?" + v.Encode()
}

func decodeSecret(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(secret, " ", "")))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("auth: decode totp secret: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("auth: empty totp secret")
	}
	return key, nil
}

// hotp implements RFC 4226's truncation over HMAC-SHA1.
func hotp(key []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	m := hmac.New(sha1.New, key)
	m.Write(buf[:])
	sum := m.Sum(nil)

	offset := sum[len(sum)-1] & 0x0F
	value := (uint32(sum[offset]&0x7F) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(math.Pow10(totpDigits))
	return fmt.Sprintf("%0*d", totpDigits, value%mod)
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// GenerateRecoveryCodes produces single-use MFA recovery codes.
//
// Each carries 100 bits of entropy, formatted in groups for legibility. They
// are shown once and stored only as keyed hashes.
func GenerateRecoveryCodes(count int) ([]string, error) {
	if count < 1 || count > 20 {
		return nil, fmt.Errorf("auth: recovery code count must be 1-20, got %d", count)
	}
	const alphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789" // no I, L, O, 0, 1, U
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		buf := make([]byte, 20)
		if _, err := io.ReadFull(rand.Reader, buf); err != nil {
			return nil, fmt.Errorf("auth: generate recovery code: %w", err)
		}
		var sb strings.Builder
		for j, b := range buf {
			if j > 0 && j%5 == 0 {
				sb.WriteByte('-')
			}
			sb.WriteByte(alphabet[int(b)%len(alphabet)])
		}
		out = append(out, sb.String())
	}
	return out, nil
}
