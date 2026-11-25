package journey

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
	"github.com/andreimerlescu/lemmings/v2/internal/secrets"
	"golang.org/x/net/html"
)

const (
	maxExtractInputBytes = 512 << 10 // cap parser work per error body (R3)
	maxBodyExcerptBytes  = 4096
	maxStackTraceBytes   = 2048
	maxDetailsCount      = 8
	maxDetailLineBytes   = 320
)

// ExtractedError holds parsed, secret-scrubbed diagnostics extracted from an
// error response body (HTML, JSON, XML, or plain text).
type ExtractedError struct {
	Format          string            `json:"format"` // html, json, xml, text, empty
	StatusCode      int               `json:"status_code"`
	ContentType     string            `json:"content_type,omitempty"`
	Title           string            `json:"title,omitempty"`
	Headline        string            `json:"headline,omitempty"`
	ErrorCode       string            `json:"error_code,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`
	Summary         string            `json:"summary"`
	Details         []string          `json:"details,omitempty"`
	StackTrace      string            `json:"stack_trace,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	BodyExcerpt     string            `json:"body_excerpt,omitempty"`
	BodyBytes       int64             `json:"body_bytes"`
	BodySHA512      string            `json:"body_sha512,omitempty"`
}

// safeResponseHeaderNames is the explicit allowlist of non-sensitive diagnostic
// response headers kept in ExtractedError (never Set-Cookie, Authorization, etc.).
var safeResponseHeaderNames = []string{
	"Content-Type",
	"Content-Length",
	"Cache-Control",
	"Retry-After",
	"Server",
	"Via",
	"X-Request-Id",
	"X-Correlation-Id",
	"X-Trace-Id",
	"Traceparent",
	"Cf-Ray",
	"X-Amz-Request-Id",
	"X-Cache",
	"X-Error-Code",
}

var stackLinePattern = regexp.MustCompile(`(?i)(^\s*at\s+\S+|\bgoroutine\s+\d+|Traceback \(most recent call last\)|\.go:\d+|\.js:\d+:\d+|\.ts:\d+:\d+|\.py", line \d+|Exception:|Error:|panic:)`)

// ExtractErrorBody parses a response body and allowlisted headers into a
// bounded, secret-scrubbed ExtractedError. It never panics on malformed input.
func ExtractErrorBody(status int, contentType string, headers http.Header, body []byte) ExtractedError {
	ex := ExtractedError{
		StatusCode:      status,
		ContentType:     contentType,
		BodyBytes:       int64(len(body)),
		ResponseHeaders: allowlistHeaders(headers),
	}
	if headers != nil {
		for _, k := range []string{"X-Request-Id", "X-Correlation-Id", "X-Trace-Id", "Cf-Ray", "X-Amz-Request-Id"} {
			if v := strings.TrimSpace(headers.Get(k)); v != "" {
				ex.RequestID = discovery.Clip(secrets.ScrubText(v), 128)
				break
			}
		}
	}
	if len(body) > 0 {
		ex.BodySHA512 = discovery.SHA512Hex(body)
	}
	work := body
	if len(work) > maxExtractInputBytes {
		work = work[:maxExtractInputBytes]
	}

	trimmed := bytes.TrimSpace(work)
	if len(trimmed) == 0 {
		ex.Format = "empty"
		ex.Summary = defaultSummary(status, "")
		return ex
	}

	media, _, _ := mime.ParseMediaType(contentType)
	media = strings.ToLower(media)

	switch {
	case media == "application/json" || strings.HasSuffix(media, "+json") || looksLikeJSON(trimmed):
		extractJSON(&ex, trimmed)
	case media == "text/html" || media == "application/xhtml+xml" || looksLikeHTML(trimmed):
		extractHTML(&ex, trimmed)
	case media == "application/xml" || media == "text/xml" || strings.HasSuffix(media, "+xml"):
		extractXML(&ex, trimmed)
	default:
		extractPlainText(&ex, trimmed)
	}

	// Scrub every text field before returning (S5/SEC-10).
	ex.Title = cleanClip(ex.Title, maxTitleBytes)
	ex.Headline = cleanClip(ex.Headline, maxDetailLineBytes)
	ex.ErrorCode = cleanClip(ex.ErrorCode, 128)
	ex.RequestID = cleanClip(ex.RequestID, 128)
	ex.StackTrace = discovery.Clip(secrets.ScrubText(strings.TrimSpace(ex.StackTrace)), maxStackTraceBytes)
	ex.BodyExcerpt = discovery.Clip(secrets.ScrubText(strings.TrimSpace(ex.BodyExcerpt)), maxBodyExcerptBytes)

	for i := range ex.Details {
		ex.Details[i] = cleanClip(ex.Details[i], maxDetailLineBytes)
	}
	ex.Details = compactStrings(ex.Details, maxDetailsCount)

	if ex.Summary == "" {
		ex.Summary = pickSummary(&ex)
	}
	ex.Summary = cleanClip(ex.Summary, maxDetailLineBytes)
	return ex
}

func allowlistHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string)
	for _, name := range safeResponseHeaderNames {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			out[name] = discovery.Clip(secrets.ScrubText(v), 256)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func looksLikeJSON(b []byte) bool {
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

func looksLikeHTML(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 64)]))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html") || strings.Contains(head, "<body") || strings.Contains(head, "<h1")
}

