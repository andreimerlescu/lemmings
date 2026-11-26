package report

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

const (
	// SchemaVersion identifies the JSON report schema emitted by this release.
	SchemaVersion            = "2.0"
	reportDateFormat         = "2006.01.02"
	reportTimestampFormat    = "2006.01.02.150405Z"
	maxReportPaths           = 1000
	otherPath                = "[other paths]"
	exactGlobalSamples       = 8192
	exactPathSamples         = 1024
	reportTailSteps          = 40
	maxHTMLInlineImageBytes  = 12 << 20 // 12 MiB cap for self-contained HTML data URIs
	maxEmailInlineImageBytes = 2 << 20  // 2 MiB cap for SMTP-safe inline HTML reports
)

//go:embed templates/report.md.tmpl templates/report.html.tmpl
var reportTemplates embed.FS

// Target receives a rendered report for delivery (Local, S3, SMTP).
type Target interface {
	Deliver(ctx context.Context, report Rendered) error
	Name() string
}

// ArtifactAsset is a standalone screenshot file written alongside reports on
// Local and S3 targets so Markdown renders relative image links natively.
type ArtifactAsset struct {
	Filename string // e.g. "<id>.jpg"
	MIMEType string
	Data     []byte
}

// Rendered is a report rendered in every output format plus standalone assets.
// RemoteMarkdown, RemoteHTML, and RemoteJSON hold screenshot-free variants used
// by default for remote delivery targets (email and S3) under SEC-10 unless the
// operator explicitly opts in with -include-remote-screenshots.
type Rendered struct {
	Filename       string
	Domain         string
	Markdown       string
	HTML           string
	HTMLForEmail   string
	JSON           []byte
	Artifacts      []ArtifactAsset
	RemoteMarkdown string
	RemoteHTML     string
	RemoteJSON     []byte
}

// ForensicsReport embeds capture statistics and all redacted error artifacts
// (including admin triage verdicts and notes) into the final report.
type ForensicsReport struct {
	Stats     forensics.Stats      `json:"stats"`
	Artifacts []forensics.Artifact `json:"artifacts,omitempty"`
}

// Data is the complete computed dataset passed to every renderer and JSON export.
type Data struct {
	SchemaVersion string        `json:"schema_version"`
	Version       string        `json:"version"`
	GeneratedAt   time.Time     `json:"generated_at"`
	Duration      time.Duration `json:"duration_ns"`
	Config        swarm.Config  `json:"config"`
	Plan          *PlanReport   `json:"plan,omitempty"`

	TotalLemmings     int64 `json:"total_lemmings"`
	LemmingsStarted   int64 `json:"lemmings_started"`
	LemmingsCompleted int64 `json:"lemmings_completed"`
	LemmingsUnborn    int64 `json:"lemmings_not_started"`
	TotalVisits       int64 `json:"total_visits"`
	TotalBytes        int64 `json:"total_bytes"`
	TotalWaitingRoom  int64 `json:"total_waiting_room"`
	Total2xx          int64 `json:"total_2xx"`
	Total3xx          int64 `json:"total_3xx"`
	Total4xx          int64 `json:"total_4xx"`
	Total5xx          int64 `json:"total_5xx"`
	TotalErrors       int64 `json:"total_transport_errors"`

	FailedVisits    int64   `json:"failed_visits"`
	CancelledVisits int64   `json:"cancelled_visits"`
	FailureRate     float64 `json:"failure_rate"`
	VisitsPerSecond float64 `json:"visits_per_second"`
	Sessions        int64   `json:"sessions_observed"`
	Rescued         int64   `json:"sessions_flawless"`
	RescuedPercent  float64 `json:"rescued_percent"`
	ChecksumChanges int64   `json:"checksum_changes"`
	HTMLPages       int64   `json:"html_pages"`

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

	Assets       AssetReport     `json:"assets"`
	Room         RoomReport      `json:"waiting_room"`
	Concert      ConcertReport   `json:"concert"`
	Scouts       ScoutReport     `json:"scouts"`
	StatusCodes  []CountRow      `json:"status_codes"`
	ErrorKinds   []CountRow      `json:"error_kinds"`
	FailedChecks []CountRow      `json:"failed_checks"`
	ExitReasons  []CountRow      `json:"exit_reasons"`
	Timeline     []TimelinePoint `json:"timeline"`
	Notable      []NotableLife   `json:"notable_lemmings"`
	Forensics    ForensicsReport `json:"forensics"`

	Gates        []GateResult `json:"gates"`
	Verdict      string       `json:"verdict"`
	DroppedLogs  int64        `json:"dropped_lifelogs"`
	TraceFile    string       `json:"trace_file,omitempty"`
	TraceWritten int64        `json:"trace_written,omitempty"`
	TraceDropped int64        `json:"trace_dropped,omitempty"`
	TraceError   string       `json:"trace_error,omitempty"`

	Paths []PathReport `json:"paths"`
}

// GatesFailed reports whether any CI gate failed.
func (d Data) GatesFailed() bool { return anyFailed(d.Gates) }

// PathReport is the per-URL breakdown row.
type PathReport struct {
	URL         string        `json:"url"`
	Hits        int64         `json:"hits"`
	Bytes       string        `json:"bytes"`
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
	HitShare    float64       `json:"hit_share"`
}

// AssetReport summarizes same-origin subresource fetches when -assets is enabled.
// Asset counts and latencies are strictly separated from page visit telemetry.
type AssetReport struct {
	Requests  int64           `json:"requests"`
	Failed    int64           `json:"failed"`
	Denied403 int64           `json:"denied_403"`
	Bytes     int64           `json:"bytes"`
	P50       time.Duration   `json:"p50_ns"`
	P95       time.Duration   `json:"p95_ns"`
	P99       time.Duration   `json:"p99_ns"`
	ByClass   []AssetClassRow `json:"by_class,omitempty"`
}

// AssetClassRow summarizes subresource fetches for one asset class.
type AssetClassRow struct {
	Class     string        `json:"class"`
	Requests  int64         `json:"requests"`
	Failed    int64         `json:"failed"`
	Denied403 int64         `json:"denied_403"`
	Bytes     int64         `json:"bytes"`
	P95       time.Duration `json:"p95_ns"`
}

// RoomReport summarizes waiting room activity and Concert queue outcomes.
type RoomReport struct {
	Held        int64         `json:"held"`
	Admitted    int64         `json:"admitted"`
	DiedInLine  int64         `json:"died_in_line"`
	Expired     int64         `json:"expired"`
	Denied      int64         `json:"denied"`
	Rejected503 int64         `json:"rejected_503"`
	TotalPolls  int64         `json:"total_polls"`
	MaxPosition int           `json:"max_position"`
	MeanWait    time.Duration `json:"mean_wait_ns"`
	P95Wait     time.Duration `json:"p95_wait_ns"`
	LongestWait time.Duration `json:"longest_wait_ns"`
}

