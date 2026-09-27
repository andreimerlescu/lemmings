package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// userAgents is the pool of realistic browser UA strings.
// One is selected once per session and remains stable for that user.
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Safari/605.1.15",
	"Mozilla/5.0 (X11; Linux x86_64; rv:125.0) Gecko/20100101 Firefox/125.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36 Edg/124.0.0.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
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
)

// Identity is the lemming's persistent persona for its entire lifespan.
type Identity struct {
	ID        string // UUID-style hex identifier
	Terrain   int64
	Pack      int64
	BornAt    time.Time
	UserAgent string
	Language  string
}

// WaitingRoomMetric captures everything about a lemming's time in a queue.
type WaitingRoomMetric struct {
	Detected  bool
	Position  int
	EnteredAt time.Time
	ExitedAt  time.Time
	Duration  time.Duration
}

// Visit is the atomic unit of measurement — one page hit by one lemming.
type Visit struct {
	RetryAfter       time.Duration
	LemmingID        string
	URL              string
	StatusCode       int
	BytesIn          int64
	Duration         time.Duration // wall clock from request start to body close
	Checksum         string        // SHA512 of actual response body
	Expected         string        // SHA512 from URLPool at index time
	Match            bool          // Checksum == Expected
	WaitingRoom      WaitingRoomMetric
	CaptchaHit       bool
	Error            error
	Timestamp        time.Time
	Sequence         int
	Step             string
	FinalURL         string
	Referrer         string
	CookieNames      []string
	Timing           RequestTiming
	ResponseCodes    map[int]int64
	OmittedResponses int64
	Responses        []ResponseHop
	Page             PageEvidence
	ErrorKind        string
	Cancelled        bool
}

// LifeLog contains session totals and a bounded tail of the last 100 visits.
// Complete visit aggregates are streamed separately while the user is alive.
type LifeLog struct {
	Identity        Identity
	Terrain         int
	Pack            int
	Visits          []Visit
	Error           error // non-nil if lemming died abnormally
	BornAt          time.Time
	DiedAt          time.Time
	Duration        time.Duration
	LiveRecorded    bool // lifecycle/visit counters were updated at the source
	Streamed        bool // visits were already aggregated by the reporter
	TotalVisits     int64
	FailedVisits    int64
	CancelledVisits int64
	ExitReason      string
	OmittedVisits   int64
}

