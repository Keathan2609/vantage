package replay

import (
	"sync"
	"time"

	"github.com/vantage/control-api/internal/domain"
)

// Clock is the application clock during a replay.
//
// # Why the clock has to move, not just the data
//
// Feeding old bars into the platform while `time.Now()` still returns today
// does not test anything useful. Every quote would be hours stale, the
// data-quality policy would refuse it, the market clock would report a
// different session from the one the data belongs to, and the daily P&L
// boundary would fall in the wrong place. The run would refuse everything and
// the refusals would all be artefacts of the harness.
//
// So replay time IS application time. `domain.Clock` is a one-method interface
// precisely so this substitution is possible, and because nothing in the
// trading path calls `time.Now()` directly, replacing it moves:
//
//   - quote ages and therefore data-quality verdicts
//   - bar buckets
//   - market and session state, including the daily maintenance break
//   - strategy scheduling and the per-bar guard
//   - economic-calendar and news windows
//   - daily loss and drawdown boundaries
//   - audit and order timestamps
//
// A run started on a Sunday in December therefore produces the same decisions
// as one started on a Tuesday in March, which is the whole point.
//
// # Why it is monotonic within a step
//
// Several components read the clock more than once while handling one step,
// and some of them compare the results. A clock that jumped mid-step would
// produce a negative duration somewhere unhelpful. `Advance` moves it; nothing
// else does.
type Clock struct {
	mu sync.RWMutex
	// at is the current replay instant, always UTC.
	at time.Time
	// offset counts intra-step nudges, so two events inside one step do not
	// share a timestamp. Audit rows and ledger sequences are ordered by time
	// in places, and identical timestamps make that order arbitrary.
	offset time.Duration
	// started records whether Advance has ever been called, so Now() before
	// the first step returns the dataset's own start rather than the zero
	// time -- which would make every quote look impossibly old.
	started bool
}

// NewClock builds a replay clock positioned at the dataset's first instant.
func NewClock(start time.Time) *Clock {
	return &Clock{at: start.UTC()}
}

// Now returns the current replay instant.
func (c *Clock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.at.Add(c.offset).UTC()
}

// Advance moves the clock to an instant.
//
// Moving BACKWARDS is refused silently rather than applied. A dataset is
// validated to advance, so a backwards move means two components disagree
// about where the run is -- and letting the clock go back would make a quote
// arrive before the one it follows, which the data-quality policy correctly
// treats as a provider fault. Better to hold than to manufacture one.
func (c *Clock) Advance(to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	to = to.UTC()
	if c.started && to.Before(c.at) {
		return
	}
	c.at = to
	c.offset = 0
	c.started = true
}

// Tick nudges the clock forward inside the current step.
//
// Used between the phases of one step -- ingest, then resting orders, then
// strategies -- so their timestamps are ordered. Deliberately tiny: it must
// not move the run into the next bar.
func (c *Clock) Tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += time.Millisecond
}

// At reports the step instant without the intra-step offset.
func (c *Clock) At() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.at.UTC()
}

var _ domain.Clock = (*Clock)(nil)

// SwitchableClock lets one process serve ordinary requests on the system clock
// and a replay on replay time.
//
// # Why this exists rather than two processes
//
// The acceptance requirement is that replay drives the SAME application, which
// means the same composition, the same handlers, the same OMS. Running a
// second process with a different clock would be a second deployment, and a
// second deployment is a second thing to keep in step.
//
// The risk this creates is worth stating plainly: while a replay is active,
// every clock reader in the process sees replay time, including ones that have
// nothing to do with trading -- session expiry, rate-limit windows, audit
// timestamps. That is acceptable ONLY because replay mode is explicit,
// refused outside development and test, and never the default. It is recorded
// in docs/MARKET_REPLAY.md as the central trade-off of this design.
type SwitchableClock struct {
	mu      sync.RWMutex
	replay  *Clock
	fallers domain.Clock
}

// NewSwitchableClock wraps a base clock.
func NewSwitchableClock(base domain.Clock) *SwitchableClock {
	if base == nil {
		base = domain.SystemClock{}
	}
	return &SwitchableClock{fallers: base}
}

// Now returns replay time when a replay is engaged, otherwise real time.
func (s *SwitchableClock) Now() time.Time {
	s.mu.RLock()
	r := s.replay
	base := s.fallers
	s.mu.RUnlock()
	if r != nil {
		return r.Now()
	}
	return base.Now()
}

// Engage switches the process onto replay time.
func (s *SwitchableClock) Engage(c *Clock) {
	s.mu.Lock()
	s.replay = c
	s.mu.Unlock()
}

// Disengage returns the process to real time.
func (s *SwitchableClock) Disengage() {
	s.mu.Lock()
	s.replay = nil
	s.mu.Unlock()
}

// Engaged reports whether replay time is in force.
func (s *SwitchableClock) Engaged() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.replay != nil
}

var _ domain.Clock = (*SwitchableClock)(nil)
