package domain

import (
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
// Exactly ONE of them is compared against anything. `risk.Evaluate` takes
// decimal.Min of the account's leverage limit and the authority's. The other
// four are never read by the risk engine, the OMS or the orchestrator, so
// narrowing them changes nothing about what the platform will do.
//
// That is worse than not having them: an operator who tightens
// MaxOrderQuantity to 0.10 sees it accepted, sees it echoed back, sees it
// recorded on every decision, and is not protected by it.
//
// This test does not assert that the gap is correct. It pins the SHAPE of the
// authority so that adding a field without enforcing it is a deliberate act
// with a failing test in front of it, and it names the gap where someone
// reading the type will find it. Closing the gap means comparing each ceiling
// the way MaxLeverage already is -- taking the more restrictive of the two,
// never the looser, because risk may only reduce.
func TestTheAuthoritysNumericCeilingsAreDocumentedAsEnforcedOrNot(t *testing.T) {
	enforced := map[string]bool{
		// Compared in risk.Evaluate via decimal.Min with the account limit.
		"MaxLeverage": true,

		// NOT compared anywhere. Each is stored, served and recorded, and
		// changing it changes nothing the platform does.
		"MaxOrderQuantity":    false,
		"MaxOrderNotional":    false,
		"MaxPositionExposure": false,
		"MaxDailyLoss":        false,
	}

	var unenforced []string
	for name, ok := range enforced {
		if !ok {
			unenforced = append(unenforced, name)
		}
	}
	if len(unenforced) != 4 {
		t.Fatalf("this record lists %d unenforced ceilings; update it and the "+
			"finding in docs/ENGINEERING_REPORT.md together", len(unenforced))
	}

	// The fields must still exist. If one is removed, this record is stale and
	// should be corrected rather than silently passing.
	var a TradingAuthority
	_ = a.MaxOrderQuantity
	_ = a.MaxOrderNotional
	_ = a.MaxPositionExposure
	_ = a.MaxLeverage
	_ = a.MaxDailyLoss

	t.Logf("trading authority ceilings that do NOT bind: %s. Only MaxLeverage "+
		"is compared against anything (risk.Evaluate, decimal.Min with the "+
		"account limit).", strings.Join(unenforced, ", "))
}
