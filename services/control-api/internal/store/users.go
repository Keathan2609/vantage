package store

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
)

// UserStore persists users, sessions and MFA material.
type UserStore struct{ pool *db.Pool }

const userColumns = `id, email, display_name, role, password_hash, mfa_enabled,
	mfa_secret_cipher, mfa_key_version, disabled, failed_login_count, locked_until,
	last_login_at, password_changed_at, created_at, updated_at`

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	var keyVersion *int
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.PasswordHash, &u.MFAEnabled,
		&u.MFASecretCipher, &keyVersion, &u.Disabled, &u.FailedLoginCount, &u.LockedUntil,
		&u.LastLoginAt, &u.PasswordChangedAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return domain.User{}, mapError(err)
	}
	if keyVersion != nil {
		u.MFAKeyVersion = *keyVersion
	}
	return u, nil
}

// CreateUser inserts a user. Email is normalised to lowercase, matching the
// database's own constraint, so lookups cannot be defeated by casing.
func (s *UserStore) CreateUser(ctx context.Context, u domain.User) (domain.User, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO users (email, display_name, role, password_hash, mfa_enabled, disabled)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+userColumns,
		strings.ToLower(strings.TrimSpace(u.Email)), u.DisplayName, u.Role,
		u.PasswordHash, u.MFAEnabled, u.Disabled)
	return scanUser(row)
}

// UserByEmail loads a user for authentication.
func (s *UserStore) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1`,
		strings.ToLower(strings.TrimSpace(email)))
	return scanUser(row)
}

// UserByID loads a user by identifier.
func (s *UserStore) UserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	return scanUser(row)
}

// ListUsers returns all users, for the admin surface.
func (s *UserStore) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, mapError(rows.Err())
}

// RecordLoginSuccess resets the failure counter and stamps the login time.
func (s *UserStore) RecordLoginSuccess(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE users
		SET failed_login_count = 0, locked_until = NULL, last_login_at = $2, updated_at = now()
		WHERE id = $1`, id, at)
	return mapError(err)
}

// RecordLoginFailure increments the failure counter and applies lockout.
//
// The backoff is exponential and capped: repeated failures make online
// guessing impractical without letting an attacker lock a known account out
// forever, which would itself be a denial-of-service vector.
func (s *UserStore) RecordLoginFailure(ctx context.Context, id uuid.UUID, threshold int, base, max time.Duration) (locked bool, until *time.Time, err error) {
	var count int
	var lockedUntil *time.Time
	err = s.pool.QueryRow(ctx, `
		UPDATE users
		SET failed_login_count = failed_login_count + 1,
		    locked_until = CASE
		        WHEN failed_login_count + 1 >= $2
		        THEN now() + LEAST($3::interval * POWER(2, LEAST(failed_login_count + 1 - $2, 6)), $4::interval)
		        ELSE locked_until
		    END,
		    updated_at = now()
		WHERE id = $1
		RETURNING failed_login_count, locked_until`,
		id, threshold, base.String(), max.String()).Scan(&count, &lockedUntil)
	if err != nil {
		return false, nil, mapError(err)
	}
	return lockedUntil != nil && lockedUntil.After(time.Now()), lockedUntil, nil
}

// SetPasswordHash rotates a user's password and invalidates their sessions.
func (s *UserStore) SetPasswordHash(ctx context.Context, id uuid.UUID, hash string) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, password_changed_at = now(), updated_at = now()
			WHERE id = $1`, id, hash); err != nil {
			return mapError(err)
		}
		// A password change ends every existing session. Otherwise a user who
		// changes their password because they suspect compromise leaves the
		// attacker's session live.
		_, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = now(), revoked_reason = 'password_changed'
			WHERE user_id = $1 AND revoked_at IS NULL`, id)
		return mapError(err)
	})
}

// SetMFASecret stores an encrypted TOTP secret and its key version.
func (s *UserStore) SetMFASecret(ctx context.Context, id uuid.UUID, cipher []byte, keyVersion int, enabled bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE users SET mfa_secret_cipher = $2, mfa_key_version = $3, mfa_enabled = $4, updated_at = now()
		WHERE id = $1`, id, cipher, keyVersion, enabled)
	return mapError(err)
}

// DisableMFA clears MFA material and its recovery codes.
func (s *UserStore) DisableMFA(ctx context.Context, id uuid.UUID) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET mfa_enabled = FALSE, mfa_secret_cipher = NULL,
			                 mfa_key_version = NULL, updated_at = now()
			WHERE id = $1`, id); err != nil {
			return mapError(err)
		}
		_, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, id)
		return mapError(err)
	})
}

// ReplaceRecoveryCodes stores a fresh set of hashed recovery codes.
func (s *UserStore) ReplaceRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes []string) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM mfa_recovery_codes WHERE user_id = $1`, userID); err != nil {
			return mapError(err)
		}
		for _, h := range hashes {
			if _, err := tx.Exec(ctx,
				`INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, $2)`,
				userID, h); err != nil {
				return mapError(err)
			}
		}
		return nil
	})
}

// ConsumeRecoveryCode marks a recovery code used, returning false if it was
// unknown or already spent. The UPDATE-with-predicate makes consumption atomic:
// two concurrent uses of the same code cannot both succeed.
func (s *UserStore) ConsumeRecoveryCode(ctx context.Context, userID uuid.UUID, hash string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE mfa_recovery_codes SET used_at = now()
		WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, userID, hash)
	if err != nil {
		return false, mapError(err)
	}
	return tag.RowsAffected() == 1, nil
}

// UnusedRecoveryCodeHashes lists the still-valid code hashes for a user.
func (s *UserStore) UnusedRecoveryCodeHashes(ctx context.Context, userID uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT code_hash FROM mfa_recovery_codes WHERE user_id = $1 AND used_at IS NULL`, userID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, mapError(err)
		}
		out = append(out, h)
	}
	return out, mapError(rows.Err())
}

