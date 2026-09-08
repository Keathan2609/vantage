package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/store"
)

// The trading verdict: whether automation may run, and why not.
//
// # Derived, never stored
//
// The verdict is computed from open issues and the account's last successful
// run every time it is asked for. It would be cheaper to store it and update
// it on change, and that is exactly the design to avoid: a stored verdict can
// disagree with the issues it is supposed to summarise, and the failure mode
// is an account that trades automatically because a flag was not cleared.
//
// # Two ways to be unsafe
//
// A halted account has a known problem. An unreconciled account has an unknown
// one — nothing is wrong, but nothing is known to be right either. Both stop
// automation, and the distinction is reported because the operator response
// differs: one needs investigating, the other needs a run.

// AccountVerdict is the reconciliation view of one account.
type AccountVerdict struct {
	AccountID  uuid.UUID
	BrokerName string
	State      domain.TradingState
	// Reason is operator-facing and names the specific cause.
	Reason    string
	HaltScope domain.HaltScope

	OpenIssues      int
	CriticalIssues  int
	OperatorAction  int
	UncertainOrders int

	LastSuccessAt       *time.Time
	LastStatus          *string
	ConsecutiveFailures int
}

// AutomationAllowed reports whether automated trading may run.
func (v AccountVerdict) AutomationAllowed() bool { return v.State.AutomationAllowed() }

// staleAfter is how long a successful reconciliation stays trustworthy.
//
// Beyond this the account is DEGRADED rather than halted: nothing is known to
// be wrong, but the evidence is old. Chosen well above the scheduled interval
// so an ordinary missed run does not degrade an account, and well below a
// trading session so a genuinely stalled scheduler is noticed.
const staleAfter = 30 * time.Minute

// Verdict computes an account's trading state.
func (s *Service) Verdict(ctx context.Context, account domain.Account) (AccountVerdict, error) {
	v := AccountVerdict{AccountID: account.ID, BrokerName: account.BrokerName}

	open, err := s.store.Reconcile.OpenIssues(ctx, account.ID)
	if err != nil {
		return v, err
	}
	state, err := s.store.Reconcile.AccountStateFor(ctx, account.ID)
	if err != nil {
		return v, err
	}
	uncertain, err := s.store.Trading.OrdersRequiringReconciliation(ctx, account.ID)
	if err != nil {
		return v, err
	}

	v.LastSuccessAt = state.LastSuccessAt
	v.LastStatus = state.LastStatus
	v.ConsecutiveFailures = state.ConsecutiveFailures
	v.UncertainOrders = len(uncertain)
	v.OpenIssues = len(open)

	// Broker-connection-scoped issues are consulted across accounts: an
	// identifier mapping that does not hold invalidates every repair on that
	// connection, so an account with no issues of its own is still unsafe.
	brokerIssues, err := s.store.Reconcile.OpenIssuesForBroker(ctx, account.BrokerName)
	if err != nil {
		return v, err
	}

	v.State = domain.TradingHealthy
	v.HaltScope = domain.HaltNone
	v.Reason = "reconciled, no unresolved divergence"

	escalate := func(to domain.TradingState, scope domain.HaltScope, reason string) {
		if to.Severity() > v.State.Severity() {
			v.State = to
			v.HaltScope = scope
			v.Reason = reason
		}
	}

	if !state.Reconciled() {
		escalate(domain.TradingReconciliationRequired, domain.HaltAccount,
			"this account has never completed a reconciliation run, so agreement with "+
				"the venue is unproven")
	} else if state.LastStatus != nil && *state.LastStatus == "failed" {
		escalate(domain.TradingReconciliationRequired, domain.HaltAccount,
			fmt.Sprintf("the last %d reconciliation run(s) failed, so Vantage cannot "+
				"confirm its records match the venue", state.ConsecutiveFailures))
	} else if s.clock.Now().Sub(*state.LastSuccessAt) > staleAfter {
		escalate(domain.TradingDegraded, domain.HaltNone,
			fmt.Sprintf("the last successful reconciliation was %s ago",
				s.clock.Now().Sub(*state.LastSuccessAt).Round(time.Minute)))
	}

	for _, issue := range open {
		if issue.Status == domain.IssueOperatorActionRequired {
			v.OperatorAction++
		}
		if issue.Severity == domain.SeverityCriticalIssue {
			v.CriticalIssues++
		}
		switch domain.PolicyFor(issue.Type).Halt {
		case domain.HaltAccount:
			escalate(domain.TradingHalted, domain.HaltAccount,
				fmt.Sprintf("unresolved %s: %s", issue.Type, issue.Description))
		case domain.HaltBrokerConnection:
			escalate(domain.TradingHalted, domain.HaltBrokerConnection,
				fmt.Sprintf("unresolved %s on the %s connection: %s",
					issue.Type, account.BrokerName, issue.Description))
		case domain.HaltAll:
			escalate(domain.TradingHalted, domain.HaltAll,
				fmt.Sprintf("unresolved %s: %s", issue.Type, issue.Description))
		case domain.HaltNone:
			escalate(domain.TradingDegraded, domain.HaltNone,
				fmt.Sprintf("unresolved %s (does not halt automation): %s",
					issue.Type, issue.Description))
		}
	}

	// Another account's connection-scoped issue.
	for _, issue := range brokerIssues {
		if issue.AccountID == account.ID {
			continue
		}
		if domain.PolicyFor(issue.Type).Halt != domain.HaltBrokerConnection {
			continue
		}
		escalate(domain.TradingHalted, domain.HaltBrokerConnection,
			fmt.Sprintf("the %s connection has an unresolved %s on another account, which "+
				"invalidates identifier mapping for every account on it",
				account.BrokerName, issue.Type))
	}

	return v, nil
}

