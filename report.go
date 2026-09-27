package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

// reportDateFormat is the date component used in output filenames.
const reportDateFormat = "2006.01.02"

const (
	// maxReportPaths bounds distinct paths tracked; the rest share otherPath.
	maxReportPaths = 1000

	// otherPath collects visits beyond maxReportPaths distinct paths.
	otherPath = "[other paths]"

	// exactGlobalSamples and exactPathSamples are how many latency samples
	// are kept for exact percentiles, globally and per path. Beyond them
	// percentiles come from a histogram within 2% of the true value.
	exactGlobalSamples = 4096
	exactPathSamples   = 512

	// reportTailSteps bounds the steps shown for each notable lemming.
	reportTailSteps = 40
)

//go:embed templates/report.md.tmpl templates/report.html.tmpl
var reportTemplates embed.FS

// Reporter accumulates visits as they happen and lemming lives as they end,
// then renders the report when Write is called. All methods are safe for
// concurrent use — lemmings record visits from their own goroutines.
type Reporter struct {
	cfg       SwarmConfig
	mu        sync.Mutex
	byPath    map[string]*pathStats
	startedAt time.Time
	endedAt   time.Time
	targets   []ReportTarget // delivery targets, populated via AddTarget
	trace     *traceWriter   // optional -trace-file writer
	unsub     func()

	latency  latencySeries
	ttfb     histogram
	timeline *timeline

	visits, failed, cancelled  int64
	checksumChanges, htmlPages int64
	statusCodes                map[int]int64
	errorKinds                 map[string]int64
	failedChecks               map[string]int64

	roomHeld, roomDied int64
	roomWait           histogram
	roomMaxPosition    int

	sessions, flawless int64
	exitReasons        map[string]int64
	notable            notableSet

	started, notStarted, dropped int64 // from SwarmMetrics at markEnded
}

// pathStats holds aggregated metrics for a single URL path.
//
// The embedded latencySeries provides durations (exact samples while
// small) and a histogram for percentiles once the path is busy.
type pathStats struct {
	URL         string
	Hits        int64
	Bytes       int64
	xx2         int64
	xx3         int64
	xx4         int64
	xx5         int64
	waitingRoom int64 // lemmings that hit a waiting room on this path
	errors      int64 // transport errors, cancellations excluded
	failed      int64 // failed visits of any kind
	latencySeries
}

// NewReporter constructs a Reporter.
func NewReporter(cfg SwarmConfig) *Reporter {
	return &Reporter{
		cfg:          cfg,
		byPath:       make(map[string]*pathStats),
		startedAt:    time.Now(),
		latency:      latencySeries{exactCap: exactGlobalSamples},
		timeline:     newTimeline(),
		statusCodes:  make(map[int]int64),
		errorKinds:   make(map[string]int64),
		failedChecks: make(map[string]int64),
		exitReasons:  make(map[string]int64),
	}
}

// AddTarget registers a ReportTarget to receive the rendered report
// when Write is called. Multiple targets can be registered — all receive
// the same rendered content delivered concurrently.
//
// Usage:
//
//	r.AddTarget(&LocalTarget{basePath: "."})
//	r.AddTarget(s3target)
//	r.AddTarget(mailTarget)
//
// Warning: AddTarget is not safe for concurrent use. Call it during
// swarm setup before Run is invoked.
func (r *Reporter) AddTarget(t ReportTarget) {
	r.targets = append(r.targets, t)
}

// Attach subscribes the reporter to visit and lifecycle events so every
// visit is counted the moment it completes. Call it once, before Run.
func (r *Reporter) Attach(bus *EventBus) {
	r.unsub = bus.Subscribe(Filter(r.handleEvent,
		EventVisitComplete, EventVisitError, EventLemmingBorn, EventLemmingDied))
}

// handleEvent is the reporter's EventBus subscriber.
func (r *Reporter) handleEvent(e Event) {
	switch e.Kind {
	case EventVisitComplete, EventVisitError:
		if e.Visit != nil {
			r.RecordVisit(e.Visit)
		}
	case EventLemmingBorn, EventLemmingDied:
		r.mu.Lock()
		b := r.timeline.bucket(e.OccurredAt.Sub(r.startedAt))
		if e.Kind == EventLemmingBorn {
			b.Born++
		} else {
			b.Died++
		}
		r.mu.Unlock()
	}
}

// markStarted records when lemmings started moving.
func (r *Reporter) markStarted(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startedAt = at
}

// markEnded records the end of the run and the lemming counters that only
// the swarm knows.
func (r *Reporter) markEnded(at time.Time, m *SwarmMetrics) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endedAt = at
	r.started = m.LemmingsCompleted.Load()
	r.notStarted = m.LemmingsFailed.Load()
	r.dropped = m.DroppedLogs.Load()
}

