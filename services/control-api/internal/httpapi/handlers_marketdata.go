package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/marketdata"
)

// Market-data catalog, coverage and acquisition controls.
//
// # What the browser may reach
//
// These routes, and only these. The browser never talks to a data provider:
// the API key lives in this process, the outbound request is made here, and
// what reaches the terminal is Vantage's own stored data. A front end that
// could call a provider directly would put the key in a bundle.
//
// # Why acquisition is admin-only
//
// Backfill, sync and repair spend a metered external quota and write to the
// bar series that every strategy reads. Reading market data is broadly
// permitted; changing it is not. The role is enforced by the router group
// rather than by a check inside a handler, so it cannot be lost in a refactor.
//
// # No arbitrary endpoints
//
// A caller names an instrument, a timeframe and a range. It cannot name a URL,
// a provider path or a symbol. The provider base URL is fixed configuration
// and the vendor symbol comes from the mapping table, because an endpoint a
// client could steer is an SSRF primitive pointed at whatever this server can
// reach.

// maxBarQuery bounds any single bar query.
//
// "Give me every bar ever" is not a request this API answers. Ten years of
// hourly gold is roughly 60 000 rows, which is a slow query, a large response
// and a browser that stops responding while it parses.
const maxBarQuery = 5000

// maxBackfillDays bounds one backfill request.
//
// Generous -- twenty years -- but finite. The chunker would otherwise accept a
// range starting in 1970 and spend a very long time proving the provider has
// nothing there.
const maxBackfillDays = 365 * 20

func (s *Server) handleMarketDataInstruments(w http.ResponseWriter, r *http.Request) {
	if s.marketSync == nil {
		writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable",
			"No market-data provider is configured.")
		return
	}
	type entry struct {
		InstrumentID string   `json:"instrument_id"`
		Provider     string   `json:"provider"`
		Timeframes   []string `json:"timeframes"`
	}
	out := make([]entry, 0, len(s.marketSymbols.Instruments()))
	for _, id := range s.marketSymbols.Instruments() {
		out = append(out, entry{
			InstrumentID: id,
			Provider:     s.marketProviderName,
			// One timeframe, deliberately. The architecture carries others;
			// this milestone proves one end to end rather than five partly.
			Timeframes: []string{"1h"},
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"instruments": out})
}

// handleMarketDataStatus reports provider health. Never carries the API key.
func (s *Server) handleMarketDataStatus(w http.ResponseWriter, r *http.Request) {
	if s.marketProvider == nil {
		writeJSON(w, r, http.StatusOK, map[string]any{
			"provider": map[string]any{
				"provider":   "none",
				"state":      string(marketdata.ProviderMisconfigured),
				"configured": false,
			},
		})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"provider": s.marketProvider.Health()})
}

// handleMarketDataCoverage answers "what does Vantage actually have".
func (s *Server) handleMarketDataCoverage(w http.ResponseWriter, r *http.Request) {
	instrumentID, tf, ok := s.instrumentAndTimeframe(w, r)
	if !ok {
		return
	}

	coverage, err := s.store.Market.Coverage(r.Context(), instrumentID, tf)
	if err != nil {
		writeStoreError(w, r, err, "Coverage is unavailable.")
		return
	}

	// Gaps are reported only across the range actually held. Asking beyond it
	// would report the absence of data nobody ever acquired as damage.
	//
	// And a CLOSED MARKET is not a gap. The venue shuts every weekend and for
	// an hour each weekday; reporting those as missing data makes a complete
	// dataset look riddled with holes and buries the one real gap among fifty
	// weekends. The store's gap query is deliberately structural -- it has no
	// market clock -- so the classification happens here, where one exists.
	gaps := []map[string]any{}
	closedMarketGaps := 0
	if coverage.EarliestBar != nil && coverage.LatestBar != nil {
		// Plus ONE BAR, not one nanosecond. FindGaps is half-open and Postgres
		// stores microseconds, so a nanosecond truncates back to the same
		// instant and the newest bar was excluded from the scan -- which meant
		// the MOST RECENT gap, the one immediately before the latest bar, was
		// never reported. A feed that stopped for three days and resumed showed
		// no gaps at all.
		barDur, derr := tf.Duration()
		if derr != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_timeframe",
				"timeframe must be one of 1m, 5m, 15m, 1h, 4h, 1d.")
			return
		}
		found, gerr := s.store.Market.FindGaps(
			r.Context(), instrumentID, tf, *coverage.EarliestBar, coverage.LatestBar.Add(barDur))
		if gerr != nil {
			writeStoreError(w, r, gerr, "Gap detection failed.")
			return
		}
		for _, g := range found {
			if s.marketClock != nil && !anyBarTradable(s.marketClock, g.From, g.To, tf) {
				closedMarketGaps++
				continue
			}
			gaps = append(gaps, map[string]any{
				"from": g.From, "to": g.To, "missing_bars": g.MissingBars,
			})
		}
	}

	segments, err := s.store.Market.Segments(r.Context(), instrumentID, tf, 20)
	if err != nil {
		writeStoreError(w, r, err, "Acquisition history is unavailable.")
		return
	}

	writeJSON(w, r, http.StatusOK, map[string]any{
		"coverage": coverage,
		"gaps":     gaps,
		// Reported rather than dropped: an operator seeing "0 gaps" should be
		// able to tell that closures were considered, not that the check was
		// skipped.
		"closed_market_gaps": closedMarketGaps,
		"segments":           segments,
	})
}

