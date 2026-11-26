package forensics

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
)

const (
	defaultMaxArtifacts = 50
	defaultMaxPerError  = 3
	maxReviewNoteBytes  = 500
)

// ErrNotFound is returned when an artifact ID does not exist in the Store.
var ErrNotFound = errors.New("artifact not found")

// ErrInvalidReviewStatus is returned when a triage status is outside the allowed enum.
var ErrInvalidReviewStatus = errors.New("invalid review status")

// FilterOptions filters artifacts returned by Store.List.
type FilterOptions struct {
	Status string // review status filter
	Kind   string // 5xx, 4xx, js, check, transport
	Query  string // substring match on path, headline, or lemming name
}

// Store is a bounded, concurrency-safe repository of captured Artifact aggregates.
// One mutex guards every multi-field invariant (reservations, caps, counts, reviews).
type Store struct {
	mu           sync.Mutex
	maxTotal     int
	maxPerFP     int
	reserved     int
	fpReserved   map[string]int
	seq          int64
	order        []string
	byID         map[string]*Artifact
	byVisit      map[string]string // "lemmingID:seq" -> artifactID
	mode         string
	browser      string
	deduplicated int64
	droppedQueue int64
	droppedCap   int64
	captureErrs  int64
}

// NewStore creates a bounded Store enforcing maxTotal artifacts and maxPerFP
// captures per distinct failure fingerprint.
func NewStore(maxTotal, maxPerFP int) *Store {
	if maxTotal <= 0 {
		maxTotal = defaultMaxArtifacts
	}
	if maxPerFP <= 0 {
		maxPerFP = defaultMaxPerError
	}
	return &Store{
		maxTotal:   maxTotal,
		maxPerFP:   maxPerFP,
		fpReserved: make(map[string]int),
		byID:       make(map[string]*Artifact),
		byVisit:    make(map[string]string),
		mode:       ModeHTTPOnly,
	}
}

// SetRuntime records the active capture mode and browser description for reports/portal.
func (s *Store) SetRuntime(mode, browser string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
	s.browser = browser
}

// TryReserve atomically checks the global and per-fingerprint caps before
// scheduling capture work. Returns (true, "") on success, or (false, dropReason).
func (s *Store) TryReserve(fp string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reserved >= s.maxTotal {
		s.droppedCap++
		return false, DropCapReached
	}
	if s.fpReserved[fp] >= s.maxPerFP {
		s.deduplicated++
		return false, DropDeduped
	}
	s.reserved++
	s.fpReserved[fp]++
	return true, ""
}

// ReleaseReservation frees a reserved slot if enqueue failed before capture ran.
func (s *Store) ReleaseReservation(fp string, recordQueueDrop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserved > 0 {
		s.reserved--
	}
	if s.fpReserved[fp] > 0 {
		s.fpReserved[fp]--
	}
	if recordQueueDrop {
		s.droppedQueue++
	}
}

// RecordCaptureError increments the capture failure counter.
func (s *Store) RecordCaptureError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captureErrs++
}

// Save stores a captured Artifact, assigns its sequence and ID if empty, and
// returns its lightweight Summary.
func (s *Store) Save(a Artifact) Summary {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	a.Sequence = s.seq
	if a.ID == "" {
		a.ID = newArtifactID()
	}
	if a.CapturedAt.IsZero() {
		a.CapturedAt = time.Now()
	}
	if !ValidReviewStatus(a.ReviewStatus) {
		a.ReviewStatus = ReviewUnreviewed
	}
	// Defensive copy of screenshot bytes & slices so caller cannot mutate stored state.
	stored := deepCopyArtifact(&a, true)
	s.byID[stored.ID] = &stored
	s.order = append(s.order, stored.ID)
	if stored.Session.LemmingID != "" && stored.Session.VisitSequence > 0 {
		vk := visitKey(stored.Session.LemmingID, stored.Session.VisitSequence)
		if _, exists := s.byVisit[vk]; !exists {
			s.byVisit[vk] = stored.ID
		}
	}
	return stored.ToSummary()
}

// Review updates the admin triage status and optional note on an artifact.
func (s *Store) Review(id string, status ReviewStatus, note string) (ReviewUpdate, error) {
	if !ValidReviewStatus(status) {
		return ReviewUpdate{}, fmt.Errorf("%w: %q", ErrInvalidReviewStatus, status)
	}
	cleanNote := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(note))
	cleanNote = discovery.Clip(cleanNote, maxReviewNoteBytes)

	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.byID[id]
	if !ok {
		return ReviewUpdate{}, ErrNotFound
	}
	a.ReviewStatus = status
	a.ReviewNote = cleanNote
	a.ReviewedAt = time.Now()

	return ReviewUpdate{
		ID:           a.ID,
		ReviewStatus: a.ReviewStatus,
		ReviewNote:   a.ReviewNote,
		ReviewedAt:   a.ReviewedAt,
		Stats:        s.statsLocked(),
	}, nil
}

// List returns matching artifact summaries (newest first) and overall store Stats.
func (s *Store) List(f FilterOptions) ([]Summary, Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q := strings.ToLower(strings.TrimSpace(f.Query))
	out := make([]Summary, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		a := s.byID[s.order[i]]
		if a == nil || !matchesFilter(a, f.Status, f.Kind, q) {
			continue
		}
		out = append(out, a.ToSummary())
	}
	return out, s.statsLocked()
}

