package reconcile

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/store"
)

// Classification: turning two snapshots into a list of typed issues.
//
// This file contains NO I/O and NO repairs. It is a pure function from
// (local, broker) to []Finding, which is what makes every branch below
// reachable from a unit test with hand-built snapshots -- including the ones a
// live venue almost never produces, like a venue reporting the opposite side
// for an order it agrees exists.
//
// The repair that follows is a separate step, driven by the finding's type and
// the domain repair policy. Detection deciding its own remedy inline is how
// the previous implementation ended up auto-adopting a venue status in one
// branch and refusing to in another for no stated reason.

// Finding is a classified divergence, before any repair is attempted.
type Finding struct {
	Type     domain.IssueType
	Severity domain.IssueSeverity
	// Fingerprint identifies the underlying problem across runs.
	Fingerprint string
	Description string

	OrderID           *uuid.UUID
	PositionID        *uuid.UUID
	InstrumentID      string
	BrokerOrderID     string
	BrokerExecutionID string

	LocalEvidence  map[string]any
	BrokerEvidence map[string]any
	Evidence       map[string]any

	// Repairable carries the data an automatic repair needs, so the repair
	// step does not have to re-derive what classification already established.
	// Nil when the finding is not automatically repairable.
	Repair *RepairPlan
}

// RepairPlan is what an automatically-safe finding proposes to do.
type RepairPlan struct {
	Action domain.ResolutionAction
	// Execution is set for IMPORT_BROKER_FILL.
	Execution *broker.ExecutionReport
	// TargetStatus is set for a status repair.
	TargetStatus domain.OrderStatus
	RejectCode   domain.RejectCode
	Reason       string
}

// policySeverity applies the default severity for a type unless the caller
// escalates. A detector may raise severity on specific evidence; it may never
// lower it, because the code that found a problem must not also get to decide
// it does not matter.
func finding(t domain.IssueType) Finding {
	return Finding{Type: t, Severity: domain.PolicyFor(t).DefaultSeverity}
}

// Classify compares two snapshots and returns every divergence found.
//
// Order matters only for readability of the resulting list; the checks are
// independent. Each check is a separate method so a test can drive one in
// isolation.
func Classify(local LocalSnapshot, remote BrokerSnapshot, balanceTolerance decimal.Decimal) []Finding {
	var out []Finding

	executionFindings := classifyExecutions(local, remote)
	out = append(out, executionFindings...)

	// Orders whose quantity difference is already EXPLAINED by an importable
	// execution.
	//
	// Without this, one problem produces two issues that tell the operator
	// opposite things: FILL_MISSING_LOCALLY ("this is safe to import
	// automatically") and PARTIAL_FILL_MISMATCH ("nothing specific to book,
	// review it yourself"). The second is not merely redundant -- it is wrong,
	// because the missing execution IS the explanation, and it also keeps the
	// order flagged as uncertain after the import has resolved it.
	explained := map[uuid.UUID]bool{}
	for _, f := range executionFindings {
		if f.Type == domain.IssueFillMissingLocally && f.OrderID != nil {
			explained[*f.OrderID] = true
		}
	}

	out = append(out, classifyOrders(local, remote, explained)...)
	out = append(out, classifyUnknownOutcomes(local, remote)...)
	out = append(out, classifyVenueOnlyOrders(local, remote)...)
	out = append(out, classifyPositions(local, remote)...)
	out = append(out, classifyBalance(local, remote, balanceTolerance)...)
	return out
}

