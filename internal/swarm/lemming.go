package swarm

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
)

var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Safari/605.1.15",
	"Mozilla/5.0 (X11; Linux x86_64; rv:125.0) Gecko/20100101 Firefox/125.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 Edg/124.0.0.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_4_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Mobile/15E148 Safari/604.1",
	"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36",
}

var acceptLanguages = []string{
	"en-US,en;q=0.9",
	"en-US,en;q=0.9",
	"en-US,en;q=0.9",
	"en-GB,en;q=0.8",
	"de-DE,de;q=0.9,en;q=0.6",
	"fr-FR,fr;q=0.9,en;q=0.5",
	"es-ES,es;q=0.9,en;q=0.5",
	"ja-JP,ja;q=0.9,en;q=0.4",
}

const (
	waitingRoomSignature    = `/queue/status`
	concertWaitingJSMarker  = `/_concert/waiting.js`
	concertQueueHeader      = `X-Concert-Queue`
	concertStatusURLHeader  = `X-Concert-Status-Url`
	concertPositionHeader   = `X-Concert-Position`
	defaultQueueStatusURL   = `/queue/status`
	concertAdmitCookieName  = `concert_admit`
	roomTicketCookieName    = `room_ticket`
	lifeTailSize            = 40
	recentTrailCap          = 8
	failureBackoff          = 100 * time.Millisecond
	maxRetryAfter           = 5 * time.Minute
	maxForensicsBodyBytes   = 512 << 10
	maxQueueStatusBodyBytes = 64 << 10
	maxAssetCacheEntries    = 2048
)

// Waiting room detection modes reported on WaitingRoomSignal.Mode and WaitingRoomMetric.Mode.
const (
	WaitingRoomModeConcertHTML = "concert-html"
	WaitingRoomModeLegacyHTML  = "legacy-html"
	WaitingRoomModeConcertJSON = "concert-json"
)

var (
	waitingRoomPollInterval = 2 * time.Second
	waitingRoom429Backoff   = 3 * time.Second
	waitingRoomMaxBackoff   = 10 * time.Second

	errQueueCookiesRequired = errors.New("waiting room requires cookies")
	errQueueExpired         = errors.New("waiting room ticket expired or evicted")
	errQueueNotAdmitted     = errors.New("waiting room reload was not admitted")
	errQueueTimeout         = errors.New("waiting room wait exceeded lemming lifespan")
)

// Lemming cohort roles recorded on Identity.Role, Visit.Role, and LifeLog.Role.
const (
	RolePlan  = "plan"
	RoleRogue = "rogue"
)

// Exit reasons recorded on LifeLog.ExitReason.
const (
	ExitLifespan     = "lifespan"
	ExitPageLimit    = "page-limit"
	ExitPlanComplete = "plan-complete"
	ExitPlanFailed   = "plan-failed"
	ExitCancelled    = "cancelled"
)

// IsRogueLemming deterministically reports whether the 1-based global lemming
// number is assigned to the rogue exploration cohort at the given rate in [0,1].
func IsRogueLemming(number int64, rate float64) bool {
	if number <= 0 || rate <= 0 || math.IsNaN(rate) {
		return false
	}
	if rate >= 1 {
		return true
	}
	return int64(math.Floor(float64(number)*rate+1e-9)) > int64(math.Floor(float64(number-1)*rate+1e-9))
}

// CountRoguesUpTo returns the exact count of rogue lemmings among 1..n for rate.
func CountRoguesUpTo(n int64, rate float64) int64 {
	if n <= 0 || rate <= 0 || math.IsNaN(rate) {
		return 0
	}
	if rate >= 1 {
		return n
	}
	return int64(math.Floor(float64(n)*rate + 1e-9))
}

// Identity is a lemming's immutable persona for its entire lifespan.
type Identity struct {
	Number    int64     `json:"number"` // sequential global lemming number (#1..N)
	ID        string    `json:"id"`     // 32-char hex identifier
	Name      string    `json:"name"`   // friendly Synthwave name
	Terrain   int64     `json:"terrain"`
	Pack      int64     `json:"pack"`
	BornAt    time.Time `json:"born_at"`
	UserAgent string    `json:"user_agent"`
	Language  string    `json:"language"`
	Role      string    `json:"role,omitempty"`
	PlanName  string    `json:"plan_name,omitempty"`
}

// WaitingRoomSignal describes a detected waiting-room response across Concert HTML,
// legacy HTML (room / Neon Arcade), and Concert -api-json 429 JSON responses.
type WaitingRoomSignal struct {
	Detected   bool
	Mode       string // "concert-html", "legacy-html", "concert-json"
	Position   int
	StatusURL  string
	RetryAfter time.Duration
}

// WaitingRoomMetric captures a lemming's waiting room stay.
type WaitingRoomMetric struct {
	Detected    bool          `json:"detected"`
	Position    int           `json:"position"`
	EnteredAt   time.Time     `json:"entered_at"`
	ExitedAt    time.Time     `json:"exited_at"`
	Duration    time.Duration `json:"duration_ns"`
	Polls       int           `json:"polls,omitempty"`
	Admitted    bool          `json:"admitted,omitempty"`
	Expired     bool          `json:"expired,omitempty"`
	Denied      bool          `json:"denied,omitempty"`
	Rejected503 bool          `json:"rejected_503,omitempty"`
	Mode        string        `json:"mode,omitempty"`
	StatusURL   string        `json:"status_url,omitempty"`
}

// AssetVisit records a single same-origin subresource fetch made by a lemming
// when -assets is enabled. Assets are tracked separately from page visits.
type AssetVisit struct {
	LemmingID     string
	LemmingNumber int64
	PageURL       string
	URL           string
	Class         string
	StatusCode    int
	BytesIn       int64
	Duration      time.Duration
	Failed        bool
	Denied403     bool
	ErrorKind     string
	Timestamp     time.Time
	FinishedAt    time.Time
}

// Visit is the atomic unit of measurement — one page hit by one lemming.
type Visit struct {
	LemmingID     string
	LemmingNumber int64
	Sequence      int
	Method        string
	PlanName      string
	Step          string
	StepIndex     int
	Role          string
	URL           string
	FinalURL      string
	Referrer      string
	StatusCode    int
	BytesIn       int64
	Duration      time.Duration
	Latency       time.Duration
	Timing        RequestTiming
	Hops          []ResponseHop
	CookieNames   []string
	Checksum      string
	Expected      string
	Match         bool
	WaitingRoom   WaitingRoomMetric
	Page          plan.PageEvidence
	RetryAfter    time.Duration
	CaptchaHit    bool
	Error         error
	ErrorKind     string
	Failed        bool
	Cancelled     bool
	Timestamp     time.Time
	FinishedAt    time.Time
	QueuePolls    int
	ConcertAdmit  bool
	RoomTicket    bool
	Banned        bool
}

// EffectiveLatency returns server Latency, falling back to Duration when unset.
func (v *Visit) EffectiveLatency() time.Duration {
	if v.Latency > 0 {
		return v.Latency
	}
	return v.Duration
}

// FailedVisit reports whether v failed transport, status, or page checks.
// Cancelled visits are never counted as failures.
func FailedVisit(v *Visit) bool {
	switch {
	case v.Cancelled:
		return false
	case v.Failed:
		return true
	}
	return len(v.Page.Checks) == 0 && (v.Error != nil || v.StatusCode < 200 || v.StatusCode >= 400)
}

