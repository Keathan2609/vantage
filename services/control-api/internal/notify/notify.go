// Package notify surfaces the events an operator must not miss.
//
// This is deliberately not an alerting SaaS integration. It has three sinks,
// all of which already exist in the platform:
//
//	a structured log line at an appropriate level
//	a Prometheus counter, so a dashboard or a scrape can see the rate
//	a notifications row, so the terminal shows it without polling anything new
//
// The design constraint that matters is DE-DUPLICATION. A stale market feed
// is detected every two seconds; a broker outage is detected on every call.
// Without a cooldown, the first minute of an incident writes hundreds of
// identical rows, the notification list becomes unreadable, and the operator
// learns to ignore it -- which is worse than having no alerting at all.
//
// Each event therefore has a key and a per-key cooldown. The first occurrence
// alerts immediately; repeats inside the window are counted and suppressed;
// when the window expires the next occurrence alerts again and reports how
// many were suppressed. Recovery is alerted once, so "it is still broken" and
// "it came back" are distinguishable.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/store"
)

// Severity of an event.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Category matches the notifications table's CHECK constraint.
type Category string

const (
	CategoryTrading        Category = "trading"
	CategoryRisk           Category = "risk"
	CategorySecurity       Category = "security"
	CategorySystem         Category = "system"
	CategoryData           Category = "data"
	CategoryStrategy       Category = "strategy"
	CategoryModel          Category = "model"
	CategoryReconciliation Category = "reconciliation"
)

// Kind identifies one class of event. The cooldown is per (Kind, Key).
type Kind string

const (
	KindMarketDataStale     Kind = "market_data_stale"
	KindBrokerFailure       Kind = "broker_failure"
	KindReconciliation      Kind = "reconciliation_mismatch"
	KindKillSwitch          Kind = "kill_switch_activated"
	KindDailyLoss           Kind = "daily_loss_threshold"
	KindAuthFailures        Kind = "repeated_auth_failure"
	KindQuantFailure        Kind = "quant_service_failure"
	KindOrderFailed         Kind = "order_outcome_unknown"
	KindAuditChainBroken    Kind = "audit_chain_broken"
	KindAutomationSuspended Kind = "automation_suspended"
	// KindAuthorityExpiring warns BEFORE a trading authority lapses.
	//
	// Expiry is otherwise completely silent. ActiveAuthorityForAccount does not
	// filter on valid_until, so the row is still returned; Effective refuses;
	// and the scheduler's strategy loop treats the refusal as an ordinary skip
	// and continues without logging. The platform simply stops trading one day,
	// with no alert, no readiness signal and no log line naming the cause.
	KindAuthorityExpiring Kind = "trading_authority_expiring"
	// KindReconciliationIssue is one newly raised divergence. Distinct from
	// KindReconciliation, which is the per-run summary: a run that finds the
	// same three issues every minute should announce them once, not announce
	// "three issues" every minute.
	KindReconciliationIssue Kind = "reconciliation_issue_raised"
	// KindReconciliationRepair records an automatic repair. Announced because
	// software writing to the ledger is worth knowing about even when it is
	// correct -- especially then.
	KindReconciliationRepair Kind = "reconciliation_repaired"
	// KindReconciliationFailed means reconciliation itself could not run.
	// Never suppressed: while it cannot run, nothing is confirming that
	// Vantage's records match the venue.
	KindReconciliationFailed Kind = "reconciliation_failed"
	// KindTradingHalted and KindTradingResumed bracket an automation stop.
	KindTradingHalted  Kind = "trading_halted"
	KindTradingResumed Kind = "trading_resumed"
)

// Cooldowns per kind. Chosen from how fast the underlying condition is
// detected, not from a single global number: a feed is checked every two
// seconds, reconciliation every five minutes.
var cooldowns = map[Kind]time.Duration{
	KindMarketDataStale:     2 * time.Minute,
	KindBrokerFailure:       1 * time.Minute,
	KindReconciliation:      10 * time.Minute,
	KindKillSwitch:          0, // never suppressed: a human did this deliberately
	KindDailyLoss:           15 * time.Minute,
	KindAuthFailures:        5 * time.Minute,
	KindQuantFailure:        2 * time.Minute,
	KindOrderFailed:         0, // never suppressed: each one needs resolving
	KindAuditChainBroken:    0, // never suppressed
	KindAutomationSuspended: 5 * time.Minute,
	// Per-issue alerts are deduplicated by the ISSUE's fingerprint rather than
	// by time: the alerter's key is the issue id, and an issue is raised once.
	// A cooldown as well would suppress a genuinely new second issue arriving
	// in the same window, which is the opposite of what is wanted.
	KindReconciliationIssue: 0,
	// A repair is a discrete event that happened once. Suppressing the second
	// of two repairs would hide a real ledger write.
	KindReconciliationRepair: 0,
	// Rate-limited, because a venue that is down fails every scheduled run and
	// would otherwise produce an alert per minute. The suppressed count still
	// reports how many, so "it has been failing for an hour" is visible.
	KindReconciliationFailed: 5 * time.Minute,
	KindTradingHalted:        0, // never suppressed: automation stopping is the headline
	KindTradingResumed:       0,
}

// Event is one thing worth telling someone about.
type Event struct {
	Kind     Kind
	Severity Severity
	Category Category
	// Key distinguishes instances within a kind -- an instrument id, an
	// account id, a provider name. Events with the same kind and key share a
	// cooldown window.
	Key string
	// Title is one line. Body explains what it means and what to do.
	Title string
	Body  string
	// AccountID scopes the notification to one account when the event belongs
	// to one; system-wide events leave it nil.
	AccountID *uuid.UUID
	// Fields are added to the log line and stored as notification metadata.
	// Never put a secret here: this is written to the database and read by the
	// terminal.
	Fields map[string]any
}