// ConcertReport summarizes Concert admission-pass, room-ticket, and ban telemetry.
type ConcertReport struct {
	AdmitPasses int64 `json:"admit_passes"`
	RoomTickets int64 `json:"room_tickets"`
	Bans429     int64 `json:"bans_429"`
	Bans403     int64 `json:"bans_403"`
}

// ScoutReport summarizes real-browser Scout page navigations.
type ScoutReport struct {
	Visits   int64 `json:"visits"`
	Failures int64 `json:"failures"`
}

// PlanReport summarizes YAML Plan / multi-persona Suite and rogue cohort execution.
type PlanReport struct {
	RogueRate     float64         `json:"rogue_rate"`
	PlanLemmings  int64           `json:"plan_lemmings"`
	PlanPassed    int64           `json:"plan_passed"`
	PlanFailed    int64           `json:"plan_failed"`
	PlanPassRate  float64         `json:"plan_pass_rate"`
	RogueLemmings int64           `json:"rogue_lemmings"`
	RogueFlawless int64           `json:"rogue_flawless"`
	Personas      []PersonaReport `json:"personas,omitempty"`
}

// PersonaReport summarizes execution metrics for one named Plan persona in a Suite.
type PersonaReport struct {
	Name       string           `json:"name"`
	Weight     int              `json:"weight"`
	TotalSteps int              `json:"total_steps"`
	Lemmings   int64            `json:"lemmings"`
	Passed     int64            `json:"passed"`
	Failed     int64            `json:"failed"`
	PassRate   float64          `json:"pass_rate"`
	Steps      []PlanStepReport `json:"steps,omitempty"`
}

// PlanStepReport summarizes execution metrics for one ordered step inside a Plan persona.
type PlanStepReport struct {
	Index      int           `json:"index"`
	Name       string        `json:"name"`
	Method     string        `json:"method"`
	Path       string        `json:"path"`
	Hits       int64         `json:"hits"`
	Passed     int64         `json:"passed"`
	Failed     int64         `json:"failed"`
	P50        time.Duration `json:"p50_ns"`
	P95        time.Duration `json:"p95_ns"`
	P99        time.Duration `json:"p99_ns"`
	TopFailure string        `json:"top_failure,omitempty"`
}

// CountRow is one labelled count row.
type CountRow struct {
	Label string  `json:"label"`
	Count int64   `json:"count"`
	Share float64 `json:"share"`
}

// GateResult is the outcome of one CI gate.
type GateResult struct {
	Name     string `json:"name"`
	Limit    string `json:"limit"`
	Observed string `json:"observed"`
	Passed   bool   `json:"passed"`
}

// LiveStats is the dashboard's once-a-second aggregate snapshot.
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
	Assets         AssetReport     `json:"assets"`
	Room           RoomReport      `json:"room"`
	Concert        ConcertReport   `json:"concert"`
	Scouts         ScoutReport     `json:"scouts"`
	Plan           *PlanReport     `json:"plan,omitempty"`
	Timeline       []TimelinePoint `json:"timeline,omitempty"`
	Paths          []PathReport    `json:"paths,omitempty"`
	StatusCodes    []CountRow      `json:"status_codes,omitempty"`
	Causes         []CountRow      `json:"causes,omitempty"`
}

// NotableLife is one lemming highlighted in the final report.
type NotableLife struct {
	Title      string        `json:"title"`
	Why        string        `json:"why"`
	Number     int64         `json:"number"`
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Persona    string        `json:"persona"`
	Language   string        `json:"language"`
	Terrain    int           `json:"terrain"`
	Pack       int           `json:"pack"`
	Role       string        `json:"role,omitempty"`
	PlanName   string        `json:"plan_name,omitempty"`
	PlanPassed bool          `json:"plan_passed,omitempty"`
	Lifespan   time.Duration `json:"lifespan_ns"`
	Visits     int64         `json:"visits"`
	Failed     int64         `json:"failed"`
	Cancelled  int64         `json:"cancelled"`
	ExitReason string        `json:"exit_reason"`
	Omitted    int64         `json:"omitted_steps"`
	Steps      []LifeStep    `json:"steps"`
}

// LifeStep is one visit inside a NotableLife.
type LifeStep struct {
	Seq       int     `json:"seq"`
	Method    string  `json:"method,omitempty"`
	Step      string  `json:"step,omitempty"`
	Path      string  `json:"path"`
	Status    int     `json:"status"`
	Millis    float64 `json:"ms"`
	Title     string  `json:"title,omitempty"`
	Failed    bool    `json:"failed"`
	Cancelled bool    `json:"cancelled"`
	QueuedMS  float64 `json:"queued_ms,omitempty"`
	Why       string  `json:"why,omitempty"`
	Bar       float64 `json:"bar"`
}

type planStepStats struct {
	Index          int
	Name           string
	Method         string
	Path           string
	Hits           int64
	Passed         int64
	Failed         int64
	FailureReasons map[string]int64
	LatencySeries
}

type personaStats struct {
	Name       string
	Weight     int
	TotalSteps int
	Lemmings   int64
	Passed     int64
	Failed     int64
	Steps      []*planStepStats
}

type pathStats struct {
	URL         string
	Hits        int64
	Bytes       int64
	xx2         int64
	xx3         int64
	xx4         int64
	xx5         int64
	waitingRoom int64
	errors      int64
	failed      int64
	LatencySeries
}

type assetClassStats struct {
	Class     string
	Requests  int64
	Failed    int64
	Denied403 int64
	Bytes     int64
	LatencySeries
}

type notableSet struct {
	explorer, unlucky, patientZero, patient *swarm.LifeLog
}

