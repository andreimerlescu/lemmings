package discovery

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

const (
	indexTimeout      = 10 * time.Second
	indexBudget       = 10 * time.Minute
	indexConcurrency  = 5
	indexMaxRedirects = 10
	indexMaxURLs      = 10_000
	indexMaxBodyBytes = 4 << 20
	sitemapPath       = "/sitemap.xml"
	robotsPath        = "/robots.txt"
	indexUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// URLPool is the shared read-only navigation surface built once at boot.
// All lemmings read from it concurrently; nothing writes after BuildURLPool returns.
type URLPool struct {
	Origin    string
	URLs      []string
	Checksums map[string]string // url -> SHA-512 hex of body at index time
}

// BuildOptions configures target URL discovery.
type BuildOptions struct {
	Crawl      bool
	CrawlDepth int
	// OnProgress receives optional status messages (replaces direct stdout writes).
	OnProgress func(msg string)
}

func (o BuildOptions) progress(format string, args ...any) {
	if o.OnProgress != nil {
		o.OnProgress(fmt.Sprintf(format, args...))
	}
}

func newIndexClient(hit string) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          indexConcurrency * 2,
		MaxIdleConnsPerHost:   indexConcurrency,
		MaxConnsPerHost:       indexConcurrency,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: indexTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   indexTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= indexMaxRedirects || !SameOrigin(hit, req.URL.String()) {
				return fmt.Errorf("index redirect outside scope or redirect limit exceeded")
			}
			return nil
		},
	}
}

// BuildURLPool constructs the URLPool using a four-step waterfall:
//  1. /sitemap.xml at origin
//  2. Sitemap: directive in /robots.txt (same-origin only)
//  3. Bounded depth-first crawl (when opts.Crawl is true)
//  4. Origin URL alone (always succeeds if reachable)
func BuildURLPool(ctx context.Context, hit string, opts BuildOptions) (*URLPool, error) {
	origin, err := ParseOrigin(hit)
	if err != nil {
		return nil, fmt.Errorf("invalid hit URL %q: %w", hit, err)
	}

	ctx, cancel := context.WithTimeout(ctx, indexBudget)
	defer cancel()

	client := newIndexClient(hit)
	defer client.CloseIdleConnections()

	pool := &URLPool{
		Origin:    origin,
		Checksums: make(map[string]string),
	}

	// Strategy 1: sitemap.xml at standard path.
	urls, err := IndexSitemap(ctx, client, origin+sitemapPath)
	if err == nil && len(urls) > 0 {
		opts.progress("sitemap found: %d URLs", len(urls))
		return pool.hydrate(ctx, client, origin, urls, opts)
	}

	// Strategy 2: sitemap declared in robots.txt.
	robotsSitemapURL, err := FindSitemapInRobots(ctx, client, origin+robotsPath)
	if err == nil && robotsSitemapURL != "" && SameOrigin(origin, robotsSitemapURL) {
		urls, err = IndexSitemap(ctx, client, robotsSitemapURL)
		if err == nil && len(urls) > 0 {
			opts.progress("sitemap found via robots.txt: %d URLs", len(urls))
			return pool.hydrate(ctx, client, origin, urls, opts)
		}
	}

	// Strategy 3: crawl.
	if opts.Crawl {
		opts.progress("no sitemap found — crawling origin...")
		urls, err = CrawlOrigin(ctx, client, origin, hit, opts.CrawlDepth)
		if err == nil && len(urls) > 0 {
			opts.progress("crawl found: %d URLs", len(urls))
			return pool.hydrate(ctx, client, origin, urls, opts)
		}
		if err != nil {
			opts.progress("crawl error: %v", err)
		}
	}

	// Strategy 4: origin only.
	opts.progress("using origin URL only — no sitemap or crawl results")
	return pool.hydrate(ctx, client, origin, []string{hit}, opts)
}

