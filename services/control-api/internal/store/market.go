package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/money"
)

// MarketStore persists instruments, prices and data-quality observations.
type MarketStore struct{ pool *db.Pool }

const instrumentColumns = `id, symbol, name, asset_class, base_currency, quote_currency, enabled,
	session_calendar_id, contract_size, price_precision, tick_size, quantity_precision,
	min_quantity, max_quantity, quantity_step, margin_rate, max_leverage,
	supported_order_types, commission_per_lot, swap_long_per_lot, swap_short_per_lot`

func scanInstrument(row pgx.Row) (domain.Instrument, error) {
	var i domain.Instrument
	var baseCcy, quoteCcy string
	var orderTypes []string
	err := row.Scan(&i.ID, &i.Symbol, &i.Name, &i.Class, &baseCcy, &quoteCcy, &i.Enabled,
		&i.SessionCalendarID, &i.Spec.ContractSize, &i.Spec.PricePrecision, &i.Spec.TickSize,
		&i.Spec.QuantityPrecision, &i.Spec.MinQuantity, &i.Spec.MaxQuantity, &i.Spec.QuantityStep,
		&i.Spec.MarginRate, &i.Spec.MaxLeverage, &orderTypes, &i.Spec.CommissionPerLot,
		&i.Spec.SwapLongPerLot, &i.Spec.SwapShortPerLot)
	if err != nil {
		return domain.Instrument{}, mapError(err)
	}
	i.BaseCcy = money.Currency(baseCcy)
	i.QuoteCcy = money.Currency(quoteCcy)
	i.Spec.SupportedOrderTypes = make(domain.OrderTypeSet, 0, len(orderTypes))
	for _, t := range orderTypes {
		i.Spec.SupportedOrderTypes = append(i.Spec.SupportedOrderTypes, domain.OrderType(t))
	}
	return i, nil
}

// UpsertInstrument inserts or updates an instrument specification.
func (s *MarketStore) UpsertInstrument(ctx context.Context, i domain.Instrument) error {
	orderTypes := make([]string, 0, len(i.Spec.SupportedOrderTypes))
	for _, t := range i.Spec.SupportedOrderTypes {
		orderTypes = append(orderTypes, string(t))
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO instruments (id, symbol, name, asset_class, base_currency, quote_currency,
			enabled, session_calendar_id, contract_size, price_precision, tick_size,
			quantity_precision, min_quantity, max_quantity, quantity_step, margin_rate,
			max_leverage, supported_order_types, commission_per_lot, swap_long_per_lot, swap_short_per_lot)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (id) DO UPDATE SET
			symbol = EXCLUDED.symbol, name = EXCLUDED.name, asset_class = EXCLUDED.asset_class,
			base_currency = EXCLUDED.base_currency, quote_currency = EXCLUDED.quote_currency,
			enabled = EXCLUDED.enabled, session_calendar_id = EXCLUDED.session_calendar_id,
			contract_size = EXCLUDED.contract_size, price_precision = EXCLUDED.price_precision,
			tick_size = EXCLUDED.tick_size, quantity_precision = EXCLUDED.quantity_precision,
			min_quantity = EXCLUDED.min_quantity, max_quantity = EXCLUDED.max_quantity,
			quantity_step = EXCLUDED.quantity_step, margin_rate = EXCLUDED.margin_rate,
			max_leverage = EXCLUDED.max_leverage, supported_order_types = EXCLUDED.supported_order_types,
			commission_per_lot = EXCLUDED.commission_per_lot,
			swap_long_per_lot = EXCLUDED.swap_long_per_lot,
			swap_short_per_lot = EXCLUDED.swap_short_per_lot,
			updated_at = now()`,
		i.ID, i.Symbol, i.Name, i.Class, string(i.BaseCcy), string(i.QuoteCcy), i.Enabled,
		i.SessionCalendarID, i.Spec.ContractSize, i.Spec.PricePrecision, i.Spec.TickSize,
		i.Spec.QuantityPrecision, i.Spec.MinQuantity, i.Spec.MaxQuantity, i.Spec.QuantityStep,
		i.Spec.MarginRate, i.Spec.MaxLeverage, orderTypes, i.Spec.CommissionPerLot,
		i.Spec.SwapLongPerLot, i.Spec.SwapShortPerLot)
	return mapError(err)
}

// Instrument loads one instrument.
func (s *MarketStore) Instrument(ctx context.Context, id string) (domain.Instrument, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+instrumentColumns+` FROM instruments WHERE id = $1`, id)
	return scanInstrument(row)
}

// ListInstruments returns instruments, optionally only the enabled ones.
func (s *MarketStore) ListInstruments(ctx context.Context, enabledOnly bool) ([]domain.Instrument, error) {
	q := `SELECT ` + instrumentColumns + ` FROM instruments`
	if enabledOnly {
		q += ` WHERE enabled = TRUE`
	}
	q += ` ORDER BY symbol`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Instrument
	for rows.Next() {
		i, err := scanInstrument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, mapError(rows.Err())
}

// Holidays loads a calendar's closure dates.
func (s *MarketStore) Holidays(ctx context.Context, calendarID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT to_char(holiday_date, 'YYYY-MM-DD'), reason FROM market_holidays WHERE calendar_id = $1`,
		calendarID)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var d, r string
		if err := rows.Scan(&d, &r); err != nil {
			return nil, mapError(err)
		}
		out[d] = r
	}
	return out, mapError(rows.Err())
}

