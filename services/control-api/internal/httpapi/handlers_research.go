package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/orchestrator"
	"github.com/vantage/control-api/internal/quant"
	"github.com/vantage/control-api/internal/store"
)

// handleListStrategies returns the strategy library with recent activity.
func (s *Server) handleListStrategies(w http.ResponseWriter, r *http.Request) {
	strategies, err := s.store.Research.ListStrategies(r.Context())
	if err != nil {
		writeStoreError(w, r, err, "No strategies found.")
		return
	}
	activity, err := s.store.Research.StrategyActivity(r.Context(), s.clock.Now().Add(-7*24*time.Hour))
	if err != nil {
		logging.FromContext(r.Context()).Warn("strategy activity unavailable", "error", err.Error())
		activity = nil
	}

	type strategyRow struct {
		ID           string     `json:"id"`
		Key          string     `json:"key"`
		Name         string     `json:"name"`
		Family       string     `json:"family"`
		Description  string     `json:"description"`
		HighRisk     bool       `json:"high_risk"`
		Enabled      bool       `json:"enabled"`
		Lifecycle    string     `json:"lifecycle"`
		Version      int        `json:"version"`
		Timeframe    string     `json:"timeframe"`
		Instruments  []string   `json:"instruments"`
		Runs7d       int        `json:"runs_7d"`
		Signals7d    int        `json:"signals_7d"`
		Failures7d   int        `json:"failures_7d"`
		LastRunAt    *time.Time `json:"last_run_at,omitempty"`
		LastSignalAt *time.Time `json:"last_signal_at,omitempty"`
	}

	out := make([]strategyRow, 0, len(strategies))
	for _, st := range strategies {
		row := strategyRow{
			ID: st.ID.String(), Key: st.Key, Name: st.Name, Family: string(st.Family),
			Description: st.Description, HighRisk: st.HighRisk, Enabled: st.Enabled,
			Lifecycle: string(domain.LifecycleDraft),
		}
		if v, err := s.store.Research.LatestStrategyVersion(r.Context(), st.ID); err == nil {
			row.Lifecycle = string(v.Lifecycle)
			row.Version = v.Version
			row.Timeframe = string(v.Timeframe)
			row.Instruments = v.Instruments
		}
		if a, ok := activity[st.ID]; ok {
			row.Runs7d, row.Signals7d, row.Failures7d = a.Runs, a.Signals, a.Failures
			row.LastRunAt, row.LastSignalAt = a.LastRunAt, a.LastSignalAt
		}
		out = append(out, row)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"strategies": out,
		"note": "Paper is the highest lifecycle stage this build supports. " +
			"Demo and live promotion are not available.",
	})
}

// handleGetStrategy returns one strategy with its versions and backtests.
func (s *Server) handleGetStrategy(w http.ResponseWriter, r *http.Request) {
	strategyID, ok := parseUUID(w, r, chi.URLParam(r, "strategyID"), "Strategy id")
	if !ok {
		return
	}
	strategy, err := s.store.Research.Strategy(r.Context(), strategyID)
	if err != nil {
		writeStoreError(w, r, err, "Strategy not found.")
		return
	}
	versions, err := s.store.Research.ListStrategyVersions(r.Context(), strategyID)
	if err != nil {
		writeStoreError(w, r, err, "Strategy not found.")
		return
	}
	backtests, err := s.store.Research.ListBacktests(r.Context(), &strategyID, 20)
	if err != nil {
		logging.FromContext(r.Context()).Warn("backtests unavailable", "error", err.Error())
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"strategy": strategy, "versions": versions, "backtests": backtests,
	})
}

type promoteStrategyRequest struct {
	Version int    `json:"version"`
	ToStage string `json:"to_stage"`
	Reason  string `json:"reason"`
}

