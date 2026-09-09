package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/auth"
	"github.com/vantage/control-api/internal/crypto"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/store"
)

// Lockout policy. Five failures start an exponential backoff capped at fifteen
// minutes: long enough to make online guessing impractical, short enough that
// an attacker cannot lock a known user out indefinitely as a denial of service.
const (
	lockoutThreshold = 5
	lockoutBase      = 30 * time.Second
	lockoutMax       = 15 * time.Minute
)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	User        userResponse `json:"user"`
	MFARequired bool         `json:"mfa_required"`
	CSRFToken   string       `json:"csrf_token"`
	ExpiresAt   time.Time    `json:"expires_at"`
}

type userResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	MFAEnabled  bool   `json:"mfa_enabled"`
}

func toUserResponse(u domain.User) userResponse {
	return userResponse{
		ID: u.ID.String(), Email: u.Email, DisplayName: u.DisplayName,
		Role: string(u.Role), MFAEnabled: u.MFAEnabled,
	}
}

// handleLogin authenticates a user and issues a session.
//
// Every failure path returns the SAME message and status. Distinguishing
// "no such user" from "wrong password" from "account locked" hands an attacker
// a user-enumeration oracle, and the small usability cost is not worth it on a
// financial system.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	log := logging.FromContext(r.Context())
	ip := clientIP(r)
	now := s.wallClock.Now()

	const genericFailure = "Those credentials are not valid."

	fail := func(reason string, userID *string) {
		metrics.AuthFailures.WithLabelValues(reason).Inc()
		fields := []any{"reason", reason, "ip", ip, "email_domain", emailDomain(email)}
		if userID != nil {
			fields = append(fields, "user_id", *userID)
		}
		log.Warn("authentication failed", fields...)
		writeError(w, r, http.StatusUnauthorized, "invalid_credentials", genericFailure)
	}

	if email == "" || req.Password == "" {
		fail("missing_credentials", nil)
		return
	}

	user, err := s.store.Users.UserByEmail(r.Context(), email)
	if err != nil {
		// Hash a dummy password anyway so a missing account and a wrong
		// password take comparable time. Without this, response timing reveals
		// which email addresses are registered.
		_ = auth.VerifyPassword(req.Password,
			"$argon2id$v=19$m=65536,t=3,p=4$YWFhYWFhYWFhYWFhYWFhYQ$"+
				"Y2FuYXJ5Y2FuYXJ5Y2FuYXJ5Y2FuYXJ5Y2FuYXJ5Y2FuYXI")
		fail("unknown_user", nil)
		return
	}

	uid := user.ID.String()
	if user.Disabled {
		fail("user_disabled", &uid)
		return
	}
	if user.Locked(now) {
		fail("account_locked", &uid)
		return
	}

	if err := auth.VerifyPassword(req.Password, user.PasswordHash); err != nil {
		locked, until, lerr := s.store.Users.RecordLoginFailure(
			r.Context(), user.ID, lockoutThreshold, lockoutBase, lockoutMax)
		if lerr != nil {
			log.Error("failed to record login failure", "error", lerr.Error())
		}
		if s.alerter != nil {
			// The attempted password is never recorded. The address and the
			// count are what an operator needs to tell a typo from an attack.
			s.alerter.RepeatedAuthFailure(r.Context(), user.Email,
				user.FailedLoginCount+1, locked, clientIP(r))
		}
		s.auditAuth(r, &user.ID, domain.AuditLoginFailed, domain.AuditFailure, map[string]any{
			"reason": "bad_password", "locked": locked,
		})
		if locked && until != nil {
			log.Warn("account locked after repeated failures", "user_id", uid, "until", until)
		}
		fail("bad_password", &uid)
		return
	}

	// The password is correct. If the stored hash predates the current cost
	// parameters, upgrade it now while the plaintext is available.
	if auth.NeedsRehash(user.PasswordHash) {
		if newHash, herr := auth.HashPassword(req.Password); herr == nil {
			if uerr := s.store.Users.SetPasswordHash(r.Context(), user.ID, newHash); uerr != nil {
				log.Warn("password rehash failed", "error", uerr.Error())
			}
		}
	}

	token, csrfToken, expires, err := s.issueSession(r, user, !user.MFAEnabled)
	if err != nil {
		log.Error("failed to issue session", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"Could not complete sign-in.")
		return
	}

	if err := s.store.Users.RecordLoginSuccess(r.Context(), user.ID, now); err != nil {
		log.Warn("failed to record login success", "error", err.Error())
	}
	metrics.AuthSuccesses.Inc()
	s.auditAuth(r, &user.ID, domain.AuditLoginSucceeded, domain.AuditSuccess, map[string]any{
		"mfa_required": user.MFAEnabled,
	})

	s.setSessionCookies(w, token, csrfToken, expires)
	writeJSON(w, r, http.StatusOK, loginResponse{
		User:        toUserResponse(user),
		MFARequired: user.MFAEnabled,
		CSRFToken:   csrfToken,
		ExpiresAt:   expires,
	})
}

