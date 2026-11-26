package forensics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/plan"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"golang.org/x/net/publicsuffix"
)

const (
	defaultCaptureTimeout = 8 * time.Second
	maxConsoleEntries     = 100
	maxScreenshotBytes    = 2 << 20 // 2 MiB hard cap per screenshot
	cdpSettleWindow       = 350 * time.Millisecond
)

// Capturer produces an Artifact for a CaptureRequest.
type Capturer interface {
	Capture(ctx context.Context, req CaptureRequest) (Artifact, error)
	Mode() string
	BrowserInfo() string
	Close() error
}

// HTTPFallbackCapturer records session state, cookie jar status, and extracted
// error body without launching a browser. Used when -capture=false or when no
// local Chrome/Chromium or remote CDP endpoint is available.
type HTTPFallbackCapturer struct {
	reason string
}

// NewHTTPFallbackCapturer constructs a pure-HTTP capturer.
func NewHTTPFallbackCapturer(reason string) *HTTPFallbackCapturer {
	return &HTTPFallbackCapturer{reason: reason}
}

func (c *HTTPFallbackCapturer) Mode() string        { return ModeHTTPOnly }
func (c *HTTPFallbackCapturer) BrowserInfo() string { return c.reason }
func (c *HTTPFallbackCapturer) Close() error        { return nil }

func (c *HTTPFallbackCapturer) Capture(_ context.Context, req CaptureRequest) (Artifact, error) {
	ex := plan.ExtractErrorBody(req.StatusCode, req.ContentType, req.Headers, req.RawBody)
	return Artifact{
		ID:             newArtifactID(),
		Fingerprint:    Fingerprint(req.Path, req.StatusCode, req.TriggerReason),
		CapturedAt:     time.Now(),
		URL:            req.SafeURL,
		Path:           req.Path,
		TriggerReason:  req.TriggerReason,
		CaptureMode:    ModeHTTPOnly,
		CaptureWarning: c.reason,
		StatusCode:     req.StatusCode,
		LatencyMS:      req.LatencyMS,
		Session:        req.Session,
		ErrorBody:      ex,
		ReviewStatus:   ReviewUnreviewed,
	}, nil
}

// CDPOptions configures the pure-Go Headless Chromium capturer.
type CDPOptions struct {
	ChromeBin      string
	CDPURL         string
	AllowRemoteCDP bool
	Format         string // "jpeg" (default) or "png"
	Timeout        time.Duration
}

// CDPCapturer captures real screenshots, JS console output, and rendered DOM
// errors using Chrome DevTools Protocol over isolated incognito BrowserContexts.
type CDPCapturer struct {
	browser *browserInstance
	format  string
	timeout time.Duration
}

// NewCDPCapturer connects to a remote CDP endpoint or launches a local hardened
// headless Chrome/Chromium instance.
func NewCDPCapturer(ctx context.Context, opts CDPOptions) (*CDPCapturer, error) {
	format := strings.ToLower(strings.TrimSpace(opts.Format))
	if format != "png" {
		format = "jpeg"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultCaptureTimeout
	}

	var b *browserInstance
	var err error
	if strings.TrimSpace(opts.CDPURL) != "" {
		b, err = connectRemoteBrowser(ctx, opts.CDPURL, opts.AllowRemoteCDP)
	} else {
		bin, findErr := FindBrowser(opts.ChromeBin)
		if findErr != nil {
			return nil, findErr
		}
		b, err = launchLocalBrowser(ctx, bin)
	}
	if err != nil {
		return nil, err
	}
	return &CDPCapturer{
		browser: b,
		format:  format,
		timeout: timeout,
	}, nil
}

func (c *CDPCapturer) Mode() string { return ModeCDPReplay }
func (c *CDPCapturer) BrowserInfo() string {
	if c.browser == nil {
		return ""
	}
	return c.browser.desc
}
func (c *CDPCapturer) Close() error {
	if c.browser == nil {
		return nil
	}
	return c.browser.Close()
}

