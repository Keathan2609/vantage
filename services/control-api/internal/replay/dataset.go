// Package replay drives the real application pipeline from a fixed dataset.
//
// # What this is for
//
// Every autonomous-behaviour test before this milestone either drove the
// decision layer in isolation or waited on the real generated market. Neither
// proves the thing that matters: that market data entering the platform's own
// ingestion comes out the far end as a fill, a ledger entry and an audit
// trail, deterministically.
//
// So this package does not simulate trading. It steps a dataset forward and
// calls the SAME jobs the scheduler calls in ordinary operation -- ingestion,
// bar aggregation, resting orders, strategy evaluation, the outbox. There is no
// alternate trading path, and adding one would defeat the purpose: a test
// engine proves the test engine works.
//
// # Determinism
//
// Replay time drives the clock, not the other way round. The clock the whole
// application reads is derived from the dataset's own timestamps, so quote
// ages, bar buckets, market sessions, event windows and daily P&L boundaries
// all move with the data. A run started on a Sunday in December produces the
// same decisions as one started on a Tuesday in March.
package replay

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vantage/control-api/internal/domain"
)

// Dataset is a named, hashed, deterministic market series.
type Dataset struct {
	// ID is the allowlist key. Callers name a dataset by ID and never by path:
	// a replay endpoint that took a filename would be a file-read primitive
	// wearing a trading-system costume.
	ID          string
	Description string
	// Hash is the SHA-256 of the parsed rows in canonical form. Recorded on
	// every run, so a result can be tied to the exact data that produced it --
	// and so editing a fixture invalidates the runs that used it rather than
	// silently changing their meaning.
	Hash   string
	Source string
	Rows   []Row
}

// Row is one observation.
//
// Deliberately carries both a two-sided quote and a bar. The pipeline needs
// quotes (ingestion, data quality, execution pricing) and bars (indicators,
// strategies), and a dataset that supplied only one would force this package to
// invent the other -- which is exactly the kind of quiet fabrication that makes
// a replay result untrustworthy.
type Row struct {
	InstrumentID string
	Timeframe    domain.Timeframe
	// Timestamp is the bar's OPEN time, in UTC.
	Timestamp time.Time
	Open      decimal.Decimal
	High      decimal.Decimal
	Low       decimal.Decimal
	Close     decimal.Decimal
	Volume    decimal.Decimal
	// Bid and Ask are the quote at the bar's close. When the fixture leaves
	// them empty they are derived from Close and SpreadFraction, which is
	// recorded on the row so the derivation is visible rather than implied.
	Bid            decimal.Decimal
	Ask            decimal.Decimal
	SpreadFraction decimal.Decimal
	// Session is what the fixture claims the session was. Advisory: the market
	// clock remains authoritative, and a disagreement is reported rather than
	// resolved in the fixture's favour.
	Session string
	Source  string
}

// header is the exact column order a dataset file must use.
//
// A fixed order rather than a flexible header map, on purpose: a dataset is
// financial evidence, and a column silently landing in the wrong field because
// someone reordered a spreadsheet is not a failure mode worth allowing.
var header = []string{
	"instrument_id", "timeframe", "timestamp",
	"open", "high", "low", "close", "volume",
	"bid", "ask", "spread_fraction", "session", "source",
}

// ErrDatasetInvalid means a dataset file could not be trusted.
type ErrDatasetInvalid struct {
	Dataset string
	Line    int
	Reason  string
}

func (e ErrDatasetInvalid) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("replay: dataset %q line %d: %s", e.Dataset, e.Line, e.Reason)
	}
	return fmt.Sprintf("replay: dataset %q: %s", e.Dataset, e.Reason)
}

