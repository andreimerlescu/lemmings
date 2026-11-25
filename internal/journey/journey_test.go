package journey

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestParseJourney_ValidAndInvalid(t *testing.T) {
	valid := []byte(`{"name":"checkout","steps":[
		{"name":"home","path":"/","expect":{"status":[200],"selectors":["nav",".grid","#hero"],"contains":["Welcome"]}},
		{"name":"pay","path":"/checkout","expect":{"status":[200],"not_contains":["Error"]}}
	]}`)
	j, err := Parse(bytes.NewReader(valid), "http://127.0.0.1:8080/")
	if err != nil {
		t.Fatalf("Parse valid journey: %v", err)
	}
	if j.Name != "checkout" || len(j.Steps) != 2 {
		t.Fatalf("unexpected parsed journey: %+v", j)
	}

	// Off-origin step must fail.
	offsite := []byte(`{"name":"bad","steps":[{"name":"evil","path":"https://evil.test/steal"}]}`)
	if _, err := Parse(bytes.NewReader(offsite), "http://127.0.0.1:8080/"); err == nil {
		t.Error("expected off-origin journey step to fail")
	}

	// Complex unsupported selector must fail.
	badSel := []byte(`{"name":"bad","steps":[{"name":"s","path":"/","expect":{"selectors":["div > span"]}}]}`)
	if _, err := Parse(bytes.NewReader(badSel), "http://127.0.0.1:8080/"); err == nil {
		t.Error("expected unsupported selector to fail")
	}
}

func TestInspectPage_ChecksAndBlankDetection(t *testing.T) {
	base := "http://127.0.0.1:8080/"
	htmlDoc := []byte(`<!doctype html><html><head><title>Neon Arcade</title></head>
		<body><nav><a href="/games">Games</a><a href="/logout">Sign out</a></nav>
		<main id="app" class="ready"><h1>Insert coin</h1></main></body></html>`)

	page, links := InspectPage(htmlDoc, "text/html; charset=utf-8", 200, Expectations{
		Status:        []int{200},
		ContentType:   "text/html",
		TitleContains: "Neon",
		Contains:      []string{"Insert coin"},
		NotContains:   []string{"Something went wrong"},
		Selectors:     []string{"nav", "#app", ".ready"},
	}, base)
	if page.FailedChecks() != 0 {
		t.Fatalf("expected all checks to pass, checks: %+v", page.Checks)
	}
	if page.Title != "Neon Arcade" {
		t.Errorf("unexpected page metadata: %+v", page)
	}
	// /logout must not be navigable.
	for _, link := range links {
		if strings.Contains(link, "logout") {
			t.Errorf("logout link should have been filtered: %q", link)
		}
	}

	// Blank HTML page fails not-blank check.
	blankPage, _ := InspectPage([]byte(`<html><body></body></html>`), "text/html", 200, Expectations{}, base)
	if blankPage.FailedChecks() == 0 {
		t.Errorf("expected blank page to fail not-blank check: page=%+v", blankPage)
	}

	// SPA root with script tag is NOT blank.
	spaPage, _ := InspectPage([]byte(`<html><body><div id="root"></div><script src="/app.js"></script></body></html>`), "text/html", 200, Expectations{}, base)
	if spaPage.FailedChecks() != 0 {
		t.Errorf("SPA page with script should not be flagged blank: page=%+v", spaPage)
	}
}