// handlePromoteStrategy advances a strategy version's lifecycle.
//
// Promotion is evidence-gated: advancing to BACKTESTED requires a stored
// backtest, and advancing to VALIDATED requires an out-of-sample one. The
// ceiling is PAPER, enforced here, in the domain, and by a database constraint.
func (s *Server) handlePromoteStrategy(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	strategyID, ok := parseUUID(w, r, chi.URLParam(r, "strategyID"), "Strategy id")
	if !ok {
		return
	}
	var req promoteStrategyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	version, err := s.store.Research.StrategyVersion(r.Context(), strategyID, req.Version)
	if err != nil {
		writeStoreError(w, r, err, "Strategy version not found.")
		return
	}
	to := domain.StrategyLifecycle(req.ToStage)
	allowed, reason := domain.CanPromote(version.Lifecycle, to)
	if !allowed {
		writeError(w, r, http.StatusUnprocessableEntity, "promotion_refused", reason)
		return
	}

	// Evidence requirements.
	backtests, err := s.store.Research.ListBacktests(r.Context(), &strategyID, 100)
	if err != nil {
		writeStoreError(w, r, err, "Strategy not found.")
		return
	}
	relevant := backtests[:0]
	for _, b := range backtests {
		if b.StrategyVersion == req.Version && b.Status == "completed" {
			relevant = append(relevant, b)
		}
	}

	evidence := map[string]any{"backtests": len(relevant)}
	switch to {
	case domain.LifecycleBacktested:
		if len(relevant) == 0 {
			writeError(w, r, http.StatusUnprocessableEntity, "evidence_required",
				"Run a backtest for this version before promoting it to BACKTESTED.")
			return
		}
	case domain.LifecycleValidated:
		hasOOS := false
		for _, b := range relevant {
			if b.SampleKind == "out_of_sample" || b.SampleKind == "walk_forward" {
				hasOOS = true
				evidence["out_of_sample_backtest_id"] = b.ID.String()
				evidence["metrics"] = json.RawMessage(b.Metrics)
				break
			}
		}
		if !hasOOS {
			writeError(w, r, http.StatusUnprocessableEntity, "evidence_required",
				"Promotion to VALIDATED requires an out-of-sample or walk-forward backtest. "+
					"In-sample results are not evidence of an edge.")
			return
		}
	case domain.LifecyclePaper:
		if version.Lifecycle != domain.LifecycleValidated {
			writeError(w, r, http.StatusUnprocessableEntity, "promotion_refused",
				"Only a VALIDATED version may be promoted to PAPER.")
			return
		}
	}

	if err := s.store.Research.PromoteStrategyVersion(r.Context(), strategyID, req.Version,
		to, evidence, p.User.ID, req.Reason); err != nil {
		writeStoreError(w, r, err, "Strategy version not found.")
		return
	}
	s.auditAuth(r, &p.User.ID, domain.AuditStrategyPromoted, domain.AuditSuccess, map[string]any{
		"strategy_id": strategyID.String(), "version": req.Version,
		"from": string(version.Lifecycle), "to": string(to), "reason": req.Reason,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "promoted", "from": version.Lifecycle, "to": to, "evidence": evidence,
	})
}

type setStrategyEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetStrategyEnabled turns a strategy on or off.
func (s *Server) handleSetStrategyEnabled(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	strategyID, ok := parseUUID(w, r, chi.URLParam(r, "strategyID"), "Strategy id")
	if !ok {
		return
	}
	var req setStrategyEnabledRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	strategy, err := s.store.Research.Strategy(r.Context(), strategyID)
	if err != nil {
		writeStoreError(w, r, err, "Strategy not found.")
		return
	}
	// High-risk research approaches cannot be enabled for execution at all.
	if strategy.HighRisk && req.Enabled {
		writeError(w, r, http.StatusForbidden, "high_risk_strategy",
			"This strategy is marked high-risk research (grid, martingale or averaging-down family). "+
				"It can be backtested but cannot be enabled for execution.")
		return
	}
	if err := s.store.Research.SetStrategyEnabled(r.Context(), strategyID, req.Enabled); err != nil {
		writeStoreError(w, r, err, "Strategy not found.")
		return
	}
	action := domain.AuditStrategyDisabled
	if req.Enabled {
		action = domain.AuditStrategyEnabled
	}
	s.auditAuth(r, &p.User.ID, action, domain.AuditSuccess, map[string]any{
		"strategy_id": strategyID.String(), "enabled": req.Enabled,
	})
	writeJSON(w, r, http.StatusOK, map[string]any{"enabled": req.Enabled})
}

type runStrategyRequest struct {
	AccountID    string `json:"account_id"`
	InstrumentID string `json:"instrument_id"`
	Version      int    `json:"version"`
	// DryRun evaluates the strategy and returns the signal WITHOUT creating an
	// order intent. It is the default, because generating a signal and acting
	// on it are separate decisions.
	DryRun bool `json:"dry_run"`
}

// handleRunStrategy evaluates one strategy now.
func (s *Server) handleRunStrategy(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	strategyID, ok := parseUUID(w, r, chi.URLParam(r, "strategyID"), "Strategy id")
	if !ok {
		return
	}
	var req runStrategyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	accountID, err := uuid.Parse(req.AccountID)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_identifier",
			"account_id must be a valid identifier.")
		return
	}
	account, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, accountID)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}

	// The instrument is validated HERE rather than being left to the foreign
	// key. An unvalidated value reached the database and came back as
	// "strategy_runs_instrument_id_fkey rejected the write", which the handler
	// then reported as HTTP 500 "The strategy could not be evaluated" — an
	// internal error for what is plainly a bad request. A caller cannot tell
	// "you named an instrument that does not exist" from "the platform is
	// broken", and only one of those is their problem to fix.
	if req.InstrumentID == "" {
		writeError(w, r, http.StatusUnprocessableEntity, "instrument_required",
			"instrument_id is required: a strategy is evaluated against one instrument.")
		return
	}
	if _, err := s.store.Market.Instrument(r.Context(), req.InstrumentID); err != nil {
		writeStoreError(w, r, err, "Unknown instrument: "+req.InstrumentID+".")
		return
	}

	result, err := s.orchestratorRun(r, account, strategyID, req.InstrumentID, req.Version, !req.DryRun)
	if err != nil {
		if errors.Is(err, quant.ErrUnavailable) || errors.Is(err, quant.ErrCircuitOpen) {
			writeError(w, r, http.StatusServiceUnavailable, "research_unavailable",
				"The research service is unavailable, so no signal can be generated. "+
					"Manual trading is unaffected.")
			return
		}
		logging.FromContext(r.Context()).Error("strategy run failed", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"The strategy could not be evaluated.")
		return
	}
	writeJSON(w, r, http.StatusOK, result)
}

