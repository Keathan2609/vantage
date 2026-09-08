package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/broker"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/oms"
	"github.com/vantage/control-api/internal/store"
)

type placeOrderRequest struct {
	AccountID    string  `json:"account_id"`
	InstrumentID string  `json:"instrument_id"`
	Side         string  `json:"side"`
	Type         string  `json:"type"`
	Quantity     string  `json:"quantity"`
	LimitPrice   *string `json:"limit_price,omitempty"`
	StopPrice    *string `json:"stop_price,omitempty"`
	StopLoss     *string `json:"stop_loss,omitempty"`
	TakeProfit   *string `json:"take_profit,omitempty"`
	TimeInForce  string  `json:"time_in_force"`
}

type orderResponse struct {
	ID             string     `json:"id"`
	AccountID      string     `json:"account_id"`
	InstrumentID   string     `json:"instrument_id"`
	Symbol         string     `json:"symbol"`
	Mode           string     `json:"mode"`
	Simulated      bool       `json:"simulated"`
	Side           string     `json:"side"`
	Type           string     `json:"type"`
	TimeInForce    string     `json:"time_in_force"`
	Status         string     `json:"status"`
	Quantity       string     `json:"quantity"`
	FilledQuantity string     `json:"filled_quantity"`
	AvgFillPrice   string     `json:"avg_fill_price"`
	LimitPrice     *string    `json:"limit_price,omitempty"`
	StopPrice      *string    `json:"stop_price,omitempty"`
	StopLoss       *string    `json:"stop_loss,omitempty"`
	TakeProfit     *string    `json:"take_profit,omitempty"`
	Source         string     `json:"source"`
	StrategyID     *string    `json:"strategy_id,omitempty"`
	BrokerName     string     `json:"broker_name"`
	BrokerOrderID  *string    `json:"broker_order_id,omitempty"`
	RejectCode     *string    `json:"reject_code,omitempty"`
	RejectReason   *string    `json:"reject_reason,omitempty"`
	Version        int64      `json:"version"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
}

func toOrderResponse(o domain.Order) orderResponse {
	dp := func(d *decimal.Decimal) *string {
		if d == nil {
			return nil
		}
		s := d.String()
		return &s
	}
	var strategyID *string
	if o.StrategyID != nil {
		s := o.StrategyID.String()
		strategyID = &s
	}
	return orderResponse{
		ID: o.ID.String(), AccountID: o.AccountID.String(), InstrumentID: o.InstrumentID,
		Symbol: o.Symbol, Mode: string(o.Mode),
		// Carried on every order so no client can present a paper fill as real
		// by omitting the mode.
		Simulated: o.Mode != domain.ModeLive,
		Side:      string(o.Side), Type: string(o.Type), TimeInForce: string(o.TimeInForce),
		Status: string(o.Status), Quantity: o.Quantity.String(),
		FilledQuantity: o.FilledQuantity.String(), AvgFillPrice: o.AvgFillPrice.String(),
		LimitPrice: dp(o.LimitPrice), StopPrice: dp(o.StopPrice),
		StopLoss: dp(o.StopLoss), TakeProfit: dp(o.TakeProfit),
		Source: string(o.Source), StrategyID: strategyID,
		BrokerName: o.BrokerName, BrokerOrderID: o.BrokerOrderID,
		RejectCode: o.RejectCode, RejectReason: o.RejectReason,
		Version: o.Version, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
		SubmittedAt: o.SubmittedAt, ClosedAt: o.ClosedAt,
	}
}

type fillResponse struct {
	ID         string    `json:"id"`
	Quantity   string    `json:"quantity"`
	Price      string    `json:"price"`
	Commission string    `json:"commission"`
	Currency   string    `json:"commission_currency"`
	ExecutedAt time.Time `json:"executed_at"`
}

type placeOrderResponse struct {
	Order     *orderResponse        `json:"order,omitempty"`
	Fills     []fillResponse        `json:"fills,omitempty"`
	Rejection *rejectionResponse    `json:"rejection,omitempty"`
	Risk      *riskDecisionResponse `json:"risk,omitempty"`
	Duplicate bool                  `json:"duplicate"`
	Simulated bool                  `json:"simulated"`
}

type rejectionResponse struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Detail  map[string]string `json:"detail,omitempty"`
}

type riskCheckResponse struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Limit    string `json:"limit"`
	Observed string `json:"observed"`
	Message  string `json:"message"`
}

type riskDecisionResponse struct {
	Approved         bool                `json:"approved"`
	ApprovedQuantity string              `json:"approved_quantity"`
	Checks           []riskCheckResponse `json:"checks"`
}

func toRiskDecisionResponse(d domain.RiskDecision) *riskDecisionResponse {
	if len(d.Checks) == 0 {
		return nil
	}
	checks := make([]riskCheckResponse, 0, len(d.Checks))
	for _, c := range d.Checks {
		checks = append(checks, riskCheckResponse{
			Name: string(c.Name), Passed: c.Passed, Limit: c.Limit,
			Observed: c.Observed, Message: c.Message,
		})
	}
	return &riskDecisionResponse{
		Approved: d.Approved, ApprovedQuantity: d.ApprovedQuantity.String(), Checks: checks,
	}
}

// handlePlaceOrder submits an order through the full pipeline.
//
// The Idempotency-Key header is REQUIRED. Without one, a client that retries
// after a timeout cannot be distinguished from a client deliberately placing a
// second order, and the safe interpretation of that ambiguity does not exist.
func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 {
		writeError(w, r, http.StatusBadRequest, "idempotency_key_required",
			"Every order must carry an Idempotency-Key header of 8 to 200 characters.")
		return
	}

	var req placeOrderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	v := newValidation()
	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		v.add("account_id", "must be a valid identifier")
	}
	side := domain.OrderSide(req.Side)
	if !side.Valid() {
		v.add("side", "must be buy or sell")
	}
	orderType := domain.OrderType(req.Type)
	if !orderType.Valid() {
		v.add("type", "must be market, limit, stop or stop_limit")
	}
	tif := domain.TimeInForce(req.TimeInForce)
	if req.TimeInForce == "" {
		tif = domain.TIFGoodTilCancelled
	} else if !tif.Valid() {
		v.add("time_in_force", "must be gtc, ioc, fok or day")
	}
	quantity, err := parseDecimal(req.Quantity)
	if err != nil {
		v.add("quantity", "must be a decimal number")
	} else if !quantity.IsPositive() {
		v.add("quantity", "must be greater than zero")
	}
	if req.InstrumentID == "" || len(req.InstrumentID) > 32 {
		v.add("instrument_id", "is required")
	}

	optional := func(field string, raw *string) *decimal.Decimal {
		if raw == nil || *raw == "" {
			return nil
		}
		d, err := parseDecimal(*raw)
		if err != nil {
			v.add(field, "must be a decimal number")
			return nil
		}
		if !d.IsPositive() {
			v.add(field, "must be greater than zero")
			return nil
		}
		return &d
	}
	limitPrice := optional("limit_price", req.LimitPrice)
	stopPrice := optional("stop_price", req.StopPrice)
	stopLoss := optional("stop_loss", req.StopLoss)
	takeProfit := optional("take_profit", req.TakeProfit)

	if !v.ok() {
		v.write(w, r)
		return
	}

	ip := clientIP(r)
	ua := r.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}

	result, err := s.oms.PlaceOrder(r.Context(), oms.PlaceOrderRequest{
		IdempotencyKey: idempotencyKey,
		ActorUserID:    p.User.ID,
		ActorRole:      p.User.Role,
		AccountID:      accountID,
		InstrumentID:   req.InstrumentID,
		Side:           side,
		Type:           orderType,
		Quantity:       quantity,
		LimitPrice:     limitPrice,
		StopPrice:      stopPrice,
		StopLoss:       stopLoss,
		TakeProfit:     takeProfit,
		TimeInForce:    tif,
		Source:         domain.SourceManual,
		RequestHash: oms.HashRequest(accountID, req.InstrumentID, side, orderType, quantity,
			limitPrice, stopPrice, stopLoss, takeProfit, tif),
		IPAddress:     &ip,
		UserAgent:     &ua,
		RequestID:     logging.RequestID(r.Context()),
		CorrelationID: logging.CorrelationID(r.Context()),
	})

	switch {
	case errors.Is(err, oms.ErrIdempotencyConflict):
		writeError(w, r, http.StatusConflict, "idempotency_conflict",
			"This Idempotency-Key was already used with different order parameters. "+
				"Use a new key, or resend the original request exactly.")
		return
	case errors.Is(err, oms.ErrCommandInFlight):
		writeError(w, r, http.StatusConflict, "command_in_flight",
			"An identical order is still being processed. Retry shortly to read its outcome.")
		return
	case errors.Is(err, oms.ErrIdempotencyKeyRequired):
		writeError(w, r, http.StatusBadRequest, "idempotency_key_required",
			"Every order must carry an Idempotency-Key header.")
		return
	case err != nil:
		logging.FromContext(r.Context()).Error("order placement failed", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"The order could not be processed.")
		return
	}

	resp := placeOrderResponse{
		Duplicate: result.Duplicate,
		Simulated: s.cfg.SimulatedFunds(),
		Risk:      toRiskDecisionResponse(result.Decision),
	}
	if result.Order != nil {
		o := toOrderResponse(*result.Order)
		resp.Order = &o
	}
	for _, f := range result.Fills {
		resp.Fills = append(resp.Fills, fillResponse{
			ID: f.ID.String(), Quantity: f.Quantity.String(), Price: f.Price.String(),
			Commission: f.Commission.String(), Currency: f.CommissionCcy, ExecutedAt: f.ExecutedAt,
		})
	}

	if result.Rejection != nil {
		resp.Rejection = &rejectionResponse{
			Code: string(result.Rejection.Code), Message: result.Rejection.Message,
			Detail: result.Rejection.Detail,
		}
		writeJSON(w, r, rejectionStatus(result.Rejection.Code), resp)
		return
	}

	status := http.StatusCreated
	if result.Duplicate {
		// A duplicate is not an error and not a new creation: the original
		// outcome is returned unchanged.
		status = http.StatusOK
	}
	writeJSON(w, r, status, resp)
}

// handleCancelOrder requests cancellation of a resting order.
func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	orderID, ok := parseUUID(w, r, chi.URLParam(r, "orderID"), "Order id")
	if !ok {
		return
	}

	order, err := s.store.Trading.OrderForUser(r.Context(), p.User.ID, orderID)
	if err != nil {
		writeStoreError(w, r, err, "Order not found.")
		return
	}
	if !order.Status.Open() {
		writeError(w, r, http.StatusConflict, "order_not_open",
			"This order is no longer open and cannot be cancelled.")
		return
	}

	account, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, order.AccountID)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	adapter, err := s.brokers.Get(account.BrokerName)
	if err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "broker_unavailable",
			"No adapter is available for this broker.")
		return
	}
	if order.BrokerOrderID == nil {
		writeError(w, r, http.StatusConflict, "order_not_at_venue",
			"This order has no venue identifier yet. Retry once it has been submitted.")
		return
	}

	accountRef := account.ID.String()
	if account.BrokerAcctRef != nil && *account.BrokerAcctRef != "" {
		accountRef = *account.BrokerAcctRef
	}

	// Mark the intent before contacting the venue, so a crash mid-cancel leaves
	// a visible CANCEL_PENDING rather than an order that silently stayed live.
	pending, err := s.transitionOrder(r, order, domain.OrderCancelPending, "cancellation requested", &p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "Order not found.")
		return
	}

	ack, err := adapter.CancelOrder(r.Context(), broker.CancelOrderRequest{
		ClientRequestID: uuid.NewString(),
		AccountRef:      accountRef,
		BrokerOrderID:   *order.BrokerOrderID,
	})
	if err != nil {
		logging.FromContext(r.Context()).Error("cancel failed at venue",
			"order_id", order.ID.String(), "error", err.Error())
		writeError(w, r, http.StatusServiceUnavailable, "cancel_failed",
			"The venue did not confirm the cancellation. The order's state will be reconciled.")
		return
	}

	// The order may have filled between the request and its arrival. That is a
	// race the venue wins, and it is reported rather than papered over.
	if ack.AlreadyFilled {
		filled, ferr := s.transitionOrder(r, pending, domain.OrderFilled,
			"venue reported the order filled before cancellation arrived", &p.User.ID)
		if ferr == nil {
			resp := toOrderResponse(filled)
			writeJSON(w, r, http.StatusConflict, map[string]any{
				"order":   resp,
				"message": "This order filled before the cancellation reached the venue.",
			})
			return
		}
	}

	cancelled, err := s.transitionOrder(r, pending, domain.OrderCancelled,
		"cancelled at venue", &p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "Order not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditOrderCancelled, domain.AuditSuccess,
		map[string]any{"order_id": cancelled.ID.String()})
	writeJSON(w, r, http.StatusOK, map[string]any{"order": toOrderResponse(cancelled)})
}

// transitionOrder applies a state change with optimistic concurrency.
func (s *Server) transitionOrder(r *http.Request, order domain.Order, to domain.OrderStatus,
	reason string, actor *uuid.UUID) (domain.Order, error) {

	var out domain.Order
	err := s.store.Pool().InTx(r.Context(), func(tx pgx.Tx) error {
		current, err := s.store.Trading.OrderByIDTx(r.Context(), tx, order.ID)
		if err != nil {
			return err
		}
		out, err = s.store.Trading.TransitionOrderTx(r.Context(), tx, current.ID, current.Version,
			to, reason, "user", actor)
		return err
	})
	return out, err
}

type flattenRequest struct {
	Confirm bool `json:"confirm"`
}

// handleFlattenPosition closes an open position at market.
//
// Flatten is deliberately SEPARATE from the kill switch. A kill switch stops
// new orders; it does not liquidate, because automatic liquidation on an alarm
// dumps positions into exactly the conditions that raised the alarm. Closing
// positions is an explicit, separately authorised, confirmed action.
func (s *Server) handleFlattenPosition(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	positionID, ok := parseUUID(w, r, chi.URLParam(r, "positionID"), "Position id")
	if !ok {
		return
	}

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if len(idempotencyKey) < 8 {
		writeError(w, r, http.StatusBadRequest, "idempotency_key_required",
			"Flattening a position requires an Idempotency-Key header.")
		return
	}

	var req flattenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if !req.Confirm {
		writeError(w, r, http.StatusUnprocessableEntity, "confirmation_required",
			"Flattening closes the position at the current market price. "+
				"Resend with confirm set to true to proceed.")
		return
	}

	accounts, err := s.store.Accounts.ListAccountsForUser(r.Context(), p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}

	var position domain.Position
	var account domain.Account
	found := false
	for _, a := range accounts {
		positions, perr := s.store.Trading.OpenPositions(r.Context(), a.ID)
		if perr != nil {
			continue
		}
		for _, pos := range positions {
			if pos.ID == positionID {
				position, account, found = pos, a, true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		writeError(w, r, http.StatusNotFound, "not_found", "Open position not found.")
		return
	}

	s.auditAuth(r, &p.User.ID, domain.AuditFlattenRequested, domain.AuditSuccess, map[string]any{
		"position_id": position.ID.String(), "symbol": position.Symbol,
		"quantity": position.Quantity.String(),
	})

	// A flatten is an ordinary market order in the opposite direction, and it
	// goes through the same pipeline as any other order. It is exempt from
	// nothing: if the market is closed, it is refused, exactly as a manual
	// close would be.
	result, err := s.oms.PlaceOrder(r.Context(), oms.PlaceOrderRequest{
		IdempotencyKey: idempotencyKey,
		ActorUserID:    p.User.ID,
		ActorRole:      p.User.Role,
		AccountID:      account.ID,
		InstrumentID:   position.InstrumentID,
		Side:           position.Side.Opposite(),
		Type:           domain.OrderTypeMarket,
		Quantity:       position.Quantity,
		TimeInForce:    domain.TIFGoodTilCancelled,
		Source:         domain.SourceRiskControl,
		RequestHash: oms.HashRequest(account.ID, position.InstrumentID, position.Side.Opposite(),
			domain.OrderTypeMarket, position.Quantity, nil, nil, nil, nil, domain.TIFGoodTilCancelled),
		RequestID:     logging.RequestID(r.Context()),
		CorrelationID: logging.CorrelationID(r.Context()),
	})
	if err != nil {
		logging.FromContext(r.Context()).Error("flatten failed", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"The position could not be closed.")
		return
	}

	resp := placeOrderResponse{Duplicate: result.Duplicate, Simulated: s.cfg.SimulatedFunds()}
	if result.Order != nil {
		o := toOrderResponse(*result.Order)
		resp.Order = &o
	}
	for _, f := range result.Fills {
		resp.Fills = append(resp.Fills, fillResponse{
			ID: f.ID.String(), Quantity: f.Quantity.String(), Price: f.Price.String(),
			Commission: f.Commission.String(), Currency: f.CommissionCcy, ExecutedAt: f.ExecutedAt,
		})
	}
	if result.Rejection != nil {
		resp.Rejection = &rejectionResponse{
			Code: string(result.Rejection.Code), Message: result.Rejection.Message,
			Detail: result.Rejection.Detail,
		}
		writeJSON(w, r, rejectionStatus(result.Rejection.Code), resp)
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditFlattenExecuted, domain.AuditSuccess, map[string]any{
		"position_id": position.ID.String(),
	})
	writeJSON(w, r, http.StatusOK, resp)
}

// handleListOrders returns the caller's orders.
func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	f := store.OrderFilter{Limit: intParam(r, "limit", 100, 500), Offset: intParam(r, "offset", 0, 100000)}

	if raw := r.URL.Query().Get("account_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_identifier", "account_id is not valid.")
			return
		}
		f.AccountID = &id
	}
	if raw := r.URL.Query().Get("instrument_id"); raw != "" {
		f.InstrumentID = &raw
	}
	if r.URL.Query().Get("open") == "true" {
		f.OpenOnly = true
	}

	orders, err := s.store.Trading.ListOrdersForUser(r.Context(), p.User.ID, f)
	if err != nil {
		writeStoreError(w, r, err, "No orders found.")
		return
	}
	out := make([]orderResponse, 0, len(orders))
	for _, o := range orders {
		out = append(out, toOrderResponse(o))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"orders": out, "simulated": s.cfg.SimulatedFunds()})
}

// handleGetOrder returns one order with its fills and transition history.
func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	orderID, ok := parseUUID(w, r, chi.URLParam(r, "orderID"), "Order id")
	if !ok {
		return
	}
	order, err := s.store.Trading.OrderForUser(r.Context(), p.User.ID, orderID)
	if err != nil {
		writeStoreError(w, r, err, "Order not found.")
		return
	}
	fills, err := s.store.Trading.FillsForOrder(r.Context(), order.ID)
	if err != nil {
		writeStoreError(w, r, err, "Order not found.")
		return
	}
	transitions, err := s.store.Trading.OrderTransitions(r.Context(), order.ID)
	if err != nil {
		writeStoreError(w, r, err, "Order not found.")
		return
	}

	fillOut := make([]fillResponse, 0, len(fills))
	for _, f := range fills {
		fillOut = append(fillOut, fillResponse{
			ID: f.ID.String(), Quantity: f.Quantity.String(), Price: f.Price.String(),
			Commission: f.Commission.String(), Currency: f.CommissionCcy, ExecutedAt: f.ExecutedAt,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"order":       toOrderResponse(order),
		"fills":       fillOut,
		"transitions": transitions,
		"simulated":   s.cfg.SimulatedFunds(),
	})
}

type positionResponse struct {
	ID            string  `json:"id"`
	AccountID     string  `json:"account_id"`
	InstrumentID  string  `json:"instrument_id"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Quantity      string  `json:"quantity"`
	AvgEntryPrice string  `json:"avg_entry_price"`
	CurrentPrice  string  `json:"current_price"`
	StopLoss      *string `json:"stop_loss,omitempty"`
	TakeProfit    *string `json:"take_profit,omitempty"`
	UnrealizedPnL string  `json:"unrealized_pnl"`
	UnrealizedCcy string  `json:"unrealized_pnl_currency"`
	NotionalValue string  `json:"notional_value"`
	MarginUsed    string  `json:"margin_used"`
	// Valued is false when the position could not be priced. Clients must show
	// that state rather than rendering a zero, which would understate risk.
	Valued        bool      `json:"valued"`
	ValuationNote string    `json:"valuation_note,omitempty"`
	StrategyID    *string   `json:"strategy_id,omitempty"`
	OpenedAt      time.Time `json:"opened_at"`
	Simulated     bool      `json:"simulated"`
}

