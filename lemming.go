package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// userAgents is the pool of realistic browser UA strings.
// One is selected when a lemming is born and kept for its whole life,
// exactly as a real browser session keeps its user agent.
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Safari/605.1.15",
	"Mozilla/5.0 (X11; Linux x86_64; rv:125.0) Gecko/20100101 Firefox/125.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 Edg/124.0.0.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_4_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Mobile/15E148 Safari/604.1",
	"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Mobile Safari/537.36",
}

// acceptLanguages is the pool of Accept-Language values a lemming may be
// born with. Weighted towards en-US by repetition. Sites that localise by
// language will serve different bodies than the index saw — that is real
// user behaviour, and checksum changes are informational by default.
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
	// waitingRoomSignature is the string we look for in a response body
	// to detect that the lemming has been placed in a room queue.
	waitingRoomSignature = `/queue/status`

	// waitingRoomPositionMarker is the HTML marker preceding the position number.
	waitingRoomPositionMarker = `id="position">`

	// waitingRoomPollInterval is how long the lemming waits between
	// re-requesting a URL it is queued for.
	waitingRoomPollInterval = 3 * time.Second

	// lifeTailSize is how many of its most recent visits a lemming keeps
	// in its LifeLog. Complete totals are streamed as visits happen, so the
	// tail is only the detail a report can show for a notable lemming.
	lifeTailSize = reportTailSteps

	// failureBackoff is the minimum pause after a failed visit, applied even
	// when think time is zero so a dead origin is not spun against.
	failureBackoff = 100 * time.Millisecond

	// maxRetryAfter caps how long a Retry-After header can park a lemming.
	maxRetryAfter = 5 * time.Minute
)

// Exit reasons recorded on LifeLog.ExitReason.
const (
	ExitLifespan        = "lifespan"         // -until elapsed
	ExitPageLimit       = "page-limit"       // -max-pages reached
	ExitJourneyComplete = "journey-complete" // every -journey step visited
	ExitCancelled       = "cancelled"        // the run itself was stopped
)

// Identity is the lemming's persistent persona for its entire lifespan.
type Identity struct {
	ID        string    `json:"id"`   // 32-char hex identifier
	Name      string    `json:"name"` // friendly name derived from ID
	Terrain   int64     `json:"terrain"`
	Pack      int64     `json:"pack"`
	BornAt    time.Time `json:"born_at"`
	UserAgent string    `json:"user_agent"`
	Language  string    `json:"language"`
}

// WaitingRoomMetric captures everything about a lemming's time in a queue.
type WaitingRoomMetric struct {
	Detected  bool          `json:"detected"`
	Position  int           `json:"position"`
	EnteredAt time.Time     `json:"entered_at"`
	ExitedAt  time.Time     `json:"exited_at"`
	Duration  time.Duration `json:"duration_ns"`
}

// Visit is the atomic unit of measurement — one page hit by one lemming.
//
// Duration is what the lemming experienced: from the first byte sent to the
// last byte read, including redirects and any time held in a waiting room.
// Latency is how long the server took to serve the page the lemming finally
// got — the same as Duration unless it queued — and is what percentiles
// measure. Timing breaks down the final request only.
type Visit struct {
	LemmingID   string
	Sequence    int    // 1-based position in the lemming's life
	Step        string // journey step name, when running a -journey
	URL         string
	FinalURL    string // URL after redirects
	Referrer    string
	StatusCode  int
	BytesIn     int64         // decoded body bytes, including queue polls
	Duration    time.Duration // wall clock from request start to body close
	Latency     time.Duration // server time for the page served; excludes queueing
	Timing      RequestTiming
	Hops        []ResponseHop // redirect chain and queue polls, bounded
	CookieNames []string      // names only — values are never recorded
	Checksum    string        // SHA512 of actual response body
	Expected    string        // SHA512 from URLPool at index time
	Match       bool          // Checksum == Expected
	WaitingRoom WaitingRoomMetric
	Page        PageEvidence
	RetryAfter  time.Duration
	CaptchaHit  bool
	Error       error
	ErrorKind   string // classification of Error; see errorKind
	Failed      bool   // transport error, failed check, or bad status
	Cancelled   bool   // interrupted because the lemming's life ended
	Timestamp   time.Time
}

// latency returns the visit's server latency, falling back to Duration for
// visits recorded without one.
func (v *Visit) latency() time.Duration {
	if v.Latency > 0 {
		return v.Latency
	}
	return v.Duration
}

