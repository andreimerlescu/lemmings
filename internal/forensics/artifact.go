// Package forensics implements Lemmings' Error Forensics bounded context:
// the Artifact aggregate (session state, cookie jar status, JS console output,
// extracted error page body, screenshot, and admin triage review), a bounded
// deduplicating store, a zero-Node pure-Go Headless Chromium (CDP) capturer,
// an HTTP-only fallback capturer, and optional real-browser Scouts.
package forensics

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/journey"
)

// ReviewStatus tracks admin triage state for a captured error artifact.
type ReviewStatus string

const (
	ReviewUnreviewed ReviewStatus = "unreviewed"
	ReviewBug        ReviewStatus = "bug"
	ReviewFlaky      ReviewStatus = "flaky"
	ReviewExpected   ReviewStatus = "expected"
	ReviewIgnored    ReviewStatus = "ignored"
)

// ValidReviewStatus reports whether s is a permitted ReviewStatus.
func ValidReviewStatus(s ReviewStatus) bool {
	switch s {
	case ReviewUnreviewed, ReviewBug, ReviewFlaky, ReviewExpected, ReviewIgnored:
		return true
	}
	return false
}

// Capture modes recorded on Artifact.CaptureMode.
const (
	ModeCDPReplay = "cdp-replay"
	ModeCDPScout  = "cdp-scout"
	ModeHTTPOnly  = "http-only"
)

// Drop reasons reported in metrics and events when a capture is skipped.
const (
	DropDeduped    = "deduped"
	DropQueueFull  = "queue_full"
	DropCapReached = "cap_reached"
)

// CookieStatus describes one cookie in a lemming's cookie jar without exposing
// its raw value in serialized reports. RawValue is intentionally json:"-" and
// is copied into a portal view only when -reveal-session-cookies is enabled.
type CookieStatus struct {
	Name        string    `json:"name"`
	Domain      string    `json:"domain,omitempty"`
	Path        string    `json:"path,omitempty"`
	Secure      bool      `json:"secure"`
	HTTPOnly    bool      `json:"http_only"`
	SameSite    string    `json:"same_site,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	SessionOnly bool      `json:"session_only"`
	Expired     bool      `json:"expired"`
	ValueBytes  int       `json:"value_bytes"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	RawValue    string    `json:"-"`
}

// PortalCookieStatus is the DTO served to the authenticated localhost portal.
// Value is populated ONLY when -reveal-session-cookies is explicitly enabled.
type PortalCookieStatus struct {
	CookieStatus
	Value string `json:"value,omitempty"`
}

// CookieJarSnapshot summarises a lemming's cookie jar at the moment of failure.
type CookieJarSnapshot struct {
	TotalCookies   int            `json:"total_cookies"`
	ActiveCookies  int            `json:"active_cookies"`
	ExpiredCookies int            `json:"expired_cookies"`
	Health         string         `json:"health"` // empty, healthy, has-expired
	Cookies        []CookieStatus `json:"cookies,omitempty"`
}

// ReplayCookie is an ephemeral in-memory cookie injected into an isolated CDP
// BrowserContext during failure replay. It is never persisted or logged.
type ReplayCookie struct {
	Name     string
	Value    string
	Domain   string
	Path     string
	Secure   bool
	HTTPOnly bool
	SameSite string
	Expires  float64
}

// SessionSnapshot captures the lemming's identity, sequential number, journey
// position, navigation trail, and cookie jar status when an error triggered.
type SessionSnapshot struct {
	LemmingNumber  int64             `json:"lemming_number"`
	LemmingID      string            `json:"lemming_id"`
	LemmingName    string            `json:"lemming_name"`
	Source         string            `json:"source"` // http-replay or browser-scout
	Terrain        int               `json:"terrain"`
	Pack           int               `json:"pack"`
	Persona        string            `json:"persona"`
	UserAgent      string            `json:"user_agent"`
	Language       string            `json:"language"`
	NavigationMode string            `json:"navigation_mode"`
	JourneyName    string            `json:"journey_name,omitempty"`
	JourneyStep    string            `json:"journey_step,omitempty"`
	VisitSequence  int               `json:"visit_sequence"`
	SessionAgeMS   float64           `json:"session_age_ms"`
	TotalVisits    int64             `json:"total_visits"`
	FailedVisits   int64             `json:"failed_visits"`
	InWaitingRoom  bool              `json:"in_waiting_room"`
	QueuePosition  int               `json:"queue_position,omitempty"`
	Referrer       string            `json:"referrer,omitempty"`
	RecentTrail    []string          `json:"recent_trail,omitempty"`
	CookieJar      CookieJarSnapshot `json:"cookie_jar"`
}