// classifyExecutions is the check that makes recovery possible.
//
// For every execution the venue reports, decide which of four things it is:
//
//	already booked                  -> nothing, or a duplicate-report note
//	attributable to exactly one order -> FILL_MISSING_LOCALLY, repairable
//	attributable to several orders    -> EXTRA_BROKER_FILL, operator
//	attributable to none              -> EXTRA_BROKER_FILL, operator
//
// Attribution is the whole question. It is answered from the venue's own
// broker_order_id and client_order_id, cross-checked against the order's
// instrument and side, and it either resolves to one order or it does not.
// "Probably that one" is not an outcome this function can produce.
func classifyExecutions(local LocalSnapshot, remote BrokerSnapshot) []Finding {
	var out []Finding

	// Track the latest execution timestamp seen per order, to detect a report
	// arriving out of order.
	latestSeen := map[string]int{}

	for i, exec := range remote.Executions {
		if exec.BrokerFillID == "" {
			// An execution with no identifier cannot be deduplicated, so it
			// can never be booked safely: a second sighting would be
			// indistinguishable from a second trade.
			f := finding(domain.IssueExtraBrokerFill)
			f.Severity = domain.SeverityCriticalIssue
			f.Fingerprint = store.Fingerprint(domain.IssueExtraBrokerFill,
				exec.BrokerOrderID, exec.Symbol, exec.Quantity.String(), exec.Price.String())
			f.Description = "The venue reported an execution with no execution identifier. " +
				"It cannot be booked, because without an identifier a replay could not be " +
				"told from a second trade."
			f.BrokerOrderID = exec.BrokerOrderID
			f.InstrumentID = exec.Symbol
			f.BrokerEvidence = executionEvidence(exec)
			out = append(out, f)
			continue
		}

		if local.FillIDs[exec.BrokerFillID] {
			// Already booked.
			//
			// This is the NORMAL, expected steady state, not an event. The
			// execution cursor deliberately overlaps by a minute so an
			// execution timestamped just before it is not skipped forever, so
			// every single poll re-sees recent executions. An earlier version
			// raised an info issue for each one, which meant a five-minute
			// reconciliation loop created a few dozen issues an hour for
			// nothing, reported them as "repaired", and made the run report's
			// repair count meaningless.
			//
			// What IS worth an operator's attention is the venue
			// CONTRADICTING itself: re-reporting the same execution id with a
			// different quantity or price. One of the two records is then
			// wrong about a trade that happened, and no amount of
			// deduplication resolves which.
			booked, known := local.BookedFills[exec.BrokerFillID]
			if !known {
				continue
			}
			if booked.Quantity.Equal(exec.Quantity) && booked.Price.Equal(exec.Price) &&
				booked.Side == exec.Side {
				continue
			}

			f := finding(domain.IssueDuplicateExecutionReport)
			f.Severity = domain.SeverityCriticalIssue
			f.Fingerprint = store.Fingerprint(domain.IssueDuplicateExecutionReport,
				exec.BrokerFillID)
			f.Description = fmt.Sprintf(
				"The venue re-reported execution %s with different content: it now says "+
					"%s %s @ %s, and Vantage booked %s %s @ %s. The execution id was "+
					"correctly refused as a duplicate, so nothing was written twice, but "+
					"one of the two records is wrong about a trade that happened, and "+
					"deduplication cannot say which.",
				exec.BrokerFillID, exec.Side, exec.Quantity, exec.Price,
				booked.Side, booked.Quantity, booked.Price)
			f.BrokerExecutionID = exec.BrokerFillID
			f.BrokerOrderID = exec.BrokerOrderID
			f.InstrumentID = exec.Symbol
			f.BrokerEvidence = executionEvidence(exec)
			f.LocalEvidence = map[string]any{
				"broker_execution_id": booked.BrokerFillID,
				"side":                string(booked.Side),
				"quantity":            booked.Quantity.String(),
				"price":               booked.Price.String(),
				"ingest_source":       booked.IngestSource,
			}
			out = append(out, f)
			continue
		}

		// Out-of-order detection, recorded before attribution because it is a
		// property of the stream rather than of any one order.
		if prev, ok := latestSeen[exec.BrokerOrderID]; ok {
			if exec.ExecutedAt.Before(remote.Executions[prev].ExecutedAt) {
				f := finding(domain.IssueOutOfOrderExecutionReport)
				f.Fingerprint = store.Fingerprint(domain.IssueOutOfOrderExecutionReport,
					exec.BrokerFillID)
				f.Description = fmt.Sprintf(
					"Execution %s arrived with a timestamp earlier than one already seen "+
						"for the same venue order. Booking is commutative, because the fill "+
						"aggregate is recomputed from the fills rather than incremented, "+
						"so this is recorded rather than treated as an error.",
					exec.BrokerFillID)
				f.BrokerExecutionID = exec.BrokerFillID
				f.BrokerOrderID = exec.BrokerOrderID
				f.InstrumentID = exec.Symbol
				f.BrokerEvidence = executionEvidence(exec)
				f.Repair = &RepairPlan{
					Action: domain.ActionAcknowledge,
					Reason: "arrival order does not affect the booked result",
				}
				out = append(out, f)
			}
		}
		latestSeen[exec.BrokerOrderID] = i

		candidates := attributionCandidates(local, exec)

		switch len(candidates) {
		case 1:
			order := candidates[0]
			f := finding(domain.IssueFillMissingLocally)
			f.Fingerprint = store.Fingerprint(domain.IssueFillMissingLocally, exec.BrokerFillID)
			f.Description = fmt.Sprintf(
				"The venue reports execution %s (%s %s @ %s) against order %s, which "+
					"Vantage has not booked. The execution's venue order id, instrument "+
					"and side all agree with that order, so it can be imported.",
				exec.BrokerFillID, exec.Side, exec.Quantity, exec.Price, order.ID)
			id := order.ID
			f.OrderID = &id
			f.InstrumentID = order.InstrumentID
			f.BrokerOrderID = exec.BrokerOrderID
			f.BrokerExecutionID = exec.BrokerFillID
			f.LocalEvidence = orderEvidence(order)
			f.BrokerEvidence = executionEvidence(exec)
			e := exec
			f.Repair = &RepairPlan{
				Action:    domain.ActionImportBrokerFill,
				Execution: &e,
				Reason:    "execution attributable to exactly one Vantage order",
			}
			out = append(out, f)

		case 0:
			f := finding(domain.IssueExtraBrokerFill)
			f.Fingerprint = store.Fingerprint(domain.IssueExtraBrokerFill, exec.BrokerFillID)
			f.Description = fmt.Sprintf(
				"The venue reports execution %s (%s %s %s @ %s) that cannot be attributed "+
					"to any Vantage order. Importing it would require guessing a parent "+
					"order, which would write a real position and a real profit or loss "+
					"against the wrong one.",
				exec.BrokerFillID, exec.Side, exec.Quantity, exec.Symbol, exec.Price)
			f.InstrumentID = exec.Symbol
			f.BrokerOrderID = exec.BrokerOrderID
			f.BrokerExecutionID = exec.BrokerFillID
			f.BrokerEvidence = executionEvidence(exec)
			out = append(out, f)

		default:
			f := finding(domain.IssueExtraBrokerFill)
			f.Fingerprint = store.Fingerprint(domain.IssueExtraBrokerFill, exec.BrokerFillID)
			ids := make([]string, 0, len(candidates))
			for _, c := range candidates {
				ids = append(ids, c.ID.String())
			}
			f.Description = fmt.Sprintf(
				"The venue reports execution %s which matches %d Vantage orders (%v). "+
					"Attribution is ambiguous, so an operator must establish which order "+
					"it belongs to before it is booked.",
				exec.BrokerFillID, len(candidates), ids)
			f.InstrumentID = exec.Symbol
			f.BrokerOrderID = exec.BrokerOrderID
			f.BrokerExecutionID = exec.BrokerFillID
			f.BrokerEvidence = executionEvidence(exec)
			f.Evidence = map[string]any{"candidate_order_ids": ids}
			out = append(out, f)
		}
	}
	return out
}