// WhyFailed explains a failed or cancelled visit in a short human-readable phrase.
func WhyFailed(v *Visit) string {
	switch {
	case v.Cancelled:
		return "life ended mid-visit"
	case v.ErrorKind != "":
		return v.ErrorKind
	case v.Error != nil:
		return ErrorKind(v.Error)
	case v.WaitingRoom.Denied:
		return "waiting room denied (403)"
	case v.WaitingRoom.Expired:
		return "waiting room ticket expired"
	}
	for _, c := range v.Page.Checks {
		if !c.Passed {
			return c.Name + ": " + c.Detail
		}
	}
	if FailedVisit(v) {
		return fmt.Sprintf("status %d", v.StatusCode)
	}
	return ""
}

// triggerReason returns a compact stable reason tag for forensics deduplication.
func triggerReason(v *Visit) string {
	if v.ErrorKind != "" {
		return "error:" + v.ErrorKind
	}
	if v.Error != nil {
		return "error:" + ErrorKind(v.Error)
	}
	for _, c := range v.Page.Checks {
		if !c.Passed {
			if c.Name == "status" && v.StatusCode > 0 {
				return fmt.Sprintf("http-%d", v.StatusCode)
			}
			return "check:" + c.Name
		}
	}
	if v.StatusCode >= 400 {
		return fmt.Sprintf("http-%d", v.StatusCode)
	}
	return "visit-failed"
}

// LifeLog is the final record of a lemming's life, sent to the swarm at death.
type LifeLog struct {
	Identity           Identity
	Terrain            int
	Pack               int
	Role               string
	PlanName           string
	PlanStepsCompleted int
	PlanStepsTotal     int
	PlanPassed         bool
	FinalCookieNames   []string
	Visits             []Visit
	Error              error
	BornAt             time.Time
	DiedAt             time.Time
	Duration           time.Duration
	TotalVisits        int64
	FailedVisits       int64
	CancelledVisits    int64
	OmittedVisits      int64
	SlowestVisit       time.Duration
	LongestWait        time.Duration
	FirstFailureAt     time.Time
	ExitReason         string
	Streamed           bool
}

// Lemming is a stateful navigating HTTP agent with its own cookie jar and persona.
type Lemming struct {
	identity     Identity
	cfg          Config
	pool         *discovery.URLPool
	client       *http.Client
	jar          *InspectingJar
	bus          *events.Bus
	metrics      *Metrics
	forensics    *forensics.Coordinator
	rng          *rand.Rand
	assignedPlan *plan.Plan

	referrer   string
	links      []string
	trail      []string
	step       int
	assetCache map[string]bool
}

// NewLemming constructs a Lemming. client carries the lemming's private cookie jar
// and connection pool; jar and coord are optional (nil allowed in unit tests).
func NewLemming(
	number int64,
	terrain int64,
	pack int64,
	cfg Config,
	pool *discovery.URLPool,
	client *http.Client,
	jar *InspectingJar,
	bus *events.Bus,
	metrics *Metrics,
	coord *forensics.Coordinator,
) *Lemming {
	id := generateLemmingID(number, terrain, pack)
	seed, _ := strconv.ParseInt(id[:15], 16, 64)
	l := &Lemming{
		identity: Identity{
			Number:  number,
			ID:      id,
			Name:    LemmingName(id),
			Terrain: terrain,
			Pack:    pack,
			BornAt:  time.Now(),
		},
		cfg:        cfg,
		pool:       pool,
		client:     client,
		jar:        jar,
		bus:        bus,
		metrics:    metrics,
		forensics:  coord,
		rng:        rand.New(rand.NewSource(seed)),
		assetCache: make(map[string]bool),
	}
	l.identity.UserAgent = userAgents[l.rng.Intn(len(userAgents))]
	l.identity.Language = acceptLanguages[l.rng.Intn(len(acceptLanguages))]
	if cfg.Plan != nil && len(cfg.Plan.Plans) > 0 {
		rogueRate := cfg.RogueRate
		if rogueRate < 0 {
			rogueRate = 0.05
		}
		if IsRogueLemming(number, rogueRate) {
			l.identity.Role = RoleRogue
		} else {
			l.identity.Role = RolePlan
			cohortIdx := number - 1 - CountRoguesUpTo(number-1, rogueRate)
			if p := cfg.Plan.Pick(cohortIdx); p != nil {
				l.assignedPlan = p
				l.identity.PlanName = p.Name
			}
		}
	}
	return l
}

// Identity returns the lemming's immutable persona.
func (l *Lemming) Identity() Identity { return l.identity }

// Run executes the lemming's entire lifespan.
// Lifecycle events (Born/Died/Failed) are emitted exclusively by Terrain, never here.
func (l *Lemming) Run(parent context.Context) LifeLog {
	ctx, cancel := context.WithTimeout(parent, l.cfg.Until)
	defer cancel()

	stepsTotal := 0
	if l.assignedPlan != nil {
		stepsTotal = len(l.assignedPlan.Items)
	}
	ll := LifeLog{
		Identity:       l.identity,
		Terrain:        int(l.identity.Terrain),
		Pack:           int(l.identity.Pack),
		Role:           l.identity.Role,
		PlanName:       l.identity.PlanName,
		PlanStepsTotal: stepsTotal,
		BornAt:         time.Now(),
		ExitReason:     ExitLifespan,
		Streamed:       true,
	}
	tail := make([]Visit, 0, 16)
	head := 0

	for ctx.Err() == nil {
		if l.cfg.MaxPages > 0 && ll.TotalVisits >= int64(l.cfg.MaxPages) {
			ll.ExitReason = ExitPageLimit
			break
		}
		target, step, stepIdx, ok := l.next(ll.TotalVisits)
		if !ok {
			ll.ExitReason = ExitPlanComplete
			break
		}

		visit, links, rawBody, headers, contentType := l.hitStep(ctx, target, step, stepIdx)
		visit.Sequence = int(ll.TotalVisits) + 1
		l.remember(&ll, &visit, links)

		if visit.Failed && l.forensics != nil {
			l.submitForensics(&ll, &visit, step, rawBody, headers, contentType)
		}

		if len(tail) < lifeTailSize {
			tail = append(tail, visit)
		} else {
			tail[head] = visit
			head = (head + 1) % lifeTailSize
			ll.OmittedVisits++
		}

		if l.assignedPlan != nil && step != nil {
			if visit.Cancelled {
				break
			}
			ll.PlanStepsCompleted = max(ll.PlanStepsCompleted, stepIdx)
			if visit.Failed && step.ShouldStopOnFail(l.assignedPlan.StopOnFail) {
				ll.ExitReason = ExitPlanFailed
				break
			}
			if l.step >= len(l.assignedPlan.Items) {
				if l.assignedPlan.Loop {
					l.step = 0
					if !l.pauseAfterStep(ctx, step, &visit) {
						break
					}
					continue
				}
				if step.Chill != nil && *step.Chill > 0 {
					if !l.sleepContext(ctx, *step.Chill) {
						break
					}
				}
				ll.ExitReason = ExitPlanComplete
				break
			}
			if !l.pauseAfterStep(ctx, step, &visit) {
				break
			}
			continue
		}

		if !l.think(ctx, &visit) {
			break
		}
	}

	if parent.Err() != nil {
		ll.ExitReason = ExitCancelled
	}
	ll.Visits = append(tail[head:len(tail):len(tail)], tail[:head]...)
	ll.DiedAt = time.Now()
	ll.Duration = ll.DiedAt.Sub(ll.BornAt)
	ll.FinalCookieNames = l.finalCookieNames()
	ll.PlanPassed = ll.Role == RolePlan &&
		ll.Error == nil &&
		ll.FailedVisits == 0 &&
		ll.CancelledVisits == 0 &&
		ll.PlanStepsTotal > 0 &&
		ll.PlanStepsCompleted >= ll.PlanStepsTotal &&
		ll.ExitReason != ExitPlanFailed &&
		ll.ExitReason != ExitCancelled
	return ll
}

