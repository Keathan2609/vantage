package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// TradingAuthority is the explicit, scoped, revocable permission under which
// Vantage may act on an account.
//
// It is a SECURITY AND CONTROL MECHANISM. It is not a legal instrument and
// confers no regulatory permission of any kind: the existence of a signed,
// audited mandate in this table says nothing about whether the activity it
// describes is lawful for the operator to perform. See
// docs/REGULATORY_BOUNDARY.md.
//
// Authority is deliberately narrow. It enumerates what MAY happen rather than
// what may not, so a gap in the enumeration fails closed: an instrument absent
// from AllowedInstruments cannot be traded, and an empty list permits nothing.
type TradingAuthority struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	AccountID uuid.UUID
	Mode      ExecutionMode

	Active            bool
	AutomationEnabled bool

	AllowedInstruments []string
	AllowedStrategyIDs []uuid.UUID
	AllowedOrderTypes  []OrderType

	MaxOrderQuantity    decimal.Decimal
	MaxOrderNotional    money.Amount
	MaxPositionExposure money.Amount
	MaxLeverage         decimal.Decimal
	MaxDailyLoss        money.Amount

	ValidFrom  time.Time
	ValidUntil *time.Time

	CreatedBy        uuid.UUID
	CreatedAt        time.Time
	UpdatedAt        time.Time
	RevokedAt        *time.Time
	RevocationReason *string
	Version          int64
}

// Effective reports whether the authority may be relied on at instant t, with
// a structured reason when it may not.
func (a TradingAuthority) Effective(now time.Time) (bool, Rejection) {
	if a.RevokedAt != nil {
		return false, NewRejection(RejectAuthorityRevoked,
			"Trading authority for this account has been revoked.",
			"revoked_at", a.RevokedAt.UTC().Format(time.RFC3339))
	}
	if !a.Active {
		return false, NewRejection(RejectNoAuthority,
			"Trading authority for this account is not active.")
	}
	if now.Before(a.ValidFrom) {
		return false, NewRejection(RejectNoAuthority,
			"Trading authority is not yet valid.",
			"valid_from", a.ValidFrom.UTC().Format(time.RFC3339))
	}
	if a.ValidUntil != nil && !now.Before(*a.ValidUntil) {
		return false, NewRejection(RejectAuthorityExpired,
			"Trading authority has expired.",
			"valid_until", a.ValidUntil.UTC().Format(time.RFC3339))
	}
	return true, Rejection{}
}

// PermitsInstrument reports whether the instrument is in scope.
func (a TradingAuthority) PermitsInstrument(instrumentID string) bool {
	for _, v := range a.AllowedInstruments {
		if v == instrumentID {
			return true
		}
	}
	return false
}

// PermitsStrategy reports whether a strategy may act under this authority.
// A nil strategy ID denotes a manual order, which requires no strategy grant.
func (a TradingAuthority) PermitsStrategy(id *uuid.UUID) bool {
	if id == nil {
		return true
	}
	for _, v := range a.AllowedStrategyIDs {
		if v == *id {
			return true
		}
	}
	return false
}

// PermitsOrderType reports whether the order type is in scope.
func (a TradingAuthority) PermitsOrderType(t OrderType) bool {
	for _, v := range a.AllowedOrderTypes {
		if v == t {
			return true
		}
	}
	return false
}

// KillSwitchScope identifies what a kill switch halts.
type KillSwitchScope string

const (
	KillScopeGlobal   KillSwitchScope = "global"
	KillScopeUser     KillSwitchScope = "user"
	KillScopeAccount  KillSwitchScope = "account"
	KillScopeBroker   KillSwitchScope = "broker"
	KillScopeStrategy KillSwitchScope = "strategy"
)

// Valid reports whether the scope is recognised.
func (s KillSwitchScope) Valid() bool {
	switch s {
	case KillScopeGlobal, KillScopeUser, KillScopeAccount, KillScopeBroker, KillScopeStrategy:
		return true
	}
	return false
}

// KillSwitch halts NEW order creation within its scope.
//
// A kill switch does NOT liquidate anything. "Stop opening new positions" and
// "close what is open" are different operations with different risk: an
// automatic liquidation triggered by, say, a stale-data alarm would dump
// positions into exactly the illiquid conditions that raised the alarm.
// Closing positions is the separate, explicitly authorised Flatten operation.
type KillSwitch struct {
	ID            uuid.UUID
	Scope         KillSwitchScope
	TargetID      *string // user, account, broker name or strategy id; nil for global
	Active        bool
	Reason        string
	ActivatedBy   uuid.UUID
	ActivatedAt   time.Time
	DeactivatedBy *uuid.UUID
	DeactivatedAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// KillSwitchState is the resolved set of active switches relevant to one order.
type KillSwitchState struct {
	Active []KillSwitch
}

// Blocked reports whether any switch in the set halts trading, with the
// most specific applicable switch reported first.
func (s KillSwitchState) Blocked() (bool, *KillSwitch) {
	if len(s.Active) == 0 {
		return false, nil
	}
	// Global switches are reported preferentially: if everything is halted,
	// that is the fact the operator most needs to see.
	for i := range s.Active {
		if s.Active[i].Scope == KillScopeGlobal {
			return true, &s.Active[i]
		}
	}
	return true, &s.Active[0]
}
