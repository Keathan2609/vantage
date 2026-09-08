package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// Role is a coarse permission tier. Fine-grained control over what may trade
// lives in TradingAuthority; roles govern what a user may operate at all.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleTrader Role = "trader"
	RoleViewer Role = "viewer"
)

// Valid reports whether the role is recognised.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleTrader, RoleViewer:
		return true
	}
	return false
}

// CanTrade reports whether the role may submit orders at all. Admin is
// deliberately excluded: administration and trading are separate duties, and an
// admin session compromised through the admin surface should not be able to
// move money. An operator who needs to trade uses a trader account.
func (r Role) CanTrade() bool { return r == RoleTrader }

// CanAdminister reports whether the role may perform security operations.
func (r Role) CanAdminister() bool { return r == RoleAdmin }

// User is an authenticated principal.
type User struct {
	ID                uuid.UUID
	Email             string
	DisplayName       string
	Role              Role
	PasswordHash      string
	MFAEnabled        bool
	MFASecretCipher   []byte
	MFAKeyVersion     int
	Disabled          bool
	FailedLoginCount  int
	LockedUntil       *time.Time
	LastLoginAt       *time.Time
	PasswordChangedAt time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Locked reports whether login is currently blocked by lockout backoff.
func (u User) Locked(now time.Time) bool {
	return u.LockedUntil != nil && now.Before(*u.LockedUntil)
}

// Account is a trading account. In the non-custodial model an account maps to a
// broker account the USER owns; Vantage holds no funds. In paper mode the
// balances are simulated and are labelled as such everywhere they surface.
type Account struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	Name          string
	Mode          ExecutionMode
	Currency      money.Currency
	BrokerName    string
	BrokerAcctRef *string
	Enabled       bool
	// TradingEnabled is a per-account switch distinct from Enabled: an account
	// can be readable and reconcilable while all new trading is suspended.
	TradingEnabled bool
	Leverage       decimal.Decimal
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Version        int64
}

// AccountSnapshot is the derived financial state of an account at an instant.
// It is computed from the ledger and open positions rather than stored as a
// mutable running total, so it can always be recomputed and audited.
type AccountSnapshot struct {
	AccountID      uuid.UUID
	Currency       money.Currency
	Balance        money.Amount
	Equity         money.Amount
	MarginUsed     money.Amount
	FreeMargin     money.Amount
	RealizedPnL    money.Amount
	UnrealizedPnL  money.Amount
	Fees           money.Amount
	Commission     money.Amount
	Swap           money.Amount
	GrossExposure  money.Amount
	NetExposure    money.Amount
	OpenPositions  int
	PendingOrders  int
	PeakEquity     money.Amount
	DayStartEquity money.Amount
	AsOf           time.Time
	// Simulated is true whenever Mode is not live. It exists so no UI or export
	// can present paper numbers as real ones by omission.
	Simulated bool
}

// MarginLevel returns equity/marginUsed as a percentage, or zero when no
// margin is in use.
func (s AccountSnapshot) MarginLevel() decimal.Decimal {
	if s.MarginUsed.IsZero() {
		return decimal.Zero
	}
	return s.Equity.Decimal().Div(s.MarginUsed.Decimal()).Mul(decimal.NewFromInt(100))
}

// DrawdownFraction returns the fractional decline from peak equity.
func (s AccountSnapshot) DrawdownFraction() decimal.Decimal {
	peak := s.PeakEquity.Decimal()
	if peak.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	dd := peak.Sub(s.Equity.Decimal()).Div(peak)
	if dd.IsNegative() {
		return decimal.Zero
	}
	return dd
}

// DayPnL returns equity change since the start of the trading day.
func (s AccountSnapshot) DayPnL() money.Amount {
	v, err := s.Equity.Sub(s.DayStartEquity)
	if err != nil {
		return money.Zero(s.Currency)
	}
	return v
}

// TransactionType classifies a ledger entry.
type TransactionType string

const (
	TxDeposit     TransactionType = "deposit"
	TxWithdrawal  TransactionType = "withdrawal"
	TxRealizedPnL TransactionType = "realized_pnl"
	TxCommission  TransactionType = "commission"
	TxFee         TransactionType = "fee"
	TxSwap        TransactionType = "swap"
	TxAdjustment  TransactionType = "adjustment"
)

// Transaction is an immutable ledger entry. Balance is never updated in place;
// it is the sum of transactions, which makes every balance explainable and any
// tampering visible as an entry someone has to account for.
type Transaction struct {
	ID           uuid.UUID
	AccountID    uuid.UUID
	Type         TransactionType
	Amount       money.Amount
	BalanceAfter money.Amount
	OrderID      *uuid.UUID
	FillID       *uuid.UUID
	PositionID   *uuid.UUID
	Description  string
	Mode         ExecutionMode
	CreatedAt    time.Time
	Sequence     int64
}