// LifeLog is the record of a lemming's life. It holds exact session totals
// plus the last lifeTailSize visits in order. It is built during Run and
// sent to the swarm at death.
type LifeLog struct {
	Identity        Identity
	Terrain         int
	Pack            int
	Visits          []Visit // chronological tail, at most lifeTailSize
	Error           error   // non-nil if lemming died abnormally
	BornAt          time.Time
	DiedAt          time.Time
	Duration        time.Duration
	TotalVisits     int64
	FailedVisits    int64
	CancelledVisits int64
	OmittedVisits   int64 // visits older than the tail
	SlowestVisit    time.Duration
	LongestWait     time.Duration
	FirstFailureAt  time.Time // when the first failed visit started
	ExitReason      string

	// Streamed is true when every visit was already counted in SwarmMetrics
	// and delivered on the EventBus as it happened. The collector only
	// re-counts visits for LifeLogs built elsewhere (Streamed == false).
	Streamed bool
}

// Lemming is a stateful navigating HTTP agent with its own session and identity.
type Lemming struct {
	identity Identity
	cfg      SwarmConfig
	pool     *URLPool
	client   *http.Client
	bus      *EventBus
	metrics  *SwarmMetrics
	rng      *rand.Rand // per-lemming rng, no lock needed

	referrer string   // final URL of the previous page
	links    []string // navigable links found on the previous page
	step     int      // index of the next -journey step
}

// NewLemming constructs a Lemming. The HTTP client is provided by Terrain,
// which has already attached an isolated cookie jar to it.
func NewLemming(
	terrain int64,
	pack int64,
	cfg SwarmConfig,
	pool *URLPool,
	client *http.Client,
	bus *EventBus,
	metrics *SwarmMetrics,
) *Lemming {
	id := generateLemmingID(terrain, pack)
	// The ID embeds a nanosecond timestamp, so it seeds each lemming's RNG
	// uniquely even when thousands are born within one clock tick.
	seed, _ := strconv.ParseInt(id[:15], 16, 64)
	l := &Lemming{
		identity: Identity{
			ID:      id,
			Name:    lemmingName(id),
			Terrain: terrain,
			Pack:    pack,
			BornAt:  time.Now(),
		},
		cfg:     cfg,
		pool:    pool,
		client:  client,
		bus:     bus,
		metrics: metrics,
		rng:     rand.New(rand.NewSource(seed)),
	}
	l.identity.UserAgent = l.randomUA()
	l.identity.Language = acceptLanguages[l.rng.Intn(len(acceptLanguages))]
	return l
}

