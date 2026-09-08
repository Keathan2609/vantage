package app

import (
	"github.com/vantage/control-api/internal/fx"
	"github.com/vantage/control-api/internal/risk"
)

// newRiskEngine builds the risk engine.
//
// It is constructed here, once, and handed only to the OMS. No other component
// receives a reference, and in particular the orchestrator does not: strategy
// code must not be able to ask the risk engine what it would allow and then
// shape a request to slip past it.
func newRiskEngine(converter *fx.Converter) *risk.Engine {
	return risk.New(converter)
}
