package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

const (
	// indexTimeout is the maximum time allowed for a single URL
	// to be fetched during pool indexing.
	indexTimeout = 10 * time.Second

	// indexBudget bounds the whole indexing phase: sitemap discovery, the
	// crawl (itself capped at 5 minutes) and checksumming every URL.
	indexBudget = 10 * time.Minute

	// indexConcurrency is how many URLs we fetch in parallel
	// during pool construction. Kept conservative — we don't want
	// to load test the target during the indexing phase.
	indexConcurrency = 5

	// sitemapPath is the conventional sitemap location.
	sitemapPath = "/sitemap.xml"

	// robotsPath is checked to find a non-standard sitemap declaration.
	robotsPath = "/robots.txt"
)

// URLPool is the shared read-only navigation surface built once at boot.
// All lemmings read from it concurrently — nothing writes after construction.
type URLPool struct {
	Origin    string
	URLs      []string
	Checksums map[string]string // url -> SHA512 of body at index time
}

// BuildURLPool constructs the URL pool for the swarm.
// Strategy order:
//  1. sitemap.xml at origin (standard path)
//  2. sitemap declared in robots.txt (non-standard path)
//  3. crawl from origin (if -crawl=true)
//  4. origin URL alone (last resort — always works)
func BuildURLPool(
	ctx context.Context,
	hit string,
	crawl bool,
	crawlDepth int,
) (*URLPool, error) {

	origin, err := parseOrigin(hit)
	if err != nil {
		return nil, fmt.Errorf("invalid hit URL %q: %w", hit, err)
	}

	ctx, cancel := context.WithTimeout(ctx, indexBudget)
	defer cancel()
	client := &http.Client{
		Timeout: indexTimeout,
		// Indexing never follows a redirect off the target's origin.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects || !sameOrigin(hit, req.URL.String()) {
				return fmt.Errorf("index redirect outside scope or redirect limit exceeded")
			}
			return nil
		},
	}

	pool := &URLPool{
		Origin:    origin,
		Checksums: make(map[string]string),
	}

	// ── Strategy 1: sitemap.xml at standard path ────────────
	urls, err := indexSitemap(ctx, client, origin+sitemapPath)
	if err == nil && len(urls) > 0 {
		fmt.Printf("  sitemap found: %d URLs\n", len(urls))
		return pool.hydrate(ctx, client, origin, urls)
	}

	// ── Strategy 2: sitemap declared in robots.txt ──────────
	robotsSitemapURL, err := findSitemapInRobots(ctx, client, origin+robotsPath)
	if err == nil && robotsSitemapURL != "" && sameOrigin(origin, robotsSitemapURL) {
		urls, err = indexSitemap(ctx, client, robotsSitemapURL)
		if err == nil && len(urls) > 0 {
			fmt.Printf("  sitemap found via robots.txt: %d URLs\n", len(urls))
			return pool.hydrate(ctx, client, origin, urls)
		}
	}

	// ── Strategy 3: crawl ───────────────────────────────────
	if crawl {
		fmt.Println("  no sitemap found — crawling origin...")
		urls, err = crawlOrigin(ctx, client, origin, hit, crawlDepth)
		if err == nil && len(urls) > 0 {
			fmt.Printf("  crawl found: %d URLs\n", len(urls))
			return pool.hydrate(ctx, client, origin, urls)
		}
		if err != nil {
			fmt.Printf("  crawl error: %v\n", err)
		}
	}

	// ── Strategy 4: origin only ─────────────────────────────
	fmt.Println("  using origin URL only — no sitemap or crawl results")
	return pool.hydrate(ctx, client, origin, []string{hit})
}

