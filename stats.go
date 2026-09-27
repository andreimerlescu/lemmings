package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ── Latency histogram ────────────────────────────────────────────────────────

const (
	// histogramBase is the upper bound of the first bucket.
	histogramBase = 10 * time.Microsecond

	// histogramGrowth is the ratio between consecutive bucket bounds. A
	// quantile read from the histogram is at most 2% above the true value.
	histogramGrowth = 1.02

	// histogramBins covers 10µs to roughly 1h45m; slower samples land in
	// the last bucket, whose reported bound is clamped to the true max.
	histogramBins = 1024
)

// histogramLogGrowth caches ln(histogramGrowth).
var histogramLogGrowth = math.Log(histogramGrowth)

// histogram is a fixed-memory log-bucketed latency histogram (4 KiB).
// It is not safe for concurrent use; callers hold their own lock.
type histogram struct {
	bins     [histogramBins]uint32
	count    int64
	sum      time.Duration
	min, max time.Duration
}

// bucketFor returns the bin index for d.
func bucketFor(d time.Duration) int {
	if d <= histogramBase {
		return 0
	}
	i := int(math.Ceil(math.Log(float64(d)/float64(histogramBase)) / histogramLogGrowth))
	return min(i, histogramBins-1)
}

// bucketBound returns the upper bound of bin i.
func bucketBound(i int) time.Duration {
	return time.Duration(math.Ceil(float64(histogramBase) * math.Pow(histogramGrowth, float64(i))))
}

