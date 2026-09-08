package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/metrics"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/reconcile"
)

type riskLimitsResponse struct {
	AccountID                string    `json:"account_id"`
	Currency                 string    `json:"currency"`
	MaxOrderQuantity         string    `json:"max_order_quantity"`
	MaxOrderNotional         string    `json:"max_order_notional"`
	MaxRiskPerTradeFraction  string    `json:"max_risk_per_trade_fraction"`
	RequireStopLoss          bool      `json:"require_stop_loss"`
	MaxOpenPositions         int       `json:"max_open_positions"`
	MaxPendingOrders         int       `json:"max_pending_orders"`
	MaxGrossExposure         string    `json:"max_gross_exposure"`
	MaxNetExposure           string    `json:"max_net_exposure"`
	MaxPerInstrumentExposure string    `json:"max_per_instrument_exposure"`
	MaxConcentrationFraction string    `json:"max_concentration_fraction"`
	MaxLeverage              string    `json:"max_leverage"`
	MaxDailyLoss             string    `json:"max_daily_loss"`
	MaxDrawdownFraction      string    `json:"max_drawdown_fraction"`
	MaxSpreadFraction        string    `json:"max_spread_fraction"`
	MaxSlippageFraction      string    `json:"max_slippage_fraction"`
	EventBlackoutBefore      int       `json:"event_blackout_before_minutes"`
	EventBlackoutAfter       int       `json:"event_blackout_after_minutes"`
	BlockOnHighImpactEvents  bool      `json:"block_on_high_impact_events"`
	Version                  int64     `json:"version"`
	UpdatedAt                time.Time `json:"updated_at"`
}

func toRiskLimitsResponse(l domain.RiskLimits) riskLimitsResponse {
	return riskLimitsResponse{
		AccountID: l.AccountID.String(), Currency: string(l.Currency),
		MaxOrderQuantity:        l.MaxOrderQuantity.String(),
		MaxOrderNotional:        l.MaxOrderNotional.StringFixed(),
		MaxRiskPerTradeFraction: l.MaxRiskPerTradeFraction.String(),
		RequireStopLoss:         l.RequireStopLoss,
		MaxOpenPositions:        l.MaxOpenPositions, MaxPendingOrders: l.MaxPendingOrders,
		MaxGrossExposure:         l.MaxGrossExposure.StringFixed(),
		MaxNetExposure:           l.MaxNetExposure.StringFixed(),
		MaxPerInstrumentExposure: l.MaxPerInstrumentExposure.StringFixed(),
		MaxConcentrationFraction: l.MaxConcentrationFraction.String(),
		MaxLeverage:              l.MaxLeverage.String(),
		MaxDailyLoss:             l.MaxDailyLoss.StringFixed(),
		MaxDrawdownFraction:      l.MaxDrawdownFraction.String(),
		MaxSpreadFraction:        l.MaxSpreadFraction.String(),
		MaxSlippageFraction:      l.MaxSlippageFraction.String(),
		EventBlackoutBefore:      l.EventBlackoutBeforeMinutes,
		EventBlackoutAfter:       l.EventBlackoutAfterMinutes,
		BlockOnHighImpactEvents:  l.BlockOnHighImpactEvents,
		Version:                  l.Version, UpdatedAt: l.UpdatedAt,
	}
}