// hydrate fetches each URL concurrently, checksums the body, and populates
// the pool. URLs that fail to fetch are excluded from the pool with a warning.
// Origin-scoping is enforced here — off-origin URLs are silently dropped.
func (p *URLPool) hydrate(
	ctx context.Context,
	client *http.Client,
	origin string,
	urls []string,
) (*URLPool, error) {

	// Deduplicate and scope to origin
	scoped := scopeToOrigin(origin, urls)
	if len(scoped) > 10000 {
		scoped = scoped[:10000]
	}
	if len(scoped) == 0 {
		// Nothing passed origin scoping — fall back to origin root
		scoped = []string{origin + "/"}
	}

	type result struct {
		url      string
		checksum string
	}

	results := make(chan result, len(scoped))
	sem := make(chan struct{}, indexConcurrency)
	var wg sync.WaitGroup

	for _, u := range scoped {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()

			// Acquire indexing concurrency slot
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			body, _, _, err := fetchURL(ctx, client, u)
			if err != nil {
				fmt.Printf("  skipping %s: %v\n", u, err)
				return
			}

			results <- result{
				url:      u,
				checksum: sha512sum(body),
			}
		}(u)
	}

	// Close results once all goroutines finish
	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		p.URLs = append(p.URLs, r.url)
		p.Checksums[r.url] = r.checksum
	}

	if len(p.URLs) == 0 {
		return nil, fmt.Errorf("pool is empty — no URLs could be fetched from %s", origin)
	}

	sort.Strings(p.URLs)
	return p, nil
}

// indexSitemap fetches and parses a sitemap.xml, returning all loc URLs.
// Handles both standard sitemaps and sitemap index files (nested sitemaps).
func indexSitemap(ctx context.Context, client *http.Client, sitemapURL string) ([]string, error) {
	seen := map[string]bool{}
	remaining := 10000
	var walk func(string, int) ([]string, error)
	walk = func(u string, depth int) ([]string, error) {
		if depth > 8 || len(seen) >= 100 || seen[u] || !sameOrigin(sitemapURL, u) {
			return nil, nil
		}
		seen[u] = true
		body, status, _, err := fetchURL(ctx, client, u)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("sitemap returned %d", status)
		}
		urls := extractSitemapLocs(string(body))
		if isSitemapIndex(string(body)) {
			var all []string
			for _, v := range urls {
				if remaining <= 0 {
					break
				}
				nested, err := walk(resolveURL(u, v), depth+1)
				if err == nil {
					all = append(all, nested...)
				}
			}
			return all, nil
		}
		if len(urls) > remaining {
			urls = urls[:remaining]
		}
		remaining -= len(urls)
		return urls, nil
	}
	return walk(sitemapURL, 0)
}

// extractSitemapLocs pulls all <loc> values from a sitemap XML body.
// We parse manually rather than using encoding/xml to avoid struct
// coupling to specific sitemap schema versions.
func extractSitemapLocs(body string) []string {
	out := []string{}
	d := xml.NewDecoder(strings.NewReader(body))
	for len(out) < 10000 {
		t, err := d.Token()
		if err != nil {
			break
		}
		if start, ok := t.(xml.StartElement); ok && start.Name.Local == "loc" {
			var value string
			if err := d.DecodeElement(&value, &start); err != nil {
				break
			}
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, value)
			}
		}
	}
	return out
}

// isSitemapIndex returns true if the body contains a <sitemapindex> element,
// indicating this is a sitemap of sitemaps rather than a page list.
func isSitemapIndex(body string) bool {
	return strings.Contains(body, "<sitemapindex")
}

// findSitemapInRobots fetches robots.txt and looks for a Sitemap: directive.
func findSitemapInRobots(ctx context.Context, client *http.Client, robotsURL string) (string, error) {
	body, statusCode, _, err := fetchURL(ctx, client, robotsURL)
	if err != nil {
		return "", err
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("robots.txt returned %d", statusCode)
	}

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "sitemap:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				candidate := strings.TrimSpace(parts[1])
				// Handle the case where the URL includes the scheme
				// and was split on its colon
				if strings.HasPrefix(candidate, "//") {
					candidate = "https:" + candidate
				}
				if candidate != "" {
					return candidate, nil
				}
			}
		}
	}

	return "", fmt.Errorf("no sitemap directive found in robots.txt")
}

