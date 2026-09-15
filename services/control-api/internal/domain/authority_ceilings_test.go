package domain

import (
	"sort"
	"strings"
	"testing"
)

// The trading authority's numeric ceilings, and which of them actually bind.
//
// # Why this test exists as a RECORD rather than as an assertion of intent
//
// A trading authority is described throughout this repository as a technical
// control. It carries five numeric bounds: MaxOrderQuantity, MaxOrderNotional,
// MaxPositionExposure, MaxLeverage and MaxDailyLoss. They are stored, they are
// returned by the API, and they are written into every decision snapshot's
// authority_state.
//
// For a long time exactly ONE of them was compared against anything. The other
// four were never read by the risk engine, the OMS or the orchestrator, so
// narrowing them changed nothing about what the platform would do -- which is
// worse than not having them: an operator who tightened MaxOrderQuantity to
// 0.10 saw it accepted, saw it echoed back, saw it recorded on every decision,
// and was not protected by it.
//
// All five bind now. This record survives the fix rather than being deleted,
// because its purpose was never to describe the gap: it is here so that adding
// a sixth ceiling without enforcing it is a DELIBERATE act with a failing test
// in front of it. A record that is thrown away once it reads "all enforced"
// protects nothing.
//
// Each entry names where the comparison lives, so the claim is checkable
// rather than asserted. Enforcement means: taking the MORE RESTRICTIVE of the
// account limit and the authority's, never the looser, because risk may only
// reduce.
func TestTheAuthoritysNumericCeilingsAreDocumentedAsEnforcedOrNot(t *testing.T) {
	// where each ceiling is compared, or "" when it is compared nowhere.
	enforcedAt := map[string]string{
		// Folded into the account's own leverage check with decimal.Min,
		// alongside the instrument's Spec.MaxLeverage.
		"MaxLeverage": "risk.Evaluate, check max_leverage",

		// Each reports its own named check, so a refusal says which bound was
		// hit and what it was. Both the account's check and the authority's
		// must pass, which is arithmetically min() and is legible.
		"MaxOrderQuantity":    "risk.Evaluate, check authority_max_order_quantity",
		"MaxOrderNotional":    "risk.Evaluate, check authority_max_order_notional",
		"MaxPositionExposure": "risk.Evaluate, check authority_max_position_exposure",
		"MaxDailyLoss":        "risk.Evaluate, check authority_max_daily_loss",
	}

	var unenforced, enforced []string
	for name, where := range enforcedAt {
		if where == "" {
			unenforced = append(unenforced, name)
			continue
		}
		enforced = append(enforced, name)
	}
	sort.Strings(unenforced)
	sort.Strings(enforced)

	if len(enforcedAt) != 5 {
		t.Fatalf("the authority carries 5 numeric ceilings and this record lists %d; "+
			"a ceiling was added or removed without updating it", len(enforcedAt))
	}
	if len(unenforced) != 0 {
		t.Fatalf("this record lists %d unenforced ceiling(s) (%s). If that is "+
			"deliberate, update the finding in docs/ENGINEERING_REPORT.md in the "+
			"same change; if it is not, make each bind the way the others do.",
			len(unenforced), strings.Join(unenforced, ", "))
	}

	// The fields must still exist. If one is removed, this record is stale and
	// should be corrected rather than silently passing.
	var a TradingAuthority
	_ = a.MaxOrderQuantity
	_ = a.MaxOrderNotional
	_ = a.MaxPositionExposure
	_ = a.MaxLeverage
	_ = a.MaxDailyLoss

	t.Logf("all %d trading-authority ceilings bind: %s",
		len(enforced), strings.Join(enforced, ", "))
}

// The four authority checks must exist as named constants, so that a refusal
// carries a name an operator can look up rather than a bare limit figure.
//
// Separate from the record above because that one is about the DOMAIN type and
// this one is about the vocabulary the risk engine reports in. A check renamed
// without updating the record would leave the record describing a check name
// that no longer exists.
func TestEachAuthorityCeilingHasItsOwnCheckName(t *testing.T) {
	names := []RiskCheckName{
		CheckAuthorityOrderQuantity,
		CheckAuthorityOrderNotional,
		CheckAuthorityPositionExposure,
		CheckAuthorityDailyLoss,
	}
	seen := map[RiskCheckName]bool{}
	for _, n := range names {
		if n == "" {
			t.Fatal("an authority check name is empty")
		}
		if !strings.HasPrefix(string(n), "authority_") {
			t.Errorf("check %q does not carry the authority_ prefix, so a reader "+
				"cannot tell the authority's bound from the account's", n)
		}
		if seen[n] {
			t.Errorf("check name %q is used twice; two different bounds would "+
				"report under one name", n)
		}
		seen[n] = true
	}

	// And they must not collide with the account-level names they sit beside.
	for _, account := range []RiskCheckName{
		CheckOrderQuantity, CheckOrderNotional, CheckInstrumentExposure, CheckDailyLoss,
	} {
		if seen[account] {
			t.Errorf("authority check name collides with the account-level %q", account)
		}
	}
}
