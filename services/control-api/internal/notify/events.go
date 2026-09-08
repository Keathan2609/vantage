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