// ConsoleEntry is a single JavaScript console message, uncaught exception, or
// failed network subresource observed by headless Chromium.
type ConsoleEntry struct {
	Level      string    `json:"level"` // error, exception, warning, network, log, info
	Text       string    `json:"text"`
	SourceURL  string    `json:"source_url,omitempty"`
	Line       int       `json:"line,omitempty"`
	Column     int       `json:"column,omitempty"`
	StackTrace string    `json:"stack_trace,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// Screenshot holds metadata for a captured visual screenshot. Data is kept out
// of JSON reports (json:"-") and written as a standalone file or bounded inline
// data URI depending on the report format.
type Screenshot struct {
	MIMEType     string `json:"mime_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256"`
	RelativePath string `json:"image_path,omitempty"`
	Data         []byte `json:"-"`
}

// Extension returns the canonical file extension for s.MIMEType.
func (s *Screenshot) Extension() string {
	if s != nil && strings.EqualFold(s.MIMEType, "image/png") {
		return ".png"
	}
	return ".jpg"
}

// Artifact is the Error Forensics aggregate root combining all four diagnostic
// artifacts (session/cookies/number, JS console, screenshot, extracted error body)
// plus admin triage review state.
type Artifact struct {
	ID             string                 `json:"id"`
	Sequence       int64                  `json:"sequence"`
	Fingerprint    string                 `json:"fingerprint"`
	CapturedAt     time.Time              `json:"captured_at"`
	URL            string                 `json:"url"`
	Path           string                 `json:"path"`
	TriggerReason  string                 `json:"trigger_reason"`
	CaptureMode    string                 `json:"capture_mode"`
	CaptureWarning string                 `json:"capture_warning,omitempty"`
	StatusCode     int                    `json:"status_code"`
	LatencyMS      float64                `json:"latency_ms"`
	Session        SessionSnapshot        `json:"session"`
	Console        []ConsoleEntry         `json:"console,omitempty"`
	ConsoleErrors  int                    `json:"console_errors"`
	ErrorBody      journey.ExtractedError `json:"error_body"`
	Screenshot     *Screenshot            `json:"screenshot,omitempty"`
	ReviewStatus   ReviewStatus           `json:"review_status"`
	ReviewNote     string                 `json:"review_note,omitempty"`
	ReviewedAt     time.Time              `json:"reviewed_at,omitempty"`
}

// Summary is the lightweight projection streamed over SSE and returned by
// GET /api/artifacts so browsers render incoming cards instantly without
// transmitting full image bytes on the event stream.
type Summary struct {
	ID            string       `json:"id"`
	Sequence      int64        `json:"sequence"`
	Fingerprint   string       `json:"fingerprint"`
	CapturedAt    time.Time    `json:"captured_at"`
	Path          string       `json:"path"`
	StatusCode    int          `json:"status_code"`
	TriggerReason string       `json:"trigger_reason"`
	CaptureMode   string       `json:"capture_mode"`
	LemmingNumber int64        `json:"lemming_number"`
	LemmingID     string       `json:"lemming_id"`
	LemmingName   string       `json:"lemming_name"`
	VisitSeq      int          `json:"visit_seq"`
	ErrorHeadline string       `json:"error_headline"`
	ConsoleErrors int          `json:"console_errors"`
	CookieCount   int          `json:"cookie_count"`
	HasScreenshot bool         `json:"has_screenshot"`
	ImageURL      string       `json:"image_url,omitempty"`
	ReviewStatus  ReviewStatus `json:"review_status"`
	ReviewNote    string       `json:"review_note,omitempty"`
}

// ToSummary projects a into a lightweight Summary.
func (a *Artifact) ToSummary() Summary {
	s := Summary{
		ID:            a.ID,
		Sequence:      a.Sequence,
		Fingerprint:   a.Fingerprint,
		CapturedAt:    a.CapturedAt,
		Path:          a.Path,
		StatusCode:    a.StatusCode,
		TriggerReason: a.TriggerReason,
		CaptureMode:   a.CaptureMode,
		LemmingNumber: a.Session.LemmingNumber,
		LemmingID:     a.Session.LemmingID,
		LemmingName:   a.Session.LemmingName,
		VisitSeq:      a.Session.VisitSequence,
		ErrorHeadline: a.ErrorBody.Summary,
		ConsoleErrors: a.ConsoleErrors,
		CookieCount:   a.Session.CookieJar.TotalCookies,
		HasScreenshot: a.Screenshot != nil && len(a.Screenshot.Data) > 0,
		ReviewStatus:  a.ReviewStatus,
		ReviewNote:    a.ReviewNote,
	}
	if s.HasScreenshot {
		s.ImageURL = "/api/artifacts/" + a.ID + "/image"
	}
	return s
}

// PortalView is the detailed artifact DTO returned by GET /api/artifacts/{id}.
// Raw cookie values appear in PortalCookies ONLY when RevealCookies is true.
type PortalView struct {
	Artifact
	ImageURL      string               `json:"image_url,omitempty"`
	RevealCookies bool                 `json:"reveal_cookies"`
	PortalCookies []PortalCookieStatus `json:"portal_cookies,omitempty"`
}

// ReviewUpdate is emitted over SSE whenever an admin updates an artifact's triage state.
type ReviewUpdate struct {
	ID           string       `json:"id"`
	ReviewStatus ReviewStatus `json:"review_status"`
	ReviewNote   string       `json:"review_note,omitempty"`
	ReviewedAt   time.Time    `json:"reviewed_at"`
	Stats        Stats        `json:"stats"`
}

// Stats summarizes capture volume, backpressure counters, and admin triage counts.
type Stats struct {
	Mode            string `json:"mode"`
	Browser         string `json:"browser,omitempty"`
	Captured        int64  `json:"captured"`
	WithScreenshots int64  `json:"with_screenshots"`
	Deduplicated    int64  `json:"deduplicated"`
	DroppedQueue    int64  `json:"dropped_queue"`
	DroppedCap      int64  `json:"dropped_cap"`
	CaptureErrors   int64  `json:"capture_errors"`
	Unreviewed      int64  `json:"unreviewed"`
	Bug             int64  `json:"bug"`
	Flaky           int64  `json:"flaky"`
	Expected        int64  `json:"expected"`
	Ignored         int64  `json:"ignored"`
}

// CaptureRequest carries everything needed by a forensics worker to produce
// an Artifact without retaining references to live lemming state.
type CaptureRequest struct {
	URL           string // full URL for replay (same-origin)
	SafeURL       string // redacted URL for persistence
	Origin        string
	Path          string
	StatusCode    int
	LatencyMS     float64
	TriggerReason string
	ContentType   string
	Headers       http.Header
	RawBody       []byte // owned copy, bounded
	Session       SessionSnapshot
	ReplayCookies []ReplayCookie // ephemeral copy for CDP injection only
	Mobile        bool
}

// Fingerprint computes a stable deduplication key from path, HTTP status,
// and failure trigger signature.
func Fingerprint(path string, status int, triggerReason string) string {
	key := fmt.Sprintf("%s|%d|%s", path, status, strings.ToLower(strings.TrimSpace(triggerReason)))
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// newArtifactID returns a cryptographically random 16-hex-character identifier.
func newArtifactID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano())))
		return hex.EncodeToString(sum[:8])
	}
	return hex.EncodeToString(b[:])
}

// sha256Hex computes lowercase SHA-256 hex digest of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
