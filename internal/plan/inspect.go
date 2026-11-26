package plan

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"golang.org/x/net/html"
)

const maxTitleBytes = 256

// CheckResult is the outcome of one check against a visited HTTP response.
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// PageEvidence captures what a lemming observed on a visited response.
type PageEvidence struct {
	ContentType string        `json:"content_type,omitempty"`
	Title       string        `json:"title,omitempty"`
	HTML        bool          `json:"html"`
	TextBytes   int           `json:"text_bytes"`
	LinkCount   int           `json:"link_count"`
	Checks      []CheckResult `json:"checks,omitempty"`
}

// FailedChecks returns the number of checks that did not pass.
func (p PageEvidence) FailedChecks() int {
	n := 0
	for _, c := range p.Checks {
		if !c.Passed {
			n++
		}
	}
	return n
}

// ResponseInput groups all HTTP response signals evaluated by InspectResponse.
type ResponseInput struct {
	Method      string
	Body        []byte
	ContentType string
	Status      int
	Headers     http.Header
	Cookies     []*http.Cookie
	Step        *PlanItem
	Base        string
}

// InspectPage checks a final response against default rules and optional step
// expectations, parsing HTML at most once. It is a convenience wrapper around
// InspectResponse for callers without response headers or cookies.
func InspectPage(body []byte, contentType string, status int, step *PlanItem, base string) (PageEvidence, []string) {
	method := http.MethodGet
	if step != nil {
		method = step.EffectiveMethod()
	}
	return InspectResponse(ResponseInput{
		Method:      method,
		Body:        body,
		ContentType: contentType,
		Status:      status,
		Step:        step,
		Base:        base,
	})
}

// InspectResponse checks an HTTP response against default rules and step
// expectations, parsing HTML at most once. It returns page evidence and
// navigable same-origin links. Sensitive header values and all cookie values
// are never echoed in CheckResult.Detail (SEC-10).
func InspectResponse(in ResponseInput) (PageEvidence, []string) {
	p := PageEvidence{ContentType: in.ContentType}
	check := func(name string, passed bool, detail string) {
		p.Checks = append(p.Checks, CheckResult{
			Name:   name,
			Passed: passed,
			Detail: detail,
		})
	}

	// 1. HTTP status check.
	var statusOK bool
	statusDetail := fmt.Sprintf("received %d", in.Status)
	if in.Step != nil {
		statusOK = in.Step.WantStatus.Matches(in.Status)
		if !in.Step.WantStatus.IsEmpty() {
			statusDetail = fmt.Sprintf("received %d (want %s)", in.Status, in.Step.WantStatus.String())
		}
	} else {
		statusOK = in.Status >= 200 && in.Status < 300
	}
	check("status", statusOK, statusDetail)

	// 2. Content-Type check.
	media, _, _ := mime.ParseMediaType(in.ContentType)
	p.HTML = media == "text/html" || media == "application/xhtml+xml"
	if in.Step != nil && in.Step.WantContentType != "" {
		check("content-type", strings.EqualFold(media, strings.TrimSpace(in.Step.WantContentType)), "received "+media)
	}

	// 3. HTML walk & conditional not-blank check.
	var links []string
	found := map[string]bool{}
	var selectors []string
	if in.Step != nil {
		selectors = in.Step.WantSelectors
	}
	if p.HTML {
		var text int
		var renders bool
		links, text, renders = walkHTML(in.Body, in.Base, &p, selectors, found)
		p.TextBytes = text
		p.LinkCount = len(links)

		method := strings.ToUpper(strings.TrimSpace(in.Method))
		if method == "" && in.Step != nil {
			method = in.Step.EffectiveMethod()
		}
		allows2xx := in.Step == nil || in.Step.WantStatus.Allows2xx()
		if method != http.MethodHead &&
			in.Status != http.StatusNoContent &&
			in.Status != http.StatusNotModified &&
			allows2xx {
			check("not-blank", text > 0 || renders, "page has no visible text, media or script")
		}
	}

	if in.Step == nil {
		return p, links
	}

	// 4. Body checks.
	for i, wb := range in.Step.WantBody {
		if wb.Contains != "" {
			check(
				fmt.Sprintf("contains[%d]", i),
				bytes.Contains(in.Body, []byte(wb.Contains)),
				secrets.ScrubText(fmt.Sprintf("must contain %q", discovery.Clip(wb.Contains, 80))),
			)
		}
		if wb.NotContains != "" {
			check(
				fmt.Sprintf("not-contains[%d]", i),
				!bytes.Contains(in.Body, []byte(wb.NotContains)),
				secrets.ScrubText(fmt.Sprintf("must not contain %q", discovery.Clip(wb.NotContains, 80))),
			)
		}
	}

	// 5. Title check.
	if in.Step.WantTitle != "" {
		check(
			"title",
			strings.Contains(p.Title, in.Step.WantTitle),
			secrets.ScrubText(fmt.Sprintf("title %q must contain %q", p.Title, in.Step.WantTitle)),
		)
	}

	// 6. Selector checks.
	for _, sel := range in.Step.WantSelectors {
		check("selector:"+sel, found[sel], "element must exist in the server HTML")
	}

	// 7. Header checks.
	for _, h := range in.Step.WantHeaders {
		passed, detail := evaluateHeaderExpectation(in.Headers, h)
		check("header:"+h.Key, passed, detail)
	}

	// 8. Cookie checks.
	for _, c := range in.Step.WantCookies {
		passed, detail := evaluateCookieExpectation(in.Cookies, c)
		check("cookie:"+c.Name, passed, detail)
	}

	return p, links
}