// Lemming is a stateful navigating HTTP agent with its own session and identity.
type Lemming struct {
	identity     Identity
	cfg          SwarmConfig
	pool         *URLPool
	client       *http.Client
	bus          *EventBus
	metrics      *SwarmMetrics
	rng          *rand.Rand // per-lemming rng, no lock needed
	previousURL  string
	nextLinks    []string
	step         int
	recordVisit  func(Visit)
	lastResponse requestEvidence
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

	l := &Lemming{
		identity: Identity{
			ID:      id,
			Terrain: terrain,
			Pack:    pack,
			BornAt:  time.Now(),
		},
		cfg:     cfg,
		pool:    pool,
		client:  client,
		bus:     bus,
		metrics: metrics,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	l.identity.UserAgent = l.randomUA()
	l.identity.Language = "en-US,en;q=0.9"
	return l
}

// Run is the lemming's entire life. It derives a deadline context from
// cfg.Until and navigates the URLPool until the deadline expires.
// It returns a fully populated LifeLog.
//
// Lifecycle events (EventLemmingBorn and EventLemmingDied) are emitted
// by the Terrain, not by Run. This keeps the birth/death counters
// accurate — exactly one increment and one decrement per lemming — and
// ensures only one component in the package owns lifecycle signaling.
// Run only emits per-visit events (EventVisitComplete, EventWaitingRoom)
// that are specific to the lemming's in-flight work.
func (l *Lemming) Run(parent context.Context) LifeLog {
	ctx, cancel := context.WithTimeout(parent, l.cfg.Until)
	defer cancel()
	ll := LifeLog{Identity: l.identity, Terrain: int(l.identity.Terrain), Pack: int(l.identity.Pack), BornAt: time.Now(), Streamed: l.recordVisit != nil, LiveRecorded: true, ExitReason: "lifespan"}
	for ctx.Err() == nil {
		if l.cfg.Experience.MaxPages > 0 && ll.TotalVisits >= int64(l.cfg.Experience.MaxPages) {
			ll.ExitReason = "page-limit"
			break
		}
		u := l.pickURL()
		if scenario := l.cfg.Experience.Scenario; scenario != nil {
			if l.step >= len(scenario.Steps) {
				ll.ExitReason = "journey-complete"
				break
			}
			u = resolveURL(l.cfg.Hit, scenario.Steps[l.step].Path)
		} else if l.cfg.Experience.Navigation == "links" {
			if ll.TotalVisits == 0 {
				u = l.cfg.Hit
			} else if len(l.nextLinks) > 0 {
				u = l.nextLinks[l.rng.Intn(len(l.nextLinks))]
			}
		}
		v := l.hit(ctx, u)
		v.Sequence = int(ll.TotalVisits) + 1
		if sc := l.cfg.Experience.Scenario; sc != nil {
			v.Step = sc.Steps[l.step].Name
		}
		l.step++
		ll.TotalVisits++
		if v.failed() {
			ll.FailedVisits++
		}
		if v.Cancelled {
			ll.CancelledVisits++
		}
		// Keep a bounded, chronological tail. Complete totals are aggregated
		// independently; an optional JSONL trace records the full visit stream.
		if len(ll.Visits) == 100 {
			copy(ll.Visits, ll.Visits[1:])
			ll.Visits = ll.Visits[:99]
			ll.OmittedVisits++
		}
		ll.Visits = append(ll.Visits, v)
		l.metrics.recordVisit(v)
		if l.recordVisit != nil {
			l.recordVisit(v)
		}
		kind := EventVisitComplete
		if v.failed() || v.Cancelled {
			kind = EventVisitError
		}
		l.bus.Emit(Event{Kind: kind, LemmingID: l.identity.ID, Terrain: int(l.identity.Terrain), Pack: int(l.identity.Pack), URL: safeURL(u), StatusCode: v.StatusCode, BytesIn: v.BytesIn, Duration: v.Duration, Err: v.Error, Failed: v.failed(), Cancelled: v.Cancelled})
		if !v.Cancelled && v.FinalURL != "" {
			l.previousURL = v.FinalURL
		}
		delay := l.cfg.Experience.ThinkMin
		if extra := l.cfg.Experience.ThinkMax - delay; extra > 0 {
			delay += time.Duration(l.rng.Int63n(int64(extra)))
		}
		// Back off a failing origin even when the explicit think time is zero.
		if v.failed() && delay < 100*time.Millisecond {
			delay = 100 * time.Millisecond
		}
		if (v.StatusCode == 429 || v.StatusCode == 503) && v.RetryAfter > delay {
			delay = v.RetryAfter
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	if parent.Err() != nil {
		ll.ExitReason = "cancelled"
	}
	ll.DiedAt = time.Now()
	ll.Duration = ll.DiedAt.Sub(ll.BornAt)
	return ll
}

// hit performs a single page visit, handling waiting room detection
// and checksum comparison. It blocks for the full duration if the
// lemming is placed in a waiting room.
//
// The WaitingRoom event is emitted after waitInRoom returns so that
// the Duration field reflects the actual time spent waiting, not zero.
func (l *Lemming) hit(ctx context.Context, url string) Visit {
	visit := Visit{
		LemmingID: l.identity.ID,
		URL:       url,
		Expected:  l.pool.Checksums[url],
		Timestamp: time.Now(),
	}

	start := time.Now()
	body, statusCode, bytesIn, err := l.request(ctx, url)
	visit.StatusCode = statusCode
	visit.BytesIn = bytesIn
	visit.Error = err
	visit.RetryAfter = l.lastResponse.retryAfter
	visit.FinalURL = l.lastResponse.finalURL
	visit.Referrer = safeURL(l.previousURL)
	visit.CookieNames = l.lastResponse.cookieNames
	visit.Timing = l.lastResponse.timing
	visit.Responses = l.lastResponse.hops
	visit.ResponseCodes = map[int]int64{}
	for _, h := range visit.Responses {
		visit.ResponseCodes[h.Status]++
	}
	visit.ErrorKind = errorKind(err)
	visit.Cancelled = err != nil && ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))

	if err != nil {
		visit.Duration = time.Since(start)
		return visit
	}

	checksum := sha512sum(body)
	visit.Checksum = checksum
	visit.Match = checksum == visit.Expected

	// Waiting room detection
	if inWaitingRoom, position := detectWaitingRoom(body); inWaitingRoom {
		visit.WaitingRoom = WaitingRoomMetric{
			Detected:  true,
			Position:  position,
			EnteredAt: time.Now(),
		}

		// Poll until admitted or context expires
		body, visit = l.waitInRoom(ctx, url, visit)

		// Emit waiting room event now that Duration is known
		l.bus.Emit(Event{
			Kind:      EventWaitingRoom,
			LemmingID: l.identity.ID,
			Terrain:   int(l.identity.Terrain),
			Pack:      int(l.identity.Pack),
			URL:       url,
			Duration:  visit.WaitingRoom.Duration,
		})
	}

	visit.Duration = time.Since(start)
	visit.FinalURL = l.lastResponse.finalURL
	visit.Timing = l.lastResponse.timing
	visit.CookieNames = l.lastResponse.cookieNames
	visit.ErrorKind = errorKind(visit.Error)
	visit.Cancelled = visit.Error != nil && ctx.Err() != nil
	expected := Expectations{}
	if sc := l.cfg.Experience.Scenario; sc != nil && l.step < len(sc.Steps) {
		expected = sc.Steps[l.step].Expect
	}
	visit.Page = inspectPage(body, l.lastResponse.contentType, visit.StatusCode, expected)
	if l.cfg.Experience.StrictChecksum {
		visit.Page.Checks = append(visit.Page.Checks, CheckResult{"checksum", visit.Expected != "" && visit.Match, "strict comparison with index-time body"})
	}
	l.nextLinks = extractHTMLLinks(body, visit.FinalURL)
	return visit
}

// waitInRoom blocks the lemming in the waiting room, re-requesting the URL
// on the room's poll interval until the queue signature disappears (admission)
// or the lemming's context expires. Dynamic checksums need not match.
func (l *Lemming) waitInRoom(ctx context.Context, url string, visit Visit) ([]byte, Visit) {
	ticker := time.NewTicker(waitingRoomPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Lemming died in the waiting room — record final state
			visit.WaitingRoom.ExitedAt = time.Now()
			visit.WaitingRoom.Duration = visit.WaitingRoom.ExitedAt.Sub(
				visit.WaitingRoom.EnteredAt,
			)
			visit.Error = ctx.Err()
			return nil, visit

		case <-ticker.C:
			body, statusCode, bytesIn, err := l.request(ctx, url)
			visit.StatusCode = statusCode
			visit.BytesIn += bytesIn // accumulate bytes across all polls
			for _, h := range l.lastResponse.hops {
				if visit.ResponseCodes == nil {
					visit.ResponseCodes = map[int]int64{}
				}
				visit.ResponseCodes[h.Status]++
				if len(visit.Responses) < 32 {
					visit.Responses = append(visit.Responses, h)
				} else {
					visit.OmittedResponses++
				}
			}

			if err != nil {
				visit.Error = err
				continue
			}

			visit.Error = nil // a successful poll clears earlier transient errors
			checksum := sha512sum(body)
			visit.Checksum = checksum

			// Still in waiting room — update position
			if inRoom, position := detectWaitingRoom(body); inRoom {
				visit.WaitingRoom.Position = position
				continue
			}

			// The queue signature has disappeared; dynamic pages need not match a checksum.
			if inRoom, _ := detectWaitingRoom(body); !inRoom {
				visit.Match = checksum == visit.Expected
				visit.WaitingRoom.ExitedAt = time.Now()
				visit.WaitingRoom.Duration = visit.WaitingRoom.ExitedAt.Sub(
					visit.WaitingRoom.EnteredAt,
				)
				return body, visit
			}

			// Body changed but doesn't match expected and isn't a waiting room.
			// This is unusual — record it and move on.
			visit.WaitingRoom.ExitedAt = time.Now()
			visit.WaitingRoom.Duration = visit.WaitingRoom.ExitedAt.Sub(
				visit.WaitingRoom.EnteredAt,
			)
			return body, visit
		}
	}
}

// request preserves the original helper API while capturing richer evidence.
func (l *Lemming) request(ctx context.Context, url string) ([]byte, int, int64, error) {
	l.lastResponse = l.requestDetailed(ctx, url)
	r := l.lastResponse
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
	if err != nil || pos < 0 { // ← clamp negative to 0
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