// attributionCandidates returns every local order an execution could belong to.
//
// The checks are conjunctive and deliberately strict:
//
//   - the venue order id must match, or the client order id must match
//   - the instrument must match
//   - the side must match
//
// The instrument and side checks are not redundant. A venue that reports the
// wrong side for a known order id is describing a mapping error, and booking
// it would move the position the wrong way; refusing to attribute it turns
// that into an operator-visible issue instead of a silent loss.
func attributionCandidates(local LocalSnapshot, exec broker.ExecutionReport) []domain.Order {
	var out []domain.Order
	seen := map[uuid.UUID]bool{}

	consider := func(o domain.Order) {
		if seen[o.ID] {
			return
		}
		if exec.Symbol != "" && o.InstrumentID != exec.Symbol && o.Symbol != exec.Symbol {
			return
		}
		if o.Side != exec.Side {
			return
		}
		seen[o.ID] = true
		out = append(out, o)
	}

	for _, o := range local.OrdersByBrokerID(exec.BrokerOrderID) {
		consider(o)
	}
	if exec.ClientOrderID != "" {
		if o, ok := local.OrderByCommandID(exec.ClientOrderID); ok {
			consider(o)
		}
	}
	return out
}

// classifyOrders compares orders both sides know about.
func classifyOrders(local LocalSnapshot, remote BrokerSnapshot,
	explained map[uuid.UUID]bool) []Finding {
	var out []Finding

	for _, order := range local.Orders {
		venue, found := lookupVenueOrder(order, remote)
		if !found {
			// An order the venue executed cannot also be an order the venue
			// never heard of. Checked independently of the snapshot's order
			// list because that list is filtered to OPEN orders, so a filled
			// order is legitimately absent from it -- and concluding "never
			// reached the market" from that absence would release the risk
			// budget for a position that exists.
			//
			// captureBroker resolves such orders by direct client-id lookup,
			// which is the primary defence. This is the second one, because the
			// consequence of getting it wrong is a real position going
			// unrecorded and the classification is cheap to assert here.
			if executionReferencesOrder(local, remote, order) {
				continue
			}
			out = append(out, classifyMissingAtBroker(order)...)
			continue
		}

		// An identifier the venue denies is more serious than a missing order:
		// every repair in this system relies on that mapping.
		if order.BrokerOrderID != nil && *order.BrokerOrderID != "" &&
			venue.BrokerOrderID != "" && *order.BrokerOrderID != venue.BrokerOrderID {
			f := finding(domain.IssueVenueIDMismatch)
			f.Fingerprint = store.Fingerprint(domain.IssueVenueIDMismatch, order.ID.String())
			f.Description = fmt.Sprintf(
				"Vantage holds venue order id %q for this order, but the venue reports %q "+
					"for the same client order id. Every repair relies on this mapping, so "+
					"none can be trusted on this connection while it is in doubt.",
				*order.BrokerOrderID, venue.BrokerOrderID)
			id := order.ID
			f.OrderID = &id
			f.InstrumentID = order.InstrumentID
			f.BrokerOrderID = venue.BrokerOrderID
			f.LocalEvidence = orderEvidence(order)
			f.BrokerEvidence = venueOrderEvidence(venue)
			out = append(out, f)
			continue
		}

		quantitiesAgree := venue.FilledQuantity.Equal(order.FilledQuantity)

		venueStatus, mappable := venue.Status.ToOrderStatus()
		if mappable && venueStatus != order.Status {
			// A status difference accompanied by a quantity difference is a
			// MISSING EXECUTION, not a labelling problem. Relabelling the
			// order would leave a FILLED order with no fills behind it, and
			// the position/fill invariant would no longer hold.
			//
			// This distinction is the reason status repair can be automatic at
			// all: it only ever runs when the numbers already agree.
			if !quantitiesAgree {
				if explained[order.ID] {
					// The missing execution is already reported as importable.
					// Adding a second, non-repairable issue for the same
					// problem would contradict it.
					continue
				}
				f := finding(domain.IssuePartialFillMismatch)
				f.Fingerprint = store.Fingerprint(domain.IssuePartialFillMismatch, order.ID.String())
				f.Description = fmt.Sprintf(
					"The venue reports this order as %s with %s filled; Vantage has %s "+
						"filled. The status cannot simply be adopted: that would leave an "+
						"order marked filled with no executions behind it. The repair is to "+
						"book the missing executions.",
					venue.Status, venue.FilledQuantity, order.FilledQuantity)
				id := order.ID
				f.OrderID = &id
				f.InstrumentID = order.InstrumentID
				f.LocalEvidence = orderEvidence(order)
				f.BrokerEvidence = venueOrderEvidence(venue)
				out = append(out, f)
				continue
			}

			f := finding(domain.IssueOrderStatusMismatch)
			f.Fingerprint = store.Fingerprint(domain.IssueOrderStatusMismatch, order.ID.String())
			repairable := domain.CanRepairTransition(order.Status, venueStatus)
			if !repairable {
				// The venue reports a state the repair machine will not move
				// to -- in practice, contradicting a terminal state Vantage has
				// already recorded. That is a contradiction to investigate.
				f.Severity = domain.SeverityCriticalIssue
				f.Description = fmt.Sprintf(
					"Vantage has this order as %s and the venue reports %s. That move is "+
						"not a legal repair, which means one of the two records is wrong "+
						"about a terminal outcome.",
					order.Status, venue.Status)
			} else {
				f.Description = fmt.Sprintf(
					"Vantage has this order as %s and the venue reports %s. Filled "+
						"quantities agree (%s), so the venue's status can be adopted: it is "+
						"authoritative about its own order states.",
					order.Status, venue.Status, order.FilledQuantity)
				code := domain.RejectCode("")
				if venueStatus == domain.OrderRejected {
					code = domain.RejectBrokerRejected
				}
				f.Repair = &RepairPlan{
					Action:       domain.ActionRecheck,
					TargetStatus: venueStatus,
					RejectCode:   code,
					Reason: fmt.Sprintf("venue reported %s and filled quantities agree",
						venue.Status),
				}
			}
			id := order.ID
			f.OrderID = &id
			f.InstrumentID = order.InstrumentID
			f.LocalEvidence = orderEvidence(order)
			f.BrokerEvidence = venueOrderEvidence(venue)
			out = append(out, f)
			continue
		}

		// Quantities disagree without a status difference, and no individual
		// execution in this snapshot explained it (that case became a
		// FILL_MISSING_LOCALLY above).
		if !quantitiesAgree && !explained[order.ID] {
			f := finding(domain.IssuePartialFillMismatch)
			f.Fingerprint = store.Fingerprint(domain.IssuePartialFillMismatch, order.ID.String())
			f.Description = fmt.Sprintf(
				"The venue reports %s filled on this order; Vantage has %s. No execution "+
					"in the venue's report accounts for the difference, so there is nothing "+
					"specific to book. The usual cause is a snapshot taken mid-execution, "+
					"which a re-check resolves.",
				venue.FilledQuantity, order.FilledQuantity)
			id := order.ID
			f.OrderID = &id
			f.InstrumentID = order.InstrumentID
			f.LocalEvidence = orderEvidence(order)
			f.BrokerEvidence = venueOrderEvidence(venue)
			out = append(out, f)
		}
	}
	return out
}