// SetRole changes a user's role.
func (s *UserStore) SetRole(ctx context.Context, id uuid.UUID, role domain.Role) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET role = $2, updated_at = now() WHERE id = $1`, id, role)
	return mapError(err)
}

// SetDisabled enables or disables a user and revokes sessions on disable.
func (s *UserStore) SetDisabled(ctx context.Context, id uuid.UUID, disabled bool) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE users SET disabled = $2, updated_at = now() WHERE id = $1`, id, disabled); err != nil {
			return mapError(err)
		}
		if disabled {
			_, err := tx.Exec(ctx, `
				UPDATE sessions SET revoked_at = now(), revoked_reason = 'user_disabled'
				WHERE user_id = $1 AND revoked_at IS NULL`, id)
			return mapError(err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// Session is a persisted authentication session.
type Session struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	MFASatisfied  bool
	IPAddress     *string
	UserAgent     *string
	CreatedAt     time.Time
	LastSeenAt    time.Time
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	RevokedReason *string
}

// Active reports whether the session may authenticate a request at time now.
func (s Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// CreateSession stores a session. Only hashes of the session and CSRF tokens
// are persisted; the plaintext values exist solely in the client's cookies.
func (s *UserStore) CreateSession(ctx context.Context, userID uuid.UUID, tokenHash, csrfHash string,
	mfaSatisfied bool, ip, userAgent *string, expiresAt time.Time) (Session, error) {

	var sess Session
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, csrf_token_hash, mfa_satisfied, ip_address, user_agent, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, user_id, mfa_satisfied, host(ip_address), user_agent,
		          created_at, last_seen_at, expires_at, revoked_at, revoked_reason`,
		userID, tokenHash, csrfHash, mfaSatisfied, ip, userAgent, expiresAt).
		Scan(&sess.ID, &sess.UserID, &sess.MFASatisfied, &sess.IPAddress, &sess.UserAgent,
			&sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.RevokedAt, &sess.RevokedReason)
	return sess, mapError(err)
}

// SessionByTokenHash loads a session and its user in one round trip.
func (s *UserStore) SessionByTokenHash(ctx context.Context, tokenHash string) (Session, domain.User, string, error) {
	var sess Session
	var u domain.User
	var csrfHash string
	var keyVersion *int

	err := s.pool.QueryRow(ctx, `
		SELECT s.id, s.user_id, s.mfa_satisfied, host(s.ip_address), s.user_agent,
		       s.created_at, s.last_seen_at, s.expires_at, s.revoked_at, s.revoked_reason,
		       s.csrf_token_hash,
		       u.id, u.email, u.display_name, u.role, u.password_hash, u.mfa_enabled,
		       u.mfa_secret_cipher, u.mfa_key_version, u.disabled, u.failed_login_count,
		       u.locked_until, u.last_login_at, u.password_changed_at, u.created_at, u.updated_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1`, tokenHash).
		Scan(&sess.ID, &sess.UserID, &sess.MFASatisfied, &sess.IPAddress, &sess.UserAgent,
			&sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.RevokedAt, &sess.RevokedReason,
			&csrfHash,
			&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.PasswordHash, &u.MFAEnabled,
			&u.MFASecretCipher, &keyVersion, &u.Disabled, &u.FailedLoginCount,
			&u.LockedUntil, &u.LastLoginAt, &u.PasswordChangedAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return Session{}, domain.User{}, "", mapError(err)
	}
	if keyVersion != nil {
		u.MFAKeyVersion = *keyVersion
	}
	return sess, u, csrfHash, nil
}

// TouchSession slides the idle window forward.
func (s *UserStore) TouchSession(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = now() WHERE id = $1`, id)
	return mapError(err)
}

// MarkSessionMFASatisfied promotes a half-authenticated session once the TOTP
// challenge succeeds.
func (s *UserStore) MarkSessionMFASatisfied(ctx context.Context, id uuid.UUID, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET mfa_satisfied = TRUE, expires_at = $2, last_seen_at = now() WHERE id = $1`,
		id, expiresAt)
	return mapError(err)
}

// RevokeSession ends one session.
func (s *UserStore) RevokeSession(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = now(), revoked_reason = $2
		WHERE id = $1 AND revoked_at IS NULL`, id, reason)
	return mapError(err)
}

// RevokeAllUserSessions ends every session for a user.
func (s *UserStore) RevokeAllUserSessions(ctx context.Context, userID uuid.UUID, reason string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = now(), revoked_reason = $2
		WHERE user_id = $1 AND revoked_at IS NULL`, userID, reason)
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// ListActiveSessions returns a user's live sessions for the security page.
func (s *UserStore) ListActiveSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, mfa_satisfied, host(ip_address), user_agent,
		       created_at, last_seen_at, expires_at, revoked_at, revoked_reason
		FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
		ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.MFASatisfied, &s.IPAddress, &s.UserAgent,
			&s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt, &s.RevokedReason); err != nil {
			return nil, mapError(err)
		}
		out = append(out, s)
	}
	return out, mapError(rows.Err())
}

// DeleteExpiredSessions prunes sessions that can no longer authenticate.
func (s *UserStore) DeleteExpiredSessions(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM sessions WHERE expires_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return 0, mapError(err)
	}
	return tag.RowsAffected(), nil
}

// CountByRole reports how many users hold a role.
//
// Used by the bootstrap command to refuse once an administrator exists. That
// check belongs in a query rather than in a "list everyone and count" loop,
// because the answer is needed before any user is trusted to be loaded.
func (s *UserStore) CountByRole(ctx context.Context, role domain.Role) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE role = $1`, string(role)).Scan(&n)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