// handleGetRiskLimits returns an account's ceilings and how much is used.
func (s *Server) handleGetRiskLimits(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	limits, err := s.store.Control.RiskLimitsForAccount(r.Context(), account.ID)
	if err != nil {
		writeStoreError(w, r, err, "Risk limits not found for this account.")
		return
	}
	snapshot, err := s.portfolio.Compute(r.Context(), account)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}

	// Utilisation is what an operator actually reads: not the limit, but how
	// close the account is to it.
	utilisation := map[string]string{}
	frac := func(used, limit decimal.Decimal) string {
		if !limit.IsPositive() {
			return "0"
		}
		return used.Div(limit).StringFixed(4)
	}
	st := snapshot.State
	utilisation["gross_exposure"] = frac(st.GrossExposure.Decimal(), limits.MaxGrossExposure.Decimal())
	utilisation["net_exposure"] = frac(st.NetExposure.Abs().Decimal(), limits.MaxNetExposure.Decimal())
	utilisation["open_positions"] = frac(
		decimal.NewFromInt(int64(st.OpenPositions)), decimal.NewFromInt(int64(limits.MaxOpenPositions)))
	utilisation["pending_orders"] = frac(
		decimal.NewFromInt(int64(st.PendingOrders)), decimal.NewFromInt(int64(limits.MaxPendingOrders)))
	utilisation["drawdown"] = frac(st.DrawdownFraction(), limits.MaxDrawdownFraction)

	dayLoss := decimal.Zero
	if st.DayPnL().IsNegative() {
		dayLoss = st.DayPnL().Abs().Decimal()
	}
	utilisation["daily_loss"] = frac(dayLoss, limits.MaxDailyLoss.Decimal())

	for name, value := range utilisation {
		if f, err := decimal.NewFromString(value); err == nil {
			f64, _ := f.Float64()
			metrics.RiskUtilisation.WithLabelValues(account.ID.String(), name).Set(f64)
		}
	}
	metrics.AccountEquity.WithLabelValues(account.ID.String(), string(account.Currency)).
		Set(mustFloat(st.Equity.Decimal()))
	metrics.AccountDrawdown.WithLabelValues(account.ID.String()).Set(mustFloat(st.DrawdownFraction()))

	writeJSON(w, r, http.StatusOK, map[string]any{
		"limits":      toRiskLimitsResponse(limits),
		"utilisation": utilisation,
		"simulated":   s.cfg.SimulatedFunds(),
	})
}

type updateRiskLimitsRequest struct {
	MaxOrderQuantity         string `json:"max_order_quantity"`
	MaxOrderNotional         string `json:"max_order_notional"`
	MaxRiskPerTradeFraction  string `json:"max_risk_per_trade_fraction"`
	RequireStopLoss          bool   `json:"require_stop_loss"`
	MaxOpenPositions         int    `json:"max_open_positions"`
	MaxPendingOrders         int    `json:"max_pending_orders"`
	MaxGrossExposure         string `json:"max_gross_exposure"`
	MaxNetExposure           string `json:"max_net_exposure"`
	MaxPerInstrumentExposure string `json:"max_per_instrument_exposure"`
	MaxConcentrationFraction string `json:"max_concentration_fraction"`
	MaxLeverage              string `json:"max_leverage"`
	MaxDailyLoss             string `json:"max_daily_loss"`
	MaxDrawdownFraction      string `json:"max_drawdown_fraction"`
	MaxSpreadFraction        string `json:"max_spread_fraction"`
	MaxSlippageFraction      string `json:"max_slippage_fraction"`
	EventBlackoutBefore      int    `json:"event_blackout_before_minutes"`
	EventBlackoutAfter       int    `json:"event_blackout_after_minutes"`
	BlockOnHighImpactEvents  bool   `json:"block_on_high_impact_events"`
	Version                  int64  `json:"version"`
}

