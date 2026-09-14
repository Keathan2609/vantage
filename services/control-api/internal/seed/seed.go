// Package seed loads deterministic development data.
//
// Everything here is DEVELOPMENT ONLY and is refused outside a development or
// test environment by the calling command. The user passwords are published in
// the repository, the market history is generated rather than observed, and the
// calendar and news entries are fixtures.
//
// Nothing in this package fabricates a claim about real market performance. The
// seeded price history is a bounded random walk, and every surface that shows
// it labels it as simulated.
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/auth"
	brokermock "github.com/vantage/control-api/internal/broker/mock"
	"github.com/vantage/control-api/internal/db"
	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/econdata"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/marketdata"
	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/store"
)

// Deps are what the seeder needs.
type Deps struct {
	Store       *store.Store
	Pool        *db.Pool
	MockBroker  *brokermock.Broker
	Provider    *marketdata.MockProvider
	EconData    *econdata.Ingestor
	MarketClock *domain.MarketClock
	Clock       domain.Clock
	Log         *logging.Logger
}

// SeededUser is a development account.
type SeededUser struct {
	Role     string
	Email    string
	Password string
}

// Result summarises what was created.
type Result struct {
	Users             []SeededUser
	AccountName       string
	StartingBalance   string
	Currency          string
	Instruments       int
	PrimaryInstrument string
	Bars              int
	Timeframes        int
	Strategies        int
	PaperStrategies   int
	Events            int
	News              int
}

