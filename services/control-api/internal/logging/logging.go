// Package logging provides structured logging with mandatory redaction.
//
// Every log line is JSON with a fixed core shape: timestamp, level, service,
// message, plus request and correlation identifiers when the line originates
// inside a request. Free-text logging is avoided because logs are queried
// during incidents, not read as prose.
//
// Redaction is enforced by the logger rather than left to call sites. A field
// whose key looks like a secret is replaced before it is written, so a careless
// log call cannot leak a password, a session token, a broker credential or an
// MFA secret. This is a backstop, not a licence: call sites must still not pass
// secrets in.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// redactedPlaceholder is written in place of any value whose key is sensitive.
const redactedPlaceholder = "[REDACTED]"

// sensitiveKeyFragments are matched case-insensitively as substrings of a log
// attribute's key. The list is deliberately broad: a false redaction costs a
// debugging session, a missed one costs a credential.
var sensitiveKeyFragments = []string{
	"password", "passwd", "secret", "token", "authorization", "auth_header",
	"cookie", "session_id", "sessionid", "api_key", "apikey", "credential",
	"private_key", "privatekey", "mfa", "totp", "otp", "recovery_code",
	"encryption_key", "signing_key", "bearer", "csrf",
}

// Options configures a logger.
type Options struct {
	Level   string
	Service string
	Env     string
	Writer  io.Writer
}

// Logger is a structured logger.
type Logger struct {
	*slog.Logger
}

// New builds a logger that writes JSON to stdout (or Options.Writer).
func New(opts Options) *Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       parseLevel(opts.Level),
		ReplaceAttr: redactAttr,
	})

	base := slog.New(handler).With(
		"service", opts.Service,
		"env", opts.Env,
	)
	return &Logger{Logger: base}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// redactAttr rewrites sensitive attributes before they are serialised.
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, redactedPlaceholder)
	}
	// Recurse into groups so nested attributes are covered too.
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		out := make([]slog.Attr, 0, len(attrs))
		for _, nested := range attrs {
			out = append(out, redactAttr(append(groups, a.Key), nested))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return a
}

// IsSensitiveKey reports whether an attribute key names something that must
// never be logged. Exported so HTTP middleware can filter headers with the
// same rules the logger applies.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, frag := range sensitiveKeyFragments {
		if strings.Contains(k, frag) {
			return true
		}
	}
	return false
}

// contextKey is the private type for context values owned by this package.
type contextKey struct{ name string }

var (
	requestIDKey     = contextKey{"request_id"}
	correlationIDKey = contextKey{"correlation_id"}
	loggerKey        = contextKey{"logger"}
)

// WithRequestID attaches a request identifier to the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request identifier, or "" when absent.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WithCorrelationID attaches a correlation identifier that spans the whole
// causal chain -- a strategy run, the order it produced and the fill that
// followed all share one, so an incident can be traced end to end.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

// CorrelationID returns the correlation identifier, or "" when absent.
func CorrelationID(ctx context.Context) string {
	if v, ok := ctx.Value(correlationIDKey).(string); ok {
		return v
	}
	return ""
}

// WithLogger stores a logger in the context.
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the context's logger enriched with its identifiers, or a
// discard logger when none is present. It never returns nil, so call sites do
// not have to guard.
func FromContext(ctx context.Context) *Logger {
	l, ok := ctx.Value(loggerKey).(*Logger)
	if !ok || l == nil {
		return &Logger{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	}
	args := make([]any, 0, 4)
	if id := RequestID(ctx); id != "" {
		args = append(args, "request_id", id)
	}
	if id := CorrelationID(ctx); id != "" {
		args = append(args, "correlation_id", id)
	}
	if len(args) == 0 {
		return l
	}
	return &Logger{Logger: l.With(args...)}
}
