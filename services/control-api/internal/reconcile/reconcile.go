// Package reconcile compares Vantage's view of an account with the venue's.
//
// The premise: Vantage's state is a CACHE of the venue's, and caches go stale.
// Responses get lost, processes die mid-write, brokers cancel orders on their
// own, and people place trades in the broker's own terminal. Any system that
// assumes its local records match the venue will eventually size a position
// against exposure that does not exist.
//
// Reconciliation runs at start-up, on a schedule, after a reconnect, and after
// any order whose outcome was unknown. Where it finds a difference, the VENUE
// WINS: it holds the money.
//
// Unresolved critical discrepancies halt automated trading for the account.
// That is deliberate. Trading on a position book that is known to be wrong is
// worse than not trading.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/store"
)

// Service performs reconciliation.
type Service struct {
	store   *store.Store
	brokers *broker.Registry
	clock   domain.Clock
	alerter Alerter
}

// Alerter is the subset of internal/notify this service needs, declared here
// so reconcile does not import notify.
type Alerter interface {
	ReconciliationMismatch(ctx context.Context, accountID uuid.UUID, critical, total int, kinds []string)
	ReconciliationClean(ctx context.Context, accountID uuid.UUID)
}

// SetAlerter attaches an alerter after construction.
func (s *Service) SetAlerter(a Alerter) { s.alerter = a }

// New builds the reconciliation service.
func New(s *store.Store, brokers *broker.Registry, clock domain.Clock) *Service {
	return &Service{store: s, brokers: brokers, clock: clock}
}

// Trigger describes why a run happened.
type Trigger string

const (
	TriggerStartup     Trigger = "startup"
	TriggerScheduled   Trigger = "scheduled"
	TriggerManual      Trigger = "manual"
	TriggerReconnect   Trigger = "reconnect"
	TriggerPostFailure Trigger = "post_failure"
)

// Report summarises one run.
type Report struct {
	RunID             uuid.UUID
	AccountID         uuid.UUID
	Trigger           Trigger
	Status            string
	OrdersCompared    int
	PositionsCompared int
	Discrepancies     []Discrepancy
	Resolved          int
	StartedAt         time.Time
	FinishedAt        time.Time
}

// Clean reports whether the two views agreed.
func (r Report) Clean() bool { return len(r.Discrepancies) == 0 }

// CriticalCount counts discrepancies that should halt automation.
func (r Report) CriticalCount() int {
	n := 0
	for _, d := range r.Discrepancies {
		if d.Severity == "critical" {
			n++
		}
	}
	return n
}

// Discrepancy is one difference between the two views.
type Discrepancy struct {
	Kind         string
	Severity     string
	OrderID      *uuid.UUID
	InstrumentID string
	VantageValue string
	BrokerValue  string
	Description  string
	// AutoResolved is true when reconciliation could safely correct Vantage's
	// record itself — for example adopting the venue's terminal status for an
	// order Vantage had marked FAILED.
	AutoResolved bool
}

