package forensics

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/journey"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
)

// ScoutConfig configures the optional real-browser Scout cohort (-scouts N).
type ScoutConfig struct {
	Count          int
	Origin         string
	Hit            string
	URLs           []string
	Journey        *journey.Journey
	Until          time.Duration
	ThinkMin       time.Duration
	ThinkMax       time.Duration
	NextNumber     func() int64
	FingerprintKey []byte
}

// RunScouts launches cfg.Count real-browser Scout lemmings inside isolated CDP
// incognito BrowserContexts. Each Scout walks the target pages/journey and
// captures screenshots + JS console + cookies + DOM body whenever it detects
// a frontend JS error, uncaught exception, failed subresource, blank render,
// HTTP >= 400 response, or failed journey check — even on HTTP 200 OK pages.
func RunScouts(
	ctx context.Context,
	cfg ScoutConfig,
	cdp *CDPCapturer,
	store *Store,
	bus *events.Bus,
) {
	if cdp == nil || cfg.Count <= 0 {
		return
	}
	count := min(cfg.Count, 8)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runOneScout(ctx, idx, cfg, cdp, store, bus)
		}(i)
	}
	wg.Wait()
}

func runOneScout(
	parent context.Context,
	idx int,
	cfg ScoutConfig,
	cdp *CDPCapturer,
	store *Store,
	bus *events.Bus,
) {
	until := cfg.Until
	if until <= 0 {
		until = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, until)
	defer cancel()

	var num int64
	if cfg.NextNumber != nil {
		num = cfg.NextNumber()
	} else {
		num = int64(idx + 1)
	}
	scoutID := fmt.Sprintf("scout-%02d-%s", idx+1, newArtifactID()[:8])
	scoutName := fmt.Sprintf("Browser Scout #%d", idx+1)
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4_1) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	lang := "en-US,en;q=0.9"
	bornAt := time.Now()

	if bus != nil {
		bus.Emit(events.Event{
			Kind:          events.ScoutStarted,
			LemmingID:     scoutID,
			LemmingNumber: num,
			Terrain:       -1,
			Pack:          idx,
		})
		defer func() {
			bus.Emit(events.Event{
				Kind:          events.ScoutDone,
				LemmingID:     scoutID,
				LemmingNumber: num,
				Terrain:       -1,
				Pack:          idx,
				Duration:      time.Since(bornAt),
			})
		}()
	}

	var bcRes struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := cdp.browser.conn.Call(ctx, "", "Target.createBrowserContext", map[string]any{
		"disposeOnDetach": true,
	}, &bcRes); err != nil {
		return
	}
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanCancel()
		_ = cdp.browser.conn.Call(cleanCtx, "", "Target.disposeBrowserContext", map[string]any{
			"browserContextId": bcRes.BrowserContextID,
		}, nil)
	}()

	var targetRes struct {
		TargetID string `json:"targetId"`
	}
	if err := cdp.browser.conn.Call(ctx, "", "Target.createTarget", map[string]any{
		"url":              "about:blank",
		"width":            1280,
		"height":           800,
		"browserContextId": bcRes.BrowserContextID,
	}, &targetRes); err != nil {
		return
	}
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanCancel()
		_ = cdp.browser.conn.Call(cleanCtx, "", "Target.closeTarget", map[string]any{
			"targetId": targetRes.TargetID,
		}, nil)
	}()

	var attachRes struct {
		SessionID string `json:"sessionId"`
	}
	if err := cdp.browser.conn.Call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetRes.TargetID,
		"flatten":  true,
	}, &attachRes); err != nil {
		return
	}
	sid := attachRes.SessionID
	evCh, unsub := cdp.browser.conn.SubscribeSession(sid)
	defer unsub()

	for _, m := range []string{"Page.enable", "Runtime.enable", "Log.enable", "Network.enable"} {
		_ = cdp.browser.conn.Call(ctx, sid, m, nil, nil)
	}
	_ = cdp.browser.conn.Call(ctx, sid, "Network.setUserAgentOverride", map[string]any{
		"userAgent":      ua,
		"acceptLanguage": lang,
	}, nil)

	targets := buildScoutTargets(cfg, idx)
	var trail []string
	var totalVisits, failedVisits int64
	referrer := ""

	for seq, target := range targets {
		if ctx.Err() != nil {
			break
		}
		visitCtx, visitCancel := context.WithTimeout(ctx, cdp.timeout)
		start := time.Now()
		_ = cdp.browser.conn.Call(visitCtx, sid, "Page.navigate", map[string]any{"url": target.url}, nil)
		consoleEntries, docStatus := collectScoutVisitEvents(visitCtx, evCh, target.url, 600*time.Millisecond)
		latencyMS := float64(time.Since(start)) / float64(time.Millisecond)

		totalVisits++
		path := discovery.PathOf(target.url)
		trail = appendTrail(trail, path, 8)

		domHTML := evalString(visitCtx, cdp.browser.conn, sid, "document.documentElement ? document.documentElement.outerHTML.slice(0, 131072) : ''")
		consoleErrs := countConsoleErrors(consoleEntries)
		evid, _ := journey.InspectPage([]byte(domHTML), "text/html", max(docStatus, 200), target.expect, target.url)

		trigger := scoutTriggerReason(docStatus, consoleEntries, consoleErrs, evid)
		if trigger != "" {
			failedVisits++
			fp := Fingerprint(path, docStatus, trigger)
			if ok, reason := store.TryReserve(fp); ok {
				shot, _ := captureScreenshot(visitCtx, cdp.browser.conn, sid, cdp.format, 1280, 800)
				cookies := fetchCDPCookies(visitCtx, cdp.browser.conn, sid, target.url, cfg.FingerprintKey)
				ex := journey.ExtractErrorBody(docStatus, "text/html", nil, []byte(domHTML))
				art := Artifact{
					ID:            newArtifactID(),
					Fingerprint:   fp,
					CapturedAt:    time.Now(),
					URL:           discovery.SafeURL(target.url),
					Path:          path,
					TriggerReason: trigger,
					CaptureMode:   ModeCDPScout,
					StatusCode:    docStatus,
					LatencyMS:     latencyMS,
					Session: SessionSnapshot{
						LemmingNumber:  num,
						LemmingID:      scoutID,
						LemmingName:    scoutName,
						Source:         "browser-scout",
						Terrain:        -1,
						Pack:           idx,
						Persona:        "Chrome · Headless Scout",
						UserAgent:      ua,
						Language:       lang,
						NavigationMode: "scout",
						JourneyName:    target.journeyName,
						JourneyStep:    target.stepName,
						VisitSequence:  seq + 1,
						SessionAgeMS:   float64(time.Since(bornAt)) / float64(time.Millisecond),
						TotalVisits:    totalVisits,
						FailedVisits:   failedVisits,
						Referrer:       referrer,
						RecentTrail:    append([]string(nil), trail...),
						CookieJar:      cookies,
					},
					Console:       consoleEntries,
					ConsoleErrors: consoleErrs,
					ErrorBody:     ex,
					Screenshot:    shot,
					ReviewStatus:  ReviewUnreviewed,
				}
				summary := store.Save(art)
				if bus != nil {
					bus.Emit(events.Event{
						Kind:          events.ArtifactCaptured,
						LemmingID:     scoutID,
						LemmingNumber: num,
						Terrain:       -1,
						Pack:          idx,
						URL:           art.URL,
						StatusCode:    docStatus,
						ArtifactID:    art.ID,
						CaptureMode:   ModeCDPScout,
						ConsoleErrors: consoleErrs,
						ReviewStatus:  string(ReviewUnreviewed),
						Payload:       summary,
					})
				}
			} else if bus != nil {
				bus.Emit(events.Event{
					Kind:          events.ArtifactDropped,
					LemmingID:     scoutID,
					LemmingNumber: num,
					Terrain:       -1,
					Pack:          idx,
					URL:           discovery.SafeURL(target.url),
					StatusCode:    docStatus,
					DropReason:    reason,
				})
			}
		}
		referrer = discovery.SafeURL(target.url)
		visitCancel()

		if !scoutThink(ctx, cfg.ThinkMin, cfg.ThinkMax) {
			break
		}
	}
}

