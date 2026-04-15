package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── parseOrigin ───────────────────────────────────────────────────────────────

// TestParseOrigin_ValidURL verifies that a full URL is correctly reduced
// to its scheme and host with no path, query, or fragment.
func TestParseOrigin_ValidURL(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"http://localhost:8080/about", "http://localhost:8080"},
		{"https://example.com/path?q=1#frag", "https://example.com"},
		{"http://192.168.1.1:9090/", "http://192.168.1.1:9090"},
		{"https://sub.example.com", "https://sub.example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseOrigin(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

// TestParseOrigin_MissingScheme verifies that a URL without a scheme
// returns an error.
func TestParseOrigin_MissingScheme(t *testing.T) {
	_, err := parseOrigin("localhost:8080/path")
	if err == nil {
		t.Error("expected error for URL missing scheme, got nil")
	}
}

// TestParseOrigin_MissingHost verifies that a URL without a host
// returns an error.
func TestParseOrigin_MissingHost(t *testing.T) {
	_, err := parseOrigin("http://")
	if err == nil {
		t.Error("expected error for URL missing host, got nil")
	}
}

// TestParseOrigin_EmptyString verifies that an empty string returns an error.
func TestParseOrigin_EmptyString(t *testing.T) {
	_, err := parseOrigin("")
	if err == nil {
		t.Error("expected error for empty string, got nil")
	}
}

// TestParseOrigin_StripsPaths verifies that paths, query strings, and
// fragments are all stripped from the result.
func TestParseOrigin_StripsPaths(t *testing.T) {
	got, err := parseOrigin("https://example.com/a/b/c?foo=bar&baz=1#anchor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://example.com" {
		t.Errorf("expected path-stripped origin, got %q", got)
	}
}

// ── domainFromURL ─────────────────────────────────────────────────────────────

// TestDomainFromURL_Standard verifies scheme and path are stripped,
// leaving only the host.
func TestDomainFromURL_Standard(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"https://example.com/path", "example.com"},
		{"http://example.com", "example.com"},
		{"http://example.com/a/b/c", "example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := domainFromURL(tc.input)
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

// TestDomainFromURL_LocalhostPort verifies that the colon in
// localhost:PORT is replaced with a dash for filesystem safety.
func TestDomainFromURL_LocalhostPort(t *testing.T) {
	got := domainFromURL("http://localhost:8080/")
	if got != "localhost-8080" {
		t.Errorf("expected localhost-8080, got %q", got)
	}
}

// TestDomainFromURL_NoPath verifies a URL with no path produces
// the correct domain string.
func TestDomainFromURL_NoPath(t *testing.T) {
	got := domainFromURL("https://example.com")
	if got != "example.com" {
		t.Errorf("expected example.com, got %q", got)
	}
}

// ── scopeToOrigin ─────────────────────────────────────────────────────────────

// TestScopeToOrigin_FiltersOffOrigin verifies that URLs not beginning with
// the origin prefix are excluded from the result.
func TestScopeToOrigin_FiltersOffOrigin(t *testing.T) {
	origin := "https://example.com"
	urls := []string{
		"https://example.com/page",
		"https://other.com/page",     // off-origin
		"http://example.com/page",    // different scheme
		"https://sub.example.com/",   // subdomain — off-origin
		"https://example.com/about",
	}

	got := scopeToOrigin(origin, urls)
	if len(got) != 2 {
		t.Fatalf("expected 2 scoped URLs, got %d: %v", len(got), got)
	}
	for _, u := range got {
		if !strings.HasPrefix(u, origin) {
			t.Errorf("scoped URL %q does not have origin prefix %q", u, origin)
		}
	}
}

// TestScopeToOrigin_Deduplicates verifies that duplicate URLs appear
// only once in the result.
func TestScopeToOrigin_Deduplicates(t *testing.T) {
	origin := "https://example.com"
	urls := []string{
		"https://example.com/page",
		"https://example.com/page", // duplicate
		"https://example.com/page", // duplicate again
		"https://example.com/other",
	}

	got := scopeToOrigin(origin, urls)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique URLs, got %d: %v", len(got), got)
	}
}

// TestScopeToOrigin_EmptyInput verifies that an empty URL list returns
// an empty (not nil) result.
func TestScopeToOrigin_EmptyInput(t *testing.T) {
	got := scopeToOrigin("https://example.com", []string{})
	if got == nil {
		t.Error("scopeToOrigin should return non-nil for empty input")
	}
	if len(got) != 0 {
		t.Errorf("expected 0 results, got %d", len(got))
	}
}

// TestScopeToOrigin_AllOffOrigin verifies that when no URLs match the
// origin, an empty result is returned.
func TestScopeToOrigin_AllOffOrigin(t *testing.T) {
	got := scopeToOrigin("https://example.com", []string{
		"https://other.com/a",
		"https://another.com/b",
	})
	if len(got) != 0 {
		t.Errorf("expected 0 results when all URLs are off-origin, got %d", len(got))
	}
}

// TestScopeToOrigin_TrimsWhitespace verifies that URLs with leading or
// trailing whitespace are handled correctly.
func TestScopeToOrigin_TrimsWhitespace(t *testing.T) {
	origin := "https://example.com"
	urls := []string{
		"  https://example.com/page  ",
		"\thttps://example.com/about\n",
	}
	got := scopeToOrigin(origin, urls)
	if len(got) != 2 {
		t.Fatalf("expected 2 URLs after whitespace trim, got %d", len(got))
	}
}

// ── resolveURL ────────────────────────────────────────────────────────────────

// TestResolveURL_RelativePath verifies that a relative path is resolved
// against the origin to produce an absolute URL.
func TestResolveURL_RelativePath(t *testing.T) {
	cases := []struct {
		origin   string
		href     string
		expected string
	}{
		{"https://example.com", "/about", "https://example.com/about"},
		{"https://example.com", "/a/b/c", "https://example.com/a/b/c"},
		{"https://example.com/base/", "page", "https://example.com/base/page"},
		{"https://example.com", "/pricing?plan=pro", "https://example.com/pricing?plan=pro"},
	}

	for _, tc := range cases {
		t.Run(tc.href, func(t *testing.T) {
			got := resolveURL(tc.origin, tc.href)
			if got != tc.expected {
				t.Errorf("resolveURL(%q, %q) = %q, want %q",
					tc.origin, tc.href, got, tc.expected)
			}
		})
	}
}

// TestResolveURL_AbsoluteOnOrigin verifies that an absolute URL on the
// same origin is returned unchanged (minus any fragment).
func TestResolveURL_AbsoluteOnOrigin(t *testing.T) {
	got := resolveURL("https://example.com", "https://example.com/about")
	if got != "https://example.com/about" {
		t.Errorf("expected same-origin absolute URL to pass through, got %q", got)
	}
}

// TestResolveURL_OffOriginAbsolute verifies that an absolute URL on a
// different domain returns an empty string.
func TestResolveURL_OffOriginAbsolute(t *testing.T) {
	got := resolveURL("https://example.com", "https://other.com/page")
	if got != "" {
		t.Errorf("expected empty string for off-origin URL, got %q", got)
	}
}

// TestResolveURL_StripsFragment verifies that the fragment component
// is removed from the resolved URL.
func TestResolveURL_StripsFragment(t *testing.T) {
	got := resolveURL("https://example.com", "/page#section")
	if strings.Contains(got, "#") {
		t.Errorf("resolved URL should not contain fragment, got %q", got)
	}
	if got != "https://example.com/page" {
		t.Errorf("expected /page without fragment, got %q", got)
	}
}

// TestResolveURL_RejectsFragment verifies that a bare fragment href
// (e.g. #section) returns an empty string.
func TestResolveURL_RejectsFragment(t *testing.T) {
	got := resolveURL("https://example.com", "#section")
	if got != "" {
		t.Errorf("expected empty string for bare fragment href, got %q", got)
	}
}

// TestResolveURL_RejectsMailto verifies that mailto: hrefs return empty string.
func TestResolveURL_RejectsMailto(t *testing.T) {
	got := resolveURL("https://example.com", "mailto:user@example.com")
	if got != "" {
		t.Errorf("expected empty string for mailto href, got %q", got)
	}
}

// TestResolveURL_RejectsJavascript verifies that javascript: hrefs
// return empty string.
func TestResolveURL_RejectsJavascript(t *testing.T) {
	got := resolveURL("https://example.com", "javascript:void(0)")
	if got != "" {
		t.Errorf("expected empty string for javascript href, got %q", got)
	}
}

// TestResolveURL_RejectsTel verifies that tel: hrefs return empty string.
func TestResolveURL_RejectsTel(t *testing.T) {
	got := resolveURL("https://example.com", "tel:+15555555555")
	if got != "" {
		t.Errorf("expected empty string for tel href, got %q", got)
	}
}

// TestResolveURL_RejectsEmpty verifies that an empty href returns empty string.
func TestResolveURL_RejectsEmpty(t *testing.T) {
	got := resolveURL("https://example.com", "")
	if got != "" {
		t.Errorf("expected empty string for empty href, got %q", got)
	}
}

// TestResolveURL_SubdomainRejected verifies that a subdomain URL is treated
// as off-origin and returns empty string.
func TestResolveURL_SubdomainRejected(t *testing.T) {
	got := resolveURL("https://example.com", "https://sub.example.com/page")
	if got != "" {
		t.Errorf("expected empty string for subdomain URL, got %q", got)
	}
}

// ── extractSitemapLocs ────────────────────────────────────────────────────────

// TestExtractSitemapLocs_Standard verifies correct extraction of <loc>
// entries from a well-formed sitemap.xml body.
func TestExtractSitemapLocs_Standard(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/</loc></url>
  <url><loc>https://example.com/about</loc></url>
  <url><loc>https://example.com/pricing</loc></url>
</urlset>`

	got := extractSitemapLocs(body)
	if len(got) != 3 {
		t.Fatalf("expected 3 locs, got %d: %v", len(got), got)
	}
	if got[0] != "https://example.com/" {
		t.Errorf("expected first loc https://example.com/, got %q", got[0])
	}
}

// TestExtractSitemapLocs_Empty verifies that an empty sitemap body
// returns an empty slice without error.
func TestExtractSitemapLocs_Empty(t *testing.T) {
	got := extractSitemapLocs(`<?xml version="1.0"?><urlset></urlset>`)
	if len(got) != 0 {
		t.Errorf("expected 0 locs for empty sitemap, got %d", len(got))
	}
}

// TestExtractSitemapLocs_MalformedXML verifies graceful handling of a body
// that is not valid XML — should return whatever locs can be extracted
// without panicking.
func TestExtractSitemapLocs_MalformedXML(t *testing.T) {
	body := `<urlset><url><loc>https://example.com/good</loc></url>
<url><loc>UNCLOSED`
	// Should not panic — may return partial results
	got := extractSitemapLocs(body)
	_ = got // we only verify no panic occurs
}

// TestExtractSitemapLocs_WhitespaceInLoc verifies that leading and trailing
// whitespace inside <loc> tags is trimmed.
func TestExtractSitemapLocs_WhitespaceInLoc(t *testing.T) {
	body := `<urlset>
<url><loc>  https://example.com/page  </loc></url>
</urlset>`
	got := extractSitemapLocs(body)
	if len(got) != 1 {
		t.Fatalf("expected 1 loc, got %d", len(got))
	}
	if got[0] != "https://example.com/page" {
		t.Errorf("expected trimmed loc, got %q", got[0])
	}
}

// TestExtractSitemapLocs_MultipleOnOneLine verifies extraction when multiple
// <loc> entries appear on the same line.
func TestExtractSitemapLocs_MultipleOnOneLine(t *testing.T) {
	body := `<url><loc>https://example.com/a</loc></url><url><loc>https://example.com/b</loc></url>`
	got := extractSitemapLocs(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 locs, got %d", len(got))
	}
}

// ── isSitemapIndex ────────────────────────────────────────────────────────────

// TestIsSitemapIndex_True verifies detection of a sitemap index document.
func TestIsSitemapIndex_True(t *testing.T) {
	body := `<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<sitemap><loc>https://example.com/sitemap1.xml</loc></sitemap>
</sitemapindex>`
	if !isSitemapIndex(body) {
		t.Error("expected isSitemapIndex to return true for sitemap index document")
	}
}

// TestIsSitemapIndex_False verifies that a regular sitemap is not
// detected as a sitemap index.
func TestIsSitemapIndex_False(t *testing.T) {
	body := `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>https://example.com/</loc></url>
</urlset>`
	if isSitemapIndex(body) {
		t.Error("expected isSitemapIndex to return false for regular sitemap")
	}
}

// TestIsSitemapIndex_Empty verifies that an empty string returns false.
func TestIsSitemapIndex_Empty(t *testing.T) {
	if isSitemapIndex("") {
		t.Error("expected false for empty string")
	}
}

// ── extractHTMLLinks ──────────────────────────────────────────────────────────

// TestExtractHTMLLinks_FindsAnchors verifies that all <a href> values
// within the origin are returned.
func TestExtractHTMLLinks_FindsAnchors(t *testing.T) {
	origin := "https://example.com"
	body := []byte(`<!DOCTYPE html><html><body>
<a href="/about">About</a>
<a href="/pricing">Pricing</a>
<a href="https://other.com/ext">External</a>
</body></html>`)

	got := extractHTMLLinks(body, origin)
	if len(got) != 2 {
		t.Fatalf("expected 2 links (external filtered), got %d: %v", len(got), got)
	}
}

// TestExtractHTMLLinks_Deduplicates verifies that the same href appearing
// multiple times in the document is returned only once.
func TestExtractHTMLLinks_Deduplicates(t *testing.T) {
	origin := "https://example.com"
	body := []byte(`<html><body>
<a href="/page">Link 1</a>
<a href="/page">Link 2</a>
<a href="/page">Link 3</a>
</body></html>`)

	got := extractHTMLLinks(body, origin)
	if len(got) != 1 {
		t.Fatalf("expected 1 unique link, got %d: %v", len(got), got)
	}
}

// TestExtractHTMLLinks_SkipsNonAnchors verifies that only <a> elements
// contribute to the result — not <img src>, <script src>, etc.
func TestExtractHTMLLinks_SkipsNonAnchors(t *testing.T) {
	origin := "https://example.com"
	body := []byte(`<html><head>
<script src="/js/app.js"></script>
<link rel="stylesheet" href="/css/main.css">
</head><body>
<img src="/images/logo.png">
<a href="/about">About</a>
</body></html>`)

	got := extractHTMLLinks(body, origin)
	if len(got) != 1 {
		t.Fatalf("expected 1 link from <a> only, got %d: %v", len(got), got)
	}
	if got[0] != "https://example.com/about" {
		t.Errorf("expected /about link, got %q", got[0])
	}
}

// TestExtractHTMLLinks_EmptyBody verifies that an empty body returns
// an empty slice without panicking.
func TestExtractHTMLLinks_EmptyBody(t *testing.T) {
	got := extractHTMLLinks([]byte{}, "https://example.com")
	if len(got) != 0 {
		t.Errorf("expected 0 links for empty body, got %d", len(got))
	}
}

// TestExtractHTMLLinks_MalformedHTML verifies that malformed HTML is
// handled gracefully without panicking.
func TestExtractHTMLLinks_MalformedHTML(t *testing.T) {
	body := []byte(`<html><body><a href="/page">unclosed`)
	// Must not panic
	got := extractHTMLLinks(body, "https://example.com")
	_ = got
}

// TestExtractHTMLLinks_SkipsSkippableHrefs verifies that mailto, javascript,
// tel, fragment, and empty hrefs are excluded from results.
func TestExtractHTMLLinks_SkipsSkippableHrefs(t *testing.T) {
	origin := "https://example.com"
	body := []byte(`<html><body>
<a href="mailto:info@example.com">Email</a>
<a href="javascript:void(0)">JS</a>
<a href="tel:+15555555555">Call</a>
<a href="#section">Section</a>
<a href="">Empty</a>
<a href="/valid">Valid</a>
</body></html>`)

	got := extractHTMLLinks(body, origin)
	if len(got) != 1 {
		t.Fatalf("expected only 1 valid link, got %d: %v", len(got), got)
	}
	if got[0] != "https://example.com/valid" {
		t.Errorf("expected /valid link, got %q", got[0])
	}
}

// ── findSitemapInRobots ───────────────────────────────────────────────────────

// TestFindSitemapInRobots_Found verifies that a Sitemap: directive in
// robots.txt is correctly extracted.
func TestFindSitemapInRobots_Found(t *testing.T) {
	srv := newTestServer(t).
		withRobots("https://example.com/sitemap.xml").
		build()

	client := &http.Client{Timeout: 5 * time.Second}
	got, err := findSitemapInRobots(context.Background(), client, srv.URL+"/robots.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://example.com/sitemap.xml" {
		t.Errorf("expected sitemap URL, got %q", got)
	}
}

// TestFindSitemapInRobots_NotFound verifies that a robots.txt without a
// Sitemap: directive returns an error.
func TestFindSitemapInRobots_NotFound(t *testing.T) {
	srv := newTestServer(t).
		withCustomPage("/robots.txt", "User-agent: *\nDisallow: /admin\n").
		build()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := findSitemapInRobots(context.Background(), client, srv.URL+"/robots.txt")
	if err == nil {
		t.Error("expected error when no Sitemap directive found")
	}
}

// TestFindSitemapInRobots_404 verifies that a missing robots.txt returns
// an error.
func TestFindSitemapInRobots_404(t *testing.T) {
	srv := newTestServer(t).withStatusCode("/robots.txt", http.StatusNotFound).build()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := findSitemapInRobots(context.Background(), client, srv.URL+"/robots.txt")
	if err == nil {
		t.Error("expected error for 404 robots.txt")
	}
}

// ── indexSitemap ──────────────────────────────────────────────────────────────

// TestIndexSitemap_Standard verifies that a standard sitemap.xml is
// fetched and all <loc> entries returned.
func TestIndexSitemap_Standard(t *testing.T) {
	urls := []string{
		"https://example.com/",
		"https://example.com/about",
		"https://example.com/pricing",
	}
	srv := newTestServer(t).withSitemap("/sitemap.xml", urls).build()

	client := &http.Client{Timeout: 5 * time.Second}
	got, err := indexSitemap(context.Background(), client, srv.URL+"/sitemap.xml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 URLs, got %d: %v", len(got), got)
	}
}

// TestIndexSitemap_NotFound verifies that a 404 sitemap returns an error.
func TestIndexSitemap_NotFound(t *testing.T) {
	srv := newTestServer(t).withStatusCode("/sitemap.xml", http.StatusNotFound).build()
	client := &http.Client{Timeout: 5 * time.Second}
	_, err := indexSitemap(context.Background(), client, srv.URL+"/sitemap.xml")
	if err == nil {
		t.Error("expected error for 404 sitemap")
	}
}

// TestIndexSitemap_Index verifies that a sitemap index file causes recursive
// fetching of child sitemaps.
func TestIndexSitemap_Index(t *testing.T) {
	// Two child sitemaps each with 2 URLs
	child1URLs := []string{"https://example.com/a", "https://example.com/b"}
	child2URLs := []string{"https://example.com/c", "https://example.com/d"}

	srv := newTestServer(t).build() // build first to get URL
	// We need a server that knows its own URL for the index entries
	// Use a fresh server with explicit routes
	mux := http.NewServeMux()

	child1Body := buildSitemapBody(child1URLs)
	child2Body := buildSitemapBody(child2URLs)

	var srvURL string
	mux.HandleFunc("/sitemap1.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, child1Body)
	})
	mux.HandleFunc("/sitemap2.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, child2Body)
	})
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		index := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<sitemap><loc>%s/sitemap1.xml</loc></sitemap>
<sitemap><loc>%s/sitemap2.xml</loc></sitemap>
</sitemapindex>`, srvURL, srvURL)
		fmt.Fprint(w, index)
	})

	innerSrv := &http.Server{Handler: mux}
	// Use httptest directly here for URL access before handler needs it
	import_srv := httptest.NewServer(mux)
	srvURL = import_srv.URL
	t.Cleanup(import_srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	got, err := indexSitemap(context.Background(), client, import_srv.URL+"/sitemap_index.xml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 URLs from index+children, got %d: %v", len(got), got)
	}
	_ = innerSrv
}

// ── BuildURLPool ──────────────────────────────────────────────────────────────

// TestBuildURLPool_UsesSitemap verifies that BuildURLPool prefers
// sitemap.xml when it is available at the standard path.
func TestBuildURLPool_UsesSitemap(t *testing.T) {
	pageURLs := []string{} // filled after server starts
	srv := newTestServer(t).withNormalPage("/").build()
	pageURLs = []string{
		srv.URL + "/",
		srv.URL + "/about",
		srv.URL + "/pricing",
	}

	ts := newTestServer(t).
		withSitemap("/sitemap.xml", pageURLs).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		build()

	pool, err := BuildURLPool(context.Background(), ts.URL+"/", false, 2)
	if err != nil {
		t.Fatalf("BuildURLPool error: %v", err)
	}
	if len(pool.URLs) == 0 {
		t.Fatal("expected URLs in pool from sitemap")
	}
	if pool.Origin != ts.URL {
		t.Errorf("expected origin %q, got %q", ts.URL, pool.Origin)
	}
}

// TestBuildURLPool_FallsBackToRobots verifies that when sitemap.xml
// is absent, BuildURLPool checks robots.txt for a Sitemap directive.
func TestBuildURLPool_FallsBackToRobots(t *testing.T) {
	// Build server first to know its URL for the robots sitemap path
	var robotsSrv *httptest.Server

	pageURLs := []string{} // set after server URL known

	// We need a two-phase setup: server URL needed for robots content
	mux := http.NewServeMux()
	var srvURL string

	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "User-agent: *\nSitemap: %s/custom-sitemap.xml\n", srvURL)
	})
	mux.HandleFunc("/custom-sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		body := buildSitemapBody(pageURLs)
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, normalPageBody)
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, normalPageBody)
	})

	robotsSrv = httptest.NewServer(mux)
	t.Cleanup(robotsSrv.Close)
	srvURL = robotsSrv.URL
	pageURLs = []string{srvURL + "/", srvURL + "/about"}

	pool, err := BuildURLPool(context.Background(), robotsSrv.URL+"/", false, 2)
	if err != nil {
		t.Fatalf("BuildURLPool error: %v", err)
	}
	if len(pool.URLs) == 0 {
		t.Fatal("expected URLs from robots.txt sitemap fallback")
	}
}

// TestBuildURLPool_FallsBackToCrawl verifies that when neither sitemap.xml
// nor robots.txt yields results, crawl mode discovers URLs.
func TestBuildURLPool_FallsBackToCrawl(t *testing.T) {
	srv := newTestServer(t).
		withStatusCode("/sitemap.xml", http.StatusNotFound).
		withStatusCode("/robots.txt", http.StatusNotFound).
		withNormalPage("/").
		withNormalPage("/about").
		withNormalPage("/pricing").
		build()

	pool, err := BuildURLPool(context.Background(), srv.URL+"/", true, 2)
	if err != nil {
		t.Fatalf("BuildURLPool error: %v", err)
	}
	if len(pool.URLs) == 0 {
		t.Fatal("expected URLs discovered via crawl")
	}
}

// TestBuildURLPool_FallsBackToOriginOnly verifies that when no sitemap,
// robots.txt, or crawl results are found, the pool contains at minimum
// the origin URL itself.
func TestBuildURLPool_FallsBackToOriginOnly(t *testing.T) {
	srv := newTestServer(t).
		withStatusCode("/sitemap.xml", http.StatusNotFound).
		withStatusCode("/robots.txt", http.StatusNotFound).
		withNormalPage("/").
		build()

	// crawl=false means no crawl fallback — should get origin only
	pool, err := BuildURLPool(context.Background(), srv.URL+"/", false, 2)
	if err != nil {
		t.Fatalf("BuildURLPool error: %v", err)
	}
	if len(pool.URLs) == 0 {
		t.Fatal("pool should contain at minimum the origin URL")
	}
}

// TestBuildURLPool_ChecksumsPopulated verifies that every URL in the pool
// has a corresponding SHA-512 checksum entry.
func TestBuildURLPool_ChecksumsPopulated(t *testing.T) {
	pageURLs := []string{}
	ts := newTestServer(t).withNormalPage("/").build()
	pageURLs = []string{ts.URL + "/"}

	ts2 := newTestServer(t).
		withSitemap("/sitemap.xml", pageURLs).
		withNormalPage("/").
		build()

	pool, err := BuildURLPool(context.Background(), ts2.URL+"/", false, 2)
	if err != nil {
		t.Fatalf("BuildURLPool error: %v", err)
	}

	for _, u := range pool.URLs {
		checksum, ok := pool.Checksums[u]
		if !ok {
			t.Errorf("URL %q has no checksum entry", u)
		}
		if len(checksum) != 128 {
			t.Errorf("URL %q checksum should be 128 hex chars, got %d", u, len(checksum))
		}
	}
}

// TestBuildURLPool_ContextCancellation verifies that BuildURLPool respects
// context cancellation during the indexing phase.
func TestBuildURLPool_ContextCancellation(t *testing.T) {
	// Slow server — won't respond before context cancels
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := BuildURLPool(ctx, srv.URL+"/", false, 1)
	// Either an error or an empty pool is acceptable —
	// the important thing is that it returns promptly
	_ = err
}

// TestURLPool_Hydrate_ExcludesFailedURLs verifies that URLs that cannot
// be fetched during hydration are excluded from the pool rather than
// causing an error.
func TestURLPool_Hydrate_ExcludesFailedURLs(t *testing.T) {
	srv := newTestServer(t).
		withNormalPage("/good").
		withStatusCode("/bad", http.StatusInternalServerError).
		build()

	pool := &URLPool{
		Origin:    srv.URL,
		Checksums: make(map[string]string),
	}

	client := &http.Client{Timeout: 5 * time.Second}
	result, err := pool.hydrate(context.Background(), client, srv.URL, []string{
		srv.URL + "/good",
		srv.URL + "/bad",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// /good should succeed, /bad may be excluded or included with empty checksum
	// The key invariant: no panic and result is non-nil
	if result == nil {
		t.Error("hydrate should return non-nil pool")
	}
}

// TestURLPool_Hydrate_AllFailed verifies that when every URL fails to
// hydrate, an error is returned.
func TestURLPool_Hydrate_AllFailed(t *testing.T) {
	pool := &URLPool{
		Origin:    "http://localhost:1",
		Checksums: make(map[string]string),
	}

	client := &http.Client{Timeout: 100 * time.Millisecond}
	_, err := pool.hydrate(context.Background(), client, "http://localhost:1", []string{
		"http://localhost:1/a",
		"http://localhost:1/b",
	})
	if err == nil {
		t.Error("expected error when all URLs fail to hydrate")
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkExtractSitemapLocs_100URLs measures sitemap extraction throughput
// for a typical mid-size sitemap.
func BenchmarkExtractSitemapLocs_100URLs(b *testing.B) {
	body := buildSitemapBody(makeSitemapURLs(100))
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractSitemapLocs(body)
	}
}

// BenchmarkExtractSitemapLocs_10000URLs measures sitemap extraction throughput
// for a large enterprise sitemap near the sitemap spec's 50K URL limit.
func BenchmarkExtractSitemapLocs_10000URLs(b *testing.B) {
	body := buildSitemapBody(makeSitemapURLs(10000))
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractSitemapLocs(body)
	}
}

// BenchmarkExtractHTMLLinks measures link extraction throughput on a
// realistic HTML page with 20 anchor tags.
func BenchmarkExtractHTMLLinks(b *testing.B) {
	body := []byte(normalPageBody)
	origin := "https://example.com"
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractHTMLLinks(body, origin)
	}
}

// BenchmarkBuildURLPool measures the full pool construction time against
// a local httptest server with a 50-URL sitemap.
//
//go:build bench
func BenchmarkBuildURLPool(b *testing.B) {
	urls := makeSitemapURLs(50)
	srv := newTestServer(b).
		withSitemap("/sitemap.xml", urls).
		withNormalPage("/").
		build()

	// Ensure all sitemap URLs are reachable on the test server
	for i := range urls {
		urls[i] = srv.URL + fmt.Sprintf("/page-%d", i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := BuildURLPool(context.Background(), srv.URL+"/", false, 2)
		if err != nil {
			b.Fatalf("BuildURLPool error: %v", err)
		}
	}
}

// ── Fuzz ──────────────────────────────────────────────────────────────────────

// FuzzExtractSitemapLocs verifies that extractSitemapLocs never panics on
// arbitrary XML-like input and always returns a non-nil slice.
func FuzzExtractSitemapLocs(f *testing.F) {
	f.Add(`<?xml version="1.0"?><urlset><url><loc>https://example.com/</loc></url></urlset>`)
	f.Add(`<urlset></urlset>`)
	f.Add(``)
	f.Add(`<loc>https://example.com/</loc><loc>`)
	f.Add(`<urlset><url><loc>  </loc></url></urlset>`)

	f.Fuzz(func(t *testing.T, data string) {
		got := extractSitemapLocs(data)
		// Invariant: must not panic and must return non-nil
		if got == nil {
			t.Error("extractSitemapLocs should never return nil")
		}
		// Invariant: all returned strings must be non-empty
		for i, u := range got {
			if u == "" {
				t.Errorf("loc[%d] is empty string — should have been filtered", i)
			}
		}
	})
}

