package httpapi

// Development-only affordances.
//
// One endpoint, gated three ways: the environment must be development or test,
// the caller must hold the trader or admin role, and the broker must actually
// be the mock venue. A fault injector reachable in a deployment would be a way
// to make a venue lie on demand, so the gate is not a formality.
//
// It exists because the alternative is worse: without it, reconciliation and
// OMS failure handling can only be tested by stubbing the adapter, which
// proves the stub behaves as the test expects rather than that the pipeline
// survives a venue that misbehaves.

import (
	"net/http"
	"time"

	brokermock "github.com/vantage/control-api/internal/broker/mock"
	"github.com/vantage/control-api/internal/logging"
)

type brokerFaultRequest struct {
	// Fault is one of the names in brokermock.AllFaults.
	Fault string `json:"fault"`
	// Times is how many calls the fault applies to. Exact, not probabilistic.
	Times int `json:"times"`
	// DelayMS is used by latency and timeout.
	DelayMS int `json:"delay_ms,omitempty"`
	// Factor is the adverse fraction for slippage, or the multiplier for
	// spread expansion.
	Factor string `json:"factor,omitempty"`
	// Fraction is the portion filled for a forced partial fill.
	Fraction string `json:"fraction,omitempty"`
}

// handleArmBrokerFault arms a deterministic venue fault.
func (s *Server) handleArmBrokerFault(w http.ResponseWriter, r *http.Request) {
	if !s.devFaultsAllowed(w, r) {
		return
	}
	var req brokerFaultRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	fault := brokermock.Fault(req.Fault)
	if !fault.Valid() {
		writeError(w, r, http.StatusUnprocessableEntity, "unknown_fault",
			"Unknown fault. Valid names are listed by GET on this endpoint.")
		return
	}
	if req.Times <= 0 {
		req.Times = 1
	}

	spec := brokermock.FaultSpec{Delay: time.Duration(req.DelayMS) * time.Millisecond}
	if req.Factor != "" {
		d, err := parseDecimal(req.Factor)
		if err != nil {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_factor",
				"factor must be a decimal number.")
			return
		}
		spec.Factor = d
	}
	if req.Fraction != "" {
		d, err := parseDecimal(req.Fraction)
		if err != nil {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_fraction",
				"fraction must be a decimal number.")
			return
		}
		spec.Fraction = d
	}

	if err := s.mockBroker.Faults().Arm(fault, req.Times, spec); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_fault", err.Error())
		return
	}
	logging.FromContext(r.Context()).Warn("broker fault armed",
		"fault", req.Fault, "times", req.Times)

	writeJSON(w, r, http.StatusOK, map[string]any{
		"armed": req.Fault, "times": req.Times,
		"note": "Deterministic and exact: the fault fires this many times and then disarms.",
	})
}

// handleResetBrokerFaults disarms everything.
func (s *Server) handleResetBrokerFaults(w http.ResponseWriter, r *http.Request) {
	if !s.devFaultsAllowed(w, r) {
		return
	}
	s.mockBroker.Faults().Reset()
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "reset"})
}

// handleListBrokerFaults reports the available modes and what is armed.
func (s *Server) handleListBrokerFaults(w http.ResponseWriter, r *http.Request) {
	if !s.devFaultsAllowed(w, r) {
		return
	}
	available := make([]string, 0, len(brokermock.AllFaults))
	for _, f := range brokermock.AllFaults {
		available = append(available, string(f))
	}
	armed := map[string]int{}
	for f, remaining := range s.mockBroker.Faults().Armed() {
		armed[string(f)] = remaining
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"available": available,
		"armed":     armed,
		"enabled":   s.mockBroker.Faults().Enabled(),
	})
}

// devFaultsAllowed enforces the environment and adapter gates.
func (s *Server) devFaultsAllowed(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.IsDevelopment() {
		writeError(w, r, http.StatusNotFound, "not_found",
			"Fault injection exists only in development.")
		return false
	}
	if s.mockBroker == nil {
		writeError(w, r, http.StatusConflict, "no_mock_broker",
			"The mock venue is not loaded, so there is nothing to inject faults into.")
		return false
	}
	return true
}