// RecordVisit counts one finished visit. Called for every visit as it
// completes, so exact totals never depend on LifeLog delivery.
func (r *Reporter) RecordVisit(v *Visit) {
	r.mu.Lock()
	r.recordVisitLocked(v, time.Now())
	r.mu.Unlock()
	if r.trace != nil {
		r.trace.record(v)
	}
}

// recordVisitLocked folds a visit into every aggregate. r.mu must be held.
func (r *Reporter) recordVisitLocked(v *Visit, finished time.Time) {
	failed := failedVisit(v)

	// Query strings are dropped so /p?id=1 and /p?id=2 are one path, and so
	// tokens in query strings never reach a report.
	key := safeURL(v.URL)
	ps := r.byPath[key]
	if ps == nil {
		if len(r.byPath) >= maxReportPaths {
			key = otherPath
			ps = r.byPath[key]
		}
		if ps == nil {
			ps = &pathStats{URL: key, latencySeries: latencySeries{exactCap: exactPathSamples}}
			r.byPath[key] = ps
		}
	}
	ps.Hits++
	ps.Bytes += v.BytesIn
	if v.WaitingRoom.Detected {
		ps.waitingRoom++
	}
	if v.Error != nil && !v.Cancelled {
		ps.errors++
	}
	if failed {
		ps.failed++
	}
	switch {
	case v.StatusCode >= 200 && v.StatusCode < 300:
		ps.xx2++
	case v.StatusCode >= 300 && v.StatusCode < 400:
		ps.xx3++
	case v.StatusCode >= 400 && v.StatusCode < 500:
		ps.xx4++
	case v.StatusCode >= 500:
		ps.xx5++
	}

	r.visits++
	r.statusCodes[v.StatusCode]++
	bucket := r.timeline.bucket(finished.Sub(r.startedAt))
	bucket.Visits++
	bucket.Bytes += v.BytesIn
	bucket.Status[statusIndex(v.StatusCode)]++

	switch {
	case v.Cancelled:
		r.cancelled++
		bucket.Cancelled++
	default:
		// Cancelled visits were cut short by the lemming's own deadline;
		// their durations say nothing about the server, so only completed
		// visits feed latency.
		ps.add(v.latency())
		r.latency.add(v.latency())
		bucket.Latency.add(v.latency())
		if v.Timing.TTFB > 0 {
			r.ttfb.add(v.Timing.TTFB)
		}
		if failed {
			r.failed++
			bucket.Failed++
		}
	}

	if v.ErrorKind != "" {
		r.errorKinds[v.ErrorKind]++
	} else if v.Error != nil && !v.Cancelled {
		r.errorKinds[errorKind(v.Error)]++
	}
	for _, c := range v.Page.Checks {
		if !c.Passed {
			r.failedChecks[c.Name]++
		}
	}
	if v.Expected != "" && !v.Match && v.Error == nil {
		r.checksumChanges++
	}
	if v.Page.HTML {
		r.htmlPages++
	}
	if v.WaitingRoom.Detected {
		r.roomHeld++
		r.roomWait.add(v.WaitingRoom.Duration)
		r.roomMaxPosition = max(r.roomMaxPosition, v.WaitingRoom.Position)
		if v.Error != nil || v.Cancelled {
			r.roomDied++
		}
	}
}

// failedVisit reports whether a visit failed. A visit recorded by a
// running lemming carries Failed; a visit built elsewhere without page
// checks is judged by its error and status. Cancelled visits never fail.
func failedVisit(v *Visit) bool {
	switch {
	case v.Cancelled:
		return false
	case v.Failed:
		return true
	}
	return len(v.Page.Checks) == 0 && (v.Error != nil || v.StatusCode < 200 || v.StatusCode >= 400)
}

// Ingest records a finished lemming's life. Visits of a Streamed LifeLog
// were already counted by RecordVisit; any other LifeLog has its visits
// counted here.
func (r *Reporter) Ingest(ll LifeLog) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !ll.Streamed {
		ll.TotalVisits, ll.FailedVisits, ll.CancelledVisits = 0, 0, 0
		for i := range ll.Visits {
			v := &ll.Visits[i]
			r.recordVisitLocked(v, v.Timestamp.Add(v.Duration))
			ll.TotalVisits++
			switch {
			case v.Cancelled:
				ll.CancelledVisits++
			case failedVisit(v):
				ll.FailedVisits++
				if ll.FirstFailureAt.IsZero() {
					ll.FirstFailureAt = v.Timestamp
				}
			}
			if !v.Cancelled {
				ll.SlowestVisit = max(ll.SlowestVisit, v.latency())
			}
			ll.LongestWait = max(ll.LongestWait, v.WaitingRoom.Duration)
		}
	}

	r.sessions++
	if ll.FailedVisits == 0 && ll.Error == nil {
		r.flawless++
	}
	if ll.ExitReason != "" {
		r.exitReasons[ll.ExitReason]++
	}
	r.notable.consider(&ll)
}

// closeTrace flushes and closes the trace file, if any.
func (r *Reporter) closeTrace() {
	if r.trace != nil {
		r.trace.close()
	}
}

