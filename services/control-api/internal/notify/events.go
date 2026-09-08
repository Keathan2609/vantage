package notify

// Typed helpers, one per event the platform surfaces.
//
// Callers get a named method rather than an Event literal, so the severity,
// category and cooldown key for a given condition are decided once, here,
// instead of being re-decided (and drifting) at every call site.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// FeedDegraded reports that an instrument's market data is not fit for
// automation. Implements marketdata.Alerter.
func (a *Alerter) FeedDegraded(ctx context.Context, instrumentID, symbol, state string, issues []string, ageSeconds float64) {
	severity := SeverityWarning
	if state == "invalid" || state == "no_data" {
		severity = SeverityCritical
	}
	a.Raise(ctx, Event{
		Kind:     KindMarketDataStale,
		Severity: severity,
		Category: CategoryData,
		Key:      instrumentID,
		Title:    fmt.Sprintf("Market data for %s is %s", symbol, state),
		Body: fmt.Sprintf(
			"Automated orders on %s are refused while the feed is %s. The last quote is %.1fs old. "+
				"Issues: %v. Check the provider before restarting anything: the refusal is the platform working.",
			symbol, state, ageSeconds, issues),
		Fields: map[string]any{
			"instrument_id": instrumentID,
			"symbol":        symbol,
			"state":         state,
			"quote_age_s":   ageSeconds,
			"issues":        issues,
		},
	})
}

// FeedRecovered reports that an instrument's feed is tradable again.
// Implements marketdata.Alerter.
func (a *Alerter) FeedRecovered(ctx context.Context, instrumentID, symbol string) {
	a.Resolve(ctx, KindMarketDataStale, instrumentID,
		fmt.Sprintf("Market data for %s recovered", symbol),
		fmt.Sprintf("%s is fit for automation again.", symbol), nil)
}

// BrokerFailure reports that a venue call failed.
//
// `unknownOutcome` separates the two cases that matter: a definitive refusal
// means nothing happened, while an unknown outcome means an order may exist at
// the venue and reconciliation is required.
func (a *Alerter) BrokerFailure(ctx context.Context, brokerName, operation string, unknownOutcome bool, cause string) {
	severity := SeverityWarning
	body := fmt.Sprintf("The %s adapter failed a %s call: %s. Nothing was placed.",
		brokerName, operation, cause)
	if unknownOutcome {
		severity = SeverityCritical
		body = fmt.Sprintf(
			"The %s adapter returned an UNKNOWN outcome for a %s call: %s. "+
				"An order may exist at the venue. It will NOT be retried; run reconciliation to establish what happened.",
			brokerName, operation, cause)
	}
	a.Raise(ctx, Event{
		Kind:     KindBrokerFailure,
		Severity: severity,
		Category: CategoryTrading,
		Key:      brokerName + ":" + operation,
		Title:    fmt.Sprintf("Broker %s failed (%s)", brokerName, operation),
		Body:     body,
		Fields: map[string]any{
			"broker": brokerName, "operation": operation,
			"unknown_outcome": unknownOutcome, "cause": cause,
		},
	})
}

// BrokerRecovered reports that a venue is answering again.
func (a *Alerter) BrokerRecovered(ctx context.Context, brokerName, operation string) {
	a.Resolve(ctx, KindBrokerFailure, brokerName+":"+operation,
		fmt.Sprintf("Broker %s recovered", brokerName),
		fmt.Sprintf("The %s adapter is answering %s calls again.", brokerName, operation), nil)
}

