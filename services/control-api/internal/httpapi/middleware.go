package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/crypto"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/ratelimit"
	"github.com/vantage/control-api/internal/store"
)

// Session cookie names. The __Host- prefix is used in production, where it
// instructs the browser to accept the cookie only over HTTPS, only from this
// exact host, and only with Path=/. That closes off subdomain-injection paths
// that a plain cookie leaves open.
const (
	sessionCookieName    = "vantage_session"
	sessionCookieNameTLS = "__Host-vantage_session"
	csrfCookieName       = "vantage_csrf"
	csrfCookieNameTLS    = "__Host-vantage_csrf"
	csrfHeaderName       = "X-Vantage-CSRF"
	requestIDHeader      = "X-Request-ID"
)

type ctxKey struct{ name string }

var (
	ctxUser    = ctxKey{"user"}
	ctxSession = ctxKey{"session"}
)

// Principal is the authenticated caller.
type Principal struct {
	User    domain.User
	Session store.Session
}

// principalFrom returns the authenticated caller, if any.
func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxUser).(Principal)
	return p, ok
}

// requestIDMiddleware assigns or adopts a request identifier and a correlation
// identifier, and puts both on the context and the response.
func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A client-supplied request id is accepted for tracing, but only if it
		// looks like one: echoing arbitrary client input into logs and response
		// headers is a log-injection and header-injection vector.
		reqID := r.Header.Get(requestIDHeader)
		if !validTraceID(reqID) {
			reqID = uuid.NewString()
		}
		ctx := logging.WithRequestID(r.Context(), reqID)
		ctx = logging.WithCorrelationID(ctx, reqID)
		ctx = logging.WithLogger(ctx, s.log)
		w.Header().Set(requestIDHeader, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validTraceID accepts only short, printable, delimiter-free identifiers.
func validTraceID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// securityHeaders sets defensive response headers.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// This is a JSON API. It never returns HTML, so the CSP is maximally
		// restrictive: nothing may load, frame, or execute.
		h.Set("Content-Security-Policy",
			"default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
		// Financial responses must never be cached by an intermediary.
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
		h.Set("Pragma", "no-cache")
		if s.cfg.IsProduction() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware allows exactly one configured origin with credentials.
//
// There is no wildcard and no origin reflection. Reflecting the request's
// Origin with Allow-Credentials would let any site drive this API with the
// user's session cookie, which is the whole attack CSRF defences exist for.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	allowed := strings.TrimRight(s.cfg.PublicWebOrigin, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && strings.TrimRight(origin, "/") == allowed {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", allowed)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers",
				"Content-Type, "+csrfHeaderName+", "+requestIDHeader+", Idempotency-Key")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			h.Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bodyLimit caps request bodies. An unbounded body on an authenticated
// endpoint is a memory-exhaustion vector.
func bodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// observability records metrics and one structured log line per request.
func (s *Server) observability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		metrics.HTTPInFlight.Inc()
		defer metrics.HTTPInFlight.Dec()

		next.ServeHTTP(ww, r)

		// The route PATTERN is used as the metric label, never the raw path:
		// labelling by path would create one time series per order id.
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		status := ww.Status()
		duration := time.Since(start)

		metrics.HTTPRequests.WithLabelValues(route, r.Method, statusClass(status)).Inc()
		metrics.HTTPDuration.WithLabelValues(route, r.Method).Observe(duration.Seconds())

		log := logging.FromContext(r.Context())
		fields := []any{
			"method", r.Method, "route", route, "status", status,
			"duration_ms", duration.Milliseconds(), "bytes", ww.BytesWritten(),
			"ip", clientIP(r),
		}
		if p, ok := principalFrom(r.Context()); ok {
			fields = append(fields, "user_id", p.User.ID.String(), "role", string(p.User.Role))
		}
		switch {
		case status >= 500:
			log.Error("request failed", fields...)
		case status >= 400:
			log.Warn("request refused", fields...)
		default:
			log.Info("request", fields...)
		}
	})
}

func statusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// recoverer turns a panic into a 500 without leaking the stack to the client.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logging.FromContext(r.Context()).Error("panic recovered",
					"panic", fmt.Sprint(rec), "path", r.URL.Path, "method", r.Method)
				writeError(w, r, http.StatusInternalServerError, "internal_error",
					"Something went wrong handling this request.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// rateLimit applies a rule, keyed by user when authenticated and by address
// otherwise.
func (s *Server) rateLimit(rule ratelimit.Rule) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := ""
			if p, ok := principalFrom(r.Context()); ok {
				userID = p.User.ID.String()
			}
			identity := ratelimit.Identity(userID, clientIP(r), "")

			decision, err := s.limiter.Allow(r.Context(), rule, identity)
			if err != nil {
				logging.FromContext(r.Context()).Error("rate limiter error", "error", err.Error())
				next.ServeHTTP(w, r) // never fail closed on the limiter itself
				return
			}
			if !decision.Allowed {
				retry := int(decision.RetryAfter.Seconds())
				if retry < 1 {
					retry = 1
				}
				w.Header().Set("Retry-After", fmt.Sprint(retry))
				logging.FromContext(r.Context()).Warn("rate limited",
					"bucket", rule.Name, "ip", clientIP(r))
				writeError(w, r, http.StatusTooManyRequests, "rate_limited",
					"Too many requests. Please wait before trying again.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireAuth authenticates the session cookie.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			metrics.AuthFailures.WithLabelValues("session_invalid").Inc()
			writeError(w, r, http.StatusUnauthorized, "unauthenticated",
				"Sign in to continue.")
			return
		}
		// A session that has passed the password step but not the TOTP
		// challenge may only complete that challenge. Treating it as
		// authenticated would make MFA optional in practice.
		if p.User.MFAEnabled && !p.Session.MFASatisfied {
			writeError(w, r, http.StatusUnauthorized, "mfa_required",
				"Multi-factor authentication is required to continue.")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, p)
		ctx = context.WithValue(ctx, ctxSession, p.Session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requirePartialAuth accepts a session that has not yet cleared MFA. It is used
// only by the MFA challenge endpoint.
func (s *Server) requirePartialAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, p)
		ctx = context.WithValue(ctx, ctxSession, p.Session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authenticate resolves the session cookie into a principal.
func (s *Server) authenticate(r *http.Request) (Principal, error) {
	cookie, err := r.Cookie(s.sessionCookieName())
	if err != nil || cookie.Value == "" {
		return Principal{}, errors.New("no session cookie")
	}

	// Only a HASH of the token is stored, so a database dump yields no usable
	// sessions. The lookup hashes the presented token the same way.
	session, user, _, err := s.store.Users.SessionByTokenHash(r.Context(), hashToken(cookie.Value))
	if err != nil {
		return Principal{}, errors.New("session not found")
	}
	now := s.wallClock.Now()
	if !session.Active(now) {
		return Principal{}, errors.New("session expired or revoked")
	}
	// Idle timeout, independent of the absolute expiry.
	if now.Sub(session.LastSeenAt) > s.cfg.SessionIdleTTL {
		_ = s.store.Users.RevokeSession(r.Context(), session.ID, "idle_timeout")
		return Principal{}, errors.New("session idle timeout")
	}
	if user.Disabled {
		return Principal{}, errors.New("user disabled")
	}

	// Slide the idle window, but not on every request: one write per minute
	// per session is enough, and doing it per request would make every read
	// a write.
	if now.Sub(session.LastSeenAt) > time.Minute {
		_ = s.store.Users.TouchSession(r.Context(), session.ID)
	}
	return Principal{User: user, Session: session}, nil
}

// requireRole enforces a role.
func (s *Server) requireRole(roles ...domain.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := principalFrom(r.Context())
			if !ok {
				writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
				return
			}
			for _, role := range roles {
				if p.User.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			metrics.AuthorizationDenied.WithLabelValues("role").Inc()
			logging.FromContext(r.Context()).Warn("authorisation denied",
				"user_id", p.User.ID.String(), "role", string(p.User.Role),
				"required", fmt.Sprint(roles), "path", r.URL.Path)
			writeError(w, r, http.StatusForbidden, "forbidden",
				"Your role does not permit this action.")
		})
	}
}

// csrfProtect enforces double-submit CSRF defence on state-changing requests.
//
// Three things are checked, not one: the Origin header must match, the CSRF
// header must be present, and it must match the value bound to the session.
// SameSite cookies alone are not relied on, because SameSite behaviour varies
// by browser and by navigation type.
func (s *Server) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		expectedOrigin := strings.TrimRight(s.cfg.PublicWebOrigin, "/")
		if origin := r.Header.Get("Origin"); origin != "" {
			if strings.TrimRight(origin, "/") != expectedOrigin {
				metrics.AuthorizationDenied.WithLabelValues("csrf_origin").Inc()
				writeError(w, r, http.StatusForbidden, "csrf_origin_mismatch",
					"This request did not come from an allowed origin.")
				return
			}
		}

		cookie, err := r.Cookie(s.csrfCookieName())
		if err != nil || cookie.Value == "" {
			writeError(w, r, http.StatusForbidden, "csrf_missing",
				"Missing CSRF token. Reload the page and try again.")
			return
		}
		header := r.Header.Get(csrfHeaderName)
		if header == "" || !crypto.ConstantTimeEqualString(header, cookie.Value) {
			metrics.AuthorizationDenied.WithLabelValues("csrf_token").Inc()
			writeError(w, r, http.StatusForbidden, "csrf_invalid",
				"Invalid CSRF token. Reload the page and try again.")
			return
		}

		// Bind the token to the session so a token minted for one session
		// cannot be replayed in another.
		if p, ok := principalFrom(r.Context()); ok {
			_, _, storedHash, err := s.store.Users.SessionByTokenHash(r.Context(), hashToken(sessionTokenFrom(r, s)))
			if err == nil && storedHash != "" && !crypto.ConstantTimeEqualString(hashToken(header), storedHash) {
				metrics.AuthorizationDenied.WithLabelValues("csrf_session_binding").Inc()
				logging.FromContext(r.Context()).Warn("csrf token not bound to session",
					"user_id", p.User.ID.String())
				writeError(w, r, http.StatusForbidden, "csrf_invalid",
					"Invalid CSRF token. Reload the page and try again.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func sessionTokenFrom(r *http.Request, s *Server) string {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) sessionCookieName() string {
	if s.cfg.IsProduction() {
		return sessionCookieNameTLS
	}
	return sessionCookieName
}

func (s *Server) csrfCookieName() string {
	if s.cfg.IsProduction() {
		return csrfCookieNameTLS
	}
	return csrfCookieName
}

// setSessionCookies issues the session and CSRF cookies.
func (s *Server) setSessionCookies(w http.ResponseWriter, token, csrfToken string, expires time.Time) {
	secure := s.cfg.IsProduction()

	// Secure is set from the environment, not omitted: true in production,
	// where the __Host- prefixed cookie name also requires it, and false only
	// for local HTTP development. A literal true here would make the stack
	// unusable on localhost, which is how people end up disabling the check.
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	http.SetCookie(w, &http.Cookie{
		Name:  s.sessionCookieName(),
		Value: token,
		Path:  "/",
		// HttpOnly keeps the session out of reach of any script, so an XSS bug
		// in the frontend cannot exfiltrate it.
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
	})
	// The CSRF cookie is deliberately readable by script: the frontend must
	// echo it into a header. It is not a secret on its own -- it is only useful
	// combined with the HttpOnly session cookie, which script cannot read. The
	// double-submit defence requires same-origin script to read this value,
	// and a cross-site request cannot. Secure follows the environment as above.
	// nosemgrep: go.lang.security.audit.net.cookie-missing-httponly.cookie-missing-httponly, go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	http.SetCookie(w, &http.Cookie{
		Name:     s.csrfCookieName(),
		Value:    csrfToken,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
	})
}

// clearSessionCookies removes both cookies.
func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	secure := s.cfg.IsProduction()
	for _, name := range []string{s.sessionCookieName(), s.csrfCookieName()} {
		// Both are marked HttpOnly on the way out. Deletion is by name and
		// path, the value is empty and already expired, and nothing needs to
		// read a cookie that is being removed. Secure follows the environment,
		// as it does when the cookies are issued.
		// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", HttpOnly: true,
			Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
			Expires: time.Unix(0, 0),
		})
	}
}

// hashToken hashes a session or CSRF token for storage and comparison.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// signValue produces an HMAC over a value using the session signing key.
func (s *Server) signValue(value string) string {
	m := hmac.New(sha256.New, s.cfg.SessionSigningKey)
	m.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// clientIP extracts the caller's address.
//
// X-Forwarded-For is honoured ONLY when the server is configured to sit behind
// a trusted proxy. Trusting it unconditionally would let any client spoof its
// address and evade per-address rate limiting.
func clientIP(r *http.Request) string {
	if trusted, _ := r.Context().Value(ctxTrustProxy).(bool); trusted {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var ctxTrustProxy = ctxKey{"trust_proxy"}