// issueSession mints a session and its CSRF token.
//
// The CSRF token's hash is stored in the session row, binding the two: a token
// minted for one session cannot be replayed against another.
func (s *Server) issueSession(r *http.Request, user domain.User, mfaSatisfied bool) (token, csrfToken string, expires time.Time, err error) {
	token, err = crypto.RandomToken(32)
	if err != nil {
		return "", "", time.Time{}, err
	}
	csrfToken, err = crypto.RandomToken(32)
	if err != nil {
		return "", "", time.Time{}, err
	}

	// A half-authenticated session gets a short life: it exists only long
	// enough to complete the TOTP challenge.
	ttl := s.cfg.SessionTTL
	if !mfaSatisfied {
		ttl = 10 * time.Minute
	}
	expires = s.wallClock.Now().Add(ttl)

	ip := clientIP(r)
	ua := r.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}

	_, err = s.store.Users.CreateSession(r.Context(), user.ID,
		hashToken(token), hashToken(csrfToken), mfaSatisfied, &ip, &ua, expires)
	if err != nil {
		return "", "", time.Time{}, err
	}
	return token, csrfToken, expires, nil
}

type mfaVerifyRequest struct {
	Code string `json:"code"`
}

// handleMFAVerify completes the TOTP challenge.
func (s *Server) handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req mfaVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	log := logging.FromContext(r.Context())
	now := s.wallClock.Now()

	if !p.User.MFAEnabled || len(p.User.MFASecretCipher) == 0 {
		writeError(w, r, http.StatusBadRequest, "mfa_not_enabled",
			"Multi-factor authentication is not enabled for this account.")
		return
	}
	if p.Session.MFASatisfied {
		writeJSON(w, r, http.StatusOK, map[string]any{"status": "already_verified"})
		return
	}

	secret, err := s.keyring.Decrypt(p.User.MFASecretCipher, []byte(p.User.ID.String()))
	if err != nil {
		log.Error("failed to decrypt MFA secret", "user_id", p.User.ID.String())
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"Could not verify the code.")
		return
	}
	// The plaintext secret exists only inside this function.
	defer zero(secret)

	code := strings.TrimSpace(req.Code)

	// A recovery code is accepted in place of a TOTP code, and is consumed
	// atomically so it cannot be used twice.
	if len(code) > 8 && strings.Contains(code, "-") {
		hash := crypto.HashRecoveryCode(s.cfg.SessionSigningKey, strings.ToUpper(code))
		used, cerr := s.store.Users.ConsumeRecoveryCode(r.Context(), p.User.ID, hash)
		if cerr != nil || !used {
			metrics.AuthFailures.WithLabelValues("bad_recovery_code").Inc()
			s.auditAuth(r, &p.User.ID, domain.AuditMFAChallengeFailed, domain.AuditFailure,
				map[string]any{"method": "recovery_code"})
			writeError(w, r, http.StatusUnauthorized, "invalid_code", "That code is not valid.")
			return
		}
		s.completeMFA(w, r, p, now, "recovery_code")
		return
	}

	if !auth.VerifyTOTP(string(secret), code, now) {
		metrics.AuthFailures.WithLabelValues("bad_totp").Inc()
		s.auditAuth(r, &p.User.ID, domain.AuditMFAChallengeFailed, domain.AuditFailure,
			map[string]any{"method": "totp"})
		writeError(w, r, http.StatusUnauthorized, "invalid_code", "That code is not valid.")
		return
	}
	s.completeMFA(w, r, p, now, "totp")
}