// executionReferencesOrder reports whether any execution in the venue snapshot
// belongs to a local order.
//
// Proof that the order reached the market, regardless of whether it appears in
// the venue's open-orders list.
func executionReferencesOrder(local LocalSnapshot, remote BrokerSnapshot, order domain.Order) bool {
	for _, exec := range remote.Executions {
		if exec.ClientOrderID != "" && exec.ClientOrderID == order.CommandID.String() {
			return true
		}
		if order.BrokerOrderID != nil && *order.BrokerOrderID != "" &&
			exec.BrokerOrderID == *order.BrokerOrderID {
			return true
		}
	}
	// A booked fill is equally good proof: the execution may predate the
	// snapshot's polling window, but Vantage would not hold a fill for an
	// order the venue never accepted.
	return len(local.FillsByOrder[order.ID]) > 0
}

// lookupVenueOrder resolves a local order in the venue snapshot, by venue id
// first and then by the client id Vantage supplied.
func lookupVenueOrder(order domain.Order, remote BrokerSnapshot) (broker.VenueOrder, bool) {
	if order.BrokerOrderID != nil && *order.BrokerOrderID != "" {
		if v, ok := remote.OrderByBrokerID(*order.BrokerOrderID); ok {
			return v, true
		}
	}
	// By client id. This is how an order whose acknowledgement was lost is
	// discovered to be live at the venue after all.
	return remote.OrderByClientID(order.CommandID.String())
}