// handleListPositions returns open positions for one account or all of them.
func (s *Server) handleListPositions(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())

	accounts, err := s.store.Accounts.ListAccountsForUser(r.Context(), p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "No accounts found.")
		return
	}
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_identifier", "account_id is not valid.")
			return
		}
		filtered := accounts[:0]
		for _, a := range accounts {
			if a.ID == id {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	out := []positionResponse{}
	for _, account := range accounts {
		snapshot, err := s.portfolio.Compute(r.Context(), account)
		if err != nil {
			logging.FromContext(r.Context()).Error("portfolio computation failed",
				"account_id", account.ID.String(), "error", err.Error())
			continue
		}
		for _, view := range snapshot.Positions {
			pos := view.Position
			var strategyID *string
			if pos.StrategyID != nil {
				sid := pos.StrategyID.String()
				strategyID = &sid
			}
			dp := func(d *decimal.Decimal) *string {
				if d == nil {
					return nil
				}
				s := d.String()
				return &s
			}
			out = append(out, positionResponse{
				ID: pos.ID.String(), AccountID: pos.AccountID.String(),
				InstrumentID: pos.InstrumentID, Symbol: pos.Symbol,
				Side: string(pos.Side), Quantity: pos.Quantity.String(),
				AvgEntryPrice: pos.AvgEntryPrice.String(),
				CurrentPrice:  view.CurrentPrice.String(),
				StopLoss:      dp(pos.StopLoss), TakeProfit: dp(pos.TakeProfit),
				UnrealizedPnL: view.UnrealizedPnL.StringFixed(),
				UnrealizedCcy: string(account.Currency),
				NotionalValue: view.NotionalValue.StringFixed(),
				MarginUsed:    view.MarginUsed.StringFixed(),
				Valued:        view.Valued, ValuationNote: view.ValuationNote,
				StrategyID: strategyID, OpenedAt: pos.OpenedAt,
				Simulated: account.Mode != domain.ModeLive,
			})
		}
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"positions": out, "simulated": s.cfg.SimulatedFunds(),
	})
}

// intParam reads a bounded integer query parameter.
func intParam(r *http.Request, name string, def, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