// LoadDataset parses and validates one dataset from a filesystem.
//
// Takes an fs.FS rather than a path so the caller decides what is reachable.
// The embedded fixture set is the only thing wired in production code; a test
// may pass its own in-memory filesystem without that becoming a way to read
// arbitrary files at runtime.
func LoadDataset(fsys fs.FS, id, description, name string) (Dataset, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return Dataset{}, ErrDatasetInvalid{Dataset: id, Reason: err.Error()}
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = len(header)
	reader.TrimLeadingSpace = true

	head, err := reader.Read()
	if err != nil {
		return Dataset{}, ErrDatasetInvalid{Dataset: id, Reason: "unreadable header: " + err.Error()}
	}
	for i, want := range header {
		if !strings.EqualFold(strings.TrimSpace(head[i]), want) {
			return Dataset{}, ErrDatasetInvalid{
				Dataset: id, Line: 1,
				Reason: fmt.Sprintf("column %d is %q, want %q", i+1, head[i], want),
			}
		}
	}

	ds := Dataset{ID: id, Description: description, Source: name}
	line := 1
	for {
		record, rerr := reader.Read()
		if rerr == io.EOF {
			break
		}
		line++
		if rerr != nil {
			return Dataset{}, ErrDatasetInvalid{Dataset: id, Line: line, Reason: rerr.Error()}
		}
		row, perr := parseRow(record)
		if perr != nil {
			return Dataset{}, ErrDatasetInvalid{Dataset: id, Line: line, Reason: perr.Error()}
		}
		ds.Rows = append(ds.Rows, row)
	}

	if len(ds.Rows) == 0 {
		return Dataset{}, ErrDatasetInvalid{Dataset: id, Reason: "no rows"}
	}

	// Sorted by time, then instrument, so the step order is total and does not
	// depend on how the file happened to be written.
	sort.SliceStable(ds.Rows, func(i, j int) bool {
		if !ds.Rows[i].Timestamp.Equal(ds.Rows[j].Timestamp) {
			return ds.Rows[i].Timestamp.Before(ds.Rows[j].Timestamp)
		}
		return ds.Rows[i].InstrumentID < ds.Rows[j].InstrumentID
	})

	if err := validate(id, ds.Rows); err != nil {
		return Dataset{}, err
	}
	ds.Hash = hashRows(ds.Rows)
	return ds, nil
}

func parseRow(r []string) (Row, error) {
	dec := func(i int, name string, required bool) (decimal.Decimal, error) {
		raw := strings.TrimSpace(r[i])
		if raw == "" {
			if required {
				return decimal.Zero, fmt.Errorf("%s is required", name)
			}
			return decimal.Zero, nil
		}
		v, err := decimal.NewFromString(raw)
		if err != nil {
			return decimal.Zero, fmt.Errorf("%s %q is not a number", name, raw)
		}
		return v, nil
	}

	row := Row{
		InstrumentID: strings.TrimSpace(r[0]),
		Timeframe:    domain.Timeframe(strings.TrimSpace(r[1])),
		Session:      strings.TrimSpace(r[11]),
		Source:       strings.TrimSpace(r[12]),
	}
	if row.InstrumentID == "" {
		return Row{}, fmt.Errorf("instrument_id is required")
	}
	if _, err := row.Timeframe.Duration(); err != nil {
		return Row{}, fmt.Errorf("timeframe %q is not recognised", row.Timeframe)
	}

	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(r[2]))
	if err != nil {
		return Row{}, fmt.Errorf("timestamp %q is not RFC3339", r[2])
	}
	// Normalised to UTC on the way in. A fixture written in a local zone would
	// otherwise put bars in the wrong session, and the failure would look like
	// a strategy problem.
	row.Timestamp = ts.UTC()

	var perr error
	if row.Open, perr = dec(3, "open", true); perr != nil {
		return Row{}, perr
	}
	if row.High, perr = dec(4, "high", true); perr != nil {
		return Row{}, perr
	}
	if row.Low, perr = dec(5, "low", true); perr != nil {
		return Row{}, perr
	}
	if row.Close, perr = dec(6, "close", true); perr != nil {
		return Row{}, perr
	}
	if row.Volume, perr = dec(7, "volume", false); perr != nil {
		return Row{}, perr
	}
	if row.Bid, perr = dec(8, "bid", false); perr != nil {
		return Row{}, perr
	}
	if row.Ask, perr = dec(9, "ask", false); perr != nil {
		return Row{}, perr
	}
	if row.SpreadFraction, perr = dec(10, "spread_fraction", false); perr != nil {
		return Row{}, perr
	}
	return row, nil
}