// handleUpdateRiskLimits replaces an account's ceilings.
//
// Widening a risk limit is one of the most consequential non-trading actions
// available, so it is audited with its previous value, subject to the database's
// own absolute ceilings, and rejected outright if it would exceed them.
func (s *Server) handleUpdateRiskLimits(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	var req updateRiskLimitsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	existing, err := s.store.Control.RiskLimitsForAccount(r.Context(), account.ID)
	if err != nil {
		writeStoreError(w, r, err, "Risk limits not found for this account.")
		return
	}
	if req.Version != 0 && req.Version != existing.Version {
		writeError(w, r, http.StatusConflict, "stale_version",
			"These limits changed since you loaded them. Reload and try again.")
		return
	}

	v := newValidation()
	dec := func(field, raw string, fallback decimal.Decimal) decimal.Decimal {
		if raw == "" {
			return fallback
		}
		d, err := parseDecimal(raw)
		if err != nil {
			v.add(field, "must be a decimal number")
			return fallback
		}
		return d
	}
	amt := func(field, raw string, fallback money.Amount) money.Amount {
		if raw == "" {
			return fallback
		}
		d, err := parseDecimal(raw)
		if err != nil {
			v.add(field, "must be a decimal number")
			return fallback
		}
		return money.New(d, account.Currency)
	}

	limits := existing
	limits.MaxOrderQuantity = dec("max_order_quantity", req.MaxOrderQuantity, existing.MaxOrderQuantity)
	limits.MaxOrderNotional = amt("max_order_notional", req.MaxOrderNotional, existing.MaxOrderNotional)
	limits.MaxRiskPerTradeFraction = dec("max_risk_per_trade_fraction",
		req.MaxRiskPerTradeFraction, existing.MaxRiskPerTradeFraction)
	limits.RequireStopLoss = req.RequireStopLoss
	if req.MaxOpenPositions > 0 {
		limits.MaxOpenPositions = req.MaxOpenPositions
	}
	if req.MaxPendingOrders > 0 {
		limits.MaxPendingOrders = req.MaxPendingOrders
	}
	limits.MaxGrossExposure = amt("max_gross_exposure", req.MaxGrossExposure, existing.MaxGrossExposure)
	limits.MaxNetExposure = amt("max_net_exposure", req.MaxNetExposure, existing.MaxNetExposure)
	limits.MaxPerInstrumentExposure = amt("max_per_instrument_exposure",
		req.MaxPerInstrumentExposure, existing.MaxPerInstrumentExposure)
	limits.MaxConcentrationFraction = dec("max_concentration_fraction",
		req.MaxConcentrationFraction, existing.MaxConcentrationFraction)
	limits.MaxLeverage = dec("max_leverage", req.MaxLeverage, existing.MaxLeverage)
	limits.MaxDailyLoss = amt("max_daily_loss", req.MaxDailyLoss, existing.MaxDailyLoss)
	limits.MaxDrawdownFraction = dec("max_drawdown_fraction",
		req.MaxDrawdownFraction, existing.MaxDrawdownFraction)
	limits.MaxSpreadFraction = dec("max_spread_fraction", req.MaxSpreadFraction, existing.MaxSpreadFraction)
	limits.MaxSlippageFraction = dec("max_slippage_fraction",
		req.MaxSlippageFraction, existing.MaxSlippageFraction)
	if req.EventBlackoutBefore >= 0 {
		limits.EventBlackoutBeforeMinutes = req.EventBlackoutBefore
	}
	if req.EventBlackoutAfter >= 0 {
		limits.EventBlackoutAfterMinutes = req.EventBlackoutAfter
	}
	limits.BlockOnHighImpactEvents = req.BlockOnHighImpactEvents

	// Application-level guard rails, ahead of the database's own constraints,
	// so the user gets a sentence rather than a constraint name.
	if limits.MaxRiskPerTradeFraction.GreaterThan(decimal.RequireFromString("0.10")) {
		v.add("max_risk_per_trade_fraction",
			"cannot exceed 10% of equity per trade; Vantage will not take more risk than that on one position")
	}
	if !limits.MaxRiskPerTradeFraction.IsPositive() {
		v.add("max_risk_per_trade_fraction", "must be greater than zero")
	}
	if limits.MaxDrawdownFraction.GreaterThan(decimal.RequireFromString("1")) {
		v.add("max_drawdown_fraction", "cannot exceed 100%")
	}
	if !v.ok() {
		v.write(w, r)
		return
	}

	updated, err := s.store.Control.UpsertRiskLimits(r.Context(), limits, p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "Risk limits not found for this account.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditRiskLimitsUpdated, domain.AuditSuccess, map[string]any{
		"account_id":                  account.ID.String(),
		"max_risk_per_trade_fraction": updated.MaxRiskPerTradeFraction.String(),
		"max_daily_loss":              updated.MaxDailyLoss.String(),
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"limits": toRiskLimitsResponse(updated)})
}

// handleRiskEvents returns recent risk occurrences.
func (s *Server) handleRiskEvents(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	events, err := s.store.Control.ListRiskEvents(r.Context(), account.ID, intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"events": events})
}