type scoutTarget struct {
	url         string
	journeyName string
	stepName    string
	expect      journey.Expectations
}

func buildScoutTargets(cfg ScoutConfig, idx int) []scoutTarget {
	if cfg.Journey != nil && len(cfg.Journey.Steps) > 0 {
		out := make([]scoutTarget, 0, len(cfg.Journey.Steps))
		for _, s := range cfg.Journey.Steps {
			if u := discovery.ResolveURL(cfg.Hit, s.Path); u != "" {
				out = append(out, scoutTarget{
					url:         u,
					journeyName: cfg.Journey.Name,
					stepName:    s.Name,
					expect:      s.Expect,
				})
			}
		}
		return out
	}
	urls := append([]string(nil), cfg.URLs...)
	if len(urls) == 0 {
		urls = []string{cfg.Hit}
	}
	// Rotate starting offset per scout so scouts cover different pages immediately.
	start := idx % len(urls)
	ordered := append(urls[start:], urls[:start]...)
	out := make([]scoutTarget, 0, len(ordered))
	for _, u := range ordered {
		out = append(out, scoutTarget{url: u})
	}
	return out
}

func collectScoutVisitEvents(ctx context.Context, evCh <-chan cdpEvent, targetURL string, settle time.Duration) ([]ConsoleEntry, int) {
	var entries []ConsoleEntry
	docStatus := 200
	var settleTimer *time.Timer
	var settleC <-chan time.Time
	defer func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return entries, docStatus
		case <-settleC:
			return entries, docStatus
		case ev, ok := <-evCh:
			if !ok {
				return entries, docStatus
			}
			switch ev.Method {
			case "Page.loadEventFired":
				if settleTimer == nil {
					settleTimer = time.NewTimer(settle)
					settleC = settleTimer.C
				}
			case "Network.responseReceived":
				var p struct {
					Type     string `json:"type"`
					Response struct {
						URL    string `json:"url"`
						Status int    `json:"status"`
					} `json:"response"`
				}
				if err := json.Unmarshal(ev.Params, &p); err == nil {
					if p.Type == "Document" && discovery.PathOf(p.Response.URL) == discovery.PathOf(targetURL) {
						docStatus = p.Response.Status
					} else if p.Response.Status >= 400 && len(entries) < maxConsoleEntries {
						entries = append(entries, ConsoleEntry{
							Level:     "network",
							Text:      fmt.Sprintf("subresource %s returned HTTP %d", discovery.SafeURL(p.Response.URL), p.Response.Status),
							SourceURL: discovery.SafeURL(p.Response.URL),
							Timestamp: time.Now(),
						})
					}
				}
			case "Runtime.consoleAPICalled":
				if entry, ok := parseConsoleAPICalled(ev.Params); ok && len(entries) < maxConsoleEntries {
					entries = append(entries, entry)
				}
			case "Runtime.exceptionThrown":
				if entry, ok := parseExceptionThrown(ev.Params); ok && len(entries) < maxConsoleEntries {
					entries = append(entries, entry)
				}
			case "Log.entryAdded":
				if entry, ok := parseLogEntryAdded(ev.Params); ok && len(entries) < maxConsoleEntries {
					entries = append(entries, entry)
				}
			case "Network.loadingFailed":
				if entry, ok := parseNetworkFailed(ev.Params); ok && len(entries) < maxConsoleEntries {
					entries = append(entries, entry)
				}
			}
		}
	}
}