// validate refuses a dataset that would produce a misleading run.
//
// Every check here corresponds to something that has actually gone wrong
// somewhere in this repository: an inconsistent candle makes every indicator
// over it meaningless, a crossed book is rejected by the data-quality policy
// and would surface as an unexplained refusal, and duplicate timestamps make
// the step order ambiguous.
func validate(id string, rows []Row) error {
	type key struct {
		instrument string
		timeframe  domain.Timeframe
	}
	last := map[key]time.Time{}

	for i, r := range rows {
		line := i + 2 // header plus one-based
		if !r.Low.IsPositive() {
			return ErrDatasetInvalid{id, line, fmt.Sprintf("low %s is not positive", r.Low)}
		}
		if r.High.LessThan(r.Low) {
			return ErrDatasetInvalid{id, line,
				fmt.Sprintf("high %s is below low %s", r.High, r.Low)}
		}
		if r.High.LessThan(r.Open) || r.High.LessThan(r.Close) {
			return ErrDatasetInvalid{id, line,
				fmt.Sprintf("high %s is below open %s or close %s", r.High, r.Open, r.Close)}
		}
		if r.Low.GreaterThan(r.Open) || r.Low.GreaterThan(r.Close) {
			return ErrDatasetInvalid{id, line,
				fmt.Sprintf("low %s is above open %s or close %s", r.Low, r.Open, r.Close)}
		}
		if r.Volume.IsNegative() {
			return ErrDatasetInvalid{id, line, "volume is negative"}
		}
		// A partially specified book is worse than none: it looks deliberate.
		if r.Bid.IsPositive() != r.Ask.IsPositive() {
			return ErrDatasetInvalid{id, line,
				"bid and ask must both be present or both be absent"}
		}
		if r.Bid.IsPositive() && r.Ask.LessThan(r.Bid) {
			return ErrDatasetInvalid{id, line,
				fmt.Sprintf("crossed book: bid %s above ask %s", r.Bid, r.Ask)}
		}
		if !r.Bid.IsPositive() && !r.SpreadFraction.IsPositive() {
			return ErrDatasetInvalid{id, line,
				"a row with no bid/ask must carry a positive spread_fraction so the " +
					"quote can be derived visibly rather than guessed"}
		}
		if r.SpreadFraction.IsNegative() {
			return ErrDatasetInvalid{id, line, "spread_fraction is negative"}
		}

		k := key{r.InstrumentID, r.Timeframe}
		if prev, seen := last[k]; seen {
			if !r.Timestamp.After(prev) {
				return ErrDatasetInvalid{id, line, fmt.Sprintf(
					"timestamp %s does not advance for %s %s (previous %s)",
					r.Timestamp.Format(time.RFC3339), r.InstrumentID, r.Timeframe,
					prev.Format(time.RFC3339))}
			}
		}
		last[k] = r.Timestamp
	}
	return nil
}