func evaluateHeaderExpectation(headers http.Header, h ExpectHeader) (bool, string) {
	vals := headerValuesFold(headers, h.Key)
	hasPositive := h.Is != "" || h.Contains != "" || h.BeginsWith != "" || h.EndsWith != "" || len(h.IsOne) > 0
	hasNotContains := h.NotContains != ""
	sensitive := isSensitiveHeader(h.Key)

	var posOK bool
	switch {
	case !hasPositive && !hasNotContains:
		posOK = len(vals) > 0
	case hasPositive:
		for _, v := range vals {
			if matchesPositive(v, h.Is, h.Contains, h.BeginsWith, h.EndsWith, h.IsOne) {
				posOK = true
				break
			}
		}
	default:
		posOK = true
	}

	negOK := true
	if hasNotContains {
		for _, v := range vals {
			if strings.Contains(v, h.NotContains) {
				negOK = false
				break
			}
		}
	}

	passed := posOK && negOK
	if len(vals) == 0 {
		if passed {
			return true, "header absent"
		}
		return false, "header missing"
	}

	if sensitive {
		if passed {
			if !hasPositive && !hasNotContains {
				return true, "header present (value redacted)"
			}
			return true, "header matched (value redacted)"
		}
		return false, "header value did not match expectation (value redacted)"
	}

	safeVal := discovery.Clip(secrets.ScrubText(strings.Join(vals, ", ")), 160)
	if passed {
		if !hasPositive && !hasNotContains {
			return true, secrets.ScrubText("present: " + safeVal)
		}
		return true, secrets.ScrubText("matched: " + safeVal)
	}
	return false, secrets.ScrubText(fmt.Sprintf("received %q", safeVal))
}

func evaluateCookieExpectation(cookies []*http.Cookie, c ExpectCookie) (bool, string) {
	var vals []string
	for _, ck := range cookies {
		if ck != nil && ck.Name == c.Name {
			vals = append(vals, ck.Value)
		}
	}

	if c.Absent {
		if len(vals) == 0 {
			return true, "cookie absent"
		}
		return false, "cookie unexpectedly present"
	}

	if len(vals) == 0 {
		return false, "cookie missing"
	}

	exact := c.exactValue()
	hasPositive := exact != "" || c.Contains != "" || c.BeginsWith != "" || c.EndsWith != "" || len(c.IsOne) > 0
	hasNotContains := c.NotContains != ""

	if !hasPositive && !hasNotContains {
		return true, "cookie present"
	}

	posOK := !hasPositive
	if hasPositive {
		for _, v := range vals {
			if matchesPositive(v, exact, c.Contains, c.BeginsWith, c.EndsWith, c.IsOne) {
				posOK = true
				break
			}
		}
	}

	negOK := true
	if hasNotContains {
		for _, v := range vals {
			if strings.Contains(v, c.NotContains) {
				negOK = false
				break
			}
		}
	}

	if posOK && negOK {
		return true, "cookie matched"
	}
	return false, "cookie value did not match expectation"
}

