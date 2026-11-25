package swarm

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/journey"
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
	waitingRoomSignature      = `/queue/status`
	waitingRoomPositionMarker = `id="position">`
	lifeTailSize              = 40
	recentTrailCap            = 8
	failureBackoff            = 100 * time.Millisecond
	maxRetryAfter             = 5 * time.Minute
	maxForensicsBodyBytes     = 512 << 10
)

var waitingRoomPollInterval = 3 * time.Second

// Exit reasons recorded on LifeLog.ExitReason.
const (
	ExitLifespan        = "lifespan"
	ExitPageLimit       = "page-limit"
	ExitJourneyComplete = "journey-complete"
	ExitCancelled       = "cancelled"
)

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
}

// WaitingRoomMetric captures a lemming's waiting room stay.
type WaitingRoomMetric struct {
	Detected  bool          `json:"detected"`
	Position  int           `json:"position"`
	EnteredAt time.Time     `json:"entered_at"`
	ExitedAt  time.Time     `json:"exited_at"`
	Duration  time.Duration `json:"duration_ns"`
}

// Visit is the atomic unit of measurement — one page hit by one lemming.
type Visit struct {
	LemmingID     string
	LemmingNumber int64
	Sequence      int
	Step          string
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
	Page          journey.PageEvidence
	RetryAfter    time.Duration
	CaptchaHit    bool
	Error         error
	ErrorKind     string
	Failed        bool
	Cancelled     bool
	Timestamp     time.Time
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
	Identity        Identity
	Terrain         int
	Pack            int
	Visits          []Visit
	Error           error
	BornAt          time.Time
	DiedAt          time.Time
	Duration        time.Duration
	TotalVisits     int64
	FailedVisits    int64
	CancelledVisits int64
	OmittedVisits   int64
	SlowestVisit    time.Duration
	LongestWait     time.Duration
	FirstFailureAt  time.Time
	ExitReason      string
	Streamed        bool
}

// Lemming is a stateful navigating HTTP agent with its own cookie jar and persona.
type Lemming struct {
	identity  Identity
	cfg       Config
	pool      *discovery.URLPool
	client    *http.Client
	jar       *InspectingJar
	bus       *events.Bus
	metrics   *Metrics
	forensics *forensics.Coordinator
	rng       *rand.Rand

	referrer string
	links    []string
	trail    []string
	step     int
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
		cfg:       cfg,
		pool:      pool,
		client:    client,
		jar:       jar,
		bus:       bus,
		metrics:   metrics,
		forensics: coord,
		rng:       rand.New(rand.NewSource(seed)),
	}
	l.identity.UserAgent = userAgents[l.rng.Intn(len(userAgents))]
	l.identity.Language = acceptLanguages[l.rng.Intn(len(acceptLanguages))]
	return l
}

// Identity returns the lemming's immutable persona.
func (l *Lemming) Identity() Identity { return l.identity }

// Run executes the lemming's entire lifespan.
// Lifecycle events (Born/Died/Failed) are emitted exclusively by Terrain, never here.
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
	head := 0

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

		visit, links, rawBody, headers, contentType := l.hit(ctx, target, step)
		visit.Sequence = int(ll.TotalVisits) + 1
		l.remember(&ll, &visit, links)

		if visit.Failed && l.forensics != nil {
			l.submitForensics(&ll, &visit, rawBody, headers, contentType)
		}

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

