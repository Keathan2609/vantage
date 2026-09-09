package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/replay"
	"github.com/vantage/control-api/internal/store"
)

// Market replay controls.
//
// # Why these are admin-only and development-only
//
// Engaging a replay puts the WHOLE PROCESS on dataset time. Session expiry,
// rate-limit windows, audit timestamps and the daily P&L boundary all move to
// wherever the dataset sits. That is acceptable for a development tool and
// would be indefensible anywhere else, so the routes exist only when the
// engine does -- which requires `VANTAGE_MARKET_DATA_PROVIDER=replay`, which
// `config.Load` refuses outside development and test.
//
// # Why a dataset is named by ID and never by path
//
// A control endpoint that accepted a filename would be a file-read primitive
// wearing a trading-system costume, and no amount of path cleaning makes that
// a good idea. Dataset IDs resolve through `replay.Registry`, an allowlist
// fixed at build time from declarations in code. An unknown ID is a 404 and
// never reaches a filesystem call.

// replayLabel travels with every replay payload.
//
// A single constant because the whole purpose of the label is that it is
// always present and always identical: a second copy would eventually differ
// from this one, and the difference would be in the direction of sounding more
// like a result.
const replayLabel = "SIMULATED — REPLAY — PAPER. Not a claim about future performance."

type replayRunView struct {
	ID          string  `json:"id"`
	DatasetID   string  `json:"dataset_id"`
	DatasetHash string  `json:"dataset_hash"`
	CodeSHA     string  `json:"code_sha"`
	ConfigHash  string  `json:"config_hash"`
	Seed        int64   `json:"seed"`
	State       string  `json:"state"`
	FromTime    string  `json:"from_time"`
	ToTime      string  `json:"to_time"`
	StartedAt   string  `json:"started_at"`
	FinishedAt  *string `json:"finished_at"`
	Error       string  `json:"error,omitempty"`

	Steps         int `json:"steps"`
	BarsProcessed int `json:"bars_processed"`
	Errors        int `json:"errors"`

	RowsPlayed int `json:"rows_played"`
	RowsTotal  int `json:"rows_total"`
	// ReplayNow is where the application clock currently sits. The single most
	// useful field for anyone wondering why a decision looks odd.
	ReplayNow string `json:"replay_now,omitempty"`
	// Simulated is always true and travels with the payload so no serialiser
	// can omit it and let replay numbers read as live ones.
	Simulated bool   `json:"simulated"`
	Label     string `json:"label"`
}

func (s *Server) replayView(run replay.Run) replayRunView {
	v := replayRunView{
		ID: run.ID.String(), DatasetID: run.DatasetID, DatasetHash: run.DatasetHash,
		CodeSHA: run.CodeSHA, ConfigHash: run.ConfigHash, Seed: run.Seed,
		State:     string(run.State),
		FromTime:  run.FromTime.UTC().Format(time.RFC3339),
		ToTime:    run.ToTime.UTC().Format(time.RFC3339),
		StartedAt: run.StartedAt.UTC().Format(time.RFC3339),
		Error:     run.Error,

		Steps: run.Counters.Steps, BarsProcessed: run.Counters.BarsProcessed,
		Errors: run.Counters.Errors,

		Simulated: true,
		Label:     replayLabel,
	}
	if run.FinishedAt != nil {
		f := run.FinishedAt.UTC().Format(time.RFC3339)
		v.FinishedAt = &f
	}
	if s.replay != nil {
		v.RowsPlayed, v.RowsTotal = s.replay.Progress()
		if now, ok := s.replay.ReplayNow(); ok {
			v.ReplayNow = now.UTC().Format(time.RFC3339)
		}
	}
	return v
}

