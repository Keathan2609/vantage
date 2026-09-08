package domain

import "testing"

func TestCanTransition_LegalPaths(t *testing.T) {
	legal := []struct{ from, to OrderStatus }{
		{OrderCreated, OrderValidating},
		{OrderValidating, OrderAccepted},
		{OrderAccepted, OrderSubmitted},
		{OrderSubmitted, OrderPartiallyFilled},
		{OrderPartiallyFilled, OrderPartiallyFilled},
		{OrderPartiallyFilled, OrderFilled},
		{OrderSubmitted, OrderFilled},
		{OrderSubmitted, OrderCancelPending},
		{OrderCancelPending, OrderCancelled},
		// A cancel request races the venue; a fill may still land.
		{OrderCancelPending, OrderFilled},
		{OrderSubmitted, OrderFailed},
		// Reconciliation resolves an unknown broker-side outcome.
		{OrderFailed, OrderFilled},
		{OrderFailed, OrderCancelled},
		{OrderFailed, OrderRejected},
	}
	for _, c := range legal {
		if !CanTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be legal", c.from, c.to)
		}
	}
}

func TestCanTransition_IllegalPaths(t *testing.T) {
	illegal := []struct{ from, to OrderStatus }{
		// Terminal states are terminal.
		{OrderFilled, OrderSubmitted},
		{OrderFilled, OrderCancelled},
		{OrderCancelled, OrderSubmitted},
		{OrderRejected, OrderAccepted},
		{OrderExpired, OrderFilled},
		// Cannot skip validation or submission.
		{OrderCreated, OrderFilled},
		{OrderCreated, OrderSubmitted},
		{OrderValidating, OrderSubmitted},
		{OrderAccepted, OrderFilled},
		// Cannot un-fill or move backwards.
		{OrderFilled, OrderPartiallyFilled},
		{OrderSubmitted, OrderAccepted},
		{OrderPartiallyFilled, OrderSubmitted},
		// Unknown states are refused rather than defaulted.
		{OrderStatus("BOGUS"), OrderFilled},
		{OrderCreated, OrderStatus("BOGUS")},
	}
	for _, c := range illegal {
		if CanTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be ILLEGAL", c.from, c.to)
		}
	}
}

func TestOrderStatus_TerminalAndOpen(t *testing.T) {
	terminal := []OrderStatus{OrderFilled, OrderRejected, OrderCancelled, OrderExpired}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
		if s.Open() {
			t.Errorf("%s should not be open", s)
		}
	}
	// FAILED means "outcome unknown", so it is neither terminal nor open: it
	// must be resolved by reconciliation.
	if OrderFailed.Terminal() {
		t.Error("FAILED must not be terminal; reconciliation can still resolve it")
	}
	open := []OrderStatus{OrderCreated, OrderValidating, OrderAccepted, OrderSubmitted,
		OrderPartiallyFilled, OrderCancelPending}
	for _, s := range open {
		if !s.Open() {
			t.Errorf("%s should be open", s)
		}
	}
}

func TestTerminalStatesHaveNoOutwardTransitions(t *testing.T) {
	for status, targets := range legalTransitions {
		if status.Terminal() && len(targets) != 0 {
			t.Errorf("terminal status %s has %d outward transitions", status, len(targets))
		}
	}
}

func TestOrderTypeRequirements(t *testing.T) {
	if !OrderTypeLimit.RequiresLimitPrice() || OrderTypeMarket.RequiresLimitPrice() {
		t.Error("limit price requirement is wrong")
	}
	if !OrderTypeStop.RequiresStopPrice() || OrderTypeLimit.RequiresStopPrice() {
		t.Error("stop price requirement is wrong")
	}
	if !OrderTypeStopLimit.RequiresLimitPrice() || !OrderTypeStopLimit.RequiresStopPrice() {
		t.Error("stop-limit requires both prices")
	}
	if OrderType("iceberg").Valid() {
		t.Error("unknown order types must not validate")
	}
}

func TestRoleSeparationOfDuties(t *testing.T) {
	// Admins administer; traders trade. An admin session must not be able to
	// move money, so that compromising the admin surface does not equal
	// compromising the account.
	if RoleAdmin.CanTrade() {
		t.Error("admin role must not be able to trade")
	}
	if !RoleTrader.CanTrade() {
		t.Error("trader role must be able to trade")
	}
	if RoleViewer.CanTrade() {
		t.Error("viewer role must not be able to trade")
	}
	if RoleTrader.CanAdminister() || RoleViewer.CanAdminister() {
		t.Error("only admin may administer")
	}
}