// orchestratorRun evaluates a strategy and optionally routes the signal.
func (s *Server) orchestratorRun(r *http.Request, account domain.Account, strategyID uuid.UUID,
	instrumentID string, version int, execute bool) (map[string]any, error) {

	p, _ := principalFrom(r.Context())
	if s.orchestrator == nil {
		return nil, fmt.Errorf("orchestrator is not configured")
	}
	outcome, err := s.orchestrator.EvaluateAndRoute(r.Context(), orchestrator.RunRequest{
		Account:      account,
		StrategyID:   strategyID,
		InstrumentID: instrumentID,
		Version:      version,
		Execute:      execute,
		ActorUserID:  p.User.ID,
		ActorRole:    p.User.Role,
		RequestID:    logging.RequestID(r.Context()),
	})
	if err != nil {
		return nil, err
	}

	resp := map[string]any{
		"status":      outcome.Status,
		"action":      outcome.Action,
		"confidence":  outcome.Confidence.String(),
		"explanation": outcome.Explanation,
		"executed":    outcome.Executed,
		"simulated":   s.cfg.SimulatedFunds(),
	}
	if outcome.SkipReason != "" {
		resp["skip_reason"] = outcome.SkipReason
	}
	if outcome.Order != nil {
		resp["order"] = toOrderResponse(*outcome.Order)
	}
	if outcome.Rejection != nil {
		resp["rejection"] = rejectionResponse{
			Code: string(outcome.Rejection.Code), Message: outcome.Rejection.Message,
			Detail: outcome.Rejection.Detail,
		}
	}
	if outcome.Indicators != nil {
		resp["indicators"] = outcome.Indicators
	}
	return resp, nil
}

// handleListSignals returns recent strategy signals.
func (s *Server) handleListSignals(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var accountID *uuid.UUID
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_identifier", "account_id is not valid.")
			return
		}
		if _, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, id); err != nil {
			writeStoreError(w, r, err, "Account not found.")
			return
		}
		accountID = &id
	}
	signals, err := s.store.Research.RecentSignals(r.Context(), accountID, intParam(r, "limit", 25, 200))
	if err != nil {
		writeStoreError(w, r, err, "No signals found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"signals": signals})
}

// handleListDecisions returns the decision audit trail.
func (s *Server) handleListDecisions(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	decisions, err := s.store.Research.ListDecisions(r.Context(), account.ID, intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"decisions": decisions})
}

