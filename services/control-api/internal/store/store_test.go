package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vantage/control-api/internal/domain"
)

// mapError decides, for every database failure in the platform, whether the
// caller sees "not found", "conflict", "retry" or an opaque 500. It is a pure
// function, and these cases were previously exercised only by accident through
// end-to-end tests -- which means the ones that never happened in a test run
// were never checked at all.

func pgErr(code, constraint, table string) error {
	return &pgconn.PgError{Code: code, ConstraintName: constraint, TableName: table}
}

func TestNoRowsBecomesNotFound(t *testing.T) {
	if got := mapError(pgx.ErrNoRows); !errors.Is(got, ErrNotFound) {
		t.Fatalf("mapError(ErrNoRows) = %v, want ErrNotFound", got)
	}
}

func TestNilStaysNil(t *testing.T) {
	if got := mapError(nil); got != nil {
		t.Fatalf("mapError(nil) = %v, want nil", got)
	}
}

func TestAWrappedNoRowsIsStillNotFound(t *testing.T) {
	// Repository methods wrap with context before returning, so unwrapping has
	// to survive it or every "does not exist" turns into a 500.
	wrapped := fmt.Errorf("loading order: %w", pgx.ErrNoRows)
	if got := mapError(wrapped); !errors.Is(got, ErrNotFound) {
		t.Fatalf("mapError(wrapped ErrNoRows) = %v, want ErrNotFound", got)
	}
}

func TestAUniqueViolationIsBothAConflictAndAConstraint(t *testing.T) {
	// Callers ask two different questions of the same error: the HTTP layer
	// wants to know whether this is a conflict, and the OMS wants to know
	// which constraint fired. ConstraintError.Is answers both, so neither
	// caller needs to learn the other's vocabulary.
	got := mapError(pgErr("23505", "fills_broker_fill_uniq", "fills"))

	if !errors.Is(got, ErrConflict) {
		t.Errorf("unique violation is not ErrConflict: %v", got)
	}
	if !errors.Is(got, ErrConstraint) {
		t.Errorf("unique violation is not ErrConstraint: %v", got)
	}
	if !IsConstraint(got, "fills_broker_fill_uniq") {
		t.Errorf("IsConstraint could not name the constraint: %v", got)
	}
	if IsConstraint(got, "some_other_uniq") {
		t.Error("IsConstraint matched a constraint that did not fire, which would " +
			"let a caller treat an unrelated violation as its own expected one")
	}
}

func TestCheckForeignKeyAndNotNullAreAllConstraintViolations(t *testing.T) {
	// All three mean the application tried to persist a state the schema
	// forbids -- a bug on our side, not a user error -- so they share a
	// classification.
	for _, code := range []string{"23514", "23503", "23502"} {
		got := mapError(pgErr(code, "orders_status_ck", "orders"))
		if !errors.Is(got, ErrConstraint) {
			t.Errorf("SQLSTATE %s = %v, want ErrConstraint", code, got)
		}
		if !IsConstraint(got, "orders_status_ck") {
			t.Errorf("SQLSTATE %s lost the constraint name", code)
		}
	}
}

func TestAnAppendOnlyTriggerIsReportedAsSuch(t *testing.T) {
	// The append-only triggers on audit_events, transactions and fills raise
	// restrict_violation with no constraint name of their own. Without the
	// synthetic name the failure reads as an unclassified 500, and the whole
	// point of those triggers is that a violation should be legible.
	got := mapError(&pgconn.PgError{
		Code:      "23001",
		TableName: "audit_events",
		Message:   "audit_events is append-only",
	})
	if !IsConstraint(got, "append_only") {
		t.Fatalf("restrict_violation was not labelled append_only: %v", got)
	}
	if !errors.Is(got, ErrConstraint) {
		t.Errorf("append-only violation is not ErrConstraint: %v", got)
	}
}

func TestASerialisationFailureAsksTheCallerToRetry(t *testing.T) {
	got := mapError(pgErr("40001", "", ""))
	if !errors.Is(got, ErrStaleVersion) {
		t.Fatalf("SQLSTATE 40001 = %v, want ErrStaleVersion", got)
	}
}

