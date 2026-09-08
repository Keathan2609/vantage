package fx

import (
	"context"

	"github.com/vantage/control-api/internal/money"
	"github.com/vantage/control-api/internal/store"
)

// StoreRateSource adapts the market store to the RateSource interface.
//
// It exists so the converter depends on a narrow interface rather than on the
// whole persistence layer: a future licensed FX provider implements RateSource
// directly and nothing in conversion logic changes.
type StoreRateSource struct {
	Market *store.MarketStore
}

// LatestFXRate returns the most recent stored rate for a pair.
func (s StoreRateSource) LatestFXRate(ctx context.Context, base, quote money.Currency) (Rate, error) {
	r, err := s.Market.LatestFXRate(ctx, base, quote)
	if err != nil {
		return Rate{}, err
	}
	return Rate{
		Base:       r.Base,
		Quote:      r.Quote,
		Rate:       r.Rate,
		SourceTime: r.SourceTime,
		IngestedAt: r.IngestedAt,
		Provider:   r.Provider,
	}, nil
}

var _ RateSource = StoreRateSource{}