// Run is the lemming's entire life. It derives a deadline context from
// cfg.Until and navigates until the deadline expires, its page limit or
// journey is exhausted, or the parent context is cancelled. It returns a
// LifeLog with exact totals and a bounded tail of visits.
//
// Every visit is counted in SwarmMetrics and emitted on the EventBus the
// moment it completes, so dashboards and reports see it live rather than
// when the lemming dies.
//
// Lifecycle events (EventLemmingBorn and EventLemmingDied) are emitted
// by the Terrain, not by Run. This keeps the birth/death counters
// accurate — exactly one increment and one decrement per lemming — and
// ensures only one component in the package owns lifecycle signaling.
// Run only emits per-visit and waiting room events.
func (l *Lemming) Run(parent context.Context) LifeLog {
	ctx, cancel := context.WithTimeout(parent, l.cfg.Until)
	defer cancel()

	ll := LifeLog{
		Identity:   l.identity,
		Terrain:    int(l.identity.Terrain),
		Pack:       int(l.identity.Pack),
		BornAt:     time.Now(),
		ExitReason: ExitLifespan,
		Streamed:   true,
	}
	tail := make([]Visit, 0, 16)
	head := 0 // index of the oldest visit once tail is full

	for ctx.Err() == nil {
		if l.cfg.MaxPages > 0 && ll.TotalVisits >= int64(l.cfg.MaxPages) {
			ll.ExitReason = ExitPageLimit
			break
		}
		target, step, ok := l.next(ll.TotalVisits)
		if !ok {
			ll.ExitReason = ExitJourneyComplete
			break
		}

		visit, links := l.hit(ctx, target, step)
		visit.Sequence = int(ll.TotalVisits) + 1
		l.remember(&ll, &visit, links)

		if len(tail) < lifeTailSize {
			tail = append(tail, visit)
		} else {
			tail[head] = visit
			head = (head + 1) % lifeTailSize
			ll.OmittedVisits++
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
	return ll
}

// next chooses where the lemming goes next. It returns the URL, the journey
// step (nil outside a journey) and false once a journey is complete.
func (l *Lemming) next(visited int64) (string, *JourneyStep, bool) {
	if j := l.cfg.Journey; j != nil {
		if l.step >= len(j.Steps) {
			return "", nil, false
		}
		step := &j.Steps[l.step]
		l.step++
		return resolveURL(l.cfg.Hit, step.Path), step, true
	}
	if l.cfg.Navigation == NavigationLinks {
		if visited == 0 {
			return l.cfg.Hit, nil, true
		}
		if len(l.links) > 0 {
			return l.links[l.rng.Intn(len(l.links))], nil, true
		}
	}
	return l.pickURL(), nil, true
}

// remember folds a finished visit into the lemming's running totals,
// shared metrics and the event bus, and prepares navigation state.
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
	if !v.Cancelled && v.latency() > ll.SlowestVisit {
		ll.SlowestVisit = v.latency()
	}
	if v.WaitingRoom.Duration > ll.LongestWait {
		ll.LongestWait = v.WaitingRoom.Duration
	}

	l.metrics.recordVisit(v)

	kind := EventVisitComplete
	if v.Failed || v.Cancelled {
		kind = EventVisitError
	}
	l.bus.Emit(Event{
		Kind:       kind,
		LemmingID:  l.identity.ID,
		Terrain:    int(l.identity.Terrain),
		Pack:       int(l.identity.Pack),
		URL:        v.URL,
		StatusCode: v.StatusCode,
		BytesIn:    v.BytesIn,
		Duration:   v.latency(),
		Visit:      v,
		Err:        v.Error,
	})

	if !v.Cancelled && v.FinalURL != "" {
		l.referrer = v.FinalURL
		l.links = links
	}
}

// think pauses between pages like a person reading. It returns false if
// the lemming's life ended while it was thinking.
//
// The pause is a uniform draw from [ThinkMin, ThinkMax], raised to
// failureBackoff after a failed visit and to the server's Retry-After on
// 429 and 503 responses (capped at maxRetryAfter and at the lemming's
// remaining life, which the context enforces).
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

// hit performs a single page visit, handling waiting room detection,
// checksum comparison and page checks. It blocks for the full duration if
// the lemming is placed in a waiting room. It returns the visit and the
// navigable links found on the final page.
func (l *Lemming) hit(ctx context.Context, url string, step *JourneyStep) (Visit, []string) {
	visit := Visit{
		LemmingID: l.identity.ID,
		URL:       url,
		Referrer:  l.referrer,
		Expected:  l.pool.Checksums[url],
		Timestamp: time.Now(),
	}
	var expect Expectations
	if step != nil {
		visit.Step = step.Name
		expect = step.Expect
	}

	start := time.Now()
	resp := l.fetch(ctx, url)
	resp.applyTo(&visit)
	visit.BytesIn = resp.bytes
	visit.Hops = resp.hops
	visit.Latency = resp.elapsed
	body := resp.body

	if resp.err == nil {
		visit.Checksum = sha512sum(body)
		visit.Match = visit.Checksum == visit.Expected
		if inRoom, position := detectWaitingRoom(body); inRoom {
			body = l.waitInRoom(ctx, url, &visit, position)
		}
	}
	visit.Duration = time.Since(start)

	visit.ErrorKind = errorKind(visit.Error)
	visit.Cancelled = visit.Error != nil && ctx.Err() != nil

	var links []string
	if !visit.Cancelled && visit.Error == nil {
		visit.Page, links = inspectPage(body, resp.contentType, visit.StatusCode, expect, visit.FinalURL)
		if l.cfg.StrictChecksum {
			visit.Page.Checks = append(visit.Page.Checks, CheckResult{
				Name:   "checksum",
				Passed: visit.Expected != "" && visit.Match,
				Detail: "body must match the index-time checksum (-strict-checksum)",
			})
		}
	}
	visit.Failed = !visit.Cancelled && (visit.Error != nil || visit.Page.failedChecks() > 0)
	return visit, links
}

// waitInRoom blocks the lemming in the waiting room, re-requesting the URL
// on the room's poll interval until the queue signature disappears
// (admission) or the lemming's context expires (death in queue). Admission
// does not require the admitted body to match the index-time checksum —
// dynamic pages rarely do.
//
// It emits EventWaitingRoomQueued on entry and whenever the position
// changes, and EventWaitingRoom once when the stay ends. It returns the
// admitted body, or nil if the lemming died in line.
func (l *Lemming) waitInRoom(ctx context.Context, url string, visit *Visit, position int) []byte {
	visit.WaitingRoom = WaitingRoomMetric{
		Detected:  true,
		Position:  position,
		EnteredAt: time.Now(),
	}
	l.emitQueued(url, position)

	defer func() {
		visit.WaitingRoom.ExitedAt = time.Now()
		visit.WaitingRoom.Duration = visit.WaitingRoom.ExitedAt.Sub(visit.WaitingRoom.EnteredAt)
		l.bus.Emit(Event{
			Kind:      EventWaitingRoom,
			LemmingID: l.identity.ID,
			Terrain:   int(l.identity.Terrain),
			Pack:      int(l.identity.Pack),
			URL:       url,
			Position:  visit.WaitingRoom.Position,
			Duration:  visit.WaitingRoom.Duration,
		})
	}()

	ticker := time.NewTicker(waitingRoomPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Lemming died in the waiting room.
			visit.Error = ctx.Err()
			return nil

		case <-ticker.C:
			resp := l.fetch(ctx, url)
			visit.StatusCode = resp.status
			visit.BytesIn += resp.bytes // accumulate bytes across all polls
			visit.Hops = appendHops(visit.Hops, resp.hops...)
			if resp.err != nil {
				visit.Error = resp.err
				continue
			}
			visit.Error = nil // a successful poll clears earlier transient errors
			resp.applyTo(visit)

			if inRoom, position := detectWaitingRoom(resp.body); inRoom {
				if position != visit.WaitingRoom.Position {
					visit.WaitingRoom.Position = position
					l.emitQueued(url, position)
				}
				continue
			}

			// The queue signature disappeared: the lemming was admitted.
			visit.Latency = resp.elapsed
			visit.Checksum = sha512sum(resp.body)
			visit.Match = visit.Checksum == visit.Expected
			return resp.body
		}
	}
}