func scoutTriggerReason(status int, entries []ConsoleEntry, consoleErrs int, evid journey.PageEvidence) string {
	if status >= 400 {
		return fmt.Sprintf("http-%d", status)
	}
	for _, e := range entries {
		if e.Level == "exception" {
			return "js-exception"
		}
	}
	if consoleErrs > 0 {
		for _, e := range entries {
			if e.Level == "error" {
				return "js-console-error"
			}
		}
		return "js-network-error"
	}
	for _, c := range evid.Checks {
		if !c.Passed {
			return "check:" + c.Name
		}
	}
	return ""
}

func fetchCDPCookies(ctx context.Context, conn *cdpConn, sid, targetURL string, fpKey []byte) CookieJarSnapshot {
	var res struct {
		Cookies []struct {
			Name     string  `json:"name"`
			Value    string  `json:"value"`
			Domain   string  `json:"domain"`
			Path     string  `json:"path"`
			Secure   bool    `json:"secure"`
			HTTPOnly bool    `json:"httpOnly"`
			SameSite string  `json:"sameSite"`
			Expires  float64 `json:"expires"`
			Session  bool    `json:"session"`
		} `json:"cookies"`
	}
	if err := conn.Call(ctx, sid, "Network.getCookies", map[string]any{
		"urls": []string{targetURL},
	}, &res); err != nil || len(res.Cookies) == 0 {
		return CookieJarSnapshot{Health: "empty"}
	}

	now := time.Now()
	snap := CookieJarSnapshot{
		TotalCookies: len(res.Cookies),
		Cookies:      make([]CookieStatus, 0, len(res.Cookies)),
	}
	for _, ck := range res.Cookies {
		cs := CookieStatus{
			Name:        ck.Name,
			Domain:      ck.Domain,
			Path:        ck.Path,
			Secure:      ck.Secure,
			HTTPOnly:    ck.HTTPOnly,
			SameSite:    strings.ToLower(ck.SameSite),
			SessionOnly: ck.Session || ck.Expires <= 0,
			ValueBytes:  len(ck.Value),
			Fingerprint: secrets.Fingerprint(ck.Value, fpKey),
			RawValue:    ck.Value,
		}
		if !cs.SessionOnly {
			cs.ExpiresAt = time.Unix(int64(ck.Expires), 0).UTC()
			cs.Expired = cs.ExpiresAt.Before(now)
		}
		if cs.Expired {
			snap.ExpiredCookies++
		} else {
			snap.ActiveCookies++
		}
		snap.Cookies = append(snap.Cookies, cs)
	}
	switch {
	case snap.TotalCookies == 0:
		snap.Health = "empty"
	case snap.ExpiredCookies > 0:
		snap.Health = "has-expired"
	default:
		snap.Health = "healthy"
	}
	return snap
}

func appendTrail(trail []string, path string, maxLen int) []string {
	if len(trail) >= maxLen {
		copy(trail, trail[1:])
		trail[maxLen-1] = path
		return trail
	}
	return append(trail, path)
}

func scoutThink(ctx context.Context, minD, maxD time.Duration) bool {
	d := minD
	if spread := maxD - minD; spread > 0 {
		d += time.Duration(rand.Int64N(int64(spread) + 1))
	}
	if d <= 0 {
		d = 250 * time.Millisecond
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
