package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/store"
)

// accountForRequest loads an account scoped to the caller and writes the error
// response itself when it cannot. Every account-scoped handler starts here, so
// no handler can accidentally operate on someone else's account.
func (s *Server) accountForRequest(w http.ResponseWriter, r *http.Request, param string) (domain.Account, bool) {
	p, _ := principalFrom(r.Context())
	id, ok := parseUUID(w, r, chi.URLParam(r, param), "Account id")
	if !ok {
		return domain.Account{}, false
	}
	account, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, id)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return domain.Account{}, false
	}
	return account, true
}

// accountForOperations resolves an account for the reconciliation and
// operations endpoints, widening to any account for an ADMIN.
//
// # Why this differs from accountForRequest
//
// accountForRequest scopes strictly to what the caller owns, which is right
// for every trading and reporting route: an operations view is not a reason to
// widen who can see whose balances.
//
// Reconciliation is the exception, and it has to be. Admin is the incident
// role -- it is deliberately barred from placing orders (separation of duties)
// and it is the role permitted to resolve a divergence. But in this build an
// admin owns no trading account, so scoping by ownership made the admin-only
// resolve endpoint unreachable: every call answered "Account not found",
// including from the one role allowed to use it. The endpoint existed and
// could not be called.
//
// So an admin may address any account here. Every use is audited with the
// actor, the account, the action and the operator's stated reason, which is
// what makes the wider reach accountable rather than merely broader.
func (s *Server) accountForOperations(w http.ResponseWriter, r *http.Request, param string) (domain.Account, bool) {
	p, _ := principalFrom(r.Context())
	id, ok := parseUUID(w, r, chi.URLParam(r, param), "Account id")
	if !ok {
		return domain.Account{}, false
	}

	if p.User.Role == domain.RoleAdmin {
		account, err := s.store.Accounts.AccountByIDUnscoped(r.Context(), id)
		if err != nil {
			writeStoreError(w, r, err, "Account not found.")
			return domain.Account{}, false
		}
		return account, true
	}

	account, err := s.store.Accounts.AccountForUser(r.Context(), p.User.ID, id)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return domain.Account{}, false
	}
	return account, true
}