func TestExtractErrorBody_HTML_JSON_XML_Text_AndScrub(t *testing.T) {
	hdr := http.Header{
		"X-Request-Id":  {"req-42"},
		"Retry-After":   {"5"},
		"Set-Cookie":    {"session=supersecretcookie"},
		"Authorization": {"Bearer secret-token-123"},
	}

	// 1. JSON Problem Details (RFC 7807)
	jsonBody := []byte(`{"type":"about:blank","title":"Gateway Fault","code":"E_UPSTREAM","error":"upstream timeout","detail":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJ1IjoxfQ.sig failed","request_id":"req-42"}`)
	exJSON := ExtractErrorBody(502, "application/problem+json", hdr, jsonBody)
	if exJSON.Format != "json" || exJSON.ErrorCode != "E_UPSTREAM" || exJSON.RequestID != "req-42" {
		t.Errorf("JSON extraction mismatch: %+v", exJSON)
	}
	if strings.Contains(exJSON.Summary+strings.Join(exJSON.Details, " ")+exJSON.BodyExcerpt, "eyJhbGciOiJIUzI1NiJ9") {
		t.Errorf("JSON extractor leaked bearer token: %+v", exJSON)
	}
	if _, leaked := exJSON.ResponseHeaders["Set-Cookie"]; leaked {
		t.Error("Set-Cookie must NOT appear in allowlisted ResponseHeaders")
	}
	if exJSON.ResponseHeaders["X-Request-Id"] != "req-42" {
		t.Errorf("expected allowlisted X-Request-Id header, got %v", exJSON.ResponseHeaders)
	}

	// 2. HTML error page with stack trace & script stripping
	htmlBody := []byte(`<!doctype html><html><head><title>500 Internal Error</title><script>var secret="x";</script></head>
		<body><h1>Database Pool Exhausted</h1><div class="alert">could not acquire pg connection</div>
		<pre>goroutine 1 [running]:
main.handleCheckout()
	/app/checkout.go:42</pre></body></html>`)
	exHTML := ExtractErrorBody(500, "text/html; charset=utf-8", hdr, htmlBody)
	if exHTML.Format != "html" || !strings.Contains(exHTML.Headline, "could not acquire pg connection") {
		t.Errorf("HTML extraction mismatch: %+v", exHTML)
	}
	if !strings.Contains(strings.Join(exHTML.Details, " "), "Database Pool Exhausted") {
		t.Errorf("expected h1 in HTML details, got %+v", exHTML.Details)
	}
	if !strings.Contains(exHTML.StackTrace, "goroutine 1") {
		t.Errorf("expected stack trace extraction, got %q", exHTML.StackTrace)
	}
	if strings.Contains(exHTML.BodyExcerpt, "var secret") {
		t.Errorf("script contents should be stripped from HTML excerpt: %q", exHTML.BodyExcerpt)
	}

	// 3. XML error body (S3/SOAP style)
	xmlBody := []byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>XML-99</RequestId></Error>`)
	exXML := ExtractErrorBody(403, "application/xml", nil, xmlBody)
	if exXML.Format != "xml" || exXML.ErrorCode != "AccessDenied" || exXML.RequestID != "XML-99" {
		t.Errorf("XML extraction mismatch: %+v", exXML)
	}

	// 4. Plain text
	exText := ExtractErrorBody(503, "text/plain", nil, []byte("503 Service Temporarily Unavailable\nTry again later"))
	if exText.Format != "text" || !strings.Contains(exText.Summary, "503 Service") {
		t.Errorf("text extraction mismatch: %+v", exText)
	}
}

func FuzzExtractErrorBody(f *testing.F) {
	f.Add(500, "text/html", []byte("<h1>Boom</h1><p>password=secret123</p>"))
	f.Add(502, "application/json", []byte(`{"error":"bad gateway","code":"E502"}`))
	f.Add(403, "application/xml", []byte(`<Error><Code>Denied</Code></Error>`))
	f.Add(503, "text/plain", []byte("overloaded"))
	f.Fuzz(func(t *testing.T, status int, ct string, body []byte) {
		ex := ExtractErrorBody(status, ct, nil, body)
		if len(ex.BodyExcerpt) > maxBodyExcerptBytes+16 {
			t.Fatalf("excerpt exceeded bound: %d", len(ex.BodyExcerpt))
		}
	})
}

func BenchmarkExtractErrorBody(b *testing.B) {
	hdr := http.Header{"X-Request-Id": {"req-bench-1"}, "Retry-After": {"2"}}
	htmlBody := []byte(`<!doctype html><html><head><title>500 Error</title></head><body><h1>Database Fault</h1><div class="alert">pool exhausted</div><pre>goroutine 1 [running]:\nmain.go:42</pre></body></html>`)
	jsonBody := []byte(`{"title":"Gateway Timeout","code":"E_TIMEOUT","error":"upstream timed out","request_id":"req-bench-1"}`)

	b.ResetTimer()
	for range b.N {
		_ = ExtractErrorBody(500, "text/html; charset=utf-8", hdr, htmlBody)
		_ = ExtractErrorBody(504, "application/problem+json", hdr, jsonBody)
	}
}
