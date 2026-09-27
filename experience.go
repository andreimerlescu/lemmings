package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ExperienceConfig controls user behavior and evidence collection. HTTP checks
// inspect server responses; only the separate browser cohort executes JavaScript.
type ExperienceConfig struct {
	ThinkMin       time.Duration
	ThinkMax       time.Duration
	RequestTimeout time.Duration
	MaxBodyBytes   int64
	MaxPages       int
	Navigation     string
	StrictChecksum bool
	ScenarioFile   string
	Scenario       *Scenario `json:"-"`
	TraceFile      string
	MaxFailureRate float64
	P95Budget      time.Duration
	BrowserUsers   int
	BrowserUntil   time.Duration
	BrowserScript  string
	BrowserOutput  string
}

// Scenario is a finite, ordered GET journey. Explicit paths make reports stable
// and allow assertions for authenticated/personalized or otherwise dynamic HTML.
// Automated form submission is deliberately not inferred from crawled pages.
type Scenario struct {
	Name  string        `json:"name"`
	Steps []JourneyStep `json:"steps"`
}
type JourneyStep struct {
	Name   string       `json:"name"`
	Path   string       `json:"path"`
	Expect Expectations `json:"expect"`
}
type Expectations struct {
	Status        []int    `json:"status,omitempty"`
	ContentType   string   `json:"content_type,omitempty"`
	Contains      []string `json:"contains,omitempty"`
	NotContains   []string `json:"not_contains,omitempty"`
	TitleContains string   `json:"title_contains,omitempty"`
	Selectors     []string `json:"selectors,omitempty"`
}

func (c *SwarmConfig) prepareExperience() error {
	x := &c.Experience
	if _, err := parseOrigin(c.Hit); err != nil {
		return err
	}
	if c.Limit == 0 || c.Limit < -1 {
		return fmt.Errorf("limit must be positive or -1")
	}
	if c.Terrain < 1 || c.Pack < 1 || c.Terrain > 10000 || c.Pack > 100000 {
		return fmt.Errorf("terrain/pack out of range")
	}
	if x.ThinkMin < 0 || x.ThinkMax < x.ThinkMin {
		return fmt.Errorf("think durations require 0 <= min <= max")
	}
	if x.MaxPages < 0 || x.RequestTimeout < 0 || x.MaxBodyBytes < 0 {
		return fmt.Errorf("negative experience limit")
	}
	if x.RequestTimeout == 0 {
		x.RequestTimeout = 10 * time.Second
	}
	if x.MaxBodyBytes == 0 {
		x.MaxBodyBytes = 2 << 20
	}
	if x.MaxBodyBytes > 64<<20 {
		return fmt.Errorf("max-body-bytes must be at most 64 MiB")
	}
	if x.Navigation == "" {
		x.Navigation = "links"
	}
	if x.Navigation != "links" && x.Navigation != "random" {
		return fmt.Errorf("navigation must be links or random")
	}
	if math.IsNaN(x.MaxFailureRate) || math.IsInf(x.MaxFailureRate, 0) || x.MaxFailureRate < -1 || (x.MaxFailureRate > -1 && x.MaxFailureRate < 0) || x.MaxFailureRate > 1 || x.P95Budget < 0 {
		return fmt.Errorf("failure rate must be -1 (disabled) or 0..1 and p95 budget nonnegative")
	}
	if x.BrowserUsers < 0 || x.BrowserUsers > 32 {
		return fmt.Errorf("browser-users must be 0..32")
	}
	if x.BrowserUntil == 0 {
		x.BrowserUntil = c.Until
	}
	if x.BrowserUntil < 0 {
		return fmt.Errorf("browser-until must be positive")
	}
	if x.ScenarioFile != "" {
		f, err := os.Open(x.ScenarioFile)
		if err != nil {
			return err
		}
		defer f.Close()
		d := json.NewDecoder(io.LimitReader(f, 1<<20))
		d.DisallowUnknownFields()
		var s Scenario
		if err := d.Decode(&s); err != nil {
			return fmt.Errorf("scenario: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("scenario must contain one JSON object")
		}
		if len(s.Steps) == 0 || len(s.Steps) > 100 {
			return fmt.Errorf("scenario needs 1..100 steps")
		}
		for i, step := range s.Steps {
			if resolveURL(c.Hit, step.Path) == "" {
				return fmt.Errorf("scenario step %d: path must stay on target origin", i+1)
			}
			for _, code := range step.Expect.Status {
				if code < 100 || code > 599 {
					return fmt.Errorf("invalid expected status %d", code)
				}
			}
			for _, selector := range step.Expect.Selectors {
				if !simpleSelector.MatchString(selector) {
					return fmt.Errorf("selector %q: use a tag, #id, or .class (same contract in both engines)", selector)
				}
			}
		}
		x.Scenario = &s
	}
	return nil
}

type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}
type PageEvidence struct {
	Mode        string        `json:"mode"`
	ContentType string        `json:"content_type"`
	Title       string        `json:"title,omitempty"`
	HTML        bool          `json:"html"`
	TextBytes   int           `json:"text_bytes"`
	LinkCount   int           `json:"link_count"`
	Checks      []CheckResult `json:"checks"`
}