// Capture replays req.URL inside a freshly created ephemeral incognito
// BrowserContext carrying the lemming's cookies, UA, Accept-Language, and Referer,
// collects JS console logs + exceptions + failed subresources, takes a screenshot,
// and tears down the BrowserContext immediately.
// When req.SkipBrowserReplay is true (e.g., failed POST/PUT/DELETE or step with
// request body), CDP replay is skipped and an HTTP-only artifact is returned.
func (c *CDPCapturer) Capture(ctx context.Context, req CaptureRequest) (Artifact, error) {
	ex := plan.ExtractErrorBody(req.StatusCode, req.ContentType, req.Headers, req.RawBody)
	if req.SkipBrowserReplay {
		warn := req.SkipBrowserReason
		if warn == "" {
			warn = "CDP replay skipped for non-GET request; captured HTTP-only forensics"
		}
		return Artifact{
			ID:             newArtifactID(),
			Fingerprint:    Fingerprint(req.Path, req.StatusCode, req.TriggerReason),
			CapturedAt:     time.Now(),
			URL:            req.SafeURL,
			Path:           req.Path,
			TriggerReason:  req.TriggerReason,
			CaptureMode:    ModeHTTPOnly,
			CaptureWarning: warn,
			StatusCode:     req.StatusCode,
			LatencyMS:      req.LatencyMS,
			Session:        req.Session,
			ErrorBody:      ex,
			ReviewStatus:   ReviewUnreviewed,
		}, nil
	}

	capCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	art := Artifact{
		ID:            newArtifactID(),
		Fingerprint:   Fingerprint(req.Path, req.StatusCode, req.TriggerReason),
		CapturedAt:    time.Now(),
		URL:           req.SafeURL,
		Path:          req.Path,
		TriggerReason: req.TriggerReason,
		CaptureMode:   ModeCDPReplay,
		StatusCode:    req.StatusCode,
		LatencyMS:     req.LatencyMS,
		Session:       req.Session,
		ErrorBody:     ex,
		ReviewStatus:  ReviewUnreviewed,
	}

	// Create an isolated incognito context so cookies never bleed across lemmings.
	var bcRes struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := c.browser.conn.Call(capCtx, "", "Target.createBrowserContext", map[string]any{
		"disposeOnDetach": true,
	}, &bcRes); err != nil {
		art.CaptureWarning = "browser context unavailable: " + err.Error()
		return art, err
	}
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanCancel()
		_ = c.browser.conn.Call(cleanCtx, "", "Target.disposeBrowserContext", map[string]any{
			"browserContextId": bcRes.BrowserContextID,
		}, nil)
	}()

	width, height := 1280, 800
	if req.Mobile {
		width, height = 390, 844
	}

	var targetRes struct {
		TargetID string `json:"targetId"`
	}
	if err := c.browser.conn.Call(capCtx, "", "Target.createTarget", map[string]any{
		"url":              "about:blank",
		"width":            width,
		"height":           height,
		"browserContextId": bcRes.BrowserContextID,
	}, &targetRes); err != nil {
		art.CaptureWarning = "target create failed: " + err.Error()
		return art, err
	}
	defer func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanCancel()
		_ = c.browser.conn.Call(cleanCtx, "", "Target.closeTarget", map[string]any{
			"targetId": targetRes.TargetID,
		}, nil)
	}()

	var attachRes struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.browser.conn.Call(capCtx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetRes.TargetID,
		"flatten":  true,
	}, &attachRes); err != nil {
		art.CaptureWarning = "target attach failed: " + err.Error()
		return art, err
	}
	sid := attachRes.SessionID

	evCh, unsub := c.browser.conn.SubscribeSession(sid)
	defer unsub()

	// Enable domains and configure persona/viewport/headers/cookies.
	for _, m := range []string{"Page.enable", "Runtime.enable", "Log.enable", "Network.enable"} {
		_ = c.browser.conn.Call(capCtx, sid, m, nil, nil)
	}
	_ = c.browser.conn.Call(capCtx, sid, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": 1,
		"mobile":            req.Mobile,
	}, nil)
	if req.Session.UserAgent != "" {
		_ = c.browser.conn.Call(capCtx, sid, "Network.setUserAgentOverride", map[string]any{
			"userAgent":      req.Session.UserAgent,
			"acceptLanguage": req.Session.Language,
		}, nil)
	}
	if req.Session.Referrer != "" {
		_ = c.browser.conn.Call(capCtx, sid, "Network.setExtraHTTPHeaders", map[string]any{
			"headers": map[string]string{"Referer": req.Session.Referrer},
		}, nil)
	}
	if len(req.ReplayCookies) > 0 {
		injectReplayCookies(capCtx, c.browser.conn, sid, req.URL, req.ReplayCookies)
	}

	// Navigate to the failing URL.
	var navRes struct {
		ErrorText string `json:"errorText"`
	}
	if err := c.browser.conn.Call(capCtx, sid, "Page.navigate", map[string]any{
		"url": req.URL,
	}, &navRes); err != nil {
		art.CaptureWarning = "page navigate failed: " + err.Error()
		return art, err
	}
	if navRes.ErrorText != "" {
		art.CaptureWarning = "navigation error: " + navRes.ErrorText
	}

	// Collect events until loadEventFired + short settle window (or event budget),
	// reserving a grace window for final-origin check, DOM extraction, and screenshot
	// capture so pages that never emit Page.loadEventFired still produce a screenshot.
	eventCtx, eventCancel := eventCollectionContext(capCtx, c.timeout)
	consoleEntries := collectSessionEvents(eventCtx, evCh, cdpSettleWindow)
	eventCancel()
	art.Console = consoleEntries
	art.ConsoleErrors = countConsoleErrors(consoleEntries)

	// Verify final origin did not escape the target origin before capturing DOM/screenshot.
	if finalURL := evalString(capCtx, c.browser.conn, sid, "location.href"); finalURL != "" && req.Origin != "" {
		if !discovery.SameOrigin(req.Origin, finalURL) && !strings.HasPrefix(finalURL, "chrome-error://") {
			art.CaptureWarning = "replay redirected off target origin; screenshot skipped"
			return art, nil
		}
	}

	// Capture rendered DOM body if original HTTP body was empty (e.g. client-rendered error).
	if art.ErrorBody.Format == "empty" || art.ErrorBody.Format == "" {
		if domHTML := evalString(capCtx, c.browser.conn, sid, "document.documentElement ? document.documentElement.outerHTML.slice(0, 131072) : ''"); domHTML != "" {
			art.ErrorBody = plan.ExtractErrorBody(req.StatusCode, "text/html", req.Headers, []byte(domHTML))
		}
	}

	// Capture visual screenshot.
	shot, err := captureScreenshot(capCtx, c.browser.conn, sid, c.format, width, height)
	if err != nil {
		art.CaptureWarning = "screenshot failed: " + err.Error()
	} else {
		art.Screenshot = shot
	}
	return art, nil
}

