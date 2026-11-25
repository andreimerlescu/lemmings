package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/forensics"
	"github.com/andreimerlescu/lemmings/v2/internal/journey"
	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/andreimerlescu/lemmings/v2/internal/swarm"
)

func newTestDash(t *testing.T, revealCookies bool) (*Dashboard, *events.Bus, *forensics.Store, string) {
	t.Helper()
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	store := forensics.NewStore(20, 5)
	cfg := swarm.Config{
		Hit:                  "http://127.0.0.1:8080/",
		Terrain:              2,
		Pack:                 3,
		Limit:                6,
		Until:                10 * time.Second,
		Ramp:                 time.Second,
		Version:              "2.0.0",
		RevealSessionCookies: revealCookies,
	}
	rawToken, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	rep := report.New(cfg)
	d, err := New(cfg, bus, &swarm.Metrics{}, rawToken, rep, store)
	if err != nil {
		t.Fatal(err)
	}
	return d, bus, store, rawToken
}

func authCookieFor(t *testing.T, h http.Handler, rawToken string) *http.Cookie {
	t.Helper()
	form := url.Values{"token": {rawToken}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "http://localhost:4000/auth", strings.NewReader(form))
	req.Host = "localhost:4000"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:50001"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("/auth status = %d, want 303", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("missing session cookie after /auth")
	return nil
}

func TestDashboard_AuthHMACRateLimitLocalOnlyAndCSRF(t *testing.T) {
	d, _, store, rawToken := newTestDash(t, false)
	h := d.Handler()

	// 1. LocalOnly rejects non-loopback Host (DNS rebinding guard).
	evilReq := httptest.NewRequest(http.MethodGet, "http://evil.example.test/", nil)
	evilReq.Host = "evil.example.test"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, evilReq)
	if w.Code != http.StatusForbidden {
		t.Errorf("non-loopback Host status = %d, want 403", w.Code)
	}

	// 2. Successful auth issues signed HMAC cookie (NOT the tokenHash itself!).
	cookie := authCookieFor(t, h, rawToken)
	if cookie.Value == d.TokenHash() || cookie.Value == rawToken {
		t.Error("SEC-05: session cookie must be signed random session ID, not raw token or tokenHash")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie hardening missing: HttpOnly=%v SameSite=%v", cookie.HttpOnly, cookie.SameSite)
	}

	// 3. Forged/tampered cookie is rejected.
	forgedReq := httptest.NewRequest(http.MethodGet, "http://localhost:4000/api/artifacts", nil)
	forgedReq.Host = "localhost:4000"
	forgedReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: d.TokenHash()})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, forgedReq)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("forged tokenHash cookie status = %d, want 401", w.Code)
	}

	// 4. Cross-origin POST to /api/artifacts/{id}/review is rejected by CSRF protection.
	sum := store.Save(forensics.Artifact{Path: "/err", StatusCode: 500, TriggerReason: "status 500"})
	csrfReq := httptest.NewRequest(http.MethodPost, "http://localhost:4000/api/artifacts/"+sum.ID+"/review",
		strings.NewReader(`{"status":"bug","note":"csrf"}`))
	csrfReq.Host = "localhost:4000"
	csrfReq.Header.Set("Origin", "https://attacker.example")
	csrfReq.Header.Set("Sec-Fetch-Site", "cross-site")
	csrfReq.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, csrfReq)
	if w.Code != http.StatusForbidden {
		t.Errorf("SEC-05: cross-origin POST status = %d, want 403", w.Code)
	}

	// 5. Brute-force auth failures trigger 429 rate limit.
	var saw429 bool
	for range 20 {
		bad := httptest.NewRequest(http.MethodPost, "http://localhost:4000/auth", strings.NewReader("token=wrong"))
		bad.Host = "localhost:4000"
		bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		bad.RemoteAddr = "127.0.0.1:59999"
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, bad)
		if rw.Code == http.StatusTooManyRequests {
			saw429 = true
			break
		}
	}
	if !saw429 {
		t.Error("SEC-05: expected 429 rate limit after repeated failed /auth attempts")
	}
}