// GetForPortal returns a portal DTO for id. Raw cookie values are copied ONLY
// when revealCookies is true (gated by -reveal-session-cookies on localhost).
func (s *Store) GetForPortal(id string, revealCookies bool) (PortalView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.byID[id]
	if !ok {
		return PortalView{}, false
	}
	copied := deepCopyArtifact(a, false) // strip RawValue from Artifact itself
	view := PortalView{
		Artifact:      copied,
		RevealCookies: revealCookies,
	}
	if a.Screenshot != nil && len(a.Screenshot.Data) > 0 {
		view.ImageURL = "/api/artifacts/" + a.ID + "/image"
	}
	if len(a.Session.CookieJar.Cookies) > 0 {
		view.PortalCookies = make([]PortalCookieStatus, len(a.Session.CookieJar.Cookies))
		for i, c := range a.Session.CookieJar.Cookies {
			pc := PortalCookieStatus{CookieStatus: c}
			pc.RawValue = ""
			if revealCookies {
				pc.Value = c.RawValue
			}
			view.PortalCookies[i] = pc
		}
	}
	return view, true
}

// Image returns a defensive copy of an artifact's screenshot bytes and MIME type.
func (s *Store) Image(id string) ([]byte, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.byID[id]
	if !ok || a.Screenshot == nil || len(a.Screenshot.Data) == 0 {
		return nil, "", false
	}
	buf := make([]byte, len(a.Screenshot.Data))
	copy(buf, a.Screenshot.Data)
	return buf, a.Screenshot.MIMEType, true
}

// FindByVisit returns the artifact ID captured for a specific lemming visit, if any.
func (s *Store) FindByVisit(lemmingID string, seq int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byVisit[visitKey(lemmingID, seq)]
	return id, ok
}

// Stats returns a snapshot of capture and review counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsLocked()
}

// SnapshotForReport returns all captured artifacts in chronological order with
// ALL raw cookie values stripped (S5/S6), safe for HTML, Markdown, JSON, S3, and Email.
func (s *Store) SnapshotForReport() ([]Artifact, Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Artifact, 0, len(s.order))
	for _, id := range s.order {
		if a := s.byID[id]; a != nil {
			out = append(out, deepCopyArtifact(a, false))
		}
	}
	return out, s.statsLocked()
}

func (s *Store) statsLocked() Stats {
	st := Stats{
		Mode:          s.mode,
		Browser:       s.browser,
		Captured:      int64(len(s.order)),
		Deduplicated:  s.deduplicated,
		DroppedQueue:  s.droppedQueue,
		DroppedCap:    s.droppedCap,
		CaptureErrors: s.captureErrs,
	}
	for _, id := range s.order {
		a := s.byID[id]
		if a == nil {
			continue
		}
		if a.Screenshot != nil && len(a.Screenshot.Data) > 0 {
			st.WithScreenshots++
		}
		switch a.ReviewStatus {
		case ReviewBug:
			st.Bug++
		case ReviewFlaky:
			st.Flaky++
		case ReviewExpected:
			st.Expected++
		case ReviewIgnored:
			st.Ignored++
		default:
			st.Unreviewed++
		}
	}
	return st
}

func matchesFilter(a *Artifact, status, kind, q string) bool {
	if status != "" && status != "all" && string(a.ReviewStatus) != status {
		return false
	}
	switch kind {
	case "", "all":
	case "5xx":
		if a.StatusCode < 500 || a.StatusCode >= 600 {
			return false
		}
	case "4xx":
		if a.StatusCode < 400 || a.StatusCode >= 500 {
			return false
		}
	case "js":
		if a.ConsoleErrors == 0 && !strings.HasPrefix(a.TriggerReason, "js") {
			return false
		}
	case "check":
		if !strings.HasPrefix(a.TriggerReason, "check:") && a.TriggerReason != "not-blank" {
			return false
		}
	case "transport":
		if a.StatusCode != 0 {
			return false
		}
	}
	if q != "" {
		hay := strings.ToLower(a.Path + " " + a.ErrorBody.Summary + " " + a.Session.LemmingName + " " + a.TriggerReason)
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

func visitKey(lemmingID string, seq int) string {
	return fmt.Sprintf("%s:%d", lemmingID, seq)
}

func deepCopyArtifact(a *Artifact, keepRawCookies bool) Artifact {
	c := *a
	if len(a.Session.RecentTrail) > 0 {
		c.Session.RecentTrail = append([]string(nil), a.Session.RecentTrail...)
	}
	if len(a.Session.CookieJar.Cookies) > 0 {
		c.Session.CookieJar.Cookies = make([]CookieStatus, len(a.Session.CookieJar.Cookies))
		for i, ck := range a.Session.CookieJar.Cookies {
			if !keepRawCookies {
				ck.RawValue = ""
			}
			c.Session.CookieJar.Cookies[i] = ck
		}
	}
	if len(a.Console) > 0 {
		c.Console = append([]ConsoleEntry(nil), a.Console...)
	}
	if len(a.ErrorBody.Details) > 0 {
		c.ErrorBody.Details = append([]string(nil), a.ErrorBody.Details...)
	}
	if len(a.ErrorBody.ResponseHeaders) > 0 {
		hdrs := make(map[string]string, len(a.ErrorBody.ResponseHeaders))
		for k, v := range a.ErrorBody.ResponseHeaders {
			hdrs[k] = v
		}
		c.ErrorBody.ResponseHeaders = hdrs
	}
	if a.Screenshot != nil {
		sc := *a.Screenshot
		if len(a.Screenshot.Data) > 0 {
			sc.Data = make([]byte, len(a.Screenshot.Data))
			copy(sc.Data, a.Screenshot.Data)
		}
		c.Screenshot = &sc
	}
	return c
}
