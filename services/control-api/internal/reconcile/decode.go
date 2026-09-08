package reconcile

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Decoding helpers for evidence stored as JSON.
//
// Kept together and deliberately narrow. Evidence is written by this package
// and read back by this package, but it is still data that has been through a
// database and could have been edited there, so every value is parsed rather
// than asserted — a malformed number must produce a refused action, not a
// panic or a zero silently booked into the ledger.

func jsonUnmarshal(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return fmt.Errorf("no evidence recorded")
	}
	return json.Unmarshal(raw, into)
}

// decimalFromString parses an exact decimal.
//
// No float anywhere in this path. A quantity or price that arrived as a string
// stays a string until it becomes a decimal, because the one thing worse than
// refusing to import an execution is importing it with a rounding error.
func decimalFromString(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, fmt.Errorf("empty value")
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, err
	}
	return d, nil
}

func zeroDecimal() decimal.Decimal { return decimal.Zero }

// timeFromString parses a timestamp as stored by encoding/json.
func timeFromString(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			// Truncated to microseconds because Postgres stores microseconds
			// and the audit chain depends on the two agreeing.
			return t.UTC().Truncate(time.Microsecond), nil
		}
	}
	return time.Time{}, fmt.Errorf("not an RFC3339 timestamp")
}
