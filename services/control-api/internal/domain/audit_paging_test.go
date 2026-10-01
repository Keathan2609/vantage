package domain

import "testing"

// Verifying a chain in pages must check the link ACROSS a page boundary.
//
// The audit verifier used to fetch the first ten thousand events and report a
// verdict for the whole chain. Past that length it examined the oldest events,
// never looked at the recent end -- which is where tampering would be -- and
// still answered "verified". Paging fixes that only if each page is verified
// against the previous page's final hash; verifying each page from genesis
// independently would pass a chain that had been cut and respliced at a
// boundary.

func chainOf(t *testing.T, n int) []AuditEvent {
	t.Helper()
	events := make([]AuditEvent, 0, n)
	prev := GenesisHash
	for i := 0; i < n; i++ {
		e := AuditEvent{
			Sequence: int64(i + 1),
			Action:   "test.event",
			Result:   "success",
			PrevHash: prev,
		}
		e.Hash = e.ComputeHash(prev)
		events = append(events, e)
		prev = e.Hash
	}
	return events
}

func TestAChainVerifiesAcrossAPageBoundary(t *testing.T) {
	all := chainOf(t, 10)
	first, second := all[:5], all[5:]

	if ok, _ := VerifyChainFrom(first, GenesisHash); !ok {
		t.Fatal("the first page did not verify from genesis")
	}
	if ok, at := VerifyChainFrom(second, first[len(first)-1].Hash); !ok {
		t.Fatalf("the second page did not verify against the first page's tail, at %d", at)
	}
}

func TestASeveredLinkAtAPageBoundaryIsCaught(t *testing.T) {
	// Two separately valid chains spliced together. Each half verifies on its
	// own from genesis; only the boundary check sees that the second does not
	// follow the first.
	first := chainOf(t, 5)
	second := chainOf(t, 5)

	if ok, _ := VerifyChainFrom(second, GenesisHash); !ok {
		t.Fatal("the fixture is wrong: the second chain should verify from genesis")
	}
	if ok, at := VerifyChainFrom(second, first[len(first)-1].Hash); ok {
		t.Fatal("a chain spliced at a page boundary verified; paging that checks " +
			"each page from genesis would accept a cut-and-respliced history")
	} else if at != 0 {
		t.Errorf("the break should be reported at the first event of the page, got %d", at)
	}
}

func TestVerifyChainStillStartsAtGenesis(t *testing.T) {
	// The original entry point must keep its meaning: callers that pass a whole
	// chain get a verdict against genesis, not against whatever came before.
	all := chainOf(t, 4)
	if ok, _ := VerifyChain(all); !ok {
		t.Fatal("a complete chain failed VerifyChain")
	}
	if ok, _ := VerifyChain(all[1:]); ok {
		t.Fatal("a chain missing its first event verified against genesis")
	}
}