// replayRecordView is a run read back from the database.
//
// Separate from replayRunView because a stored run has no live progress: rows
// played, the current replay instant and whether the clock is engaged are
// properties of a run happening now. Serving them as zeroes on a historical
// record would read as "this run played nothing".
type replayRecordView struct {
	ID          string  `json:"id"`
	DatasetID   string  `json:"dataset_id"`
	DatasetHash string  `json:"dataset_hash"`
	CodeSHA     string  `json:"code_sha"`
	ConfigHash  string  `json:"config_hash"`
	Seed        int64   `json:"seed"`
	State       string  `json:"state"`
	FromTime    string  `json:"from_time"`
	ToTime      string  `json:"to_time"`
	StartedAt   string  `json:"started_at"`
	FinishedAt  *string `json:"finished_at"`

	Steps         int `json:"steps"`
	BarsProcessed int `json:"bars_processed"`
	Errors        int `json:"errors"`

	Failure string `json:"failure,omitempty"`
	// Warnings are the preflight findings recorded at Start. A run with no
	// orders and a run that was never permitted to place one are
	// indistinguishable without them.
	Warnings []string `json:"warnings"`

	Simulated bool   `json:"simulated"`
	Label     string `json:"label"`
}

func replayRecord(run store.ReplayRun) replayRecordView {
	v := replayRecordView{
		ID: run.ID.String(), DatasetID: run.DatasetID, DatasetHash: run.DatasetHash,
		CodeSHA: run.CodeSHA, ConfigHash: run.ConfigHash, Seed: run.Seed,
		State:     run.State,
		FromTime:  run.FromTime.UTC().Format(time.RFC3339),
		ToTime:    run.ToTime.UTC().Format(time.RFC3339),
		StartedAt: run.StartedAt.UTC().Format(time.RFC3339),

		Steps: run.Steps, BarsProcessed: run.BarsProcessed, Errors: run.StepErrors,
		Failure:  run.Failure,
		Warnings: run.Warnings,

		Simulated: true,
		Label:     replayLabel,
	}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	if run.FinishedAt != nil {
		f := run.FinishedAt.UTC().Format(time.RFC3339)
		v.FinishedAt = &f
	}
	return v
}

// handleReplayRuns lists recorded runs, newest first.
//
// Deliberately NOT gated on a live engine. The control endpoints exist only
// where a replay can be driven, but the record of what was replayed is
// evidence, and evidence that disappears when the process is configured
// differently is not much use. Reading it is an admin-only read of data that
// carries "simulated" in every row.
func (s *Server) handleReplayRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// An allowlist key or empty, passed as a parameter. An unknown dataset is
	// an empty list, never a query built from the string.
	dataset := strings.TrimSpace(r.URL.Query().Get("dataset"))
	runs, err := s.store.Replay.Runs(r.Context(), dataset, limit)
	if err != nil {
		writeStoreError(w, r, err, "Replay runs not found.")
		return
	}
	out := make([]replayRecordView, 0, len(runs))
	for _, run := range runs {
		out = append(out, replayRecord(run))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"runs": out, "simulated": true, "label": replayLabel,
	})
}

// handleReplayRun reports one recorded run.
func (s *Server) handleReplayRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, r, chi.URLParam(r, "runID"), "Run id")
	if !ok {
		return
	}
	run, err := s.store.Replay.Run(r.Context(), id)
	if err != nil {
		writeStoreError(w, r, err, "No replay run with that id was recorded.")
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"run": replayRecord(run)})
}

// handleReplayDatasets lists the allowlisted datasets.
func (s *Server) handleReplayDatasets(w http.ResponseWriter, r *http.Request) {
	if s.replay == nil {
		s.replayUnavailable(w, r)
		return
	}
	type view struct {
		ID           string   `json:"id"`
		Description  string   `json:"description"`
		Hash         string   `json:"hash"`
		Rows         int      `json:"rows"`
		Instruments  []string `json:"instruments"`
		From         string   `json:"from"`
		To           string   `json:"to"`
		TimeframeSet []string `json:"timeframes"`
	}
	out := []view{}
	for _, ds := range s.replay.Datasets() {
		from, to := ds.Span()
		tfs := map[string]bool{}
		for _, row := range ds.Rows {
			tfs[string(row.Timeframe)] = true
		}
		list := make([]string, 0, len(tfs))
		for tf := range tfs {
			list = append(list, tf)
		}
		out = append(out, view{
			ID: ds.ID, Description: ds.Description, Hash: ds.Hash,
			Rows: len(ds.Rows), Instruments: ds.Instruments(),
			From: from.UTC().Format(time.RFC3339), To: to.UTC().Format(time.RFC3339),
			TimeframeSet: list,
		})
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"datasets": out})
}

