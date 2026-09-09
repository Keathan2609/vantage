package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// A replay run record is refused before it reaches the database when it could
// not serve as evidence. The validation is deliberately in Go as well as in the
// schema: the CHECK constraints catch a bad write, but a caller that receives
// ErrReplayRunIncomplete is told which field is missing and why it matters,
// whereas a constraint violation arrives as an opaque 500.
//
// The store is constructed with a nil querier on purpose. If any of these cases
// reached the database the test would panic, which is a stronger assertion than
// checking the error.

func TestARunWithNoDatasetIsRefused(t *testing.T) {
	s := &ReplayStore{}
	err := s.RecordRun(context.Background(), ReplayRun{
		ID: uuid.New(), DatasetHash: hex64, CodeSHA: "sha", State: "paused",
	})
	if !errors.Is(err, ErrReplayRunIncomplete) {
		t.Fatalf("err = %v, want ErrReplayRunIncomplete", err)
	}
}

func TestARunWithNoDatasetHashIsRefused(t *testing.T) {
	// The hash is what catches an edited fixture. Without it, "the same
	// dataset" means "the same name", and a name is not evidence.
	s := &ReplayStore{}
	err := s.RecordRun(context.Background(), ReplayRun{
		ID: uuid.New(), DatasetID: "trend-clean", CodeSHA: "sha", State: "paused",
	})
	if !errors.Is(err, ErrReplayRunIncomplete) {
		t.Fatalf("err = %v, want ErrReplayRunIncomplete", err)
	}
}

func TestARunWithNoCodeSHAIsRefused(t *testing.T) {
	// Without the code SHA a result cannot be tied to the strategies and risk
	// rules that produced it, so two runs that differ are indistinguishable
	// from two runs of different code.
	s := &ReplayStore{}
	err := s.RecordRun(context.Background(), ReplayRun{
		ID: uuid.New(), DatasetID: "trend-clean", DatasetHash: hex64, State: "paused",
	})
	if !errors.Is(err, ErrReplayRunIncomplete) {
		t.Fatalf("err = %v, want ErrReplayRunIncomplete", err)
	}
}

func TestARunWithNoIDIsRefused(t *testing.T) {
	s := &ReplayStore{}
	err := s.RecordRun(context.Background(), ReplayRun{
		DatasetID: "trend-clean", DatasetHash: hex64, CodeSHA: "sha", State: "paused",
	})
	if !errors.Is(err, ErrReplayRunIncomplete) {
		t.Fatalf("err = %v, want ErrReplayRunIncomplete", err)
	}
}

func TestWhitespaceIsNotAnIdentity(t *testing.T) {
	// A caller filling required fields with spaces to get past the check would
	// produce a row that looks complete and says nothing.
	s := &ReplayStore{}
	err := s.RecordRun(context.Background(), ReplayRun{
		ID: uuid.New(), DatasetID: "   ", DatasetHash: hex64, CodeSHA: " ", State: "paused",
	})
	if !errors.Is(err, ErrReplayRunIncomplete) {
		t.Fatalf("err = %v, want ErrReplayRunIncomplete", err)
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