func eventCollectionContext(capCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	dl, ok := capCtx.Deadline()
	if !ok {
		return capCtx, func() {}
	}
	grace := min(2*time.Second, timeout/4)
	if grace <= 0 {
		return capCtx, func() {}
	}
	if rem := time.Until(dl); rem > grace {
		return context.WithDeadline(capCtx, dl.Add(-grace))
	}
	// Already inside the grace window: stop waiting for loadEventFired immediately
	// so the remaining budget goes to origin check, DOM extraction, and screenshot.
	ctx, cancel := context.WithCancel(capCtx)
	cancel()
	return ctx, cancel
}

func injectReplayCookies(ctx context.Context, conn *cdpConn, sid, targetURL string, cookies []ReplayCookie) {
	params := buildReplayCookieParams(targetURL, cookies)
	if len(params) > 0 {
		_ = conn.Call(ctx, sid, "Network.setCookies", map[string]any{"cookies": params}, nil)
	}
}

func buildReplayCookieParams(targetURL string, cookies []ReplayCookie) []map[string]any {
	u, err := url.Parse(targetURL)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	targetHost := u.Hostname()
	params := make([]map[string]any, 0, len(cookies))
	for _, ck := range cookies {
		if ck.Name == "" {
			continue
		}
		if !cookieDomainAllowed(ck.Domain, targetHost) {
			continue
		}
		m := map[string]any{
			"name":     ck.Name,
			"value":    ck.Value,
			"url":      targetURL,
			"path":     ck.Path,
			"secure":   ck.Secure,
			"httpOnly": ck.HTTPOnly,
		}
		if dom := strings.TrimSpace(ck.Domain); dom != "" {
			m["domain"] = dom
		}
		if ck.Path == "" {
			m["path"] = "/"
		}
		if ck.Expires > 0 {
			m["expires"] = ck.Expires
		}
		params = append(params, m)
	}
	return params
}

