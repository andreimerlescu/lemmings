package forensics

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"mime"
	"strings"
	"sync"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/events"
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
)

// ScoutConfig configures the optional real-browser Scout cohort (-scouts N).
type ScoutConfig struct {
	Count          int
	Origin         string
	Hit            string
	URLs           []string
	Plan           *plan.Suite
	Until          time.Duration
	ThinkMin       time.Duration
	ThinkMax       time.Duration
	NextNumber     func() int64
	FingerprintKey []byte
}

// RunScouts launches cfg.Count real-browser Scout lemmings inside isolated CDP
// incognito BrowserContexts. Each Scout walks the target pages/plan and
// captures screenshots + JS console + cookies + DOM body whenever it detects
// a frontend JS error, uncaught exception, failed subresource, blank render,
// HTTP >= 400 response, or failed plan check — even on HTTP 200 OK pages.
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
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if cfg.Until > 0 {
		ctx, cancel = context.WithTimeout(parent, cfg.Until)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
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
			Role:          "scout",
		})
		defer func() {
			bus.Emit(events.Event{
				Kind:          events.ScoutDone,
				LemmingID:     scoutID,
				LemmingNumber: num,
				Terrain:       -1,
				Pack:          idx,
				Duration:      time.Since(bornAt),
				Role:          "scout",
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

	origin := strings.TrimSpace(cfg.Origin)
	if origin == "" && cfg.Hit != "" {
		origin, _ = discovery.ParseOrigin(cfg.Hit)
	}

	targets := buildScoutTargets(cfg, idx)
	if len(targets) == 0 {
		return
	}
	isPlan := cfg.Plan != nil && len(cfg.Plan.Plans) > 0 && !cfg.Plan.HasNonGET()
	var trail []string
	var totalVisits, failedVisits int64
	referrer := ""

	for seq := 0; ctx.Err() == nil; seq++ {
		if isPlan && seq >= len(targets) {
			break
		}
		target := targets[seq%len(targets)]
		visitCtx, visitCancel := context.WithTimeout(ctx, cdp.timeout)
		start := time.Now()

		// Drain late CDP events from previous page so they never bleed into this visit.
		drainCDPEvents(evCh)

		var navRes struct {
			FrameID   string `json:"frameId"`
			ErrorText string `json:"errorText"`
		}
		navErr := cdp.browser.conn.Call(visitCtx, sid, "Page.navigate", map[string]any{"url": target.url}, &navRes)

		var (
			evRes        scoutVisitResult
			navErrorText string
			docStatus    int
			docCT        string
		)
		if navErr != nil {
			navErrorText = navErr.Error()
		} else {
			eventCtx, eventCancel := eventCollectionContext(visitCtx, cdp.timeout)
			evRes = collectScoutVisitEvents(eventCtx, evCh, navRes.FrameID, 600*time.Millisecond)
			eventCancel()
			if navRes.ErrorText != "" {
				navErrorText = navRes.ErrorText
				docStatus = 0
			} else {
				docStatus = evRes.docStatus
				docCT = evRes.contentType
			}
		}

		dur := time.Since(start)
		latencyMS := float64(dur) / float64(time.Millisecond)

		totalVisits++
		path := discovery.PathOf(target.url)
		trail = appendTrail(trail, path, 8)
		safeTargetURL := discovery.SafeURL(target.url)

		// Determine actual final URL and verify it did not escape target origin
		// before touching DOM, cookies, or screenshots.
		observedURL := evRes.frameURL
		if observedURL == "" {
			observedURL = evRes.docURL
		}
		finalURL := observedURL
		offOrigin := origin != "" && observedURL != "" && !scoutOriginAllowed(origin, observedURL, navErrorText != "")
		if !offOrigin && navErr == nil {
			if locURL := evalString(visitCtx, cdp.browser.conn, sid, "window.location.href"); locURL != "" {
				finalURL = locURL
			}
		}
		if finalURL == "" {
			finalURL = target.url
		}
		if origin != "" && !scoutOriginAllowed(origin, finalURL, navErrorText != "") {
			offOrigin = true
		}

		if offOrigin {
			failedVisits++
			warnMsg := fmt.Sprintf("scout redirected off target origin to %s; DOM, cookies, and screenshot skipped", discovery.SafeURL(finalURL))
			visitErr := fmt.Errorf("%s", warnMsg)
			if store != nil {
				fp := Fingerprint(path, docStatus, "off-origin-redirect")
				if ok, reason := store.TryReserve(fp); ok {
					art := Artifact{
						ID:             newArtifactID(),
						Fingerprint:    fp,
						CapturedAt:     time.Now(),
						URL:            safeTargetURL,
						Path:           path,
						TriggerReason:  "off-origin-redirect",
						CaptureMode:    ModeCDPScout,
						CaptureWarning: warnMsg,
						StatusCode:     docStatus,
						LatencyMS:      latencyMS,
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
							PlanName:       target.planName,
							PlanStep:       target.stepName,
							Method:         "GET",
							Role:           "scout",
							VisitSequence:  seq + 1,
							SessionAgeMS:   float64(time.Since(bornAt)) / float64(time.Millisecond),
							TotalVisits:    totalVisits,
							FailedVisits:   failedVisits,
							Referrer:       referrer,
							RecentTrail:    append([]string(nil), trail...),
							CookieJar:      CookieJarSnapshot{Health: "empty"},
						},
						ErrorBody: plan.ExtractedError{
							Format:     "empty",
							StatusCode: docStatus,
							Summary:    warnMsg,
						},
						ReviewStatus: ReviewUnreviewed,
					}
					summary := store.Save(art)
					if bus != nil {
						bus.Emit(events.Event{
							Kind:          events.ArtifactCaptured,
							LemmingID:     scoutID,
							LemmingNumber: num,
							Terrain:       -1,
							Pack:          idx,
							Method:        "GET",
							URL:           art.URL,
							StatusCode:    docStatus,
							PlanName:      target.planName,
							Role:          "scout",
							ArtifactID:    art.ID,
							CaptureMode:   ModeCDPScout,
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
						Method:        "GET",
						URL:           safeTargetURL,
						StatusCode:    docStatus,
						PlanName:      target.planName,
						Role:          "scout",
						DropReason:    reason,
					})
				}
			}
			if bus != nil {
				bus.Emit(events.Event{
					Kind:          events.ScoutVisit,
					LemmingID:     scoutID,
					LemmingNumber: num,
					Terrain:       -1,
					Pack:          idx,
					Method:        "GET",
					URL:           safeTargetURL,
					StatusCode:    docStatus,
					Duration:      dur,
					Failed:        true,
					PlanName:      target.planName,
					Role:          "scout",
					Err:           visitErr,
				})
			}
			referrer = safeTargetURL
			visitCancel()

			if isPlan && seq+1 >= len(targets) {
				break
			}
			if !scoutThink(ctx, cfg.ThinkMin, cfg.ThinkMax) {
				break
			}
			continue
		}

		var pageBody string
		if navErr == nil {
			pageBody = evalString(visitCtx, cdp.browser.conn, sid, scoutBodyExpr(docCT))
		}
		consoleEntries := evRes.entries
		consoleErrs := countConsoleErrors(consoleEntries)
		evid, _ := plan.InspectPage([]byte(pageBody), docCT, docStatus, target.step, target.url)

		trigger := scoutTriggerReason(navErrorText, docStatus, consoleEntries, consoleErrs, evid)
		visitFailed := trigger != ""
		var visitErr error
		if visitFailed {
			failedVisits++
			if navErr != nil {
				visitErr = navErr
			} else if navErrorText != "" {
				visitErr = fmt.Errorf("navigation error: %s", navErrorText)
			} else {
				visitErr = fmt.Errorf("scout trigger: %s", trigger)
			}
			if store != nil {
				fp := Fingerprint(path, docStatus, trigger)
				if ok, reason := store.TryReserve(fp); ok {
					shot, _ := captureScreenshot(visitCtx, cdp.browser.conn, sid, cdp.format, 1280, 800)
					cookies := fetchCDPCookies(visitCtx, cdp.browser.conn, sid, target.url, cfg.FingerprintKey)
					ex := plan.ExtractErrorBody(docStatus, docCT, nil, []byte(pageBody))
					var capWarn string
					if navErrorText != "" {
						capWarn = "navigation error: " + navErrorText
					}
					art := Artifact{
						ID:             newArtifactID(),
						Fingerprint:    fp,
						CapturedAt:     time.Now(),
						URL:            safeTargetURL,
						Path:           path,
						TriggerReason:  trigger,
						CaptureMode:    ModeCDPScout,
						CaptureWarning: capWarn,
						StatusCode:     docStatus,
						LatencyMS:      latencyMS,
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
							PlanName:       target.planName,
							PlanStep:       target.stepName,
							Method:         "GET",
							Role:           "scout",
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
							Method:        "GET",
							URL:           art.URL,
							StatusCode:    docStatus,
							PlanName:      target.planName,
							Role:          "scout",
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
						Method:        "GET",
						URL:           safeTargetURL,
						StatusCode:    docStatus,
						PlanName:      target.planName,
						Role:          "scout",
						DropReason:    reason,
					})
				}
			}
		}
		if bus != nil {
			bus.Emit(events.Event{
				Kind:          events.ScoutVisit,
				LemmingID:     scoutID,
				LemmingNumber: num,
				Terrain:       -1,
				Pack:          idx,
				Method:        "GET",
				URL:           safeTargetURL,
				StatusCode:    docStatus,
				Duration:      dur,
				Failed:        visitFailed,
				Cancelled:     ctx.Err() != nil && navErr != nil,
				PlanName:      target.planName,
				Role:          "scout",
				Err:           visitErr,
			})
		}
		referrer = safeTargetURL
		visitCancel()

		if isPlan && seq+1 >= len(targets) {
			break
		}
		if !scoutThink(ctx, cfg.ThinkMin, cfg.ThinkMax) {
			break
		}
	}
}

type scoutTarget struct {
	url      string
	planName string
	stepName string
	step     *plan.PlanItem
}

func buildScoutTargets(cfg ScoutConfig, idx int) []scoutTarget {
	if cfg.Plan != nil && len(cfg.Plan.Plans) > 0 && !cfg.Plan.HasNonGET() {
		if p := cfg.Plan.Pick(int64(idx)); p != nil && len(p.Items) > 0 {
			out := make([]scoutTarget, 0, len(p.Items))
			for i := range p.Items {
				item := &p.Items[i]
				if u := discovery.ResolveURL(cfg.Hit, item.Path); u != "" {
					out = append(out, scoutTarget{
						url:      u,
						planName: p.Name,
						stepName: item.Name,
						step:     item,
					})
				}
			}
			return out
		}
	}
	urls := append([]string(nil), cfg.URLs...)
	if len(urls) == 0 {
		if cfg.Hit != "" {
			urls = []string{cfg.Hit}
		} else {
			return nil
		}
	}
	// Rotate starting offset per scout so scouts cover different pages immediately.
	start := idx % len(urls)
	if start < 0 {
		start += len(urls)
	}
	ordered := make([]string, 0, len(urls))
	ordered = append(ordered, urls[start:]...)
	ordered = append(ordered, urls[:start]...)
	out := make([]scoutTarget, 0, len(ordered))
	for _, u := range ordered {
		out = append(out, scoutTarget{url: u})
	}
	return out
}

type scoutVisitResult struct {
	entries     []ConsoleEntry
	docStatus   int
	contentType string
	docURL      string
	frameURL    string
}

func drainCDPEvents(evCh <-chan cdpEvent) {
	for {
		select {
		case _, ok := <-evCh:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func scoutOriginAllowed(origin, finalURL string, allowBlank bool) bool {
	if origin == "" || finalURL == "" {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(finalURL))
	if strings.HasPrefix(lower, "chrome-error://") {
		return true
	}
	if allowBlank && lower == "about:blank" {
		return true
	}
	return discovery.SameOrigin(origin, finalURL)
}

func scoutBodyExpr(contentType string) string {
	media, _, _ := mime.ParseMediaType(contentType)
	media = strings.ToLower(media)
	if media == "" || media == "text/html" || media == "application/xhtml+xml" {
		return "document.documentElement ? document.documentElement.outerHTML.slice(0, 131072) : ''"
	}
	return "(document.body && document.body.innerText) ? document.body.innerText.slice(0, 131072) : (document.documentElement ? document.documentElement.outerHTML.slice(0, 131072) : '')"
}

func responseContentType(headers map[string]any, mimeType string) string {
	for k, v := range headers {
		if strings.EqualFold(k, "content-type") {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return strings.TrimSpace(mimeType)
}

func collectScoutVisitEvents(ctx context.Context, evCh <-chan cdpEvent, mainFrameID string, settle time.Duration) scoutVisitResult {
	res := scoutVisitResult{docStatus: 0}
	var settleTimer *time.Timer
	var settleC <-chan time.Time
	defer func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}()

	handle := func(ev cdpEvent, allowTimer bool) {
		switch ev.Method {
		case "Page.loadEventFired":
			if allowTimer && settleTimer == nil {
				settleTimer = time.NewTimer(settle)
				settleC = settleTimer.C
			}
		case "Page.frameNavigated":
			var p struct {
				Frame struct {
					ID       string `json:"id"`
					ParentID string `json:"parentId"`
					URL      string `json:"url"`
				} `json:"frame"`
			}
			if err := json.Unmarshal(ev.Params, &p); err == nil {
				if p.Frame.ParentID == "" && (mainFrameID == "" || p.Frame.ID == "" || p.Frame.ID == mainFrameID) && p.Frame.URL != "" {
					res.frameURL = p.Frame.URL
				}
			}
		case "Network.responseReceived":
			var p struct {
				Type     string `json:"type"`
				FrameID  string `json:"frameId"`
				Response struct {
					URL      string         `json:"url"`
					Status   int            `json:"status"`
					MIMEType string         `json:"mimeType"`
					Headers  map[string]any `json:"headers"`
				} `json:"response"`
			}
			if err := json.Unmarshal(ev.Params, &p); err == nil {
				if p.Type == "Document" && (mainFrameID == "" || p.FrameID == "" || p.FrameID == mainFrameID) {
					res.docStatus = p.Response.Status
					res.docURL = p.Response.URL
					if ct := responseContentType(p.Response.Headers, p.Response.MIMEType); ct != "" {
						res.contentType = ct
					}
				} else if p.Response.Status >= 400 && len(res.entries) < maxConsoleEntries {
					res.entries = append(res.entries, ConsoleEntry{
						Level:     "network",
						Text:      fmt.Sprintf("subresource %s returned HTTP %d", discovery.SafeURL(p.Response.URL), p.Response.Status),
						SourceURL: discovery.SafeURL(p.Response.URL),
						Timestamp: time.Now(),
					})
				}
			}
		case "Runtime.consoleAPICalled":
			if entry, ok := parseConsoleAPICalled(ev.Params); ok && len(res.entries) < maxConsoleEntries {
				res.entries = append(res.entries, entry)
			}
		case "Runtime.exceptionThrown":
			if entry, ok := parseExceptionThrown(ev.Params); ok && len(res.entries) < maxConsoleEntries {
				res.entries = append(res.entries, entry)
			}
		case "Log.entryAdded":
			if entry, ok := parseLogEntryAdded(ev.Params); ok && len(res.entries) < maxConsoleEntries {
				res.entries = append(res.entries, entry)
			}
		case "Network.loadingFailed":
			if entry, ok := parseNetworkFailed(ev.Params); ok && len(res.entries) < maxConsoleEntries {
				res.entries = append(res.entries, entry)
			}
		}
	}

	drainBuffered := func() {
		for {
			select {
			case ev, ok := <-evCh:
				if !ok {
					return
				}
				handle(ev, false)
			default:
				return
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			drainBuffered()
			return res
		case <-settleC:
			drainBuffered()
			return res
		case ev, ok := <-evCh:
			if !ok {
				return res
			}
			handle(ev, true)
		}
	}
}

func scoutTriggerReason(navError string, status int, entries []ConsoleEntry, consoleErrs int, evid plan.PageEvidence) string {
	if navError != "" || status == 0 {
		return "nav-error"
	}
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