// Alerter fans an event out to the log, the metrics and the notification list.
type Alerter struct {
	store *store.Store
	now   func() time.Time

	mu    sync.Mutex
	state map[string]*window
}

type window struct {
	lastAlert  time.Time
	suppressed int
	active     bool
}

// New builds an alerter.
func New(s *store.Store, now func() time.Time) *Alerter {
	return &Alerter{store: s, now: now, state: map[string]*window{}}
}

func (a *Alerter) key(e Event) string { return string(e.Kind) + "|" + e.Key }

// shouldAlert applies the cooldown and returns how many repeats were
// suppressed since the last alert.
func (a *Alerter) shouldAlert(e Event) (bool, int) {
	cooldown, ok := cooldowns[e.Kind]
	if !ok {
		cooldown = 5 * time.Minute
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	k := a.key(e)
	w, seen := a.state[k]
	if !seen {
		w = &window{}
		a.state[k] = w
	}
	now := a.now()
	w.active = true

	if cooldown == 0 || w.lastAlert.IsZero() || now.Sub(w.lastAlert) >= cooldown {
		suppressed := w.suppressed
		w.suppressed = 0
		w.lastAlert = now
		return true, suppressed
	}
	w.suppressed++
	return false, 0
}

// Raise records an event. It never returns an error: a failure to store a
// notification must not fail the operation that noticed the problem, and the
// log line has already been written by then.
func (a *Alerter) Raise(ctx context.Context, e Event) {
	log := logging.FromContext(ctx)

	fields := []any{"kind", string(e.Kind), "key", e.Key, "severity", string(e.Severity)}
	for name, value := range e.Fields {
		fields = append(fields, name, value)
	}

	metrics.AlertsRaised.WithLabelValues(string(e.Kind), string(e.Severity)).Inc()

	alert, suppressed := a.shouldAlert(e)
	if !alert {
		// Counted, not logged: the point of a cooldown is that the hundredth
		// occurrence does not push the first one off the screen.
		metrics.AlertsSuppressed.WithLabelValues(string(e.Kind)).Inc()
		return
	}
	if suppressed > 0 {
		fields = append(fields, "suppressed_repeats", suppressed)
	}

	// The log level carries the severity, so an operator watching stdout in
	// development sees the same thing the terminal will show.
	switch e.Severity {
	case SeverityCritical:
		log.Error("ALERT: "+e.Title, fields...)
	case SeverityWarning:
		log.Warn("ALERT: "+e.Title, fields...)
	default:
		log.Info("ALERT: "+e.Title, fields...)
	}

	if a.store == nil {
		return
	}
	body := e.Body
	if suppressed > 0 {
		body += fmt.Sprintf(" (%d further occurrence(s) were suppressed while this was already active.)", suppressed)
	}
	metadata := e.Fields
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["kind"] = string(e.Kind)
	metadata["key"] = e.Key
	raw, err := json.Marshal(metadata)
	if err != nil {
		raw = []byte(`{}`)
	}

	recipients, err := a.recipients(ctx, e)
	if err != nil {
		log.Warn("could not resolve alert recipients", "error", err.Error(), "kind", string(e.Kind))
		return
	}
	for _, userID := range recipients {
		n := store.Notification{
			UserID:    userID,
			AccountID: e.AccountID,
			Severity:  string(e.Severity),
			Category:  string(e.Category),
			Title:     e.Title,
			Body:      body,
			Metadata:  raw,
		}
		if err := a.store.Control.CreateNotification(ctx, a.store.Pool(), n); err != nil {
			log.Warn("could not store notification", "error", err.Error(), "kind", string(e.Kind))
			return
		}
	}
}

// Resolve reports that a previously raised condition has cleared, once.
//
// Without this, an operator cannot tell "still broken" from "recovered", and
// the only signal is the absence of new alerts -- which is indistinguishable
// from the detector having died.
func (a *Alerter) Resolve(ctx context.Context, kind Kind, key string, title, body string, accountID *uuid.UUID) {
	a.mu.Lock()
	w, seen := a.state[string(kind)+"|"+key]
	if !seen || !w.active {
		a.mu.Unlock()
		return
	}
	w.active = false
	w.lastAlert = time.Time{}
	w.suppressed = 0
	a.mu.Unlock()

	a.Raise(ctx, Event{
		Kind:      kind,
		Severity:  SeverityInfo,
		Category:  CategorySystem,
		Key:       key + ":resolved",
		Title:     title,
		Body:      body,
		AccountID: accountID,
	})
}

// Active reports whether a condition is currently raised, for tests and for
// the health endpoint.
func (a *Alerter) Active(kind Kind, key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.state[string(kind)+"|"+key]
	return ok && w.active
}

// recipients resolves who is told.
//
// An account-scoped event goes to that account's owner. A system event goes to
// every enabled user who could act on it -- a viewer cannot fix a broker
// outage, but they are entitled to know the numbers on their screen are stale.
func (a *Alerter) recipients(ctx context.Context, e Event) ([]uuid.UUID, error) {
	if e.AccountID != nil {
		owner, err := a.store.Accounts.OwnerOf(ctx, *e.AccountID)
		if err == nil {
			return []uuid.UUID{owner}, nil
		}
		// Fall through to everyone rather than dropping the alert: an alert
		// nobody receives is the failure mode this package exists to avoid.
	}
	users, err := a.store.Users.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(users))
	for _, u := range users {
		if u.Disabled {
			continue
		}
		out = append(out, u.ID)
	}
	return out, nil
}