// classifyMissingAtBroker handles an order the venue has no record of.
//
// The split here is the one the previous audit found the hard way. An order
// holding NO venue identifier, which the venue does not recognise by client
// id, provably never reached the market and can be closed out. An order that
// DOES hold a venue identifier the venue denies is a mapping failure, and
// closing it out would free the risk budget for a position that may well
// exist.
func classifyMissingAtBroker(order domain.Order) []Finding {
	hasVenueID := order.BrokerOrderID != nil && *order.BrokerOrderID != ""

	if hasVenueID {
		f := finding(domain.IssueVenueIDMismatch)
		f.Fingerprint = store.Fingerprint(domain.IssueVenueIDMismatch, order.ID.String())
		f.Description = fmt.Sprintf(
			"Vantage holds venue order id %q for this order and the venue has no record of "+
				"it. Closing it out would free the risk budget for a position that may "+
				"exist, so it is not repaired automatically.", *order.BrokerOrderID)
		id := order.ID
		f.OrderID = &id
		f.InstrumentID = order.InstrumentID
		f.BrokerOrderID = *order.BrokerOrderID
		f.LocalEvidence = orderEvidence(order)
		f.BrokerEvidence = map[string]any{"present": false}
		return []Finding{f}
	}

	f := finding(domain.IssueOrderMissingAtBroker)
	f.Fingerprint = store.Fingerprint(domain.IssueOrderMissingAtBroker, order.ID.String())
	f.Description = fmt.Sprintf(
		"This order holds no venue identifier and the venue does not recognise its client "+
			"id (%s), so it never reached the market. It is marked not-executed, which "+
			"releases the pending-order budget it was consuming.", order.CommandID)
	id := order.ID
	f.OrderID = &id
	f.InstrumentID = order.InstrumentID
	f.LocalEvidence = orderEvidence(order)
	f.BrokerEvidence = map[string]any{"present": false}
	f.Repair = &RepairPlan{
		Action:       domain.ActionMarkNotExecuted,
		TargetStatus: domain.OrderRejected,
		RejectCode:   domain.RejectReconciledAbsent,
		Reason:       "no venue identifier and the venue does not recognise the client id",
	}
	return []Finding{f}
}