// crawlOrigin performs a bounded depth-first crawl from the start URL,
// collecting all reachable origin-scoped links up to maxDepth levels deep.
// Crawl runs for at most 5 minutes regardless of depth or link count.
func crawlOrigin(
	ctx context.Context,
	client *http.Client,
	origin string,
	start string,
	maxDepth int,
) ([]string, error) {

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	visited := make(map[string]bool)
	var mu sync.Mutex
	var results []string

	var crawl func(url string, depth int)
	crawl = func(u string, depth int) {
		if depth > maxDepth || len(visited) >= 10000 {
			return
		}

		mu.Lock()
		if visited[u] {
			mu.Unlock()
			return
		}
		visited[u] = true
		mu.Unlock()

		select {
		case <-ctx.Done():
			return
		default:
		}

		body, statusCode, _, err := fetchURL(ctx, client, u)
		if err != nil || statusCode != http.StatusOK {
			return
		}

		mu.Lock()
		results = append(results, u)
		mu.Unlock()

		links := extractHTMLLinks(body, u)
		for _, link := range links {
			crawl(link, depth+1)
		}
	}

	crawl(start, 0)

	if len(results) == 0 {
		return nil, fmt.Errorf("crawl found no reachable URLs from %s", start)
	}

	return results, nil
}

// extractHTMLLinks parses an HTML body and returns all href values that
// are scoped to origin. Relative paths are resolved to absolute URLs.
func extractHTMLLinks(body []byte, origin string) []string {
	links := []string{}
	seen := make(map[string]bool)

	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return links
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					resolved := resolveURL(origin, attr.Val)
					if resolved != "" && !seen[resolved] {
						seen[resolved] = true
						links = append(links, resolved)
					}
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return links
}

// scopeToOrigin filters a URL list to only those belonging to origin,
// deduplicates them, and normalises trailing slashes.
func scopeToOrigin(origin string, urls []string) []string {
	seen := make(map[string]bool)
	scoped := make([]string, 0) // non-nil empty slice

	for _, u := range urls {
		u = strings.TrimSpace(u)
		if !sameOrigin(u, origin) {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		scoped = append(scoped, u)
	}

	return scoped
}

// resolveURL resolves an href against base — an origin or the URL of the
// page the href appeared on. Returns "" if the result is off-origin,
// unparseable, or a link a lemming never follows (fragment, mailto:,
// javascript:, tel:).
func resolveURL(origin, href string) string {
	href = strings.TrimSpace(href)

	// Skip fragments, mailto, javascript, and empty hrefs
	if href == "" ||
		strings.HasPrefix(href, "#") ||
		strings.HasPrefix(href, "mailto:") ||
		strings.HasPrefix(href, "javascript:") ||
		strings.HasPrefix(href, "tel:") {
		return ""
	}

	base, err := url.Parse(origin)
	if err != nil {
		return ""
	}

	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}

	resolved := base.ResolveReference(ref)

	// Strip fragment — lemmings don't care about anchors
	resolved.Fragment = ""

	// Enforce origin scope — no subdomains, no external domains. The
	// prefix check also rejects hosts that only match once re-escaped.
	out := resolved.String()
	if !sameOrigin(out, base.String()) || !strings.HasPrefix(out, base.Scheme+"://"+base.Host) {
		return ""
	}

	return out
}

// parseOrigin extracts the scheme+host from a URL string,
// stripping any path, query, or fragment.
func parseOrigin(hit string) (string, error) {
	u, err := url.Parse(hit)
	if err != nil {
		return "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("URL must include scheme and host")
	}
	return u.Scheme + "://" + u.Host, nil
}

// fetchURL performs a GET request and returns body, status, bytes, error.
// Shared by both the pool builder and individual lemmings during indexing.
func fetchURL(ctx context.Context, client *http.Client, u string) ([]byte, int, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("User-Agent", userAgents[0]) // fixed UA for indexing
	req.Header.Set("Accept", "text/html,application/xml,application/xhtml+xml,*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if len(body) > 4<<20 {
		return nil, resp.StatusCode, int64(len(body)), fmt.Errorf("index body exceeds 4 MiB")
	}
	if err != nil {
		return nil, resp.StatusCode, 0, fmt.Errorf("read body: %w", err)
	}

	return body, resp.StatusCode, int64(len(body)), nil
}

// sameOrigin compares parsed origins, including effective ports. Prefix checks
// incorrectly allow example.com.evil.test and leak redirected traffic off-target.
func sameOrigin(a, b string) bool {
	x, err := url.Parse(a)
	if err != nil {
		return false
	}
	y, err := url.Parse(b)
	if err != nil {
		return false
	}
	if x.User != nil || y.User != nil || (x.Scheme != "http" && x.Scheme != "https") || x.Scheme != y.Scheme || x.Hostname() == "" {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(x.Hostname(), y.Hostname()) && port(x) == port(y)
}