// hashRows produces a stable digest of the parsed data.
//
// Hashing the PARSED rows rather than the file bytes on purpose: a comment, a
// line ending or a reordered file should not change a dataset's identity, and
// a changed price must.
func hashRows(rows []Row) string {
	h := sha256.New()
	for _, r := range rows {
		fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n",
			r.InstrumentID, r.Timeframe, r.Timestamp.UTC().Format(time.RFC3339Nano),
			r.Open.String(), r.High.String(), r.Low.String(), r.Close.String(),
			r.Volume.String(), r.Bid.String(), r.Ask.String(), r.SpreadFraction.String())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Instruments lists the instruments the dataset covers, sorted.
func (d Dataset) Instruments() []string {
	seen := map[string]bool{}
	for _, r := range d.Rows {
		seen[r.InstrumentID] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Span reports the first and last timestamp.
func (d Dataset) Span() (from, to time.Time) {
	if len(d.Rows) == 0 {
		return time.Time{}, time.Time{}
	}
	return d.Rows[0].Timestamp, d.Rows[len(d.Rows)-1].Timestamp
}

// Bars converts the dataset's rows for one instrument into domain bars.
func (d Dataset) Bars(instrumentID string) []domain.Bar {
	var out []domain.Bar
	for _, r := range d.Rows {
		if r.InstrumentID != instrumentID {
			continue
		}
		dur, err := r.Timeframe.Duration()
		if err != nil {
			continue
		}
		out = append(out, domain.Bar{
			InstrumentID: r.InstrumentID,
			Timeframe:    r.Timeframe,
			OpenTime:     r.Timestamp,
			CloseTime:    r.Timestamp.Add(dur),
			Open:         r.Open, High: r.High, Low: r.Low, Close: r.Close,
			Volume:   r.Volume,
			Complete: true,
			Provider: "replay",
		})
	}
	return out
}

// Registry is the allowlist of datasets a caller may name.
//
// An explicit map rather than a directory listing: a replay control endpoint
// accepts a dataset ID from an authenticated admin, and the set of things that
// ID can resolve to must be fixed at build time. Directory traversal is not a
// risk worth managing when it can be designed out.
type Registry struct {
	datasets map[string]Dataset
	order    []string
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{datasets: map[string]Dataset{}}
}

// Add registers a dataset.
func (r *Registry) Add(d Dataset) error {
	if d.ID == "" {
		return fmt.Errorf("replay: a dataset needs an id")
	}
	if _, exists := r.datasets[d.ID]; exists {
		return fmt.Errorf("replay: dataset %q is already registered", d.ID)
	}
	r.datasets[d.ID] = d
	r.order = append(r.order, d.ID)
	sort.Strings(r.order)
	return nil
}

// Get resolves a dataset ID.
//
// The error deliberately does not distinguish "no such dataset" from anything
// else, and does not echo the requested id back into a filesystem operation.
func (r *Registry) Get(id string) (Dataset, bool) {
	d, ok := r.datasets[id]
	return d, ok
}

// IDs lists the registered datasets in a stable order.
func (r *Registry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// List returns every registered dataset.
func (r *Registry) List() []Dataset {
	out := make([]Dataset, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.datasets[id])
	}
	return out
}

// LoadRegistry parses every declared fixture from a filesystem.
//
// The declarations are code, not a directory scan, so a stray file dropped
// into the fixtures directory does not become a runnable dataset.
func LoadRegistry(fsys fs.FS, dir string, declared []Declaration) (*Registry, error) {
	reg := NewRegistry()
	for _, d := range declared {
		ds, err := LoadDataset(fsys, d.ID, d.Description, path.Join(dir, d.File))
		if err != nil {
			return nil, err
		}
		if err := reg.Add(ds); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// Declaration names one fixture that may be loaded.
type Declaration struct {
	ID          string
	File        string
	Description string
}

// FormatRow renders a row as a dataset line, for generating fixtures.
func FormatRow(r Row) []string {
	return []string{
		r.InstrumentID, string(r.Timeframe), r.Timestamp.UTC().Format(time.RFC3339),
		r.Open.String(), r.High.String(), r.Low.String(), r.Close.String(),
		r.Volume.String(), decimalOrEmpty(r.Bid), decimalOrEmpty(r.Ask),
		decimalOrEmpty(r.SpreadFraction), r.Session, r.Source,
	}
}

// Header returns the dataset column order, for generating fixtures.
func Header() []string {
	out := make([]string, len(header))
	copy(out, header)
	return out
}

func decimalOrEmpty(d decimal.Decimal) string {
	if d.IsZero() {
		return ""
	}
	return d.String()
}

// ParseSpeed converts a speed name into a multiplier.
//
// Speed changes PACING ONLY. It must never touch a price, a timestamp, a size
// or an accounting decision: a run at 100x has to produce byte-identical
// financial output to the same run at 1x, or the fast mode is a different
// system and the slow mode's results do not transfer.
func ParseSpeed(name string) (float64, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "1x", "1":
		return 1, nil
	case "10x", "10":
		return 10, nil
	case "100x", "100":
		return 100, nil
	case "max":
		return 0, nil // zero means "no delay at all"
	}
	if v, err := strconv.ParseFloat(strings.TrimSuffix(name, "x"), 64); err == nil && v > 0 {
		return v, nil
	}
	return 0, fmt.Errorf("replay: speed %q is not one of 1x, 10x, 100x, max", name)
}