// Write renders the report and delivers it to every registered target
// concurrently. Returns a combined error if any target fails — successful
// targets are not affected by the failure of others.
//
// The context is passed to each target's Deliver method. Cancelling the
// context aborts in-progress deliveries (S3 uploads, SMTP connections)
// but does not affect targets that have already completed.
//
// Usage:
//
//	if err := reporter.Write(ctx); err != nil {
//	    log.Printf("report delivery error: %v", err)
//	}
//
// Warning: Write must be called after Run completes. Calling it while
// lemmings are still running produces a report of incomplete data.
func (r *Reporter) Write(ctx context.Context) error {
	r.closeTrace()
	if r.unsub != nil {
		r.unsub()
	}

	r.mu.Lock()
	data := r.buildReportData()
	r.mu.Unlock()

	rendered, err := renderReport(data, r.buildFilename())
	if err != nil {
		return err
	}

	if len(r.targets) == 0 {
		// No targets registered — fall back to stdout
		fmt.Println(rendered.Markdown)
		return nil
	}

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(r.targets))
	for _, target := range r.targets {
		go func() {
			results <- result{name: target.Name(), err: target.Deliver(ctx, rendered)}
		}()
	}

	var errs []string
	for range r.targets {
		res := <-results
		if res.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", res.name, res.err))
			fmt.Printf("  ⚠ delivery failed [%s]: %v\n", res.name, res.err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("report delivery errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Result returns the report data for a finished run, for callers such as
// main that need the verdict without re-rendering.
func (r *Reporter) Result() ReportData {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buildReportData()
}

// LiveStats is the dashboard's and ticker's once-a-second view of the
// aggregates. Percentiles come from the histograms, so building it never
// sorts samples while lemmings wait on the lock.
type LiveStats struct {
	Visits         int64           `json:"visits"`
	Failed         int64           `json:"failed"`
	Cancelled      int64           `json:"cancelled"`
	FailureRate    float64         `json:"failure_rate"`
	P50            time.Duration   `json:"p50_ns"`
	P95            time.Duration   `json:"p95_ns"`
	P99            time.Duration   `json:"p99_ns"`
	TTFBP95        time.Duration   `json:"ttfb_p95_ns"`
	Sessions       int64           `json:"sessions"`
	Rescued        int64           `json:"rescued"`
	RescuedPercent float64         `json:"rescued_percent"`
	Room           RoomReport      `json:"room"`
	Timeline       []TimelinePoint `json:"timeline,omitempty"`
	Paths          []PathReport    `json:"paths,omitempty"`
	StatusCodes    []CountRow      `json:"status_codes,omitempty"`
	Causes         []CountRow      `json:"causes,omitempty"`
}

// Live returns the current aggregates with the last points of the
// timeline and the busiest paths. Zero points or paths skips them.
func (r *Reporter) Live(points, paths int) LiveStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := LiveStats{
		Visits:    r.visits,
		Failed:    r.failed,
		Cancelled: r.cancelled,
		P50:       r.latency.hist.quantile(50),
		P95:       r.latency.hist.quantile(95),
		P99:       r.latency.hist.quantile(99),
		TTFBP95:   r.ttfb.quantile(95),
		Sessions:  r.sessions,
		Rescued:   r.flawless,
		Room: RoomReport{
			Held: r.roomHeld, DiedInLine: r.roomDied, MaxPosition: r.roomMaxPosition,
			MeanWait: r.roomWait.mean(), P95Wait: r.roomWait.quantile(95), LongestWait: r.roomWait.max,
		},
	}
	if completed := r.visits - r.cancelled; completed > 0 {
		s.FailureRate = float64(r.failed) / float64(completed)
	}
	if r.sessions > 0 {
		s.RescuedPercent = 100 * float64(r.flawless) / float64(r.sessions)
	}
	if points > 0 {
		s.Timeline = r.timeline.points(points)
	}
	if paths > 0 {
		all := make([]*pathStats, 0, len(r.byPath))
		for _, ps := range r.byPath {
			all = append(all, ps)
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].Hits != all[j].Hits {
				return all[i].Hits > all[j].Hits
			}
			return all[i].URL < all[j].URL
		})
		for i, ps := range all[:min(paths, len(all))] {
			s.Paths = append(s.Paths, PathReport{
				URL: pathOf(ps.URL), Hits: ps.Hits, Failed: ps.failed,
				XX2: ps.xx2, XX3: ps.xx3, XX4: ps.xx4, XX5: ps.xx5,
				P50: ps.hist.quantile(50), P95: ps.hist.quantile(95), P99: ps.hist.quantile(99),
				HitShare: float64(ps.Hits) / float64(max(all[0].Hits, 1)),
			})
			if ps.URL == otherPath {
				s.Paths[i].URL = otherPath
			}
		}
		s.StatusCodes = countRows(r.statusCodes, func(code int) string { return strconv.Itoa(code) })
		causes := make(map[string]int64, len(r.errorKinds)+len(r.failedChecks))
		for k, n := range r.errorKinds {
			causes["error: "+k] += n
		}
		for k, n := range r.failedChecks {
			causes["check: "+k] += n
		}
		s.Causes = countRows(causes, func(k string) string { return k })
	}
	return s
}

