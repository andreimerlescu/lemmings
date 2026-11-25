package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

const (
	histogramBase      = 10 * time.Microsecond
	histogramGrowth    = 1.005
	histogramBins      = 4096
	maxTimelineBuckets = 600
	timelineStartWidth = time.Second
	traceQueueSize     = 8192
)

var histogramLogGrowth = math.Log(histogramGrowth)

// Histogram is a fixed-memory (32 KiB) log-bucketed latency histogram with
// <= 0.5% relative error across 10µs..>2h and overflow-proof uint64 counts.
type Histogram struct {
	bins     [histogramBins]uint64
	count    int64
	sum      time.Duration
	min, max time.Duration
}

func bucketFor(d time.Duration) int {
	if d <= histogramBase {
		return 0
	}
	i := int(math.Ceil(math.Log(float64(d)/float64(histogramBase)) / histogramLogGrowth))
	if i < 0 {
		return 0
	}
	return min(i, histogramBins-1)
}

func bucketBound(i int) time.Duration {
	if i <= 0 {
		return histogramBase
	}
	return time.Duration(math.Round(float64(histogramBase) * math.Pow(histogramGrowth, float64(i)-0.5)))
}

// Add records one latency sample.
func (h *Histogram) Add(d time.Duration) {
	d = max(d, 0)
	if h.count == 0 || d < h.min {
		h.min = d
	}
	if d > h.max {
		h.max = d
	}
	h.count++
	h.sum += d
	h.bins[bucketFor(d)]++
}

// Merge folds other into h.
func (h *Histogram) Merge(other *Histogram) {
	if other.count == 0 {
		return
	}
	if h.count == 0 || other.min < h.min {
		h.min = other.min
	}
	h.max = max(h.max, other.max)
	h.count += other.count
	h.sum += other.sum
	for i, c := range other.bins {
		h.bins[i] += c
	}
}

// Quantile returns the p-th percentile (0..100) within 0.5% of the exact value.
func (h *Histogram) Quantile(p float64) time.Duration {
	if h.count == 0 {
		return 0
	}
	if p <= 0 {
		return h.min
	}
	if p >= 100 {
		return h.max
	}
	rank := uint64(math.Ceil(p / 100 * float64(h.count)))
	if rank < 1 {
		rank = 1
	}
	var seen uint64
	for i, c := range h.bins {
		seen += c
		if seen >= rank {
			return min(max(bucketBound(i), h.min), h.max)
		}
	}
	return h.max
}

// Mean returns the arithmetic average, or 0 when empty.
func (h *Histogram) Mean() time.Duration {
	if h.count == 0 {
		return 0
	}
	return h.sum / time.Duration(h.count)
}

// LatencySeries answers exact percentiles while <= exactCap samples and falls
// back to Histogram beyond that, keeping memory strictly bounded.
type LatencySeries struct {
	durations []time.Duration
	exactCap  int
	sorted    bool
	hist      Histogram
}

// Add records one sample.
func (s *LatencySeries) Add(d time.Duration) {
	s.hist.Add(d)
	if len(s.durations) < s.exactCap {
		s.durations = append(s.durations, d)
		s.sorted = false
	}
}

// IsExact reports whether every sample was kept for exact nearest-rank percentiles.
func (s *LatencySeries) IsExact() bool {
	return s.hist.count <= int64(s.exactCap)
}

// Quantile returns the p-th percentile (0..100).
func (s *LatencySeries) Quantile(p float64) time.Duration {
	if !s.IsExact() {
		return s.hist.Quantile(p)
	}
	if !s.sorted {
		sort.Slice(s.durations, func(i, j int) bool { return s.durations[i] < s.durations[j] })
		s.sorted = true
	}
	return Percentile(s.durations, int(p))
}

type timeBucket struct {
	Visits    int64
	Failed    int64
	Cancelled int64
	Status    [6]int64
	Bytes     int64
	Born      int64
	Died      int64
	Latency   Histogram
}

// Timeline is an auto-compacting bounded time series.
type Timeline struct {
	width   time.Duration
	buckets []timeBucket
}

// NewTimeline returns an empty 1s-resolution Timeline.
func NewTimeline() *Timeline {
	return &Timeline{width: timelineStartWidth}
}

func (t *Timeline) bucket(offset time.Duration) *timeBucket {
	offset = max(offset, 0)
	idx := int(offset / t.width)
	for idx >= maxTimelineBuckets {
		t.compact()
		idx = int(offset / t.width)
	}
	for len(t.buckets) <= idx {
		t.buckets = append(t.buckets, timeBucket{})
	}
	return &t.buckets[idx]
}