func (l *Lemming) pauseAfterStep(ctx context.Context, step *plan.PlanItem, v *Visit) bool {
	if step != nil && step.Chill != nil {
		if *step.Chill > 0 {
			return l.sleepContext(ctx, *step.Chill)
		}
		return ctx.Err() == nil
	}
	return l.think(ctx, v)
}

func (l *Lemming) finalCookieNames() []string {
	var cookieJar http.CookieJar
	if l.jar != nil {
		cookieJar = l.jar
	} else if l.client != nil && l.client.Jar != nil {
		cookieJar = l.client.Jar
	}
	if cookieJar == nil {
		return nil
	}
	hitURL, err := url.Parse(l.cfg.Hit)
	if err != nil || hitURL.Scheme == "" || hitURL.Host == "" {
		return nil
	}
	cookies := cookieJar.Cookies(hitURL)
	if len(cookies) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(cookies))
	names := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		if _, dup := seen[c.Name]; dup {
			continue
		}
		seen[c.Name] = struct{}{}
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return names
}

func (l *Lemming) submitForensics(ll *LifeLog, v *Visit, step *plan.PlanItem, body []byte, headers http.Header, contentType string) {
	targetURL := v.FinalURL
	if targetURL == "" {
		targetURL = v.URL
	}
	parsedURL, _ := url.Parse(targetURL)

	var jarSnap forensics.CookieJarSnapshot
	var replayCookies []forensics.ReplayCookie
	if l.jar != nil {
		jarSnap = l.jar.Snapshot(parsedURL, time.Now())
		replayCookies = l.jar.ReplayCookies(parsedURL)
	} else {
		jarSnap = forensics.CookieJarSnapshot{Health: "empty"}
	}

	bodyCopy := body
	if len(bodyCopy) > maxForensicsBodyBytes {
		bodyCopy = bodyCopy[:maxForensicsBodyBytes]
	}
	ownedBody := make([]byte, len(bodyCopy))
	copy(ownedBody, bodyCopy)

	navMode := l.cfg.Navigation
	planName := ""
	switch {
	case l.assignedPlan != nil:
		navMode = "plan"
		planName = l.assignedPlan.Name
	case l.identity.Role == RoleRogue:
		navMode = "rogue"
	}

	origin := ""
	if l.pool != nil {
		origin = l.pool.Origin
	}
	if origin == "" {
		origin, _ = discovery.ParseOrigin(l.cfg.Hit)
	}

	var skipBrowserReplay bool
	var skipBrowserReason string
	switch {
	case v.Method != "" && v.Method != http.MethodGet:
		skipBrowserReplay = true
		skipBrowserReason = "CDP replay skipped for non-GET request; captured HTTP-only forensics"
	case step != nil && step.HasRequestBody():
		skipBrowserReplay = true
		skipBrowserReason = "CDP replay skipped for request with body; captured HTTP-only forensics"
	case step != nil && len(step.WithHeaders) > 0:
		skipBrowserReplay = true
		skipBrowserReason = "CDP replay skipped for request with custom with-headers; captured HTTP-only forensics"
	}

	req := forensics.CaptureRequest{
		URL:               targetURL,
		SafeURL:           discovery.SafeURL(targetURL),
		Origin:            origin,
		Path:              discovery.PathOf(targetURL),
		Method:            v.Method,
		SkipBrowserReplay: skipBrowserReplay,
		SkipBrowserReason: skipBrowserReason,
		StatusCode:        v.StatusCode,
		LatencyMS:         float64(v.EffectiveLatency()) / float64(time.Millisecond),
		TriggerReason:     triggerReason(v),
		ContentType:       contentType,
		Headers:           headers,
		RawBody:           ownedBody,
		ReplayCookies:     replayCookies,
		Mobile:            IsMobileUA(l.identity.UserAgent),
		Session: forensics.SessionSnapshot{
			LemmingNumber:  l.identity.Number,
			LemmingID:      l.identity.ID,
			LemmingName:    l.identity.Name,
			Source:         "http-swarm",
			Terrain:        int(l.identity.Terrain),
			Pack:           int(l.identity.Pack),
			Persona:        PersonaOf(l.identity.UserAgent),
			UserAgent:      l.identity.UserAgent,
			Language:       l.identity.Language,
			NavigationMode: navMode,
			PlanName:       planName,
			PlanStep:       v.Step,
			Method:         v.Method,
			Role:           l.identity.Role,
			VisitSequence:  v.Sequence,
			SessionAgeMS:   float64(time.Since(ll.BornAt)) / float64(time.Millisecond),
			TotalVisits:    ll.TotalVisits,
			FailedVisits:   ll.FailedVisits,
			InWaitingRoom:  v.WaitingRoom.Detected,
			QueuePosition:  v.WaitingRoom.Position,
			Referrer:       discovery.SafeURL(v.Referrer),
			RecentTrail:    append([]string(nil), l.trail...),
			CookieJar:      jarSnap,
		},
	}
	l.forensics.Submit(req)
}

func (l *Lemming) next(visited int64) (string, *plan.PlanItem, int, bool) {
	if p := l.assignedPlan; p != nil {
		if l.step >= len(p.Items) {
			return "", nil, 0, false
		}
		step := &p.Items[l.step]
		stepIdx := l.step + 1
		l.step++
		return discovery.ResolveURL(l.cfg.Hit, step.Path), step, stepIdx, true
	}
	if l.cfg.Navigation == plan.NavigationLinks {
		if visited == 0 {
			return l.cfg.Hit, nil, 0, true
		}
		if len(l.links) > 0 {
			return l.links[l.rng.Intn(len(l.links))], nil, 0, true
		}
	}
	return l.pickURL(), nil, 0, true
}

func (l *Lemming) remember(ll *LifeLog, v *Visit, links []string) {
	ll.TotalVisits++
	switch {
	case v.Cancelled:
		ll.CancelledVisits++
	case v.Failed:
		ll.FailedVisits++
		if ll.FirstFailureAt.IsZero() {
			ll.FirstFailureAt = v.Timestamp
		}
	}
	if !v.Cancelled && v.EffectiveLatency() > ll.SlowestVisit {
		ll.SlowestVisit = v.EffectiveLatency()
	}
	if v.WaitingRoom.Duration > ll.LongestWait {
		ll.LongestWait = v.WaitingRoom.Duration
	}

	if l.metrics != nil {
		l.metrics.RecordVisit(v)
	}

	p := discovery.PathOf(v.URL)
	if len(l.trail) >= recentTrailCap {
		copy(l.trail, l.trail[1:])
		l.trail[recentTrailCap-1] = p
	} else {
		l.trail = append(l.trail, p)
	}

	kind := events.VisitComplete
	if v.Failed || v.Cancelled {
		kind = events.VisitError
	}
	if l.bus != nil {
		l.bus.Emit(events.Event{
			Kind:          kind,
			LemmingID:     l.identity.ID,
			LemmingNumber: l.identity.Number,
			Terrain:       int(l.identity.Terrain),
			Pack:          int(l.identity.Pack),
			Method:        v.Method,
			URL:           v.URL,
			StatusCode:    v.StatusCode,
			BytesIn:       v.BytesIn,
			Duration:      v.EffectiveLatency(),
			Failed:        v.Failed,
			Cancelled:     v.Cancelled,
			QueuePolls:    v.QueuePolls,
			Banned:        v.Banned,
			AdmitPass:     v.ConcertAdmit,
			PlanName:      v.PlanName,
			StepIndex:     v.StepIndex,
			Role:          v.Role,
			Payload:       v,
			Err:           v.Error,
		})
	}

	if !v.Cancelled && v.FinalURL != "" {
		l.referrer = v.FinalURL
		l.links = links
	}
}