// anyBarTradable reports whether a missing run covers any open-market bar.
//
// Bounded, because a multi-year hole is open somewhere and walking every hour
// to prove it would cost more than the answer is worth.
func anyBarTradable(
	clock *domain.MarketClock, from, to time.Time, tf domain.Timeframe,
) bool {
	dur, err := tf.Duration()
	if err != nil {
		return true
	}
	const probeLimit = 2000
	probes := 0
	for t := from; t.Before(to) && probes < probeLimit; t = t.Add(dur) {
		if clock.Status(t).Tradable() {
			return true
		}
		probes++
	}
	return probes >= probeLimit
}

// handleMarketDataBackfill acquires a named historical range.
func (s *Server) handleMarketDataBackfill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstrumentID string `json:"instrument_id"`
		Timeframe    string `json:"timeframe"`
		Start        string `json:"start"`
		End          string `json:"end"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	instrumentID, tf, ok := s.validateInstrumentTimeframe(w, r, body.InstrumentID, body.Timeframe)
	if !ok {
		return
	}
	start, end, ok := s.parseRange(w, r, body.Start, body.End)
	if !ok {
		return
	}
	syncer, ok := s.requireSyncer(w, r)
	if !ok {
		return
	}

	result, err := syncer.Backfill(r.Context(), instrumentID, tf, start, end)
	s.writeSyncResult(w, r, result, err)
}

// handleMarketDataSync fetches only what is newer than the latest stored bar.
func (s *Server) handleMarketDataSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstrumentID string `json:"instrument_id"`
		Timeframe    string `json:"timeframe"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	instrumentID, tf, ok := s.validateInstrumentTimeframe(w, r, body.InstrumentID, body.Timeframe)
	if !ok {
		return
	}
	syncer, ok := s.requireSyncer(w, r)
	if !ok {
		return
	}

	result, err := syncer.Sync(r.Context(), instrumentID, tf)
	s.writeSyncResult(w, r, result, err)
}