// classifyUnknownOutcomes reports orders Vantage cannot resolve at all.
//
// An order flagged for reconciliation which the venue does not appear in
// either direction -- no order record, no execution -- is genuinely unknown. It
// is NOT collapsed into a tidy state: recording it as rejected would free risk
// budget for a position that may exist, and recording it as filled would
// invent one that may not.
func classifyUnknownOutcomes(local LocalSnapshot, remote BrokerSnapshot) []Finding {
	var out []Finding
	for _, order := range local.Orders {
		if !order.ReconciliationRequired {
			continue
		}
		if _, found := lookupVenueOrder(order, remote); found {
			// Covered by classifyOrders, which can say something specific.
			continue
		}
		if order.BrokerOrderID == nil || *order.BrokerOrderID == "" {
			// Covered by classifyMissingAtBroker, which can prove it never
			// reached the market.
			continue
		}
		f := finding(domain.IssueUnknownExecutionState)
		f.Fingerprint = store.Fingerprint(domain.IssueUnknownExecutionState, order.ID.String())
		f.Description = fmt.Sprintf(
			"Vantage cannot determine whether this order executed. It holds venue order id "+
				"%q, the venue's snapshot does not include it, and no execution references "+
				"it. This state is held open deliberately rather than resolved to a tidy "+
				"one, because both tidy answers are wrong in a way that costs money.",
			*order.BrokerOrderID)
		id := order.ID
		f.OrderID = &id
		f.InstrumentID = order.InstrumentID
		f.BrokerOrderID = *order.BrokerOrderID
		f.LocalEvidence = orderEvidence(order)
		f.BrokerEvidence = map[string]any{"present": false, "executions_referencing": 0}
		out = append(out, f)
	}
	return out
}