func (l *Lemming) think(ctx context.Context, v *Visit) bool {
	d := l.cfg.ThinkMin
	if spread := l.cfg.ThinkMax - l.cfg.ThinkMin; spread > 0 {
		d += time.Duration(l.rng.Int63n(int64(spread) + 1))
	}
	if v.Failed && d < failureBackoff {
		d = failureBackoff
	}
	if (v.StatusCode == http.StatusTooManyRequests || v.StatusCode == http.StatusServiceUnavailable) && v.RetryAfter > d {
		d = v.RetryAfter
	}
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (l *Lemming) hit(ctx context.Context, targetURL string, step *plan.PlanItem) (Visit, []string, []byte, http.Header, string) {
	return l.hitStep(ctx, targetURL, step, 0)
}

func (l *Lemming) hitStep(ctx context.Context, targetURL string, step *plan.PlanItem, stepIdx int) (Visit, []string, []byte, http.Header, string) {
	expected := ""
	if l.pool != nil {
		expected = l.pool.Checksums[targetURL]
	}
	method := http.MethodGet
	if step != nil {
		method = step.EffectiveMethod()
	}
	visit := Visit{
		LemmingID:     l.identity.ID,
		LemmingNumber: l.identity.Number,
		Method:        method,
		PlanName:      l.identity.PlanName,
		StepIndex:     stepIdx,
		Role:          l.identity.Role,
		URL:           targetURL,
		Referrer:      l.referrer,
		Expected:      expected,
		Timestamp:     time.Now(),
	}
	if step != nil {
		visit.Step = step.Name
	}

	start := time.Now()
	resp := l.fetch(ctx, targetURL, step)
	resp.applyTo(&visit)
	visit.BytesIn = resp.bytes
	visit.Hops = resp.hops
	visit.Latency = resp.elapsed
	body := resp.body
	headers := resp.headers
	contentType := resp.contentType

	if parsedInitial, err := url.Parse(visit.FinalURL); err == nil {
		if l.hasActiveCookie(parsedInitial, roomTicketCookieName) {
			visit.RoomTicket = true
		}
	}

	if resp.err == nil {
		if sig, inRoom := DetectWaitingRoomResponse(resp.status, contentType, headers, body); inRoom {
			body, headers, contentType = l.waitInRoom(ctx, targetURL, step, &visit, sig, headers, contentType)
		} else {
			if resp.status == http.StatusServiceUnavailable && isConcertBreaker503(contentType, body) {
				visit.WaitingRoom.Rejected503 = true
			}
			visit.Checksum = discovery.SHA512Hex(body)
			visit.Match = visit.Checksum == visit.Expected
		}
	}

	visit.FinishedAt = time.Now()
	visit.Duration = time.Since(start)
	visit.QueuePolls = visit.WaitingRoom.Polls
	visit.ErrorKind = ErrorKind(visit.Error)
	visit.Cancelled = visit.Error != nil && ctx.Err() != nil && !errors.Is(visit.Error, errQueueTimeout)

	finalURL := visit.FinalURL
	if finalURL == "" {
		finalURL = targetURL
	}
	parsedFinal, _ := url.Parse(finalURL)
	if parsedFinal != nil {
		visit.ConcertAdmit = l.hasActiveCookie(parsedFinal, concertAdmitCookieName)
		if l.hasActiveCookie(parsedFinal, roomTicketCookieName) {
			visit.RoomTicket = true
		}
	}

	if !visit.Cancelled {
		if visit.WaitingRoom.Denied || visit.StatusCode == http.StatusForbidden {
			visit.Banned = true
		} else if visit.StatusCode == http.StatusTooManyRequests && !visit.WaitingRoom.Detected {
			visit.Banned = true
		}
	}

	var links []string
	if !visit.Cancelled && visit.Error == nil && !visit.WaitingRoom.Denied && !visit.WaitingRoom.Expired {
		var activeCookies []*http.Cookie
		if parsedFinal != nil {
			if l.jar != nil {
				activeCookies = l.jar.Cookies(parsedFinal)
			} else if l.client != nil && l.client.Jar != nil {
				activeCookies = l.client.Jar.Cookies(parsedFinal)
			}
		}
		visit.Page, links = plan.InspectResponse(plan.ResponseInput{
			Method:      visit.Method,
			Body:        body,
			ContentType: contentType,
			Status:      visit.StatusCode,
			Headers:     headers,
			Cookies:     activeCookies,
			Step:        step,
			Base:        finalURL,
		})
		links = filterSiteLinks(links)
		visit.Page.LinkCount = len(links)
		if l.cfg.StrictChecksum {
			visit.Page.Checks = append(visit.Page.Checks, plan.CheckResult{
				Name:   "checksum",
				Passed: visit.Expected != "" && visit.Match,
				Detail: "body must match the index-time checksum (-strict-checksum)",
			})
		}
	}
	visit.Failed = !visit.Cancelled && (visit.Failed || visit.Error != nil || visit.Page.FailedChecks() > 0)

	// Opt-in asset tier: fetch same-origin subresources after a successful HTML
	// page visit (including after waiting-room admission). Runs after Visit
	// timings are finalized so assets never mix into page latency or TotalVisits.
	if !visit.Cancelled && !visit.Failed && visit.Page.HTML && l.cfg.Assets && l.cfg.AssetsPerPage > 0 && parsedFinal != nil {
		l.loadPageAssets(ctx, parsedFinal, body)
	}

	return visit, links, body, headers, contentType
}

type queueStatusResponse struct {
	Ready           *bool `json:"ready"`
	Position        int   `json:"position"`
	Waiting         *bool `json:"waiting"`
	CookiesRequired bool  `json:"cookies_required"`
}

func (l *Lemming) waitInRoom(
	ctx context.Context,
	targetURL string,
	step *plan.PlanItem,
	visit *Visit,
	sig WaitingRoomSignal,
	lastHeaders http.Header,
	lastContentType string,
) ([]byte, http.Header, string) {
	pollURL, pollPath, ok := resolvePollURL(targetURL, sig.StatusURL)
	if !ok {
		visit.Error = ErrOffsiteRedirect
		visit.Failed = true
		return nil, lastHeaders, lastContentType
	}

	visit.WaitingRoom = WaitingRoomMetric{
		Detected:  true,
		Position:  sig.Position,
		EnteredAt: time.Now(),
		Mode:      sig.Mode,
		StatusURL: pollPath,
	}
	l.emitQueued(targetURL, sig.Position)

	defer func() {
		if visit.WaitingRoom.ExitedAt.IsZero() {
			visit.WaitingRoom.ExitedAt = time.Now()
		}
		visit.WaitingRoom.Duration = visit.WaitingRoom.ExitedAt.Sub(visit.WaitingRoom.EnteredAt)
		if errors.Is(visit.Error, errQueueTimeout) {
			visit.Latency = visit.WaitingRoom.Duration
		}
		visit.QueuePolls = visit.WaitingRoom.Polls
		if l.bus != nil {
			l.bus.Emit(events.Event{
				Kind:          events.WaitingRoom,
				LemmingID:     l.identity.ID,
				LemmingNumber: l.identity.Number,
				Terrain:       int(l.identity.Terrain),
				Pack:          int(l.identity.Pack),
				URL:           targetURL,
				Position:      visit.WaitingRoom.Position,
				QueuePolls:    visit.WaitingRoom.Polls,
				Duration:      visit.WaitingRoom.Duration,
				Banned:        visit.Banned,
				AdmitPass:     visit.ConcertAdmit,
			})
		}
	}()

	nextWait := waitingRoomPollInterval
	if sig.RetryAfter > 0 {
		nextWait = sig.RetryAfter
	}
	netFailures := 0
	lastPos := sig.Position

	for {
		if !l.sleepContext(ctx, nextWait) {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				visit.Error = errQueueTimeout
				visit.Failed = true
			} else {
				visit.Error = ctx.Err()
			}
			return nil, lastHeaders, lastContentType
		}

		pollBody, pollStatus, pollRetryAfter, pollDur, pollErr := l.pollQueueStatus(ctx, pollURL)
		visit.WaitingRoom.Polls++
		visit.QueuePolls = visit.WaitingRoom.Polls
		visit.Hops = AppendHops(visit.Hops, ResponseHop{
			URL:    discovery.SafeURL(pollURL),
			Status: pollStatus,
		})

		if pollErr != nil {
			l.emitQueuePoll(pollURL, pollStatus, lastPos, visit.WaitingRoom.Polls, pollDur, true, pollErr)
			if ctx.Err() != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					visit.Error = errQueueTimeout
					visit.Failed = true
				} else {
					visit.Error = ctx.Err()
				}
				return nil, lastHeaders, lastContentType
			}
			netFailures++
			nextWait = l.pollErrorBackoff(netFailures)
			continue
		}

		switch pollStatus {
		case http.StatusForbidden:
			l.emitQueuePoll(pollURL, pollStatus, lastPos, visit.WaitingRoom.Polls, pollDur, true, nil)
			visit.WaitingRoom.Denied = true
			visit.Banned = true
			visit.Failed = true
			visit.StatusCode = http.StatusForbidden
			visit.Error = nil
			return pollBody, lastHeaders, lastContentType

		case http.StatusTooManyRequests:
			l.emitQueuePoll(pollURL, pollStatus, lastPos, visit.WaitingRoom.Polls, pollDur, false, nil)
			base := max(pollRetryAfter, waitingRoom429Backoff)
			nextWait = base + l.pollJitter(base)
			continue
		}

		if pollStatus < 200 || pollStatus >= 300 {
			l.emitQueuePoll(pollURL, pollStatus, lastPos, visit.WaitingRoom.Polls, pollDur, true, nil)
			netFailures++
			nextWait = l.pollErrorBackoff(netFailures)
			continue
		}

		var qs queueStatusResponse
		if err := json.Unmarshal(pollBody, &qs); err != nil {
			l.emitQueuePoll(pollURL, pollStatus, lastPos, visit.WaitingRoom.Polls, pollDur, true, err)
			netFailures++
			nextWait = l.pollErrorBackoff(netFailures)
			continue
		}

		netFailures = 0
		pos := max(qs.Position, 0)
		l.emitQueuePoll(pollURL, pollStatus, pos, visit.WaitingRoom.Polls, pollDur, qs.CookiesRequired, nil)

		if qs.CookiesRequired {
			visit.Error = errQueueCookiesRequired
			visit.Failed = true
			return pollBody, lastHeaders, lastContentType
		}

		ready := false
		if qs.Ready != nil {
			ready = *qs.Ready
		} else if qs.Waiting == nil {
			// Legacy / Neon Arcade protocol omits "ready"; position == 0 means admitted.
			ready = qs.Position <= 0
		}

		if qs.Waiting != nil && !*qs.Waiting && !ready {
			visit.WaitingRoom.Expired = true
			visit.Failed = true
			visit.Error = errQueueExpired
			return pollBody, lastHeaders, lastContentType
		}

		if !ready {
			if pos > visit.WaitingRoom.Position {
				visit.WaitingRoom.Position = pos
			}
			if pos != lastPos {
				lastPos = pos
				l.emitQueued(targetURL, pos)
			}
			nextWait = waitingRoomPollInterval + l.pollJitter(waitingRoomPollInterval)
			continue
		}

		// Admitted! Record pure queue exit time BEFORE loading the origin page
		// so WaitingRoom.Duration and Visit.Latency stay cleanly separated.
		visit.WaitingRoom.ExitedAt = time.Now()

		reload := l.fetch(ctx, targetURL, step)
		visit.BytesIn += reload.bytes
		visit.Hops = AppendHops(visit.Hops, reload.hops...)
		reload.applyTo(visit)
		if reload.err != nil {
			return nil, reload.headers, reload.contentType
		}
		if reload.status == http.StatusForbidden {
			visit.WaitingRoom.Denied = true
			visit.Banned = true
			visit.Failed = true
			return reload.body, reload.headers, reload.contentType
		}
		if _, stillQueued := DetectWaitingRoomResponse(reload.status, reload.contentType, reload.headers, reload.body); stillQueued {
			visit.WaitingRoom.Admitted = false
			visit.Failed = true
			visit.Error = errQueueNotAdmitted
			return reload.body, reload.headers, reload.contentType
		}
		if reload.status == http.StatusServiceUnavailable {
			visit.WaitingRoom.Rejected503 = true
			return reload.body, reload.headers, reload.contentType
		}
		if reload.status == http.StatusTooManyRequests {
			visit.WaitingRoom.Admitted = false
			visit.Banned = true
			return reload.body, reload.headers, reload.contentType
		}

		visit.WaitingRoom.Admitted = true
		visit.Latency = reload.elapsed
		visit.Checksum = discovery.SHA512Hex(reload.body)
		visit.Match = visit.Checksum == visit.Expected
		if u, err := url.Parse(visit.FinalURL); err == nil {
			visit.ConcertAdmit = l.hasActiveCookie(u, concertAdmitCookieName)
		}
		return reload.body, reload.headers, reload.contentType
	}
}