// AutomationBlocked reports whether unresolved issues should stop automated
// trading on an account.
//
// Kept as the orchestrator's entry point, so the halt decision has one caller
// contract regardless of how the verdict is computed.
func (s *Service) AutomationBlocked(ctx context.Context, accountID uuid.UUID) (bool, []store.Issue, error) {
	account, err := s.store.Accounts.AccountByIDUnscoped(ctx, accountID)
	if err != nil {
		return false, nil, err
	}
	verdict, err := s.Verdict(ctx, account)
	if err != nil {
		return false, nil, err
	}
	if verdict.AutomationAllowed() {
		return false, nil, nil
	}
	open, err := s.store.Reconcile.OpenIssues(ctx, accountID)
	if err != nil {
		return true, nil, err
	}
	return true, open, nil
}

// AutomationBlockedTx is the durable form, for use inside the transaction that
// persists an order.
//
// # Why this exists separately
//
// Checking the halt state before opening a transaction makes the halt
// timing-dependent: an order can pass the check microseconds before a
// reconciliation repair writes a critical issue, and still commit afterwards.
// The window is small and it is real, and "small" is not a property to rely on
// where the consequence is an automated order placed against a book known to
// be wrong.
//
// Reading it INSIDE the order's transaction, after taking the account row lock
// that reconciliation's repair also takes, closes the window with the
// database's own serialisation rather than with luck. Either the order commits
// before the issue exists, or it sees the issue.
func (s *Service) AutomationBlockedTx(ctx context.Context, tx pgx.Tx,
	accountID uuid.UUID) (bool, string, error) {

	open, err := s.store.Reconcile.OpenIssuesTx(ctx, tx, accountID)
	if err != nil {
		return false, "", err
	}
	for _, issue := range open {
		switch domain.PolicyFor(issue.Type).Halt {
		case domain.HaltAccount, domain.HaltBrokerConnection, domain.HaltAll:
			return true, fmt.Sprintf("unresolved %s: %s", issue.Type, issue.Description), nil
		}
	}
	return false, "", nil
}

// SystemVerdict aggregates every account, for readiness.
type SystemVerdict struct {
	State    domain.TradingState
	Accounts []AccountVerdict
	// HaltedAccounts is what an operator needs first: which accounts stopped.
	HaltedAccounts int
	OpenIssues     int
	CriticalIssues int
}

// System computes the aggregate trading state.
//
// Reported alongside process health rather than folded into it. An HTTP server
// and a database that are both alive say nothing about whether this system's
// picture of the market is trustworthy, and answering "healthy" on that basis
// is how an operator comes to believe a halted account is trading.
func (s *Service) System(ctx context.Context) (SystemVerdict, error) {
	accounts, err := s.store.Accounts.ListAllAccounts(ctx)
	if err != nil {
		return SystemVerdict{}, err
	}

	out := SystemVerdict{}
	states := make([]domain.TradingState, 0, len(accounts))
	for _, a := range accounts {
		v, err := s.Verdict(ctx, a)
		if err != nil {
			return out, err
		}
		out.Accounts = append(out.Accounts, v)
		states = append(states, v.State)
		out.OpenIssues += v.OpenIssues
		out.CriticalIssues += v.CriticalIssues
		if !v.AutomationAllowed() {
			out.HaltedAccounts++
		}
	}

	if len(accounts) == 0 {
		// No accounts is not a problem: there is nothing to be wrong about.
		out.State = domain.TradingHealthy
		return out, nil
	}
	out.State = domain.WorstTradingState(states...)
	return out, nil
}