func (t *Timeline) compact() {
	merged := make([]timeBucket, (len(t.buckets)+1)/2)
	for i := range t.buckets {
		dst, src := &merged[i/2], &t.buckets[i]
		dst.Visits += src.Visits
		dst.Failed += src.Failed
		dst.Cancelled += src.Cancelled
		dst.Bytes += src.Bytes
		dst.Born += src.Born
		dst.Died += src.Died
		for c := range src.Status {
			dst.Status[c] += src.Status[c]
		}
		dst.Latency.Merge(&src.Latency)
	}
	t.buckets = merged
	t.width *= 2
}

// TimelinePoint is one rendered bucket of the run timeline.
type TimelinePoint struct {
	OffsetSeconds float64 `json:"t"`
	Visits        int64   `json:"visits"`
	Failed        int64   `json:"failed"`
	Cancelled     int64   `json:"cancelled"`
	Status2xx     int64   `json:"s2xx"`
	Status3xx     int64   `json:"s3xx"`
	Status4xx     int64   `json:"s4xx"`
	Status5xx     int64   `json:"s5xx"`
	NoResponse    int64   `json:"s000"`
	Bytes         int64   `json:"bytes"`
	Alive         int64   `json:"alive"`
	RatePerSecond float64 `json:"rps"`
	P50Millis     float64 `json:"p50_ms"`
	P95Millis     float64 `json:"p95_ms"`
}

