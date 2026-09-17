package orchestrator

import (
	"fmt"
	"sort"

	"github.com/shopspring/decimal"
)

// The consensus policy register: every version that has ever decided anything.
//
// # Why a register and not just a default
//
// `DefaultConsensusPolicy()` returns whatever today's thresholds are. That is
// useless for reading history: a decision taken six months ago recorded
// `policy_version: 1`, and if the constants behind version 1 have since moved,
// the recorded verdict can no longer be recomputed or argued with. "Why did it
// not trade?" becomes unanswerable precisely when it matters.
//
// So each version is frozen here, by value, and a version that has decided
// anything is never edited. A new threshold set is a NEW version. The register
// is what makes that rule enforceable rather than aspirational -- a test walks
// it and fails if v1's numbers change.
//
// # v1 is frozen as it actually ran
//
// Its thresholds were never fitted against outcomes. They are recorded here as
// the historical fact they are, NOT as a recommendation: v1 compares an
// uncalibrated raw score against numbers that read as probabilities, which is
// the category error this milestone exists to resolve. Freezing it is how the
// decisions it took stay interpretable while a successor is derived from
// evidence.

// PolicyID names one frozen threshold set.
type PolicyID string

// ConsensusPolicyV1 is the policy every decision recorded before this register
// existed was taken under.
//
// DO NOT EDIT THESE NUMBERS. Changing them silently rewrites the meaning of
// every historical decision that recorded policy_version 1.
const ConsensusPolicyV1 PolicyID = "consensus-policy/v1"

// RegisteredPolicy is a frozen policy plus the account of where it came from.
type RegisteredPolicy struct {
	ID      PolicyID
	Version int
	Policy  ConsensusPolicy
	// Provenance says how the thresholds were arrived at. "Chosen as a
	// starting posture" and "fitted on validation data" are very different
	// claims, and a reader comparing two runs needs to know which they have.
	Provenance string
	// Frozen marks a policy that has decided something and may never change.
	Frozen bool
	// ScoreKindCompared is what the thresholds were applied to. v1 compared a
	// raw score; a successor fitted against outcomes would compare a
	// calibrated probability, and the two are not interchangeable even at
	// identical numeric thresholds.
	ScoreKindCompared string
}

// policyRegister holds every version by id.
var policyRegister = map[PolicyID]RegisteredPolicy{
	ConsensusPolicyV1: {
		ID:      ConsensusPolicyV1,
		Version: 1,
		Policy: ConsensusPolicy{
			MinConfidence:             decimal.RequireFromString("0.55"),
			MinNetConfidence:          decimal.RequireFromString("0.60"),
			ModelVetoConfidence:       decimal.RequireFromString("0.65"),
			MaxOpposingWeightFraction: decimal.RequireFromString("0.20"),
		},
		Provenance: "Chosen as a starting posture for a platform that had never " +
			"traded unattended, so that the common outcome is NO TRADE. Never " +
			"fitted against realised outcomes, and applied to an UNCALIBRATED " +
			"raw score whose scale differs between strategies -- four of the " +
			"twelve are capped below 0.80 by a constant inside the formula, so " +
			"a 0.55 floor means something different for each of them.",
		Frozen:            true,
		ScoreKindCompared: "raw_score",
	},
}

// PolicyByID returns a frozen policy.
func PolicyByID(id PolicyID) (RegisteredPolicy, bool) {
	p, ok := policyRegister[id]
	return p, ok
}

// RegisteredPolicies lists every version, oldest first.
func RegisteredPolicies() []RegisteredPolicy {
	out := make([]RegisteredPolicy, 0, len(policyRegister))
	for _, p := range policyRegister {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// ActivePolicyID is the version in force.
//
// A constant rather than configuration, deliberately: which policy decides is
// not an operational dial. Moving it is a code change that shows up in a diff,
// in a review and in the run's own record, which is the minimum for something
// that changes what the platform trades.
const ActivePolicyID = ConsensusPolicyV1

// ActivePolicy returns the policy in force, and panics if it is not registered.
//
// A panic at start-up rather than a silent fallback: a platform that cannot
// name the policy it is deciding under should not be deciding. The only way to
// reach it is to point ActivePolicyID at an id that was never registered,
// which a test also catches.
func ActivePolicy() RegisteredPolicy {
	p, ok := PolicyByID(ActivePolicyID)
	if !ok {
		panic(fmt.Sprintf("orchestrator: the active consensus policy %q is not "+
			"registered; a decision cannot record which rules produced it",
			ActivePolicyID))
	}
	return p
}