// UpsertHoliday records a market closure date.
func (s *MarketStore) UpsertHoliday(ctx context.Context, calendarID, date, reason string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO market_holidays (calendar_id, holiday_date, reason)
		VALUES ($1, $2::date, $3)
		ON CONFLICT (calendar_id, holiday_date) DO UPDATE SET reason = EXCLUDED.reason`,
		calendarID, date, reason)
	return mapError(err)
}

// ---------------------------------------------------------------------------
// Quotes
// ---------------------------------------------------------------------------

// RecordQuote appends a quote and updates the latest-quote row, carrying the
// displaced values forward so out-of-order and duplicate ticks stay detectable.
func (s *MarketStore) RecordQuote(ctx context.Context, q domain.Quote) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO market_quotes (instrument_id, bid, ask, source_time, ingested_at, provider)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			q.InstrumentID, q.Bid, q.Ask, q.SourceTime, q.IngestedAt, q.Provider); err != nil {
			return mapError(err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO market_quotes_latest
				(instrument_id, bid, ask, source_time, ingested_at, provider)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (instrument_id) DO UPDATE SET
				prev_bid = market_quotes_latest.bid,
				prev_ask = market_quotes_latest.ask,
				prev_source_time = market_quotes_latest.source_time,
				bid = EXCLUDED.bid,
				ask = EXCLUDED.ask,
				source_time = EXCLUDED.source_time,
				ingested_at = EXCLUDED.ingested_at,
				provider = EXCLUDED.provider`,
			q.InstrumentID, q.Bid, q.Ask, q.SourceTime, q.IngestedAt, q.Provider)
		return mapError(err)
	})
}

// LatestQuote returns the current quote and the one it replaced.
func (s *MarketStore) LatestQuote(ctx context.Context, instrumentID string) (domain.Quote, *domain.Quote, error) {
	var q domain.Quote
	var prevBid, prevAsk *decimal.Decimal
	var prevTime *time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT l.instrument_id, i.symbol, l.bid, l.ask, l.source_time, l.ingested_at, l.provider,
		       l.prev_bid, l.prev_ask, l.prev_source_time
		FROM market_quotes_latest l
		JOIN instruments i ON i.id = l.instrument_id
		WHERE l.instrument_id = $1`, instrumentID).
		Scan(&q.InstrumentID, &q.Symbol, &q.Bid, &q.Ask, &q.SourceTime, &q.IngestedAt, &q.Provider,
			&prevBid, &prevAsk, &prevTime)
	if err != nil {
		return domain.Quote{}, nil, mapError(err)
	}

	var prev *domain.Quote
	if prevBid != nil && prevAsk != nil && prevTime != nil {
		prev = &domain.Quote{
			InstrumentID: q.InstrumentID,
			Symbol:       q.Symbol,
			Bid:          *prevBid,
			Ask:          *prevAsk,
			SourceTime:   *prevTime,
			Provider:     q.Provider,
		}
	}
	return q, prev, nil
}

// LatestQuotes returns the current quote for every instrument that has one.
func (s *MarketStore) LatestQuotes(ctx context.Context) ([]domain.Quote, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.instrument_id, i.symbol, l.bid, l.ask, l.source_time, l.ingested_at, l.provider
		FROM market_quotes_latest l
		JOIN instruments i ON i.id = l.instrument_id
		ORDER BY i.symbol`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.Quote
	for rows.Next() {
		var q domain.Quote
		if err := rows.Scan(&q.InstrumentID, &q.Symbol, &q.Bid, &q.Ask,
			&q.SourceTime, &q.IngestedAt, &q.Provider); err != nil {
			return nil, mapError(err)
		}
		out = append(out, q)
	}
	return out, mapError(rows.Err())
}