func extractJSON(ex *ExtractedError, body []byte) {
	ex.Format = "json"
	ex.BodyExcerpt = string(body[:min(len(body), maxBodyExcerptBytes)])

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		// Fall back to text extraction if body only started with '{'.
		extractPlainText(ex, body)
		return
	}

	// Pretty-print compact JSON excerpt if small enough.
	if pretty, err := json.MarshalIndent(obj, "", "  "); err == nil {
		ex.BodyExcerpt = string(pretty[:min(len(pretty), maxBodyExcerptBytes)])
	}

	getStr := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if raw, ok := m[k]; ok {
				switch v := raw.(type) {
				case string:
					if s := strings.TrimSpace(v); s != "" {
						return s
					}
				case float64:
					return fmt.Sprintf("%g", v)
				}
			}
		}
		return ""
	}

	// Unwrap nested {"error": {...}} objects common in REST/GraphQL APIs.
	inner := obj
	if nested, ok := obj["error"].(map[string]any); ok {
		inner = nested
	}

	ex.Headline = firstNonEmpty(
		getStr(inner, "title", "message", "error", "detail", "reason"),
		getStr(obj, "title", "message", "error", "detail", "reason"),
	)
	ex.ErrorCode = firstNonEmpty(
		getStr(inner, "code", "error_code", "type", "status"),
		getStr(obj, "code", "error_code", "type"),
	)
	if ex.RequestID == "" {
		ex.RequestID = firstNonEmpty(
			getStr(inner, "request_id", "trace_id", "correlation_id", "incident_id"),
			getStr(obj, "request_id", "trace_id", "correlation_id", "incident_id"),
		)
	}
	if detail := firstNonEmpty(getStr(inner, "detail", "description", "hint"), getStr(obj, "detail", "description", "hint")); detail != "" && detail != ex.Headline {
		ex.Details = append(ex.Details, detail)
	}
	if stack := firstNonEmpty(getStr(inner, "stack", "stack_trace", "trace"), getStr(obj, "stack", "stack_trace")); stack != "" {
		ex.StackTrace = stack
	}

	// Collect array error lists (e.g. {"errors": [{"message": ...}]}).
	if arr, ok := obj["errors"].([]any); ok {
		for _, item := range arr {
			if len(ex.Details) >= maxDetailsCount {
				break
			}
			switch v := item.(type) {
			case string:
				ex.Details = append(ex.Details, v)
			case map[string]any:
				if msg := getStr(v, "message", "detail", "error", "title"); msg != "" {
					if field := getStr(v, "field", "path"); field != "" {
						msg = field + ": " + msg
					}
					ex.Details = append(ex.Details, msg)
				}
			}
		}
	}
}

func extractHTML(ex *ExtractedError, body []byte) {
	ex.Format = "html"
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		extractPlainText(ex, body)
		return
	}

	var headings []string
	var alerts []string
	var paragraphs []string
	var preBlocks []string
	var visible strings.Builder

	var walk func(n *html.Node, hidden bool)
	walk = func(n *html.Node, hidden bool) {
		switch n.Type {
		case html.ElementNode:
			tag := strings.ToLower(n.Data)
			switch tag {
			case "script", "style", "template", "noscript", "svg":
				return
			case "head":
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.ElementNode && strings.EqualFold(c.Data, "title") && ex.Title == "" {
						ex.Title = normalizeWhitespace(nodeText(c))
					}
				}
				return
			case "h1", "h2":
				if txt := normalizeWhitespace(nodeText(n)); txt != "" {
					headings = append(headings, txt)
				}
			case "pre", "code":
				if tag == "pre" {
					if txt := strings.TrimSpace(nodeText(n)); txt != "" {
						preBlocks = append(preBlocks, txt)
					}
				}
			case "p", "li":
				if txt := normalizeWhitespace(nodeText(n)); txt != "" && len(paragraphs) < maxDetailsCount*2 {
					paragraphs = append(paragraphs, txt)
				}
			}
			if isErrorContainer(n) {
				if txt := normalizeWhitespace(nodeText(n)); txt != "" {
					alerts = append(alerts, txt)
				}
			}
		case html.TextNode:
			if !hidden && visible.Len() < maxBodyExcerptBytes*2 {
				if txt := normalizeWhitespace(n.Data); txt != "" {
					if visible.Len() > 0 {
						visible.WriteByte('\n')
					}
					visible.WriteString(txt)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, hidden)
		}
	}
	walk(doc, false)

	ex.Headline = firstNonEmpty(sliceFirst(alerts), sliceFirst(headings), ex.Title)
	for _, a := range alerts {
		if a != ex.Headline {
			ex.Details = append(ex.Details, a)
		}
	}
	for _, h := range headings {
		if h != ex.Headline {
			ex.Details = append(ex.Details, h)
		}
	}
	for _, p := range paragraphs {
		if p != ex.Headline {
			ex.Details = append(ex.Details, p)
		}
	}
	for _, pre := range preBlocks {
		if stackLinePattern.MatchString(pre) || len(pre) > 40 {
			ex.StackTrace = pre
			break
		}
	}
	ex.BodyExcerpt = visible.String()
	if ex.BodyExcerpt == "" {
		ex.BodyExcerpt = string(body[:min(len(body), maxBodyExcerptBytes)])
	}
}

