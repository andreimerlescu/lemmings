package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// born emits a birth into the observatory.
func born(o *Observatory, id string, terrain int) {
	o.handle(Event{Kind: EventLemmingBorn, LemmingID: id, Terrain: terrain, OccurredAt: time.Now(),
		Identity: &Identity{ID: id, Name: "Test " + id, UserAgent: userAgents[0], Language: "en-US,en;q=0.9"}})
}

// visited emits one visit into the observatory.
func visited(o *Observatory, id string, terrain, status int, failed bool) {
	v := &Visit{LemmingID: id, URL: "https://example.com/p", StatusCode: status, Duration: time.Millisecond, Failed: failed, Sequence: 1}
	o.handle(Event{Kind: EventVisitComplete, LemmingID: id, Terrain: terrain, Visit: v, OccurredAt: time.Now()})
}

// died emits a death into the observatory.
func died(o *Observatory, id string, terrain int, failedVisits int64) {
	o.handle(Event{Kind: EventLemmingDied, LemmingID: id, Terrain: terrain, OccurredAt: time.Now(),
		Life: &LifeLog{TotalVisits: 3, FailedVisits: failedVisits, ExitReason: ExitLifespan}})
}

// TestObservatory_StageSlotsAreBoundedAndReused verifies that only
// stageSize lemmings are on stage, that a death frees its slot, and that
// the next birth takes it.
func TestObservatory_StageSlotsAreBoundedAndReused(t *testing.T) {
	o := NewObservatory(1)
	for i := 0; i < stageSize+10; i++ {
		born(o, fmt.Sprintf("l%03d", i), 0)
	}
	f, _ := o.frame(0, true)
	if len(f.Stage) != stageSize {
		t.Fatalf("stage holds %d lemmings, want %d", len(f.Stage), stageSize)
	}
	offStage, _ := o.life(fmt.Sprintf("l%03d", stageSize+5))
	if offStage.Slot != -1 {
		t.Fatal("a lemming born after the stage filled should not be on stage")
	}

	died(o, "l007", 0, 0)
	born(o, "newcomer", 0)
	f, _ = o.frame(0, true)
	kinds := ""
	for _, ev := range f.Events {
		kinds += ev.Kind
	}
	if kinds != "db" {
		t.Fatalf("expected a death then a birth on stage, got %q", kinds)
	}
	if f.Events[0].Slot != f.Events[1].Slot || f.Events[1].ID != "newcomer" {
		t.Fatalf("the newcomer should take the freed slot: %+v", f.Events)
	}
}

// TestObservatory_CollapsesVisitsPerFrame verifies that many visits by one
// lemming within a frame become one event that keeps the worst outcome,
// and that visits flushed before a death are attributed to the lemming
// that made them.
func TestObservatory_CollapsesVisitsPerFrame(t *testing.T) {
	o := NewObservatory(1)
	born(o, "a", 0)
	o.frame(0, false) // drain the birth

	visited(o, "a", 0, 200, false)
	visited(o, "a", 0, 503, true)
	visited(o, "a", 0, 200, false)
	f, _ := o.frame(0, false)
	if len(f.Events) != 1 || f.Events[0].Count != 3 || f.Events[0].Status != 503 || !f.Events[0].Failed {
		t.Fatalf("expected one collapsed visit keeping the failure, got %+v", f.Events)
	}

	visited(o, "a", 0, 200, false)
	died(o, "a", 0, 1)
	born(o, "b", 0)
	visited(o, "b", 0, 404, true)
	f, _ = o.frame(0, false)
	var order []string
	for _, ev := range f.Events {
		order = append(order, fmt.Sprintf("%s%d", ev.Kind, ev.Status))
	}
	if strings.Join(order, " ") != "v200 d0 b0 v404" {
		t.Fatalf("visits were attributed to the wrong occupant: %v", order)
	}
}

// TestObservatory_TracksLivesAndGraveyard verifies the inspector's view of
// a life and that the graveyard evicts the oldest dead lemming.
func TestObservatory_TracksLivesAndGraveyard(t *testing.T) {
	o := NewObservatory(2)
	born(o, "hero", 1)
	for i := 0; i < lifeHistory+5; i++ {
		visited(o, "hero", 1, 200, false)
	}
	o.handle(Event{Kind: EventWaitingRoomQueued, LemmingID: "hero", Terrain: 1, Position: 12, OccurredAt: time.Now()})
	life, ok := o.life("hero")
	if !ok || life.State != "queued" || life.QueuePos != 12 || life.Visits != lifeHistory+5 ||
		len(life.History) != lifeHistory || life.Omitted != 5 || life.Persona != "Chrome · Windows" {
		t.Fatalf("life: %+v", life)
	}
	died(o, "hero", 1, 0)
	if life, _ = o.life("hero"); life.State != "home" || life.Died == 0 {
		t.Fatalf("a flawless death should be home: %+v", life)
	}

	for i := 0; i < graveyardSize; i++ {
		id := fmt.Sprintf("g%d", i)
		born(o, id, 0)
		died(o, id, 0, 1)
	}
	if _, ok := o.life("hero"); ok {
		t.Fatal("the oldest grave should have been evicted")
	}
	if life, ok := o.life(fmt.Sprintf("g%d", graveyardSize-1)); !ok || life.State != "gone" {
		t.Fatalf("recent dead should stay inspectable: ok=%v state=%s", ok, life.State)
	}
}

