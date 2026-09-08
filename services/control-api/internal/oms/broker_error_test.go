package oms

import (
	"errors"
	"fmt"
	"testing"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
)

// The branch table on which "never retry an unknown outcome" rests.
//
// The previous audit found that three of these six branches were reached by no
// test at all. Getting one wrong means an order that DID reach the market is
// recorded as refused — after which its risk budget is released and the
// position it opened is invisible to every subsequent risk check.

func TestADefinitiveVenueRejectionIsAKnownOutcome(t *testing.T) {
	d := ClassifyBrokerError(broker.RejectionError{
		Reason: "price too far from market", Code: "10015",
	})

	if !d.OutcomeKnown {
		t.Fatal("a venue that named a rejection reason is the most definitive answer " +
			"available; treating it as unknown would halt the account for nothing")
	}
	if d.Rejection.Code != domain.RejectBrokerRejected {
		t.Errorf("reject code is %s; expected broker_rejected", d.Rejection.Code)
	}
	if d.Rejection.Message == "" {
		t.Error("the venue's own reason was discarded")
	}
}

func TestTheVenueCodeIsRetainedForADiagnosableRejection(t *testing.T) {
	// A venue error code is the difference between "the broker said no" and a
	// specific, searchable cause. Losing it costs an operator the one detail
	// that makes the refusal actionable.
	d := ClassifyBrokerError(broker.RejectionError{Reason: "invalid volume", Code: "10014"})
	if v := d.Rejection.Detail["venue_code"]; v != "10014" {
		t.Errorf("the venue code is %q, not the venue's own 10014: %+v", v, d.Rejection)
	}
}

func TestEachDefinitiveErrorMapsToItsOwnRejectCode(t *testing.T) {
	// Distinct codes matter: the metrics, the UI and the audit log all branch
	// on them, and collapsing "no margin" into "broker rejected" would make
	// "why did nothing trade today?" unanswerable.
	cases := []struct {
		name string
		err  error
		want domain.RejectCode
	}{
		{"sentinel rejection", broker.ErrOrderRejected, domain.RejectBrokerRejected},
		{"insufficient margin", broker.ErrInsufficientMargin, domain.RejectInsufficientMargin},
		{"market closed", broker.ErrMarketClosed, domain.RejectMarketClosed},
		{"venue unavailable", broker.ErrVenueUnavailable, domain.RejectBrokerUnavailable},
		{"rate limited", broker.ErrRateLimited, domain.RejectBrokerUnavailable},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := ClassifyBrokerError(c.err)
			if !d.OutcomeKnown {
				t.Fatalf("%v is a definitive answer but was classified as unknown; the "+
					"order would be left FAILED and the account halted for a refusal the "+
					"venue was clear about", c.err)
			}
			if d.Rejection.Code != c.want {
				t.Errorf("reject code is %s; expected %s", d.Rejection.Code, c.want)
			}
		})
	}
}

func TestADefinitiveErrorSurvivesWrapping(t *testing.T) {
	// Adapters wrap. A classification that only worked on bare sentinels would
	// silently start returning "unknown" the first time someone added context
	// to an error message.
	wrapped := fmt.Errorf("mock venue: place order: %w", broker.ErrMarketClosed)
	d := ClassifyBrokerError(wrapped)
	if !d.OutcomeKnown || d.Rejection.Code != domain.RejectMarketClosed {
		t.Fatalf("a wrapped ErrMarketClosed was classified as %+v", d)
	}
}

// TestAnUnknownOutcomeIsNeverReportedAsAKnownRefusal is the branch that
// matters most.
func TestAnUnknownOutcomeIsNeverReportedAsAKnownRefusal(t *testing.T) {
	d := ClassifyBrokerError(broker.ErrUnknownOutcome)
	if d.OutcomeKnown {
		t.Fatal("ErrUnknownOutcome was classified as a known refusal. The order may be " +
			"live at the venue: closing it out releases risk budget for a position that " +
			"exists, and the position is then invisible to every later risk check")
	}
	if d.Rejection.Message == "" {
		t.Error("an unknown outcome carries no explanation for the operator")
	}
}

func TestAnUnknownOutcomeSurvivesWrapping(t *testing.T) {
	wrapped := fmt.Errorf("%w: injected lost response after venue acceptance",
		broker.ErrUnknownOutcome)
	if ClassifyBrokerError(wrapped).OutcomeKnown {
		t.Fatal("a wrapped ErrUnknownOutcome was classified as a known refusal")
	}
}

// TestAnUnrecognisedErrorFailsClosed is the property that makes this table
// safe to extend.
func TestAnUnrecognisedErrorFailsClosed(t *testing.T) {
	for _, err := range []error{
		errors.New("connection reset by peer"),
		errors.New("context deadline exceeded"),
		errors.New(""),
		fmt.Errorf("some future broker error nobody has classified yet"),
	} {
		if ClassifyBrokerError(err).OutcomeKnown {
			t.Fatalf("%q was classified as a definitive refusal. Anything this switch "+
				"has never heard of must be UNKNOWN: a new adapter error that defaulted "+
				"to 'rejected' would silently release risk budget for live positions", err)
		}
	}
}

// TestANilErrorIsStillTreatedAsUnknown.
//
// A nil error should never reach here — the caller only calls this on failure —
// but if it ever did, the safe reading is "we do not know", not "the venue
// refused it".
func TestANilErrorIsStillTreatedAsUnknown(t *testing.T) {
	if ClassifyBrokerError(nil).OutcomeKnown {
		t.Fatal("a nil error was classified as a definitive venue refusal")
	}
}

// TestEveryDefinitiveErrorInTheAdapterPackageIsClassified.
//
// The adapter package defines the vocabulary; this asserts the OMS speaks all
// of it. A sentinel added to broker/adapter.go and not handled here would
// silently become an unknown outcome — which is SAFE, but halts an account for
// what may be an ordinary refusal, so it should be a deliberate choice.
func TestEveryDefinitiveErrorInTheAdapterPackageIsClassified(t *testing.T) {
	definitive := []error{
		broker.ErrOrderRejected,
		broker.ErrInsufficientMargin,
		broker.ErrMarketClosed,
		broker.ErrVenueUnavailable,
		broker.ErrRateLimited,
	}
	indefinite := []error{
		broker.ErrUnknownOutcome,
		// ErrNotFound and ErrUnsupported are not outcomes of a placement at
		// all. Reaching this function with either would mean something is
		// wrong upstream, so unknown is the correct reading.
		broker.ErrNotFound,
		broker.ErrUnsupported,
	}

	for _, err := range definitive {
		if !ClassifyBrokerError(err).OutcomeKnown {
			t.Errorf("%v is definitive but classified as unknown", err)
		}
	}
	for _, err := range indefinite {
		if ClassifyBrokerError(err).OutcomeKnown {
			t.Errorf("%v is not a definitive placement outcome but was classified as one", err)
		}
	}
}

// TestClassificationIsSideEffectFree.
//
// Called from a path that then writes to the database. If classification
// depended on anything mutable, the same error could be classified two ways on
// two calls and the order's recorded outcome would depend on timing.
func TestClassificationIsSideEffectFree(t *testing.T) {
	err := broker.RejectionError{Reason: "no liquidity", Code: "10018"}
	first := ClassifyBrokerError(err)
	for i := 0; i < 100; i++ {
		again := ClassifyBrokerError(err)
		if again.OutcomeKnown != first.OutcomeKnown ||
			again.Rejection.Code != first.Rejection.Code {
			t.Fatalf("call %d classified the same error differently", i)
		}
	}
}