// classifyVenueOnlyOrders reports orders the venue is working that Vantage did
// not place.
//
// In a paper build with a single mock venue this should not happen, and it
// happening is a signal worth a critical issue. In a future world where a user
// can act in the broker's own terminal, this is what that looks like -- and
// Vantage must report it as external activity rather than adopt it as its own.
func classifyVenueOnlyOrders(local LocalSnapshot, remote BrokerSnapshot) []Finding {
	var out []Finding
	for _, venue := range remote.Orders {
		if _, ok := local.OrderByBrokerID(venue.BrokerOrderID); ok {
			continue
		}
		if venue.ClientOrderID != "" {
			if _, ok := local.OrderByCommandID(venue.ClientOrderID); ok {
				continue
			}
		}

		// A venue order carrying no client order id was not created by
		// Vantage: Vantage always supplies one. That is the signature of
		// external activity rather than of a lost record.
		t := domain.IssueOrderMissingLocally
		description := fmt.Sprintf(
			"The venue is working an order (%s %s %s, venue id %s) that Vantage has no "+
				"record of. Vantage cannot invent the risk decision, authority check and "+
				"intent that every order carries, so this is not adopted automatically.",
			venue.Side, venue.Quantity, venue.Symbol, venue.BrokerOrderID)
		if venue.ClientOrderID == "" {
			t = domain.IssueExternalBrokerActivity
			description = fmt.Sprintf(
				"The venue is working an order (%s %s %s, venue id %s) with no client "+
					"order id. Vantage supplies one on every order it places, so this "+
					"originated outside Vantage. It is reported as external activity and "+
					"never recorded as though Vantage had placed it.",
				venue.Side, venue.Quantity, venue.Symbol, venue.BrokerOrderID)
		}

		f := finding(t)
		f.Fingerprint = store.Fingerprint(t, venue.BrokerOrderID)
		f.Description = description
		f.InstrumentID = venue.Symbol
		f.BrokerOrderID = venue.BrokerOrderID
		f.BrokerEvidence = venueOrderEvidence(venue)
		f.LocalEvidence = map[string]any{"present": false}
		out = append(out, f)
	}
	return out
}

// classifyPositions compares the position books.
//
// Never repairable. A position is the consequence of executions, never an
// input: writing one to match the venue would leave a position that no
// sequence of fills explains, and the invariant "position quantity equals
// fill-derived quantity" would stop holding. The repair for a position
// difference is to find and book the missing executions, which is a different
// issue type.
func classifyPositions(local LocalSnapshot, remote BrokerSnapshot) []Finding {
	var out []Finding

	for _, position := range local.Positions {
		venue, found := remote.PositionBySymbol(position.InstrumentID)
		if !found {
			f := finding(domain.IssuePositionMismatch)
			f.Fingerprint = store.Fingerprint(domain.IssuePositionMismatch, position.InstrumentID)
			f.Description = fmt.Sprintf(
				"Vantage holds %s %s of %s and the venue reports flat. Risk sizing is "+
					"being computed against exposure that does not exist.",
				position.Side, position.Quantity, position.InstrumentID)
			id := position.ID
			f.PositionID = &id
			f.InstrumentID = position.InstrumentID
			f.LocalEvidence = positionEvidence(position)
			f.BrokerEvidence = map[string]any{"present": false, "quantity": "0"}
			out = append(out, f)
			continue
		}
		if venue.Side != position.Side || !venue.Quantity.Equal(position.Quantity) {
			f := finding(domain.IssuePositionMismatch)
			f.Fingerprint = store.Fingerprint(domain.IssuePositionMismatch, position.InstrumentID)
			f.Description = fmt.Sprintf(
				"Vantage holds %s %s of %s; the venue reports %s %s. A position is the "+
					"consequence of executions, so it is never written to match a "+
					"snapshot. The repair is to book the executions that explain the "+
					"difference.",
				position.Side, position.Quantity, position.InstrumentID,
				venue.Side, venue.Quantity)
			id := position.ID
			f.PositionID = &id
			f.InstrumentID = position.InstrumentID
			f.LocalEvidence = positionEvidence(position)
			f.BrokerEvidence = venuePositionEvidence(venue)
			out = append(out, f)
		}
	}

	for _, venue := range remote.Positions {
		if _, found := local.PositionByInstrument(venue.Symbol); found {
			continue
		}
		f := finding(domain.IssuePositionMismatch)
		f.Fingerprint = store.Fingerprint(domain.IssuePositionMismatch, venue.Symbol)
		f.Description = fmt.Sprintf(
			"The venue holds %s %s of %s and Vantage is flat. Either an execution was "+
				"missed, or the position was opened outside Vantage.",
			venue.Side, venue.Quantity, venue.Symbol)
		f.InstrumentID = venue.Symbol
		f.LocalEvidence = map[string]any{"present": false, "quantity": "0"}
		f.BrokerEvidence = venuePositionEvidence(venue)
		out = append(out, f)
	}
	return out
}