type authorityResponse struct {
	ID                  string     `json:"id"`
	AccountID           string     `json:"account_id"`
	Mode                string     `json:"mode"`
	Active              bool       `json:"active"`
	AutomationEnabled   bool       `json:"automation_enabled"`
	AllowedInstruments  []string   `json:"allowed_instruments"`
	AllowedStrategyIDs  []string   `json:"allowed_strategy_ids"`
	AllowedOrderTypes   []string   `json:"allowed_order_types"`
	MaxOrderQuantity    string     `json:"max_order_quantity"`
	MaxOrderNotional    string     `json:"max_order_notional"`
	MaxPositionExposure string     `json:"max_position_exposure"`
	MaxLeverage         string     `json:"max_leverage"`
	MaxDailyLoss        string     `json:"max_daily_loss"`
	Currency            string     `json:"currency"`
	ValidFrom           time.Time  `json:"valid_from"`
	ValidUntil          *time.Time `json:"valid_until,omitempty"`
	RevokedAt           *time.Time `json:"revoked_at,omitempty"`
	RevocationReason    *string    `json:"revocation_reason,omitempty"`
	Version             int64      `json:"version"`
	// Notice is returned with every authority so no interface can present it
	// as a legal permission. See docs/REGULATORY_BOUNDARY.md.
	Notice string `json:"notice"`
}

const authorityNotice = "Trading authority is a technical control that limits what Vantage may do " +
	"on this account. It is not a legal or regulatory authorisation."

func toAuthorityResponse(a domain.TradingAuthority) authorityResponse {
	strategies := make([]string, 0, len(a.AllowedStrategyIDs))
	for _, id := range a.AllowedStrategyIDs {
		strategies = append(strategies, id.String())
	}
	types := make([]string, 0, len(a.AllowedOrderTypes))
	for _, t := range a.AllowedOrderTypes {
		types = append(types, string(t))
	}
	instruments := a.AllowedInstruments
	if instruments == nil {
		instruments = []string{}
	}
	return authorityResponse{
		ID: a.ID.String(), AccountID: a.AccountID.String(), Mode: string(a.Mode),
		Active: a.Active, AutomationEnabled: a.AutomationEnabled,
		AllowedInstruments: instruments, AllowedStrategyIDs: strategies, AllowedOrderTypes: types,
		MaxOrderQuantity:    a.MaxOrderQuantity.String(),
		MaxOrderNotional:    a.MaxOrderNotional.StringFixed(),
		MaxPositionExposure: a.MaxPositionExposure.StringFixed(),
		MaxLeverage:         a.MaxLeverage.String(), MaxDailyLoss: a.MaxDailyLoss.StringFixed(),
		Currency:  string(a.MaxDailyLoss.Currency()),
		ValidFrom: a.ValidFrom, ValidUntil: a.ValidUntil,
		RevokedAt: a.RevokedAt, RevocationReason: a.RevocationReason,
		Version: a.Version, Notice: authorityNotice,
	}
}

// handleGetAuthority returns the active mandate for an account.
func (s *Server) handleGetAuthority(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	authority, err := s.store.Control.ActiveAuthorityForAccount(r.Context(), account.ID)
	if err != nil {
		if storeErr(err) {
			writeJSON(w, r, http.StatusOK, map[string]any{
				"authority": nil,
				"notice":    authorityNotice,
				"message":   "No active trading authority exists for this account. Automated and manual orders will be refused.",
			})
			return
		}
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"authority": toAuthorityResponse(authority)})
}

type createAuthorityRequest struct {
	AccountID           string   `json:"account_id"`
	AutomationEnabled   bool     `json:"automation_enabled"`
	AllowedInstruments  []string `json:"allowed_instruments"`
	AllowedStrategyIDs  []string `json:"allowed_strategy_ids"`
	AllowedOrderTypes   []string `json:"allowed_order_types"`
	MaxOrderQuantity    string   `json:"max_order_quantity"`
	MaxOrderNotional    string   `json:"max_order_notional"`
	MaxPositionExposure string   `json:"max_position_exposure"`
	MaxLeverage         string   `json:"max_leverage"`
	MaxDailyLoss        string   `json:"max_daily_loss"`
	ValidUntil          *string  `json:"valid_until,omitempty"`
}