// buildFilename constructs the base report filename from the current date
// and the domain extracted from the hit URL.
//
// Format: lemmings.YYYY.MM.DD.domain
func (r *Reporter) buildFilename() string {
	date := time.Now().Format(reportDateFormat)
	domain := domainFromURL(r.cfg.Hit)
	return fmt.Sprintf("lemmings.%s.%s", date, domain)
}

// ── Report data model ────────────────────────────────────────────────────────

// ReportData is the fully computed dataset handed to every renderer.
type ReportData struct {
	Version     string        `json:"version"`
	GeneratedAt time.Time     `json:"generated_at"`
	Duration    time.Duration `json:"duration_ns"`
	Config      SwarmConfig   `json:"config"`
	Journey     string        `json:"journey,omitempty"`

	// Swarm-level aggregates
	TotalLemmings    int64 `json:"total_lemmings"`
	LemmingsStarted  int64 `json:"lemmings_started"`
	LemmingsUnborn   int64 `json:"lemmings_not_started"`
	TotalVisits      int64 `json:"total_visits"`
	TotalBytes       int64 `json:"total_bytes"`
	TotalWaitingRoom int64 `json:"total_waiting_room"`
	Total2xx         int64 `json:"total_2xx"`
	Total3xx         int64 `json:"total_3xx"`
	Total4xx         int64 `json:"total_4xx"`
	Total5xx         int64 `json:"total_5xx"`
	TotalErrors      int64 `json:"total_transport_errors"`

	// Outcomes
	FailedVisits    int64   `json:"failed_visits"`
	CancelledVisits int64   `json:"cancelled_visits"`
	FailureRate     float64 `json:"failure_rate"`
	VisitsPerSecond float64 `json:"visits_per_second"`
	Sessions        int64   `json:"sessions_observed"`
	Rescued         int64   `json:"sessions_flawless"`
	RescuedPercent  float64 `json:"rescued_percent"`
	ChecksumChanges int64   `json:"checksum_changes"`
	HTMLPages       int64   `json:"html_pages"`

	// Timing percentiles across all completed visits
	Fastest          time.Duration `json:"fastest_ns"`
	Slowest          time.Duration `json:"slowest_ns"`
	Mean             time.Duration `json:"mean_ns"`
	P50              time.Duration `json:"p50_ns"`
	P90              time.Duration `json:"p90_ns"`
	P95              time.Duration `json:"p95_ns"`
	P99              time.Duration `json:"p99_ns"`
	TTFBP50          time.Duration `json:"ttfb_p50_ns"`
	TTFBP95          time.Duration `json:"ttfb_p95_ns"`
	PercentileMethod string        `json:"percentile_method"`

	// Waiting room
	Room RoomReport `json:"waiting_room"`

	// Breakdowns
	StatusCodes  []CountRow `json:"status_codes"`
	ErrorKinds   []CountRow `json:"error_kinds"`
	FailedChecks []CountRow `json:"failed_checks"`
	ExitReasons  []CountRow `json:"exit_reasons"`

	// The run over time and the lives worth reading about
	Timeline []TimelinePoint `json:"timeline"`
	Notable  []NotableLife   `json:"notable_lemmings"`

	// CI gates and evidence completeness
	Gates        []GateResult `json:"gates"`
	Verdict      string       `json:"verdict"` // PASSED, FAILED or NO GATES
	DroppedLogs  int64        `json:"dropped_lifelogs"`
	TraceFile    string       `json:"trace_file,omitempty"`
	TraceWritten int64        `json:"trace_written,omitempty"`
	TraceDropped int64        `json:"trace_dropped,omitempty"`
	TraceError   string       `json:"trace_error,omitempty"`

	// Per-path breakdown, sorted by hit count descending
	Paths []PathReport `json:"paths"`
}

// GatesFailed reports whether any CI gate tripped.
func (d ReportData) GatesFailed() bool {
	return anyFailed(d.Gates)
}

// PathReport is the per-URL section of the report.
type PathReport struct {
	URL         string        `json:"url"`
	Hits        int64         `json:"hits"`
	Bytes       string        `json:"bytes"` // human-readable
	XX2         int64         `json:"2xx"`
	XX3         int64         `json:"3xx"`
	XX4         int64         `json:"4xx"`
	XX5         int64         `json:"5xx"`
	WaitingRoom int64         `json:"waiting_room"`
	Errors      int64         `json:"transport_errors"`
	Failed      int64         `json:"failed"`
	P50         time.Duration `json:"p50_ns"`
	P95         time.Duration `json:"p95_ns"`
	P99         time.Duration `json:"p99_ns"`
	HitShare    float64       `json:"hit_share"` // 0..1 of the busiest path, for bars
}