// ReconciliationMismatch reports discrepancies against the venue.
func (a *Alerter) ReconciliationMismatch(ctx context.Context, accountID uuid.UUID, critical, total int, kinds []string) {
	severity := SeverityWarning
	body := fmt.Sprintf("Reconciliation found %d discrepancy(ies) (%v). None are critical, so automation continues.",
		total, kinds)
	if critical > 0 {
		severity = SeverityCritical
		body = fmt.Sprintf(
			"Reconciliation found %d CRITICAL discrepancy(ies) out of %d (%v). "+
				"Automated trading is halted for this account: trading on a position book known to be wrong is worse than not trading. "+
				"Manual trading is unaffected.",
			critical, total, kinds)
	}
	id := accountID
	a.Raise(ctx, Event{
		Kind:      KindReconciliation,
		Severity:  severity,
		Category:  CategoryReconciliation,
		Key:       accountID.String(),
		Title:     "Reconciliation mismatch against the venue",
		Body:      body,
		AccountID: &id,
		Fields: map[string]any{
			"account_id": accountID.String(),
			"critical":   critical, "total": total, "kinds": kinds,
		},
	})
}

// ReconciliationClean reports that an account agrees with the venue again.
func (a *Alerter) ReconciliationClean(ctx context.Context, accountID uuid.UUID) {
	id := accountID
	a.Resolve(ctx, KindReconciliation, accountID.String(),
		"Reconciliation clean",
		"Vantage's records agree with the venue again. Automation is permitted.", &id)
}

// KillSwitchActivated reports a halt. Never suppressed: a person did this
// deliberately and everyone affected should see it.
func (a *Alerter) KillSwitchActivated(ctx context.Context, scope, target, reason string, accountID *uuid.UUID) {
	a.Raise(ctx, Event{
		Kind:     KindKillSwitch,
		Severity: SeverityCritical,
		Category: CategoryRisk,
		Key:      scope + ":" + target,
		Title:    fmt.Sprintf("Kill switch activated (%s)", scope),
		Body: fmt.Sprintf(
			"New orders are refused within the %s scope. Reason: %s. "+
				"Open positions are NOT affected — closing one is the separate, confirmed Flatten action.",
			scope, reason),
		AccountID: accountID,
		Fields:    map[string]any{"scope": scope, "target": target, "reason": reason},
	})
}

// KillSwitchReleased reports that a halt was lifted.
func (a *Alerter) KillSwitchReleased(ctx context.Context, scope, target string, accountID *uuid.UUID) {
	a.Raise(ctx, Event{
		Kind:      KindKillSwitch,
		Severity:  SeverityInfo,
		Category:  CategoryRisk,
		Key:       scope + ":" + target + ":released",
		Title:     fmt.Sprintf("Kill switch released (%s)", scope),
		Body:      "New orders are accepted again within this scope.",
		AccountID: accountID,
		Fields:    map[string]any{"scope": scope, "target": target},
	})
}

// DailyLossThreshold reports that an account is at or near its daily ceiling.
func (a *Alerter) DailyLossThreshold(ctx context.Context, accountID uuid.UUID, loss, limit, currency string, breached bool) {
	severity := SeverityWarning
	title := "Daily loss approaching its limit"
	body := fmt.Sprintf("Today's loss is %s %s against a limit of %s %s. New orders are still accepted.",
		loss, currency, limit, currency)
	if breached {
		severity = SeverityCritical
		title = "Daily loss limit reached"
		body = fmt.Sprintf(
			"Today's loss is %s %s against a limit of %s %s. New orders are refused for the rest of the trading day. "+
				"An account past its daily limit gets no trade, not a smaller one.",
			loss, currency, limit, currency)
	}
	id := accountID
	a.Raise(ctx, Event{
		Kind:      KindDailyLoss,
		Severity:  severity,
		Category:  CategoryRisk,
		Key:       accountID.String(),
		Title:     title,
		Body:      body,
		AccountID: &id,
		Fields: map[string]any{
			"account_id": accountID.String(), "loss": loss,
			"limit": limit, "currency": currency, "breached": breached,
		},
	})
}