// Run reconciles one account.
func (s *Service) Run(ctx context.Context, account domain.Account, trigger Trigger) (Report, error) {
	log := logging.FromContext(ctx)
	started := s.clock.Now()

	report := Report{AccountID: account.ID, Trigger: trigger, StartedAt: started}

	adapter, err := s.brokers.Get(account.BrokerName)
	if err != nil {
		return report, fmt.Errorf("reconcile: %w", err)
	}

	runID, err := s.store.Research.StartReconciliationRun(ctx, account.ID, account.BrokerName, string(trigger))
	if err != nil {
		return report, fmt.Errorf("reconcile: start run: %w", err)
	}
	report.RunID = runID

	accountRef := account.ID.String()
	if account.BrokerAcctRef != nil && *account.BrokerAcctRef != "" {
		accountRef = *account.BrokerAcctRef
	}

	// A venue that cannot be reached is not a clean reconciliation. Recording
	// it as failed keeps automation halted rather than letting a silent error
	// look like agreement.
	venueOrders, err := adapter.FetchOpenOrders(ctx, accountRef)
	if err != nil {
		_ = s.store.Research.FinishReconciliationRun(ctx, runID, "failed", 0, 0, 0, err.Error())
		metrics.ReconciliationRuns.WithLabelValues(string(trigger), "failed").Inc()
		return report, fmt.Errorf("reconcile: fetch venue orders: %w", err)
	}
	venuePositions, err := adapter.FetchPositions(ctx, accountRef)
	if err != nil {
		_ = s.store.Research.FinishReconciliationRun(ctx, runID, "failed", 0, 0, 0, err.Error())
		metrics.ReconciliationRuns.WithLabelValues(string(trigger), "failed").Inc()
		return report, fmt.Errorf("reconcile: fetch venue positions: %w", err)
	}

	orderDiscrepancies, ordersCompared, err := s.reconcileOrders(ctx, account, adapter, accountRef, venueOrders, runID)
	if err != nil {
		return report, err
	}
	positionDiscrepancies, positionsCompared, err := s.reconcilePositions(ctx, account, venuePositions, runID)
	if err != nil {
		return report, err
	}

	report.OrdersCompared = ordersCompared
	report.PositionsCompared = positionsCompared
	report.Discrepancies = append(orderDiscrepancies, positionDiscrepancies...)
	report.FinishedAt = s.clock.Now()

	status := "clean"
	if len(report.Discrepancies) > 0 {
		status = "discrepancies_found"
	}
	report.Status = status

	if err := s.store.Research.FinishReconciliationRun(ctx, runID, status,
		ordersCompared, positionsCompared, len(report.Discrepancies), ""); err != nil {
		return report, err
	}

	metrics.ReconciliationRuns.WithLabelValues(string(trigger), status).Inc()
	unresolved, _ := s.store.Research.UnresolvedDiscrepancies(ctx, account.ID)
	metrics.UnresolvedDiscrepancies.WithLabelValues(account.ID.String()).Set(float64(len(unresolved)))

	if len(report.Discrepancies) > 0 {
		log.Warn("reconciliation found discrepancies",
			"account_id", account.ID.String(), "count", len(report.Discrepancies),
			"critical", report.CriticalCount(), "trigger", trigger)
		if s.alerter != nil {
			kinds := make([]string, 0, len(report.Discrepancies))
			seen := map[string]bool{}
			for _, d := range report.Discrepancies {
				if !seen[d.Kind] {
					seen[d.Kind] = true
					kinds = append(kinds, d.Kind)
				}
			}
			s.alerter.ReconciliationMismatch(ctx, account.ID,
				report.CriticalCount(), len(report.Discrepancies), kinds)
		}
	} else {
		log.Info("reconciliation clean",
			"account_id", account.ID.String(), "orders", ordersCompared,
			"positions", positionsCompared, "trigger", trigger)
		if s.alerter != nil {
			// Only fires if a mismatch was previously raised, so a permanently
			// healthy account is silent.
			s.alerter.ReconciliationClean(ctx, account.ID)
		}
	}
	return report, nil
}