// ---------------------------------------------------------------------------
// Bars
// ---------------------------------------------------------------------------

// UpsertBars writes bars in one batch. Re-ingesting an existing bar updates it,
// which is correct for a bar that was incomplete when first stored.
func (s *MarketStore) UpsertBars(ctx context.Context, bars []domain.Bar) error {
	if len(bars) == 0 {
		return nil
	}
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, b := range bars {
			batch.Queue(`
				INSERT INTO market_bars (instrument_id, timeframe, open_time, close_time,
					open, high, low, close, volume, complete, provider)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
				ON CONFLICT (instrument_id, timeframe, open_time) DO UPDATE SET
					close_time = EXCLUDED.close_time, open = EXCLUDED.open,
					high = EXCLUDED.high, low = EXCLUDED.low, close = EXCLUDED.close,
					volume = EXCLUDED.volume, complete = EXCLUDED.complete,
					provider = EXCLUDED.provider, ingested_at = now()`,
				b.InstrumentID, b.Timeframe, b.OpenTime, b.CloseTime,
				b.Open, b.High, b.Low, b.Close, b.Volume, b.Complete, b.Provider)
		}
		res := tx.SendBatch(ctx, batch)
		defer res.Close()
		for range bars {
			if _, err := res.Exec(); err != nil {
				return mapError(err)
			}
		}
		return nil
	})
}

// Bars returns completed bars in ascending time order.
//
// Only complete bars are returned by default. A strategy that reads the
// in-progress bar is reading the future relative to its own decision point,
// which is the single most common way a backtest becomes fiction.
func (s *MarketStore) Bars(ctx context.Context, instrumentID string, tf domain.Timeframe, limit int, completeOnly bool) ([]domain.Bar, error) {
	if limit <= 0 || limit > 10000 {
		limit = 500
	}
	q := `
		SELECT instrument_id, timeframe, open_time, close_time, open, high, low, close,
		       volume, complete, provider
		FROM (
			SELECT * FROM market_bars
			WHERE instrument_id = $1 AND timeframe = $2`
	if completeOnly {
		q += ` AND complete = TRUE`
	}
	q += `
			ORDER BY open_time DESC
			LIMIT $3
		) recent
		ORDER BY open_time ASC`

	rows, err := s.pool.Query(ctx, q, instrumentID, string(tf), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanBars(rows)
}

// BarsInRange returns bars whose open time falls in [from, to).
func (s *MarketStore) BarsInRange(ctx context.Context, instrumentID string, tf domain.Timeframe, from, to time.Time) ([]domain.Bar, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT instrument_id, timeframe, open_time, close_time, open, high, low, close,
		       volume, complete, provider
		FROM market_bars
		WHERE instrument_id = $1 AND timeframe = $2 AND open_time >= $3 AND open_time < $4
		  AND complete = TRUE
		ORDER BY open_time ASC`, instrumentID, string(tf), from, to)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	return scanBars(rows)
}

func scanBars(rows pgx.Rows) ([]domain.Bar, error) {
	var out []domain.Bar
	for rows.Next() {
		var b domain.Bar
		if err := rows.Scan(&b.InstrumentID, &b.Timeframe, &b.OpenTime, &b.CloseTime,
			&b.Open, &b.High, &b.Low, &b.Close, &b.Volume, &b.Complete, &b.Provider); err != nil {
			return nil, mapError(err)
		}
		out = append(out, b)
	}
	return out, mapError(rows.Err())
}

// CountBarGaps reports how many expected bars are missing from a range. A
// gap-riddled series produces indicators that look plausible and are wrong.
func (s *MarketStore) CountBarGaps(ctx context.Context, instrumentID string, tf domain.Timeframe, from, to time.Time) (int, error) {
	d, err := tf.Duration()
	if err != nil {
		return 0, err
	}
	var present int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM market_bars
		WHERE instrument_id = $1 AND timeframe = $2 AND open_time >= $3 AND open_time < $4`,
		instrumentID, string(tf), from, to).Scan(&present); err != nil {
		return 0, mapError(err)
	}
	expected := int(to.Sub(from) / d)
	gaps := expected - present
	if gaps < 0 {
		return 0, nil
	}
	return gaps, nil
}

