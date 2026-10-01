package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AuthorityExpiring warns that a trading authority is about to lapse, or has.
//
// Nothing warned before. The development seed grants three years, so the
// condition is far away and completely invisible until the day it arrives --
// and on that day the platform stops trading in a way that looks exactly like
// a strategy finding nothing: the authority row is still returned by
// `ActiveAuthorityForAccount`, which does not filter on `valid_until`,
// `Effective` refuses, and the scheduler's loop treats it as an ordinary skip
// and continues without a log line.
//
// `remaining` is negative once it has already expired, which is the more
// serious case and is reported as critical.
func (a *Alerter) AuthorityExpiring(ctx context.Context, accountID uuid.UUID,
	validUntil time.Time, remaining time.Duration) {

	id := accountID
	severity := SeverityWarning
	title := fmt.Sprintf("Trading authority expires in %s", humaniseDuration(remaining))
	body := fmt.Sprintf(
		"The trading authority for this account is valid until %s. When it lapses the "+
			"platform stops placing orders: the authority is still returned by the store "+
			"but is no longer effective, and every strategy evaluation becomes a skip. "+
			"Renew it before then, or expect trading to stop silently.",
		validUntil.UTC().Format(time.RFC3339))

	if remaining <= 0 {
		severity = SeverityCritical
		title = "Trading authority has expired"
		body = fmt.Sprintf(
			"The trading authority for this account expired at %s. No order can be placed "+
				"autonomous or manual, until it is renewed. This is why nothing is "+
				"trading.",
			validUntil.UTC().Format(time.RFC3339))
	}

	a.Raise(ctx, Event{
		Kind:     KindAuthorityExpiring,
		Severity: severity,
		Category: CategoryRisk,
		// Keyed on the account alone, so the cooldown stops this repeating
		// hourly for the whole thirty days. A change of severity still gets
		// through, because crossing into "expired" is new information.
		Key:       accountID.String(),
		Title:     title,
		Body:      body,
		AccountID: &id,
		Fields: map[string]any{
			"account_id":  accountID.String(),
			"valid_until": validUntil.UTC().Format(time.RFC3339),
			"expired":     remaining <= 0,
		},
	})
}

// humaniseDuration renders a coarse, readable remaining time.
//
// Deliberately coarse: "29 days" is what an operator acts on, and a precise
// "28 days 17 hours 3 minutes" in an alert title reads as noise.
func humaniseDuration(d time.Duration) string {
	if d <= 0 {
		return "0 days"
	}
	days := int(d.Hours() / 24)
	if days >= 2 {
		return fmt.Sprintf("%d days", days)
	}
	hours := int(d.Hours())
	if hours >= 2 {
		return fmt.Sprintf("%d hours", hours)
	}
	return "under an hour"
}