// reconcileOrders compares open orders both ways.
func (s *Service) reconcileOrders(ctx context.Context, account domain.Account, adapter broker.Adapter,
	accountRef string, venueOrders []broker.VenueOrder, runID uuid.UUID) ([]Discrepancy, int, error) {

	localOrders, err := s.store.Trading.ListOrdersForUser(ctx, account.UserID, store.OrderFilter{
		AccountID: &account.ID, OpenOnly: true, Limit: 500,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("reconcile: list local orders: %w", err)
	}

	// Orders Vantage does not consider open but which may still be unresolved.
	failedOrders, err := s.store.Trading.ListOrdersForUser(ctx, account.UserID, store.OrderFilter{
		AccountID: &account.ID, Status: []domain.OrderStatus{domain.OrderFailed}, Limit: 200,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("reconcile: list failed orders: %w", err)
	}
	localOrders = append(localOrders, failedOrders...)

	byBrokerID := map[string]broker.VenueOrder{}
	byClientID := map[string]broker.VenueOrder{}
	for _, vo := range venueOrders {
		byBrokerID[vo.BrokerOrderID] = vo
		byClientID[vo.ClientOrderID] = vo
	}

	var out []Discrepancy
	compared := 0

	for _, local := range localOrders {
		compared++

		var venue broker.VenueOrder
		var found bool
		if local.BrokerOrderID != nil {
			venue, found = byBrokerID[*local.BrokerOrderID]
		}
		if !found {
			// Resolve by client id: this is how an order whose acknowledgement
			// was lost is discovered to be live at the venue after all.
			venue, found = byClientID[local.CommandID.String()]
			if !found {
				vo, err := adapter.FetchOrderByClientID(ctx, accountRef, local.CommandID.String())
				if err == nil {
					venue, found = vo, true
				} else if !errors.Is(err, broker.ErrNotFound) {
					return out, compared, fmt.Errorf("reconcile: fetch order by client id: %w", err)
				}
			}
		}

		if !found {
			// The venue has no record. A FAILED order that never reached the
			// venue is good news and can be closed out; an order Vantage
			// believes is working is a genuine mismatch.
			if local.Status == domain.OrderFailed {
				d := Discrepancy{
					Kind: "order_missing_at_broker", Severity: "info",
					OrderID: &local.ID, InstrumentID: local.InstrumentID,
					VantageValue: string(local.Status), BrokerValue: "absent",
					Description:  "Order marked FAILED never reached the venue; marking it rejected.",
					AutoResolved: true,
				}
				if err := s.transitionWithCode(ctx, local, domain.OrderRejected,
					domain.RejectReconciledAbsent,
					"reconciliation: venue has no record of this order"); err != nil {
					return out, compared, err
				}
				out = append(out, d)
				if err := s.record(ctx, runID, account.ID, d); err != nil {
					return out, compared, err
				}
				continue
			}
			// An open order that never received a venue identifier, and which
			// the venue does not recognise by its client id either, never
			// reached the market. It is closed out on the same evidence the
			// FAILED branch above uses: a direct lookup by client id, not a
			// gap in a paginated list.
			//
			// This case exists because a crash or a rolled-back transaction
			// between persisting the order and recording the venue's answer
			// leaves it in ACCEPTED with no venue id. Before this branch, such
			// an order was UNRESOLVABLE: cancellation refuses it
			// ("order_not_at_venue"), no endpoint resolves a discrepancy by
			// hand, and it kept consuming the account's pending-order budget
			// forever. Six of them accumulated during a concurrency test and
			// took the account from "can trade" to "every order refused for
			// max_pending_orders" permanently.
			//
			// An order that DOES hold a venue identifier the venue denies is a
			// different and much more alarming thing, and stays critical and
			// manual.
			if local.BrokerOrderID == nil || *local.BrokerOrderID == "" {
				d := Discrepancy{
					Kind: "order_never_reached_venue", Severity: "warning",
					OrderID: &local.ID, InstrumentID: local.InstrumentID,
					VantageValue: string(local.Status), BrokerValue: "absent",
					Description: "Order holds no venue identifier and the venue does not " +
						"recognise its client id; it never reached the market. Marking it rejected.",
					AutoResolved: true,
				}
				if err := s.transitionWithCode(ctx, local, domain.OrderRejected,
					domain.RejectReconciledAbsent,
					"reconciliation: order never reached the venue"); err != nil {
					return out, compared, err
				}
				out = append(out, d)
				if err := s.record(ctx, runID, account.ID, d); err != nil {
					return out, compared, err
				}
				continue
			}

			d := Discrepancy{
				Kind: "order_missing_at_broker", Severity: "critical",
				OrderID: &local.ID, InstrumentID: local.InstrumentID,
				VantageValue: string(local.Status), BrokerValue: "absent",
				Description: "Vantage believes this order is working and holds a venue " +
					"identifier for it, but the venue has no record of it.",
			}
			out = append(out, d)
			if err := s.record(ctx, runID, account.ID, d); err != nil {
				return out, compared, err
			}
			continue
		}

		// Both know the order. Compare status and filled quantity.
		venueStatus, mappable := venue.Status.ToOrderStatus()
		if mappable && venueStatus != local.Status {
			severity := "warning"
			autoResolved := false
			// Where the venue reports a terminal state and Vantage's state
			// machine permits the move, adopt it: the venue is authoritative.
			if domain.CanTransition(local.Status, venueStatus) {
				if err := s.transition(ctx, local, venueStatus,
					"reconciliation: venue reported "+string(venue.Status)); err != nil {
					return out, compared, err
				}
				autoResolved = true
				severity = "info"
			} else {
				severity = "critical"
			}
			d := Discrepancy{
				Kind: "order_status_mismatch", Severity: severity,
				OrderID: &local.ID, InstrumentID: local.InstrumentID,
				VantageValue: string(local.Status), BrokerValue: string(venue.Status),
				Description:  "Vantage and the venue disagree about this order's status.",
				AutoResolved: autoResolved,
			}
			out = append(out, d)
			if err := s.record(ctx, runID, account.ID, d); err != nil {
				return out, compared, err
			}
		}

		if !venue.FilledQuantity.Equal(local.FilledQuantity) {
			d := Discrepancy{
				Kind: "fill_quantity_mismatch", Severity: "critical",
				OrderID: &local.ID, InstrumentID: local.InstrumentID,
				VantageValue: local.FilledQuantity.String(),
				BrokerValue:  venue.FilledQuantity.String(),
				Description: "The venue reports a different filled quantity. " +
					"Executions are missing from Vantage's records.",
			}
			out = append(out, d)
			if err := s.record(ctx, runID, account.ID, d); err != nil {
				return out, compared, err
			}
		}
	}

	// Orders the venue is working that Vantage has never heard of. This is
	// what a trade placed directly in the broker's own terminal looks like.
	localByBrokerID := map[string]bool{}
	localByCommandID := map[string]bool{}
	for _, l := range localOrders {
		if l.BrokerOrderID != nil {
			localByBrokerID[*l.BrokerOrderID] = true
		}
		localByCommandID[l.CommandID.String()] = true
	}
	for _, vo := range venueOrders {
		if localByBrokerID[vo.BrokerOrderID] || localByCommandID[vo.ClientOrderID] {
			continue
		}
		compared++
		d := Discrepancy{
			Kind: "order_unknown_to_vantage", Severity: "critical",
			InstrumentID: vo.Symbol,
			VantageValue: "absent", BrokerValue: string(vo.Status),
			Description: fmt.Sprintf(
				"The venue is working an order (%s %s %s) that Vantage did not place.",
				vo.Side, vo.Quantity, vo.Symbol),
		}
		out = append(out, d)
		if err := s.record(ctx, runID, account.ID, d); err != nil {
			return out, compared, err
		}
	}
	return out, compared, nil
}

// reconcilePositions compares the position books both ways.
func (s *Service) reconcilePositions(ctx context.Context, account domain.Account,
	venuePositions []broker.VenuePosition, runID uuid.UUID) ([]Discrepancy, int, error) {

	localPositions, err := s.store.Trading.OpenPositions(ctx, account.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("reconcile: list local positions: %w", err)
	}

	venueBySymbol := map[string]broker.VenuePosition{}
	for _, vp := range venuePositions {
		venueBySymbol[vp.Symbol] = vp
	}
	localBySymbol := map[string]domain.Position{}
	for _, lp := range localPositions {
		localBySymbol[lp.InstrumentID] = lp
	}

	var out []Discrepancy
	compared := 0

	for _, local := range localPositions {
		compared++
		venue, found := venueBySymbol[local.InstrumentID]
		if !found {
			d := Discrepancy{
				Kind: "position_missing_at_broker", Severity: "critical",
				InstrumentID: local.InstrumentID,
				VantageValue: fmt.Sprintf("%s %s", local.Side, local.Quantity),
				BrokerValue:  "flat",
				Description: "Vantage holds a position the venue does not. " +
					"Risk sizing is being computed against exposure that does not exist.",
			}
			out = append(out, d)
			if err := s.record(ctx, runID, account.ID, d); err != nil {
				return out, compared, err
			}
			continue
		}
		if venue.Side != local.Side || !venue.Quantity.Equal(local.Quantity) {
			d := Discrepancy{
				Kind: "position_quantity_mismatch", Severity: "critical",
				InstrumentID: local.InstrumentID,
				VantageValue: fmt.Sprintf("%s %s", local.Side, local.Quantity),
				BrokerValue:  fmt.Sprintf("%s %s", venue.Side, venue.Quantity),
				Description:  "Vantage and the venue disagree about this position's size or direction.",
			}
			out = append(out, d)
			if err := s.record(ctx, runID, account.ID, d); err != nil {
				return out, compared, err
			}
		}
	}

	for _, venue := range venuePositions {
		if _, found := localBySymbol[venue.Symbol]; found {
			continue
		}
		compared++
		d := Discrepancy{
			Kind: "position_unknown_to_vantage", Severity: "critical",
			InstrumentID: venue.Symbol,
			VantageValue: "flat",
			BrokerValue:  fmt.Sprintf("%s %s", venue.Side, venue.Quantity),
			Description: "The venue holds a position Vantage does not know about. " +
				"It may have been opened outside Vantage, or an execution was missed.",
		}
		out = append(out, d)
		if err := s.record(ctx, runID, account.ID, d); err != nil {
			return out, compared, err
		}
	}
	return out, compared, nil
}

// transition applies a reconciliation-driven state change.
func (s *Service) transition(ctx context.Context, order domain.Order, to domain.OrderStatus, reason string) error {
	return s.transitionWithCode(ctx, order, to, "", reason)
}

// transitionWithCode applies a state change, carrying a reject code where the
// destination requires one.
func (s *Service) transitionWithCode(ctx context.Context, order domain.Order,
	to domain.OrderStatus, code domain.RejectCode, reason string) error {

	return s.store.Pool().InTx(ctx, func(tx pgx.Tx) error {
		current, err := s.store.Trading.OrderByIDTx(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		if !domain.CanTransition(current.Status, to) {
			return nil
		}
		_, err = s.store.Trading.TransitionOrderWithCodeTx(ctx, tx, current.ID, current.Version,
			to, code, reason, "system", nil)
		return err
	})
}

func (s *Service) record(ctx context.Context, runID, accountID uuid.UUID, d Discrepancy) error {
	metrics.ReconciliationMismatches.WithLabelValues(d.Kind, d.Severity).Inc()

	instrument := d.InstrumentID
	rec := store.Discrepancy{
		RunID: runID, AccountID: accountID, Kind: d.Kind, Severity: d.Severity,
		OrderID: d.OrderID, Description: d.Description,
	}
	if instrument != "" {
		rec.InstrumentID = &instrument
	}
	if d.VantageValue != "" {
		v := d.VantageValue
		rec.VantageValue = &v
	}
	if d.BrokerValue != "" {
		b := d.BrokerValue
		rec.BrokerValue = &b
	}
	if err := s.store.Research.RecordDiscrepancy(ctx, rec); err != nil {
		return fmt.Errorf("reconcile: record discrepancy: %w", err)
	}
	return nil
}

// AutomationBlocked reports whether unresolved critical discrepancies should
// stop automated trading on an account.
//
// Consulted by the orchestrator before every scheduled strategy run. A human
// may still trade manually — they can see the warning and decide — but an
// algorithm must not act on a position book known to be wrong.
func (s *Service) AutomationBlocked(ctx context.Context, accountID uuid.UUID) (bool, []store.Discrepancy, error) {
	unresolved, err := s.store.Research.UnresolvedDiscrepancies(ctx, accountID)
	if err != nil {
		return false, nil, err
	}
	var critical []store.Discrepancy
	for _, d := range unresolved {
		if d.Severity == "critical" {
			critical = append(critical, d)
		}
	}
	return len(critical) > 0, critical, nil
}

// RunAll reconciles every account, used at start-up and on a schedule.
func (s *Service) RunAll(ctx context.Context, trigger Trigger) ([]Report, error) {
	accounts, err := s.store.Accounts.ListAllAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var reports []Report
	for _, a := range accounts {
		report, err := s.Run(ctx, a, trigger)
		if err != nil {
			// One account's failure must not stop the others being checked.
			logging.FromContext(ctx).Error("reconciliation failed",
				"account_id", a.ID.String(), "error", err.Error())
			continue
		}
		reports = append(reports, report)
	}
	return reports, nil
}

// CompareBalances checks the venue's balance against Vantage's ledger.
//
// Reported as informational rather than critical: in paper mode the two are
// computed differently by design (the venue simulates its own book), and a
// small difference is expected. A large one is worth investigating, which is
// why the threshold is explicit rather than assumed.
func (s *Service) CompareBalances(ctx context.Context, account domain.Account, venue broker.VenueAccount,
	tolerance decimal.Decimal) (bool, string) {

	ledgerBalance, err := s.store.Accounts.Balance(ctx, account.ID, account.Currency)
	if err != nil {
		return false, "ledger balance unavailable"
	}
	if string(account.Currency) != venue.Currency {
		return false, fmt.Sprintf("currency mismatch: ledger in %s, venue in %s",
			account.Currency, venue.Currency)
	}
	diff := ledgerBalance.Decimal().Sub(venue.Balance).Abs()
	if diff.GreaterThan(tolerance) {
		return false, fmt.Sprintf("balance differs by %s (ledger %s, venue %s)",
			diff.StringFixed(2), ledgerBalance.StringFixed(), venue.Balance.StringFixed(2))
	}
	return true, ""
}