func TestDashboard_ForensicsEndpointsReviewAndFinalize(t *testing.T) {
	d, _, store, rawToken := newTestDash(t, true)
	h := d.Handler()
	cookie := authCookieFor(t, h, rawToken)

	jpgBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46}
	sum := store.Save(forensics.Artifact{
		Fingerprint:   forensics.Fingerprint("/tokens", 500, "status 500"),
		URL:           "http://127.0.0.1:8080/tokens",
		Path:          "/tokens",
		StatusCode:    500,
		TriggerReason: "status 500",
		CaptureMode:   forensics.ModeCDPReplay,
		Session: forensics.SessionSnapshot{
			LemmingNumber: 3,
			LemmingID:     "t0-p2",
			LemmingName:   "Arcade Lemming",
			CookieJar: forensics.CookieJarSnapshot{
				TotalCookies: 1,
				Cookies:      []forensics.CookieStatus{{Name: "player", Fingerprint: "abc", RawValue: "p99"}},
			},
		},
		Screenshot: &forensics.Screenshot{MIMEType: "image/jpeg", Bytes: len(jpgBytes), Data: jpgBytes},
	})

	// GET /api/artifacts
	req := httptest.NewRequest(http.MethodGet, "http://localhost:4000/api/artifacts?kind=5xx", nil)
	req.Host = "localhost:4000"
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), sum.ID) {
		t.Fatalf("GET /api/artifacts failed: %d %s", w.Code, w.Body.String())
	}

	// GET /api/artifacts/{id} (with RevealSessionCookies=true)
	req = httptest.NewRequest(http.MethodGet, "http://localhost:4000/api/artifacts/"+sum.ID, nil)
	req.Host = "localhost:4000"
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var pv forensics.PortalView
	if err := json.NewDecoder(w.Body).Decode(&pv); err != nil {
		t.Fatal(err)
	}
	if !pv.RevealCookies || len(pv.PortalCookies) != 1 || pv.PortalCookies[0].Value != "p99" {
		t.Errorf("expected revealed cookie in PortalView: %+v", pv)
	}

	// GET /api/artifacts/{id}/image
	req = httptest.NewRequest(http.MethodGet, "http://localhost:4000/api/artifacts/"+sum.ID+"/image", nil)
	req.Host = "localhost:4000"
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), jpgBytes) {
		t.Fatalf("GET image failed: code=%d len=%d", w.Code, w.Body.Len())
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("missing image security headers: %v", w.Header())
	}

	// POST /api/artifacts/{id}/review
	req = httptest.NewRequest(http.MethodPost, "http://localhost:4000/api/artifacts/"+sum.ID+"/review",
		strings.NewReader(`{"status":"bug","note":"Reproduced on staging"}`))
	req.Host = "localhost:4000"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST review failed: %d %s", w.Code, w.Body.String())
	}
	if st := store.Stats(); st.Bug != 1 {
		t.Errorf("expected Bug=1 after review, got %+v", st)
	}

	// EnterReviewMode + POST /api/finalize unblocks channel.
	finCh := d.EnterReviewMode()
	req = httptest.NewRequest(http.MethodPost, "http://localhost:4000/api/finalize", nil)
	req.Host = "localhost:4000"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/finalize failed: %d", w.Code)
	}
	select {
	case <-finCh:
	case <-time.After(time.Second):
		t.Fatal("finalizeCh did not close after POST /api/finalize")
	}
}

func TestDashboard_CSPAllowsExactlyTheInlineAssets(t *testing.T) {
	checkPage := func(name, htmlDoc, csp string) {
		t.Helper()
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s CSP must not contain unsafe-inline/unsafe-eval: %s", name, csp)
		}
		if strings.Contains(htmlDoc, "innerHTML") || strings.Contains(htmlDoc, "outerHTML") || strings.Contains(htmlDoc, "document.write") {
			t.Errorf("%s HTML/JS must never use innerHTML/outerHTML/document.write", name)
		}
		if regexp.MustCompile(`\sstyle=`).MatchString(htmlDoc) {
			t.Errorf("%s HTML must not contain inline style= attributes", name)
		}
		blockRe := regexp.MustCompile(`(?s)<(script|style)([^>]*)>(.*?)</(?:script|style)>`)
		for _, m := range blockRe.FindAllStringSubmatch(htmlDoc, -1) {
			if m[1] == "script" && strings.Contains(m[2], `type="application/json"`) {
				continue // inert JSON config data block, not executed by browser
			}
			sum := sha256.Sum256([]byte(m[3]))
			hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(csp, hash) {
				t.Errorf("%s inline <%s> hash %s not found in CSP %q", name, m[1], hash, csp)
			}
		}
	}

	cfg := swarm.Config{Hit: "http://127.0.0.1:8080/", Version: "2.0.0"}
	checkPage("dashboard", DashboardHTML(cfg), pages.dashboardCSP)
	checkPage("login", AuthHTML(), pages.loginCSP)
}