func (l *Lemming) pollQueueStatus(ctx context.Context, pollURL string) ([]byte, int, time.Duration, time.Duration, error) {
	timeout := l.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, 0, 0, time.Since(start), fmt.Errorf("build queue poll request: %w", err)
	}
	req.Header.Set("User-Agent", l.identity.UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", l.identity.Language)

	client := *l.client
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !discovery.SameOrigin(pollURL, next.URL.String()) {
			return ErrOffsiteRedirect
		}
		if len(via) >= maxRedirects {
			return ErrTooManyRedirects
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		return nil, status, 0, time.Since(start), fmt.Errorf("queue poll request: %w", err)
	}
	defer resp.Body.Close()

	retryAfter := ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxQueueStatusBodyBytes))
	if readErr != nil {
		return body, resp.StatusCode, retryAfter, time.Since(start), fmt.Errorf("read queue status: %w", readErr)
	}
	return body, resp.StatusCode, retryAfter, time.Since(start), nil
}

func (l *Lemming) loadPageAssets(ctx context.Context, pageURL *url.URL, htmlBody []byte) {
	refs := plan.ExtractAssets(pageURL, htmlBody)
	if len(refs) == 0 {
		return
	}
	if l.assetCache == nil {
		l.assetCache = make(map[string]bool)
	}

	pageURLStr := pageURL.String()
	queue := make([]plan.AssetRef, 0, len(refs))
	seenInPage := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if _, dup := seenInPage[ref.URL]; dup {
			continue
		}
		seenInPage[ref.URL] = struct{}{}
		queue = append(queue, ref)
	}

	fetched := 0
	for len(queue) > 0 && fetched < l.cfg.AssetsPerPage && ctx.Err() == nil {
		ref := queue[0]
		queue = queue[1:]
		if l.assetCache[ref.URL] {
			continue
		}
		if !discovery.SameOrigin(pageURLStr, ref.URL) {
			continue
		}

		fetched++
		av, cssBody, contentType, finalAssetURL, fetchErr := l.fetchAsset(ctx, pageURLStr, ref)
		if fetchErr != nil && ctx.Err() != nil {
			// Parent lifespan ended mid-asset fetch; do not report as a server failure.
			return
		}

		if l.metrics != nil {
			l.metrics.RecordAsset(&av)
		}

		assetParsed, _ := url.Parse(finalAssetURL)
		if assetParsed == nil {
			assetParsed, _ = url.Parse(ref.URL)
		}
		admitPass := l.hasActiveCookie(assetParsed, concertAdmitCookieName)

		if l.bus != nil {
			kind := events.AssetComplete
			if av.Failed {
				kind = events.AssetError
			}
			l.bus.Emit(events.Event{
				Kind:          kind,
				LemmingID:     l.identity.ID,
				LemmingNumber: l.identity.Number,
				Terrain:       int(l.identity.Terrain),
				Pack:          int(l.identity.Pack),
				URL:           av.URL,
				StatusCode:    av.StatusCode,
				BytesIn:       av.BytesIn,
				Duration:      av.Duration,
				Failed:        av.Failed,
				AssetClass:    av.Class,
				Banned:        av.Denied403,
				AdmitPass:     admitPass,
				Payload:       &av,
				Err:           fetchErr,
			})
		}

		// Cache 2xx and 304 responses per lemming (capped at maxAssetCacheEntries).
		if !av.Failed && ((av.StatusCode >= 200 && av.StatusCode < 300) || av.StatusCode == http.StatusNotModified) {
			if len(l.assetCache) < maxAssetCacheEntries {
				l.assetCache[ref.URL] = true
			}
		}

		// For 200 CSS responses, discover and enqueue sub-assets (fonts, images,
		// @imports) within the same cfg.AssetsPerPage budget.
		if !av.Failed && av.StatusCode == http.StatusOK && len(cssBody) > 0 && isCSSAsset(contentType, av.Class) {
			cssBase := assetParsed
			if cssBase != nil {
				for _, sub := range plan.ExtractCSSAssets(cssBase, cssBody) {
					if l.assetCache[sub.URL] {
						continue
					}
					if _, dup := seenInPage[sub.URL]; dup {
						continue
					}
					seenInPage[sub.URL] = struct{}{}
					queue = append(queue, sub)
				}
			}
		}
	}
}