var simpleSelector = regexp.MustCompile(`^(?:[a-zA-Z][a-zA-Z0-9-]*|[#.][a-zA-Z_][a-zA-Z0-9_-]*)$`)

func inspectPage(body []byte, contentType string, status int, expected Expectations) PageEvidence {
	p := PageEvidence{Mode: "http-response", ContentType: contentType}
	check := func(name string, pass bool, detail string) {
		p.Checks = append(p.Checks, CheckResult{name, pass, detail})
	}
	statusOK := status >= 200 && status < 300
	if len(expected.Status) > 0 {
		statusOK = false
		for _, s := range expected.Status {
			if s == status {
				statusOK = true
			}
		}
	}
	check("status", statusOK, fmt.Sprintf("received %d", status))
	media, _, _ := mime.ParseMediaType(contentType)
	p.HTML = media == "text/html" || media == "application/xhtml+xml"
	if expected.ContentType != "" {
		check("content-type", strings.EqualFold(media, expected.ContentType), media)
	}
	var doc *html.Node
	var text strings.Builder
	if p.HTML {
		var err error
		doc, err = html.Parse(bytes.NewReader(body))
		check("html-parse", err == nil, "HTML5 parsing repairs malformed markup; this is not conformance or rendering validation")
		if doc != nil {
			var walk func(*html.Node, bool)
			walk = func(n *html.Node, hidden bool) {
				if n.Type == html.ElementNode {
					hidden = hidden || n.Data == "script" || n.Data == "style" || n.Data == "head" || n.Data == "template"
					if n.Data == "title" && n.FirstChild != nil {
						p.Title = clip(n.FirstChild.Data, 256)
					}
					if n.Data == "a" {
						p.LinkCount++
					}
				}
				if n.Type == html.TextNode && !hidden {
					text.WriteString(n.Data)
					text.WriteByte(' ')
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c, hidden)
				}
			}
			walk(doc, false)
		}
		p.TextBytes = len(strings.TrimSpace(text.String()))
		check("nonempty-html", len(bytes.TrimSpace(body)) > 0, "response body must not be empty")
	}
	// Content assertions apply to raw server HTML. Browser assertions apply to
	// visible body text, allowing the same expected user-facing text in both.
	for i, s := range expected.Contains {
		check(fmt.Sprintf("contains[%d]", i), bytes.Contains(body, []byte(s)), "required content")
	}
	for i, s := range expected.NotContains {
		check(fmt.Sprintf("not-contains[%d]", i), !bytes.Contains(body, []byte(s)), "forbidden content")
	}
	if expected.TitleContains != "" {
		check("title", strings.Contains(p.Title, expected.TitleContains), "required title text")
	}
	for _, s := range expected.Selectors {
		check("selector:"+s, findElement(doc, s), "element must exist in server HTML; visibility untested")
	}
	return p
}

func findElement(n *html.Node, selector string) bool {
	if n == nil {
		return false
	}
	if n.Type == html.ElementNode {
		if n.Data == selector {
			return true
		}
		for _, a := range n.Attr {
			if strings.HasPrefix(selector, "#") && a.Key == "id" && a.Val == selector[1:] {
				return true
			}
			if strings.HasPrefix(selector, ".") && a.Key == "class" {
				for _, s := range strings.Fields(a.Val) {
					if s == selector[1:] {
						return true
					}
				}
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if findElement(c, selector) {
			return true
		}
	}
	return false
}

// safeURL intentionally omits userinfo, query values and fragments from evidence.
// Cookie values, authorization headers and full response bodies are never stored.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	u.User = nil
	u.Fragment = ""
	u.RawQuery = ""
	u.ForceQuery = false
	return clip(u.String(), 2048)
}
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func (v Visit) failed() bool {
	if v.Cancelled {
		return false
	}
	if v.Error != nil {
		return true
	}
	for _, c := range v.Page.Checks {
		if !c.Passed {
			return true
		}
	}
	return len(v.Page.Checks) == 0 && (v.StatusCode < 200 || v.StatusCode >= 400)
}
