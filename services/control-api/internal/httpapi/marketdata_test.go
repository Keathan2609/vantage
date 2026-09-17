package httpapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vantage/control-api/internal/config"
	"github.com/vantage/control-api/internal/marketdata"
)

// The secret boundary and the route table. Handler behaviour that needs a
// database is covered by the smoke suite against a running stack; what is
// proved here is what a unit test can actually prove -- that the API key
// cannot reach a response or a stored record, and which role each route sits
// behind.

func TestTheTwelveDataKeyIsNotInTheConfigDigest(t *testing.T) {
	// The digest travels into stored replay-run records. A secret that reached
	// it would be persisted, exported, and impossible to rotate out of history.
	base := testConfig()
	withKey := testConfig()
	withKey.TwelveDataAPIKey = "a-real-looking-secret-value"

	if configDigest(base) != configDigest(withKey) {
		t.Fatal("the API key changes the configuration digest, so it is being " +
			"hashed into records that are stored and exported")
	}
}

func TestProviderHealthSerialisesWithoutTheKey(t *testing.T) {
	// A health payload is the easiest place to leak a secret, because it is
	// the one place everybody agrees should show everything about a
	// connection. This pins the serialised SHAPE: a future field carrying the
	// key fails here.
	const secret = "sk-test-key-that-must-not-escape"
	provider := newTestTwelveData(t, secret)

	// Drive a failure so the reason field is populated; an error path that
	// echoed the provider's response could carry the key back.
	encoded, err := json.Marshal(provider.Health())
	if err != nil {
		t.Fatalf("marshal health: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("the API key appears in serialised provider health: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"configured":true`) {
		t.Fatalf("health does not report whether a key is configured: %s", encoded)
	}
}

func TestAnUnconfiguredProviderReportsItsStateRatherThanFailing(t *testing.T) {
	// Section 26: the platform must start without a key, and an operator must
	// be able to see why acquisition is unavailable.
	provider := newTestTwelveData(t, "")
	health := provider.Health()

	if health.State != marketdata.ProviderMisconfigured {
		t.Fatalf("state = %s, want MISCONFIGURED", health.State)
	}
	if health.Configured {
		t.Fatal("an absent key reported as configured")
	}
	if health.LastFailureReason == "" {
		t.Fatal("no reason was given for the unconfigured state")
	}
}

func TestTheAcquisitionRoutesAreRegisteredInTheAdminGroup(t *testing.T) {
	// Read the route table rather than calling handlers. The role is enforced
	// by the router GROUP, so a handler-level test would pass even if the
	// route were moved out from behind the admin middleware -- which is
	// exactly the refactor this guards against.
	source := readServerSource(t)

	adminGroup := routerGroup(t, source, "admin.Use(s.requireRole(domain.RoleAdmin))")

	for _, route := range []string{
		`admin.Post("/market-data/backfill"`,
		`admin.Post("/market-data/sync"`,
		`admin.Post("/market-data/repair"`,
	} {
		if !strings.Contains(adminGroup, route) {
			t.Errorf("%s is not inside the admin group.\n"+
				"Acquiring data spends a metered external quota and writes to the "+
				"bar series every strategy reads; it must not be reachable by a "+
				"trader or a viewer.", route)
		}
	}
}

func TestTheMarketDataReadRoutesAreRegisteredAndNotAdminOnly(t *testing.T) {
	// An operator who cannot see what data the platform holds cannot interpret
	// anything built on it. Reading is deliberately not privileged.
	source := readServerSource(t)

	for _, route := range []string{
		`auth.Get("/market-data/instruments"`,
		`auth.Get("/market-data/status"`,
		`auth.Get("/market-data/coverage"`,
	} {
		if !strings.Contains(source, route) {
			t.Errorf("%s is not registered as an authenticated read route", route)
		}
	}

	adminGroup := routerGroup(t, source, "admin.Use(s.requireRole(domain.RoleAdmin))")
	for _, route := range []string{
		"/market-data/instruments", "/market-data/status", "/market-data/coverage",
	} {
		if strings.Contains(adminGroup, `admin.Get("`+route+`"`) {
			t.Errorf("%s is admin-only; reading stored market data should not be", route)
		}
	}
}

func TestTheProviderBaseURLIsNotTakenFromARequest(t *testing.T) {
	// An endpoint whose destination a client could steer is an SSRF primitive
	// pointed at whatever this server can reach. The handlers accept an
	// instrument, a timeframe and a range -- never a URL, a host or a path.
	source := readFileForTest(t, "handlers_marketdata.go")

	for _, forbidden := range []string{
		"http.Get(", "http.Post(", "http.NewRequest",
		`Query().Get("url")`, `Query().Get("endpoint")`, `Query().Get("provider_url")`,
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("handlers_marketdata.go contains %q.\n"+
				"The market-data handlers must never construct an outbound request "+
				"from request input; the provider base URL is fixed configuration.",
				forbidden)
		}
	}
}

func TestAConfiguredKeyIsDetectedAndWhitespaceIsNot(t *testing.T) {
	cfg := config.Config{}
	if cfg.TwelveDataConfigured() {
		t.Fatal("an empty configuration reported a configured provider")
	}
	cfg.TwelveDataAPIKey = "   "
	if cfg.TwelveDataConfigured() {
		t.Fatal("whitespace counted as a configured key")
	}
	cfg.TwelveDataAPIKey = "x"
	if !cfg.TwelveDataConfigured() {
		t.Fatal("a present key reported as unconfigured")
	}
}

// --- helpers -----------------------------------------------------------------

func newTestTwelveData(t *testing.T, key string) *marketdata.TwelveDataProvider {
	t.Helper()
	symbols, err := marketdata.NewSymbolMap("twelvedata", marketdata.TwelveDataSymbols)
	if err != nil {
		t.Fatalf("symbol map: %v", err)
	}
	provider, err := marketdata.NewTwelveDataProvider(
		"https://api.twelvedata.com", key, symbols, testClockForMarketData{}, 7, time.Second)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return provider
}

type testClockForMarketData struct{}

func (testClockForMarketData) Now() time.Time {
	return time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
}

// routerGroup returns the body of one chi group, bounded at its closing brace.
//
// The bound matters more than it looks. An earlier version of this helper took
// the text from the group marker to the next "// ----" comment, and since the
// admin group is the last one, that ran 9 777 bytes past it to the end of the
// file -- so a route registered in ANY later group would still have satisfied
// an "is it in the admin group" assertion. A probe caught it. The section is
// now closed at the group's own indentation level.
func routerGroup(t *testing.T, source, marker string) string {
	t.Helper()
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatalf("marker %q not found; the route table has been restructured "+
			"and this test no longer checks what it claims to", marker)
	}
	rest := source[start:]
	// chi groups in this file close with a tab-tab-brace-paren at the end of a
	// line. The first one after the marker ends the group.
	end := strings.Index(rest, "\n\t\t})")
	if end < 0 {
		t.Fatal("could not find the end of the router group; the bound would be " +
			"the rest of the file and the assertion would be meaningless")
	}
	section := rest[:end]
	// A group that swallowed the whole file would make every assertion pass.
	if len(section) > 8000 {
		t.Fatalf("the router group section is %d bytes, which is larger than a "+
			"single group should be; the bound has stopped working", len(section))
	}
	return section
}

// readServerSource reads the route table.
//
// Reading the source is cruder than parsing it and deliberately so: a check
// over the text fails loudly when the file is restructured, which is exactly
// when a route's protection is most likely to have moved.
func readServerSource(t *testing.T) string {
	t.Helper()
	return readFileForTest(t, "server.go")
}

func readFileForTest(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty; this test would pass vacuously", name)
	}
	return string(data)
}