func (s *Server) completeMFA(w http.ResponseWriter, r *http.Request, p Principal, now time.Time, method string) {
	expires := now.Add(s.cfg.SessionTTL)
	if err := s.store.Users.MarkSessionMFASatisfied(r.Context(), p.Session.ID, expires); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"Could not complete verification.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditLoginSucceeded, domain.AuditSuccess,
		map[string]any{"mfa_method": method})
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "verified", "expires_at": expires,
	})
}

// handleLogout revokes the current session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if p, ok := principalFrom(r.Context()); ok {
		if err := s.store.Users.RevokeSession(r.Context(), p.Session.ID, "logout"); err != nil {
			logging.FromContext(r.Context()).Warn("failed to revoke session", "error", err.Error())
		}
		s.auditAuth(r, &p.User.ID, domain.AuditLogout, domain.AuditSuccess, nil)
	}
	s.clearSessionCookies(w)
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "signed_out"})
}

type sessionResponse struct {
	User           userResponse `json:"user"`
	ExecutionMode  string       `json:"execution_mode"`
	SimulatedFunds bool         `json:"simulated_funds"`
	MFASatisfied   bool         `json:"mfa_satisfied"`
	ExpiresAt      time.Time    `json:"expires_at"`
}

// handleSession returns the current principal.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	writeJSON(w, r, http.StatusOK, sessionResponse{
		User:           toUserResponse(p.User),
		ExecutionMode:  s.cfg.ExecutionMode,
		SimulatedFunds: s.cfg.SimulatedFunds(),
		MFASatisfied:   p.Session.MFASatisfied,
		ExpiresAt:      p.Session.ExpiresAt,
	})
}