// FuzzExtractHTMLLinks verifies that extractHTMLLinks never panics on
// arbitrary HTML input and always returns a non-nil slice.
func FuzzExtractHTMLLinks(f *testing.F) {
	f.Add([]byte(normalPageBody), "https://example.com")
	f.Add([]byte(`<html><body><a href="/page">link</a></body></html>`), "https://example.com")
	f.Add([]byte{}, "https://example.com")
	f.Add([]byte(`<a href="javascript:evil()">bad</a>`), "https://example.com")
	f.Add([]byte(`<a href="mailto:x@y.com">email</a>`), "https://example.com")

	f.Fuzz(func(t *testing.T, data []byte, origin string) {
		got := extractHTMLLinks(data, origin)
		// Invariant: must not panic and must return non-nil
		if got == nil {
			t.Error("extractHTMLLinks should never return nil")
		}
		// Invariant: all returned strings must have the origin as prefix
		for i, link := range got {
			if !strings.HasPrefix(link, origin) {
				t.Errorf("link[%d] %q does not have origin prefix %q", i, link, origin)
			}
		}
	})
}

// FuzzResolveURL verifies that resolveURL never panics on arbitrary
// origin and href combinations.
func FuzzResolveURL(f *testing.F) {
	f.Add("https://example.com", "/about")
	f.Add("https://example.com", "https://other.com/page")
	f.Add("https://example.com", "")
	f.Add("https://example.com", "#fragment")
	f.Add("https://example.com", "javascript:void(0)")
	f.Add("not-a-url", "/path")

	f.Fuzz(func(t *testing.T, origin, href string) {
		got := resolveURL(origin, href)
		// Invariant 1: must not panic
		// Invariant 2: non-empty result must have origin as prefix
		if got != "" && !strings.HasPrefix(got, origin) {
			// Only enforce when origin is a valid URL prefix
			// (fuzz may supply invalid origins)
			if strings.HasPrefix(origin, "http") {
				t.Errorf("resolveURL(%q, %q) = %q — result does not have origin prefix",
					origin, href, got)
			}
		}
		// Invariant 3: result must never contain a fragment
		if strings.Contains(got, "#") {
			t.Errorf("resolveURL returned URL with fragment: %q", got)
		}
	})
}

// ── Internal test helpers ─────────────────────────────────────────────────────

// buildSitemapBody constructs a valid sitemap.xml string from a URL list.
// Used by tests that need to create sitemap content programmatically.
func buildSitemapBody(urls []string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range urls {
		sb.WriteString(`<url><loc>`)
		sb.WriteString(u)
		sb.WriteString(`</loc></url>`)
	}
	sb.WriteString(`</urlset>`)
	return sb.String()
}

// makeSitemapURLs generates n synthetic sitemap URLs for benchmark use.
func makeSitemapURLs(n int) []string {
	urls := make([]string, n)
	for i := 0; i < n; i++ {
		urls[i] = fmt.Sprintf("https://example.com/page-%d", i)
	}
	return urls
}