// classifyBalance compares the venue's balance with the ledger-derived one.
//
// Informational by policy, and that is a considered decision rather than
// leniency: in paper mode the venue simulates its own book and computes a
// balance its own way, so a small difference is expected by design. Halting
// automation on it would mean automation never runs. A LARGE difference is
// still worth an operator's attention, which is why the tolerance is an
// explicit parameter rather than an assumption.
func classifyBalance(local LocalSnapshot, remote BrokerSnapshot, tolerance decimal.Decimal) []Finding {
	if !remote.Balances || !local.BalanceKnown {
		return nil
	}
	if string(local.Account.Currency) != remote.Account.Currency {
		f := finding(domain.IssueBalanceMismatch)
		f.Severity = domain.SeverityCriticalIssue
		f.Fingerprint = store.Fingerprint(domain.IssueBalanceMismatch, "currency")
		f.Description = fmt.Sprintf(
			"The ledger is denominated in %s and the venue reports %s. No comparison is "+
				"meaningful across currencies, and converting one to the other would hide "+
				"a configuration error behind an exchange rate.",
			local.Account.Currency, remote.Account.Currency)
		f.LocalEvidence = map[string]any{"currency": string(local.Account.Currency)}
		f.BrokerEvidence = map[string]any{"currency": remote.Account.Currency}
		return []Finding{f}
	}

	diff := local.LedgerBalance.Sub(remote.Account.Balance).Abs()
	if diff.LessThanOrEqual(tolerance) {
		return nil
	}

	f := finding(domain.IssueBalanceMismatch)
	f.Fingerprint = store.Fingerprint(domain.IssueBalanceMismatch, "amount")
	f.Description = fmt.Sprintf(
		"The ledger balance is %s and the venue reports %s, a difference of %s against a "+
			"tolerance of %s. A balance is the running total of an append-only ledger, so "+
			"there is no way to correct it directly: the difference is either an unbooked "+
			"execution or a venue-side adjustment.",
		local.LedgerBalance.StringFixed(2), remote.Account.Balance.StringFixed(2),
		diff.StringFixed(2), tolerance.StringFixed(2))
	f.LocalEvidence = map[string]any{"balance": local.LedgerBalance.String()}
	f.BrokerEvidence = map[string]any{
		"balance": remote.Account.Balance.String(),
		"equity":  remote.Account.Equity.String(),
	}
	f.Evidence = map[string]any{"difference": diff.String(), "tolerance": tolerance.String()}
	return []Finding{f}
}

func positionEvidence(p domain.Position) map[string]any {
	return map[string]any{
		"position_id":     p.ID.String(),
		"instrument_id":   p.InstrumentID,
		"side":            string(p.Side),
		"quantity":        p.Quantity.String(),
		"avg_entry_price": p.AvgEntryPrice.String(),
		"status":          string(p.Status),
	}
}

func venuePositionEvidence(p broker.VenuePosition) map[string]any {
	return map[string]any{
		"symbol":          p.Symbol,
		"side":            string(p.Side),
		"quantity":        p.Quantity.String(),
		"avg_entry_price": p.AvgEntryPrice.String(),
		"unrealized_pnl":  p.UnrealizedPnL.String(),
	}
}