// ---------------------------------------------------------------------------
// Data-quality observations
// ---------------------------------------------------------------------------

// RecordHealth persists a data-quality verdict.
func (s *MarketStore) RecordHealth(ctx context.Context, h domain.MarketDataHealth) error {
	issues := make([]string, 0, len(h.Issues))
	for _, i := range h.Issues {
		issues = append(issues, string(i))
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO market_data_health
			(instrument_id, state, issues, quote_age_ms, spread, spread_fraction, provider, evaluated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		h.InstrumentID, string(h.State), issues, h.QuoteAge.Milliseconds(),
		h.Spread, h.SpreadPct, h.Provider, h.EvaluatedAt)
	return mapError(err)
}

// ---------------------------------------------------------------------------
// FX
// ---------------------------------------------------------------------------

// UpsertFXRate records a conversion rate.
func (s *MarketStore) UpsertFXRate(ctx context.Context, base, quote money.Currency, rate decimal.Decimal, sourceTime time.Time, provider string) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fx_rates (base_currency, quote_currency, rate, source_time, provider)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (base_currency, quote_currency, source_time) DO UPDATE SET rate = EXCLUDED.rate`,
			string(base), string(quote), rate, sourceTime, provider); err != nil {
			return mapError(err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO fx_rates_latest (base_currency, quote_currency, rate, source_time, ingested_at, provider)
			VALUES ($1,$2,$3,$4,now(),$5)
			ON CONFLICT (base_currency, quote_currency) DO UPDATE SET
				rate = EXCLUDED.rate, source_time = EXCLUDED.source_time,
				ingested_at = now(), provider = EXCLUDED.provider`,
			string(base), string(quote), rate, sourceTime, provider)
		return mapError(err)
	})
}

// FXRate is a conversion rate with provenance.
type FXRate struct {
	Base       money.Currency
	Quote      money.Currency
	Rate       decimal.Decimal
	SourceTime time.Time
	IngestedAt time.Time
	Provider   string
}

// LatestFXRate returns the most recent rate for a currency pair. Only the
// direct pair is returned; inversion and cross-rate derivation are the FX
// service's job, so the rules for both are in one place and testable.
func (s *MarketStore) LatestFXRate(ctx context.Context, base, quote money.Currency) (FXRate, error) {
	var r FXRate
	var b, q string
	err := s.pool.QueryRow(ctx, `
		SELECT base_currency, quote_currency, rate, source_time, ingested_at, provider
		FROM fx_rates_latest WHERE base_currency = $1 AND quote_currency = $2`,
		string(base), string(quote)).
		Scan(&b, &q, &r.Rate, &r.SourceTime, &r.IngestedAt, &r.Provider)
	if err != nil {
		return FXRate{}, mapError(err)
	}
	r.Base = money.Currency(b)
	r.Quote = money.Currency(q)
	return r, nil
}

// PurgeReplayMarketData removes market data produced by a previous replay.
//
// # Why a replay must start from a clean market-data state
//
// This closes a defect that made a second replay run wrong in a way that
// looked like a broken clock.
//
// Quotes are upserted unconditionally, and the upsert moves the outgoing quote
// into `prev`. So starting a dataset dated 2 March while the database still
// held a quote from a previous run at 10 March produced two effects at once:
// the new quote read as `timestamp_regressed` against the old one, and for the
// rest of the run the orchestrator's own health check saw a stored quote eight
// days in the FUTURE and refused with `future_timestamp`. In one measured pass
// that was 415 of 1082 strategy runs skipped -- correct refusals, for a
// condition the harness had created.
//
// Bars matter for the same reason and worse: they are upserted by
// (instrument, timeframe, open_time), so bars from a DIFFERENT dataset would
// silently feed the indicators of this one.
//
// Only replay-provided rows are removed. Seeded and mock data are untouched,
// because a replay is a development tool and destroying the surrounding
// fixture would be a surprise.
func (s *MarketStore) PurgeReplayMarketData(ctx context.Context) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		for _, stmt := range []string{
			`DELETE FROM market_quotes_latest WHERE provider = 'replay'`,
			`DELETE FROM market_quotes WHERE provider = 'replay'`,
			`DELETE FROM market_bars WHERE provider = 'replay'`,
			`DELETE FROM market_data_health WHERE provider = 'replay'`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return mapError(err)
			}
		}
		return nil
	})
}