// RoomReport summarises waiting room behaviour.
type RoomReport struct {
	Held        int64         `json:"held"`
	DiedInLine  int64         `json:"died_in_line"`
	MaxPosition int           `json:"max_position"`
	MeanWait    time.Duration `json:"mean_wait_ns"`
	P95Wait     time.Duration `json:"p95_wait_ns"`
	LongestWait time.Duration `json:"longest_wait_ns"`
}

// CountRow is one row of a labelled count table.
type CountRow struct {
	Label string  `json:"label"`
	Count int64   `json:"count"`
	Share float64 `json:"share"` // 0..1 of the table total
}

// GateResult is the outcome of one CI gate.
type GateResult struct {
	Name     string `json:"name"`
	Limit    string `json:"limit"`
	Observed string `json:"observed"`
	Passed   bool   `json:"passed"`
}

// buildReportData computes the full ReportData from accumulated state.
// Must be called with r.mu held.
func (r *Reporter) buildReportData() ReportData {
	end := r.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	cfg := r.cfg
	cfg.Hit = safeURL(cfg.Hit)

	data := ReportData{
		Version:         r.cfg.Version,
		GeneratedAt:     time.Now(),
		Duration:        end.Sub(r.startedAt).Round(time.Millisecond),
		Config:          cfg,
		TotalLemmings:   r.cfg.Terrain * r.cfg.Pack,
		LemmingsStarted: r.started,
		LemmingsUnborn:  r.notStarted,
		TotalVisits:     r.visits,
		FailedVisits:    r.failed,
		CancelledVisits: r.cancelled,
		Sessions:        r.sessions,
		Rescued:         r.flawless,
		ChecksumChanges: r.checksumChanges,
		HTMLPages:       r.htmlPages,
		DroppedLogs:     r.dropped,
		Timeline:        r.timeline.points(0),
		Notable:         r.notable.lives(),
		TraceFile:       r.cfg.TraceFile,
	}
	if r.cfg.Journey != nil {
		data.Journey = r.cfg.Journey.Name
	}
	if completed := r.visits - r.cancelled; completed > 0 {
		data.FailureRate = float64(r.failed) / float64(completed)
	}
	if secs := data.Duration.Seconds(); secs > 0 {
		data.VisitsPerSecond = float64(r.visits) / secs
	}
	if r.sessions > 0 {
		data.RescuedPercent = 100 * float64(r.flawless) / float64(r.sessions)
	}

	var busiest int64
	for _, ps := range r.byPath {
		data.TotalBytes += ps.Bytes
		data.TotalWaitingRoom += ps.waitingRoom
		data.Total2xx += ps.xx2
		data.Total3xx += ps.xx3
		data.Total4xx += ps.xx4
		data.Total5xx += ps.xx5
		data.TotalErrors += ps.errors
		busiest = max(busiest, ps.Hits)

		data.Paths = append(data.Paths, PathReport{
			URL:         ps.URL,
			Hits:        ps.Hits,
			Bytes:       formatBytes(ps.Bytes),
			XX2:         ps.xx2,
			XX3:         ps.xx3,
			XX4:         ps.xx4,
			XX5:         ps.xx5,
			WaitingRoom: ps.waitingRoom,
			Errors:      ps.errors,
			Failed:      ps.failed,
			P50:         ps.quantile(50),
			P95:         ps.quantile(95),
			P99:         ps.quantile(99),
		})
	}
	// Sort paths by hit count descending, then by URL for stable output.
	sort.Slice(data.Paths, func(i, j int) bool {
		if data.Paths[i].Hits != data.Paths[j].Hits {
			return data.Paths[i].Hits > data.Paths[j].Hits
		}
		return data.Paths[i].URL < data.Paths[j].URL
	})
	for i := range data.Paths {
		if busiest > 0 {
			data.Paths[i].HitShare = float64(data.Paths[i].Hits) / float64(busiest)
		}
	}

	// Global timing percentiles across completed visits.
	if r.latency.hist.count > 0 {
		data.Fastest = r.latency.hist.min
		data.Slowest = r.latency.hist.max
		data.Mean = r.latency.hist.mean()
		data.P50 = r.latency.quantile(50)
		data.P90 = r.latency.quantile(90)
		data.P95 = r.latency.quantile(95)
		data.P99 = r.latency.quantile(99)
	}
	data.TTFBP50 = r.ttfb.quantile(50)
	data.TTFBP95 = r.ttfb.quantile(95)
	data.PercentileMethod = "exact (nearest rank)"
	if !r.latency.isExact() {
		data.PercentileMethod = fmt.Sprintf("log histogram, within %.0f%% of the true value", (histogramGrowth-1)*100)
	}

	data.Room = RoomReport{
		Held:        r.roomHeld,
		DiedInLine:  r.roomDied,
		MaxPosition: r.roomMaxPosition,
		MeanWait:    r.roomWait.mean(),
		P95Wait:     r.roomWait.quantile(95),
		LongestWait: r.roomWait.max,
	}

	data.StatusCodes = countRows(r.statusCodes, func(code int) string {
		if code == 0 {
			return "no response"
		}
		return fmt.Sprint(code)
	})
	data.ErrorKinds = countRows(r.errorKinds, func(s string) string { return s })
	data.FailedChecks = countRows(r.failedChecks, func(s string) string { return s })
	data.ExitReasons = countRows(r.exitReasons, func(s string) string { return s })

	if r.trace != nil {
		data.TraceWritten = r.trace.written.Load()
		data.TraceDropped = r.trace.dropped.Load()
		data.TraceError = r.trace.failure()
	}

	data.Gates, data.Verdict = r.evaluateGates(data)
	return data
}