type accountResponse struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Mode           string    `json:"mode"`
	Currency       string    `json:"currency"`
	BrokerName     string    `json:"broker_name"`
	Enabled        bool      `json:"enabled"`
	TradingEnabled bool      `json:"trading_enabled"`
	Leverage       string    `json:"leverage"`
	Simulated      bool      `json:"simulated"`
	Version        int64     `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
}

func toAccountResponse(a domain.Account) accountResponse {
	return accountResponse{
		ID: a.ID.String(), Name: a.Name, Mode: string(a.Mode),
		Currency: string(a.Currency), BrokerName: a.BrokerName,
		Enabled: a.Enabled, TradingEnabled: a.TradingEnabled,
		Leverage: a.Leverage.String(), Simulated: a.Mode != domain.ModeLive,
		Version: a.Version, CreatedAt: a.CreatedAt,
	}
}

// handleListAccounts returns the caller's accounts.
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	accounts, err := s.store.Accounts.ListAccountsForUser(r.Context(), p.User.ID)
	if err != nil {
		writeStoreError(w, r, err, "No accounts found.")
		return
	}
	out := make([]accountResponse, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, toAccountResponse(a))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"accounts": out})
}

// handleGetAccount returns one account.
func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"account": toAccountResponse(account)})
}

type portfolioResponse struct {
	Account              accountResponse    `json:"account"`
	Balance              string             `json:"balance"`
	Equity               string             `json:"equity"`
	MarginUsed           string             `json:"margin_used"`
	FreeMargin           string             `json:"free_margin"`
	MarginLevel          string             `json:"margin_level_pct"`
	RealizedPnL          string             `json:"realized_pnl"`
	UnrealizedPnL        string             `json:"unrealized_pnl"`
	DayPnL               string             `json:"day_pnl"`
	Commission           string             `json:"commission"`
	Swap                 string             `json:"swap"`
	GrossExposure        string             `json:"gross_exposure"`
	NetExposure          string             `json:"net_exposure"`
	PeakEquity           string             `json:"peak_equity"`
	Drawdown             string             `json:"drawdown_fraction"`
	OpenPositions        int                `json:"open_positions"`
	PendingOrders        int                `json:"pending_orders"`
	Currency             string             `json:"currency"`
	Positions            []positionResponse `json:"positions"`
	ExposureByInstrument map[string]string  `json:"exposure_by_instrument"`
	ExposureByCurrency   map[string]string  `json:"exposure_by_currency"`
	// UnvaluedPositions above zero means the numbers above understate risk,
	// because at least one position could not be priced.
	UnvaluedPositions int       `json:"unvalued_positions"`
	Simulated         bool      `json:"simulated"`
	AsOf              time.Time `json:"as_of"`
}

// handlePortfolio returns an account's full financial state.
func (s *Server) handlePortfolio(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	snapshot, err := s.portfolio.Compute(r.Context(), account)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}

	positions := make([]positionResponse, 0, len(snapshot.Positions))
	for _, view := range snapshot.Positions {
		pos := view.Position
		positions = append(positions, positionResponse{
			ID: pos.ID.String(), AccountID: pos.AccountID.String(),
			InstrumentID: pos.InstrumentID, Symbol: pos.Symbol,
			Side: string(pos.Side), Quantity: pos.Quantity.String(),
			AvgEntryPrice: pos.AvgEntryPrice.String(), CurrentPrice: view.CurrentPrice.String(),
			UnrealizedPnL: view.UnrealizedPnL.StringFixed(),
			UnrealizedCcy: string(account.Currency),
			NotionalValue: view.NotionalValue.StringFixed(),
			MarginUsed:    view.MarginUsed.StringFixed(),
			Valued:        view.Valued, ValuationNote: view.ValuationNote,
			OpenedAt: pos.OpenedAt, Simulated: account.Mode != domain.ModeLive,
		})
	}

	exposureInstrument := map[string]string{}
	for k, v := range snapshot.ExposureByInstrument {
		exposureInstrument[k] = v.StringFixed()
	}
	exposureCurrency := map[string]string{}
	for k, v := range snapshot.ExposureByCurrency {
		exposureCurrency[string(k)] = v.StringFixed()
	}

	st := snapshot.State
	writeJSON(w, r, http.StatusOK, portfolioResponse{
		Account:              toAccountResponse(account),
		Balance:              st.Balance.StringFixed(),
		Equity:               st.Equity.StringFixed(),
		MarginUsed:           st.MarginUsed.StringFixed(),
		FreeMargin:           st.FreeMargin.StringFixed(),
		MarginLevel:          st.MarginLevel().StringFixed(2),
		RealizedPnL:          st.RealizedPnL.StringFixed(),
		UnrealizedPnL:        st.UnrealizedPnL.StringFixed(),
		DayPnL:               st.DayPnL().StringFixed(),
		Commission:           st.Commission.StringFixed(),
		Swap:                 st.Swap.StringFixed(),
		GrossExposure:        st.GrossExposure.StringFixed(),
		NetExposure:          st.NetExposure.StringFixed(),
		PeakEquity:           st.PeakEquity.StringFixed(),
		Drawdown:             st.DrawdownFraction().StringFixed(4),
		OpenPositions:        st.OpenPositions,
		PendingOrders:        st.PendingOrders,
		Currency:             string(st.Currency),
		Positions:            positions,
		ExposureByInstrument: exposureInstrument,
		ExposureByCurrency:   exposureCurrency,
		UnvaluedPositions:    snapshot.UnvaluedPositions,
		Simulated:            st.Simulated,
		AsOf:                 st.AsOf,
	})
}

// handleEquityHistory returns the equity curve.
func (s *Server) handleEquityHistory(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	points, err := s.store.Accounts.EquityHistory(r.Context(), account.ID, intParam(r, "limit", 500, 5000))
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	type point struct {
		Equity        string    `json:"equity"`
		Balance       string    `json:"balance"`
		UnrealizedPnL string    `json:"unrealized_pnl"`
		MarginUsed    string    `json:"margin_used"`
		RecordedAt    time.Time `json:"recorded_at"`
	}
	out := make([]point, 0, len(points))
	for _, p := range points {
		out = append(out, point{
			Equity: p.Equity.StringFixed(), Balance: p.Balance.StringFixed(),
			UnrealizedPnL: p.UnrealizedPnL.StringFixed(), MarginUsed: p.MarginUsed.StringFixed(),
			RecordedAt: p.RecordedAt,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"points": out, "currency": string(account.Currency), "simulated": s.cfg.SimulatedFunds(),
	})
}

// handleTransactions returns ledger entries.
func (s *Server) handleTransactions(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	txs, err := s.store.Accounts.ListTransactions(r.Context(), account.ID, account.Currency,
		intParam(r, "limit", 100, 500), intParam(r, "offset", 0, 100000))
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	type txResponse struct {
		ID           string    `json:"id"`
		Sequence     int64     `json:"sequence"`
		Type         string    `json:"type"`
		Amount       string    `json:"amount"`
		BalanceAfter string    `json:"balance_after"`
		Currency     string    `json:"currency"`
		Description  string    `json:"description"`
		OrderID      *string   `json:"order_id,omitempty"`
		CreatedAt    time.Time `json:"created_at"`
	}
	out := make([]txResponse, 0, len(txs))
	for _, t := range txs {
		var orderID *string
		if t.OrderID != nil {
			s := t.OrderID.String()
			orderID = &s
		}
		out = append(out, txResponse{
			ID: t.ID.String(), Sequence: t.Sequence, Type: string(t.Type),
			Amount: t.Amount.StringFixed(), BalanceAfter: t.BalanceAfter.StringFixed(),
			Currency: string(t.Amount.Currency()), Description: t.Description,
			OrderID: orderID, CreatedAt: t.CreatedAt,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"transactions": out, "simulated": s.cfg.SimulatedFunds(),
	})
}

// handleAttribution returns realised P&L grouped by instrument.
func (s *Server) handleAttribution(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForRequest(w, r, "accountID")
	if !ok {
		return
	}
	rows, err := s.portfolio.AttributionByInstrument(r.Context(), account.ID, account.Currency, 500)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	type row struct {
		Key         string `json:"key"`
		Label       string `json:"label"`
		RealizedPnL string `json:"realized_pnl"`
		Trades      int    `json:"trades"`
		WinRate     string `json:"win_rate"`
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		out = append(out, row{
			Key: r.Key, Label: r.Label, RealizedPnL: r.RealizedPnL.StringFixed(),
			Trades: r.Trades, WinRate: r.WinRate.StringFixed(4),
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"attribution": out, "currency": string(account.Currency),
		"simulated": s.cfg.SimulatedFunds(),
	})
}

type instrumentResponse struct {
	ID                  string   `json:"id"`
	Symbol              string   `json:"symbol"`
	Name                string   `json:"name"`
	AssetClass          string   `json:"asset_class"`
	BaseCurrency        string   `json:"base_currency"`
	QuoteCurrency       string   `json:"quote_currency"`
	Enabled             bool     `json:"enabled"`
	ContractSize        string   `json:"contract_size"`
	PricePrecision      int32    `json:"price_precision"`
	TickSize            string   `json:"tick_size"`
	MinQuantity         string   `json:"min_quantity"`
	MaxQuantity         string   `json:"max_quantity"`
	QuantityStep        string   `json:"quantity_step"`
	MarginRate          string   `json:"margin_rate"`
	MaxLeverage         string   `json:"max_leverage"`
	SupportedOrderTypes []string `json:"supported_order_types"`
	CommissionPerLot    string   `json:"commission_per_lot"`
}

func toInstrumentResponse(i domain.Instrument) instrumentResponse {
	types := make([]string, 0, len(i.Spec.SupportedOrderTypes))
	for _, t := range i.Spec.SupportedOrderTypes {
		types = append(types, string(t))
	}
	return instrumentResponse{
		ID: i.ID, Symbol: i.Symbol, Name: i.Name, AssetClass: string(i.Class),
		BaseCurrency: string(i.BaseCcy), QuoteCurrency: string(i.QuoteCcy), Enabled: i.Enabled,
		ContractSize: i.Spec.ContractSize.String(), PricePrecision: i.Spec.PricePrecision,
		TickSize: i.Spec.TickSize.String(), MinQuantity: i.Spec.MinQuantity.String(),
		MaxQuantity: i.Spec.MaxQuantity.String(), QuantityStep: i.Spec.QuantityStep.String(),
		MarginRate: i.Spec.MarginRate.String(), MaxLeverage: i.Spec.MaxLeverage.String(),
		SupportedOrderTypes: types, CommissionPerLot: i.Spec.CommissionPerLot.String(),
	}
}

// handleListInstruments returns the tradable universe.
func (s *Server) handleListInstruments(w http.ResponseWriter, r *http.Request) {
	instruments, err := s.store.Market.ListInstruments(r.Context(), r.URL.Query().Get("enabled") == "true")
	if err != nil {
		writeStoreError(w, r, err, "No instruments found.")
		return
	}
	out := make([]instrumentResponse, 0, len(instruments))
	for _, i := range instruments {
		out = append(out, toInstrumentResponse(i))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"instruments": out})
}

// handleGetInstrument returns one instrument specification.
func (s *Server) handleGetInstrument(w http.ResponseWriter, r *http.Request) {
	inst, err := s.store.Market.Instrument(r.Context(), chi.URLParam(r, "instrumentID"))
	if err != nil {
		writeStoreError(w, r, err, "Instrument not found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"instrument": toInstrumentResponse(inst)})
}

type quoteResponse struct {
	InstrumentID string    `json:"instrument_id"`
	Symbol       string    `json:"symbol"`
	Bid          string    `json:"bid"`
	Ask          string    `json:"ask"`
	Mid          string    `json:"mid"`
	Spread       string    `json:"spread"`
	SpreadPct    string    `json:"spread_fraction"`
	SourceTime   time.Time `json:"source_time"`
	IngestedAt   time.Time `json:"ingested_at"`
	AgeSeconds   float64   `json:"age_seconds"`
	Provider     string    `json:"provider"`
	Health       string    `json:"health"`
	Tradable     bool      `json:"tradable_by_automation"`
}

// handleQuotes returns the latest prices with their health verdicts.
func (s *Server) handleQuotes(w http.ResponseWriter, r *http.Request) {
	quotes, err := s.store.Market.LatestQuotes(r.Context())
	if err != nil {
		writeStoreError(w, r, err, "No quotes available.")
		return
	}
	now := s.clock.Now()
	out := make([]quoteResponse, 0, len(quotes))
	for _, q := range quotes {
		health := "unknown"
		tradable := false
		if h, ok := s.ingestor.Health(q.InstrumentID); ok {
			health = string(h.State)
			tradable = h.Healthy()
		}
		out = append(out, quoteResponse{
			InstrumentID: q.InstrumentID, Symbol: q.Symbol,
			Bid: q.Bid.String(), Ask: q.Ask.String(), Mid: q.Mid().String(),
			Spread: q.Spread().String(), SpreadPct: q.SpreadFraction().StringFixed(6),
			SourceTime: q.SourceTime, IngestedAt: q.IngestedAt,
			AgeSeconds: now.Sub(q.IngestedAt).Seconds(), Provider: q.Provider,
			Health: health, Tradable: tradable,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"quotes": out, "as_of": now})
}

// handleMarketHealth returns the data-quality verdict per instrument.
func (s *Server) handleMarketHealth(w http.ResponseWriter, r *http.Request) {
	all := s.ingestor.AllHealth()
	type healthResponse struct {
		InstrumentID string   `json:"instrument_id"`
		Symbol       string   `json:"symbol"`
		State        string   `json:"state"`
		Issues       []string `json:"issues"`
		QuoteAgeMS   int64    `json:"quote_age_ms"`
		SpreadPct    string   `json:"spread_fraction"`
		Provider     string   `json:"provider"`
		Tradable     bool     `json:"tradable_by_automation"`
	}
	out := make([]healthResponse, 0, len(all))
	for _, h := range all {
		issues := make([]string, 0, len(h.Issues))
		for _, i := range h.Issues {
			issues = append(issues, string(i))
		}
		out = append(out, healthResponse{
			InstrumentID: h.InstrumentID, Symbol: h.Symbol, State: string(h.State),
			Issues: issues, QuoteAgeMS: h.QuoteAge.Milliseconds(),
			SpreadPct: h.SpreadPct.StringFixed(6), Provider: h.Provider,
			Tradable: h.Healthy(),
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"health": out})
}

// handleBars returns candles for charting and research.
func (s *Server) handleBars(w http.ResponseWriter, r *http.Request) {
	instrumentID := r.URL.Query().Get("instrument_id")
	if instrumentID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_parameter", "instrument_id is required.")
		return
	}
	tf, err := domain.ParseTimeframe(defaultString(r.URL.Query().Get("timeframe"), "1h"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_timeframe",
			"timeframe must be one of 1m, 5m, 15m, 1h, 4h, 1d.")
		return
	}
	bars, err := s.store.Market.Bars(r.Context(), instrumentID, tf, intParam(r, "limit", 500, 5000), true)
	if err != nil {
		writeStoreError(w, r, err, "No bars available.")
		return
	}
	type barResponse struct {
		OpenTime time.Time `json:"open_time"`
		Open     string    `json:"open"`
		High     string    `json:"high"`
		Low      string    `json:"low"`
		Close    string    `json:"close"`
		Volume   string    `json:"volume"`
	}
	out := make([]barResponse, 0, len(bars))
	for _, b := range bars {
		out = append(out, barResponse{
			OpenTime: b.OpenTime, Open: b.Open.String(), High: b.High.String(),
			Low: b.Low.String(), Close: b.Close.String(), Volume: b.Volume.String(),
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"instrument_id": instrumentID, "timeframe": string(tf), "bars": out,
	})
}

// handleMarketStatus reports session state and the next close.
func (s *Server) handleMarketStatus(w http.ResponseWriter, r *http.Request) {
	now := s.clock.Now()
	status := s.marketClock.Status(now)
	sessions := s.marketClock.ActiveSessions(now)
	sessionNames := make([]string, 0, len(sessions))
	for _, sn := range sessions {
		sessionNames = append(sessionNames, string(sn))
	}
	resp := map[string]any{
		"status":          string(status),
		"tradable":        status.Tradable(),
		"active_sessions": sessionNames,
		"server_time":     now,
	}
	if next, ok := s.marketClock.NextClose(now); ok {
		resp["next_close"] = next
	}
	writeJSON(w, r, http.StatusOK, resp)
}

// handleConnections reports provider and broker connectivity.
//
// Credentials are never included: only which broker, which mode, and whether
// it is reachable.
func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	type connection struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Mode      string `json:"mode"`
		Status    string `json:"status"`
		Detail    string `json:"detail,omitempty"`
		LatencyMS int64  `json:"latency_ms,omitempty"`
	}
	out := []connection{}

	for _, name := range s.brokers.Names() {
		adapter, err := s.brokers.Get(name)
		if err != nil {
			continue
		}
		h := adapter.Health(r.Context())
		out = append(out, connection{
			Name: name, Kind: "broker", Mode: s.cfg.ExecutionMode,
			Status: string(h.State), Detail: h.Message, LatencyMS: h.LatencyMS,
		})
	}

	quantStatus := "unavailable"
	quantDetail := "research service unreachable; signal generation is paused"
	if s.quant != nil {
		if err := s.quant.Health(r.Context()); err == nil {
			quantStatus, quantDetail = "up", "research service reachable"
		}
	}
	out = append(out,
		connection{Name: "quant", Kind: "research", Mode: s.cfg.ExecutionMode,
			Status: quantStatus, Detail: quantDetail},
		connection{Name: "mock", Kind: "market_data", Mode: s.cfg.ExecutionMode,
			Status: "up", Detail: "deterministic development price generator"},
	)

	// The calendar and news providers report their own names, so this list
	// says which implementation is actually loaded rather than asserting one.
	calendarName, newsName := "unconfigured", "unconfigured"
	calendarStatus, newsStatus := "unavailable", "unavailable"
	if s.econData != nil {
		calendarName = s.econData.CalendarProviderName()
		newsName = s.econData.NewsProviderName()
		calendarStatus, newsStatus = "up", "up"
	}
	out = append(out,
		connection{Name: calendarName, Kind: "calendar", Mode: s.cfg.ExecutionMode,
			Status: calendarStatus, Detail: "deterministic development fixtures, not a licensed feed"},
		connection{Name: newsName, Kind: "news", Mode: s.cfg.ExecutionMode,
			Status: newsStatus, Detail: "deterministic development fixtures, not a licensed feed"},
	)
	writeJSON(w, r, http.StatusOK, map[string]any{"connections": out})
}

// handleNotifications lists the caller's alerts.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	notifications, err := s.store.Control.ListNotifications(r.Context(), p.User.ID,
		r.URL.Query().Get("unread") == "true", intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "No notifications found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"notifications": notifications})
}

// handleMarkNotificationRead marks one of the caller's notifications read.
func (s *Server) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, ok := parseUUID(w, r, chi.URLParam(r, "notificationID"), "Notification id")
	if !ok {
		return
	}
	if err := s.store.Control.MarkNotificationRead(r.Context(), p.User.ID, id); err != nil {
		writeStoreError(w, r, err, "Notification not found.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "read"})
}

// handleReconciliationStatus reports the last run and open issues.
//
// accountForOperations, for the same reason as the run endpoint: this is the
// operations surface, and an admin must be able to read the state of the
// account they are being asked to fix.
func (s *Server) handleReconciliationStatus(w http.ResponseWriter, r *http.Request) {
	account, ok := s.accountForOperations(w, r, "accountID")
	if !ok {
		return
	}
	blocked, critical, err := s.reconciler.AutomationBlocked(r.Context(), account.ID)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	// Reads the ISSUE store, not the dropped reconciliation_discrepancies
	// table. Migration 0010 replaced that table and this handler kept
	// querying it, which produced a 500 on the dashboard -- the one page every
	// session loads first.
	unresolved, err := s.store.Reconcile.OpenIssues(r.Context(), account.ID)
	if err != nil {
		writeStoreError(w, r, err, "Account not found.")
		return
	}
	resp := map[string]any{
		"automation_blocked": blocked,
		// Kept under its original name so existing clients do not break; the
		// richer view is /reconciliation/{id}/issues.
		"critical_discrepancies": len(critical),
		"unresolved":             issueViews(unresolved),
	}
	if last, err := s.store.Research.LatestReconciliation(r.Context(), account.ID); err == nil {
		resp["last_run"] = last
	}
	writeJSON(w, r, http.StatusOK, resp)
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// uuidPtr converts a UUID to a pointer, or nil for the zero value.
func uuidPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

var _ = store.ErrNotFound