// handleCreateAuthority grants a scoped trading mandate.
func (s *Server) handleCreateAuthority(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req createAuthorityRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	v := newValidation()
	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		v.add("account_id", "must be a valid identifier")
		v.write(w, r)
		return
	}
	account, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, accountID)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}

	// An empty allow-list is refused rather than stored: it would create an
	// authority that permits nothing, which is confusing rather than safe.
	if len(req.AllowedInstruments) == 0 {
		v.add("allowed_instruments", "list at least one instrument this authority covers")
	}
	if len(req.AllowedOrderTypes) == 0 {
		v.add("allowed_order_types", "list at least one order type this authority permits")
	}
	orderTypes := make([]domain.OrderType, 0, len(req.AllowedOrderTypes))
	for _, t := range req.AllowedOrderTypes {
		ot := domain.OrderType(t)
		if !ot.Valid() {
			v.add("allowed_order_types", "contains an unknown order type: "+t)
			continue
		}
		orderTypes = append(orderTypes, ot)
	}
	for _, id := range req.AllowedInstruments {
		if _, err := s.store.Market.Instrument(r.Context(), id); err != nil {
			v.add("allowed_instruments", "unknown instrument: "+id)
		}
	}
	strategyIDs := make([]uuid.UUID, 0, len(req.AllowedStrategyIDs))
	for _, raw := range req.AllowedStrategyIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			v.add("allowed_strategy_ids", "contains an invalid identifier")
			continue
		}
		strategyIDs = append(strategyIDs, id)
	}

	mustDec := func(field, raw string) decimal.Decimal {
		d, err := parseDecimal(raw)
		if err != nil {
			v.add(field, "must be a decimal number")
			return decimal.Zero
		}
		if !d.IsPositive() {
			v.add(field, "must be greater than zero")
		}
		return d
	}
	maxQty := mustDec("max_order_quantity", req.MaxOrderQuantity)
	maxNotional := mustDec("max_order_notional", req.MaxOrderNotional)
	maxExposure := mustDec("max_position_exposure", req.MaxPositionExposure)
	maxLeverage := mustDec("max_leverage", req.MaxLeverage)
	maxDailyLoss := mustDec("max_daily_loss", req.MaxDailyLoss)

	var validUntil *time.Time
	if req.ValidUntil != nil && *req.ValidUntil != "" {
		t, err := time.Parse(time.RFC3339, *req.ValidUntil)
		if err != nil {
			v.add("valid_until", "must be an RFC3339 timestamp")
		} else if !t.After(s.clock.Now()) {
			v.add("valid_until", "must be in the future")
		} else {
			validUntil = &t
		}
	}
	if !v.ok() {
		v.write(w, r)
		return
	}

	authority := domain.TradingAuthority{
		UserID: p.User.ID, AccountID: account.ID, Mode: domain.ModePaper,
		Active: true, AutomationEnabled: req.AutomationEnabled,
		AllowedInstruments:  req.AllowedInstruments,
		AllowedStrategyIDs:  strategyIDs,
		AllowedOrderTypes:   orderTypes,
		MaxOrderQuantity:    maxQty,
		MaxOrderNotional:    money.New(maxNotional, account.Currency),
		MaxPositionExposure: money.New(maxExposure, account.Currency),
		MaxLeverage:         maxLeverage,
		MaxDailyLoss:        money.New(maxDailyLoss, account.Currency),
		ValidFrom:           s.clock.Now(),
		ValidUntil:          validUntil,
	}

	created, err := s.store.Control.CreateAuthority(r.Context(), authority, p.User.ID)
	if err != nil {
		writeStoreError(w, r, err,
			"Could not create the authority. An active authority may already exist for this account.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditAuthorityCreated, domain.AuditSuccess, map[string]any{
		"authority_id": created.ID.String(), "account_id": account.ID.String(),
		"automation_enabled": created.AutomationEnabled,
		"instruments":        created.AllowedInstruments,
	})
	writeJSON(w, r, http.StatusCreated, map[string]any{"authority": toAuthorityResponse(created)})
}

type updateAuthorityRequest struct {
	AutomationEnabled  *bool    `json:"automation_enabled,omitempty"`
	AllowedInstruments []string `json:"allowed_instruments,omitempty"`
	AllowedStrategyIDs []string `json:"allowed_strategy_ids,omitempty"`
	Version            int64    `json:"version"`
}