// cookieDomainAllowed validates that cookieDomain may be replayed for targetHost.
// Empty cookieDomain represents a host-only cookie on targetHost. Non-empty
// cookieDomain (with at most one leading ".") must equal targetHost or be a
// proper dot-boundary parent domain of targetHost, rejecting IP suffix tricks,
// public suffixes/TLDs, and unrelated domains.
func cookieDomainAllowed(cookieDomain, targetHost string) bool {
	target := strings.ToLower(strings.TrimSpace(targetHost))
	if target == "" {
		return false
	}
	raw := strings.ToLower(strings.TrimSpace(cookieDomain))
	if raw == "" {
		return true
	}
	dom := strings.TrimPrefix(raw, ".")
	if dom == "" || strings.HasPrefix(dom, ".") || strings.HasSuffix(dom, ".") {
		return false
	}
	if strings.ContainsAny(dom, " \t\r\n/@") {
		return false
	}
	// IP literals (IPv4 or IPv6) only allow exact IP equality; never parent-domain suffix matching.
	if domIP, err := netip.ParseAddr(dom); err == nil {
		targetIP, terr := netip.ParseAddr(target)
		return terr == nil && domIP == targetIP
	}
	if strings.Contains(dom, ":") {
		return false
	}
	if _, err := netip.ParseAddr(target); err == nil {
		return false
	}
	if dom == target {
		return true
	}
	// Reject single-label parent domains (e.g. "com") and ICANN public suffixes.
	if !strings.Contains(dom, ".") {
		return false
	}
	if ps, icann := publicsuffix.PublicSuffix(dom); icann && ps == dom {
		return false
	}
	return strings.HasSuffix(target, "."+dom)
}

func collectSessionEvents(ctx context.Context, evCh <-chan cdpEvent, settle time.Duration) []ConsoleEntry {
	var out []ConsoleEntry
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
		case "Runtime.consoleAPICalled":
			if entry, ok := parseConsoleAPICalled(ev.Params); ok && len(out) < maxConsoleEntries {
				out = append(out, entry)
			}
		case "Runtime.exceptionThrown":
			if entry, ok := parseExceptionThrown(ev.Params); ok && len(out) < maxConsoleEntries {
				out = append(out, entry)
			}
		case "Log.entryAdded":
			if entry, ok := parseLogEntryAdded(ev.Params); ok && len(out) < maxConsoleEntries {
				out = append(out, entry)
			}
		case "Network.loadingFailed":
			if entry, ok := parseNetworkFailed(ev.Params); ok && len(out) < maxConsoleEntries {
				out = append(out, entry)
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
			return out
		case <-settleC:
			drainBuffered()
			return out
		case ev, ok := <-evCh:
			if !ok {
				return out
			}
			handle(ev, true)
		}
	}
}

func parseConsoleAPICalled(raw json.RawMessage) (ConsoleEntry, bool) {
	var p struct {
		Type string `json:"type"`
		Args []struct {
			Type        string `json:"type"`
			Value       any    `json:"value"`
			Description string `json:"description"`
		} `json:"args"`
		StackTrace *cdpStackTrace `json:"stackTrace"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ConsoleEntry{}, false
	}
	parts := make([]string, 0, len(p.Args))
	for _, a := range p.Args {
		switch {
		case a.Description != "":
			parts = append(parts, a.Description)
		case a.Value != nil:
			parts = append(parts, fmt.Sprint(a.Value))
		case a.Type != "":
			parts = append(parts, a.Type)
		}
	}
	text := discovery.Clip(secrets.ScrubText(strings.TrimSpace(strings.Join(parts, " "))), 1024)
	if text == "" {
		return ConsoleEntry{}, false
	}
	entry := ConsoleEntry{
		Level:     normalizeConsoleLevel(p.Type),
		Text:      text,
		Timestamp: time.Now(),
	}
	applyStackTrace(&entry, p.StackTrace)
	return entry, true
}

func parseExceptionThrown(raw json.RawMessage) (ConsoleEntry, bool) {
	var p struct {
		ExceptionDetails struct {
			Text       string `json:"text"`
			URL        string `json:"url"`
			LineNumber int    `json:"lineNumber"`
			ColNumber  int    `json:"columnNumber"`
			Exception  *struct {
				Description string `json:"description"`
			} `json:"exception"`
			StackTrace *cdpStackTrace `json:"stackTrace"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ConsoleEntry{}, false
	}
	d := p.ExceptionDetails
	msg := d.Text
	if d.Exception != nil && d.Exception.Description != "" {
		msg = d.Exception.Description
	}
	msg = discovery.Clip(secrets.ScrubText(strings.TrimSpace(msg)), 1024)
	if msg == "" {
		msg = "Uncaught JavaScript exception"
	}
	entry := ConsoleEntry{
		Level:     "exception",
		Text:      msg,
		SourceURL: discovery.SafeURL(d.URL),
		Line:      d.LineNumber + 1,
		Column:    d.ColNumber + 1,
		Timestamp: time.Now(),
	}
	applyStackTrace(&entry, d.StackTrace)
	return entry, true
}