// TestObservatory_FeedIsRateLimited verifies that a storm of failures
// produces a readable feed rather than one line per failure.
func TestObservatory_FeedIsRateLimited(t *testing.T) {
	o := NewObservatory(1)
	born(o, "a", 0)
	for i := 0; i < 500; i++ {
		visited(o, "a", 0, 500, true)
	}
	f, seq := o.frame(0, false)
	fails := 0
	for _, it := range f.Feed {
		if it.Kind == "fail" {
			fails++
		}
	}
	if fails != feedBudget["fail"] {
		t.Fatalf("expected %d fail items in a second, got %d", feedBudget["fail"], fails)
	}
	if again, _ := o.frame(seq, false); len(again.Feed) != 0 {
		t.Fatal("feed items were delivered twice")
	}
}

// TestObservatory_TerrainTilesGroupLargeSwarms verifies the terrain map
// stays within maxTerrainTiles and aggregates what it groups.
func TestObservatory_TerrainTilesGroupLargeSwarms(t *testing.T) {
	o := NewObservatory(1000)
	o.handle(Event{Kind: EventTerrainOnline, Terrain: 0})
	born(o, "a", 0)
	born(o, "b", 1)
	visited(o, "a", 0, 500, true)
	tiles := o.terrainTiles()
	if len(tiles) != maxTerrainTiles || tiles[0].From != 0 || tiles[len(tiles)-1].To != 999 {
		t.Fatalf("tiles=%d first=%+v last=%+v", len(tiles), tiles[0], tiles[len(tiles)-1])
	}
	if tiles[0].Alive != 2 || tiles[0].Failed != 1 || tiles[0].State != 1 {
		t.Fatalf("first tile should aggregate terrains 0-3: %+v", tiles[0])
	}
}

// TestDashboard_CSPAllowsExactlyTheInlineAssets verifies that the policy
// hashes match the inline script and style actually served, so the page
// runs, and that nothing else is allowed.
func TestDashboard_CSPAllowsExactlyTheInlineAssets(t *testing.T) {
	for name, tc := range map[string]struct{ page, csp string }{
		"dashboard": {dashboardHTML(testConfig()), pages.dashboardCSP},
		"login":     {authHTML(), pages.loginCSP},
	} {
		t.Run(name, func(t *testing.T) {
			for _, tag := range []string{"script", "style"} {
				rest := tc.page
				for {
					i := strings.Index(rest, "<"+tag+">")
					if i < 0 {
						break
					}
					rest = rest[i+len(tag)+2:]
					body := rest[:strings.Index(rest, "</"+tag+">")]
					sum := sha256.Sum256([]byte(body))
					if h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(tc.csp, h) {
						t.Errorf("inline %s is not allowed by the CSP", tag)
					}
				}
			}
			for _, must := range []string{"default-src 'none'", "frame-ancestors 'none'", "connect-src 'self'"} {
				if !strings.Contains(tc.csp, must) {
					t.Errorf("CSP lacks %q", must)
				}
			}
			if strings.Contains(tc.csp, "unsafe-inline") || strings.Contains(tc.page, "style=\"") || strings.Contains(tc.page, "innerHTML") {
				t.Error("the page must not rely on inline styles, unsafe-inline or innerHTML")
			}
		})
	}
}

// TestDashboard_ConfigCannotBreakOutOfItsScriptTag verifies that a
// hostile -hit value is escaped wherever it lands in the page.
func TestDashboard_ConfigCannotBreakOutOfItsScriptTag(t *testing.T) {
	cfg := testConfig()
	cfg.Hit = `https://example.com/</script><script>alert(1)</script>`
	page := dashboardHTML(cfg)
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("hit URL broke out of its context")
	}
}

// TestDashboard_LocalOnlyRejectsForeignHosts verifies the DNS-rebinding
// defence: only loopback Host headers reach the dashboard.
func TestDashboard_LocalOnlyRejectsForeignHosts(t *testing.T) {
	h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for host, want := range map[string]int{
		"localhost:4000": 200, "127.0.0.1:4000": 200, "[::1]:4000": 200, "localhost": 200,
		"attacker.example:4000": 403, "localhost.attacker.example": 403,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("Host %q: got %d, want %d", host, w.Code, want)
		}
	}
}

// TestDashboard_LemmingAPI verifies the inspector endpoint.
func TestDashboard_LemmingAPI(t *testing.T) {
	d, _ := newTestDashboard(t)
	d.handleEvent(Event{Kind: EventLemmingBorn, LemmingID: "abc", Terrain: 0, OccurredAt: time.Now()})

	req := httptest.NewRequest(http.MethodGet, "/api/lemming?id=abc", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
	w := httptest.NewRecorder()
	d.requireAuth(d.handleLemming)(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"abc"`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/lemming?id=nobody", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookieName, Value: d.tokenHash})
	w = httptest.NewRecorder()
	d.requireAuth(d.handleLemming)(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown lemming: got %d", w.Code)
	}

	w = httptest.NewRecorder()
	d.requireAuth(d.handleLemming)(w, httptest.NewRequest(http.MethodGet, "/api/lemming?id=abc", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d", w.Code)
	}
}

// TestDashboard_EventLogSkipsSuccessfulVisits verifies that the replay
// log keeps lifecycle and failures but not the flood of successes.
func TestDashboard_EventLogSkipsSuccessfulVisits(t *testing.T) {
	d, _ := newTestDashboard(t)
	d.bus.Emit(Event{Kind: EventVisitComplete})
	d.bus.Emit(Event{Kind: EventVisitError, Err: errors.New("boom")})
	d.bus.Emit(Event{Kind: EventLemmingBorn})
	kinds := ""
	for _, e := range d.eventLog.Snapshot() {
		kinds += string(e.Kind) + " "
	}
	if kinds != "visit.error lemming.born " {
		t.Fatalf("replay log kept %q", kinds)
	}
}
