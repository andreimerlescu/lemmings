package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// userAgents is the pool of realistic browser UA strings.
// One is selected at random on every individual request.
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
	LemmingID   string
	URL         string
	StatusCode  int
	BytesIn     int64
	Duration    time.Duration // wall clock from request start to body close
	Checksum    string        // SHA512 of actual response body
	Expected    string        // SHA512 from URLPool at index time
	Match       bool          // Checksum == Expected
	WaitingRoom WaitingRoomMetric
	CaptchaHit  bool
	Error       error
	Timestamp   time.Time
}

// LifeLog is the complete record of a lemming's life.
// It is constructed during Run() and sent to the swarm at death.
type LifeLog struct {
	Identity Identity
	Terrain  int
	Pack     int
	Visits   []Visit
	Error    error // non-nil if lemming died abnormally
	BornAt   time.Time
	DiedAt   time.Time
	Duration time.Duration
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

	return &Lemming{
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
}

// Run is the lemming's entire life. It derives a deadline context from
// cfg.Until and navigates the URLPool until the deadline expires.
// It returns a fully populated LifeLog.
func (l *Lemming) Run(parent context.Context) LifeLog {
	ctx, cancel := context.WithTimeout(parent, l.cfg.Until)
	defer cancel()

	bornAt := time.Now()
	visits := make([]Visit, 0, 16) // pre-allocate reasonable capacity

	l.bus.Emit(Event{
		Kind:      EventLemmingBorn,
		LemmingID: l.identity.ID,
		Terrain:   int(l.identity.Terrain),
		Pack:      int(l.identity.Pack),
	})

	// Navigation loop — runs until context deadline
	for {
		select {
		case <-ctx.Done():
			// Life is over — assemble and return the lifelog
			diedAt := time.Now()
			return LifeLog{
				Identity: l.identity,
				Terrain:  int(l.identity.Terrain),
				Pack:     int(l.identity.Pack),
				Visits:   visits,
				BornAt:   bornAt,
				DiedAt:   diedAt,
				Duration: diedAt.Sub(bornAt),
			}

		default:
			url := l.pickURL()
			visit := l.hit(ctx, url)
			visits = append(visits, visit)

			l.bus.Emit(Event{
				Kind:       EventVisitComplete,
				LemmingID:  l.identity.ID,
				Terrain:    int(l.identity.Terrain),
				Pack:       int(l.identity.Pack),
				URL:        url,
				StatusCode: visit.StatusCode,
			})
		}
	}
}

// hit performs a single page visit, handling waiting room detection
// and checksum comparison. It blocks for the full duration if the
// lemming is placed in a waiting room.
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

		l.bus.Emit(Event{
			Kind:      EventWaitingRoom,
			LemmingID: l.identity.ID,
			Terrain:   int(l.identity.Terrain),
			Pack:      int(l.identity.Pack),
			URL:       url,
		})

		// Poll until admitted (checksum matches) or context expires
		body, visit = l.waitInRoom(ctx, url, visit)
		_ = body
	}

	visit.Duration = time.Since(start)
	return visit
}

// waitInRoom blocks the lemming in the waiting room, re-requesting the URL
// on the room's poll interval until the body checksum matches the indexed
// value (admission) or the lemming's context expires (death in queue).
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
			return nil, visit

		case <-ticker.C:
			body, statusCode, bytesIn, err := l.request(ctx, url)
			visit.StatusCode = statusCode
			visit.BytesIn += bytesIn // accumulate bytes across all polls

			if err != nil {
				visit.Error = err
				continue
			}

			checksum := sha512sum(body)
			visit.Checksum = checksum

			// Still in waiting room — update position
			if inRoom, position := detectWaitingRoom(body); inRoom {
				visit.WaitingRoom.Position = position
				continue
			}

			// Checksum matches — lemming has been admitted
			if checksum == visit.Expected {
				visit.Match = true
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

// request performs a single HTTP GET with a random UA string.
// It returns the body bytes, status code, byte count, and any error.
func (l *Lemming) request(ctx context.Context, url string) ([]byte, int, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("User-Agent", l.randomUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Connection", "keep-alive")

	resp, err := l.client.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, 0, fmt.Errorf("read body: %w", err)
	}

	return body, resp.StatusCode, int64(len(body)), nil
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
	if err != nil {
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