// handleUpdateAuthority narrows or adjusts an existing mandate.
func (s *Server) handleUpdateAuthority(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	authorityID, ok := parseUUID(w, r, chi.URLParam(r, "authorityID"), "Authority id")
	if !ok {
		return
	}
	var req updateAuthorityRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	existing, err := s.store.Control.AuthorityForUser(r.Context(), p.User.ID, authorityID)
	if err != nil {
		writeStoreError(w, r, err, "Authority not found.")
		return
	}
	if req.Version != 0 && req.Version != existing.Version {
		writeError(w, r, http.StatusConflict, "stale_version",
			"This authority changed since you loaded it. Reload and try again.")
		return
	}

	updated := existing
	if req.AutomationEnabled != nil {
		updated.AutomationEnabled = *req.AutomationEnabled
	}
	if req.AllowedInstruments != nil {
		for _, id := range req.AllowedInstruments {
			if _, err := s.store.Market.Instrument(r.Context(), id); err != nil {
				writeError(w, r, http.StatusUnprocessableEntity, "unknown_instrument",
					"Unknown instrument: "+id)
				return
			}
		}
		updated.AllowedInstruments = req.AllowedInstruments
	}
	if req.AllowedStrategyIDs != nil {
		ids := make([]uuid.UUID, 0, len(req.AllowedStrategyIDs))
		for _, raw := range req.AllowedStrategyIDs {
			id, err := uuid.Parse(raw)
			if err != nil {
				writeError(w, r, http.StatusUnprocessableEntity, "invalid_identifier",
					"allowed_strategy_ids contains an invalid identifier.")
				return
			}
			ids = append(ids, id)
		}
		updated.AllowedStrategyIDs = ids
	}

	result, err := s.store.Control.UpdateAuthority(r.Context(), updated, p.User.ID, existing.Version)
	if err != nil {
		writeStoreError(w, r, err, "Authority not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditAuthorityUpdated, domain.AuditSuccess, map[string]any{
		"authority_id": result.ID.String(), "automation_enabled": result.AutomationEnabled,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"authority": toAuthorityResponse(result)})
}

type revokeAuthorityRequest struct {
	Reason string `json:"reason"`
}

// handleRevokeAuthority ends a mandate immediately.
func (s *Server) handleRevokeAuthority(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	authorityID, ok := parseUUID(w, r, chi.URLParam(r, "authorityID"), "Authority id")
	if !ok {
		return
	}
	var req revokeAuthorityRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Reason == "" {
		req.Reason = "revoked by user"
	}
	if _, err := s.store.Control.AuthorityForUser(r.Context(), p.User.ID, authorityID); err != nil {
		writeStoreError(w, r, err, "Authority not found.")
		return
	}
	if err := s.store.Control.RevokeAuthority(r.Context(), authorityID, p.User.ID, req.Reason); err != nil {
		writeStoreError(w, r, err, "Authority not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditAuthorityRevoked, domain.AuditSuccess, map[string]any{
		"authority_id": authorityID.String(), "reason": req.Reason,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status":  "revoked",
		"message": "Trading authority revoked. New orders on this account will be refused until a new authority is granted.",
	})
}

type killSwitchRequest struct {
	Scope    string  `json:"scope"`
	TargetID *string `json:"target_id,omitempty"`
	Reason   string  `json:"reason"`
}