// evaluateGates checks the configured CI budgets against the run.
func (r *Reporter) evaluateGates(data ReportData) ([]GateResult, string) {
	var gates []GateResult
	if r.cfg.FailureGate {
		gates = append(gates, GateResult{
			Name:     "failure rate",
			Limit:    fmt.Sprintf("≤ %.2f%%", r.cfg.MaxFailureRate*100),
			Observed: fmt.Sprintf("%.2f%%", data.FailureRate*100),
			Passed:   data.FailureRate <= r.cfg.MaxFailureRate,
		})
	}
	if r.cfg.P95Budget > 0 {
		gates = append(gates, GateResult{
			Name:     "p95 latency",
			Limit:    "≤ " + r.cfg.P95Budget.String(),
			Observed: formatMillis(data.P95),
			Passed:   data.P95 <= r.cfg.P95Budget,
		})
	}
	switch {
	case len(gates) == 0:
		return gates, "NO GATES"
	case anyFailed(gates):
		return gates, "FAILED"
	}
	return gates, "PASSED"
}

// anyFailed reports whether any gate in gates failed.
func anyFailed(gates []GateResult) bool {
	for _, g := range gates {
		if !g.Passed {
			return true
		}
	}
	return false
}

// countRows turns a count map into rows sorted by count, then label.
func countRows[K comparable](m map[K]int64, label func(K) string) []CountRow {
	var total int64
	rows := make([]CountRow, 0, len(m))
	for k, n := range m {
		total += n
		rows = append(rows, CountRow{Label: label(k), Count: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Label < rows[j].Label
	})
	for i := range rows {
		if total > 0 {
			rows[i].Share = float64(rows[i].Count) / float64(total)
		}
	}
	return rows
}

// ── Notable lemmings ─────────────────────────────────────────────────────────

// NotableLife is one lemming whose life is worth reading in full.
type NotableLife struct {
	Title      string        `json:"title"`
	Why        string        `json:"why"`
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Persona    string        `json:"persona"`
	Language   string        `json:"language"`
	Terrain    int           `json:"terrain"`
	Pack       int           `json:"pack"`
	Lifespan   time.Duration `json:"lifespan_ns"`
	Visits     int64         `json:"visits"`
	Failed     int64         `json:"failed"`
	Cancelled  int64         `json:"cancelled"`
	ExitReason string        `json:"exit_reason"`
	Omitted    int64         `json:"omitted_steps"`
	Steps      []LifeStep    `json:"steps"`
}

// LifeStep is one visit in a NotableLife.
type LifeStep struct {
	Seq       int     `json:"seq"`
	Step      string  `json:"step,omitempty"`
	Path      string  `json:"path"`
	Status    int     `json:"status"`
	Millis    float64 `json:"ms"`
	Title     string  `json:"title,omitempty"`
	Failed    bool    `json:"failed"`
	Cancelled bool    `json:"cancelled"`
	QueuedMS  float64 `json:"queued_ms,omitempty"`
	Why       string  `json:"why,omitempty"`
	Bar       float64 `json:"bar"` // 0..1 of the slowest step shown
}

// notableSet keeps the current best candidate for each notable title.
type notableSet struct {
	explorer, unlucky, patientZero, patient *LifeLog
}

// consider offers a finished life to every notable category.
func (n *notableSet) consider(ll *LifeLog) {
	keep := func(slot **LifeLog, better bool) {
		if better {
			c := *ll
			*slot = &c
		}
	}
	keep(&n.explorer, ll.TotalVisits > 0 && (n.explorer == nil || ll.TotalVisits > n.explorer.TotalVisits))
	keep(&n.unlucky, ll.SlowestVisit > 0 && (n.unlucky == nil || ll.SlowestVisit > n.unlucky.SlowestVisit))
	keep(&n.patientZero, !ll.FirstFailureAt.IsZero() &&
		(n.patientZero == nil || ll.FirstFailureAt.Before(n.patientZero.FirstFailureAt)))
	keep(&n.patient, ll.LongestWait > 0 && (n.patient == nil || ll.LongestWait > n.patient.LongestWait))
}

// lives renders the notable lemmings in a fixed order.
func (n *notableSet) lives() []NotableLife {
	var out []NotableLife
	add := func(ll *LifeLog, title, why string) {
		if ll != nil {
			out = append(out, notableLife(ll, title, why))
		}
	}
	if n.explorer != nil {
		add(n.explorer, "The Explorer", fmt.Sprintf("visited the most pages: %s", formatInt(n.explorer.TotalVisits)))
	}
	if n.patientZero != nil {
		add(n.patientZero, "Patient Zero", "hit the first failure of the run")
	}
	if n.unlucky != nil {
		add(n.unlucky, "The Unlucky One", "got the slowest page of the run: "+n.unlucky.SlowestVisit.Round(time.Millisecond).String())
	}
	if n.patient != nil {
		add(n.patient, "The Patient One", "spent longest in the waiting room: "+n.patient.LongestWait.Round(time.Second).String())
	}
	return out
}

// notableLife converts a LifeLog into its report form.
func notableLife(ll *LifeLog, title, why string) NotableLife {
	life := NotableLife{
		Title:      title,
		Why:        why,
		ID:         ll.Identity.ID,
		Name:       ll.Identity.Name,
		Persona:    personaOf(ll.Identity.UserAgent),
		Language:   ll.Identity.Language,
		Terrain:    ll.Terrain,
		Pack:       ll.Pack,
		Lifespan:   ll.Duration.Round(time.Millisecond),
		Visits:     ll.TotalVisits,
		Failed:     ll.FailedVisits,
		Cancelled:  ll.CancelledVisits,
		ExitReason: ll.ExitReason,
		Omitted:    ll.OmittedVisits,
	}
	visits := ll.Visits
	if len(visits) > reportTailSteps {
		life.Omitted += int64(len(visits) - reportTailSteps)
		visits = visits[len(visits)-reportTailSteps:]
	}
	var slowest float64
	for i := range visits {
		v := &visits[i]
		step := LifeStep{
			Seq:       v.Sequence,
			Step:      v.Step,
			Path:      pathOf(v.URL),
			Status:    v.StatusCode,
			Millis:    millis(v.latency()),
			Title:     v.Page.Title,
			Failed:    failedVisit(v),
			Cancelled: v.Cancelled,
			QueuedMS:  millis(v.WaitingRoom.Duration),
			Why:       whyFailed(v),
		}
		slowest = math.Max(slowest, step.Millis)
		life.Steps = append(life.Steps, step)
	}
	for i := range life.Steps {
		if slowest > 0 {
			life.Steps[i].Bar = life.Steps[i].Millis / slowest
		}
	}
	return life
}

// whyFailed explains a failed or cancelled visit in a few words.
func whyFailed(v *Visit) string {
	switch {
	case v.Cancelled:
		return "life ended mid-visit"
	case v.ErrorKind != "":
		return v.ErrorKind
	case v.Error != nil:
		return errorKind(v.Error)
	}
	for _, c := range v.Page.Checks {
		if !c.Passed {
			return c.Name + ": " + c.Detail
		}
	}
	if failedVisit(v) {
		return fmt.Sprintf("status %d", v.StatusCode)
	}
	return ""
}

// pathOf returns the path of a URL for compact display.
func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return "/"
	}
	return u.Path
}