func (n *notableSet) consider(ll *swarm.LifeLog) {
	keep := func(slot **swarm.LifeLog, better bool) {
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

func (n *notableSet) lives() []NotableLife {
	var out []NotableLife
	add := func(ll *swarm.LifeLog, title, why string) {
		if ll != nil {
			out = append(out, notableLife(ll, title, why))
		}
	}
	if n.explorer != nil {
		add(n.explorer, "The Explorer", fmt.Sprintf("visited the most pages: %s", FormatInt(n.explorer.TotalVisits)))
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

func notableLife(ll *swarm.LifeLog, title, why string) NotableLife {
	role := ll.Role
	if role == "" {
		role = ll.Identity.Role
	}
	planName := ll.PlanName
	if planName == "" {
		planName = ll.Identity.PlanName
	}
	life := NotableLife{
		Title:      title,
		Why:        why,
		Number:     ll.Identity.Number,
		ID:         ll.Identity.ID,
		Name:       ll.Identity.Name,
		Persona:    swarm.PersonaOf(ll.Identity.UserAgent),
		Language:   ll.Identity.Language,
		Terrain:    ll.Terrain,
		Pack:       ll.Pack,
		Role:       role,
		PlanName:   planName,
		PlanPassed: ll.PlanPassed,
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
			Method:    v.Method,
			Step:      v.Step,
			Path:      discovery.PathOf(v.URL),
			Status:    v.StatusCode,
			Millis:    Millis(v.EffectiveLatency()),
			Title:     v.Page.Title,
			Failed:    swarm.FailedVisit(v),
			Cancelled: v.Cancelled,
			QueuedMS:  Millis(v.WaitingRoom.Duration),
			Why:       swarm.WhyFailed(v),
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

// Reporter accumulates live visits, completed lemming lives, and forensics
// artifacts, then renders and delivers the final reports. Safe for concurrent use.
type Reporter struct {
	cfg        swarm.Config
	mu         sync.Mutex
	byPath     map[string]*pathStats
	startedAt  time.Time
	endedAt    time.Time
	targets    []Target
	trace      *TraceWriter
	forensics  *forensics.Store
	unsub      func()
	onErrorLog func(targetName string, err error)

	latency        LatencySeries
	ttfb           Histogram
	timeline       *Timeline
	queuedLemmings map[string]struct{}

	visits, failed, cancelled  int64
	checksumChanges, htmlPages int64
	statusCodes                map[int]int64
	errorKinds                 map[string]int64
	failedChecks               map[string]int64

	roomHeld, roomAdmitted, roomDied           int64
	roomExpired, roomDenied, roomRejected503   int64
	roomPollEvents, roomVisitPolls             int64
	roomWait                                   Histogram
	roomMaxPosition                            int
	concertAdmitPasses, concertRoomTickets     int64
	concertBans429, concertBans403             int64
	assetRequests, assetFailed, assetDenied403 int64
	assetBytes                                 int64
	assetLatency                               LatencySeries
	assetsByClass                              map[string]*assetClassStats
	scoutVisits, scoutFailures                 int64

	planOrder     []string
	personas      map[string]*personaStats
	planLemmings  int64
	planPassed    int64
	planFailed    int64
	rogueLemmings int64
	rogueFlawless int64

	sessions, flawless int64
	exitReasons        map[string]int64
	notable            notableSet

	started, completed, dropped int64
}

// New constructs a Reporter for cfg.
func New(cfg swarm.Config) *Reporter {
	r := &Reporter{
		cfg:            cfg,
		byPath:         make(map[string]*pathStats),
		startedAt:      time.Now(),
		latency:        LatencySeries{exactCap: exactGlobalSamples},
		assetLatency:   LatencySeries{exactCap: exactGlobalSamples},
		assetsByClass:  make(map[string]*assetClassStats),
		personas:       make(map[string]*personaStats),
		timeline:       NewTimeline(),
		queuedLemmings: make(map[string]struct{}),
		statusCodes:    make(map[int]int64),
		errorKinds:     make(map[string]int64),
		failedChecks:   make(map[string]int64),
		exitReasons:    make(map[string]int64),
	}
	if cfg.Plan != nil {
		for _, p := range cfg.Plan.Plans {
			if p == nil || p.Name == "" {
				continue
			}
			if _, exists := r.personas[p.Name]; exists {
				continue
			}
			weight := p.Weight
			if weight <= 0 {
				weight = 1
			}
			steps := make([]*planStepStats, len(p.Items))
			for i, item := range p.Items {
				name := item.Name
				if name == "" {
					name = fmt.Sprintf("step-%d", i+1)
				}
				steps[i] = &planStepStats{
					Index:          i + 1,
					Name:           name,
					Method:         item.EffectiveMethod(),
					Path:           discovery.SafeURL(item.Path),
					FailureReasons: make(map[string]int64),
					LatencySeries:  LatencySeries{exactCap: exactPathSamples},
				}
			}
			r.planOrder = append(r.planOrder, p.Name)
			r.personas[p.Name] = &personaStats{
				Name:       p.Name,
				Weight:     weight,
				TotalSteps: len(p.Items),
				Steps:      steps,
			}
		}
	}
	return r
}

// SetForensics attaches the forensics Store so captured screenshots and admin
// triage reviews are embedded into the final report.
func (r *Reporter) SetForensics(store *forensics.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forensics = store
}

// SetTrace attaches an optional JSONL trace writer.
func (r *Reporter) SetTrace(tw *TraceWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trace = tw
}

// SetDeliveryErrorHook registers an optional callback invoked when a target fails.
func (r *Reporter) SetDeliveryErrorHook(fn func(targetName string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onErrorLog = fn
}

// AddTarget registers a delivery target. Call before Write.
func (r *Reporter) AddTarget(t Target) {
	r.targets = append(r.targets, t)
}

// Attach subscribes the reporter to visit, asset, queue, scout, and lifecycle events on bus.
func (r *Reporter) Attach(bus *events.Bus) {
	r.unsub = bus.Subscribe(events.Filter(r.handleEvent,
		events.VisitComplete, events.VisitError,
		events.AssetComplete, events.AssetError,
		events.WaitingRoomQueued, events.WaitingRoom,
		events.QueuePoll,
		events.ScoutVisit,
		events.LemmingBorn, events.LemmingDied,
	))
}

func (r *Reporter) handleEvent(e events.Event) {
	switch e.Kind {
	case events.VisitComplete, events.VisitError:
		if v, ok := e.Payload.(*swarm.Visit); ok && v != nil {
			r.RecordVisit(v)
		}
	case events.AssetComplete, events.AssetError:
		var av *swarm.AssetVisit
		if p, ok := e.Payload.(*swarm.AssetVisit); ok && p != nil {
			av = p
		} else {
			av = &swarm.AssetVisit{
				LemmingID:     e.LemmingID,
				LemmingNumber: e.LemmingNumber,
				URL:           e.URL,
				Class:         e.AssetClass,
				StatusCode:    e.StatusCode,
				BytesIn:       e.BytesIn,
				Duration:      e.Duration,
				Failed:        e.Failed || e.Kind == events.AssetError,
				Denied403:     e.Banned || e.StatusCode == http.StatusForbidden,
				Timestamp:     e.OccurredAt,
				FinishedAt:    e.OccurredAt,
			}
			if e.Err != nil {
				av.ErrorKind = swarm.ErrorKind(e.Err)
			}
		}
		r.recordAsset(av, e.AdmitPass)
	case events.WaitingRoomQueued:
		r.mu.Lock()
		if e.LemmingID == "" {
			r.timeline.bucket(e.OccurredAt.Sub(r.startedAt)).QueueEntered++
		} else if _, already := r.queuedLemmings[e.LemmingID]; !already {
			r.queuedLemmings[e.LemmingID] = struct{}{}
			r.timeline.bucket(e.OccurredAt.Sub(r.startedAt)).QueueEntered++
		}
		r.mu.Unlock()
	case events.WaitingRoom:
		r.mu.Lock()
		if e.LemmingID == "" {
			r.timeline.bucket(e.OccurredAt.Sub(r.startedAt)).QueueExited++
		} else if _, wasQueued := r.queuedLemmings[e.LemmingID]; wasQueued {
			delete(r.queuedLemmings, e.LemmingID)
			r.timeline.bucket(e.OccurredAt.Sub(r.startedAt)).QueueExited++
		}
		r.mu.Unlock()
	case events.QueuePoll:
		r.mu.Lock()
		r.roomPollEvents++
		r.mu.Unlock()
	case events.ScoutVisit:
		r.recordScoutVisit(e)
	case events.LemmingBorn, events.LemmingDied:
		r.mu.Lock()
		b := r.timeline.bucket(e.OccurredAt.Sub(r.startedAt))
		if e.Kind == events.LemmingBorn {
			b.Born++
		} else {
			b.Died++
			if _, wasQueued := r.queuedLemmings[e.LemmingID]; wasQueued {
				delete(r.queuedLemmings, e.LemmingID)
				b.QueueExited++
			}
		}
		r.mu.Unlock()
	}
}

// MarkStarted sets the start timestamp of the run.
func (r *Reporter) MarkStarted(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startedAt = at
}

// MarkEnded records run completion and swarm-level lemming counters.
func (r *Reporter) MarkEnded(at time.Time, m *swarm.Metrics) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endedAt = at
	if m != nil {
		r.started = m.LemmingsStarted.Load()
		r.completed = m.LemmingsCompleted.Load()
		r.dropped = m.DroppedLogs.Load()
	}
}

// RecordAsset folds one completed same-origin asset fetch into AssetReport and
// the trace stream without mixing into page visit totals or page latencies.
func (r *Reporter) RecordAsset(a *swarm.AssetVisit) {
	if a == nil {
		return
	}
	r.recordAsset(a, false)
}

func (r *Reporter) recordAsset(a *swarm.AssetVisit, admitPass bool) {
	class := normalizeAssetClass(a.Class)
	denied403 := a.Denied403 || a.StatusCode == http.StatusForbidden

	r.mu.Lock()
	r.assetRequests++
	r.assetBytes += a.BytesIn
	if a.Failed {
		r.assetFailed++
	}
	if denied403 {
		r.assetDenied403++
	}
	r.assetLatency.Add(a.Duration)

	cs := r.assetsByClass[class]
	if cs == nil {
		cs = &assetClassStats{Class: class, LatencySeries: LatencySeries{exactCap: exactPathSamples}}
		r.assetsByClass[class] = cs
	}
	cs.Requests++
	cs.Bytes += a.BytesIn
	if a.Failed {
		cs.Failed++
	}
	if denied403 {
		cs.Denied403++
	}
	cs.Add(a.Duration)
	tw := r.trace
	r.mu.Unlock()

	if tw != nil {
		tw.RecordTrace(traceOfAsset(a, class, admitPass))
	}
}

func (r *Reporter) recordScoutVisit(e events.Event) {
	r.mu.Lock()
	r.scoutVisits++
	if e.Failed {
		r.scoutFailures++
	}
	tw := r.trace
	r.mu.Unlock()

	if tw != nil {
		tw.RecordTrace(traceOfScout(e))
	}
}

// RecordVisit folds one completed visit into all aggregates and the trace stream.
func (r *Reporter) RecordVisit(v *swarm.Visit) {
	finished := v.FinishedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	r.mu.Lock()
	r.recordVisitLocked(v, finished)
	tw := r.trace
	r.mu.Unlock()
	if tw != nil {
		tw.Record(v)
	}
}

func (r *Reporter) recordVisitLocked(v *swarm.Visit, finished time.Time) {
	if !v.FinishedAt.IsZero() {
		finished = v.FinishedAt
	} else if finished.IsZero() {
		finished = time.Now()
	}
	failed := swarm.FailedVisit(v)

	key := discovery.SafeURL(v.URL)
	ps := r.byPath[key]
	if ps == nil {
		if len(r.byPath) >= maxReportPaths {
			key = otherPath
			ps = r.byPath[key]
		}
		if ps == nil {
			ps = &pathStats{URL: key, LatencySeries: LatencySeries{exactCap: exactPathSamples}}
			r.byPath[key] = ps
		}
	}
	ps.Hits++
	ps.Bytes += v.BytesIn
	if v.WaitingRoom.Detected {
		ps.waitingRoom++
	}

	r.visits++
	bucket := r.timeline.bucket(finished.Sub(r.startedAt))
	bucket.Visits++
	bucket.Bytes += v.BytesIn

	if v.Cancelled {
		r.cancelled++
		bucket.Cancelled++
	} else {
		if v.Error != nil {
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
		r.statusCodes[v.StatusCode]++
		bucket.Status[statusIndex(v.StatusCode)]++

		ps.Add(v.EffectiveLatency())
		r.latency.Add(v.EffectiveLatency())
		bucket.Latency.Add(v.EffectiveLatency())
		if v.Timing.TTFB > 0 {
			r.ttfb.Add(v.Timing.TTFB)
		}
		if failed {
			r.failed++
			bucket.Failed++
		}
		if v.ErrorKind != "" {
			r.errorKinds[v.ErrorKind]++
		} else if v.Error != nil {
			r.errorKinds[swarm.ErrorKind(v.Error)]++
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
		if v.Banned {
			switch {
			case v.StatusCode == http.StatusTooManyRequests:
				r.concertBans429++
			case v.StatusCode == http.StatusForbidden || v.WaitingRoom.Denied:
				r.concertBans403++
			}
		}
		if v.Role == swarm.RolePlan && v.PlanName != "" && v.StepIndex >= 1 {
			r.recordPlanStepLocked(v, failed)
		}
	}

	if v.ConcertAdmit {
		r.concertAdmitPasses++
	}
	if v.RoomTicket {
		r.concertRoomTickets++
	}
	if v.WaitingRoom.Rejected503 {
		r.roomRejected503++
	}
	if v.WaitingRoom.Detected {
		r.roomHeld++
		r.roomWait.Add(v.WaitingRoom.Duration)
		r.roomMaxPosition = max(r.roomMaxPosition, v.WaitingRoom.Position)
		if polls := max(v.WaitingRoom.Polls, v.QueuePolls); polls > 0 {
			r.roomVisitPolls += int64(polls)
		}
		if v.WaitingRoom.Admitted {
			r.roomAdmitted++
		}
		if v.WaitingRoom.Expired {
			r.roomExpired++
		}
		if v.WaitingRoom.Denied {
			r.roomDenied++
		}
		if !v.WaitingRoom.Admitted && !v.WaitingRoom.Expired && !v.WaitingRoom.Denied && !v.WaitingRoom.Rejected503 && (v.Error != nil || v.Cancelled) {
			r.roomDied++
		}
	}
}

func (r *Reporter) ensurePersonaLocked(name string, totalSteps int) *personaStats {
	if name == "" {
		return nil
	}
	ps := r.personas[name]
	if ps == nil {
		ps = &personaStats{
			Name:       name,
			Weight:     1,
			TotalSteps: totalSteps,
		}
		r.planOrder = append(r.planOrder, name)
		r.personas[name] = ps
	} else if totalSteps > ps.TotalSteps {
		ps.TotalSteps = totalSteps
	}
	return ps
}

func (r *Reporter) recordPlanStepLocked(v *swarm.Visit, failed bool) {
	ps := r.ensurePersonaLocked(v.PlanName, v.StepIndex)
	if ps == nil {
		return
	}
	idx := v.StepIndex - 1
	if idx < 0 {
		return
	}
	if idx >= len(ps.Steps) {
		if v.StepIndex > plan.DefaultMaxPlanItems {
			return
		}
		for len(ps.Steps) < v.StepIndex {
			stepIdx := len(ps.Steps) + 1
			ps.Steps = append(ps.Steps, &planStepStats{
				Index:          stepIdx,
				Name:           fmt.Sprintf("step-%d", stepIdx),
				Method:         http.MethodGet,
				FailureReasons: make(map[string]int64),
				LatencySeries:  LatencySeries{exactCap: exactPathSamples},
			})
		}
	}
	st := ps.Steps[idx]
	if v.Step != "" && (st.Name == "" || st.Name == fmt.Sprintf("step-%d", v.StepIndex)) {
		st.Name = v.Step
	}
	if v.Method != "" && st.Hits == 0 && r.cfg.Plan == nil {
		st.Method = strings.ToUpper(strings.TrimSpace(v.Method))
	}
	if st.Path == "" && v.URL != "" {
		st.Path = discovery.PathOf(v.URL)
	}
	st.Hits++
	st.Add(v.EffectiveLatency())
	if failed {
		st.Failed++
		if reason := swarm.WhyFailed(v); reason != "" {
			st.FailureReasons[reason]++
		}
	} else {
		st.Passed++
	}
}

func topFailure(reasons map[string]int64) string {
	var best string
	var bestCount int64
	for reason, n := range reasons {
		if n > bestCount || (n == bestCount && (best == "" || reason < best)) {
			best = reason
			bestCount = n
		}
	}
	return best
}

// Ingest records a completed lemming's LifeLog. Satisfies swarm.LifeSink.
func (r *Reporter) Ingest(ll swarm.LifeLog) {
	r.mu.Lock()
	defer r.mu.Unlock()

	role := ll.Role
	if role == "" {
		role = ll.Identity.Role
	}
	planName := ll.PlanName
	if planName == "" {
		planName = ll.Identity.PlanName
	}
	ll.Role = role
	ll.PlanName = planName

	if !ll.Streamed {
		ll.TotalVisits, ll.FailedVisits, ll.CancelledVisits = 0, 0, 0
		maxStepIdx := 0
		for i := range ll.Visits {
			v := &ll.Visits[i]
			finished := v.FinishedAt
			if finished.IsZero() {
				if !v.Timestamp.IsZero() {
					finished = v.Timestamp.Add(v.Duration)
				} else {
					finished = time.Now()
				}
			}
			rec := v
			if (v.Role == "" && role != "") || (v.PlanName == "" && planName != "") {
				vc := *v
				if vc.Role == "" {
					vc.Role = role
				}
				if vc.PlanName == "" {
					vc.PlanName = planName
				}
				rec = &vc
			}
			r.recordVisitLocked(rec, finished)
			ll.TotalVisits++
			switch {
			case v.Cancelled:
				ll.CancelledVisits++
			case swarm.FailedVisit(v):
				ll.FailedVisits++
				if ll.FirstFailureAt.IsZero() {
					ll.FirstFailureAt = v.Timestamp
				}
			}
			if !v.Cancelled {
				ll.SlowestVisit = max(ll.SlowestVisit, v.EffectiveLatency())
				if v.StepIndex > maxStepIdx {
					maxStepIdx = v.StepIndex
				}
			}
			ll.LongestWait = max(ll.LongestWait, v.WaitingRoom.Duration)
		}
		if role == swarm.RolePlan {
			if ll.PlanStepsCompleted == 0 && maxStepIdx > 0 {
				ll.PlanStepsCompleted = maxStepIdx
			}
			if ll.PlanStepsTotal == 0 {
				if ps := r.personas[planName]; ps != nil && ps.TotalSteps > 0 {
					ll.PlanStepsTotal = ps.TotalSteps
				} else if ll.PlanStepsCompleted > 0 {
					ll.PlanStepsTotal = ll.PlanStepsCompleted
				}
			}
			if !ll.PlanPassed {
				ll.PlanPassed = ll.Error == nil &&
					ll.FailedVisits == 0 &&
					ll.CancelledVisits == 0 &&
					ll.PlanStepsTotal > 0 &&
					ll.PlanStepsCompleted >= ll.PlanStepsTotal &&
					ll.ExitReason != swarm.ExitPlanFailed &&
					ll.ExitReason != swarm.ExitCancelled
			} else if ll.Error != nil || ll.FailedVisits > 0 || ll.CancelledVisits > 0 ||
				ll.ExitReason == swarm.ExitPlanFailed || ll.ExitReason == swarm.ExitCancelled {
				ll.PlanPassed = false
			}
		}
	}

	r.sessions++
	if ll.FailedVisits == 0 && ll.Error == nil {
		r.flawless++
	}
	switch role {
	case swarm.RolePlan:
		r.planLemmings++
		if ll.PlanPassed {
			r.planPassed++
		} else {
			r.planFailed++
		}
		if ps := r.ensurePersonaLocked(planName, ll.PlanStepsTotal); ps != nil {
			ps.Lemmings++
			if ll.PlanPassed {
				ps.Passed++
			} else {
				ps.Failed++
			}
		}
	case swarm.RoleRogue:
		r.rogueLemmings++
		if ll.FailedVisits == 0 && ll.Error == nil {
			r.rogueFlawless++
		}
	}
	if ll.ExitReason != "" {
		r.exitReasons[ll.ExitReason]++
	}
	r.notable.consider(&ll)
}

// CloseTrace flushes and closes the optional trace writer.
func (r *Reporter) CloseTrace() {
	r.mu.Lock()
	tw := r.trace
	r.mu.Unlock()
	if tw != nil {
		tw.Close()
	}
}

// BuildFilename constructs the base report filename (without extension).
func (r *Reporter) BuildFilename() string {
	domain := discovery.DomainFromURL(r.cfg.Hit)
	if r.cfg.ReportName != "" {
		return r.cfg.ReportName
	}
	if r.cfg.UniqueReport {
		return fmt.Sprintf("lemmings.%s.%s", time.Now().UTC().Format(reportTimestampFormat), domain)
	}
	return fmt.Sprintf("lemmings.%s.%s", time.Now().Format(reportDateFormat), domain)
}

// Render builds all report formats (HTML, SMTP-safe HTML, Markdown, JSON, and
// standalone screenshot assets) without delivering them.
func (r *Reporter) Render() (Rendered, error) {
	r.mu.Lock()
	data := r.buildReportDataLocked()
	r.mu.Unlock()

	return RenderData(data, r.BuildFilename(), discovery.DomainFromURL(r.cfg.Hit))
}

// Write renders the report and delivers it concurrently to every registered target.
func (r *Reporter) Write(ctx context.Context) error {
	r.CloseTrace()
	if r.unsub != nil {
		r.unsub()
	}

	rendered, err := r.Render()
	if err != nil {
		return err
	}

	if len(r.targets) == 0 {
		return nil
	}

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(r.targets))
	for _, target := range r.targets {
		t := target
		go func() {
			results <- result{name: t.Name(), err: t.Deliver(ctx, rendered)}
		}()
	}

	r.mu.Lock()
	errHook := r.onErrorLog
	r.mu.Unlock()

	var errs []string
	for range r.targets {
		res := <-results
		if res.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", res.name, res.err))
			if errHook != nil {
				errHook(res.name, res.err)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("report delivery errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Result returns the computed Data snapshot.
func (r *Reporter) Result() Data {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buildReportDataLocked()
}

// Live returns the once-a-second aggregate snapshot for the dashboard & ticker.
func (r *Reporter) Live(points, paths int) LiveStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := LiveStats{
		Visits:    r.visits,
		Failed:    r.failed,
		Cancelled: r.cancelled,
		P50:       r.latency.hist.Quantile(50),
		P95:       r.latency.hist.Quantile(95),
		P99:       r.latency.hist.Quantile(99),
		TTFBP95:   r.ttfb.Quantile(95),
		Sessions:  r.sessions,
		Rescued:   r.flawless,
		Assets:    r.buildAssetReportLocked(true),
		Room:      r.buildRoomReportLocked(),
		Concert:   r.buildConcertReportLocked(),
		Scouts:    r.buildScoutReportLocked(),
		Plan:      r.buildPlanReportLocked(true),
	}
	if completed := r.visits - r.cancelled; completed > 0 {
		s.FailureRate = float64(r.failed) / float64(completed)
	}
	if r.sessions > 0 {
		s.RescuedPercent = 100 * float64(r.flawless) / float64(r.sessions)
	}
	if points > 0 {
		s.Timeline = r.timeline.Points(points)
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
				URL: discovery.PathOf(ps.URL), Hits: ps.Hits, Failed: ps.failed,
				XX2: ps.xx2, XX3: ps.xx3, XX4: ps.xx4, XX5: ps.xx5,
				P50: ps.hist.Quantile(50), P95: ps.hist.Quantile(95), P99: ps.hist.Quantile(99),
				HitShare: float64(ps.Hits) / float64(max(all[0].Hits, 1)),
			})
			if ps.URL == otherPath {
				s.Paths[i].URL = otherPath
			}
		}
		s.StatusCodes = countRows(r.statusCodes, strconv.Itoa)
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

func (r *Reporter) buildReportDataLocked() Data {
	end := r.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	cfg := r.cfg
	cfg.Hit = discovery.SafeURL(cfg.Hit)

	totalLemmings := r.cfg.Terrain * r.cfg.Pack
	data := Data{
		SchemaVersion:     SchemaVersion,
		Version:           r.cfg.Version,
		GeneratedAt:       time.Now(),
		Duration:          end.Sub(r.startedAt).Round(time.Millisecond),
		Config:            cfg,
		TotalLemmings:     totalLemmings,
		LemmingsStarted:   r.started,
		LemmingsCompleted: r.completed,
		LemmingsUnborn:    max(totalLemmings-r.started, 0),
		TotalVisits:       r.visits,
		FailedVisits:      r.failed,
		CancelledVisits:   r.cancelled,
		Sessions:          r.sessions,
		Rescued:           r.flawless,
		ChecksumChanges:   r.checksumChanges,
		HTMLPages:         r.htmlPages,
		DroppedLogs:       r.dropped,
		Timeline:          r.timeline.Points(0),
		Notable:           r.notable.lives(),
		TraceFile:         r.cfg.TraceFile,
	}
	data.Plan = r.buildPlanReportLocked(false)
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
			Bytes:       FormatBytes(ps.Bytes),
			XX2:         ps.xx2,
			XX3:         ps.xx3,
			XX4:         ps.xx4,
			XX5:         ps.xx5,
			WaitingRoom: ps.waitingRoom,
			Errors:      ps.errors,
			Failed:      ps.failed,
			P50:         ps.Quantile(50),
			P95:         ps.Quantile(95),
			P99:         ps.Quantile(99),
		})
	}
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

	if r.latency.hist.count > 0 {
		data.Fastest = r.latency.hist.min
		data.Slowest = r.latency.hist.max
		data.Mean = r.latency.hist.Mean()
		data.P50 = r.latency.Quantile(50)
		data.P90 = r.latency.Quantile(90)
		data.P95 = r.latency.Quantile(95)
		data.P99 = r.latency.Quantile(99)
	}
	data.TTFBP50 = r.ttfb.Quantile(50)
	data.TTFBP95 = r.ttfb.Quantile(95)
	data.PercentileMethod = "exact (nearest rank)"
	if !r.latency.IsExact() {
		data.PercentileMethod = fmt.Sprintf("log histogram, within %.1f%% of the true value", (histogramGrowth-1)*100)
	}

	data.Assets = r.buildAssetReportLocked(false)
	data.Room = r.buildRoomReportLocked()
	data.Concert = r.buildConcertReportLocked()
	data.Scouts = r.buildScoutReportLocked()

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
		data.TraceError = r.trace.Failure()
	}

	if r.forensics != nil {
		arts, stats := r.forensics.SnapshotForReport()
		data.Forensics = ForensicsReport{
			Stats:     stats,
			Artifacts: arts,
		}
	}

	data.Gates, data.Verdict = r.evaluateGates(data)
	return data
}

func (r *Reporter) buildAssetReportLocked(live bool) AssetReport {
	ar := AssetReport{
		Requests:  r.assetRequests,
		Failed:    r.assetFailed,
		Denied403: r.assetDenied403,
		Bytes:     r.assetBytes,
	}
	if r.assetLatency.hist.count > 0 {
		if live {
			ar.P50 = r.assetLatency.hist.Quantile(50)
			ar.P95 = r.assetLatency.hist.Quantile(95)
			ar.P99 = r.assetLatency.hist.Quantile(99)
		} else {
			ar.P50 = r.assetLatency.Quantile(50)
			ar.P95 = r.assetLatency.Quantile(95)
			ar.P99 = r.assetLatency.Quantile(99)
		}
	}
	if len(r.assetsByClass) > 0 {
		rows := make([]AssetClassRow, 0, len(r.assetsByClass))
		for _, cs := range r.assetsByClass {
			var p95 time.Duration
			if cs.hist.count > 0 {
				if live {
					p95 = cs.hist.Quantile(95)
				} else {
					p95 = cs.Quantile(95)
				}
			}
			rows = append(rows, AssetClassRow{
				Class:     cs.Class,
				Requests:  cs.Requests,
				Failed:    cs.Failed,
				Denied403: cs.Denied403,
				Bytes:     cs.Bytes,
				P95:       p95,
			})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Requests != rows[j].Requests {
				return rows[i].Requests > rows[j].Requests
			}
			return rows[i].Class < rows[j].Class
		})
		ar.ByClass = rows
	}
	return ar
}

func (r *Reporter) buildRoomReportLocked() RoomReport {
	return RoomReport{
		Held:        r.roomHeld,
		Admitted:    r.roomAdmitted,
		DiedInLine:  r.roomDied,
		Expired:     r.roomExpired,
		Denied:      r.roomDenied,
		Rejected503: r.roomRejected503,
		TotalPolls:  max(r.roomPollEvents, r.roomVisitPolls),
		MaxPosition: r.roomMaxPosition,
		MeanWait:    r.roomWait.Mean(),
		P95Wait:     r.roomWait.Quantile(95),
		LongestWait: r.roomWait.max,
	}
}

func (r *Reporter) buildConcertReportLocked() ConcertReport {
	return ConcertReport{
		AdmitPasses: r.concertAdmitPasses,
		RoomTickets: r.concertRoomTickets,
		Bans429:     r.concertBans429,
		Bans403:     r.concertBans403,
	}
}

func (r *Reporter) buildScoutReportLocked() ScoutReport {
	return ScoutReport{
		Visits:   r.scoutVisits,
		Failures: r.scoutFailures,
	}
}

func (r *Reporter) buildPlanReportLocked(live bool) *PlanReport {
	if r.cfg.Plan == nil && r.planLemmings == 0 && r.rogueLemmings == 0 && len(r.planOrder) == 0 {
		return nil
	}
	rogueRate := r.cfg.RogueRate
	if rogueRate < 0 {
		rogueRate = 0
	}
	pr := &PlanReport{
		RogueRate:     rogueRate,
		PlanLemmings:  r.planLemmings,
		PlanPassed:    r.planPassed,
		PlanFailed:    r.planFailed,
		RogueLemmings: r.rogueLemmings,
		RogueFlawless: r.rogueFlawless,
	}
	if r.planLemmings > 0 {
		pr.PlanPassRate = 100 * float64(r.planPassed) / float64(r.planLemmings)
	}
	if len(r.planOrder) > 0 {
		pr.Personas = make([]PersonaReport, 0, len(r.planOrder))
		for _, name := range r.planOrder {
			ps := r.personas[name]
			if ps == nil {
				continue
			}
			pRep := PersonaReport{
				Name:       ps.Name,
				Weight:     ps.Weight,
				TotalSteps: ps.TotalSteps,
				Lemmings:   ps.Lemmings,
				Passed:     ps.Passed,
				Failed:     ps.Failed,
			}
			if ps.Lemmings > 0 {
				pRep.PassRate = 100 * float64(ps.Passed) / float64(ps.Lemmings)
			}
			if len(ps.Steps) > 0 {
				pRep.Steps = make([]PlanStepReport, 0, len(ps.Steps))
				for _, st := range ps.Steps {
					sr := PlanStepReport{
						Index:      st.Index,
						Name:       st.Name,
						Method:     st.Method,
						Path:       st.Path,
						Hits:       st.Hits,
						Passed:     st.Passed,
						Failed:     st.Failed,
						TopFailure: topFailure(st.FailureReasons),
					}
					if st.hist.count > 0 {
						if live {
							sr.P50 = st.hist.Quantile(50)
							sr.P95 = st.hist.Quantile(95)
							sr.P99 = st.hist.Quantile(99)
						} else {
							sr.P50 = st.Quantile(50)
							sr.P95 = st.Quantile(95)
							sr.P99 = st.Quantile(99)
						}
					}
					pRep.Steps = append(pRep.Steps, sr)
				}
			}
			pr.Personas = append(pr.Personas, pRep)
		}
	}
	return pr
}

func normalizeAssetClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "style", "script", "image", "font", "audio", "video", "data":
		return strings.ToLower(strings.TrimSpace(class))
	default:
		return "other"
	}
}

func (r *Reporter) evaluateGates(data Data) ([]GateResult, string) {
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
			Observed: FormatMillis(data.P95),
			Passed:   data.P95 <= r.cfg.P95Budget,
		})
	}
	if r.cfg.PlanPassGate {
		var obsShare float64
		if data.Plan != nil && data.Plan.PlanLemmings > 0 {
			obsShare = float64(data.Plan.PlanPassed) / float64(data.Plan.PlanLemmings)
		}
		gates = append(gates, GateResult{
			Name:     "plan pass rate",
			Limit:    fmt.Sprintf("≥ %.2f%%", r.cfg.MinPlanPassRate*100),
			Observed: fmt.Sprintf("%.2f%%", obsShare*100),
			Passed:   data.Plan != nil && data.Plan.PlanLemmings > 0 && obsShare >= r.cfg.MinPlanPassRate,
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

func anyFailed(gates []GateResult) bool {
	for _, g := range gates {
		if !g.Passed {
			return true
		}
	}
	return false
}

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

// ── Rendering ────────────────────────────────────────────────────────────────

type htmlTemplateModel struct {
	Data
	InlineImages    map[string]htmltemplate.URL
	OmitScreenshots bool
}

type mdTemplateModel struct {
	Data
	OmitScreenshots bool
}

// RenderData renders data into Markdown, self-contained HTML, SMTP-safe HTML,
// JSON, standalone screenshot assets, and screenshot-free remote variants.
func RenderData(data Data, filename, domain string) (Rendered, error) {
	if domain == "" {
		domain = discovery.DomainFromURL(data.Config.Hit)
	}

	// Build standalone assets & populate relative image_path for Markdown/JSON.
	var assets []ArtifactAsset
	fullInline := make(map[string]htmltemplate.URL)
	emailInline := make(map[string]htmltemplate.URL)
	var fullBudget, emailBudget int

	for i := range data.Forensics.Artifacts {
		a := &data.Forensics.Artifacts[i]
		if a.Screenshot == nil || len(a.Screenshot.Data) == 0 {
			continue
		}
		ext := a.Screenshot.Extension()
		assetName := a.ID + ext
		a.Screenshot.RelativePath = filename + ".artifacts/" + assetName
		assets = append(assets, ArtifactAsset{
			Filename: assetName,
			MIMEType: a.Screenshot.MIMEType,
			Data:     a.Screenshot.Data,
		})

		if uri, ok := safeImageDataURI(a.Screenshot); ok {
			size := len(a.Screenshot.Data)
			if fullBudget+size <= maxHTMLInlineImageBytes {
				fullInline[a.ID] = uri
				fullBudget += size
			}
			if emailBudget+size <= maxEmailInlineImageBytes {
				emailInline[a.ID] = uri
				emailBudget += size
			}
		}
	}

	md, err := renderMarkdown(mdTemplateModel{Data: data})
	if err != nil {
		return Rendered{}, fmt.Errorf("render markdown: %w", err)
	}
	htmlFull, err := renderHTML(htmlTemplateModel{Data: data, InlineImages: fullInline})
	if err != nil {
		return Rendered{}, fmt.Errorf("render html: %w", err)
	}
	htmlEmail := htmlFull
	if len(emailInline) != len(fullInline) {
		htmlEmail, err = renderHTML(htmlTemplateModel{Data: data, InlineImages: emailInline})
		if err != nil {
			return Rendered{}, fmt.Errorf("render email html: %w", err)
		}
	}
	js, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return Rendered{}, fmt.Errorf("render json: %w", err)
	}

	remoteMD := md
	remoteHTML := htmlFull
	remoteJSON := js
	if len(assets) > 0 {
		remoteData := cloneDataWithoutScreenshotPaths(data)
		remoteMD, err = renderMarkdown(mdTemplateModel{Data: remoteData, OmitScreenshots: true})
		if err != nil {
			return Rendered{}, fmt.Errorf("render remote markdown: %w", err)
		}
		remoteHTML, err = renderHTML(htmlTemplateModel{Data: remoteData, OmitScreenshots: true})
		if err != nil {
			return Rendered{}, fmt.Errorf("render remote html: %w", err)
		}
		remoteJSON, err = json.MarshalIndent(remoteData, "", "  ")
		if err != nil {
			return Rendered{}, fmt.Errorf("render remote json: %w", err)
		}
	}

	return Rendered{
		Filename:       filename,
		Domain:         domain,
		Markdown:       md,
		HTML:           htmlFull,
		HTMLForEmail:   htmlEmail,
		JSON:           js,
		Artifacts:      assets,
		RemoteMarkdown: remoteMD,
		RemoteHTML:     remoteHTML,
		RemoteJSON:     remoteJSON,
	}, nil
}

func cloneDataWithoutScreenshotPaths(data Data) Data {
	if len(data.Forensics.Artifacts) == 0 {
		return data
	}
	arts := make([]forensics.Artifact, len(data.Forensics.Artifacts))
	copy(arts, data.Forensics.Artifacts)
	for i := range arts {
		if arts[i].Screenshot != nil {
			sc := *arts[i].Screenshot
			sc.Data = nil
			sc.RelativePath = ""
			arts[i].Screenshot = &sc
		}
	}
	data.Forensics.Artifacts = arts
	return data
}

func safeImageDataURI(sc *forensics.Screenshot) (htmltemplate.URL, bool) {
	if sc == nil || len(sc.Data) == 0 {
		return "", false
	}
	mimeType := strings.ToLower(sc.MIMEType)
	if mimeType != "image/jpeg" && mimeType != "image/png" {
		return "", false
	}
	encoded := base64.StdEncoding.EncodeToString(sc.Data)
	return htmltemplate.URL("data:" + mimeType + ";base64," + encoded), true
}

var templateFuncs = map[string]any{
	"formatInt": FormatInt,
	"formatBytesInt": func(n any) string {
		switch v := n.(type) {
		case int64:
			return FormatBytes(v)
		case int:
			return FormatBytes(int64(v))
		default:
			return "0 B"
		}
	},
	"ms":          FormatMillis,
	"pct":         func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) },
	"pct100":      func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
	"width":       func(f float64) string { return fmt.Sprintf("%.1f%%", math.Max(f*100, 0.5)) },
	"float1":      func(f float64) string { return fmt.Sprintf("%.1f", f) },
	"statusClass": StatusClass,
	"atoi":        func(s string) int { n, _ := strconv.Atoi(s); return n },
	"sparkline":   Sparkline,
	"add":         func(a, b int) int { return a + b },
	"round":       RoundDuration,
	"lang":        func(s string) string { tag, _, _ := strings.Cut(s, ","); return tag },
	"shortID":     func(id string) string { return discovery.Clip(id, 8) },
	"rescueLine":  RescueLine,
	"join":        strings.Join,
	"upper":       strings.ToUpper,
	"mdCell":      mdCell,
	"mdFence":     mdFence,
	"planNames":   planNames,
	"methodClass": methodClass,
}

