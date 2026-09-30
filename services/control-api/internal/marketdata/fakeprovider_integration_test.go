package marketdata

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A stand-in for Twelve Data that speaks the protocol as DOCUMENTED.
//
// # Why a fake and not a mock
//
// The acquisition path -- Backfill, Sync, Repair, Snapshot -- had no test that
// called it. Every existing test exercised a pure helper: chunking, bar
// validation, hashing. The orchestration that would actually run against a
// provider had never been executed, so the first evidence it worked would have
// been the production run itself.
//
// A mock returning a canned slice would not have helped, because the things
// most likely to be wrong are the ones a mock papers over: that the client
// asks for the window it means to, that it copes with the provider's DEFAULT
// ordering rather than the one it requested, that it pages a range larger than
// one response can carry, and that it recognises an error delivered with HTTP
// 200. This server does all of those, taken from the published documentation:
//
//   - values are returned NEWEST FIRST unless order=ASC is requested, which is
//     the documented default and the trap a caller that assumed ascending
//     would fall into
//   - at most `outputsize` rows come back, capped at 5000
//   - errors arrive as {"code":..,"message":..,"status":"error"} with HTTP 200
//   - datetime is "YYYY-MM-DD HH:MM:SS"
//   - start_date/end_date are parsed in the documented REQUEST format
//
// It is NOT a claim that the real API behaves this way in every respect. It is
// the documented contract, written down and executed, which is a great deal
// more than an untested assumption. See the report for what remains unverified.
type fakeTwelveData struct {
	server *httptest.Server
	URL    string

	mu sync.Mutex
	// bars is the provider's whole "market", keyed by open time.
	bars map[time.Time]fakeBar
	// requests records every query received, so a test can assert what was
	// actually asked for rather than what the caller believed it asked for.
	requests []url.Values
	// failNext makes the next n requests fail in a named way.
	failNext   int
	failMode   string
	lastAPIKey string
}

type fakeBar struct {
	open, high, low, close float64
	volume                 int
}

const fakeDatetimeLayout = "2006-01-02 15:04:05"

// The REQUEST format the documentation specifies. A client sending anything
// else is rejected here, which is how the date-format fix stays fixed.
const fakeRequestDateLayout = "2006-01-02T15:04:05"

func newFakeTwelveData() *fakeTwelveData {
	f := &fakeTwelveData{bars: map[time.Time]fakeBar{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/time_series", f.handleTimeSeries)
	srv := httptest.NewServer(mux)
	f.server = srv
	f.URL = srv.URL
	return f
}

// Close shuts the server down.
//
// It used to keep only srv.Config and do nothing here, so the t.Cleanup that
// calls it was a no-op and all sixteen tests leaked a listener and its
// goroutines for the life of the test binary.
func (f *fakeTwelveData) Close() { f.server.Close() }

// Seed fills the provider's market with a deterministic hourly series.
func (f *fakeTwelveData) Seed(from time.Time, hours int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	price := 2000.0
	for i := 0; i < hours; i++ {
		t := from.UTC().Add(time.Duration(i) * time.Hour)
		// A gentle deterministic wave: coherent OHLC, never flat, never
		// impossible. The point is a series the pipeline accepts, not a
		// market simulation.
		drift := math.Sin(float64(i)/12.0) * 3.0
		open := price
		closePx := price + drift
		f.bars[t] = fakeBar{
			open:  open,
			high:  math.Max(open, closePx) + 0.7,
			low:   math.Min(open, closePx) - 0.7,
			close: closePx,
			// Volume varies so a constant-volume bug would show.
			volume: 100 + i%37,
		}
		price = closePx
	}
}

// Remove deletes a run of bars, so a test can create a gap the provider can
// later fill.
func (f *fakeTwelveData) Remove(from time.Time, hours int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < hours; i++ {
		delete(f.bars, from.UTC().Add(time.Duration(i)*time.Hour))
	}
}

// FailNext makes the next n requests fail. mode is "rate_limit", "server",
// "error_body" or "bad_json".
func (f *fakeTwelveData) FailNext(n int, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext, f.failMode = n, mode
}

func (f *fakeTwelveData) Requests() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]url.Values, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeTwelveData) LastAPIKey() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAPIKey
}

func (f *fakeTwelveData) handleTimeSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f.mu.Lock()
	f.requests = append(f.requests, q)
	f.lastAPIKey = q.Get("apikey")
	fail, mode := f.failNext, f.failMode
	if fail > 0 {
		f.failNext--
	}
	f.mu.Unlock()

	if fail > 0 {
		switch mode {
		case "rate_limit":
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case "server":
			w.WriteHeader(http.StatusInternalServerError)
			return
		case "error_body":
			// HTTP 200 with an error in the body, which is how this API
			// actually reports most failures.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":400,"message":"**symbol** not found","status":"error"}`)
			return
		case "bad_json":
			fmt.Fprint(w, `{"values":[ truncated`)
			return
		}
	}

	if q.Get("apikey") == "" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":401,"message":"apikey missing","status":"error"}`)
		return
	}

	// The documented request date format. Anything else is refused: a
	// provider that silently reinterprets a date returns a window that is not
	// the one that was asked for, and the response still looks like a success.
	start, serr := time.ParseInLocation(fakeRequestDateLayout, q.Get("start_date"), time.UTC)
	end, eerr := time.ParseInLocation(fakeRequestDateLayout, q.Get("end_date"), time.UTC)
	if serr != nil || eerr != nil {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":400,"message":"start_date/end_date must be %s","status":"error"}`,
			fakeRequestDateLayout)
		return
	}

	outputsize := 30 // the documented default
	if raw := q.Get("outputsize"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			outputsize = n
		}
	}
	if outputsize > 5000 {
		outputsize = 5000
	}

	f.mu.Lock()
	times := make([]time.Time, 0, len(f.bars))
	for t := range f.bars {
		if !t.Before(start) && t.Before(end) {
			times = append(times, t)
		}
	}
	f.mu.Unlock()

	// Newest first is the DOCUMENTED default. Ascending only on request.
	ascending := strings.EqualFold(q.Get("order"), "asc")
	sort.Slice(times, func(i, j int) bool {
		if ascending {
			return times[i].Before(times[j])
		}
		return times[i].After(times[j])
	})
	if len(times) > outputsize {
		times = times[:outputsize]
	}

	type value struct {
		Datetime string `json:"datetime"`
		Open     string `json:"open"`
		High     string `json:"high"`
		Low      string `json:"low"`
		Close    string `json:"close"`
		Volume   string `json:"volume"`
	}
	values := make([]value, 0, len(times))
	f.mu.Lock()
	for _, t := range times {
		b := f.bars[t]
		values = append(values, value{
			Datetime: t.UTC().Format(fakeDatetimeLayout),
			Open:     strconv.FormatFloat(b.open, 'f', 5, 64),
			High:     strconv.FormatFloat(b.high, 'f', 5, 64),
			Low:      strconv.FormatFloat(b.low, 'f', 5, 64),
			Close:    strconv.FormatFloat(b.close, 'f', 5, 64),
			Volume:   strconv.Itoa(b.volume),
		})
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"meta": map[string]string{
			"symbol": q.Get("symbol"), "interval": q.Get("interval"),
			"currency": "USD", "exchange_timezone": "UTC",
			"exchange": "PHYSICAL CURRENCY", "type": "Physical Currency",
		},
		"values": values,
		"status": "ok",
	})
}
