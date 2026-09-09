package scheduler

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vantage/control-api/internal/logging"
)

// The scheduler is what makes Vantage autonomous, so its failure modes are
// availability failures rather than accounting ones: a job that panics must not
// take the others down, a job that hangs must not hold its slot forever, and a
// slow job must not be entered twice at once.
//
// The jobs themselves need a database and are exercised by the smoke and race
// suites. What is tested here is the loop that drives them, which needs
// nothing.

func testScheduler() *Scheduler {
	return &Scheduler{
		deps:     Deps{Log: logging.New(logging.Options{Level: "error", Service: "test", Env: "test", Writer: io.Discard})},
		holderID: "test-holder",
	}
}

func TestTheLoopRunsTheJobRepeatedly(t *testing.T) {
	s := testScheduler()
	var runs int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, "counter", 10*time.Millisecond, func(context.Context) error {
		atomic.AddInt64(&runs, 1)
		return nil
	})

	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&runs) >= 3 },
		"the job ran fewer than three times")
}

func TestTheJobDoesNotRunBeforeTheFirstInterval(t *testing.T) {
	// A ticker fires after the interval, not immediately. That is deliberate:
	// the market-data ingestor primes the feed first, and a strategy job that
	// fired at t=0 would evaluate against whatever was in the database from
	// the previous process.
	s := testScheduler()
	var runs int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, "late", 400*time.Millisecond, func(context.Context) error {
		atomic.AddInt64(&runs, 1)
		return nil
	})

	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt64(&runs); got != 0 {
		t.Errorf("the job ran %d time(s) before its first interval elapsed", got)
	}
}

func TestAPanickingJobDoesNotStopTheLoop(t *testing.T) {
	// One bad job must not end autonomous operation. Before the recover, a nil
	// map write anywhere in a strategy run would have silently killed the
	// scheduler goroutine and left the platform looking healthy while doing
	// nothing.
	s := testScheduler()
	var runs int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, "panicky", 10*time.Millisecond, func(context.Context) error {
		atomic.AddInt64(&runs, 1)
		panic("deliberate panic in a scheduled job")
	})

	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&runs) >= 3 },
		"the loop stopped after a job panicked")
}

func TestAFailingJobDoesNotStopTheLoop(t *testing.T) {
	s := testScheduler()
	var runs int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, "failing", 10*time.Millisecond, func(context.Context) error {
		atomic.AddInt64(&runs, 1)
		return errors.New("the database is unreachable")
	})

	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&runs) >= 3 },
		"the loop stopped after a job returned an error")
}

func TestCancellingTheContextStopsTheLoop(t *testing.T) {
	s := testScheduler()
	var runs int64
	done := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		s.loop(ctx, "stoppable", 10*time.Millisecond, func(context.Context) error {
			atomic.AddInt64(&runs, 1)
			return nil
		})
		close(done)
	}()

	waitFor(t, time.Second, func() bool { return atomic.LoadInt64(&runs) >= 1 },
		"the job never ran")
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop did not return after its context was cancelled")
	}

	after := atomic.LoadInt64(&runs)
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt64(&runs); got != after {
		t.Errorf("the job ran again after cancellation (%d -> %d)", after, got)
	}
}

func TestTheJobReceivesACancelledContextWhenTheSchedulerStops(t *testing.T) {
	// A running strategy evaluation has to notice the shutdown, or the process
	// waits on an HTTP call to the research service while trying to exit.
	s := testScheduler()
	observed := make(chan error, 1)

	// A long interval on purpose, so the job's own three-interval deadline is
	// far away and cancellation is unambiguously what ends it. With a short
	// interval the deadline fires first and the test would pass for the wrong
	// reason.
	ctx, cancel := context.WithCancel(context.Background())
	go s.loop(ctx, "observer", 2*time.Second, func(jobCtx context.Context) error {
		select {
		case <-jobCtx.Done():
			observed <- jobCtx.Err()
		case <-time.After(30 * time.Second):
			observed <- nil
		}
		return nil
	})

	// Wait for the job to actually be running before cancelling; cancelling
	// first would test the select in the loop rather than the job's context.
	time.Sleep(2100 * time.Millisecond)
	cancel()

	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the job's context ended with %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the job never observed its context ending")
	}
}