// handleActivateKillSwitch halts new orders within a scope.
func (s *Server) handleActivateKillSwitch(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req killSwitchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	scope := domain.KillSwitchScope(req.Scope)
	if !scope.Valid() {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_scope",
			"Scope must be global, user, account, broker or strategy.")
		return
	}
	if req.Reason == "" {
		writeError(w, r, http.StatusUnprocessableEntity, "reason_required",
			"Give a reason: it is recorded in the audit log and shown to anyone whose trading is halted.")
		return
	}
	if scope == domain.KillScopeGlobal && req.TargetID != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_target",
			"A global kill switch has no target.")
		return
	}
	if scope != domain.KillScopeGlobal && (req.TargetID == nil || *req.TargetID == "") {
		writeError(w, r, http.StatusUnprocessableEntity, "target_required",
			"This scope requires a target identifier.")
		return
	}
	// A user may only halt their own accounts; only an admin may halt globally
	// or halt someone else.
	if scope == domain.KillScopeGlobal && !p.User.Role.CanAdminister() {
		writeError(w, r, http.StatusForbidden, "forbidden",
			"Only an administrator may activate a global kill switch.")
		return
	}
	if scope == domain.KillScopeAccount {
		id, err := uuid.Parse(*req.TargetID)
		if err != nil {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_target",
				"target_id must be a valid account identifier.")
			return
		}
		if _, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, id); err != nil {
			writeStoreError(w, r, err, "Account not found.")
			return
		}
	}

	ks, err := s.store.Control.ActivateKillSwitch(r.Context(), scope, req.TargetID, req.Reason, p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "Could not activate the kill switch.")
		return
	}
	metrics.KillSwitchActive.WithLabelValues(string(scope)).Set(1)
	s.auditAuth(r, &p.User.ID, domain.AuditKillSwitchActivated, domain.AuditSuccess, map[string]any{
		"scope": string(scope), "target_id": req.TargetID, "reason": req.Reason,
	})
	logging.FromContext(r.Context()).Warn("kill switch activated",
		"scope", string(scope), "reason", req.Reason, "user_id", p.User.ID.String())

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"kill_switch": ks,
		"message": "New orders are halted within this scope. Open positions are NOT closed: " +
			"use Flatten on a position to close it deliberately.",
	})
}

// handleDeactivateKillSwitch resumes trading within a scope.
func (s *Server) handleDeactivateKillSwitch(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, ok := parseUUID(w, r, chi.URLParam(r, "killSwitchID"), "Kill switch id")
	if !ok {
		return
	}
	if err := s.store.Control.DeactivateKillSwitch(r.Context(), id, p.User.ID); err != nil {
		writeStoreError(w, r, err, "Active kill switch not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditKillSwitchDeactivated, domain.AuditSuccess,
		map[string]any{"kill_switch_id": id.String()})
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "deactivated"})
}

// handleListKillSwitches shows active and historical switches.
func (s *Server) handleListKillSwitches(w http.ResponseWriter, r *http.Request) {
	switches, err := s.store.Control.ListKillSwitches(r.Context(), r.URL.Query().Get("active") == "true")
	if err != nil {
		writeStoreError(w, r, err, "No kill switches found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"kill_switches": switches})
}

type tradingEnabledRequest struct {
	Enabled bool   `json:"enabled"`
	Version int64  `json:"version"`
	Reason  string `json:"reason"`
}

// handleSetTradingEnabled suspends or resumes trading on an account.
func (s *Server) handleSetTradingEnabled(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	var req tradingEnabledRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	version := req.Version
	if version == 0 {
		version = account.Version
	}
	if err := s.store.Accounts.SetTradingEnabled(r.Context(), account.ID, req.Enabled, version); err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditAdminAction, domain.AuditSuccess, map[string]any{
		"action": "set_trading_enabled", "account_id": account.ID.String(),
		"enabled": req.Enabled, "reason": req.Reason,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{
		"account_id": account.ID.String(), "trading_enabled": req.Enabled,
	})
}

// handleRunReconciliation compares Vantage's records with the venue's.
func (s *Server) handleRunReconciliation(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	report, err := s.reconciler.Run(r.Context(), account, reconcile.TriggerManual)
	if err != nil {
		logging.FromContext(r.Context()).Error("reconciliation failed", "error", err.Error())
		writeError(w, r, http.StatusServiceUnavailable, "reconciliation_failed",
			"Reconciliation could not complete. The venue may be unreachable.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditReconciliationRun, domain.AuditSuccess, map[string]any{
		"account_id": account.ID.String(), "status": report.Status,
		"discrepancies": len(report.Discrepancies),
	})
	writeJSON(w, r, http.StatusOK, map[string]any{
		"run_id":             report.RunID.String(),
		"status":             report.Status,
		"orders_compared":    report.OrdersCompared,
		"positions_compared": report.PositionsCompared,
		"discrepancies":      report.Discrepancies,
		"clean":              report.Clean(),
		"critical":           report.CriticalCount(),
	})
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// handleAdminListUsers lists users. Password hashes and MFA material are never
// serialised: the response type has no field for them.
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Users.ListUsers(r.Context())
	if err != nil {
		writeStoreError(w, r, err, "No users found.")
		return
	}
	type adminUser struct {
		userResponse
		Disabled    bool       `json:"disabled"`
		LastLoginAt *time.Time `json:"last_login_at,omitempty"`
		CreatedAt   time.Time  `json:"created_at"`
	}
	out := make([]adminUser, 0, len(users))
	for _, u := range users {
		out = append(out, adminUser{
			userResponse: toUserResponse(u), Disabled: u.Disabled,
			LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"users": out})
}

type setRoleRequest struct {
	Role string `json:"role"`
}

// handleAdminSetRole changes a user's role.
func (s *Server) handleAdminSetRole(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	userID, ok := parseUUID(w, r, chi.URLParam(r, "userID"), "User id")
	if !ok {
		return
	}
	var req setRoleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	role := domain.Role(req.Role)
	if !role.Valid() {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_role",
			"Role must be admin, trader or viewer.")
		return
	}
	// An admin cannot change their own role: that is how the last administrator
	// accidentally locks everyone out of administration.
	if userID == p.User.ID {
		writeError(w, r, http.StatusForbidden, "self_modification",
			"You cannot change your own role. Ask another administrator.")
		return
	}
	if err := s.store.Users.SetRole(r.Context(), userID, role); err != nil {
		writeStoreError(w, r, err, "User not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditRoleChanged, domain.AuditSuccess, map[string]any{
		"target_user_id": userID.String(), "new_role": string(role),
	})
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "role_changed"})
}

