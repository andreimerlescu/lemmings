package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const retainedDurations = 4096

// latencyHistogram has fixed memory and an upper-bound bucket error of 5% above
// 1 microsecond. Small datasets retain exact percentiles in the reporter.
type latencyHistogram struct {
	Bins     [512]int64
	Count    int64
	Min, Max time.Duration
}

func (h *latencyHistogram) add(d time.Duration) {
	if d < 0 {
		d = 0
	}
	if h.Count == 0 || d < h.Min {
		h.Min = d
	}
	if d > h.Max {
		h.Max = d
	}
	h.Count++
	i := 0
	if d > time.Microsecond {
		i = int(math.Ceil(math.Log(float64(d)/float64(time.Microsecond)) / math.Log(1.05)))
	}
	if i >= len(h.Bins) {
		i = len(h.Bins) - 1
	}
	h.Bins[i]++
}
func (h *latencyHistogram) quantile(p float64) time.Duration {
	if h.Count == 0 {
		return 0
	}
	rank := int64(math.Ceil(p / 100 * float64(h.Count)))
	var n int64
	for i, c := range h.Bins {
		n += c
		if n >= rank {
			d := time.Duration(math.Ceil(float64(time.Microsecond) * math.Pow(1.05, float64(i))))
			if d > h.Max {
				return h.Max
			}
			return d
		}
	}
	return h.Max
}

type VisitEvidence struct {
	RetryAfter       time.Duration     `json:"retry_after_ns"`
	SessionID        string            `json:"session_id"`
	Sequence         int               `json:"sequence"`
	Step             string            `json:"step,omitempty"`
	URL              string            `json:"url"`
	FinalURL         string            `json:"final_url"`
	Referrer         string            `json:"referrer,omitempty"`
	Status           int               `json:"status"`
	Bytes            int64             `json:"decoded_body_bytes"`
	DurationMS       float64           `json:"duration_ms"`
	Timing           RequestTiming     `json:"timing"`
	Timestamp        time.Time         `json:"timestamp"`
	Failed           bool              `json:"failed"`
	Cancelled        bool              `json:"cancelled"`
	ErrorKind        string            `json:"error_kind,omitempty"`
	ChecksumMatched  bool              `json:"checksum_matched"`
	CookieNames      []string          `json:"cookie_names,omitempty"`
	Responses        []ResponseHop     `json:"responses,omitempty"`
	ResponseCodes    map[int]int64     `json:"response_codes"`
	OmittedResponses int64             `json:"omitted_responses"`
	Page             PageEvidence      `json:"page"`
	Queue            WaitingRoomMetric `json:"queue"`
}

func evidence(v Visit) VisitEvidence {
	return VisitEvidence{RetryAfter: v.RetryAfter, SessionID: v.LemmingID, Sequence: v.Sequence, Step: v.Step, URL: safeURL(v.URL), FinalURL: safeURL(v.FinalURL), Referrer: safeURL(v.Referrer), Status: v.StatusCode, Bytes: v.BytesIn, DurationMS: float64(v.Duration) / float64(time.Millisecond), Timing: v.Timing, Timestamp: v.Timestamp, Failed: v.failed(), Cancelled: v.Cancelled, ErrorKind: v.ErrorKind, ChecksumMatched: v.Match, CookieNames: v.CookieNames, Responses: v.Responses, ResponseCodes: v.ResponseCodes, OmittedResponses: v.OmittedResponses, Page: v.Page, Queue: v.WaitingRoom}
}

type SessionEvidence struct {
	ID         string          `json:"id"`
	UserAgent  string          `json:"user_agent"`
	Language   string          `json:"language"`
	Terrain    int             `json:"terrain"`
	Pack       int             `json:"pack"`
	Started    time.Time       `json:"started"`
	DurationMS float64         `json:"duration_ms"`
	Visits     int64           `json:"visits"`
	Failed     int64           `json:"failed_visits"`
	Cancelled  int64           `json:"cancelled_visits"`
	ExitReason string          `json:"exit_reason"`
	Omitted    int64           `json:"omitted_visits"`
	Tail       []VisitEvidence `json:"visit_tail"`
}
type ExperienceReport struct {
	SessionsStarted    int64             `json:"sessions_started"`
	SessionsNotStarted int64             `json:"sessions_not_started"`
	Sessions           int64             `json:"sessions_observed"`
	SessionsFailed     int64             `json:"sessions_with_failures"`
	Visits             int64             `json:"visits"`
	Failed             int64             `json:"failed_visits"`
	Cancelled          int64             `json:"cancelled_visits"`
	FailureRate        float64           `json:"failure_rate"`
	Throughput         float64           `json:"visits_per_second"`
	StatusCodes        map[int]int64     `json:"final_status_codes"`
	ResponseCodes      map[int]int64     `json:"http_response_codes_including_redirects_and_queue_polls"`
	ErrorKinds         map[string]int64  `json:"errors_by_kind"`
	CheckFailures      map[string]int64  `json:"failed_checks"`
	ChecksumChanges    int64             `json:"checksum_changes"`
	HTMLPages          int64             `json:"html_pages"`
	P95TTFB            time.Duration     `json:"p95_final_hop_ttfb_ns"`
	PercentileMethod   string            `json:"percentile_method"`
	RetainedSessions   []SessionEvidence `json:"session_samples"`
	DroppedLifeLogs    int64             `json:"dropped_lifelogs"`
	TraceDropped       int64             `json:"trace_dropped"`
	TraceError         string            `json:"trace_error,omitempty"`
	GateFailures       []string          `json:"gate_failures"`
	Browser            *BrowserReport    `json:"browser,omitempty"`
}

// traceWriter moves disk I/O off the request path. Every omitted record and write
// failure is reported; aggregate counters do not depend on this optional stream.
type traceWriter struct {
	queue   chan VisitEvidence
	done    chan struct{}
	dropped atomic.Int64
	mu      sync.Mutex
	err     error
	once    sync.Once
}

func newTraceWriter(path string) (*traceWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	t := &traceWriter{queue: make(chan VisitEvidence, 4096), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		w := bufio.NewWriterSize(f, 64<<10)
		encoder := json.NewEncoder(w)
		for v := range t.queue {
			if err := encoder.Encode(v); err != nil {
				t.setError(err)
			}
		}
		if err := w.Flush(); err != nil {
			t.setError(err)
		}
		if err := f.Close(); err != nil {
			t.setError(err)
		}
	}()
	return t, nil
}
func (t *traceWriter) setError(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
}
func (t *traceWriter) record(v Visit) {
	select {
	case t.queue <- evidence(v):
	default:
		t.dropped.Add(1)
	}
}
func (t *traceWriter) close() { t.once.Do(func() { close(t.queue); <-t.done }) }
func (t *traceWriter) failure() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return fmt.Sprint(t.err)
	}
	return ""
}