func isErrorContainer(n *html.Node) bool {
	for _, a := range n.Attr {
		key := strings.ToLower(a.Key)
		val := strings.ToLower(a.Val)
		if key == "role" && (val == "alert" || val == "alertdialog") {
			return true
		}
		if key == "class" || key == "id" {
			for _, tok := range strings.FieldsFunc(val, func(r rune) bool {
				return r == ' ' || r == '\t' || r == '-' || r == '_'
			}) {
				switch tok {
				case "error", "err", "alert", "danger", "exception", "fault", "traceback", "stacktrace", "failure":
					return true
				}
			}
		}
	}
	return false
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(cur *html.Node) {
		if cur.Type == html.ElementNode {
			switch strings.ToLower(cur.Data) {
			case "script", "style", "template", "noscript":
				return
			}
		}
		if cur.Type == html.TextNode {
			b.WriteString(cur.Data)
			b.WriteByte(' ')
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

func extractXML(ex *ExtractedError, body []byte) {
	ex.Format = "xml"
	ex.BodyExcerpt = string(body[:min(len(body), maxBodyExcerptBytes)])
	dec := xml.NewDecoder(bytes.NewReader(body))
	type elemFrame struct {
		name        string
		text        strings.Builder
		hasChildren bool
	}
	var stack []elemFrame
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) > 0 {
				stack[len(stack)-1].hasChildren = true
			}
			stack = append(stack, elemFrame{name: strings.ToLower(t.Name.Local)})
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			frame := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if frame.hasChildren {
				continue
			}
			val := normalizeWhitespace(frame.text.String())
			if val == "" {
				continue
			}
			switch frame.name {
			case "message", "error", "faultstring", "detail", "title", "reason":
				if ex.Headline == "" {
					ex.Headline = val
				} else {
					ex.Details = append(ex.Details, val)
				}
			case "code", "faultcode":
				if ex.ErrorCode == "" {
					ex.ErrorCode = val
				}
			case "requestid", "request_id", "traceid":
				if ex.RequestID == "" {
					ex.RequestID = val
				}
			}
		}
	}
}

func extractPlainText(ex *ExtractedError, body []byte) {
	ex.Format = "text"
	text := string(body[:min(len(body), maxBodyExcerptBytes)])
	ex.BodyExcerpt = text

	lines := strings.Split(text, "\n")
	var stackLines []string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if ex.Headline == "" {
			ex.Headline = line
		} else if len(ex.Details) < maxDetailsCount {
			ex.Details = append(ex.Details, line)
		}
		if stackLinePattern.MatchString(raw) {
			stackLines = append(stackLines, raw)
		}
	}
	if len(stackLines) > 0 {
		ex.StackTrace = strings.Join(stackLines, "\n")
	}
}

func pickSummary(ex *ExtractedError) string {
	parts := make([]string, 0, 3)
	if ex.ErrorCode != "" {
		parts = append(parts, ex.ErrorCode)
	}
	if ex.Headline != "" {
		parts = append(parts, ex.Headline)
	} else if ex.Title != "" {
		parts = append(parts, ex.Title)
	}
	if len(parts) == 0 && len(ex.Details) > 0 {
		parts = append(parts, ex.Details[0])
	}
	return defaultSummary(ex.StatusCode, strings.Join(parts, ": "))
}

func defaultSummary(status int, extracted string) string {
	if extracted != "" {
		return extracted
	}
	if status > 0 {
		if text := http.StatusText(status); text != "" {
			return fmt.Sprintf("HTTP %d %s", status, text)
		}
		return fmt.Sprintf("HTTP %d", status)
	}
	return "empty error response"
}

func cleanClip(s string, maxBytes int) string {
	return discovery.Clip(secrets.ScrubText(normalizeWhitespace(s)), maxBytes)
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sliceFirst(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func compactStrings(in []string, limit int) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, min(len(in), limit))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out
}