func (l *Lemming) submitForensics(ll *LifeLog, v *Visit, body []byte, headers http.Header, contentType string) {
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
	journeyName := ""
	if l.cfg.Journey != nil {
		navMode = "journey"
		journeyName = l.cfg.Journey.Name
	}

	origin := ""
	if l.pool != nil {
		origin = l.pool.Origin
	}
	if origin == "" {
		origin, _ = discovery.ParseOrigin(l.cfg.Hit)
	}

	req := forensics.CaptureRequest{
		URL:           targetURL,
		SafeURL:       discovery.SafeURL(targetURL),
		Origin:        origin,
		Path:          discovery.PathOf(targetURL),
		StatusCode:    v.StatusCode,
		LatencyMS:     float64(v.EffectiveLatency()) / float64(time.Millisecond),
		TriggerReason: triggerReason(v),
		ContentType:   contentType,
		Headers:       headers,
		RawBody:       ownedBody,
		ReplayCookies: replayCookies,
		Mobile:        IsMobileUA(l.identity.UserAgent),
		Session: forensics.SessionSnapshot{
			LemmingNumber:  l.identity.Number,
			LemmingID:      l.identity.ID,
			LemmingName:    l.identity.Name,
			Source:         "http-replay",
			Terrain:        int(l.identity.Terrain),
			Pack:           int(l.identity.Pack),
			Persona:        PersonaOf(l.identity.UserAgent),
			UserAgent:      l.identity.UserAgent,
			Language:       l.identity.Language,
			NavigationMode: navMode,
			JourneyName:    journeyName,
			JourneyStep:    v.Step,
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

func (l *Lemming) next(visited int64) (string, *journey.Step, bool) {
	if j := l.cfg.Journey; j != nil {
		if l.step >= len(j.Steps) {
			return "", nil, false
		}
		step := &j.Steps[l.step]
		l.step++
		return discovery.ResolveURL(l.cfg.Hit, step.Path), step, true
	}
	if l.cfg.Navigation == journey.NavigationLinks {
		if visited == 0 {
			return l.cfg.Hit, nil, true
		}
		if len(l.links) > 0 {
			return l.links[l.rng.Intn(len(l.links))], nil, true
		}
	}
	return l.pickURL(), nil, true
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
			URL:           v.URL,
			StatusCode:    v.StatusCode,
			BytesIn:       v.BytesIn,
			Duration:      v.EffectiveLatency(),
			Failed:        v.Failed,
			Cancelled:     v.Cancelled,
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

func (l *Lemming) hit(ctx context.Context, targetURL string, step *journey.Step) (Visit, []string, []byte, http.Header, string) {
	expected := ""
	if l.pool != nil {
		expected = l.pool.Checksums[targetURL]
	}
	visit := Visit{
		LemmingID:     l.identity.ID,
		LemmingNumber: l.identity.Number,
		URL:           targetURL,
		Referrer:      l.referrer,
		Expected:      expected,
		Timestamp:     time.Now(),
	}
	var expect journey.Expectations
	if step != nil {
		visit.Step = step.Name
		expect = step.Expect
	}

	start := time.Now()
	resp := l.fetch(ctx, targetURL)
	resp.applyTo(&visit)
	visit.BytesIn = resp.bytes
	visit.Hops = resp.hops
	visit.Latency = resp.elapsed
	body := resp.body
	headers := resp.headers
	contentType := resp.contentType

	if resp.err == nil {
		visit.Checksum = discovery.SHA512Hex(body)
		visit.Match = visit.Checksum == visit.Expected
		if inRoom, position := DetectWaitingRoom(body); inRoom {
			body, headers, contentType = l.waitInRoom(ctx, targetURL, &visit, position, headers, contentType)
		}
	}
	visit.Duration = time.Since(start)
	visit.ErrorKind = ErrorKind(visit.Error)
	visit.Cancelled = visit.Error != nil && ctx.Err() != nil

	var links []string
	if !visit.Cancelled && visit.Error == nil {
		visit.Page, links = journey.InspectPage(body, contentType, visit.StatusCode, expect, visit.FinalURL)
		if l.cfg.StrictChecksum {
			visit.Page.Checks = append(visit.Page.Checks, journey.CheckResult{
				Name:   "checksum",
				Passed: visit.Expected != "" && visit.Match,
				Detail: "body must match the index-time checksum (-strict-checksum)",
			})
		}
	}
	visit.Failed = !visit.Cancelled && (visit.Error != nil || visit.Page.FailedChecks() > 0)
	return visit, links, body, headers, contentType
}

func (l *Lemming) waitInRoom(
	ctx context.Context,
	targetURL string,
	visit *Visit,
	position int,
	lastHeaders http.Header,
	lastContentType string,
) ([]byte, http.Header, string) {
	visit.WaitingRoom = WaitingRoomMetric{
		Detected:  true,
		Position:  position,
		EnteredAt: time.Now(),
	}
	l.emitQueued(targetURL, position)

	defer func() {
		visit.WaitingRoom.ExitedAt = time.Now()
		visit.WaitingRoom.Duration = visit.WaitingRoom.ExitedAt.Sub(visit.WaitingRoom.EnteredAt)
		if l.bus != nil {
			l.bus.Emit(events.Event{
				Kind:          events.WaitingRoom,
				LemmingID:     l.identity.ID,
				LemmingNumber: l.identity.Number,
				Terrain:       int(l.identity.Terrain),
				Pack:          int(l.identity.Pack),
				URL:           targetURL,
				Position:      visit.WaitingRoom.Position,
				Duration:      visit.WaitingRoom.Duration,
			})
		}
	}()

	ticker := time.NewTicker(waitingRoomPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			visit.Error = ctx.Err()
			return nil, lastHeaders, lastContentType
		case <-ticker.C:
			resp := l.fetch(ctx, targetURL)
			visit.StatusCode = resp.status
			visit.BytesIn += resp.bytes
			visit.Hops = AppendHops(visit.Hops, resp.hops...)
			if resp.err != nil {
				visit.Error = resp.err
				continue
			}
			visit.Error = nil
			resp.applyTo(visit)
			lastHeaders = resp.headers
			lastContentType = resp.contentType

			if inRoom, pos := DetectWaitingRoom(resp.body); inRoom {
				if pos != visit.WaitingRoom.Position {
					visit.WaitingRoom.Position = pos
					l.emitQueued(targetURL, pos)
				}
				continue
			}

			visit.Latency = resp.elapsed
			visit.Checksum = discovery.SHA512Hex(resp.body)
			visit.Match = visit.Checksum == visit.Expected
			return resp.body, lastHeaders, lastContentType
		}
	}
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

func (l *Lemming) pickURL() string {
	if l.pool == nil || len(l.pool.URLs) == 0 {
		return l.cfg.Hit
	}
	return l.pool.URLs[l.rng.Intn(len(l.pool.URLs))]
}

// DetectWaitingRoom checks body for the room package's waiting room signature
// and extracts its queue position (clamped to >= 0).
func DetectWaitingRoom(body []byte) (bool, int) {
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
	if err != nil || pos < 0 {
		return true, 0
	}
	return true, pos
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
