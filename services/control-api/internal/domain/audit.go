package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AuditAction enumerates the events Vantage records. The list is closed: a new
// kind of security- or money-relevant action requires a constant here, which
// makes "is this audited?" answerable by reading one file.
type AuditAction string

const (
	AuditLoginSucceeded     AuditAction = "auth.login.succeeded"
	AuditLoginFailed        AuditAction = "auth.login.failed"
	AuditLogout             AuditAction = "auth.logout"
	AuditSessionRevoked     AuditAction = "auth.session.revoked"
	AuditMFAEnrolled        AuditAction = "auth.mfa.enrolled"
	AuditMFADisabled        AuditAction = "auth.mfa.disabled"
	AuditMFAChallengeFailed AuditAction = "auth.mfa.challenge_failed"
	AuditPasswordChanged    AuditAction = "auth.password.changed"
	AuditRoleChanged        AuditAction = "user.role.changed"
	AuditUserDisabled       AuditAction = "user.disabled"

	AuditBrokerConnectionCreated AuditAction = "broker.connection.created"
	AuditBrokerConnectionRemoved AuditAction = "broker.connection.removed"

	AuditAuthorityCreated AuditAction = "authority.created"
	AuditAuthorityUpdated AuditAction = "authority.updated"
	AuditAuthorityRevoked AuditAction = "authority.revoked"

	AuditRiskLimitsUpdated AuditAction = "risk.limits.updated"
	AuditRiskRejection     AuditAction = "risk.rejection"

	AuditKillSwitchActivated   AuditAction = "killswitch.activated"
	AuditKillSwitchDeactivated AuditAction = "killswitch.deactivated"
	AuditBlockedTradeAttempt   AuditAction = "killswitch.blocked_trade_attempt"
	AuditFlattenRequested      AuditAction = "risk.flatten.requested"
	AuditFlattenExecuted       AuditAction = "risk.flatten.executed"

	AuditStrategyPromoted AuditAction = "strategy.promoted"
	AuditStrategyRetired  AuditAction = "strategy.retired"
	AuditStrategyEnabled  AuditAction = "strategy.enabled"
	AuditStrategyDisabled AuditAction = "strategy.disabled"
	AuditModelPromoted    AuditAction = "model.promoted"
	AuditModelRetired     AuditAction = "model.retired"

	AuditOrderIntent              AuditAction = "order.intent"
	AuditOrderSubmitted           AuditAction = "order.submitted"
	AuditOrderRejected            AuditAction = "order.rejected"
	AuditOrderCancelled           AuditAction = "order.cancelled"
	AuditOrderFilled              AuditAction = "order.filled"
	AuditOrderDuplicateSuppressed AuditAction = "order.duplicate_suppressed"

	AuditReconciliationRun AuditAction = "reconciliation.run"
	// AuditReconciliationResolve records an operator resolving a divergence.
	// Written on refusal as well as success: an audit trail that records only
	// successes cannot answer "did anyone try".
	AuditReconciliationResolve     AuditAction = "reconciliation.resolve"
	AuditReconciliationDiscrepancy AuditAction = "reconciliation.discrepancy"

	// Autopilot is its own pair of actions rather than a generic admin
	// action, because "when was autonomous trading on?" has to be answerable
	// from the audit log by filtering, not by reading every admin event.
	// AuditReplayControl records starting or stopping a market replay.
	//
	// Its own action because a replay puts the whole process on dataset time,
	// which is the sort of thing an investigation into an odd timestamp needs
	// to be able to find by filtering rather than by reading everything.
	AuditReplayControl AuditAction = "replay.control"

	AuditAutopilotEnabled  AuditAction = "autopilot.enabled"
	AuditAutopilotDisabled AuditAction = "autopilot.disabled"

	AuditAdminAction   AuditAction = "admin.action"
	AuditSecurityEvent AuditAction = "security.event"
)

// AuditResult is the outcome of the audited action.
type AuditResult string

const (
	AuditSuccess AuditResult = "success"
	AuditFailure AuditResult = "failure"
	AuditBlocked AuditResult = "blocked"
)

// AuditEvent is an append-only record.
//
// Events are hash-chained: each row stores the hash of its own canonical
// content combined with the previous row's hash. This provides TAMPER
// EVIDENCE, not immutability. Anyone with write access to the database can
// still rewrite history — but they must rewrite every subsequent row to keep
// the chain consistent, and a verifier that has recorded an earlier head hash
// (or shipped it off-box) will detect the divergence. See docs/SECURITY.md.
type AuditEvent struct {
	ID            uuid.UUID
	Sequence      int64
	OccurredAt    time.Time
	ActorUserID   *uuid.UUID
	ActorType     string // user | system | strategy | admin
	Action        AuditAction
	TargetType    string
	TargetID      *string
	AccountID     *uuid.UUID
	Result        AuditResult
	RequestID     string
	CorrelationID string
	IPAddress     *string
	UserAgent     *string
	Metadata      json.RawMessage
	PrevHash      string
	Hash          string
}

// CanonicalPayload renders the fields covered by the hash. Field order is
// fixed and explicit so the hash is reproducible across versions and languages.
func (e AuditEvent) CanonicalPayload() string {
	actor := ""
	if e.ActorUserID != nil {
		actor = e.ActorUserID.String()
	}
	target := ""
	if e.TargetID != nil {
		target = *e.TargetID
	}
	account := ""
	if e.AccountID != nil {
		account = e.AccountID.String()
	}
	return fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
		e.Sequence,
		e.OccurredAt.UTC().Format(time.RFC3339Nano),
		actor,
		e.ActorType,
		e.Action,
		e.TargetType,
		target,
		account,
		e.Result,
		e.RequestID,
		e.CorrelationID,
		string(e.Metadata),
	)
}

// ComputeHash returns the chained hash for this event given its predecessor.
func (e AuditEvent) ComputeHash(prevHash string) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write([]byte("\n"))
	h.Write([]byte(e.CanonicalPayload()))
	return hex.EncodeToString(h.Sum(nil))
}

// GenesisHash is the chain's starting value.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// VerifyChain recomputes hashes over an ordered slice and reports the first
// index whose stored hash disagrees with its recomputed value.
func VerifyChain(events []AuditEvent) (ok bool, brokenAt int) {
	prev := GenesisHash
	for i, e := range events {
		if e.PrevHash != prev {
			return false, i
		}
		if e.ComputeHash(prev) != e.Hash {
			return false, i
		}
		prev = e.Hash
	}
	return true, -1
}