// RepeatedAuthFailure reports a locked account or a burst of failures.
func (a *Alerter) RepeatedAuthFailure(ctx context.Context, email string, failures int, locked bool, ip string) {
	severity := SeverityWarning
	title := fmt.Sprintf("Repeated sign-in failures for %s", email)
	body := fmt.Sprintf("%d consecutive failures from %s. The account is not locked yet.", failures, ip)
	if locked {
		severity = SeverityCritical
		title = fmt.Sprintf("Account locked after repeated sign-in failures: %s", email)
		body = fmt.Sprintf(
			"%d consecutive failures from %s. The account is locked with exponential backoff. "+
				"If this was not you, the password is being guessed.",
			failures, ip)
	}
	a.Raise(ctx, Event{
		Kind:     KindAuthFailures,
		Severity: severity,
		Category: CategorySecurity,
		Key:      email,
		Title:    title,
		Body:     body,
		// The address and the failure count are operationally necessary. The
		// attempted password is never recorded anywhere.
		Fields: map[string]any{"email": email, "failures": failures, "locked": locked, "ip": ip},
	})
}

// QuantFailure reports that the research service is unusable.
func (a *Alerter) QuantFailure(ctx context.Context, reason string, breakerOpen bool) {
	body := fmt.Sprintf("The research service is unreachable: %s. Signal generation is paused; "+
		"the answer is NO SIGNAL rather than an unfiltered trade. Manual trading is unaffected.", reason)
	if breakerOpen {
		body = fmt.Sprintf("The research service circuit breaker is OPEN after repeated failures: %s. "+
			"No calls will be attempted until it closes. Manual trading is unaffected.", reason)
	}
	a.Raise(ctx, Event{
		Kind:     KindQuantFailure,
		Severity: SeverityWarning,
		Category: CategorySystem,
		Key:      "quant",
		Title:    "Research service unavailable",
		Body:     body,
		Fields:   map[string]any{"reason": reason, "breaker_open": breakerOpen},
	})
}

// QuantRecovered reports that the research service is answering again.
func (a *Alerter) QuantRecovered(ctx context.Context) {
	a.Resolve(ctx, KindQuantFailure, "quant",
		"Research service recovered",
		"The research service is answering again and signal generation has resumed.", nil)
}

// OrderOutcomeUnknown reports an order whose venue-side result was lost.
// Never suppressed: each one needs resolving individually.
func (a *Alerter) OrderOutcomeUnknown(ctx context.Context, accountID uuid.UUID, orderID, symbol, cause string) {
	id := accountID
	a.Raise(ctx, Event{
		Kind:     KindOrderFailed,
		Severity: SeverityCritical,
		Category: CategoryTrading,
		Key:      orderID,
		Title:    fmt.Sprintf("Order outcome unknown: %s", symbol),
		Body: fmt.Sprintf(
			"Order %s on %s is FAILED, which means the venue-side outcome is unknown rather than that nothing happened: %s. "+
				"It will NOT be retried automatically. Automated trading is blocked for this account until reconciliation resolves it.",
			orderID, symbol, cause),
		AccountID: &id,
		Fields: map[string]any{
			"order_id": orderID, "symbol": symbol,
			"account_id": accountID.String(), "cause": cause,
		},
	})
}

// AuditChainBroken is the one alert that means stop trading immediately.
func (a *Alerter) AuditChainBroken(ctx context.Context, brokenAtSequence int64) {
	a.Raise(ctx, Event{
		Kind:     KindAuditChainBroken,
		Severity: SeverityCritical,
		Category: CategorySecurity,
		Key:      "audit",
		Title:    "AUDIT CHAIN VERIFICATION FAILED",
		Body: fmt.Sprintf(
			"The audit hash chain does not verify from sequence %d. The record of what this system did has been altered. "+
				"Stop trading, preserve the database, and establish who had write access before doing anything else.",
			brokenAtSequence),
		Fields: map[string]any{"broken_at_sequence": brokenAtSequence},
	})
}

// ---------------------------------------------------------------------------
// Reconciliation recovery
// ---------------------------------------------------------------------------

// ReconciliationIssueRaised announces one newly detected divergence.
//
// Keyed on the issue's identity rather than the account, so a run that finds
// the same three issues every minute announces them once. The per-run summary
// (ReconciliationMismatch) is what reports the standing total.
func (a *Alerter) ReconciliationIssueRaised(ctx context.Context, accountID uuid.UUID,
	issueType, severity, description string) {

	id := accountID
	sev := SeverityWarning
	switch severity {
	case "critical":
		sev = SeverityCritical
	case "info":
		sev = SeverityInfo
	}
	a.Raise(ctx, Event{
		Kind:      KindReconciliationIssue,
		Severity:  sev,
		Category:  CategoryReconciliation,
		Key:       accountID.String() + ":" + issueType,
		Title:     "Reconciliation issue: " + issueType,
		Body:      description,
		AccountID: &id,
		Fields: map[string]any{
			"account_id": accountID.String(),
			"issue_type": issueType, "severity": severity,
		},
	})
}

