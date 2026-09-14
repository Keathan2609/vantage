package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Helpers for the replay run record.
//
// Kept here rather than inline so app.go stays readable: the wiring in that
// file is already the densest part of the composition root.

// timePtr converts a zero time to nil.
//
// A zero timestamp in the record would be 0001-01-01, which reads as a real
// value and is not. NULL says "this run declared none", which a run recorded
// before the window existed genuinely did.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// decimalPtr parses a recorded balance, or nil when there was none.
//
// Nil rather than zero: an account whose balance could not be read and an
// account with no money are different facts, and only one of them is a
// starting condition.
func decimalPtr(s string) *decimal.Decimal {
	if s == "" {
		return nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return nil
	}
	return &d
}

// digestOf fingerprints a configuration for the run record.
//
// A digest rather than the document: the question a reader asks is "was this
// the same configuration", and a short hash answers it. Storing the document
// would also risk a credential or a limit set reaching a record that is read
// more widely than the configuration is.
func digestOf(v any) string {
	h := sha256.New()
	fmt.Fprintf(h, "%+v", v)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
