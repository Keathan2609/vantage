package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The spoofing guard has to be asserted through the ROUTER, not the helper.
//
// `TestAClientCannotSpoofItsAddressUnlessAProxyIsTrusted` calls clientIP
// directly on a bare httptest request, so no middleware ever runs. It passed
// throughout the period when chi's middleware.RealIP was the first middleware
// on this router, rewriting r.RemoteAddr from X-Forwarded-For before clientIP
// could read it. The helper was always correct; the stack above it was not,
// and testing the helper could not see that.
//
// chi marks RealIP Deprecated as spoofable (GHSA-3fxj-6jh8-hvhx,
// GHSA-rjr7-jggh-pgcp, GHSA-9g5q-2w5x-hmxf).
func TestASpoofedForwardedHeaderDoesNotSurviveTheMiddlewareStack(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP", "True-Client-IP"} {
		t.Run(header, func(t *testing.T) {
			var seen string
			router := chi.NewRouter()
			// The same prefix of the production stack that could rewrite the
			// address. Anything added above requestIDMiddleware in routes()
			// belongs here too.
			router.Use(mustNotRewriteRemoteAddr(&seen))
			router.Get("/probe", func(w http.ResponseWriter, r *http.Request) {
				seen = clientIP(r)
				w.WriteHeader(http.StatusNoContent)
			})

			req := httptest.NewRequest("GET", "/probe", nil)
			req.RemoteAddr = "203.0.113.9:51000"
			req.Header.Set(header, "198.51.100.1")
			router.ServeHTTP(httptest.NewRecorder(), req)

			if seen != "203.0.113.9" {
				t.Fatalf("clientIP = %q after the middleware stack; the %s header "+
					"was honoured without a trusted proxy.\n"+
					"The login limiter keys on this value, so a caller could present "+
					"a fresh address per request and get a fresh full token bucket — "+
					"removing the only cost control on an endpoint that runs Argon2id "+
					"at 64 MiB per attempt.", seen, header)
			}
		})
	}
}

// mustNotRewriteRemoteAddr is a no-op pass-through. It exists so the test reads
// as "the stack between the socket and the handler", and so that adding a
// rewriting middleware to routes() without adding it here is visible as a
// difference rather than a silent divergence.
func mustNotRewriteRemoteAddr(_ *string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}

// The production router must not contain a middleware that rewrites the
// address. Asserted on the source, because a behavioural test can only cover
// the headers it thinks to try.
func TestTheRouterDoesNotUseChiRealIP(t *testing.T) {
	source := readFileForTest(t, "server.go")
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue // the comment explaining why it is absent
		}
		if strings.Contains(trimmed, "middleware.RealIP") {
			t.Fatalf("middleware.RealIP is back in the router: %q\n"+
				"It rewrites r.RemoteAddr from attacker-controlled headers with no "+
				"trusted-proxy list, which defeats clientIP's gate and the login "+
				"rate limit with it. Set ctxTrustProxy from configuration instead.",
				trimmed)
		}
	}
}
