package orchestrator

import (
	"testing"

	"github.com/shopspring/decimal"
)

// A frozen policy is only frozen if something notices when it thaws.

func TestConsensusPolicyV1IsFrozenAtTheNumbersItActuallyRan(t *testing.T) {
	// Transcribed rather than derived from DefaultConsensusPolicy(), which is
	// the whole point: if the default moves, this must NOT move with it.
	// Every decision recorded under policy_version 1 was taken against these,
	// and changing them silently rewrites what those decisions meant.
	p, ok := PolicyByID(ConsensusPolicyV1)
	if !ok {
		t.Fatal("consensus-policy/v1 is not registered, so no historical decision " +
			"can say which rules produced it")
	}

	for _, c := range []struct {
		name string
		got  decimal.Decimal
		want string
	}{
		{"MinConfidence", p.Policy.MinConfidence, "0.55"},
		{"MinNetConfidence", p.Policy.MinNetConfidence, "0.60"},
		{"ModelVetoConfidence", p.Policy.ModelVetoConfidence, "0.65"},
		{"MaxOpposingWeightFraction", p.Policy.MaxOpposingWeightFraction, "0.20"},
	} {
		if !c.got.Equal(decimal.RequireFromString(c.want)) {
			t.Errorf("v1 %s is %s, and it ran as %s.\n"+
				"A frozen policy may not be edited. If the platform should decide "+
				"differently, register a NEW version and point ActivePolicyID at "+
				"it, because every decision that recorded policy_version 1 was taken "+
				"against these numbers and must stay interpretable.",
				c.name, c.got, c.want)
		}
	}

	if !p.Frozen {
		t.Error("v1 is not marked frozen, yet it has decided every autonomous " +
			"verdict this platform has taken")
	}
	if p.ScoreKindCompared != "raw_score" {
		t.Errorf("v1 records that it compared %q. It compared an uncalibrated raw "+
			"score, and saying otherwise would claim a calibration that was "+
			"never fitted.", p.ScoreKindCompared)
	}
	if p.Provenance == "" {
		t.Error("v1 has no provenance, so a reader cannot tell a threshold chosen " +
			"as a posture from one fitted against outcomes")
	}
}

func TestTheActivePolicyIsRegistered(t *testing.T) {
	// ActivePolicy panics on an unregistered id, which is right at start-up
	// and unhelpful as a test failure. This gives the same guarantee with a
	// readable message.
	if _, ok := PolicyByID(ActivePolicyID); !ok {
		t.Fatalf("the active policy %q is not in the register; the platform would "+
			"be deciding under rules it cannot name", ActivePolicyID)
	}
	if got := ActivePolicy().ID; got != ActivePolicyID {
		t.Errorf("ActivePolicy() returned %q for active id %q", got, ActivePolicyID)
	}
}

func TestThePolicyVersionConstantMatchesTheActivePolicy(t *testing.T) {
	// `PolicyVersion` is what a Verdict stamps onto every decision, and the
	// register is what a reader looks that number up in. If they disagree, a
	// decision points at the wrong rules.
	if ActivePolicy().Version != PolicyVersion {
		t.Fatalf("decisions are stamped policy_version %d and the active policy is "+
			"version %d. Every decision would point a reader at rules it was not "+
			"taken under.", PolicyVersion, ActivePolicy().Version)
	}
}

func TestDefaultConsensusPolicyStillMatchesTheActiveFrozenPolicy(t *testing.T) {
	// The bridge between the old shape and the new one.
	//
	// `DefaultConsensusPolicy()` is what the production path still calls. While
	// the active policy is v1, the two must agree -- otherwise the platform
	// decides under one set of numbers and records another. When a successor is
	// selected, this test is what forces the call site to move with it rather
	// than being left pointing at the default.
	active := ActivePolicy().Policy
	def := DefaultConsensusPolicy()

	for _, c := range []struct {
		name       string
		got, want  decimal.Decimal
		whichField string
	}{
		{"MinConfidence", def.MinConfidence, active.MinConfidence, "MinConfidence"},
		{"MinNetConfidence", def.MinNetConfidence, active.MinNetConfidence, "MinNetConfidence"},
		{"ModelVetoConfidence", def.ModelVetoConfidence, active.ModelVetoConfidence, "ModelVetoConfidence"},
		{"MaxOpposingWeightFraction", def.MaxOpposingWeightFraction, active.MaxOpposingWeightFraction, "MaxOpposingWeightFraction"},
	} {
		if !c.got.Equal(c.want) {
			t.Errorf("DefaultConsensusPolicy().%s is %s and the active frozen "+
				"policy says %s. The platform would decide under one set of "+
				"numbers and stamp decisions with another.", c.name, c.got, c.want)
		}
	}
}

func TestEveryRegisteredPolicyHasADistinctVersionAndID(t *testing.T) {
	ids := map[PolicyID]bool{}
	versions := map[int]bool{}
	for _, p := range RegisteredPolicies() {
		if ids[p.ID] {
			t.Errorf("policy id %q is registered twice", p.ID)
		}
		if versions[p.Version] {
			t.Errorf("policy version %d is registered twice; a decision stamped "+
				"with it could not say which rules it meant", p.Version)
		}
		if p.ID == "" || p.Version < 1 {
			t.Errorf("policy %+v has no usable identity", p)
		}
		ids[p.ID], versions[p.Version] = true, true
	}
	if len(ids) == 0 {
		t.Fatal("no policy is registered")
	}
}