func TestDashboard_StartAndShutdownClean(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	d, _, _, _ := newTestDash(t, false)
	d.cfg.DashboardPort = port

	addr, err := d.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	resp, err := http.Get(fmt.Sprintf("http://%s/", addr))
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / status = %d, want 200", resp.StatusCode)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func FuzzHandleAuth(f *testing.F) {
	f.Add("token=abc")
	f.Add("")
	f.Add("token=" + strings.Repeat("a", 64))
	f.Fuzz(func(t *testing.T, body string) {
		d, _, _, _ := newTestDash(t, false)
		req := httptest.NewRequest(http.MethodPost, "http://localhost:4000/auth", strings.NewReader(body))
		req.Host = "localhost:4000"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "127.0.0.1:41234"
		w := httptest.NewRecorder()
		d.handleAuth(w, req)
	})
}

func FuzzDashboardHTML(f *testing.F) {
	f.Add("http://localhost:8080/", "2.0.0")
	f.Add(`https://evil.test/"><script>alert(1)</script>`, "1.0")
	f.Fuzz(func(t *testing.T, hit, ver string) {
		out := DashboardHTML(swarm.Config{Hit: hit, Version: ver})
		if strings.Contains(out, "<script>alert(1)</script>") {
			t.Fatal("XSS unescaped in DashboardHTML")
		}
	})
}

func TestObservatory_ScaleInvariants(t *testing.T) {
	t.Run("StageSlotsAreBoundedAndReused", func(t *testing.T) {
		obs := NewObservatory(4)
		for i := 1; i <= StageSize+80; i++ {
			id := fmt.Sprintf("lem-%04d", i)
			ident := swarm.Identity{Number: int64(i), ID: id, Name: "Runner"}
			obs.Handle(events.Event{Kind: events.LemmingBorn, LemmingID: id, LemmingNumber: int64(i), Payload: &ident})
		}
		_, _, stage, _, _ := obs.snapshot()
		if len(stage) != StageSize {
			t.Fatalf("stage slots = %d, want cap %d", len(stage), StageSize)
		}
		// Kill one staged lemming and verify the freed slot is reused.
		victim := stage[0].ID
		obs.Handle(events.Event{Kind: events.LemmingDied, LemmingID: victim, Payload: &swarm.LifeLog{Identity: swarm.Identity{ID: victim}}})
		newID := "lem-reused"
		ident := swarm.Identity{Number: 999, ID: newID, Name: "Fresh"}
		obs.Handle(events.Event{Kind: events.LemmingBorn, LemmingID: newID, LemmingNumber: 999, Payload: &ident})
		life, ok := obs.Life(newID)
		if !ok || life.Slot < 0 {
			t.Fatalf("expected reused stage slot for %s, got ok=%v slot=%d", newID, ok, life.Slot)
		}
	})

	t.Run("CollapsesVisitsPerFrame", func(t *testing.T) {
		obs := NewObservatory(1)
		ident := swarm.Identity{Number: 1, ID: "fast-1", Name: "Sprinter"}
		obs.Handle(events.Event{Kind: events.LemmingBorn, LemmingID: "fast-1", LemmingNumber: 1, Payload: &ident})
		obs.frame(0, false) // clear birth event
		for i := range 25 {
			v := &swarm.Visit{Sequence: i + 1, URL: "http://127.0.0.1/ok", StatusCode: 200, Duration: time.Millisecond}
			obs.Handle(events.Event{Kind: events.VisitComplete, LemmingID: "fast-1", StatusCode: 200, URL: v.URL, Payload: v})
		}
		f, _ := obs.frame(0, false)
		if len(f.Events) != 1 || f.Events[0].Count != 25 {
			t.Errorf("expected 25 visits collapsed into 1 event with count=25, got %+v", f.Events)
		}
	})

	t.Run("TracksLivesAndGraveyardEviction", func(t *testing.T) {
		obs := NewObservatory(1)
		for i := 0; i < graveyardSize+10; i++ {
			id := fmt.Sprintf("dead-%04d", i)
			ident := swarm.Identity{Number: int64(i + 1), ID: id, Name: "Ghost"}
			obs.Handle(events.Event{Kind: events.LemmingBorn, LemmingID: id, LemmingNumber: int64(i + 1), Payload: &ident})
			obs.Handle(events.Event{Kind: events.LemmingDied, LemmingID: id, Payload: &swarm.LifeLog{Identity: ident, ExitReason: swarm.ExitLifespan}})
		}
		if _, exists := obs.Life("dead-0000"); exists {
			t.Error("oldest buried lemming should have been evicted from graveyard")
		}
		if _, exists := obs.Life(fmt.Sprintf("dead-%04d", graveyardSize+9)); !exists {
			t.Error("most recent dead lemming should remain inspectable in graveyard")
		}
	})

	t.Run("FeedIsRateLimited", func(t *testing.T) {
		obs := NewObservatory(1)
		for i := range 50 {
			v := &swarm.Visit{Sequence: i + 1, URL: "http://127.0.0.1/err", StatusCode: 500, Failed: true}
			obs.Handle(events.Event{Kind: events.VisitError, LemmingID: "l1", StatusCode: 500, Failed: true, URL: v.URL, Payload: v})
		}
		f, _ := obs.frame(0, false)
		if len(f.Feed) > feedBudget["fail"] {
			t.Errorf("feed burst exceeded per-second budget: got %d > %d", len(f.Feed), feedBudget["fail"])
		}
	})

	t.Run("TerrainTilesGroupLargeSwarms", func(t *testing.T) {
		obs := NewObservatory(1000)
		obs.Handle(events.Event{Kind: events.TerrainOnline, Terrain: 0})
		obs.Handle(events.Event{Kind: events.TerrainDone, Terrain: 999})
		tiles := obs.terrainTiles()
		if len(tiles) != maxTerrainTiles {
			t.Fatalf("expected %d grouped tiles for 1000 terrains, got %d", maxTerrainTiles, len(tiles))
		}
		if tiles[0].From != 0 || tiles[len(tiles)-1].To != 999 {
			t.Errorf("tiles did not span 0..999: first=%+v last=%+v", tiles[0], tiles[len(tiles)-1])
		}
	})
}

