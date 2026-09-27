package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Navigation modes for -navigation.
const (
	// NavigationLinks lands on -hit and then follows a random same-origin
	// link from the page just read, like a person clicking around. At a
	// dead end it jumps to a random URL from the pool.
	NavigationLinks = "links"

	// NavigationRandom picks every URL at random from the indexed pool.
	NavigationRandom = "random"
)

const (
	// maxJourneyBytes bounds the size of a -journey file.
	maxJourneyBytes = 1 << 20

	// maxJourneySteps bounds the number of steps in a journey.
	maxJourneySteps = 100

	// maxTitleBytes bounds a recorded page title.
	maxTitleBytes = 256
)

// Journey is a finite, ordered sequence of GET page visits with
// expectations for each. Every lemming walks the same journey once.
//
// Usage (JSON):
//
//	{
//	  "name": "browse-catalog",
//	  "steps": [
//	    {"name": "home", "path": "/", "expect": {"status": [200], "contains": ["Welcome"]}},
//	    {"name": "product", "path": "/product", "expect": {"selectors": ["#buy"]}}
//	  ]
//	}
//
// Warning: journeys are GET-only. Lemmings never submit forms, so a
// journey cannot log in or check out; cookies set by GET responses do
// persist for the lemming's whole life.
type Journey struct {
	Name  string        `json:"name"`
	Steps []JourneyStep `json:"steps"`
}

// JourneyStep is one page in a Journey.
type JourneyStep struct {
	Name   string       `json:"name"`
	Path   string       `json:"path"`
	Expect Expectations `json:"expect"`
}

// Expectations are the checks applied to a journey step's final response.
// Every field is optional. Without Status, any 2xx passes.
type Expectations struct {
	Status        []int    `json:"status,omitempty"`
	ContentType   string   `json:"content_type,omitempty"`
	Contains      []string `json:"contains,omitempty"`
	NotContains   []string `json:"not_contains,omitempty"`
	TitleContains string   `json:"title_contains,omitempty"`
	Selectors     []string `json:"selectors,omitempty"`
}

// simpleSelector accepts a tag name, #id or .class — the subset that can
// be checked against server HTML without a CSS engine.
var simpleSelector = regexp.MustCompile(`^(?:[a-zA-Z][a-zA-Z0-9-]*|[#.][a-zA-Z_][a-zA-Z0-9_-]*)$`)

// LoadJourney reads and validates a journey file. Every step's path must
// resolve to the same origin as hit.
func LoadJourney(file, hit string) (*Journey, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseJourney(io.LimitReader(f, maxJourneyBytes), hit)
}

// parseJourney decodes and validates a journey. Unknown fields are
// rejected so a typo in an expectation cannot silently disable it.
func parseJourney(r io.Reader, hit string) (*Journey, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var j Journey
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("journey: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("journey: file must contain exactly one JSON object")
	}
	if len(j.Steps) == 0 || len(j.Steps) > maxJourneySteps {
		return nil, fmt.Errorf("journey: needs 1..%d steps, got %d", maxJourneySteps, len(j.Steps))
	}
	for i := range j.Steps {
		step := &j.Steps[i]
		if step.Name == "" {
			step.Name = fmt.Sprintf("step-%d", i+1)
		}
		if resolveURL(hit, step.Path) == "" {
			return nil, fmt.Errorf("journey step %q: path %q must stay on %s", step.Name, step.Path, hit)
		}
		for _, code := range step.Expect.Status {
			if code < 100 || code > 599 {
				return nil, fmt.Errorf("journey step %q: invalid expected status %d", step.Name, code)
			}
		}
		for _, sel := range step.Expect.Selectors {
			if !simpleSelector.MatchString(sel) {
				return nil, fmt.Errorf("journey step %q: selector %q must be a tag, #id or .class", step.Name, sel)
			}
		}
	}
	return &j, nil
}

// CheckResult is the outcome of one check against a visited page.
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// PageEvidence is what a lemming learned from a page's response.
//
// Checks run against the server's response. Lemmings do not execute
// JavaScript, so a page that renders its content client-side will show
// little text here — assert on what the server actually sends.
type PageEvidence struct {
	ContentType string        `json:"content_type,omitempty"`
	Title       string        `json:"title,omitempty"`
	HTML        bool          `json:"html"`
	TextBytes   int           `json:"text_bytes"`
	LinkCount   int           `json:"link_count"`
	Checks      []CheckResult `json:"checks,omitempty"`
}

// failedChecks counts checks that did not pass.
func (p PageEvidence) failedChecks() int {
	n := 0
	for _, c := range p.Checks {
		if !c.Passed {
			n++
		}
	}
	return n
}