func (l *Lemming) fetchAsset(
	ctx context.Context,
	pageURL string,
	ref plan.AssetRef,
) (AssetVisit, []byte, string, string, error) {
	timeout := l.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	limit := l.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	av := AssetVisit{
		LemmingID:     l.identity.ID,
		LemmingNumber: l.identity.Number,
		PageURL:       pageURL,
		URL:           ref.URL,
		Class:         ref.Class,
		Timestamp:     start,
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ref.URL, nil)
	if err != nil {
		av.FinishedAt = time.Now()
		av.Duration = time.Since(start)
		av.Failed = true
		av.ErrorKind = ErrorKind(err)
		return av, nil, "", ref.URL, fmt.Errorf("build asset request: %w", err)
	}
	req.Header.Set("User-Agent", l.identity.UserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", l.identity.Language)
	if pageURL != "" {
		req.Header.Set("Referer", discovery.SafeURL(pageURL))
	}

	client := *l.client
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !discovery.SameOrigin(ref.URL, next.URL.String()) {
			return ErrOffsiteRedirect
		}
		if len(via) >= maxRedirects {
			return ErrTooManyRedirects
		}
		return nil
	}

	resp, err := client.Do(req)
	finalAssetURL := ref.URL
	contentType := ""
	var cssBody []byte

	if resp != nil {
		av.StatusCode = resp.StatusCode
		if resp.Request != nil && resp.Request.URL != nil {
			finalAssetURL = resp.Request.URL.String()
		}
		contentType = resp.Header.Get("Content-Type")
		if cls := plan.ClassifyAsset(finalAssetURL, contentType); cls != "other" || av.Class == "" {
			av.Class = cls
		}

		if err == nil {
			keepCSS := resp.StatusCode == http.StatusOK && isCSSAsset(contentType, av.Class)
			if keepCSS {
				var body []byte
				body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
				av.BytesIn = int64(len(body))
				if av.BytesIn > limit {
					av.BytesIn = limit
					body = body[:limit]
					if err == nil {
						err = ErrBodyTooLarge
					}
				}
				cssBody = body
			} else {
				var n int64
				n, err = io.Copy(io.Discard, io.LimitReader(resp.Body, limit+1))
				av.BytesIn = n
				if av.BytesIn > limit {
					av.BytesIn = limit
					if err == nil {
						err = ErrBodyTooLarge
					}
				}
			}

			if !errors.Is(err, ErrBodyTooLarge) &&
				(err == nil || errors.Is(err, io.ErrUnexpectedEOF)) &&
				resp.ContentLength >= 0 && !resp.Uncompressed && av.BytesIn < resp.ContentLength &&
				req.Method != http.MethodHead {
				err = fmt.Errorf("%w: read %d of %d bytes", ErrTruncatedBody, av.BytesIn, resp.ContentLength)
			}
			if err != nil && !errors.Is(err, ErrBodyTooLarge) && !errors.Is(err, ErrTruncatedBody) {
				err = fmt.Errorf("read asset body: %w", err)
			}
		}
		_ = resp.Body.Close()
	} else if err != nil {
		err = fmt.Errorf("do asset request: %w", err)
	}

	av.FinishedAt = time.Now()
	av.Duration = time.Since(start)
	if av.Class == "" {
		av.Class = plan.ClassifyAsset(finalAssetURL, contentType)
	}
	av.ErrorKind = ErrorKind(err)
	if av.StatusCode == http.StatusForbidden {
		av.Denied403 = true
	}
	if err != nil || av.StatusCode < 200 || av.StatusCode >= 400 {
		av.Failed = true
	}
	return av, cssBody, contentType, finalAssetURL, err
}

func (l *Lemming) hasActiveCookie(u *url.URL, name string) bool {
	if u == nil || name == "" {
		return false
	}
	if l.jar != nil {
		return l.jar.HasActiveCookie(u, name)
	}
	if l.client != nil && l.client.Jar != nil {
		for _, c := range l.client.Jar.Cookies(u) {
			if c != nil && c.Name == name && c.Value != "" {
				return true
			}
		}
	}
	return false
}

func (l *Lemming) sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (l *Lemming) pollJitter(base time.Duration) time.Duration {
	if base <= 0 || l.rng == nil {
		return 0
	}
	maxJitter := min(base/5, 500*time.Millisecond)
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(l.rng.Int63n(int64(maxJitter) + 1))
}

func (l *Lemming) pollErrorBackoff(failures int) time.Duration {
	shift := min(max(failures-1, 0), 4)
	base := waitingRoomPollInterval
	if base <= 0 {
		base = time.Second
	}
	d := base << shift
	if d > waitingRoomMaxBackoff || d <= 0 {
		d = waitingRoomMaxBackoff
	}
	return d + l.pollJitter(d)
}