func TestDashboard_ConfigCannotBreakOutOfItsScriptTag(t *testing.T) {
	cfg := swarm.Config{
		Hit:     "http://127.0.0.1:8080/",
		Version: "2.0.0",
		Journey: &journey.Journey{Name: `</script><script>alert(1)</script>`},
	}
	out := DashboardHTML(cfg)
	if strings.Contains(out, `</script><script>alert(1)</script>`) {
		t.Fatal("journey name broke out of JSON <script id=\"config\"> tag")
	}
	if !strings.Contains(out, `\u003c/script\u003e`) {
		t.Error("expected Go json.Marshal HTML escaping (\\u003c/script\\u003e) inside config JSON")
	}
}

func TestDashboard_SSEAndLemmingAPI(t *testing.T) {
	d, bus, store, rawToken := newTestDash(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	d.cfg.DashboardPort = port

	addr, err := d.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = d.Shutdown(shutCtx)
	})

	base := "http://" + addr
	form := url.Values{"token": {rawToken}}.Encode()
	authClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Second,
	}
	resp, err := authClient.Post(base+"/auth", "application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	_ = resp.Body.Close()
	if sessionCookie == nil {
		t.Fatal("missing session cookie")
	}

	// Emit birth and verify /api/lemming (200) and missing id (404).
	ident := swarm.Identity{Number: 5, ID: "lem-api-5", Name: "Laser Fox"}
	bus.Emit(events.Event{Kind: events.LemmingBorn, LemmingID: ident.ID, LemmingNumber: 5, Payload: &ident})

	req, _ := http.NewRequest(http.MethodGet, base+"/api/lemming?id="+ident.ID, nil)
	req.AddCookie(sessionCookie)
	r1, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var life LemmingLife
	_ = json.NewDecoder(r1.Body).Decode(&life)
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusOK || life.Number != 5 || life.ID != ident.ID {
		t.Fatalf("GET /api/lemming failed: status=%d life=%+v", r1.StatusCode, life)
	}

	req404, _ := http.NewRequest(http.MethodGet, base+"/api/lemming?id=missing", nil)
	req404.AddCookie(sessionCookie)
	r404, err := http.DefaultClient.Do(req404)
	if err != nil {
		t.Fatal(err)
	}
	_ = r404.Body.Close()
	if r404.StatusCode != http.StatusNotFound {
		t.Errorf("missing lemming status = %d, want 404", r404.StatusCode)
	}

	// Open SSE stream and deterministically wait for an ArtifactCaptured event.
	sseReq, _ := http.NewRequest(http.MethodGet, base+"/events", nil)
	sseReq.AddCookie(sessionCookie)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()

	gotArtifact := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"kind":"artifact"`) {
				gotArtifact <- line
				return
			}
		}
	}()

	sum := store.Save(forensics.Artifact{Path: "/sse-boom", StatusCode: 500, TriggerReason: "status 500"})
	bus.Emit(events.Event{Kind: events.ArtifactCaptured, ArtifactID: sum.ID, Payload: sum})

	select {
	case line := <-gotArtifact:
		if !strings.Contains(line, sum.ID) {
			t.Errorf("SSE artifact event missing ID %s: %s", sum.ID, line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE artifact event")
	}
}

func TestDashboard_AuthLimiterPerIPGlobalCeilingAndMapCap(t *testing.T) {
	t.Run("PerIPIsolationAndIgnoreXFF", func(t *testing.T) {
		d, _, _, rawToken := newTestDash(t, false)
		h := d.Handler()

		postAuth := func(remoteAddr, xff, tok string) int {
			req := httptest.NewRequest(http.MethodPost, "http://localhost:4000/auth", strings.NewReader("token="+tok))
			req.Host = "localhost:4000"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = remoteAddr
			if xff != "" {
				req.Header.Set("X-Forwarded-For", xff)
			}
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)
			return rw.Code
		}

		// Exhaust IP A's 15/min budget using rotating ephemeral ports & spoofed XFF.
		for i := range maxAuthFailuresPerIPPerMin {
			code := postAuth(fmt.Sprintf("127.0.0.1:%d", 40000+i), fmt.Sprintf("203.0.113.%d", i+1), "wrong")
			if code != http.StatusUnauthorized {
				t.Fatalf("attempt %d from IP A: status = %d, want 401", i+1, code)
			}
		}
		// 16th attempt from IP A (even with fresh spoofed XFF) must be 429.
		if code := postAuth("127.0.0.1:49999", "198.51.100.77", rawToken); code != http.StatusTooManyRequests {
			t.Fatalf("IP A after %d failures: status = %d, want 429", maxAuthFailuresPerIPPerMin, code)
		}
		// IP B (distinct RemoteAddr) must remain unaffected.
		if code := postAuth("[::1]:51000", "127.0.0.1", rawToken); code != http.StatusSeeOther {
			t.Fatalf("IP B must not be blocked by IP A failures: status = %d, want 303", code)
		}
	})

	t.Run("Global60PerMinCeiling", func(t *testing.T) {
		d, _, _, rawToken := newTestDash(t, false)
		h := d.Handler()

		// Spread 60 failures across 5 distinct IPs (12 each, all under per-IP cap 15).
		for ipIdx := range 5 {
			for j := range 12 {
				req := httptest.NewRequest(http.MethodPost, "http://localhost:4000/auth", strings.NewReader("token=bad"))
				req.Host = "localhost:4000"
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.RemoteAddr = fmt.Sprintf("127.0.0.%d:%d", ipIdx+10, 50000+j)
				rw := httptest.NewRecorder()
				h.ServeHTTP(rw, req)
				if rw.Code != http.StatusUnauthorized {
					t.Fatalf("ip %d attempt %d: status = %d, want 401", ipIdx, j+1, rw.Code)
				}
			}
		}

		// A brand-new 6th IP with 0 prior failures must hit the 60/min global ceiling.
		req := httptest.NewRequest(http.MethodPost, "http://localhost:4000/auth", strings.NewReader("token="+rawToken))
		req.Host = "localhost:4000"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "127.0.0.99:55555"
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusTooManyRequests {
			t.Fatalf("6th IP after 60 global failures: status = %d, want 429", rw.Code)
		}
		if rw.Header().Get("Retry-After") != "60" {
			t.Errorf("Retry-After = %q, want 60", rw.Header().Get("Retry-After"))
		}
	})

	t.Run("Bounded1024IPMapCapAndExpiredEviction", func(t *testing.T) {
		var lim authLimiter
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		// Fill 1024 active IPs at distinct instants.
		for i := range maxAuthTrackedIPs {
			ip := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
			lim.recordFailure(ip, base.Add(time.Duration(i)*time.Millisecond))
		}
		if got := len(lim.byIP); got != maxAuthTrackedIPs {
			t.Fatalf("tracked IPs = %d, want %d", got, maxAuthTrackedIPs)
		}

		// Adding 200 more IPs within the window must never exceed maxAuthTrackedIPs (1024).
		for i := range 200 {
			ip := fmt.Sprintf("10.1.%d.%d", i/256, i%256)
			lim.recordFailure(ip, base.Add(2*time.Second+time.Duration(i)*time.Millisecond))
			if len(lim.byIP) > maxAuthTrackedIPs {
				t.Fatalf("tracked IPs exceeded cap: %d > %d", len(lim.byIP), maxAuthTrackedIPs)
			}
		}
		// Oldest entry 10.0.0.0 should have been evicted.
		if _, exists := lim.byIP["10.0.0.0"]; exists {
			t.Error("expected oldest IP 10.0.0.0 to be evicted once cap was reached")
		}

		// After >1 minute, expired entries are purged cleanly on the next record.
		later := base.Add(2 * time.Minute)
		lim.recordFailure("10.9.9.9", later)
		if got := len(lim.byIP); got != 1 {
			t.Fatalf("expected expired IPs purged leaving 1 active IP, got %d", got)
		}
		if !lim.allow("10.9.9.9", later) {
			t.Error("expected single failure after window reset to still be allowed")
		}
	})
}

func TestDashboard_LiveMetricsJSONIncludesConcertAndAssets(t *testing.T) {
	d, bus, _, rawToken := newTestDash(t, false)
	d.cfg.Assets = true
	d.cfg.AssetsPerPage = 8

	// Populate both Swarm.Metrics and Reporter (via EventBus) to confirm full wiring.
	rep := d.stats.(*report.Reporter)
	rep.Attach(bus)

	d.metrics.LemmingsStarted.Store(14)
	d.metrics.LemmingsCompleted.Store(10)
	d.metrics.LemmingsAlive.Store(4)
	d.metrics.AssetRequests.Store(22)
	d.metrics.AssetFailed.Store(3)
	d.metrics.AssetDenied403.Store(2)
	d.metrics.AssetBytes.Store(48120)
	d.metrics.QueuePolls.Store(19)
	d.metrics.Bans429.Store(4)
	d.metrics.Bans403.Store(1)
	d.metrics.ScoutVisits.Store(7)
	d.metrics.ScoutFailures.Store(2)

	h := d.Handler()
	cookie := authCookieFor(t, h, rawToken)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:4000/metrics", nil)
	req.Host = "localhost:4000"
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", w.Code)
	}

	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode /metrics: %v", err)
	}
	want := map[string]float64{
		"lemmings_started": 14,
		"asset_requests":   22,
		"asset_failed":     3,
		"asset_denied_403": 2,
		"asset_bytes":      48120,
		"queue_polls":      19,
		"bans_429":         4,
		"bans_403":         1,
		"scout_visits":     7,
		"scout_failures":   2,
	}
	for k, wantVal := range want {
		got, ok := raw[k].(float64)
		if !ok || got != wantVal {
			t.Errorf("/metrics[%q] = %v (present=%v), want %v", k, raw[k], ok, wantVal)
		}
	}

	// Verify /api/state embeds both the expanded metricsSnapshot and LiveStats.
	req = httptest.NewRequest(http.MethodGet, "http://localhost:4000/api/state", nil)
	req.Host = "localhost:4000"
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/state status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"lemmings_started":14`) || !strings.Contains(w.Body.String(), `"asset_denied_403":2`) {
		t.Errorf("/api/state missing new metricsSnapshot fields: %s", w.Body.String())
	}
}

func BenchmarkObservatory_Frame(b *testing.B) {
	obs := NewObservatory(16)
	for i := range StageSize {
		id := fmt.Sprintf("bench-%03d", i)
		ident := swarm.Identity{Number: int64(i + 1), ID: id, Name: "Runner"}
		obs.Handle(events.Event{Kind: events.LemmingBorn, LemmingID: id, LemmingNumber: int64(i + 1), Payload: &ident})
	}
	v := &swarm.Visit{Sequence: 1, URL: "http://127.0.0.1/games", StatusCode: 200, Duration: 5 * time.Millisecond}
	b.ResetTimer()
	for i := range b.N {
		obs.Handle(events.Event{Kind: events.VisitComplete, LemmingID: fmt.Sprintf("bench-%03d", i%StageSize), StatusCode: 200, URL: v.URL, Payload: v})
		if i%50 == 0 {
			obs.frame(0, false)
		}
	}
}
