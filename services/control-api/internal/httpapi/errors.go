package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
	"github.com/vantage/control-api/internal/logging"
	"github.com/vantage/control-api/internal/store"
)

// errorBody is the single error shape every endpoint returns.
//
// Clients branch on `code`, never on `message`. The message is for humans and
// may change; the code is part of the contract.
type errorBody struct {
	Error struct {
		Code      string            `json:"code"`
		Message   string            `json:"message"`
		Detail    map[string]string `json:"detail,omitempty"`
		RequestID string            `json:"request_id,omitempty"`
	} `json:"error"`
}

// writeError sends a structured error.
//
// The message is written by Vantage, never derived from an internal error
// string. Internal errors carry table names, query fragments and file paths;
// those go to the log, and the client gets a stable code plus a sentence.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, detail ...map[string]string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.RequestID = logging.RequestID(r.Context())
	if len(detail) > 0 {
		body.Error.Detail = detail[0]
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logging.FromContext(r.Context()).Error("failed to write error response", "error", err.Error())
	}
}

// writeJSON sends a success payload.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logging.FromContext(r.Context()).Error("failed to write response", "error", err.Error())
	}
}

// writeStoreError maps a persistence error to a response.
//
// Not-found and not-yours are both reported as 404 with the same body. Telling
// a caller that an object exists but belongs to someone else turns any listing
// endpoint into an enumeration oracle.
func writeStoreError(w http.ResponseWriter, r *http.Request, err error, notFoundMessage string) {
	log := logging.FromContext(r.Context())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", notFoundMessage)
	case errors.Is(err, store.ErrStaleVersion):
		writeError(w, r, http.StatusConflict, "stale_version",
			"This record changed while you were editing it. Reload and try again.")
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrConstraint):
		// The constraint name is logged, not returned: it describes the schema.
		log.Warn("constraint violation", "error", err.Error(), "path", r.URL.Path)
		writeError(w, r, http.StatusConflict, "conflict",
			"That change conflicts with an existing record or a platform rule.")
	default:
		log.Error("unhandled store error", "error", err.Error(), "path", r.URL.Path)
		writeError(w, r, http.StatusInternalServerError, "internal_error",
			"Something went wrong handling this request.")
	}
}

// decodeJSON reads and validates a JSON request body.
//
// Unknown fields are rejected rather than ignored. Silently dropping an
// unrecognised field is how a client ends up believing it set a stop loss that
// the server never saw.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	ct := r.Header.Get("Content-Type")
	if ct != "" {
		base, _, _ := strings.Cut(ct, ";")
		if strings.TrimSpace(base) != "application/json" {
			writeError(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"This endpoint accepts application/json.")
			return errors.New("unsupported media type")
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var maxBytes *http.MaxBytesError

		switch {
		case errors.As(err, &maxBytes):
			writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large",
				"The request body is too large.")
		case errors.As(err, &syntaxErr):
			writeError(w, r, http.StatusBadRequest, "invalid_json",
				fmt.Sprintf("The request body is not valid JSON (at byte %d).", syntaxErr.Offset))
		case errors.As(err, &typeErr):
			writeError(w, r, http.StatusBadRequest, "invalid_field",
				fmt.Sprintf("Field %q has the wrong type.", typeErr.Field))
		case strings.Contains(err.Error(), "unknown field"):
			field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
			writeError(w, r, http.StatusBadRequest, "unknown_field",
				fmt.Sprintf("Unrecognised field %q.", field))
		default:
			writeError(w, r, http.StatusBadRequest, "invalid_json",
				"The request body could not be read.")
		}
		return err
	}

	// A body containing a second JSON document is rejected: it is ambiguous
	// which one the caller meant.
	if err := dec.Decode(&struct{}{}); err == nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json",
			"The request body must contain exactly one JSON object.")
		return errors.New("multiple json values")
	}
	return nil
}

// validationError collects field-level problems.
type validationError struct {
	fields map[string]string
}

func newValidation() *validationError { return &validationError{fields: map[string]string{}} }

func (v *validationError) add(field, message string) { v.fields[field] = message }

func (v *validationError) ok() bool { return len(v.fields) == 0 }

func (v *validationError) write(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusUnprocessableEntity, "validation_failed",
		"Some fields are invalid.", v.fields)
}

// parseUUID reads a UUID path parameter.
func parseUUID(w http.ResponseWriter, r *http.Request, raw, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_identifier",
			fmt.Sprintf("%s is not a valid identifier.", name))
		return uuid.Nil, false
	}
	return id, true
}

// parseDecimal parses a decimal string from a request field.
func parseDecimal(raw string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil {
		return decimal.Zero, fmt.Errorf("must be a decimal number")
	}
	return d, nil
}

// rejectionStatus maps a structured trading rejection to an HTTP status.
//
// The distinction that matters: a rejection caused by the CALLER's request is
// 422, while one caused by platform or market STATE is 409. A client retrying
// blindly should be able to tell "fix your request" from "try later".
func rejectionStatus(code domain.RejectCode) int {
	switch code {
	case domain.RejectSchemaInvalid, domain.RejectQuantityInvalid, domain.RejectPriceInvalid:
		return http.StatusUnprocessableEntity
	case domain.RejectUnauthenticated:
		return http.StatusUnauthorized
	case domain.RejectForbidden, domain.RejectAccountNotOwned, domain.RejectNoAuthority,
		domain.RejectAuthorityExpired, domain.RejectAuthorityRevoked,
		domain.RejectInstrumentNotAllowed, domain.RejectStrategyNotAllowed,
		domain.RejectOrderTypeNotAllowed, domain.RejectAutomationDisabled,
		domain.RejectModeNotPermitted:
		return http.StatusForbidden
	case domain.RejectIdempotencyConflict:
		return http.StatusConflict
	case domain.RejectDuplicateCommand, domain.RejectStaleVersion:
		return http.StatusConflict
	case domain.RejectBrokerUnavailable:
		return http.StatusServiceUnavailable
	case domain.RejectInternalError:
		return http.StatusInternalServerError
	default:
		// Risk limits, kill switches, market closure, stale data: the request
		// is well-formed but the platform will not act on it right now.
		return http.StatusConflict
	}
}