// handleReplayStatus reports the current run.
func (s *Server) handleReplayStatus(w http.ResponseWriter, r *http.Request) {
	if s.replay == nil {
		s.replayUnavailable(w, r)
		return
	}
	run, active := s.replay.Status()
	if !active {
		writeJSON(w, r, http.StatusOK, map[string]any{
			"active":    false,
			"engaged":   s.replay.Engaged(),
			"simulated": true,
		})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"active": true, "engaged": s.replay.Engaged(), "run": s.replayView(run),
	})
}

type replayControlRequest struct {
	Action string `json:"action"`
	// Dataset is required for START and is an allowlist key, never a path.
	Dataset string `json:"dataset"`
	Seed    int64  `json:"seed"`
	Speed   string `json:"speed"`
	// Steps applies to STEP. Bounded, because a request that asked for a
	// million steps would hold the handler for the life of the process.
	Steps int `json:"steps"`
	// Reason is required for START and STOP, like every other control that
	// changes what the platform does on its own.
	Reason string `json:"reason"`
}

const maxReplayStepsPerRequest = 2000

// handleReplayControl drives the run.
func (s *Server) handleReplayControl(w http.ResponseWriter, r *http.Request) {
	if s.replay == nil {
		s.replayUnavailable(w, r)
		return
	}
	p, _ := principalFrom(r.Context())
	var req replayControlRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	action := strings.ToLower(strings.TrimSpace(req.Action))
	log := logging.FromContext(r.Context())

	var (
		run replay.Run
		err error
	)
	switch action {
	case "start":
		if strings.TrimSpace(req.Dataset) == "" {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
				"A dataset id is required. List them at GET /api/v1/replay/datasets.")
			return
		}
		if len(strings.TrimSpace(req.Reason)) < 10 {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
				"A reason of at least 10 characters is required: a replay puts this "+
					"process on dataset time and the record should say why.")
			return
		}
		run, err = s.replay.Start(replay.Options{
			DatasetID: req.Dataset, Seed: req.Seed, Speed: req.Speed,
			CodeSHA: s.commit, ConfigHash: s.configHash,
		})
	case "step":
		steps := req.Steps
		if steps <= 0 {
			steps = 1
		}
		if steps > maxReplayStepsPerRequest {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
				"At most "+strconv.Itoa(maxReplayStepsPerRequest)+" steps per request. "+
					"Use \"advance\" to run to the end of the dataset.")
			return
		}
		run, err = s.replay.Step(r.Context(), steps)
	case "advance":
		run, err = s.replay.Advance(r.Context())
	case "pause":
		run, err = s.replay.Pause()
	case "resume":
		run, err = s.replay.Resume()
	case "reset":
		run, err = s.replay.Reset()
	case "stop":
		if len(strings.TrimSpace(req.Reason)) < 10 {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
				"A reason of at least 10 characters is required.")
			return
		}
		run, err = s.replay.Stop()
	case "speed":
		run, err = s.replay.SetSpeed(req.Speed)
	case "reconcile":
		if rerr := s.replay.Reconcile(r.Context()); rerr != nil {
			s.writeReplayError(w, r, rerr)
			return
		}
		run, _ = s.replay.Status()
	default:
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
			"action must be one of start, step, advance, pause, resume, reset, "+
				"stop, speed, reconcile.")
		return
	}
	if err != nil {
		s.writeReplayError(w, r, err)
		return
	}

	// Audited for start and stop only. A step is not a decision; starting a
	// replay is, and so is ending one early.
	if action == "start" || action == "stop" {
		target := run.ID.String()
		s.auditReplay(r, p.User.ID, action, target, req.Dataset, req.Reason, run.DatasetHash)
		log.Warn("market replay control", "action", action,
			"run", target, "dataset", run.DatasetID, "actor", p.User.ID.String())
	}

	writeJSON(w, r, http.StatusOK, map[string]any{"run": s.replayView(run)})
}