// Points renders the timeline (or the last n points when n > 0).
func (t *Timeline) Points(n int) []TimelinePoint {
	out := make([]TimelinePoint, len(t.buckets))
	var alive int64
	secs := t.width.Seconds()
	for i := range t.buckets {
		b := &t.buckets[i]
		alive += b.Born - b.Died
		out[i] = TimelinePoint{
			OffsetSeconds: float64(i) * secs,
			Visits:        b.Visits,
			Failed:        b.Failed,
			Cancelled:     b.Cancelled,
			NoResponse:    b.Status[0],
			Status2xx:     b.Status[2],
			Status3xx:     b.Status[3],
			Status4xx:     b.Status[4],
			Status5xx:     b.Status[5],
			Bytes:         b.Bytes,
			Alive:         max(alive, 0),
			RatePerSecond: float64(b.Visits) / secs,
			P50Millis:     Millis(b.Latency.Quantile(50)),
			P95Millis:     Millis(b.Latency.Quantile(95)),
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func statusIndex(code int) int {
	if code < 100 || code > 599 {
		return 0
	}
	return code / 100
}

// VisitTrace is one JSONL record written to -trace-file (URLs redacted, cookie names only).
type VisitTrace struct {
	Kind          string              `json:"kind,omitempty"`
	AssetClass    string              `json:"asset_class,omitempty"`
	Lemming       string              `json:"lemming"`
	LemmingNumber int64               `json:"lemming_number,omitempty"`
	Sequence      int                 `json:"seq"`
	Step          string              `json:"step,omitempty"`
	URL           string              `json:"url"`
	FinalURL      string              `json:"final_url,omitempty"`
	Referrer      string              `json:"referrer,omitempty"`
	Status        int                 `json:"status"`
	Bytes         int64               `json:"bytes"`
	DurationMS    float64             `json:"duration_ms"`
	LatencyMS     float64             `json:"latency_ms"`
	Timing        swarm.RequestTiming `json:"timing"`
	Title         string              `json:"title,omitempty"`
	Failed        bool                `json:"failed"`
	Cancelled     bool                `json:"cancelled"`
	ErrorKind     string              `json:"error_kind,omitempty"`
	FailedCheck   []string            `json:"failed_checks,omitempty"`
	QueuedMS      float64             `json:"queued_ms,omitempty"`
	QueuePolls    int                 `json:"queue_polls,omitempty"`
	QueueAdmitted bool                `json:"queue_admitted,omitempty"`
	ConcertAdmit  bool                `json:"concert_admit,omitempty"`
	RoomTicket    bool                `json:"room_ticket,omitempty"`
	Banned        bool                `json:"banned,omitempty"`
	Hops          []swarm.ResponseHop `json:"hops,omitempty"`
	Cookies       []string            `json:"cookie_names,omitempty"`
	At            time.Time           `json:"at"`
}

func traceOf(v *swarm.Visit) VisitTrace {
	t := VisitTrace{
		Kind:          "page",
		Lemming:       v.LemmingID,
		LemmingNumber: v.LemmingNumber,
		Sequence:      v.Sequence,
		Step:          v.Step,
		URL:           discovery.SafeURL(v.URL),
		FinalURL:      discovery.SafeURL(v.FinalURL),
		Referrer:      discovery.SafeURL(v.Referrer),
		Status:        v.StatusCode,
		Bytes:         v.BytesIn,
		DurationMS:    Millis(v.Duration),
		LatencyMS:     Millis(v.EffectiveLatency()),
		Timing:        v.Timing,
		Title:         v.Page.Title,
		Failed:        v.Failed,
		Cancelled:     v.Cancelled,
		ErrorKind:     v.ErrorKind,
		QueuedMS:      Millis(v.WaitingRoom.Duration),
		QueuePolls:    max(v.QueuePolls, v.WaitingRoom.Polls),
		QueueAdmitted: v.WaitingRoom.Admitted,
		ConcertAdmit:  v.ConcertAdmit,
		RoomTicket:    v.RoomTicket,
		Banned:        v.Banned,
		Hops:          v.Hops,
		Cookies:       v.CookieNames,
		At:            v.Timestamp,
	}
	for _, c := range v.Page.Checks {
		if !c.Passed {
			t.FailedCheck = append(t.FailedCheck, c.Name)
		}
	}
	return t
}

func traceOfAsset(a *swarm.AssetVisit, class string, admitPass bool) VisitTrace {
	at := a.Timestamp
	if at.IsZero() {
		at = a.FinishedAt
	}
	if at.IsZero() {
		at = time.Now()
	}
	return VisitTrace{
		Kind:          "asset",
		AssetClass:    class,
		Lemming:       a.LemmingID,
		LemmingNumber: a.LemmingNumber,
		URL:           discovery.SafeURL(a.URL),
		Referrer:      discovery.SafeURL(a.PageURL),
		Status:        a.StatusCode,
		Bytes:         a.BytesIn,
		DurationMS:    Millis(a.Duration),
		LatencyMS:     Millis(a.Duration),
		Failed:        a.Failed,
		ErrorKind:     a.ErrorKind,
		ConcertAdmit:  admitPass,
		Banned:        a.Denied403 || a.StatusCode == http.StatusForbidden,
		At:            at,
	}
}

func traceOfScout(e events.Event) VisitTrace {
	at := e.OccurredAt
	if at.IsZero() {
		at = time.Now()
	}
	var errKind string
	if e.Err != nil {
		errKind = swarm.ErrorKind(e.Err)
	}
	return VisitTrace{
		Kind:          "scout",
		Lemming:       e.LemmingID,
		LemmingNumber: e.LemmingNumber,
		URL:           discovery.SafeURL(e.URL),
		Status:        e.StatusCode,
		Bytes:         e.BytesIn,
		DurationMS:    Millis(e.Duration),
		LatencyMS:     Millis(e.Duration),
		Failed:        e.Failed,
		Cancelled:     e.Cancelled,
		ErrorKind:     errKind,
		ConcertAdmit:  e.AdmitPass,
		Banned:        e.Banned,
		At:            at,
	}
}

// TraceWriter streams visits asynchronously to a new 0600 JSONL file.
type TraceWriter struct {
	queue   chan VisitTrace
	done    chan struct{}
	dropped atomic.Int64
	written atomic.Int64
	once    sync.Once
	mu      sync.Mutex
	err     error
}

// NewTraceWriter creates path with O_EXCL and mode 0600, refusing to overwrite existing files.
func NewTraceWriter(path string) (*TraceWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	t := &TraceWriter{
		queue: make(chan VisitTrace, traceQueueSize),
		done:  make(chan struct{}),
	}
	go t.run(f)
	return t, nil
}

func (t *TraceWriter) run(f *os.File) {
	defer close(t.done)
	w := bufio.NewWriterSize(f, 64<<10)
	enc := json.NewEncoder(w)
	for rec := range t.queue {
		if err := enc.Encode(rec); err != nil {
			t.setErr(err)
			continue
		}
		t.written.Add(1)
	}
	if err := w.Flush(); err != nil {
		t.setErr(err)
	}
	if err := f.Close(); err != nil {
		t.setErr(err)
	}
}

// Record enqueues a page visit non-blockingly, counting drops if the queue is full.
func (t *TraceWriter) Record(v *swarm.Visit) {
	t.RecordTrace(traceOf(v))
}

// RecordTrace enqueues rec non-blockingly, counting drops if the queue is full.
func (t *TraceWriter) RecordTrace(rec VisitTrace) {
	select {
	case t.queue <- rec:
	default:
		t.dropped.Add(1)
	}
}

// Close flushes and closes the trace file and joins the writer goroutine. Idempotent.
func (t *TraceWriter) Close() {
	t.once.Do(func() {
		close(t.queue)
		<-t.done
	})
}

func (t *TraceWriter) setErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = err
	}
}

// Failure returns the first I/O error encountered, or "".
func (t *TraceWriter) Failure() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		return ""
	}
	return fmt.Sprint(t.err)
}