type sessionSummary struct {
	ID         string    `json:"id"`
	IPAddress  *string   `json:"ip_address"`
	UserAgent  *string   `json:"user_agent"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

// handleListSessions shows a user their active sessions.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	sessions, err := s.store.Users.ListActiveSessions(r.Context(), p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "No sessions found.")
		return
	}
	out := make([]sessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionSummary{
			ID: sess.ID.String(), IPAddress: sess.IPAddress, UserAgent: sess.UserAgent,
			CreatedAt: sess.CreatedAt, LastSeenAt: sess.LastSeenAt, ExpiresAt: sess.ExpiresAt,
			Current: sess.ID == p.Session.ID,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sessions": out})
}

// handleRevokeSession ends one of the caller's own sessions.
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	sessionID, ok := parseUUID(w, r, chi.URLParam(r, "sessionID"), "Session id")
	if !ok {
		return
	}

	// Scoped to the caller's own sessions: revoking is only ever self-service.
	sessions, err := s.store.Users.ListActiveSessions(r.Context(), p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "Session not found.")
		return
	}
	owned := false
	for _, sess := range sessions {
		if sess.ID == sessionID {
			owned = true
			break
		}
	}
	if !owned {
		writeError(w, r, http.StatusNotFound, "not_found", "Session not found.")
		return
	}

	if err := s.store.Users.RevokeSession(r.Context(), sessionID, "revoked_by_user"); err != nil {
		writeStoreError(w, r, err, "Session not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditSessionRevoked, domain.AuditSuccess,
		map[string]any{"session_id": sessionID.String()})
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "revoked"})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword rotates the caller's password.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req changePasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	// The current password is required even though the caller is already
	// authenticated: it re-establishes that the person at the keyboard is the
	// account owner and not someone using an unattended session.
	if err := auth.VerifyPassword(req.CurrentPassword, p.User.PasswordHash); err != nil {
		metrics.AuthFailures.WithLabelValues("password_change_bad_current").Inc()
		writeError(w, r, http.StatusUnauthorized, "invalid_credentials",
			"Your current password is not correct.")
		return
	}
	if err := auth.ValidatePassword(req.NewPassword, p.User.Email, p.User.DisplayName); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "weak_password", err.Error())
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"Could not update the password.")
		return
	}
	// SetPasswordHash also revokes every session, including this one.
	if err := s.store.Users.SetPasswordHash(r.Context(), p.User.ID, hash); err != nil {
		writeStoreError(w, r, err, "Could not update the password.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditPasswordChanged, domain.AuditSuccess, nil)
	s.clearSessionCookies(w)
	writeJSON(w, r, http.StatusOK, map[string]string{
		"status":  "password_changed",
		"message": "Your password was changed and all sessions were signed out.",
	})
}

type mfaEnrollResponse struct {
	Secret          string   `json:"secret"`
	ProvisioningURI string   `json:"provisioning_uri"`
	RecoveryCodes   []string `json:"recovery_codes"`
	Message         string   `json:"message"`
}

// handleMFAEnroll begins TOTP enrolment.
//
// The secret and recovery codes are returned exactly once, here. They are
// never retrievable afterwards, never logged, and stored only encrypted
// (secret) or hashed (recovery codes).
func (s *Server) handleMFAEnroll(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if p.User.MFAEnabled {
		writeError(w, r, http.StatusConflict, "mfa_already_enabled",
			"Multi-factor authentication is already enabled. Disable it first to re-enrol.")
		return
	}

	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Could not start enrolment.")
		return
	}
	// The secret is bound to this user via the associated data, so a
	// ciphertext moved to another row will not decrypt.
	cipher, err := s.keyring.Encrypt([]byte(secret), []byte(p.User.ID.String()))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Could not start enrolment.")
		return
	}
	// Stored with mfa_enabled still false: enrolment is not complete until a
	// code is verified, so a user cannot lock themselves out by abandoning it.
	if err := s.store.Users.SetMFASecret(r.Context(), p.User.ID, cipher, s.keyring.ActiveVersion(), false); err != nil {
		writeStoreError(w, r, err, "Could not start enrolment.")
		return
	}

	codes, err := auth.GenerateRecoveryCodes(10)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Could not start enrolment.")
		return
	}
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, crypto.HashRecoveryCode(s.cfg.SessionSigningKey, c))
	}
	if err := s.store.Users.ReplaceRecoveryCodes(r.Context(), p.User.ID, hashes); err != nil {
		writeStoreError(w, r, err, "Could not start enrolment.")
		return
	}

	writeJSON(w, r, http.StatusOK, mfaEnrollResponse{
		Secret:          secret,
		ProvisioningURI: auth.TOTPProvisioningURI(secret, p.User.Email, "Vantage"),
		RecoveryCodes:   codes,
		Message: "Store these recovery codes somewhere safe. They are shown once and cannot be " +
			"retrieved again. Enrolment completes when you verify a code.",
	})
}

// handleMFAActivate completes enrolment by verifying a code.
func (s *Server) handleMFAActivate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req mfaVerifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if len(p.User.MFASecretCipher) == 0 {
		writeError(w, r, http.StatusBadRequest, "mfa_not_started",
			"Start enrolment before activating.")
		return
	}
	secret, err := s.keyring.Decrypt(p.User.MFASecretCipher, []byte(p.User.ID.String()))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Could not verify the code.")
		return
	}
	defer zero(secret)

	if !auth.VerifyTOTP(string(secret), strings.TrimSpace(req.Code), s.wallClock.Now()) {
		writeError(w, r, http.StatusUnauthorized, "invalid_code", "That code is not valid.")
		return
	}
	if err := s.store.Users.SetMFASecret(r.Context(), p.User.ID,
		p.User.MFASecretCipher, p.User.MFAKeyVersion, true); err != nil {
		writeStoreError(w, r, err, "Could not enable multi-factor authentication.")
		return
	}
	if err := s.store.Users.MarkSessionMFASatisfied(r.Context(), p.Session.ID,
		s.wallClock.Now().Add(s.cfg.SessionTTL)); err != nil {
		logging.FromContext(r.Context()).Warn("could not mark session MFA-satisfied", "error", err.Error())
	}
	s.auditAuth(r, &p.User.ID, domain.AuditMFAEnrolled, domain.AuditSuccess, nil)
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "mfa_enabled"})
}

type mfaDisableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// handleMFADisable turns MFA off.
//
// Both the password and a current code are required. Removing a security
// control is exactly the action an attacker with a hijacked session would
// take, so it demands proof of both factors.
func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req mfaDisableRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if !p.User.MFAEnabled {
		writeError(w, r, http.StatusBadRequest, "mfa_not_enabled",
			"Multi-factor authentication is not enabled.")
		return
	}
	if err := auth.VerifyPassword(req.Password, p.User.PasswordHash); err != nil {
		writeError(w, r, http.StatusUnauthorized, "invalid_credentials",
			"Your password is not correct.")
		return
	}
	secret, err := s.keyring.Decrypt(p.User.MFASecretCipher, []byte(p.User.ID.String()))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Could not verify the code.")
		return
	}
	defer zero(secret)
	if !auth.VerifyTOTP(string(secret), strings.TrimSpace(req.Code), s.wallClock.Now()) {
		writeError(w, r, http.StatusUnauthorized, "invalid_code", "That code is not valid.")
		return
	}
	if err := s.store.Users.DisableMFA(r.Context(), p.User.ID); err != nil {
		writeStoreError(w, r, err, "Could not disable multi-factor authentication.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditMFADisabled, domain.AuditSuccess, nil)
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "mfa_disabled"})
}

// auditAuth records an authentication event on the hash chain.
func (s *Server) auditAuth(r *http.Request, userID *uuid.UUID, action domain.AuditAction,
	result domain.AuditResult, meta map[string]any) {

	raw := json.RawMessage(`{}`)
	if meta != nil {
		if b, err := json.Marshal(meta); err == nil {
			raw = b
		}
	}
	ip := clientIP(r)
	ua := r.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}

	err := s.store.Pool().InTx(r.Context(), func(tx pgx.Tx) error {
		_, err := s.store.Control.AppendAudit(r.Context(), tx, domain.AuditEvent{
			ActorUserID:   userID,
			ActorType:     actorTypeFor(userID),
			Action:        action,
			TargetType:    "user",
			TargetID:      userIDString(userID),
			Result:        result,
			RequestID:     logging.RequestID(r.Context()),
			CorrelationID: logging.CorrelationID(r.Context()),
			IPAddress:     &ip,
			UserAgent:     &ua,
			Metadata:      raw,
		})
		return err
	})
	if err != nil {
		logging.FromContext(r.Context()).Error("failed to append audit event",
			"action", string(action), "error", err.Error())
	}
}

func actorTypeFor(userID *uuid.UUID) string {
	if userID == nil {
		return "anonymous"
	}
	return "user"
}

func userIDString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

// emailDomain returns only the domain part, so authentication logs can be
// analysed without recording which individual addresses were attempted.
func emailDomain(email string) string {
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return "unknown"
	}
	return domain
}

// zero overwrites a secret in memory once it is no longer needed. Go's garbage
// collector may still have copied it, so this is a reduction in exposure
// window rather than a guarantee.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// storeErr is a convenience for handlers that only need the not-found case.
func storeErr(err error) bool { return errors.Is(err, store.ErrNotFound) }
