// Package fx converts between currencies explicitly.
//
// Vantage never mixes currencies implicitly. An account denominated in ZAR
// holding a position in an instrument quoted in USD has two different kinds of
// money in play, and every crossing between them passes through this package,
// which records the rate it used and refuses to guess when it has none.
//
// The alternative — treating a number as "just a number" once it leaves the
// database — is how a 500 ZAR account ends up believing it can support a
// position sized in dollars.
package fx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/money"
)

// ErrNoRate means no usable conversion exists. It is deliberately fatal to the
// operation that needed it: an order whose margin cannot be expressed in the
// account's currency is an order whose risk cannot be checked.
var ErrNoRate = errors.New("fx: no conversion rate available")

// ErrRateStale means a rate exists but is too old to rely on.
var ErrRateStale = errors.New("fx: conversion rate is stale")

// RateSource supplies rates. It is an interface so a future licensed provider
// drops in without touching conversion logic.
type RateSource interface {
	LatestFXRate(ctx context.Context, base, quote money.Currency) (Rate, error)
}

// Rate is a conversion factor with provenance.
type Rate struct {
	Base       money.Currency
	Quote      money.Currency
	Rate       decimal.Decimal
	SourceTime time.Time
	IngestedAt time.Time
	Provider   string
}

// Inverse returns the reciprocal rate.
func (r Rate) Inverse() (Rate, error) {
	if r.Rate.IsZero() {
		return Rate{}, fmt.Errorf("%w: cannot invert a zero rate", ErrNoRate)
	}
	return Rate{
		Base:       r.Quote,
		Quote:      r.Base,
		Rate:       decimal.NewFromInt(1).Div(r.Rate),
		SourceTime: r.SourceTime,
		IngestedAt: r.IngestedAt,
		Provider:   r.Provider + " (inverted)",
	}, nil
}

// Converter performs currency conversion.
type Converter struct {
	source RateSource
	// MaxAge bounds how old a rate may be. Position valuation on a rate from
	// last week is not valuation, it is fiction.
	MaxAge time.Duration
	now    func() time.Time
}

// NewConverter builds a converter.
func NewConverter(source RateSource, maxAge time.Duration, now func() time.Time) *Converter {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	return &Converter{source: source, MaxAge: maxAge, now: now}
}

// Conversion records a completed conversion and how it was derived, so a
// stored valuation can be explained later.
type Conversion struct {
	From     money.Amount
	To       money.Amount
	Rate     decimal.Decimal
	Path     string // "direct", "inverse", or "via USD"
	RateTime time.Time
	Provider string
}

// Convert converts an amount into the target currency.
//
// Three resolution steps are attempted, in order, and each is recorded in the
// result so the path taken is never ambiguous:
//
//  1. a direct rate;
//  2. the inverse of the opposite pair;
//  3. a cross through USD, which is the pivot for every pair Vantage supports.
//
// If none applies, the conversion fails. It does not fall back to 1.0.
func (c *Converter) Convert(ctx context.Context, amount money.Amount, target money.Currency) (Conversion, error) {
	if amount.Currency() == target {
		return Conversion{
			From: amount, To: amount, Rate: decimal.NewFromInt(1),
			Path: "identity", RateTime: c.now(), Provider: "n/a",
		}, nil
	}
	if amount.Currency() == "" || target == "" {
		return Conversion{}, fmt.Errorf("%w: conversion requires two currencies (got %q -> %q)",
			ErrNoRate, amount.Currency(), target)
	}

	if r, err := c.usableRate(ctx, amount.Currency(), target); err == nil {
		return c.apply(amount, target, r, "direct"), nil
	} else if errors.Is(err, ErrRateStale) {
		return Conversion{}, err
	}

	if r, err := c.usableRate(ctx, target, amount.Currency()); err == nil {
		inv, ierr := r.Inverse()
		if ierr != nil {
			return Conversion{}, ierr
		}
		return c.apply(amount, target, inv, "inverse"), nil
	}

	// Cross through USD.
	const pivot = money.USD
	if amount.Currency() != pivot && target != pivot {
		toPivot, err1 := c.anyRate(ctx, amount.Currency(), pivot)
		fromPivot, err2 := c.anyRate(ctx, pivot, target)
		if err1 == nil && err2 == nil {
			crossed := Rate{
				Base:       amount.Currency(),
				Quote:      target,
				Rate:       toPivot.Rate.Mul(fromPivot.Rate),
				SourceTime: earliest(toPivot.SourceTime, fromPivot.SourceTime),
				Provider:   toPivot.Provider + "+" + fromPivot.Provider,
			}
			return c.apply(amount, target, crossed, "via "+string(pivot)), nil
		}
	}

	return Conversion{}, fmt.Errorf("%w: %s -> %s", ErrNoRate, amount.Currency(), target)
}

// MustConvertOrZero converts and returns a zero amount on failure. It exists
// only for DISPLAY aggregation, where one unconvertible leg should not blank
// the whole page. It must never be used on a path that decides whether an
// order may proceed.
func (c *Converter) MustConvertOrZero(ctx context.Context, amount money.Amount, target money.Currency) money.Amount {
	conv, err := c.Convert(ctx, amount, target)
	if err != nil {
		return money.Zero(target)
	}
	return conv.To
}

func (c *Converter) apply(amount money.Amount, target money.Currency, r Rate, path string) Conversion {
	converted := money.New(amount.Decimal().Mul(r.Rate), target)
	return Conversion{
		From:     amount,
		To:       converted,
		Rate:     r.Rate,
		Path:     path,
		RateTime: r.SourceTime,
		Provider: r.Provider,
	}
}

// usableRate returns a rate only if it is fresh enough to act on.
func (c *Converter) usableRate(ctx context.Context, base, quote money.Currency) (Rate, error) {
	r, err := c.source.LatestFXRate(ctx, base, quote)
	if err != nil {
		return Rate{}, fmt.Errorf("%w: %s/%s", ErrNoRate, base, quote)
	}
	if age := c.now().Sub(r.SourceTime); age > c.MaxAge {
		return Rate{}, fmt.Errorf("%w: %s/%s is %s old", ErrRateStale, base, quote, age.Truncate(time.Second))
	}
	return r, nil
}

// anyRate ignores staleness, used only while assembling a cross where the
// freshness of each leg is checked by the caller's own use of usableRate first.
func (c *Converter) anyRate(ctx context.Context, base, quote money.Currency) (Rate, error) {
	r, err := c.usableRate(ctx, base, quote)
	if err == nil {
		return r, nil
	}
	inv, ierr := c.source.LatestFXRate(ctx, quote, base)
	if ierr != nil {
		return Rate{}, err
	}
	if age := c.now().Sub(inv.SourceTime); age > c.MaxAge {
		return Rate{}, fmt.Errorf("%w: %s/%s is stale", ErrRateStale, quote, base)
	}
	return inv.Inverse()
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