// handleReplayInject arms a fault mid-run.
//
// Separate from the venue's own fault injection, which is about the BROKER.
// These are about the market data: a disconnected provider and a widened book
// are conditions the platform must refuse to trade through, and they cannot be
// expressed in a dataset because they are not price history.
func (s *Server) handleReplayInject(w http.ResponseWriter, r *http.Request) {
	if s.replay == nil {
		s.replayUnavailable(w, r)
		return
	}
	var req struct {
		Fault      string `json:"fault"`
		Instrument string `json:"instrument_id"`
		Value      string `json:"value"`
		Clear      bool   `json:"clear"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	switch strings.ToLower(strings.TrimSpace(req.Fault)) {
	case "outage":
		if err := s.replay.SetOutage(!req.Clear); err != nil {
			s.writeReplayError(w, r, err)
			return
		}
	case "spread":
		if req.Instrument == "" {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
				"instrument_id is required for a spread fault.")
			return
		}
		if err := s.replay.SetSpreadFraction(req.Instrument, req.Value); err != nil {
			s.writeReplayError(w, r, err)
			return
		}
	default:
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request",
			"fault must be outage or spread.")
		return
	}

	run, _ := s.replay.Status()
	writeJSON(w, r, http.StatusOK, map[string]any{"run": s.replayView(run)})
}

func (s *Server) replayUnavailable(w http.ResponseWriter, r *http.Request) {
	// 404, not 403. The route genuinely does not exist in this process, and
	// saying "forbidden" would imply it could be reached with more privilege.
	writeError(w, r, http.StatusNotFound, "not_found",
		"Market replay is not enabled in this process. It requires "+
			"VANTAGE_MARKET_DATA_PROVIDER=replay, which is refused outside development.")
}

func (s *Server) writeReplayError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, replay.ErrNoDataset):
		writeError(w, r, http.StatusNotFound, "not_found", "No such dataset.")
	case errors.Is(err, replay.ErrAlreadyActive):
		writeError(w, r, http.StatusConflict, "conflict",
			"A replay run is already active. Stop it before starting another.")
	case errors.Is(err, replay.ErrNotActive):
		writeError(w, r, http.StatusConflict, "conflict", "No replay run is active.")
	case errors.Is(err, replay.ErrTerminal):
		writeError(w, r, http.StatusConflict, "conflict",
			"This run has finished. Reset it or start a new one.")
	case errors.Is(err, replay.ErrNotPaused):
		writeError(w, r, http.StatusConflict, "conflict", "The run is not paused.")
	default:
		var invalid replay.ErrDatasetInvalid
		if errors.As(err, &invalid) {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_dataset", invalid.Error())
			return
		}
		writeStoreError(w, r, err, "The replay control could not be applied.")
	}
}

func (s *Server) auditReplay(r *http.Request, actorID uuid.UUID, action, runID,
	dataset, reason, datasetHash string) {

	target := runID
	metadata, _ := json.Marshal(map[string]string{
		"action": action, "dataset": dataset, "dataset_hash": datasetHash,
		"reason": reason,
	})
	uid := actorID
	err := s.store.Pool().InTx(r.Context(), func(tx pgx.Tx) error {
		_, aerr := s.store.Control.AppendAudit(r.Context(), tx, domain.AuditEvent{
			ActorUserID:   &uid,
			ActorType:     "user",
			Action:        domain.AuditReplayControl,
			TargetType:    "replay_run",
			TargetID:      &target,
			Result:        domain.AuditSuccess,
			RequestID:     logging.RequestID(r.Context()),
			CorrelationID: logging.CorrelationID(r.Context()),
			Metadata:      metadata,
		})
		return aerr
	})
	if err != nil {
		logging.FromContext(r.Context()).Error("could not audit a replay control",
			"action", action, "error", err.Error())
	}
}