// handleGetDecision returns one decision snapshot in full.
func (s *Server) handleGetDecision(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	decisionID, ok := parseUUID(w, r, chi.URLParam(r, "decisionID"), "Decision id")
	if !ok {
		return
	}
	decision, err := s.store.Research.Decision(r.Context(), account.ID, decisionID)
	if err != nil {
		writeStoreError(w, r, err, "Decision not found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"decision": decision})
}

type runBacktestRequest struct {
	StrategyID     string `json:"strategy_id"`
	Version        int    `json:"version"`
	InstrumentID   string `json:"instrument_id"`
	Timeframe      string `json:"timeframe"`
	From           string `json:"from"`
	To             string `json:"to"`
	InitialCapital string `json:"initial_capital"`
	Currency       string `json:"currency"`
	SampleKind     string `json:"sample_kind"`
	RiskPerTrade   string `json:"risk_per_trade"`
	Seed           int64  `json:"seed"`
}

// handleRunBacktest runs a historical evaluation through the research service.
func (s *Server) handleRunBacktest(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req runBacktestRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if s.quant == nil {
		writeError(w, r, http.StatusServiceUnavailable, "research_unavailable",
			"The research service is not configured.")
		return
	}

	v := newValidation()
	strategyID, err := uuid.Parse(req.StrategyID)
	if err != nil {
		v.add("strategy_id", "must be a valid identifier")
	}
	tf, err := domain.ParseTimeframe(defaultString(req.Timeframe, "1h"))
	if err != nil {
		v.add("timeframe", "must be one of 1m, 5m, 15m, 1h, 4h, 1d")
	}
	from, err := time.Parse(time.RFC3339, req.From)
	if err != nil {
		v.add("from", "must be an RFC3339 timestamp")
	}
	to, err := time.Parse(time.RFC3339, req.To)
	if err != nil {
		v.add("to", "must be an RFC3339 timestamp")
	}
	if err == nil && !to.After(from) {
		v.add("to", "must be after from")
	}
	sampleKind := defaultString(req.SampleKind, "out_of_sample")
	switch sampleKind {
	case "in_sample", "validation", "out_of_sample", "walk_forward", "paper_forward":
	default:
		v.add("sample_kind", "must be in_sample, validation, out_of_sample, walk_forward or paper_forward")
	}
	capital, err := parseDecimal(defaultString(req.InitialCapital, "500"))
	if err != nil || !capital.IsPositive() {
		v.add("initial_capital", "must be a positive decimal number")
	}
	if !v.ok() {
		v.write(w, r)
		return
	}

	strategy, err := s.store.Research.Strategy(r.Context(), strategyID)
	if err != nil {
		writeStoreError(w, r, err, "Strategy not found.")
		return
	}
	version := req.Version
	if version == 0 {
		latest, verr := s.store.Research.LatestStrategyVersion(r.Context(), strategyID)
		if verr != nil {
			writeStoreError(w, r, verr, "Strategy version not found.")
			return
		}
		version = latest.Version
	}
	strategyVersion, err := s.store.Research.StrategyVersion(r.Context(), strategyID, version)
	if err != nil {
		writeStoreError(w, r, err, "Strategy version not found.")
		return
	}
	instrument, err := s.store.Market.Instrument(r.Context(), req.InstrumentID)
	if err != nil {
		writeStoreError(w, r, err, "Instrument not found.")
		return
	}

	bars, err := s.store.Market.BarsInRange(r.Context(), instrument.ID, tf, from, to)
	if err != nil {
		writeStoreError(w, r, err, "No market data for that range.")
		return
	}
	if len(bars) < 50 {
		writeError(w, r, http.StatusUnprocessableEntity, "insufficient_data",
			"That range holds too few bars to produce a meaningful result. Widen the period.")
		return
	}

	barInputs := toBarInputs(bars)
	currency := defaultString(req.Currency, "ZAR")
	riskPerTrade := defaultString(req.RiskPerTrade, "0.01")

	resp, err := s.quant.Backtest(r.Context(), quant.BacktestRequest{
		StrategyKey: strategy.Key, Version: version,
		InstrumentID: instrument.ID, Timeframe: string(tf),
		Parameters: strategyVersion.Parameters, Bars: barInputs,
		InitialCapital: capital.String(), Currency: currency,
		ContractSize:     instrument.Spec.ContractSize.String(),
		CommissionPerLot: instrument.Spec.CommissionPerLot.String(),
		// Costs are always applied. A frictionless backtest is available only
		// by explicitly labelling the result as unrealistic, which this
		// endpoint does not offer.
		SpreadFraction:   "0.00012",
		SlippageFraction: "0.0001",
		SwapLongPerLot:   instrument.Spec.SwapLongPerLot.String(),
		SwapShortPerLot:  instrument.Spec.SwapShortPerLot.String(),
		RiskPerTrade:     riskPerTrade,
		MinQuantity:      instrument.Spec.MinQuantity.String(),
		QuantityStep:     instrument.Spec.QuantityStep.String(),
		SampleKind:       sampleKind,
		Seed:             req.Seed,
	})
	if err != nil {
		if errors.Is(err, quant.ErrUnavailable) || errors.Is(err, quant.ErrCircuitOpen) {
			writeError(w, r, http.StatusServiceUnavailable, "research_unavailable",
				"The research service is unavailable.")
			return
		}
		logging.FromContext(r.Context()).Error("backtest failed", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "backtest_failed",
			"The backtest could not be completed.")
		return
	}

	metricsJSON, _ := json.Marshal(resp.Metrics)
	equityJSON, _ := json.Marshal(resp.EquityCurve)
	costModel, _ := json.Marshal(map[string]string{
		"spread_fraction": "0.00012", "slippage_fraction": "0.0001",
		"commission_per_lot": instrument.Spec.CommissionPerLot.String(),
		"swap_long_per_lot":  instrument.Spec.SwapLongPerLot.String(),
		"swap_short_per_lot": instrument.Spec.SwapShortPerLot.String(),
	})

	trades := make([]store.BacktestTrade, 0, len(resp.Trades))
	for _, t := range resp.Trades {
		trade := store.BacktestTrade{
			InstrumentID: instrument.ID, Side: t.Side,
			Quantity:   mustDecimal(t.Quantity),
			EntryPrice: mustDecimal(t.EntryPrice),
			GrossPnL:   mustDecimal(t.GrossPnL),
			Commission: mustDecimal(t.Commission),
			Slippage:   mustDecimal(t.Slippage),
			Swap:       mustDecimal(t.Swap),
			NetPnL:     mustDecimal(t.NetPnL),
		}
		if ts, err := time.Parse(time.RFC3339, t.EntryTime); err == nil {
			trade.EntryTime = ts
		}
		if t.ExitTime != nil {
			if ts, err := time.Parse(time.RFC3339, *t.ExitTime); err == nil {
				trade.ExitTime = &ts
			}
		}
		if t.ExitPrice != nil {
			d := mustDecimal(*t.ExitPrice)
			trade.ExitPrice = &d
		}
		if t.MAE != nil {
			d := mustDecimal(*t.MAE)
			trade.MAE = &d
		}
		if t.MFE != nil {
			d := mustDecimal(*t.MFE)
			trade.MFE = &d
		}
		if t.ExitReason != "" {
			reason := t.ExitReason
			trade.ExitReason = &reason
		}
		trades = append(trades, trade)
	}

	seed := req.Seed
	backtestID, err := s.store.Research.SaveBacktest(r.Context(), store.Backtest{
		StrategyID: strategyID, StrategyVersion: version,
		InstrumentID: instrument.ID, Timeframe: string(tf),
		DatasetHash: resp.DatasetHash, PeriodStart: from, PeriodEnd: to,
		SampleKind: sampleKind, InitialCapital: capital, Currency: currency,
		Parameters: strategyVersion.Parameters, CostModel: costModel,
		Frictionless: resp.Frictionless, Metrics: metricsJSON, EquityCurve: equityJSON,
		Warnings: resp.Warnings, CodeHash: resp.CodeHash, Seed: &seed,
		Status: "completed", CreatedBy: &p.User.ID,
	}, trades)
	if err != nil {
		writeStoreError(w, r, err, "Could not store the backtest result.")
		return
	}

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"backtest_id":  backtestID.String(),
		"metrics":      resp.Metrics,
		"trades":       len(resp.Trades),
		"warnings":     resp.Warnings,
		"sample_kind":  sampleKind,
		"dataset_hash": resp.DatasetHash,
		"note": "Costs, spread and slippage are applied. Past simulated performance " +
			"is not evidence of future results.",
	})
}