func matchesPositive(v, exact, contains, beginsWith, endsWith string, isOne StringList) bool {
	if exact != "" && v != exact {
		return false
	}
	if contains != "" && !strings.Contains(v, contains) {
		return false
	}
	if beginsWith != "" && !strings.HasPrefix(v, beginsWith) {
		return false
	}
	if endsWith != "" && !strings.HasSuffix(v, endsWith) {
		return false
	}
	if len(isOne) > 0 && !slices.Contains(isOne, v) {
		return false
	}
	return true
}

func headerValuesFold(headers http.Header, key string) []string {
	if len(headers) == 0 {
		return nil
	}
	if vals := headers.Values(key); len(vals) > 0 {
		return vals
	}
	var out []string
	for k, vs := range headers {
		if strings.EqualFold(k, key) {
			out = append(out, vs...)
		}
	}
	return out
}

func isSensitiveHeader(name string) bool {
	low := strings.ToLower(strings.TrimSpace(name))
	switch low {
	case "authorization",
		"proxy-authorization",
		"cookie",
		"set-cookie",
		"x-api-key",
		"x-auth-token",
		"x-csrf-token",
		"x-amz-security-token",
		"phpsessid":
		return true
	}
	for _, sub := range []string{"token", "secret", "session", "password", "passwd", "api-key", "apikey"} {
		if strings.Contains(low, sub) {
			return true
		}
	}
	return false
}

func walkHTML(body []byte, base string, p *PageEvidence, selectors []string, found map[string]bool) ([]string, int, bool) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, 0, false
	}
	var links []string
	seen := map[string]bool{}
	text := 0
	renders := false

	var walk func(n *html.Node, hidden bool)
	walk = func(n *html.Node, hidden bool) {
		switch n.Type {
		case html.ElementNode:
			switch n.Data {
			case "script":
				renders = true
				hidden = true
			case "img", "svg", "canvas", "video", "picture", "iframe", "object", "embed":
				renders = true
			case "style", "head", "template", "noscript":
				hidden = true
			case "title":
				if p.Title == "" && n.FirstChild != nil {
					p.Title = discovery.Clip(strings.TrimSpace(n.FirstChild.Data), maxTitleBytes)
				}
			case "a":
				if link := NavigableLink(n, base); link != "" && !seen[link] {
					seen[link] = true
					links = append(links, link)
				}
			}
			for _, sel := range selectors {
				if !found[sel] && matchesSelector(n, sel) {
					found[sel] = true
				}
			}
		case html.TextNode:
			if !hidden {
				text += len(strings.TrimSpace(n.Data))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, hidden)
		}
	}
	walk(doc, false)
	return links, text, renders
}

func matchesSelector(n *html.Node, sel string) bool {
	if sel == "" {
		return false
	}
	switch sel[0] {
	case '#':
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == sel[1:] {
				return true
			}
		}
	case '.':
		for _, a := range n.Attr {
			if a.Key == "class" {
				for _, c := range strings.Fields(a.Val) {
					if c == sel[1:] {
						return true
					}
				}
			}
		}
	default:
		return strings.EqualFold(n.Data, sel)
	}
	return false
}

var skipLinkExtensions = map[string]bool{
	".pdf": true, ".zip": true, ".gz": true, ".tar": true, ".tgz": true, ".dmg": true,
	".exe": true, ".msi": true, ".pkg": true, ".iso": true, ".apk": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true,
	".ico": true, ".mp3": true, ".mp4": true, ".mov": true, ".webm": true, ".wav": true,
	".css": true, ".js": true, ".woff": true, ".woff2": true, ".ttf": true,
	".csv": true, ".xlsx": true, ".docx": true,
}

var destructiveLink = regexp.MustCompile(`(?i)(^|[/_.-])(log-?out|sign-?out|log-?off|delete|destroy|remove|unsubscribe|deactivate)($|[/_.?-])`)

// NavigableLink returns the resolved same-origin URL of an anchor that a lemming
// may follow, or "" for downloads, destructive paths, and off-origin links.
func NavigableLink(n *html.Node, base string) string {
	var href string
	for _, a := range n.Attr {
		switch a.Key {
		case "href":
			href = a.Val
		case "download":
			return ""
		}
	}
	resolved := discovery.ResolveURL(base, href)
	if resolved == "" {
		return ""
	}
	u, err := url.Parse(resolved)
	if err != nil {
		return ""
	}
	if skipLinkExtensions[strings.ToLower(path.Ext(u.Path))] || destructiveLink.MatchString(u.Path) {
		return ""
	}
	return resolved
}