func TestADeadlockIsClassifiedRatherThanLeakingAsAFiveHundred(t *testing.T) {
	// This was a real defect: an unclassified 40P01 surfaced as a bare HTTP
	// 500 on order placement. It is classified now, and must stay classified,
	// because whether a deadlock is safe to retry depends on the phase it
	// happened in -- and a caller cannot make that decision about an error it
	// cannot recognise.
	got := mapError(&pgconn.PgError{Code: "40P01", Message: "deadlock detected"})
	if !errors.Is(got, ErrDeadlock) {
		t.Fatalf("SQLSTATE 40P01 = %v, want ErrDeadlock", got)
	}
	// It is emphatically NOT a conflict or a stale version: those two are
	// safely retryable in place, and a deadlock after a broker call is not.
	if errors.Is(got, ErrConflict) || errors.Is(got, ErrStaleVersion) {
		t.Error("a deadlock was classified as retryable, which would allow a " +
			"retry after a venue call whose answer cannot be reproduced")
	}
}

func TestAnUnrecognisedDatabaseErrorIsPassedThroughUnchanged(t *testing.T) {
	// Fail loud rather than mapping the unknown onto a friendly category: an
	// unrecognised SQLSTATE reported as ErrNotFound would hide a real fault
	// behind a 404.
	original := pgErr("42P01", "", "orders")
	got := mapError(original)
	if !errors.Is(got, original) {
		t.Fatalf("mapError swallowed an unknown SQLSTATE: %v", got)
	}
	for name, sentinel := range map[string]error{
		"ErrNotFound":     ErrNotFound,
		"ErrConflict":     ErrConflict,
		"ErrConstraint":   ErrConstraint,
		"ErrStaleVersion": ErrStaleVersion,
		"ErrDeadlock":     ErrDeadlock,
	} {
		if errors.Is(got, sentinel) {
			t.Errorf("unknown SQLSTATE 42P01 was classified as %s", name)
		}
	}
}

func TestIsConstraintIgnoresErrorsThatAreNotConstraintViolations(t *testing.T) {
	if IsConstraint(errors.New("connection refused"), "anything") {
		t.Error("IsConstraint matched a non-constraint error")
	}
	if IsConstraint(nil, "anything") {
		t.Error("IsConstraint matched nil")
	}
}

// Fingerprint decides whether two detections are the same incident. Getting it
// wrong in either direction is expensive: too coarse and separate problems
// collapse into one issue, too fine and every reconciliation run re-raises the
// same problem and halts the account forever.

func TestTheSameProblemFingerprintsIdenticallyAcrossRuns(t *testing.T) {
	a := Fingerprint(domain.IssueFillMissingLocally, "EXEC-1")
	b := Fingerprint(domain.IssueFillMissingLocally, "EXEC-1")
	if a != b {
		t.Fatalf("the same problem produced two fingerprints: %s and %s", a, b)
	}
}

func TestDifferentProblemsDoNotShareAFingerprint(t *testing.T) {
	seen := map[string]string{}
	for _, c := range []struct {
		label string
		fp    string
	}{
		{"missing fill EXEC-1", Fingerprint(domain.IssueFillMissingLocally, "EXEC-1")},
		{"missing fill EXEC-2", Fingerprint(domain.IssueFillMissingLocally, "EXEC-2")},
		{"extra fill EXEC-1", Fingerprint(domain.IssueExtraBrokerFill, "EXEC-1")},
		{"position mismatch", Fingerprint(domain.IssuePositionMismatch, "XAUUSD")},
		{"balance mismatch", Fingerprint(domain.IssueBalanceMismatch, "ZAR")},
	} {
		if prev, dup := seen[c.fp]; dup {
			t.Errorf("%q and %q share fingerprint %s; one would silently "+
				"suppress the other", prev, c.label, c.fp)
		}
		seen[c.fp] = c.label
	}
}