// inspectPage checks a final response against the default rules and the
// step's expectations, parsing HTML at most once. It returns the evidence
// and the navigable same-origin links on the page, resolved against base.
//
// Default rules: the status must be 2xx (or one of expect.Status), and an
// HTML response must not be blank.
func inspectPage(body []byte, contentType string, status int, expect Expectations, base string) (PageEvidence, []string) {
	p := PageEvidence{ContentType: contentType}
	check := func(name string, passed bool, detail string) {
		p.Checks = append(p.Checks, CheckResult{Name: name, Passed: passed, Detail: detail})
	}

	statusOK := status >= 200 && status < 300
	if len(expect.Status) > 0 {
		statusOK = false
		for _, s := range expect.Status {
			statusOK = statusOK || s == status
		}
	}
	check("status", statusOK, fmt.Sprintf("received %d", status))

	media, _, _ := mime.ParseMediaType(contentType)
	p.HTML = media == "text/html" || media == "application/xhtml+xml"
	if expect.ContentType != "" {
		check("content-type", strings.EqualFold(media, expect.ContentType), "received "+media)
	}

	var links []string
	found := map[string]bool{}
	if p.HTML {
		var text int
		var renders bool
		links, text, renders = walkHTML(body, base, &p, expect.Selectors, found)
		p.TextBytes = text
		p.LinkCount = len(links)
		// A page with no text, no media and no script has nothing a person
		// could see. Pages with scripts may render client-side, which a
		// lemming cannot judge, so they get the benefit of the doubt.
		check("not-blank", text > 0 || renders, "page has no visible text, media or script")
	}

	for i, s := range expect.Contains {
		check(fmt.Sprintf("contains[%d]", i), bytes.Contains(body, []byte(s)), fmt.Sprintf("must contain %q", clip(s, 80)))
	}
	for i, s := range expect.NotContains {
		check(fmt.Sprintf("not-contains[%d]", i), !bytes.Contains(body, []byte(s)), fmt.Sprintf("must not contain %q", clip(s, 80)))
	}
	if expect.TitleContains != "" {
		check("title", strings.Contains(p.Title, expect.TitleContains), fmt.Sprintf("title %q must contain %q", p.Title, expect.TitleContains))
	}
	for _, sel := range expect.Selectors {
		check("selector:"+sel, found[sel], "element must exist in the server HTML")
	}
	return p, links
}

// walkHTML parses body once, filling in the title, collecting navigable
// links and marking which selectors matched. It returns the links, the
// byte count of visible text (outside script, style, head and template),
// and whether the page carries script or media that could render content.
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
					p.Title = clip(strings.TrimSpace(n.FirstChild.Data), maxTitleBytes)
				}
			case "a":
				if link := navigableLink(n, base); link != "" && !seen[link] {
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

// matchesSelector reports whether an element matches a tag, #id or .class.
func matchesSelector(n *html.Node, sel string) bool {
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

// skipLinkExtensions are file types a person clicks to download, not to
// read. Lemmings measure pages, so they do not follow these.
var skipLinkExtensions = map[string]bool{
	".pdf": true, ".zip": true, ".gz": true, ".tar": true, ".tgz": true, ".dmg": true,
	".exe": true, ".msi": true, ".pkg": true, ".iso": true, ".apk": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true,
	".ico": true, ".mp3": true, ".mp4": true, ".mov": true, ".webm": true, ".wav": true,
	".css": true, ".js": true, ".woff": true, ".woff2": true, ".ttf": true,
	".csv": true, ".xlsx": true, ".docx": true,
}

// destructiveLink matches path segments that end or mutate a session.
// Following them would make a lemming log itself out or worse.
var destructiveLink = regexp.MustCompile(`(?i)(^|[/_.-])(log-?out|sign-?out|log-?off|delete|destroy|remove|unsubscribe|deactivate)($|[/_.?-])`)

// navigableLink returns the resolved same-origin URL of an anchor that a
// lemming may follow, or "" for downloads, destructive links and anchors
// that leave the origin.
func navigableLink(n *html.Node, base string) string {
	var href string
	for _, a := range n.Attr {
		switch a.Key {
		case "href":
			href = a.Val
		case "download":
			return ""
		}
	}
	resolved := resolveURL(base, href)
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

// safeURL removes userinfo, query values and fragments before a URL is
// written to any report, trace or dashboard. Query strings routinely carry
// tokens and personal data.
func safeURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""
	u.RawQuery = ""
	u.ForceQuery = false
	return clip(u.String(), 2048)
}

// clip truncates s to at most n bytes without splitting a UTF-8 rune.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