// ReconciliationRepaired announces an automatic repair.
//
// Announced even though it succeeded. Software writing to an append-only
// ledger on the strength of a venue snapshot is worth knowing about
// especially when it is correct: an operator who never hears about repairs
// cannot notice that they have started happening every day.
func (a *Alerter) ReconciliationRepaired(ctx context.Context, accountID uuid.UUID,
	issueType, action, detail string) {

	id := accountID
	a.Raise(ctx, Event{
		Kind:     KindReconciliationRepair,
		Severity: SeverityWarning,
		Category: CategoryReconciliation,
		Key:      accountID.String() + ":" + issueType + ":" + action,
		Title:    "Reconciliation repaired a divergence",
		Body: "Vantage corrected its own records to match the venue (" + issueType +
			", " + action + "): " + detail + ". The repair went through the same " +
			"accounting path as an ordinary execution.",
		AccountID: &id,
		Fields: map[string]any{
			"account_id": accountID.String(),
			"issue_type": issueType, "action": action, "detail": detail,
		},
	})
}

// ReconciliationFailed reports that reconciliation could not run.
//
// This is not the same as finding a divergence: while reconciliation cannot
// run, NOTHING is confirming that Vantage's records match the venue, and the
// account's readiness reports RECONCILIATION_REQUIRED rather than healthy.
// Repeated failure escalates to critical, because a venue that has been
// unreachable for an hour is a different problem from one that blipped.
func (a *Alerter) ReconciliationFailed(ctx context.Context, accountID uuid.UUID,
	consecutive int, cause string) {

	id := accountID
	sev := SeverityWarning
	body := "Reconciliation could not complete: " + cause +
		". Automated trading is paused for this account until a run succeeds, because " +
		"nothing is currently confirming that Vantage's records match the venue."
	if consecutive >= 3 {
		sev = SeverityCritical
		body = "Reconciliation has failed " + itoa(consecutive) + " times in a row: " + cause +
			". Automated trading is paused. This is no longer a transient venue problem."
	}
	a.Raise(ctx, Event{
		Kind:      KindReconciliationFailed,
		Severity:  sev,
		Category:  CategoryReconciliation,
		Key:       accountID.String(),
		Title:     "Reconciliation failed",
		Body:      body,
		AccountID: &id,
		Fields: map[string]any{
			"account_id": accountID.String(), "consecutive_failures": consecutive,
			"cause": cause,
		},
	})
}

// TradingHalted announces that automation stopped, and how widely.
func (a *Alerter) TradingHalted(ctx context.Context, accountID uuid.UUID, scope, reason string) {
	id := accountID
	a.Raise(ctx, Event{
		Kind:     KindTradingHalted,
		Severity: SeverityCritical,
		Category: CategoryReconciliation,
		Key:      accountID.String(),
		Title:    "Automated trading halted (" + scope + ")",
		Body: "Automated trading is halted: " + reason +
			". Manual trading and all read access are unaffected -- an operator can see " +
			"the warning and decide, which an algorithm cannot.",
		AccountID: &id,
		Fields: map[string]any{
			"account_id": accountID.String(), "halt_scope": scope, "reason": reason,
		},
	})
}

// TradingResumed announces that automation may run again.
func (a *Alerter) TradingResumed(ctx context.Context, accountID uuid.UUID) {
	id := accountID
	a.Resolve(ctx, KindTradingHalted, accountID.String(),
		"Automated trading resumed",
		"Every divergence is resolved and reconciliation is current. Automation is permitted.",
		&id)
}

// itoa avoids importing strconv for one call in a message.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