func TestTheIssueTypeIsReadableInTheFingerprint(t *testing.T) {
	// The type is prefixed in clear text as well as hashed, because a
	// fingerprint shows up in operator-facing evidence and a bare hash tells
	// whoever is reading it nothing about what went wrong.
	fp := Fingerprint(domain.IssuePositionMismatch, "XAUUSD")
	want := string(domain.IssuePositionMismatch) + ":"
	if len(fp) <= len(want) || fp[:len(want)] != want {
		t.Fatalf("fingerprint %q is not prefixed with %q", fp, want)
	}
}

func TestPartBoundariesCannotBeForged(t *testing.T) {
	// Concatenating parts without a separator would make ("AB","C") and
	// ("A","BC") the same problem. They are not, and a collision here means
	// one real divergence hides another.
	if Fingerprint(domain.IssueExtraBrokerFill, "AB", "C") ==
		Fingerprint(domain.IssueExtraBrokerFill, "A", "BC") {
		t.Fatal("part boundaries are not encoded, so distinct problems collide")
	}
}

func TestAFingerprintWithNoPartsIsStillStable(t *testing.T) {
	// Account-wide issues (a balance mismatch, say) carry no discriminator.
	// They must still fingerprint consistently, or the account is halted anew
	// by every run.
	if Fingerprint(domain.IssueBalanceMismatch) != Fingerprint(domain.IssueBalanceMismatch) {
		t.Fatal("a part-less fingerprint is not stable")
	}
}

// The execution cursor freezes while any execution-derived issue is
// unresolved, because a run only re-detects an execution while it is still
// inside the fetch window. Getting the classification of a type wrong here
// reintroduces a defect that closed three real unbooked executions as "the
// divergence is gone" and released the account's halt.

func TestEveryIssueTypeIsClassifiedAsExecutionOrSnapshotDerived(t *testing.T) {
	execution := map[string]bool{}
	for _, t := range executionDerivedIssueTypes {
		execution[t] = true
	}
	snapshot := map[string]bool{}
	for _, t := range snapshotDerivedIssueTypes {
		snapshot[t] = true
	}

	for _, issueType := range domain.AllIssueTypes() {
		name := string(issueType)
		inExec, inSnap := execution[name], snapshot[name]
		switch {
		case inExec && inSnap:
			t.Errorf("%s is listed as both execution- and snapshot-derived", name)
		case !inExec && !inSnap:
			t.Errorf("%s is in neither list. Decide which it is: if its evidence is a "+
				"venue EXECUTION it must be execution-derived, or the cursor will "+
				"advance past it, the next run will not re-detect it, and the issue "+
				"will be closed as resolved while the divergence is still there", name)
		}
	}
}

func TestPositionAndBalanceNeedNoHeldWindow(t *testing.T) {
	// They are recomputed from a full snapshot on every run, so holding the
	// execution window open for them would widen every fetch for nothing.
	for _, name := range []string{
		string(domain.IssuePositionMismatch),
		string(domain.IssueBalanceMismatch),
	} {
		for _, held := range executionDerivedIssueTypes {
			if held == name {
				t.Errorf("%s is treated as execution-derived; it is computed from a full "+
					"snapshot and needs no hold", name)
			}
		}
	}
}

func TestTheAmbiguousExecutionTypesAreHeldOpen(t *testing.T) {
	// The types that carry an unattributable or contradictory execution are
	// the whole reason the hold exists. If one of these is ever dropped from
	// the list, the issue it raises can close itself.
	held := map[string]bool{}
	for _, t := range executionDerivedIssueTypes {
		held[t] = true
	}
	for _, name := range []string{
		string(domain.IssueExtraBrokerFill),
		string(domain.IssueExternalBrokerActivity),
		string(domain.IssueDuplicateExecutionReport),
		string(domain.IssueUnknownExecutionState),
	} {
		if !held[name] {
			t.Errorf("%s is not held open. An unresolved issue of this type would stop "+
				"being re-detected once the cursor passed its execution, and would then "+
				"be closed as though it had been fixed", name)
		}
	}
}