// personaOf summarises a user agent as "Browser · Platform".
func personaOf(ua string) string {
	browser, platform := "Browser", "Unknown"
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	switch {
	case strings.Contains(ua, "iPhone"):
		platform = "iPhone"
	case strings.Contains(ua, "Android"):
		platform = "Android"
	case strings.Contains(ua, "Windows"):
		platform = "Windows"
	case strings.Contains(ua, "Mac OS X"):
		platform = "macOS"
	case strings.Contains(ua, "Linux"):
		platform = "Linux"
	}
	return browser + " · " + platform
}

// ── Rendering ────────────────────────────────────────────────────────────────

// RenderedReport is a report rendered in every format, ready to deliver.
type RenderedReport struct {
	Filename string // base name without extension
	Markdown string
	HTML     string
	JSON     []byte
}

// renderReport renders data in every format.
func renderReport(data ReportData, filename string) (RenderedReport, error) {
	md, err := renderMarkdown(data)
	if err != nil {
		return RenderedReport{}, fmt.Errorf("render markdown: %w", err)
	}
	html, err := renderHTML(data)
	if err != nil {
		return RenderedReport{}, fmt.Errorf("render html: %w", err)
	}
	js, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return RenderedReport{}, fmt.Errorf("render json: %w", err)
	}
	return RenderedReport{Filename: filename, Markdown: md, HTML: html, JSON: js}, nil
}