// Development credentials. Published deliberately: a reader must be able to
// tell at a glance that these are not secrets, and the seed command refuses to
// run outside development.
var devUsers = []struct {
	email, name, password string
	role                  domain.Role
}{
	{"admin@vantage.local", "Vantage Admin", "dev-Admin-Passw0rd!", domain.RoleAdmin},
	{"trader@vantage.local", "Vantage Trader", "dev-Trader-Passw0rd!", domain.RoleTrader},
	{"viewer@vantage.local", "Vantage Viewer", "dev-Viewer-Passw0rd!", domain.RoleViewer},
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// instrumentSeeds define the tradable universe.
//
// XAUUSD is the primary market and its specification mirrors a typical retail
// CFD venue: 100 troy ounces per lot, 0.01 lot minimum, 0.5% margin. Nothing in
// the platform's logic depends on these values — they are data.
var instrumentSeeds = []domain.Instrument{
	{
		ID: "XAUUSD", Symbol: "XAUUSD", Name: "Gold vs US Dollar",
		Class: domain.AssetClassMetal, BaseCcy: money.XAU, QuoteCcy: money.USD,
		Enabled: true, SessionCalendarID: "fx_metals_24x5",
		Spec: domain.InstrumentSpec{
			ContractSize: dec("100"), PricePrecision: 2, TickSize: dec("0.01"),
			QuantityPrecision: 2, MinQuantity: dec("0.01"), MaxQuantity: dec("50"),
			QuantityStep: dec("0.01"), MarginRate: dec("0.005"), MaxLeverage: dec("200"),
			SupportedOrderTypes: domain.OrderTypeSet{
				domain.OrderTypeMarket, domain.OrderTypeLimit, domain.OrderTypeStop},
			CommissionPerLot: dec("0"),
			// Financing: long gold pays, short gold receives a little.
			SwapLongPerLot: dec("-4.50"), SwapShortPerLot: dec("1.20"),
		},
	},
	{
		// Micro gold: ONE troy ounce per lot rather than a hundred.
		//
		// This instrument exists because of an arithmetic fact rather than a
		// preference. A 100-ounce contract has a 0.01-lot minimum, which is one
		// whole ounce — roughly R48,000 of notional at current prices. A R500
		// account cannot hold that, and the risk engine correctly refuses it.
		// With a 1-ounce contract the same 0.01-lot minimum is R480 of notional
		// and R2.40 of margin, which a small account can actually carry.
		//
		// Standard XAUUSD is deliberately kept in the universe as well: an
		// account that cannot afford it should be told so, not shielded from it.
		ID: "XAUUSD.m", Symbol: "XAUUSD.m", Name: "Gold vs US Dollar (Micro, 1oz)",
		Class: domain.AssetClassMetal, BaseCcy: money.XAU, QuoteCcy: money.USD,
		Enabled: true, SessionCalendarID: "fx_metals_24x5",
		Spec: domain.InstrumentSpec{
			ContractSize: dec("1"), PricePrecision: 2, TickSize: dec("0.01"),
			QuantityPrecision: 2, MinQuantity: dec("0.01"), MaxQuantity: dec("500"),
			QuantityStep: dec("0.01"), MarginRate: dec("0.005"), MaxLeverage: dec("200"),
			SupportedOrderTypes: domain.OrderTypeSet{
				domain.OrderTypeMarket, domain.OrderTypeLimit, domain.OrderTypeStop},
			CommissionPerLot: dec("0"),
			SwapLongPerLot:   dec("-0.045"), SwapShortPerLot: dec("0.012"),
		},
	},
	{
		// TEST_XAU exists so a PARTIAL FILL can be represented at all.
		//
		// # The arithmetic that forced it
		//
		// Every order the R500 account produces is 0.01 lots, which on
		// XAUUSD.m is simultaneously the minimum quantity AND the quantity
		// step. Forty percent of one step is 0.004 lots, which is not a
		// representable quantity, so the venue correctly declines to split the
		// order and the partial-fill path -- the one that touches the order
		// state machine, the position, the weighted average price, the fee
		// accrual and the ledger all at once -- could not be exercised at all.
		//
		// # What was NOT changed to fix it
		//
		// Not XAUUSD.m's minimum or step, and not the account's size or its
		// authority ceiling. Those are the assumptions under test; relaxing
		// them to make a test pass would be testing a different platform. The
		// authority's 0.10-lot ceiling still applies to this instrument, and
		// 0.10 lots here splits into 0.04 and 0.06 -- the 40/60 ratio -- on
		// quantities this instrument can actually express.
		//
		// # Why it is safe to have in the universe
		//
		// The seed refuses to run outside development, no strategy declares
		// it, and the trading authority grants it explicitly rather than by
		// wildcard. Nothing autonomous can reach it. Its economics are
		// deliberately synthetic and it must never appear in a research
		// result: the name says so.
		ID: "TEST_XAU", Symbol: "TEST_XAU",
		Name:  "TEST ONLY - Synthetic Gold (1/100 oz, fine lot step)",
		Class: domain.AssetClassMetal, BaseCcy: money.XAU, QuoteCcy: money.USD,
		Enabled: true, SessionCalendarID: "fx_metals_24x5",
		Spec: domain.InstrumentSpec{
			ContractSize: dec("0.01"), PricePrecision: 2, TickSize: dec("0.01"),
			// FOUR decimal places, which is the whole point: 0.10 lots can be
			// split into 0.04 and 0.06 and both are legal quantities.
			QuantityPrecision: 4, MinQuantity: dec("0.0001"), MaxQuantity: dec("10"),
			QuantityStep: dec("0.0001"), MarginRate: dec("0.005"), MaxLeverage: dec("200"),
			SupportedOrderTypes: domain.OrderTypeSet{
				domain.OrderTypeMarket, domain.OrderTypeLimit, domain.OrderTypeStop},
			CommissionPerLot: dec("0"),
			SwapLongPerLot:   dec("0"), SwapShortPerLot: dec("0"),
		},
	},
	{
		ID: "EURUSD", Symbol: "EURUSD", Name: "Euro vs US Dollar",
		Class: domain.AssetClassForex, BaseCcy: money.EUR, QuoteCcy: money.USD,
		Enabled: true, SessionCalendarID: "fx_metals_24x5",
		Spec: domain.InstrumentSpec{
			ContractSize: dec("100000"), PricePrecision: 5, TickSize: dec("0.00001"),
			QuantityPrecision: 2, MinQuantity: dec("0.01"), MaxQuantity: dec("100"),
			QuantityStep: dec("0.01"), MarginRate: dec("0.0033"), MaxLeverage: dec("300"),
			SupportedOrderTypes: domain.OrderTypeSet{
				domain.OrderTypeMarket, domain.OrderTypeLimit, domain.OrderTypeStop},
			CommissionPerLot: dec("0"),
			SwapLongPerLot:   dec("-6.20"), SwapShortPerLot: dec("2.10"),
		},
	},
	{
		ID: "XAGUSD", Symbol: "XAGUSD", Name: "Silver vs US Dollar",
		Class: domain.AssetClassMetal, BaseCcy: "XAG", QuoteCcy: money.USD,
		Enabled: true, SessionCalendarID: "fx_metals_24x5",
		Spec: domain.InstrumentSpec{
			ContractSize: dec("5000"), PricePrecision: 3, TickSize: dec("0.001"),
			QuantityPrecision: 2, MinQuantity: dec("0.01"), MaxQuantity: dec("50"),
			QuantityStep: dec("0.01"), MarginRate: dec("0.01"), MaxLeverage: dec("100"),
			SupportedOrderTypes: domain.OrderTypeSet{
				domain.OrderTypeMarket, domain.OrderTypeLimit, domain.OrderTypeStop},
			CommissionPerLot: dec("0"),
			SwapLongPerLot:   dec("-3.10"), SwapShortPerLot: dec("0.80"),
		},
	},
	{
		// Included because the example account is denominated in ZAR: the
		// platform must exercise a real cross-currency conversion path rather
		// than assuming the account and the instrument share a currency.
		ID: "USDZAR", Symbol: "USDZAR", Name: "US Dollar vs South African Rand",
		Class: domain.AssetClassForex, BaseCcy: money.USD, QuoteCcy: money.ZAR,
		Enabled: true, SessionCalendarID: "fx_metals_24x5",
		Spec: domain.InstrumentSpec{
			ContractSize: dec("100000"), PricePrecision: 4, TickSize: dec("0.0001"),
			QuantityPrecision: 2, MinQuantity: dec("0.01"), MaxQuantity: dec("50"),
			QuantityStep: dec("0.01"), MarginRate: dec("0.01"), MaxLeverage: dec("100"),
			SupportedOrderTypes: domain.OrderTypeSet{
				domain.OrderTypeMarket, domain.OrderTypeLimit},
			CommissionPerLot: dec("0"),
			SwapLongPerLot:   dec("-12.00"), SwapShortPerLot: dec("4.00"),
		},
	},
}

// strategySeeds register the strategy library.
//
// Each entry corresponds to an implementation in the research service. The
// registry is reconciled against that service at start-up, so a strategy
// present here but missing there is visible rather than silently inert.
var strategySeeds = []struct {
	key, name, description string
	family                 domain.StrategyFamily
	highRisk               bool
	enabled                bool
	promoteToPaper         bool
	timeframe              domain.Timeframe
	instruments            []string
	params                 string
	validRegimes           []string
}{
	{
		key: "ma_trend_crossover", name: "Moving Average Trend Crossover",
		description: "Enters in the direction of a fast/slow moving-average crossover, " +
			"filtered by an ATR-based volatility floor so it stands aside in dead markets.",
		family: domain.FamilyTrendFollowing, enabled: true, promoteToPaper: true,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params: `{"fast_period":20,"slow_period":50,"atr_period":14,"atr_stop_multiple":2.0,
		          "min_atr_fraction":0.0008,"target_multiple":3.0}`,
		validRegimes: []string{"TRENDING", "HIGH_VOLATILITY"},
	},
	{
		key: "donchian_breakout", name: "Donchian Channel Breakout",
		description: "Buys a break of the N-bar high and sells a break of the N-bar low, " +
			"with the stop placed at the opposite channel edge.",
		family: domain.FamilyBreakout, enabled: true, promoteToPaper: true,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params:       `{"channel_period":20,"atr_period":14,"atr_stop_multiple":2.0,"target_multiple":2.5}`,
		validRegimes: []string{"TRENDING", "HIGH_VOLATILITY"},
	},
	{
		key: "rsi_mean_reversion", name: "RSI Mean Reversion",
		description: "Fades exhaustion: buys deeply oversold and sells deeply overbought " +
			"readings, only while price sits inside a Bollinger envelope.",
		family: domain.FamilyMeanReversion, enabled: true, promoteToPaper: true,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params: `{"rsi_period":14,"oversold":28,"overbought":72,"bb_period":20,"bb_std":2.0,
		          "atr_period":14,"atr_stop_multiple":1.5,"target_multiple":1.5}`,
		validRegimes: []string{"RANGING", "LOW_VOLATILITY"},
	},
	{
		key: "macd_momentum", name: "MACD Momentum",
		description: "Follows MACD histogram expansion in the direction of the signal line.",
		family:      domain.FamilyMomentum, enabled: true, promoteToPaper: true,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params: `{"fast":12,"slow":26,"signal":9,"atr_period":14,"atr_stop_multiple":2.0,
		          "target_multiple":2.0}`,
		validRegimes: []string{"TRENDING"},
	},
	{
		key: "bollinger_zscore_reversion", name: "Bollinger Z-Score Reversion",
		description: "Trades statistical deviation from a rolling mean, sized by the " +
			"z-score's magnitude and requiring a minimum deviation to act.",
		family: domain.FamilyStatistical, enabled: true, promoteToPaper: true,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params:       `{"period":20,"entry_z":2.0,"exit_z":0.5,"atr_period":14,"atr_stop_multiple":1.5}`,
		validRegimes: []string{"RANGING"},
	},
	{
		key: "atr_volatility_regime", name: "ATR Volatility Regime",
		description: "Classifies volatility expansion and contraction, trading breakouts " +
			"only during expansion.",
		family: domain.FamilyVolatility, enabled: true, promoteToPaper: false,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params:       `{"atr_period":14,"lookback":50,"expansion_ratio":1.4,"atr_stop_multiple":2.0}`,
		validRegimes: []string{"HIGH_VOLATILITY"},
	},
	{
		key: "session_london_breakout", name: "London Session Breakout",
		description: "Trades the break of the Asian range as the London session opens, " +
			"and stands aside outside that window.",
		family: domain.FamilySession, enabled: true, promoteToPaper: false,
		timeframe: domain.TF15m, instruments: []string{"XAUUSD.m"},
		params: `{"range_start_hour":0,"range_end_hour":7,"session":"london",
		          "atr_period":14,"atr_stop_multiple":1.5}`,
		validRegimes: []string{"TRENDING", "HIGH_VOLATILITY"},
	},
	{
		key: "multi_timeframe_trend", name: "Multi-Timeframe Trend",
		description: "Requires the higher timeframe to agree with the lower-timeframe entry.",
		family:      domain.FamilyMultiTimeframe, enabled: true, promoteToPaper: false,
		timeframe: domain.TF15m, instruments: []string{"XAUUSD.m"},
		params: `{"htf_period":50,"ltf_fast":10,"ltf_slow":20,"atr_period":14,
		          "atr_stop_multiple":2.0}`,
		validRegimes: []string{"TRENDING"},
	},
	{
		key: "pre_event_blackout", name: "Pre-Event Blackout",
		description: "An event-driven policy strategy: it never opens exposure, and exists " +
			"to demonstrate the event-risk pathway by declining to trade near releases.",
		family: domain.FamilyEventDriven, enabled: true, promoteToPaper: false,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params:       `{"blackout_minutes_before":30,"blackout_minutes_after":15}`,
		validRegimes: []string{"EVENT_RISK"},
	},
	{
		key: "ensemble_weighted_vote", name: "Ensemble Weighted Vote",
		description: "Aggregates the trend, momentum and reversion families by confidence " +
			"weight, and returns no-trade when they disagree.",
		family: domain.FamilyEnsemble, enabled: true, promoteToPaper: false,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params: `{"members":["ma_trend_crossover","macd_momentum","rsi_mean_reversion"],
		          "min_agreement":0.6,"atr_period":14,"atr_stop_multiple":2.0}`,
		validRegimes: []string{"TRENDING", "RANGING"},
	},
	{
		key: "ml_direction_filter", name: "ML Direction Filter",
		description: "Uses a trained directional-probability model to filter a trend signal. " +
			"Fails closed: without a usable model or fresh features it returns no-trade.",
		family: domain.FamilyMachineLearning, enabled: false, promoteToPaper: false,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params: `{"model_key":"xauusd_direction","min_probability":0.58,"fast_period":20,
		          "slow_period":50,"atr_period":14,"atr_stop_multiple":2.0}`,
		validRegimes: []string{"TRENDING"},
	},
	{
		// Registered so the platform can demonstrate that it REFUSES to enable
		// this family. It is backtestable and can never be switched on for
		// execution: both the API and a database constraint refuse.
		key: "grid_martingale_research", name: "Grid / Martingale (Research Only)",
		description: "HIGH RISK RESEARCH. Adds to losing positions on a grid, which " +
			"produces smooth equity curves until it does not. Permanently disabled for " +
			"execution; available for backtesting so its failure mode can be studied.",
		family: domain.FamilyHighRiskResearch, highRisk: true, enabled: false, promoteToPaper: false,
		timeframe: domain.TF1h, instruments: []string{"XAUUSD.m"},
		params:       `{"grid_step_atr":1.0,"max_levels":5,"multiplier":2.0,"atr_period":14}`,
		validRegimes: []string{"UNKNOWN"},
	},
}

// Run loads the development dataset. It is idempotent: running it twice leaves
// the same state rather than duplicating anything.
func Run(ctx context.Context, d Deps) (Result, error) {
	now := d.Clock.Now()
	result := Result{PrimaryInstrument: "XAUUSD.m (micro, 1oz)", Currency: "ZAR", StartingBalance: "500.00"}

	// ---- Instruments -------------------------------------------------------
	for _, inst := range instrumentSeeds {
		if err := d.Store.Market.UpsertInstrument(ctx, inst); err != nil {
			return result, fmt.Errorf("seed: instrument %s: %w", inst.ID, err)
		}
		result.Instruments++
	}

	// ---- Market holidays ---------------------------------------------------
	// A small, real set so the closure path is exercised. Maintained as data.
	holidays := map[string]string{
		"2026-01-01": "New Year's Day",
		"2026-04-03": "Good Friday",
		"2026-12-25": "Christmas Day",
		"2026-12-26": "Boxing Day",
	}
	for date, reason := range holidays {
		if err := d.Store.Market.UpsertHoliday(ctx, "fx_metals_24x5", date, reason); err != nil {
			return result, fmt.Errorf("seed: holiday %s: %w", date, err)
		}
	}

	// ---- FX rates ----------------------------------------------------------
	// Required, not optional: without a USD/ZAR rate the platform cannot value
	// a gold position in a rand-denominated account, and it will correctly
	// refuse to trade rather than guess.
	rates := []struct {
		base, quote money.Currency
		rate        string
	}{
		{money.USD, money.ZAR, "18.2500"},
		{money.EUR, money.ZAR, "19.8000"},
		{money.EUR, money.USD, "1.0850"},
		{money.GBP, money.USD, "1.2700"},
	}
	for _, r := range rates {
		if err := d.Store.Market.UpsertFXRate(ctx, r.base, r.quote, dec(r.rate), now, "seed"); err != nil {
			return result, fmt.Errorf("seed: fx rate %s/%s: %w", r.base, r.quote, err)
		}
	}

	// ---- Users -------------------------------------------------------------
	userIDs := map[domain.Role]uuid.UUID{}
	for _, u := range devUsers {
		existing, err := d.Store.Users.UserByEmail(ctx, u.email)
		if err == nil {
			userIDs[u.role] = existing.ID
			result.Users = append(result.Users, SeededUser{
				Role: string(u.role), Email: u.email, Password: u.password})
			continue
		}
		hash, err := auth.HashPassword(u.password)
		if err != nil {
			return result, fmt.Errorf("seed: hash password: %w", err)
		}
		created, err := d.Store.Users.CreateUser(ctx, domain.User{
			Email: u.email, DisplayName: u.name, Role: u.role, PasswordHash: hash,
		})
		if err != nil {
			return result, fmt.Errorf("seed: create user %s: %w", u.email, err)
		}
		userIDs[u.role] = created.ID
		result.Users = append(result.Users, SeededUser{
			Role: string(u.role), Email: u.email, Password: u.password})
	}

	traderID := userIDs[domain.RoleTrader]
	adminID := userIDs[domain.RoleAdmin]

	// ---- Paper account ----------------------------------------------------
	// R500, the small-account case the platform is explicitly designed to
	// survive. At this size most gold trades are simply not affordable, and
	// the correct behaviour is to decline them.
	const accountName = "Paper — Gold (R500)"
	result.AccountName = accountName

	accounts, err := d.Store.Accounts.ListAccountsForUser(ctx, traderID)
	if err != nil {
		return result, fmt.Errorf("seed: list accounts: %w", err)
	}
	var account domain.Account
	for _, a := range accounts {
		if a.Name == accountName {
			account = a
			break
		}
	}
	startingBalance := money.MustParse("500.00", money.ZAR)

	if account.ID == uuid.Nil {
		account, err = d.Store.Accounts.CreateAccount(ctx, domain.Account{
			UserID: traderID, Name: accountName, Mode: domain.ModePaper,
			Currency: money.ZAR, BrokerName: "mock",
			Enabled: true, TradingEnabled: true, Leverage: dec("100"),
		})
		if err != nil {
			return result, fmt.Errorf("seed: create account: %w", err)
		}

		// The opening deposit is a ledger entry like any other, so the balance
		// is derived from the ledger from the very first rand.
		err = d.Pool.InTx(ctx, func(tx pgx.Tx) error {
			_, terr := d.Store.Accounts.AppendTransactionTx(ctx, tx, domain.Transaction{
				AccountID: account.ID, Type: domain.TxDeposit,
				Amount: startingBalance, BalanceAfter: startingBalance,
				Mode:        domain.ModePaper,
				Description: "Simulated opening balance (development seed). No real funds exist.",
			})
			return terr
		})
		if err != nil {
			return result, fmt.Errorf("seed: opening deposit: %w", err)
		}
	}

	if err := d.Store.Accounts.InitEquityState(ctx, account.ID, money.ZAR, startingBalance, now); err != nil {
		return result, fmt.Errorf("seed: equity state: %w", err)
	}

	// The simulated venue's own account, deliberately funded in the same
	// currency so reconciliation compares like with like.
	if err := d.MockBroker.EnsureAccount(ctx, account.ID.String(), "ZAR",
		startingBalance.Decimal(), dec("100")); err != nil {
		return result, fmt.Errorf("seed: venue account: %w", err)
	}

	// ---- Risk limits -------------------------------------------------------
	limits := domain.DefaultRiskLimitsFor(account.ID, money.ZAR, startingBalance)
	if _, err := d.Store.Control.UpsertRiskLimits(ctx, limits, adminID); err != nil {
		return result, fmt.Errorf("seed: risk limits: %w", err)
	}

	// ---- Historical bars ---------------------------------------------------
	timeframes := []domain.Timeframe{domain.TF15m, domain.TF1h, domain.TF4h}
	result.Timeframes = len(timeframes)
	for _, inst := range instrumentSeeds {
		for _, tf := range timeframes {
			dur, err := tf.Duration()
			if err != nil {
				return result, err
			}
			// Enough history for a 50-period indicator plus a train/validation/
			// test split with an embargo between windows.
			barCount := 900
			from := now.Add(-time.Duration(barCount) * dur).Truncate(dur)
			bars, err := d.Provider.HistoricalBars(ctx, inst, tf, from, now)
			if err != nil {
				return result, fmt.Errorf("seed: bars %s %s: %w", inst.ID, tf, err)
			}
			if err := d.Store.Market.UpsertBars(ctx, bars); err != nil {
				return result, fmt.Errorf("seed: store bars %s %s: %w", inst.ID, tf, err)
			}
			result.Bars += len(bars)
		}

		// One live quote per instrument so the platform has a price to act on
		// immediately rather than waiting for the first ingestion tick.
		quote, err := d.Provider.Quote(ctx, inst, now)
		if err == nil {
			if err := d.Store.Market.RecordQuote(ctx, quote); err != nil {
				return result, fmt.Errorf("seed: quote %s: %w", inst.ID, err)
			}
		}
	}

	// ---- Strategies --------------------------------------------------------
	var paperStrategyIDs []uuid.UUID
	for _, s := range strategySeeds {
		strategy, err := d.Store.Research.UpsertStrategy(ctx, domain.Strategy{
			Key: s.key, Name: s.name, Family: s.family, Description: s.description,
			HighRisk: s.highRisk, Enabled: false, OwnerUserID: traderID,
		})
		if err != nil {
			return result, fmt.Errorf("seed: strategy %s: %w", s.key, err)
		}
		result.Strategies++

		lifecycle := domain.LifecycleValidated
		if s.promoteToPaper {
			lifecycle = domain.LifecyclePaper
		}
		if _, err := d.Store.Research.UpsertStrategyVersion(ctx, domain.StrategyVersion{
			StrategyID: strategy.ID, Version: 1,
			CodeHash: "seed-" + s.key, GitSHA: "",
			Parameters: json.RawMessage(compactJSON(s.params)),
			Timeframe:  s.timeframe, Instruments: s.instruments,
			Lifecycle: lifecycle, ValidRegimes: s.validRegimes,
			Notes:     "Registered by the development seed.",
			CreatedBy: traderID,
		}); err != nil {
			return result, fmt.Errorf("seed: strategy version %s: %w", s.key, err)
		}

		// High-risk research strategies are never enabled, by construction.
		if s.enabled && !s.highRisk {
			if err := d.Store.Research.SetStrategyEnabled(ctx, strategy.ID, true); err != nil {
				return result, fmt.Errorf("seed: enable strategy %s: %w", s.key, err)
			}
		}
		if s.promoteToPaper {
			paperStrategyIDs = append(paperStrategyIDs, strategy.ID)
			result.PaperStrategies++
		}
	}

	// ---- Trading authority -------------------------------------------------
	// Scoped narrowly on purpose: gold only, market and limit orders only,
	// automation enabled, and a hard notional ceiling well inside the
	// account's means.
	existingAuthority, authErr := d.Store.Control.ActiveAuthorityForAccount(ctx, account.ID)
	if authErr == nil {
		// An authority already exists. Re-seeding does not silently widen a
		// mandate — that would defeat the point of it being a control — but it
		// does reconcile the development grant to the seed's current intent,
		// and the change is written to the authority history like any other.
		wanted := []string{"XAUUSD.m", "XAUUSD", "TEST_XAU"}
		if !sameStrings(existingAuthority.AllowedInstruments, wanted) ||
			len(existingAuthority.AllowedStrategyIDs) != len(paperStrategyIDs) {
			updated := existingAuthority
			updated.AllowedInstruments = wanted
			updated.AllowedStrategyIDs = paperStrategyIDs
			if _, err := d.Store.Control.UpdateAuthority(ctx, updated, traderID,
				existingAuthority.Version); err != nil {
				return result, fmt.Errorf("seed: reconcile trading authority: %w", err)
			}
			d.Log.Info("development trading authority reconciled to seed scope",
				"account_id", account.ID.String(), "instruments", wanted)
		}
	} else {
		// Three years, not ninety days.
		//
		// A trading authority is a real control and this does not weaken it:
		// the window is still enforced on every order. Ninety days was chosen
		// when nothing ran unattended, and it makes the DEVELOPMENT fixture
		// unusable for the thing this repository now needs it for -- a market
		// replay is dated ahead of the seed on purpose (see the fixture
		// generator), so a ninety-day authority expires before the dataset
		// starts and every strategy run is skipped with "Trading authority
		// has expired". That refusal is correct and the reason for it is
		// invisible.
		//
		// A real deployment sets its own window through the API; this is the
		// seed's default only.
		validUntil := now.Add(3 * 365 * 24 * time.Hour)
		if _, err := d.Store.Control.CreateAuthority(ctx, domain.TradingAuthority{
			UserID: traderID, AccountID: account.ID, Mode: domain.ModePaper,
			Active: true, AutomationEnabled: true,
			AllowedInstruments: []string{"XAUUSD.m", "XAUUSD", "TEST_XAU"},
			AllowedStrategyIDs: paperStrategyIDs,
			AllowedOrderTypes: []domain.OrderType{
				domain.OrderTypeMarket, domain.OrderTypeLimit},
			MaxOrderQuantity:    dec("0.10"),
			MaxOrderNotional:    money.MustParse("2500.00", money.ZAR),
			MaxPositionExposure: money.MustParse("2500.00", money.ZAR),
			MaxLeverage:         dec("10"),
			MaxDailyLoss:        money.MustParse("15.00", money.ZAR),
			ValidFrom:           now, ValidUntil: &validUntil,
		}, traderID); err != nil {
			return result, fmt.Errorf("seed: trading authority: %w", err)
		}
	}

	// ---- Economic calendar and news ----------------------------------------
	// Both come through their provider interfaces rather than being written
	// here, so the seed exercises the same ingestion path the running service
	// uses. A separate copy of the fixtures in this file would be a second
	// source of truth that could disagree with the provider.
	feeds, err := d.EconData.Refresh(ctx, now)
	if err != nil {
		return result, err
	}
	result.Events = feeds.Events
	result.News = feeds.Headlines

	// Which currencies matter to which instruments. A USD release moves gold;
	// a rand release moves USDZAR. Not every event matters to every market.
	eventMap := []struct {
		currency, instrument, relevance string
	}{
		{"USD", "XAUUSD", "1.0"},
		{"USD", "XAUUSD.m", "1.0"},
		{"USD", "EURUSD", "1.0"},
		{"USD", "XAGUSD", "0.9"},
		{"USD", "USDZAR", "1.0"},
		{"EUR", "EURUSD", "1.0"},
		{"ZAR", "USDZAR", "0.9"},
	}
	for _, m := range eventMap {
		if err := d.Store.Research.MapEventCurrencyToInstrument(ctx,
			m.currency, m.instrument, dec(m.relevance)); err != nil {
			return result, fmt.Errorf("seed: event map: %w", err)
		}
	}

	d.Log.Info("development data seeded",
		"instruments", result.Instruments, "bars", result.Bars,
		"strategies", result.Strategies, "paper_strategies", result.PaperStrategies,
		"events", result.Events, "news", result.News,
		"account", account.ID.String())

	return result, nil
}

// sameStrings reports whether two slices hold the same values in order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// compactJSON strips the whitespace the parameter literals carry for
// readability, so the stored value is canonical.
func compactJSON(s string) []byte {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return []byte(`{}`)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return out
}