// handleMarketDataRepair re-requests the missing ranges, and nothing else.
func (s *Server) handleMarketDataRepair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstrumentID string `json:"instrument_id"`
		Timeframe    string `json:"timeframe"`
		Start        string `json:"start"`
		End          string `json:"end"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	instrumentID, tf, ok := s.validateInstrumentTimeframe(w, r, body.InstrumentID, body.Timeframe)
	if !ok {
		return
	}
	start, end, ok := s.parseRange(w, r, body.Start, body.End)
	if !ok {
		return
	}
	syncer, ok := s.requireSyncer(w, r)
	if !ok {
		return
	}

	result, err := syncer.Repair(r.Context(), instrumentID, tf, start, end)
	s.writeSyncResult(w, r, result, err)
}

// --- shared validation --------------------------------------------------------

func (s *Server) requireSyncer(w http.ResponseWriter, r *http.Request) (*marketdata.Syncer, bool) {
	if s.marketSync == nil {
		writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable",
			"No market-data provider is configured. Set TWELVE_DATA_API_KEY and restart.")
		return nil, false
	}
	if s.marketProvider != nil && !s.marketProvider.Configured() {
		// A distinct code, because "no key" and "provider down" call for
		// different actions and an operator should not have to guess which.
		writeError(w, r, http.StatusServiceUnavailable, "provider_misconfigured",
			"TWELVE_DATA_API_KEY is not configured, so no data can be acquired.")
		return nil, false
	}
	return s.marketSync, true
}

func (s *Server) instrumentAndTimeframe(
	w http.ResponseWriter, r *http.Request,
) (string, domain.Timeframe, bool) {
	return s.validateInstrumentTimeframe(w, r,
		r.URL.Query().Get("instrument_id"), r.URL.Query().Get("timeframe"))
}

func (s *Server) validateInstrumentTimeframe(
	w http.ResponseWriter, r *http.Request, instrumentID, timeframe string,
) (string, domain.Timeframe, bool) {
	if instrumentID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_parameter",
			"instrument_id is required.")
		return "", "", false
	}
	tf, err := domain.ParseTimeframe(defaultString(timeframe, "1h"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_timeframe",
			"timeframe must be one of 1m, 5m, 15m, 1h, 4h, 1d.")
		return "", "", false
	}
	// The instrument must exist HERE, not merely be a string the caller sent.
	// Passing an unknown id through to a provider request would turn this
	// endpoint into a way to probe what the provider carries.
	if _, err := s.store.Market.Instrument(r.Context(), instrumentID); err != nil {
		writeError(w, r, http.StatusNotFound, "instrument_not_found",
			"That instrument is not in this platform's universe.")
		return "", "", false
	}
	return instrumentID, tf, true
}

func (s *Server) parseRange(
	w http.ResponseWriter, r *http.Request, start, end string,
) (time.Time, time.Time, bool) {
	from, err := time.Parse(time.RFC3339, start)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_range",
			"start must be an RFC3339 timestamp.")
		return time.Time{}, time.Time{}, false
	}
	to, err := time.Parse(time.RFC3339, end)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_range",
			"end must be an RFC3339 timestamp.")
		return time.Time{}, time.Time{}, false
	}
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		writeError(w, r, http.StatusBadRequest, "invalid_range",
			"end must be after start.")
		return time.Time{}, time.Time{}, false
	}
	if to.Sub(from) > maxBackfillDays*24*time.Hour {
		writeError(w, r, http.StatusBadRequest, "range_too_large",
			"That range is longer than this endpoint accepts. Request it in parts.")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func (s *Server) writeSyncResult(
	w http.ResponseWriter, r *http.Request, result marketdata.Result, err error,
) {
	switch {
	case err == nil:
		writeJSON(w, r, http.StatusOK, result)
	case errors.Is(err, marketdata.ErrProviderNotConfigured):
		writeError(w, r, http.StatusServiceUnavailable, "provider_misconfigured",
			"TWELVE_DATA_API_KEY is not configured, so no data can be acquired.")
	case errors.Is(err, marketdata.ErrUnsupportedInstrument):
		writeError(w, r, http.StatusBadRequest, "instrument_unsupported",
			"The configured provider does not carry that instrument.")
	default:
		// The provider's own message is not echoed. It is outside our control
		// and could carry anything, including a reflected credential.
		writeError(w, r, http.StatusBadGateway, "acquisition_failed",
			"Market-data acquisition failed. See the segment records for detail.")
	}
}

// handleMarketDataSnapshot freezes a range of stored bars as a research dataset.
//
// ADMIN, like the acquisition routes: it writes a file into the research
// directory and creates an identity that later research runs will cite. A
// reader can list and fetch snapshots without privilege; minting one is a
// different act.
func (s *Server) handleMarketDataSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InstrumentID string `json:"instrument_id"`
		Timeframe    string `json:"timeframe"`
		Start        string `json:"start"`
		End          string `json:"end"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	instrumentID, tf, ok := s.validateInstrumentTimeframe(w, r, body.InstrumentID, body.Timeframe)
	if !ok {
		return
	}
	start, end, ok := s.parseRange(w, r, body.Start, body.End)
	if !ok {
		return
	}
	if s.marketSync == nil {
		writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable",
			"No market-data layer is configured.")
		return
	}

	// A snapshot reads bars already stored; it does NOT require a configured
	// provider. Data that arrived by any path can be frozen for research.
	result, err := s.marketSync.Snapshot(r.Context(), marketdata.SnapshotRequest{
		InstrumentID: instrumentID,
		Timeframe:    tf,
		From:         start,
		To:           end,
		ResearchDir:  s.researchDir,
		CodeSHA:      s.commit,
	})
	switch {
	case err == nil:
		writeJSON(w, r, http.StatusCreated, result)
	case errors.Is(err, marketdata.ErrNoBarsInRange):
		writeError(w, r, http.StatusNotFound, "no_bars",
			"No stored bars in that range. Backfill it first.")
	case errors.Is(err, marketdata.ErrQualityRefused):
		// Deliberately explicit. INVALID data must never become a research
		// dataset, and an operator needs to know that is why rather than
		// guessing at an empty result.
		writeError(w, r, http.StatusUnprocessableEntity, "quality_refused",
			"Those bars did not pass quality validation and cannot become a research dataset.")
	default:
		writeStoreError(w, r, err, "Could not create the research dataset.")
	}
}

// handleResearchDatasets lists snapshot identities.
func (s *Server) handleResearchDatasets(w http.ResponseWriter, r *http.Request) {
	datasets, err := s.store.Market.ListResearchDatasets(r.Context(), intParam(r, "limit", 50, 200))
	if err != nil {
		writeStoreError(w, r, err, "Research datasets are unavailable.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"datasets": datasets})
}