// add records one sample. Negative samples are recorded as zero.
func (h *histogram) add(d time.Duration) {
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

// merge folds other into h.
func (h *histogram) merge(other *histogram) {
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

// quantile returns the nearest-rank p-th percentile (0..100), reported as
// the upper bound of its bucket and clamped to [min, max].
func (h *histogram) quantile(p float64) time.Duration {
	if h.count == 0 {
		return 0
	}
	rank := max(int64(math.Ceil(p/100*float64(h.count))), 1)
	var seen int64
	for i, c := range h.bins {
		seen += int64(c)
		if seen >= rank {
			return min(max(bucketBound(i), h.min), h.max)
		}
	}
	return h.max
}

// mean returns the average sample, or 0 when empty.
func (h *histogram) mean() time.Duration {
	if h.count == 0 {
		return 0
	}
	return h.sum / time.Duration(h.count)
}

// ── Latency series: exact while small, histogram when large ──────────────────

// latencySeries tracks one distribution. While it holds at most exactCap
// samples every percentile is exact (nearest rank). Beyond that it answers
// from the histogram, within histogramGrowth of the true value. Memory is
// bounded either way.
type latencySeries struct {
	durations []time.Duration // every sample while count <= exactCap
	exactCap  int
	sorted    bool
	hist      histogram
}

// add records one sample.
func (s *latencySeries) add(d time.Duration) {
	s.hist.add(d)
	if len(s.durations) < s.exactCap {
		s.durations = append(s.durations, d)
		s.sorted = false
	}
}

// isExact reports whether percentiles are exact.
func (s *latencySeries) isExact() bool {
	return s.hist.count <= int64(s.exactCap)
}

// quantile returns the p-th percentile (0..100).
func (s *latencySeries) quantile(p float64) time.Duration {
	if !s.isExact() {
		return s.hist.quantile(p)
	}
	if !s.sorted {
		sort.Slice(s.durations, func(i, j int) bool { return s.durations[i] < s.durations[j] })
		s.sorted = true
	}
	return percentile(s.durations, int(p))
}

// ── Timeline ─────────────────────────────────────────────────────────────────

const (
	// maxTimelineBuckets bounds the timeline. When a run outgrows it,
	// adjacent buckets merge and the bucket width doubles, so any run
	// length fits in constant memory.
	maxTimelineBuckets = 600

	// timelineStartWidth is the initial bucket width.
	timelineStartWidth = time.Second
)

// timeBucket aggregates everything that finished within one bucket.
type timeBucket struct {
	Visits    int64
	Failed    int64
	Cancelled int64
	Status    [6]int64 // index = status/100; 0 means no response
	Bytes     int64
	Born      int64
	Died      int64
	Latency   histogram
}

// timeline is a bounded time series of visit outcomes and lemming births
// and deaths, relative to the start of the run.
type timeline struct {
	width   time.Duration
	buckets []timeBucket
}

// newTimeline returns an empty timeline.
func newTimeline() *timeline {
	return &timeline{width: timelineStartWidth}
}

// bucket returns the bucket covering offset, growing or compacting the
// timeline as needed. Negative offsets land in the first bucket.
func (t *timeline) bucket(offset time.Duration) *timeBucket {
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

// compact merges adjacent bucket pairs and doubles the width.
func (t *timeline) compact() {
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
		dst.Latency.merge(&src.Latency)
	}
	t.buckets = merged
	t.width *= 2
}

// TimelinePoint is one bucket of the timeline as reported.
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
	Alive         int64   `json:"alive"` // lemmings alive at the bucket's end
	RatePerSecond float64 `json:"rps"`
	P50Millis     float64 `json:"p50_ms"`
	P95Millis     float64 `json:"p95_ms"`
}

// points renders the timeline, starting from zero alive lemmings. Only
// the last n points are returned when n > 0.
func (t *timeline) points(n int) []TimelinePoint {
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
			P50Millis:     millis(b.Latency.quantile(50)),
			P95Millis:     millis(b.Latency.quantile(95)),
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// millis converts a duration to fractional milliseconds.
func millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// statusIndex maps a status code to its class index, 0 for no response.
func statusIndex(code int) int {
	if code < 100 || code > 599 {
		return 0
	}
	return code / 100
}

// ── Trace writer ─────────────────────────────────────────────────────────────

// traceQueueSize bounds visits waiting to be written to -trace-file.
const traceQueueSize = 8192

// VisitTrace is one line of the -trace-file JSONL stream. URLs are passed
// through safeURL; cookie values, headers and bodies are never written.
type VisitTrace struct {
	Lemming     string        `json:"lemming"`
	Sequence    int           `json:"seq"`
	Step        string        `json:"step,omitempty"`
	URL         string        `json:"url"`
	FinalURL    string        `json:"final_url,omitempty"`
	Referrer    string        `json:"referrer,omitempty"`
	Status      int           `json:"status"`
	Bytes       int64         `json:"bytes"`
	DurationMS  float64       `json:"duration_ms"`
	LatencyMS   float64       `json:"latency_ms"`
	Timing      RequestTiming `json:"timing"`
	Title       string        `json:"title,omitempty"`
	Failed      bool          `json:"failed"`
	Cancelled   bool          `json:"cancelled"`
	ErrorKind   string        `json:"error_kind,omitempty"`
	FailedCheck []string      `json:"failed_checks,omitempty"`
	QueuedMS    float64       `json:"queued_ms,omitempty"`
	Hops        []ResponseHop `json:"hops,omitempty"`
	Cookies     []string      `json:"cookie_names,omitempty"`
	At          time.Time     `json:"at"`
}

// traceOf converts a visit into its trace line.
func traceOf(v *Visit) VisitTrace {
	t := VisitTrace{
		Lemming:    v.LemmingID,
		Sequence:   v.Sequence,
		Step:       v.Step,
		URL:        safeURL(v.URL),
		FinalURL:   safeURL(v.FinalURL),
		Referrer:   safeURL(v.Referrer),
		Status:     v.StatusCode,
		Bytes:      v.BytesIn,
		DurationMS: millis(v.Duration),
		LatencyMS:  millis(v.latency()),
		Timing:     v.Timing,
		Title:      v.Page.Title,
		Failed:     v.Failed,
		Cancelled:  v.Cancelled,
		ErrorKind:  v.ErrorKind,
		QueuedMS:   millis(v.WaitingRoom.Duration),
		Hops:       v.Hops,
		Cookies:    v.CookieNames,
		At:         v.Timestamp,
	}
	for _, c := range v.Page.Checks {
		if !c.Passed {
			t.FailedCheck = append(t.FailedCheck, c.Name)
		}
	}
	return t
}

// traceWriter streams visits to a JSONL file on its own goroutine, keeping
// disk I/O off the lemmings' path. When the queue is full a record is
// dropped and counted rather than slowing the swarm; the report shows the
// dropped count so an incomplete trace is never mistaken for a full one.
type traceWriter struct {
	queue   chan VisitTrace
	done    chan struct{}
	dropped atomic.Int64
	written atomic.Int64
	once    sync.Once
	mu      sync.Mutex
	err     error
}

// newTraceWriter creates path and starts the writer. It refuses to
// overwrite an existing file so earlier evidence is never destroyed.
func newTraceWriter(path string) (*traceWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	t := &traceWriter{
		queue: make(chan VisitTrace, traceQueueSize),
		done:  make(chan struct{}),
	}
	go t.run(f)
	return t, nil
}

// run writes queued records until the queue is closed.
func (t *traceWriter) run(f *os.File) {
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

// record queues one visit without blocking.
func (t *traceWriter) record(v *Visit) {
	select {
	case t.queue <- traceOf(v):
	default:
		t.dropped.Add(1)
	}
}

// close flushes and closes the file. Safe to call more than once.
func (t *traceWriter) close() {
	t.once.Do(func() {
		close(t.queue)
		<-t.done
	})
}

// setErr keeps the first write error.
func (t *traceWriter) setErr(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		t.err = err
	}
}

// failure returns the first write error as text, or "".
func (t *traceWriter) failure() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err == nil {
		return ""
	}
	return fmt.Sprint(t.err)
}