// templateFuncs are available inside both templates.
var templateFuncs = map[string]any{
	"formatInt":      func(n int64) string { return formatInt(n) },
	"formatBytesInt": func(n int64) string { return formatBytes(n) },
	"ms":             func(d time.Duration) string { return formatMillis(d) },
	"pct":            func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"pct100":         func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
	"width":          func(f float64) string { return fmt.Sprintf("%.1f%%", math.Max(f*100, 0.5)) },
	"float1":         func(f float64) string { return fmt.Sprintf("%.1f", f) },
	"statusClass":    func(code int) string { return statusClass(code) },
	"atoi":           func(s string) int { n, _ := strconv.Atoi(s); return n },
	"sparkline":      sparkline,
	"add":            func(a, b int) int { return a + b },
	"round":          roundDuration,
	"lang":           func(s string) string { tag, _, _ := strings.Cut(s, ","); return tag },
	"shortID":        func(id string) string { return clip(id, 8) },
	"rescueLine":     rescueLine,
}

var (
	markdownTemplate = template.Must(template.New("report.md.tmpl").Funcs(templateFuncs).
				ParseFS(reportTemplates, "templates/report.md.tmpl"))
	htmlTemplate = htmltemplate.Must(htmltemplate.New("report.html.tmpl").
			Funcs(templateFuncs).
			Funcs(htmltemplate.FuncMap{"timelineSVG": timelineSVG}).
			ParseFS(reportTemplates, "templates/report.html.tmpl"))
)

// renderMarkdown renders the markdown report.
func renderMarkdown(data ReportData) (string, error) {
	var buf bytes.Buffer
	if err := markdownTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderHTML renders the self-contained HTML report.
func renderHTML(data ReportData) (string, error) {
	var buf bytes.Buffer
	if err := htmlTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// formatMillis renders a duration in milliseconds with sensible precision.
func formatMillis(d time.Duration) string {
	ms := millis(d)
	switch {
	case d == 0:
		return "0 ms"
	case ms < 1:
		return fmt.Sprintf("%.2f ms", ms)
	case ms < 100:
		return fmt.Sprintf("%.1f ms", ms)
	case ms < 10_000:
		return fmt.Sprintf("%.0f ms", ms)
	}
	return d.Round(100 * time.Millisecond).String()
}

// roundDuration rounds for display: whole seconds from ten seconds up,
// milliseconds below.
func roundDuration(d time.Duration) time.Duration {
	if d >= 10*time.Second {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}

// sparkBlocks are the eight levels of a text sparkline.
var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// sparkline renders visits per second over the run as block characters,
// resampled to at most 60 characters.
func sparkline(points []TimelinePoint) string {
	if len(points) == 0 {
		return ""
	}
	const width = 60
	values := make([]float64, 0, width)
	step := math.Max(1, float64(len(points))/width)
	for x := 0.0; int(x) < len(points); x += step {
		lo, hi := int(x), min(int(x+step), len(points))
		var sum float64
		for _, p := range points[lo:max(hi, lo+1)] {
			sum += p.RatePerSecond
		}
		values = append(values, sum/float64(max(hi-lo, 1)))
	}
	var top float64
	for _, v := range values {
		top = math.Max(top, v)
	}
	var b strings.Builder
	for _, v := range values {
		i := 0
		if top > 0 {
			i = int(math.Round(v / top * float64(len(sparkBlocks)-1)))
		}
		b.WriteRune(sparkBlocks[i])
	}
	return b.String()
}

// rescueLine is the end-of-level message, in the spirit of the game.
func rescueLine(d ReportData) string {
	switch {
	case d.Sessions == 0:
		return "No lemmings made it home to report."
	case d.Rescued == d.Sessions:
		return "All lemmings accounted for."
	case d.RescuedPercent >= 90:
		return "So close. A few lemmings hit trouble."
	case d.RescuedPercent >= 50:
		return "Oh no! Many lemmings hit trouble."
	}
	return "Oh no! Most lemmings hit trouble."
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// percentile computes the Nth percentile of a pre-sorted duration slice.
// Returns 0 if the slice is empty.
func percentile(sorted []time.Duration, n int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	// Nearest rank method
	idx := int(math.Ceil(float64(n)/100.0*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// resolveSavePath resolves the output directory from saveTo and the hit URL.
// Handles local paths and s3:// URIs.
// Local: <saveTo>/lemmings/<domain>/
// S3:    s3://<bucket>/<prefix>/<domain>/
func resolveSavePath(saveTo, hit string) (string, error) {
	domain := domainFromURL(hit)

	if strings.HasPrefix(saveTo, "s3://") {
		return saveTo + "/" + domain, nil
	}

	return filepath.Join(saveTo, "lemmings", domain), nil
}

// domainFromURL extracts a filesystem-safe host[-port] string from a URL.
// Userinfo, path, query and fragment are ignored.
func domainFromURL(hit string) string {
	host := ""
	if u, err := url.Parse(hit); err == nil && u.Host != "" {
		host = u.Host
	} else {
		host = strings.TrimPrefix(strings.TrimPrefix(hit, "https://"), "http://")
		host = strings.Split(host, "/")[0]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	}
	return strings.NewReplacer(":", "-", "[", "", "]", "").Replace(host)
}