func TestALongJobIsNotEnteredTwiceAtOnce(t *testing.T) {
	// This is the "duplicate scheduler tick" case. The loop calls the job
	// synchronously on its own goroutine, so ticks that arrive while a job is
	// running are coalesced by the ticker rather than starting a second copy.
	//
	// It matters because two concurrent strategy runs on the same bar would
	// each ask the research service for an opinion and each try to place an
	// order. The idempotency key would catch the duplicate order, but relying
	// on that means relying on a guard downstream of the one that should have
	// held.
	s := testScheduler()
	var concurrent, maxConcurrent int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.loop(ctx, "slow", 5*time.Millisecond, func(context.Context) error {
		n := atomic.AddInt64(&concurrent, 1)
		for {
			seen := atomic.LoadInt64(&maxConcurrent)
			if n <= seen || atomic.CompareAndSwapInt64(&maxConcurrent, seen, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt64(&concurrent, -1)
		return nil
	})

	time.Sleep(400 * time.Millisecond)
	if got := atomic.LoadInt64(&maxConcurrent); got > 1 {
		t.Errorf("up to %d copies of the job ran at once; the loop must run one at a time", got)
	}
}

func TestAHangingJobIsBoundedByATimeout(t *testing.T) {
	// A job whose context never ends would hold its slot for the life of the
	// process. Reconciliation once did exactly that: it held a lease and an
	// advisory lock indefinitely and every later run silently declined.
	s := testScheduler()
	deadlines := make(chan time.Duration, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const interval = 30 * time.Millisecond
	go s.loop(ctx, "hanging", interval, func(jobCtx context.Context) error {
		deadline, ok := jobCtx.Deadline()
		if !ok {
			deadlines <- 0
			return nil
		}
		deadlines <- time.Until(deadline)
		<-jobCtx.Done()
		return jobCtx.Err()
	})

	select {
	case budget := <-deadlines:
		if budget <= 0 {
			t.Fatal("the job was given no deadline at all, so a hang would be permanent")
		}
		// The loop allows three intervals. Asserting the relationship rather
		// than the constant, so the intent survives a retuned interval.
		if budget > 10*interval {
			t.Errorf("the job's budget was %s for a %s interval, which is not a bound "+
				"that would catch a hang", budget, interval)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the job never ran")
	}
}

func TestTheLoopRecoversFromRepeatedPanicsWithoutLeaking(t *testing.T) {
	// A job that panics every time must not accumulate goroutines: the
	// recover is inside a closure called synchronously, so each panic unwinds
	// into the same loop goroutine.
	s := testScheduler()
	var runs int64

	ctx, cancel := context.WithCancel(context.Background())
	go s.loop(ctx, "always-panics", 2*time.Millisecond, func(context.Context) error {
		atomic.AddInt64(&runs, 1)
		panic("again")
	})
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt64(&runs) >= 20 },
		"the loop did not survive repeated panics")
	cancel()
}

func TestEachSchedulerInstanceHasItsOwnHolderIdentity(t *testing.T) {
	// The lease is held by holderID. Two instances sharing one would each
	// believe it held the lease, which is the exact double-run the lease
	// exists to prevent.
	seen := map[string]bool{}
	for k := 0; k < 5; k++ {
		id := New(Deps{Log: logging.New(logging.Options{Level: "error", Service: "test", Env: "test", Writer: io.Discard})}).holderID
		if id == "" {
			t.Fatal("holderID is empty")
		}
		if seen[id] {
			t.Fatalf("two schedulers were built with the same holder id %q", id)
		}
		seen[id] = true
		// The identity is time-derived, so consecutive construction is the
		// case worth checking.
		time.Sleep(time.Millisecond)
	}
}

func TestTheConfiguredIntervalsAreOrderedSensibly(t *testing.T) {
	// Not a tautology: these constants encode a policy, and getting the order
	// wrong has consequences. Market data must be the fastest, or resting
	// orders trigger on prices that have already moved on; the outbox must be
	// faster than strategy evaluation, or an alert about a trade arrives after
	// the next one is placed; and the lease must outlive a strategy run, or a
	// second instance takes the lease mid-evaluation.
	if marketDataInterval >= strategyInterval {
		t.Errorf("market data (%s) is not faster than strategy evaluation (%s)",
			marketDataInterval, strategyInterval)
	}
	if outboxInterval >= strategyInterval {
		t.Errorf("the outbox (%s) is not faster than strategy evaluation (%s)",
			outboxInterval, strategyInterval)
	}
	if leaseTTL <= strategyInterval {
		t.Errorf("the lease TTL (%s) does not outlive a strategy interval (%s)",
			leaseTTL, strategyInterval)
	}
	if reconciliationTimeout >= reconciliationInterval {
		t.Errorf("the reconciliation timeout (%s) is not inside its interval (%s), so a "+
			"slow run would still be running when the next one is due",
			reconciliationTimeout, reconciliationInterval)
	}
	if econDataInterval <= strategyInterval {
		t.Errorf("the calendar refresh (%s) is not slower than strategy evaluation (%s); "+
			"a schedule of macroeconomic releases does not change by the second",
			econDataInterval, strategyInterval)
	}
}

// waitFor polls until cond holds, failing with msg on timeout. Polling rather
// than sleeping a fixed duration, so a slow machine does not turn a passing
// test into a flake.
func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