func (l *Lemming) emitQueued(targetURL string, position int) {
	if l.bus == nil {
		return
	}
	l.bus.Emit(events.Event{
		Kind:          events.WaitingRoomQueued,
		LemmingID:     l.identity.ID,
		LemmingNumber: l.identity.Number,
		Terrain:       int(l.identity.Terrain),
		Pack:          int(l.identity.Pack),
		URL:           targetURL,
		Position:      position,
	})
}

func (l *Lemming) emitQueuePoll(pollURL string, status, position, polls int, dur time.Duration, failed bool, err error) {
	if l.bus == nil {
		return
	}
	l.bus.Emit(events.Event{
		Kind:          events.QueuePoll,
		LemmingID:     l.identity.ID,
		LemmingNumber: l.identity.Number,
		Terrain:       int(l.identity.Terrain),
		Pack:          int(l.identity.Pack),
		URL:           pollURL,
		StatusCode:    status,
		Position:      position,
		QueuePolls:    polls,
		Duration:      dur,
		Failed:        failed,
		Err:           err,
	})
}

func (l *Lemming) pickURL() string {
	if l.pool == nil || len(l.pool.URLs) == 0 {
		return l.cfg.Hit
	}
	n := len(l.pool.URLs)
	off := l.rng.Intn(n)
	for i := range n {
		candidate := l.pool.URLs[(off+i)%n]
		if !isConcertInternalURL(candidate) {
			return candidate
		}
	}
	return l.cfg.Hit
}

// DetectWaitingRoomResponse inspects an HTTP response for Concert and legacy
// waiting rooms (served with HTTP 2xx, 429, or 503, while leaving bare and
// breaker 503s undetected):
//  1. Explicit Concert header: X-Concert-Queue: 1 (or true), checked first so
//     copy/markup edits never break detection; honors Retry-After and optional
//     X-Concert-Status-Url / X-Concert-Position headers or JSON/HTML metadata.
//  2. Concert -api-json 429/503 JSON: {"queued": true, ...} (Mode: "concert-json").
//  3. Fallback HTML matching: Concert waiting.html (/_concert/waiting.js,
//     class="queue-page" + id="position", Set-Cookie room_ticket + waiting
//     markers, or "Your place is saved" page -> Mode: "concert-html") and
//     legacy Neon Arcade / room (/queue/status + id="position" -> Mode: "legacy-html").
func DetectWaitingRoomResponse(status int, contentType string, headers http.Header, body []byte) (WaitingRoomSignal, bool) {
	statusAllowed := status == 0 ||
		(status >= 200 && status < 300) ||
		status == http.StatusTooManyRequests ||
		status == http.StatusServiceUnavailable
	if !statusAllowed {
		return WaitingRoomSignal{}, false
	}

	var headerRetry time.Duration
	if headers != nil {
		headerRetry = ParseRetryAfter(headers.Get("Retry-After"), time.Now())
	}

	// (1) Explicit Concert queue header takes precedence over body heuristics.
	if headers != nil {
		if qv := strings.ToLower(strings.TrimSpace(headers.Get(concertQueueHeader))); qv == "1" || qv == "true" || qv == "yes" {
			return detectConcertHeaderSignal(contentType, headers, body, headerRetry)
		}
	}

	if len(body) == 0 {
		return WaitingRoomSignal{}, false
	}

	// (2) Concert -api-json 429/503 JSON queue response (breaker 503 has queued:false).
	if (status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable) && isJSONContentTypeOrEmpty(contentType) {
		if sig, ok := parseConcertJSONSignal(body, headerRetry, false); ok {
			return sig, true
		}
	}

	// (3) Fallback Concert HTML and Legacy HTML waiting rooms.
	if !isHTMLContentTypeOrEmpty(contentType) {
		return WaitingRoomSignal{}, false
	}

	s := string(body)
	hasPos := hasPositionMarker(s)

	// (a) Concert waiting.html.
	if strings.Contains(s, concertWaitingJSMarker) ||
		(hasPos && (strings.Contains(s, `class="queue-page"`) || strings.Contains(s, `class='queue-page'`))) ||
		(hasSetCookie(headers, roomTicketCookieName) && (hasPos || strings.Contains(s, "Your place is saved") || strings.Contains(s, waitingRoomSignature))) ||
		(strings.Contains(s, "Your place is saved") && (hasPos || strings.Contains(s, "Concert") || strings.Contains(s, "concert."))) {
		return WaitingRoomSignal{
			Detected:   true,
			Mode:       WaitingRoomModeConcertHTML,
			Position:   extractWaitingRoomPosition(s),
			StatusURL:  defaultQueueStatusURL,
			RetryAfter: headerRetry,
		}, true
	}

	// (b) Legacy room / Neon Arcade HTML.
	if hasPos && strings.Contains(s, waitingRoomSignature) {
		return WaitingRoomSignal{
			Detected:   true,
			Mode:       WaitingRoomModeLegacyHTML,
			Position:   extractWaitingRoomPosition(s),
			StatusURL:  defaultQueueStatusURL,
			RetryAfter: headerRetry,
		}, true
	}

	return WaitingRoomSignal{}, false
}

func detectConcertHeaderSignal(contentType string, headers http.Header, body []byte, headerRetry time.Duration) (WaitingRoomSignal, bool) {
	statusURL := sanitizeStatusURL(headers.Get(concertStatusURLHeader))
	pos := parseNonNegativeInt(headers.Get(concertPositionHeader))

	if isJSONContentTypeOrEmpty(contentType) && len(bytes.TrimSpace(body)) > 0 && bytes.TrimSpace(body)[0] == '{' {
		// If the JSON body explicitly says {"queued":false}, treat as not queued (breaker).
		if isConcertBreaker503(contentType, body) {
			return WaitingRoomSignal{}, false
		}
		if sig, ok := parseConcertJSONSignal(body, headerRetry, true); ok {
			if sig.StatusURL == defaultQueueStatusURL && statusURL != defaultQueueStatusURL {
				sig.StatusURL = statusURL
			}
			if sig.Position == 0 && pos > 0 {
				sig.Position = pos
			}
			return sig, true
		}
	}

	if pos == 0 && len(body) > 0 {
		pos = extractWaitingRoomPosition(string(body))
	}
	return WaitingRoomSignal{
		Detected:   true,
		Mode:       WaitingRoomModeConcertHTML,
		Position:   pos,
		StatusURL:  statusURL,
		RetryAfter: headerRetry,
	}, true
}

func parseConcertJSONSignal(body []byte, headerRetry time.Duration, allowOmittedQueued bool) (WaitingRoomSignal, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || len(trimmed) > maxQueueStatusBodyBytes || trimmed[0] != '{' {
		return WaitingRoomSignal{}, false
	}
	var payload struct {
		Queued            *bool  `json:"queued"`
		StatusURL         string `json:"status_url"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
		Position          int    `json:"position"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return WaitingRoomSignal{}, false
	}
	if payload.Queued != nil && !*payload.Queued {
		return WaitingRoomSignal{}, false
	}
	if payload.Queued == nil && !allowOmittedQueued {
		return WaitingRoomSignal{}, false
	}
	retryAfter := clampRetryAfterSeconds(payload.RetryAfterSeconds)
	if retryAfter == 0 {
		retryAfter = headerRetry
	}
	return WaitingRoomSignal{
		Detected:   true,
		Mode:       WaitingRoomModeConcertJSON,
		Position:   max(payload.Position, 0),
		StatusURL:  sanitizeStatusURL(payload.StatusURL),
		RetryAfter: retryAfter,
	}, true
}

func parseNonNegativeInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func hasSetCookie(headers http.Header, name string) bool {
	if headers == nil || name == "" {
		return false
	}
	if len(headers.Values("Set-Cookie")) == 0 {
		return false
	}
	resp := &http.Response{Header: headers}
	for _, c := range resp.Cookies() {
		if c.Name == name && c.Value != "" && c.MaxAge >= 0 {
			return true
		}
	}
	return false
}

// DetectWaitingRoom checks body for Concert waiting.html or legacy waiting room
// HTML signatures and extracts its queue position (clamped to >= 0).
func DetectWaitingRoom(body []byte) (bool, int) {
	sig, ok := DetectWaitingRoomResponse(http.StatusOK, "text/html", nil, body)
	if !ok {
		return false, 0
	}
	return true, sig.Position
}

func hasPositionMarker(s string) bool {
	return strings.Contains(s, `id="position"`) || strings.Contains(s, `id='position'`)
}

func extractWaitingRoomPosition(s string) int {
	idx := strings.Index(s, `id="position"`)
	if idx < 0 {
		idx = strings.Index(s, `id='position'`)
	}
	if idx < 0 {
		return 0
	}
	rest := s[idx+len(`id="position"`):]
	gt := strings.IndexByte(rest, '>')
	if gt < 0 {
		return 0
	}
	rest = rest[gt+1:]
	if lt := strings.IndexByte(rest, '<'); lt >= 0 {
		rest = rest[:lt]
	}
	pos, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil || pos < 0 {
		return 0
	}
	return pos
}

func isJSONContentTypeOrEmpty(ct string) bool {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return true
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		if semi := strings.IndexByte(ct, ';'); semi >= 0 {
			ct = ct[:semi]
		}
		media = strings.ToLower(strings.TrimSpace(ct))
	} else {
		media = strings.ToLower(strings.TrimSpace(media))
	}
	return media == "application/json" || strings.HasSuffix(media, "+json")
}

func isHTMLContentTypeOrEmpty(ct string) bool {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return true
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		if semi := strings.IndexByte(ct, ';'); semi >= 0 {
			ct = ct[:semi]
		}
		media = strings.ToLower(strings.TrimSpace(ct))
	} else {
		media = strings.ToLower(strings.TrimSpace(media))
	}
	return media == "text/html" || media == "application/xhtml+xml"
}

func isCSSAsset(contentType, class string) bool {
	ct := strings.TrimSpace(contentType)
	if ct != "" {
		media, _, err := mime.ParseMediaType(ct)
		if err != nil {
			if semi := strings.IndexByte(ct, ';'); semi >= 0 {
				ct = ct[:semi]
			}
			media = strings.ToLower(strings.TrimSpace(ct))
		} else {
			media = strings.ToLower(strings.TrimSpace(media))
		}
		return media == "text/css"
	}
	return class == "style"
}

func isConcertBreaker503(contentType string, body []byte) bool {
	if !isJSONContentTypeOrEmpty(contentType) || len(body) == 0 || len(body) > maxQueueStatusBodyBytes {
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var payload struct {
		Queued *bool `json:"queued"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return false
	}
	return payload.Queued != nil && !*payload.Queued
}

func clampRetryAfterSeconds(secs int) time.Duration {
	if secs <= 0 {
		return 0
	}
	maxSecs := int(maxRetryAfter / time.Second)
	if secs > maxSecs {
		return maxRetryAfter
	}
	return time.Duration(secs) * time.Second
}

// sanitizeStatusURL ensures a status_url from a JSON waiting-room payload is a
// safe same-origin relative path, falling back to "/queue/status" otherwise.
func sanitizeStatusURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultQueueStatusURL
	}
	if strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n") {
		return defaultQueueStatusURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return defaultQueueStatusURL
	}
	if !strings.HasPrefix(raw, "/") {
		return defaultQueueStatusURL
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

func resolvePollURL(targetURL, statusURL string) (resolvedURL, normalizedPath string, ok bool) {
	base, err := url.Parse(targetURL)
	if err != nil || base.Host == "" {
		return "", "", false
	}
	candidate := strings.TrimSpace(statusURL)
	if candidate == "" {
		candidate = defaultQueueStatusURL
	}
	ref, err := url.Parse(candidate)
	if err != nil || ref.User != nil {
		ref, _ = url.Parse(defaultQueueStatusURL)
	}
	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""
	resolved.RawFragment = ""

	if !discovery.SameOrigin(targetURL, resolved.String()) {
		fallbackRef, _ := url.Parse(defaultQueueStatusURL)
		resolved = base.ResolveReference(fallbackRef)
		if !discovery.SameOrigin(targetURL, resolved.String()) {
			return "", "", false
		}
	}
	return resolved.String(), resolved.RequestURI(), true
}

func isConcertInternalURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := u.Path
	if p == "/_concert" || strings.HasPrefix(p, "/_concert/") {
		return true
	}
	cleaned := path.Clean("/" + p)
	return cleaned == "/_concert" || strings.HasPrefix(cleaned, "/_concert/")
}

func filterSiteLinks(links []string) []string {
	if len(links) == 0 {
		return links
	}
	out := links[:0]
	for _, link := range links {
		if !isConcertInternalURL(link) {
			out = append(out, link)
		}
	}
	return out
}

func generateLemmingID(number, terrain, pack int64) string {
	raw := fmt.Sprintf("%d-%d-%d-%d", number, terrain, pack, time.Now().UnixNano())
	h := sha512.New()
	h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

var (
	nameFirst = []string{
		"Neon", "Turbo", "Laser", "Chrome", "Pixel", "Retro", "Vapor", "Cosmic",
		"Midnight", "Electric", "Hyper", "Solar", "Velvet", "Arcade", "Synth", "Nova",
		"Static", "Echo", "Glitch", "Magenta", "Cyber", "Lunar", "Radiant", "Sunset",
		"Voltage", "Quartz", "Prism", "Rocket", "Holo", "Stellar", "Analog", "Outrun",
	}
	nameLast = []string{
		"Rider", "Drifter", "Dreamer", "Runner", "Glider", "Nomad", "Ranger", "Voyager",
		"Racer", "Wanderer", "Pilot", "Scout", "Rebel", "Ghost", "Comet", "Falcon",
		"Tiger", "Fox", "Wolf", "Moth", "Otter", "Heron", "Lynx", "Viper",
		"Cruiser", "Skater", "Surfer", "Hopper", "Seeker", "Jumper", "Diver", "Walker",
	}
)

// LemmingName derives a stable, memorable Synthwave name from a lemming ID.
func LemmingName(id string) string {
	var a, b int
	if len(id) >= 4 {
		x, _ := strconv.ParseUint(id[:2], 16, 8)
		y, _ := strconv.ParseUint(id[2:4], 16, 8)
		a, b = int(x), int(y)
	}
	return nameFirst[a%len(nameFirst)] + " " + nameLast[b%len(nameLast)]
}

// PersonaOf summarizes a User-Agent string as "Browser · Platform".
func PersonaOf(ua string) string {
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

// IsMobileUA reports whether ua represents a mobile viewport device.
func IsMobileUA(ua string) bool {
	return strings.Contains(ua, "iPhone") || strings.Contains(ua, "Mobile") || strings.Contains(ua, "Android")
}