// handleListBacktests returns stored backtest runs.
func (s *Server) handleListBacktests(w http.ResponseWriter, r *http.Request) {
	var strategyID *uuid.UUID
	if raw := r.URL.Query().Get("strategy_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_identifier", "strategy_id is not valid.")
			return
		}
		strategyID = &id
	}
	backtests, err := s.store.Research.ListBacktests(r.Context(), strategyID, intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "No backtests found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"backtests": backtests})
}

// handleGetBacktest returns one run with its trades.
func (s *Server) handleGetBacktest(w http.ResponseWriter, r *http.Request) {
	backtestID, ok := parseUUID(w, r, chi.URLParam(r, "backtestID"), "Backtest id")
	if !ok {
		return
	}
	backtest, err := s.store.Research.Backtest(r.Context(), backtestID)
	if err != nil {
		writeStoreError(w, r, err, "Backtest not found.")
		return
	}
	trades, err := s.store.Research.BacktestTrades(r.Context(), backtestID, 1000)
	if err != nil {
		writeStoreError(w, r, err, "Backtest not found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"backtest": backtest, "trades": trades})
}

type trainModelRequest struct {
	ModelKey     string `json:"model_key"`
	Algorithm    string `json:"algorithm"`
	InstrumentID string `json:"instrument_id"`
	Timeframe    string `json:"timeframe"`
	From         string `json:"from"`
	To           string `json:"to"`
	Horizon      int    `json:"horizon"`
	Seed         int64  `json:"seed"`
}

// handleTrainModel trains a model through the research service and records its
// full reproducibility metadata.
func (s *Server) handleTrainModel(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req trainModelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if s.quant == nil {
		writeError(w, r, http.StatusServiceUnavailable, "research_unavailable",
			"The research service is not configured.")
		return
	}

	tf, err := domain.ParseTimeframe(defaultString(req.Timeframe, "1h"))
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_timeframe",
			"timeframe must be one of 1m, 5m, 15m, 1h, 4h, 1d.")
		return
	}
	from, err1 := time.Parse(time.RFC3339, req.From)
	to, err2 := time.Parse(time.RFC3339, req.To)
	if err1 != nil || err2 != nil || !to.After(from) {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_period",
			"from and to must be RFC3339 timestamps with to after from.")
		return
	}
	instrument, err := s.store.Market.Instrument(r.Context(), req.InstrumentID)
	if err != nil {
		writeStoreError(w, r, err, "Instrument not found.")
		return
	}
	bars, err := s.store.Market.BarsInRange(r.Context(), instrument.ID, tf, from, to)
	if err != nil {
		writeStoreError(w, r, err, "No market data for that range.")
		return
	}
	if len(bars) < 300 {
		writeError(w, r, http.StatusUnprocessableEntity, "insufficient_data",
			"Training needs at least 300 bars so that chronological train, validation "+
				"and test windows each hold enough data to mean anything.")
		return
	}

	resp, err := s.quant.Train(r.Context(), quant.TrainRequest{
		ModelKey: req.ModelKey, Algorithm: defaultString(req.Algorithm, "logistic_regression"),
		InstrumentID: instrument.ID, Timeframe: string(tf), Bars: toBarInputs(bars),
		TrainFraction: 0.6, ValidationFraction: 0.2,
		// The embargo drops bars between windows so a label computed over a
		// forward horizon cannot leak across the split boundary.
		EmbargoBars: maxInt(req.Horizon, 1) * 2,
		Horizon:     maxInt(req.Horizon, 1),
		Seed:        req.Seed,
	})
	if err != nil {
		if errors.Is(err, quant.ErrUnavailable) || errors.Is(err, quant.ErrCircuitOpen) {
			writeError(w, r, http.StatusServiceUnavailable, "research_unavailable",
				"The research service is unavailable.")
			return
		}
		logging.FromContext(r.Context()).Error("training failed", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "training_failed",
			"Model training could not be completed.")
		return
	}

	datasetID, err := s.store.Research.UpsertDataset(r.Context(), store.MLDataset{
		Name:         req.ModelKey + " " + instrument.ID + " " + string(tf),
		InstrumentID: instrument.ID, Timeframe: string(tf),
		FeatureSet: resp.FeatureDefinition, LabelDefinition: resp.LabelDefinition,
		PeriodStart: from, PeriodEnd: to, RowCount: resp.RowCount,
		ContentHash: resp.DatasetHash,
	})
	if err != nil {
		writeStoreError(w, r, err, "Could not record the dataset.")
		return
	}

	modelID, err := s.store.Research.UpsertModel(r.Context(), req.ModelKey, req.ModelKey,
		"direction_probability", "Directional probability model for "+instrument.Symbol)
	if err != nil {
		writeStoreError(w, r, err, "Could not record the model.")
		return
	}

	parseWindow := func(name, field string) *time.Time {
		wnd, ok := resp.Windows[name]
		if !ok {
			return nil
		}
		raw, ok := wnd[field].(string)
		if !ok {
			return nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil
		}
		return &t
	}
	trainStart := parseWindow("train", "start")
	trainEnd := parseWindow("train", "end")
	if trainStart == nil || trainEnd == nil {
		writeError(w, r, http.StatusInternalServerError, "training_failed",
			"The research service did not report its training windows.")
		return
	}

	existing, _ := s.store.Research.ListModelVersions(r.Context(), 200)
	nextVersion := 1
	for _, v := range existing {
		if v.ModelID == modelID && v.Version >= nextVersion {
			nextVersion = v.Version + 1
		}
	}

	modelVersionID, err := s.store.Research.SaveModelVersion(r.Context(), store.MLModelVersion{
		ModelID: modelID, Version: nextVersion, DatasetID: datasetID,
		Algorithm: resp.Algorithm, Hyperparameters: resp.Hyperparameters,
		CodeGitSHA: resp.CodeHash, DatasetHash: resp.DatasetHash,
		FeatureDefinition: resp.FeatureDefinition, LabelDefinition: resp.LabelDefinition,
		TrainStart: *trainStart, TrainEnd: *trainEnd,
		ValidationStart: parseWindow("validation", "start"),
		ValidationEnd:   parseWindow("validation", "end"),
		TestStart:       parseWindow("test", "start"),
		TestEnd:         parseWindow("test", "end"),
		RandomSeed:      resp.Seed, DependencyVersions: resp.DependencyVersions,
		ArtifactPath: strPtr(resp.ArtifactPath), ArtifactHash: strPtr(resp.ArtifactHash),
		// Every new model starts EXPERIMENTAL. Nothing self-promotes.
		Lifecycle: "EXPERIMENTAL",
	})
	if err != nil {
		writeStoreError(w, r, err, "Could not record the model version.")
		return
	}

	for split, metrics := range resp.Evaluations {
		raw, _ := json.Marshal(metrics)
		start, end := from, to
		if wnd, ok := resp.Windows[split]; ok {
			if v, ok := wnd["start"].(string); ok {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					start = t
				}
			}
			if v, ok := wnd["end"].(string); ok {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					end = t
				}
			}
		}
		if err := s.store.Research.SaveEvaluation(r.Context(), modelVersionID, split, raw, start, end); err != nil {
			logging.FromContext(r.Context()).Warn("could not store evaluation",
				"split", split, "error", err.Error())
		}
	}

	s.auditAuth(r, &p.User.ID, domain.AuditModelPromoted, domain.AuditSuccess, map[string]any{
		"model_key": req.ModelKey, "version": nextVersion, "lifecycle": "EXPERIMENTAL",
	})

	writeJSON(w, r, http.StatusCreated, map[string]any{
		"model_version_id": modelVersionID.String(),
		"version":          nextVersion,
		"algorithm":        resp.Algorithm,
		"evaluations":      resp.Evaluations,
		"windows":          resp.Windows,
		"warnings":         resp.Warnings,
		"lifecycle":        "EXPERIMENTAL",
		// The reproducibility record travels with the response. A result whose
		// data, environment and feature definition are invisible cannot be
		// checked by whoever is asked to trust it.
		"dataset_hash":        resp.DatasetHash,
		"dependency_versions": resp.DependencyVersions,
		"feature_definition":  resp.FeatureDefinition,
		"label_definition":    resp.LabelDefinition,
		"seed":                resp.Seed,
		"row_count":           resp.RowCount,
		"artifact_hash":       resp.ArtifactHash,
		"note": "Trained with chronological splits and an embargo between windows. " +
			"The model is EXPERIMENTAL and cannot influence orders until it is promoted.",
	})
}