func planNames(pr *PlanReport) string {
	if pr == nil || len(pr.Personas) == 0 {
		return ""
	}
	names := make([]string, 0, len(pr.Personas))
	for _, p := range pr.Personas {
		if p.Name != "" {
			names = append(names, p.Name)
		}
	}
	return strings.Join(names, ", ")
}

func methodClass(m string) string {
	switch strings.ToUpper(strings.TrimSpace(m)) {
	case http.MethodPost:
		return "post"
	case http.MethodPut:
		return "put"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		return "delete"
	case http.MethodHead:
		return "head"
	case http.MethodOptions:
		return "options"
	default:
		return "get"
	}
}

func mdCell(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	return s
}

func mdFence(s string) string {
	maxRun := 0
	cur := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			if cur > maxRun {
				maxRun = cur
			}
		} else {
			cur = 0
		}
	}
	n := 3
	if maxRun >= n {
		n = maxRun + 1
	}
	return strings.Repeat("`", n)
}

var (
	markdownTemplate = template.Must(template.New("report.md.tmpl").Funcs(templateFuncs).
				ParseFS(reportTemplates, "templates/report.md.tmpl"))
	htmlTemplate = htmltemplate.Must(htmltemplate.New("report.html.tmpl").
			Funcs(templateFuncs).
			Funcs(htmltemplate.FuncMap{"timelineSVG": TimelineSVG}).
			ParseFS(reportTemplates, "templates/report.html.tmpl"))
)

func renderMarkdown(model mdTemplateModel) (string, error) {
	var buf bytes.Buffer
	if err := markdownTemplate.Execute(&buf, model); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderHTML(model htmlTemplateModel) (string, error) {
	var buf bytes.Buffer
	if err := htmlTemplate.Execute(&buf, model); err != nil {
		return "", err
	}
	return buf.String(), nil
}