type setDisabledRequest struct {
	Disabled bool   `json:"disabled"`
	Reason   string `json:"reason"`
}

// handleAdminSetDisabled enables or disables a user.
func (s *Server) handleAdminSetDisabled(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	userID, ok := parseUUID(w, r, chi.URLParam(r, "userID"), "User id")
	if !ok {
		return
	}
	var req setDisabledRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if userID == p.User.ID {
		writeError(w, r, http.StatusForbidden, "self_modification",
			"You cannot disable your own account.")
		return
	}
	if err := s.store.Users.SetDisabled(r.Context(), userID, req.Disabled); err != nil {
		writeStoreError(w, r, err, "User not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditUserDisabled, domain.AuditSuccess, map[string]any{
		"target_user_id": userID.String(), "disabled": req.Disabled, "reason": req.Reason,
	})
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "updated"})
}

// handleVerifyAuditChain recomputes the audit hash chain.
//
// This is the check that gives the audit log its value: it detects any edit,
// deletion or reordering after the fact. It reports the first broken link
// rather than only a boolean, so an investigation has somewhere to start.
func (s *Server) handleVerifyAuditChain(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.Control.AuditChainSlice(r.Context(), 1, 10000)
	if err != nil {
		writeStoreError(w, r, err, "Audit chain unavailable.")
		return
	}
	ok, brokenAt := domain.VerifyChain(events)
	headHash, headSeq, herr := s.store.Control.AuditHead(r.Context())
	if herr != nil {
		writeStoreError(w, r, herr, "Audit chain unavailable.")
		return
	}

	resp := map[string]any{
		"verified":       ok,
		"events_checked": len(events),
		"head_sequence":  headSeq,
		"head_hash":      headHash,
		"note": "Hash chaining provides tamper EVIDENCE, not immutability. " +
			"A party with database write access can rewrite history, but cannot do so " +
			"without breaking this chain.",
	}
	if !ok {
		resp["broken_at_index"] = brokenAt
		if brokenAt >= 0 && brokenAt < len(events) {
			resp["broken_at_sequence"] = events[brokenAt].Sequence
		}
		logging.FromContext(r.Context()).Error("AUDIT CHAIN VERIFICATION FAILED",
			"broken_at_index", brokenAt)
		writeJSON(w, r, http.StatusConflict, resp)
		return
	}
	writeJSON(w, r, http.StatusOK, resp)
}

func mustFloat(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}
