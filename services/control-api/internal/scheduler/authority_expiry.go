package scheduler

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/vantage/control-api/internal/domain"
)

// Alerter is the slice of the notification service this package needs.
//
// Narrow, and declared here rather than imported, because internal/notify is a
// leaf by architectural rule: TestNoPackageDependsOnNotify fails the build if a
// package imports it. internal/app wires the concrete alerter in.
type Alerter interface {
	AuthorityExpiring(ctx context.Context, accountID uuid.UUID,
		validUntil time.Time, remaining time.Duration)
}

// authorityExpiryHorizons are the points at which an operator is warned.
//
// Three, widening: enough notice to arrange a renewal, a reminder, and a last
// call. One warning at a single horizon is missed by anyone not looking that
// day, and an alert nobody needs to act on is cheap.
var authorityExpiryHorizons = []time.Duration{
	30 * 24 * time.Hour,
	7 * 24 * time.Hour,
	24 * time.Hour,
}

// orderedHorizons returns the warning points narrowest first.
//
// Sorted rather than trusting the declaration order, so a horizon added out of
// order cannot make the warning say "30 days" on the day it expires.
func orderedHorizons() []time.Duration {
	out := append([]time.Duration(nil), authorityExpiryHorizons...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// warnOnExpiringAuthority tells the operator BEFORE trading stops.
//
// Expiry is otherwise entirely silent. `ActiveAuthorityForAccount` does not
// filter on `valid_until`, so the row keeps being returned; `Effective`
// refuses; and the strategy loop treats that refusal as an ordinary skip and
// continues without a log line. The platform simply stops placing orders one
// day, and the symptom -- every strategy skipping -- is indistinguishable from
// a quiet market.
//
// Runs in the hourly cleanup, so a horizon is crossed many times inside the day
// it names. The alerter's own cooldown, keyed on the account, is what stops
// that becoming a notification an hour.
func (s *Scheduler) warnOnExpiringAuthority(ctx context.Context, account domain.Account) {
	if s.deps.Alerter == nil {
		return
	}
	authority, err := s.deps.Store.Control.ActiveAuthorityForAccount(ctx, account.ID)
	if err != nil {
		// No authority at all is a different condition with its own refusal
		// path. It is not an expiry and is not this function's business.
		return
	}
	if authority.ValidUntil == nil {
		return // open-ended, so it cannot lapse
	}

	remaining := authority.ValidUntil.Sub(s.deps.Clock.Now())
	if remaining <= 0 {
		// Already expired. Reported on every pass on purpose: the platform is
		// not trading and this is the reason, so the alerter's cooldown is the
		// right place for the suppression rather than a decision taken here.
		s.deps.Alerter.AuthorityExpiring(ctx, account.ID, *authority.ValidUntil, remaining)
		return
	}
	for _, horizon := range orderedHorizons() {
		if remaining <= horizon {
			s.deps.Alerter.AuthorityExpiring(ctx, account.ID, *authority.ValidUntil, remaining)
			return // the narrowest matching horizon is the one to report
		}
	}
}