// emitQueued announces the lemming's current waiting room position.
func (l *Lemming) emitQueued(url string, position int) {
	l.bus.Emit(Event{
		Kind:      EventWaitingRoomQueued,
		LemmingID: l.identity.ID,
		Terrain:   int(l.identity.Terrain),
		Pack:      int(l.identity.Pack),
		URL:       url,
		Position:  position,
	})
}

// request performs a single HTTP GET with the lemming's persona headers.
// It returns the body bytes, status code, byte count, and any error.
// It is a thin view over fetch, which also captures timing and redirects.
func (l *Lemming) request(ctx context.Context, url string) ([]byte, int, int64, error) {
	r := l.fetch(ctx, url)
	return r.body, r.status, r.bytes, r.err
}

// pickURL selects a random URL from the shared pool.
func (l *Lemming) pickURL() string {
	n := len(l.pool.URLs)
	if n == 0 {
		return l.cfg.Hit
	}
	return l.pool.URLs[l.rng.Intn(n)]
}

// randomUA selects a random user agent string from the pool.
func (l *Lemming) randomUA() string {
	return userAgents[l.rng.Intn(len(userAgents))]
}

// detectWaitingRoom checks the response body for the room package signature
// and extracts the queue position if found.
func detectWaitingRoom(body []byte) (bool, int) {
	s := string(body)
	if !strings.Contains(s, waitingRoomSignature) {
		return false, 0
	}

	idx := strings.Index(s, waitingRoomPositionMarker)
	if idx == -1 {
		return true, 0
	}

	start := idx + len(waitingRoomPositionMarker)
	end := strings.Index(s[start:], "<")
	if end == -1 {
		return true, 0
	}

	pos, err := strconv.Atoi(strings.TrimSpace(s[start : start+end]))
	if err != nil || pos < 0 { // clamp negative to 0
		return true, 0
	}

	return true, pos
}

// sha512sum returns the hex-encoded SHA512 checksum of a byte slice.
func sha512sum(b []byte) string {
	h := sha512.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// generateLemmingID produces a unique identifier for a lemming
// from its terrain and pack indices plus a timestamp component.
func generateLemmingID(terrain, pack int64) string {
	raw := fmt.Sprintf("%d-%d-%d", terrain, pack, time.Now().UnixNano())
	h := sha512.New()
	h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// nameFirst and nameLast are combined to give every lemming a memorable
// name on the dashboard and in reports. 32 × 32 = 1,024 combinations.
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

// lemmingName derives a stable, friendly name from a lemming ID.
func lemmingName(id string) string {
	var a, b int
	if len(id) >= 4 {
		x, _ := strconv.ParseUint(id[:2], 16, 8)
		y, _ := strconv.ParseUint(id[2:4], 16, 8)
		a, b = int(x), int(y)
	}
	return nameFirst[a%len(nameFirst)] + " " + nameLast[b%len(nameLast)]
}