// handleListModels returns trained model versions with their evaluations.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	versions, err := s.store.Research.ListModelVersions(r.Context(), intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "No models found.")
		return
	}
	type modelRow struct {
		store.ModelVersionRow
		Evaluations []store.EvaluationRow `json:"evaluations"`
	}
	out := make([]modelRow, 0, len(versions))
	for _, v := range versions {
		row := modelRow{ModelVersionRow: v}
		if evals, err := s.store.Research.EvaluationsForVersion(r.Context(), v.ID); err == nil {
			row.Evaluations = evals
		}
		out = append(out, row)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"models": out,
		"note":   "Paper is the highest lifecycle stage available. Models cannot promote themselves.",
	})
}

// handleListDatasets returns dataset snapshots.
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	datasets, err := s.store.Research.ListDatasets(r.Context(), intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "No datasets found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"datasets": datasets})
}

// handleCalendar returns economic events.
func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	f := store.EventFilter{Limit: intParam(r, "limit", 200, 500)}
	now := s.clock.Now()

	switch r.URL.Query().Get("range") {
	case "today":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		end := start.Add(24 * time.Hour)
		f.From, f.To = &start, &end
	case "week":
		start := now.Add(-24 * time.Hour)
		end := now.Add(7 * 24 * time.Hour)
		f.From, f.To = &start, &end
	default:
		start := now.Add(-24 * time.Hour)
		end := now.Add(14 * 24 * time.Hour)
		f.From, f.To = &start, &end
	}
	if raw := r.URL.Query().Get("currency"); raw != "" {
		f.Currencies = []string{raw}
	}
	f.MinImpact = r.URL.Query().Get("impact")

	events, err := s.store.Research.ListEconomicEvents(r.Context(), f)
	if err != nil {
		writeStoreError(w, r, err, "No events found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"events": events,
		"source": "seeded development fixtures",
		"note": "Calendar data in this build is deterministic development fixture data, " +
			"not a licensed provider feed.",
	})
}

