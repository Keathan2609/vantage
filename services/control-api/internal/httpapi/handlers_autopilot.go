package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/store"
)

// The Autopilot switch: reading it is available to anyone signed in, changing
// it is ADMIN only.
//
// # Why reading is not restricted
//
// "Is the robot running?" is the first question anyone looking at this
// platform asks, and a viewer who cannot answer it cannot interpret anything
// else on the screen. There is nothing sensitive in the answer.
//
// # Why changing it is ADMIN
//
// Switching autopilot ON commits the platform to placing orders without a
// human in the loop, which is a larger decision than any single order a trader
// can make. Note that ADMIN cannot place an order at all, so the role that can
// start the machine is not the role that can trade -- the same separation the
// reconciliation repair routes use.

type autopilotView struct {
	Enabled bool `json:"enabled"`
	// Reason is why it was last set this way. Mandatory when changing, so this
	// is never empty.
	Reason    string  `json:"reason"`
	ChangedAt string  `json:"changed_at"`
	ChangedBy *string `json:"changed_by"`
	// Explanation states what the switch does and does not stop, because the
	// most common misreading of a halted system is that it has stopped
	// entirely.
	Explanation string `json:"explanation"`
}

func autopilotToView(st store.AutopilotState) autopilotView {
	v := autopilotView{
		Enabled:   st.Enabled,
		Reason:    st.Reason,
		ChangedAt: st.ChangedAt.UTC().Format(time.RFC3339),
	}
	if st.ChangedBy != nil {
		id := st.ChangedBy.String()
		v.ChangedBy = &id
	}
	if st.Enabled {
		v.Explanation = "Autonomous trading is ON: the scheduler evaluates PAPER " +
			"strategies and may create order intents without a human. Every order still " +
			"passes the full risk pipeline, trading authority and the kill switches."
	} else {
		v.Explanation = "Autonomous trading is OFF: no strategy evaluation will create an " +
			"order, and automated orders are refused inside the order transaction. Manual " +
			"trading, cancellation, flatten and every read path are unaffected."
	}
	return v
}

// handleAutopilot returns the current switch state.
func (s *Server) handleAutopilot(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Autopilot.Autopilot(r.Context())
	if err != nil {
		writeStoreError(w, r, err, "Autopilot state is unavailable.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"autopilot": autopilotToView(st)})
}

// handleAutopilotHistory returns the switch's history.
//
// Exists because the single-row state table holds only the present. Without
// history, a period of autonomous trading leaves no trace once the switch is
// flipped back, and an investigation into a trade cannot establish whether
// autopilot was even running when it was placed.
func (s *Server) handleAutopilotHistory(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.Autopilot.AutopilotHistory(r.Context(), 100)
	if err != nil {
		writeStoreError(w, r, err, "Autopilot history is unavailable.")
		return
	}
	out := make([]autopilotView, 0, len(entries))
	for _, e := range entries {
		out = append(out, autopilotToView(e))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"history": out})
}

type setAutopilotRequest struct {
	Enabled *bool  `json:"enabled"`
	Reason  string `json:"reason"`
}

// handleSetAutopilot switches autonomous trading on or off.
func (s *Server) handleSetAutopilot(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req setAutopilotRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	// A pointer, so "enabled": false is distinguishable from an omitted field.
	// Defaulting a missing value to false would silently switch autopilot off
	// for a caller who only meant to update the reason.
	if req.Enabled == nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
			"enabled is required and must be true or false.")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) < 10 {
		// The same floor as a reconciliation resolution. The reason is the
		// only record of why a human committed the platform to trading
		// unattended, and "ok" is not one.
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
			"A reason of at least 10 characters is required: it is the only record of "+
				"why autonomous trading was changed.")
		return
	}

	actorID := p.User.ID
	target := "global"
	metadata, _ := json.Marshal(map[string]string{
		"enabled": boolText(*req.Enabled),
		"reason":  reason,
	})

	var st store.AutopilotState
	err := s.store.Pool().InTx(r.Context(), func(tx pgx.Tx) error {
		var terr error
		st, terr = s.store.Autopilot.SetAutopilot(r.Context(), tx, *req.Enabled, reason, &actorID)
		if terr != nil {
			return terr
		}
		// Audited in the same transaction as the change, so an enabled
		// autopilot with no audit trail is not a state the database can hold.
		action := domain.AuditAutopilotDisabled
		if st.Enabled {
			action = domain.AuditAutopilotEnabled
		}
		_, aerr := s.store.Control.AppendAudit(r.Context(), tx, domain.AuditEvent{
			ActorUserID:   &actorID,
			ActorType:     "user",
			Action:        action,
			TargetType:    "autopilot",
			TargetID:      &target,
			Result:        domain.AuditSuccess,
			RequestID:     logging.RequestID(r.Context()),
			CorrelationID: logging.CorrelationID(r.Context()),
			Metadata:      metadata,
		})
		return aerr
	})
	if err != nil {
		if errors.Is(err, store.ErrAutopilotReasonRequired) {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
				"A reason is required.")
			return
		}
		writeStoreError(w, r, err, "Autopilot could not be changed.")
		return
	}

	log := logging.FromContext(r.Context())
	if st.Enabled {
		log.Warn("autopilot ENABLED: autonomous trading may now place orders",
			"actor", actorID.String(), "reason", reason)
	} else {
		log.Info("autopilot disabled", "actor", actorID.String(), "reason", reason)
	}

	writeJSON(w, r, http.StatusOK, map[string]any{"autopilot": autopilotToView(st)})
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
