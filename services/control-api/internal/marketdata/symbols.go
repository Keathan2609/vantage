package marketdata

import (
	"fmt"
	"sort"
	"strings"
)

// Symbol mapping between Vantage's canonical instrument ids and the strings a
// particular provider expects.
//
// # Why this is a layer rather than a string concatenation
//
// Vantage calls gold XAUUSD. Twelve Data calls it XAU/USD. A broker will call
// it something else again, probably with a suffix that means a contract size.
// Each of those is that provider's private business, and the moment a
// provider's spelling appears in a strategy, a query, a chart URL or a
// research dataset, changing provider stops being a configuration change and
// becomes a search-and-replace across the codebase.
//
// So the translation happens exactly once, at the provider boundary, in both
// directions, and everything upstream speaks only canonical ids.
//
// # Why it is a registry rather than a transformation
//
// "Insert a slash before the last three characters" works for XAUUSD and fails
// for USDZAR the moment a provider writes it differently, and fails silently:
// the request succeeds against a symbol that is not the instrument asked for.
// An explicit table refuses what it does not know.

// SymbolMap translates canonical instrument ids to one provider's symbols.
type SymbolMap struct {
	provider string
	toVendor map[string]string
	toCanon  map[string]string
}

// UnknownSymbolError is returned when a mapping is absent.
//
// A distinct type because "this provider does not carry this instrument" is an
// ordinary, expected condition that a caller should handle by skipping the
// instrument, not by failing the run.
type UnknownSymbolError struct {
	Provider string
	Symbol   string
}

func (e UnknownSymbolError) Error() string {
	return fmt.Sprintf("marketdata: %s has no symbol mapping for %q", e.Provider, e.Symbol)
}

// NewSymbolMap builds a map from canonical id to vendor symbol.
func NewSymbolMap(provider string, pairs map[string]string) (*SymbolMap, error) {
	m := &SymbolMap{
		provider: provider,
		toVendor: make(map[string]string, len(pairs)),
		toCanon:  make(map[string]string, len(pairs)),
	}
	for canonical, vendor := range pairs {
		canonical, vendor = strings.TrimSpace(canonical), strings.TrimSpace(vendor)
		if canonical == "" || vendor == "" {
			return nil, fmt.Errorf("marketdata: %s symbol map has an empty entry", provider)
		}
		// A vendor symbol mapping to two canonical ids would make the reverse
		// direction ambiguous, and the reverse direction is what attributes
		// incoming bars to an instrument. Refused at construction rather than
		// discovered when the wrong instrument gets the data.
		if existing, clash := m.toCanon[vendor]; clash {
			return nil, fmt.Errorf(
				"marketdata: %s maps %q to both %s and %s; incoming bars could not be "+
					"attributed", provider, vendor, existing, canonical)
		}
		m.toVendor[canonical] = vendor
		m.toCanon[vendor] = canonical
	}
	return m, nil
}

// Vendor returns the provider's symbol for a canonical instrument id.
func (m *SymbolMap) Vendor(canonical string) (string, error) {
	if v, ok := m.toVendor[canonical]; ok {
		return v, nil
	}
	return "", UnknownSymbolError{Provider: m.provider, Symbol: canonical}
}

// Canonical returns the Vantage instrument id for a provider symbol.
func (m *SymbolMap) Canonical(vendor string) (string, error) {
	if c, ok := m.toCanon[vendor]; ok {
		return c, nil
	}
	return "", UnknownSymbolError{Provider: m.provider, Symbol: vendor}
}

// Instruments lists the canonical ids this provider can supply, sorted.
func (m *SymbolMap) Instruments() []string {
	out := make([]string, 0, len(m.toVendor))
	for canonical := range m.toVendor {
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out
}

// Supports reports whether the provider carries an instrument.
func (m *SymbolMap) Supports(canonical string) bool {
	_, ok := m.toVendor[canonical]
	return ok
}

// TwelveDataSymbols is the mapping for Twelve Data.
//
// Gold only, deliberately. The brief asks for XAU/USD and one instrument
// proven end to end is worth more than five half-checked ones; each addition
// needs its own verification that the vendor symbol is the instrument we think
// it is, at the resolution we think it is.
//
// XAUUSD.m is Vantage's broker-contract instrument and is NOT mapped. Twelve
// Data quotes a spot gold price; a broker's .m contract has its own
// specification, session and pricing. Treating a generic spot series as that
// contract's history would produce research about one instrument presented as
// research about another. The mapping's absence is the enforcement.
var TwelveDataSymbols = map[string]string{
	"XAUUSD": "XAU/USD",
}