// handleNews returns headlines.
func (s *Server) handleNews(w http.ResponseWriter, r *http.Request) {
	var instrumentID *string
	if raw := r.URL.Query().Get("instrument_id"); raw != "" {
		instrumentID = &raw
	}
	news, err := s.store.Research.ListNews(r.Context(), instrumentID, intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "No news found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"news":   news,
		"source": "seeded development fixtures",
		"note":   "Headlines in this build are development fixtures, not a licensed news feed.",
	})
}

// handleScanner ranks opportunities across the permitted universe.
//
// The scanner analyses and ranks. It places nothing: any candidate a user acts
// on goes through the ordinary order pipeline with every gate applied.
func (s *Server) handleScanner(w http.ResponseWriter, r *http.Request) {
	if s.quant == nil {
		writeError(w, r, http.StatusServiceUnavailable, "research_unavailable",
			"The research service is not configured.")
		return
	}
	tf, err := domain.ParseTimeframe(defaultString(r.URL.Query().Get("timeframe"), "1h"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_timeframe",
			"timeframe must be one of 1m, 5m, 15m, 1h, 4h, 1d.")
		return
	}

	instruments, err := s.store.Market.ListInstruments(r.Context(), true)
	if err != nil {
		writeStoreError(w, r, err, "No instruments found.")
		return
	}

	now := s.clock.Now()
	sessions := s.marketClock.ActiveSessions(now)
	sessionName := "none"
	if len(sessions) > 0 {
		sessionName = string(sessions[0])
	}

	req := quant.ScanRequest{Timeframe: string(tf)}
	for _, inst := range instruments {
		bars, err := s.store.Market.Bars(r.Context(), inst.ID, tf, 200, true)
		if err != nil || len(bars) < 50 {
			continue
		}
		spread := "0"
		if q, _, err := s.store.Market.LatestQuote(r.Context(), inst.ID); err == nil {
			spread = q.SpreadFraction().String()
		}
		eventRisk := "none"
		if events, err := s.store.Research.UpcomingHighImpactEvents(r.Context(), inst.ID,
			now, now.Add(2*time.Hour)); err == nil && len(events) > 0 {
			eventRisk = "high"
		}
		req.Instruments = append(req.Instruments, quant.ScanInstrumentInput{
			InstrumentID: inst.ID, Symbol: inst.Symbol, Bars: toBarInputs(bars),
			SpreadFraction: spread, Session: sessionName, EventRisk: eventRisk,
		})
	}
	if len(req.Instruments) == 0 {
		writeJSON(w, r, http.StatusOK, map[string]any{
			"candidates": []any{},
			"message":    "Not enough market history yet to scan. Let the platform run for a while.",
		})
		return
	}

	resp, err := s.quant.Scan(r.Context(), req)
	if err != nil {
		if errors.Is(err, quant.ErrUnavailable) || errors.Is(err, quant.ErrCircuitOpen) {
			writeError(w, r, http.StatusServiceUnavailable, "research_unavailable",
				"The research service is unavailable, so the scanner cannot run.")
			return
		}
		logging.FromContext(r.Context()).Error("scan failed", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "scan_failed", "The scan could not be completed.")
		return
	}
	sort.SliceStable(resp.Candidates, func(i, j int) bool {
		return resp.Candidates[i].Score.GreaterThan(resp.Candidates[j].Score)
	})
	writeJSON(w, r, http.StatusOK, map[string]any{
		"candidates": resp.Candidates,
		"timeframe":  string(tf),
		"note":       "The scanner ranks opportunities. It does not place orders.",
	})
}