func parseLogEntryAdded(raw json.RawMessage) (ConsoleEntry, bool) {
	var p struct {
		Entry struct {
			Level      string         `json:"level"`
			Text       string         `json:"text"`
			URL        string         `json:"url"`
			LineNumber int            `json:"lineNumber"`
			StackTrace *cdpStackTrace `json:"stackTrace"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ConsoleEntry{}, false
	}
	text := discovery.Clip(secrets.ScrubText(strings.TrimSpace(p.Entry.Text)), 1024)
	if text == "" {
		return ConsoleEntry{}, false
	}
	entry := ConsoleEntry{
		Level:     normalizeConsoleLevel(p.Entry.Level),
		Text:      text,
		SourceURL: discovery.SafeURL(p.Entry.URL),
		Line:      p.Entry.LineNumber,
		Timestamp: time.Now(),
	}
	applyStackTrace(&entry, p.Entry.StackTrace)
	return entry, true
}

func parseNetworkFailed(raw json.RawMessage) (ConsoleEntry, bool) {
	var p struct {
		ErrorText string `json:"errorText"`
		Type      string `json:"type"`
		Canceled  bool   `json:"canceled"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Canceled || p.ErrorText == "" {
		return ConsoleEntry{}, false
	}
	msg := p.ErrorText
	if p.Type != "" {
		msg = p.Type + " load failed: " + msg
	}
	return ConsoleEntry{
		Level:     "network",
		Text:      discovery.Clip(secrets.ScrubText(msg), 512),
		Timestamp: time.Now(),
	}, true
}

type cdpStackTrace struct {
	CallFrames []struct {
		FunctionName string `json:"functionName"`
		URL          string `json:"url"`
		LineNumber   int    `json:"lineNumber"`
		ColumnNumber int    `json:"columnNumber"`
	} `json:"callFrames"`
}

func applyStackTrace(entry *ConsoleEntry, st *cdpStackTrace) {
	if st == nil || len(st.CallFrames) == 0 {
		return
	}
	if entry.SourceURL == "" && st.CallFrames[0].URL != "" {
		entry.SourceURL = discovery.SafeURL(st.CallFrames[0].URL)
		entry.Line = st.CallFrames[0].LineNumber + 1
		entry.Column = st.CallFrames[0].ColumnNumber + 1
	}
	var b strings.Builder
	for i, f := range st.CallFrames {
		if i >= 6 {
			break
		}
		fn := f.FunctionName
		if fn == "" {
			fn = "<anonymous>"
		}
		fmt.Fprintf(&b, "at %s (%s:%d:%d)\n", fn, discovery.SafeURL(f.URL), f.LineNumber+1, f.ColumnNumber+1)
	}
	entry.StackTrace = discovery.Clip(secrets.ScrubText(strings.TrimSpace(b.String())), 1024)
}

func normalizeConsoleLevel(lvl string) string {
	switch strings.ToLower(lvl) {
	case "error", "assert":
		return "error"
	case "warning", "warn":
		return "warning"
	case "info":
		return "info"
	case "debug":
		return "debug"
	default:
		return "log"
	}
}

func countConsoleErrors(entries []ConsoleEntry) int {
	n := 0
	for _, e := range entries {
		if e.Level == "error" || e.Level == "exception" || e.Level == "network" {
			n++
		}
	}
	return n
}

func evalString(ctx context.Context, conn *cdpConn, sid, expr string) string {
	var res struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := conn.Call(ctx, sid, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
	}, &res); err != nil {
		return ""
	}
	return res.Result.Value
}

func captureScreenshot(ctx context.Context, conn *cdpConn, sid, format string, width, height int) (*Screenshot, error) {
	params := map[string]any{
		"format": format,
	}
	mimeType := "image/jpeg"
	if format == "png" {
		mimeType = "image/png"
	} else {
		params["quality"] = 82
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := conn.Call(ctx, sid, "Page.captureScreenshot", params, &res); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(res.Data)
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}
	if len(raw) > maxScreenshotBytes {
		return nil, fmt.Errorf("screenshot exceeds %d byte cap", maxScreenshotBytes)
	}
	return &Screenshot{
		MIMEType: mimeType,
		Width:    width,
		Height:   height,
		Bytes:    len(raw),
		SHA256:   sha256Hex(raw),
		Data:     raw,
	}, nil
}