// hydrate fetches each URL with a fixed worker pool (bounded goroutines per R3),
// computes baseline SHA-512 checksums, and populates the pool.
func (p *URLPool) hydrate(
	ctx context.Context,
	client *http.Client,
	origin string,
	urls []string,
	opts BuildOptions,
) (*URLPool, error) {
	scoped := ScopeToOrigin(origin, urls)
	if len(scoped) > indexMaxURLs {
		scoped = scoped[:indexMaxURLs]
	}
	if len(scoped) == 0 {
		scoped = []string{origin + "/"}
	}

	type result struct {
		url      string
		checksum string
	}

	workers := min(indexConcurrency, len(scoped))
	jobs := make(chan string, workers)
	results := make(chan result, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				body, _, _, err := FetchURL(ctx, client, u)
				if err != nil {
					opts.progress("skipping %s: %v", SafeURL(u), err)
					continue
				}
				select {
				case results <- result{url: u, checksum: SHA512Hex(body)}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Feed jobs on a dedicated goroutine so producer/consumer never deadlock.
	var feedWg sync.WaitGroup
	feedWg.Add(1)
	go func() {
		defer feedWg.Done()
		defer close(jobs)
		for _, u := range scoped {
			select {
			case jobs <- u:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		feedWg.Wait()
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

// IndexSitemap fetches and parses a sitemap XML document (including nested
// sitemap indexes, bounded against cycles and off-origin links).
func IndexSitemap(ctx context.Context, client *http.Client, sitemapURL string) ([]string, error) {
	seen := map[string]bool{}
	remaining := indexMaxURLs
	var walk func(string, int) ([]string, error)
	walk = func(u string, depth int) ([]string, error) {
		if depth > 8 || len(seen) >= 100 || seen[u] || !SameOrigin(sitemapURL, u) {
			return nil, nil
		}
		seen[u] = true
		body, status, _, err := FetchURL(ctx, client, u)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("sitemap returned %d", status)
		}
		urls := ExtractSitemapLocs(string(body))
		if IsSitemapIndex(string(body)) {
			var all []string
			for _, v := range urls {
				if remaining <= 0 {
					break
				}
				nested, err := walk(ResolveURL(u, v), depth+1)
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

// ExtractSitemapLocs extracts up to indexMaxURLs <loc> values from sitemap XML.
func ExtractSitemapLocs(body string) []string {
	out := []string{}
	d := xml.NewDecoder(strings.NewReader(body))
	for len(out) < indexMaxURLs {
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

// IsSitemapIndex reports whether body looks like a sitemap index document.
func IsSitemapIndex(body string) bool {
	return strings.Contains(body, "<sitemapindex")
}

// FindSitemapInRobots fetches robots.txt and returns the first Sitemap: URL.
func FindSitemapInRobots(ctx context.Context, client *http.Client, robotsURL string) (string, error) {
	body, statusCode, _, err := FetchURL(ctx, client, robotsURL)
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

// CrawlOrigin performs a bounded depth-first crawl from start up to maxDepth
// levels deep and at most 5 minutes / indexMaxURLs pages.
func CrawlOrigin(
	ctx context.Context,
	client *http.Client,
	origin string,
	start string,
	maxDepth int,
) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	visited := make(map[string]bool)
	var results []string

	var crawl func(u string, depth int)
	crawl = func(u string, depth int) {
		if depth > maxDepth || len(visited) >= indexMaxURLs || visited[u] {
			return
		}
		if ctx.Err() != nil {
			return
		}
		visited[u] = true

		body, statusCode, _, err := FetchURL(ctx, client, u)
		if err != nil || statusCode != http.StatusOK {
			return
		}
		results = append(results, u)

		for _, link := range ExtractHTMLLinks(body, u) {
			crawl(link, depth+1)
		}
	}

	crawl(start, 0)
	if len(results) == 0 {
		return nil, fmt.Errorf("crawl found no reachable URLs from %s", SafeURL(start))
	}
	return results, nil
}

// ExtractHTMLLinks parses HTML and returns all same-origin anchor hrefs resolved against base.
func ExtractHTMLLinks(body []byte, base string) []string {
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
					resolved := ResolveURL(base, attr.Val)
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

// ScopeToOrigin filters urls to those matching origin and deduplicates them.
func ScopeToOrigin(origin string, urls []string) []string {
	seen := make(map[string]bool)
	scoped := make([]string, 0)
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if !SameOrigin(u, origin) || seen[u] {
			continue
		}
		seen[u] = true
		scoped = append(scoped, u)
	}
	return scoped
}

// FetchURL performs a bounded GET request during pool indexing.
func FetchURL(ctx context.Context, client *http.Client, u string) ([]byte, int, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", indexUserAgent)
	req.Header.Set("Accept", "text/html,application/xml,application/xhtml+xml,*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close() // read side

	body, err := io.ReadAll(io.LimitReader(resp.Body, indexMaxBodyBytes+1))
	if len(body) > indexMaxBodyBytes {
		return nil, resp.StatusCode, int64(len(body)), fmt.Errorf("index body exceeds 4 MiB")
	}
	if err != nil {
		return nil, resp.StatusCode, 0, fmt.Errorf("read body: %w", err)
	}
	return body, resp.StatusCode, int64(len(body)), nil
}