// handleActivity returns a unified chronological timeline.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	limit := intParam(r, "limit", 50, 200)

	type activityItem struct {
		Kind      string    `json:"kind"`
		Title     string    `json:"title"`
		Detail    string    `json:"detail"`
		Severity  string    `json:"severity"`
		Reference string    `json:"reference,omitempty"`
		At        time.Time `json:"at"`
	}
	items := []activityItem{}

	orders, err := s.store.Trading.ListOrdersForUser(r.Context(), account.UserID, store.OrderFilter{
		AccountID: &account.ID, Limit: limit,
	})
	if err == nil {
		for _, o := range orders {
			severity := "info"
			detail := string(o.Side) + " " + o.Quantity.String() + " " + o.Symbol
			if o.Status == domain.OrderRejected {
				severity = "warning"
				if o.RejectReason != nil {
					detail = *o.RejectReason
				}
			}
			items = append(items, activityItem{
				Kind: "order", Title: string(o.Status) + " " + o.Symbol,
				Detail: detail, Severity: severity, Reference: o.ID.String(), At: o.UpdatedAt,
			})
		}
	}

	if events, err := s.store.Control.ListRiskEvents(r.Context(), account.ID, limit); err == nil {
		for _, e := range events {
			items = append(items, activityItem{
				Kind: "risk", Title: string(e.Code), Detail: e.Message,
				Severity: string(e.Severity), At: e.CreatedAt,
			})
		}
	}

	if decisions, err := s.store.Research.ListDecisions(r.Context(), account.ID, limit); err == nil {
		for _, d := range decisions {
			severity := "info"
			if d.Outcome == "rejected" {
				severity = "warning"
			}
			detail := d.SignalAction + " " + d.InstrumentID
			if d.OutcomeReason != nil && *d.OutcomeReason != "" {
				detail = *d.OutcomeReason
			}
			items = append(items, activityItem{
				Kind: "decision", Title: "Decision: " + d.Outcome, Detail: detail,
				Severity: severity, Reference: d.ID.String(), At: d.CreatedAt,
			})
		}
	}

	if audits, err := s.store.Control.ListAudit(r.Context(), store.AuditFilter{
		AccountID: &account.ID, Limit: limit,
	}); err == nil {
		for _, a := range audits {
			severity := "info"
			if a.Result == domain.AuditBlocked || a.Result == domain.AuditFailure {
				severity = "warning"
			}
			items = append(items, activityItem{
				Kind: "audit", Title: string(a.Action), Detail: string(a.Result),
				Severity: severity, At: a.OccurredAt,
			})
		}
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"activity": items})
}

// handleAudit returns audit events.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	f := store.AuditFilter{Limit: intParam(r, "limit", 100, 500), Offset: intParam(r, "offset", 0, 10000)}

	// Non-admins see only their own actions. The audit log records security
	// events across the platform, and one user's login history is not another
	// user's business.
	if !p.User.Role.CanAdminister() {
		f.UserID = &p.User.ID
	} else if raw := r.URL.Query().Get("user_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_identifier", "user_id is not valid.")
			return
		}
		f.UserID = &id
	}
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_identifier", "account_id is not valid.")
			return
		}
		if _, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, id); err != nil && !p.User.Role.CanAdminister() {
			writeStoreError(w, r, err, "Account not found.")
			return
		}
		f.AccountID = &id
	}

	events, err := s.store.Control.ListAudit(r.Context(), f)
	if err != nil {
		writeStoreError(w, r, err, "No audit events found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"events": events})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toBarInputs(bars []domain.Bar) []quant.BarInput {
	out := make([]quant.BarInput, 0, len(bars))
	for _, b := range bars {
		out = append(out, quant.BarInput{
			OpenTime: b.OpenTime.UTC().Format(time.RFC3339),
			Open:     b.Open.String(), High: b.High.String(),
			Low: b.Low.String(), Close: b.Close.String(), Volume: b.Volume.String(),
		})
	}
	return out
}

func mustDecimal(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
