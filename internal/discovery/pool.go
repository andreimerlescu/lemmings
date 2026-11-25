package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
)

const (
	indexTimeout            = 10 * time.Second
	indexBudget             = 10 * time.Minute
	indexConcurrency        = 8
	indexMaxRedirects       = 10
	indexMaxURLs            = 10_000
	indexMaxBodyBytes       = 4 << 20
	maxWaitingRoomJSONBytes = 64 << 10
	sitemapPath             = "/sitemap.xml"
	robotsPath              = "/robots.txt"
	indexUserAgent          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
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
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           SafeDialContext(&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}),
		TLSClientConfig:       MinTLSConfig(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          indexConcurrency * 2,
		MaxIdleConnsPerHost:   indexConcurrency,
		MaxConnsPerHost:       indexConcurrency,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: indexTimeout,
		ExpectContinueTimeout: time.Second,
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout:   indexTimeout,
		Transport: tr,
		Jar:       jar,
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

	isConcert := false
	isWaitingRoom := false

	// Strategy 1: sitemap.xml at standard path.
	urls, sitemapConcert, sitemapWR, err := indexSitemap(ctx, client, origin+sitemapPath)
	isConcert = isConcert || sitemapConcert
	isWaitingRoom = isWaitingRoom || sitemapWR
	if err == nil && len(urls) > 0 {
		opts.progress("sitemap found: %d URLs", len(urls))
		return pool.hydrate(ctx, client, origin, urls, isConcert, isWaitingRoom, opts)
	}

	// Strategy 2: sitemap declared in robots.txt.
	if !isWaitingRoom {
		robotsSitemapURL, robotsConcert, robotsWR, err := findSitemapInRobots(ctx, client, origin+robotsPath)
		isConcert = isConcert || robotsConcert
		isWaitingRoom = isWaitingRoom || robotsWR
		if !isWaitingRoom && err == nil && robotsSitemapURL != "" && SameOrigin(origin, robotsSitemapURL) {
			urls, subConcert, subWR, err := indexSitemap(ctx, client, robotsSitemapURL)
			isConcert = isConcert || subConcert
			isWaitingRoom = isWaitingRoom || subWR
			if err == nil && len(urls) > 0 {
				opts.progress("sitemap found via robots.txt: %d URLs", len(urls))
				return pool.hydrate(ctx, client, origin, urls, isConcert, isWaitingRoom, opts)
			}
		}
	}

	// Strategy 3: crawl.
	if !isWaitingRoom && opts.Crawl {
		opts.progress("no sitemap found — crawling origin...")
		urls, crawlConcert, crawlWR, err := crawlOrigin(ctx, client, origin, hit, opts.CrawlDepth)
		isConcert = isConcert || crawlConcert
		isWaitingRoom = isWaitingRoom || crawlWR
		if err == nil && len(urls) > 0 {
			opts.progress("crawl found: %d URLs", len(urls))
			return pool.hydrate(ctx, client, origin, urls, isConcert, isWaitingRoom, opts)
		}
		if err != nil {
			opts.progress("crawl error: %v", err)
		}
	}

	// Strategy 4: origin only.
	opts.progress("using origin URL only — no sitemap or crawl results")
	return pool.hydrate(ctx, client, origin, []string{hit}, isConcert, isWaitingRoom, opts)
}

// hydrate fetches each URL, computes baseline SHA-512 checksums, and populates
// the pool. It probes scoped[0] sequentially first to detect Concert or waiting
// rooms: Concert targets drop to sequential (1-worker) hydration so boot
// indexing never saturates Concert's FIFO queue, and waiting-room responses
// halt further checksum fetches immediately without storing waiting-room hashes.
func (p *URLPool) hydrate(
	ctx context.Context,
	client *http.Client,
	origin string,
	urls []string,
	isConcert bool,
	isWaitingRoom bool,
	opts BuildOptions,
) (*URLPool, error) {
	scoped := ScopeToOrigin(origin, urls)
	if len(scoped) > indexMaxURLs {
		scoped = scoped[:indexMaxURLs]
	}
	if len(scoped) == 0 {
		scoped = []string{origin + "/"}
	}

	// If discovery already encountered a waiting room, preserve all discovered
	// URLs with empty expected checksums and avoid sending further requests.
	if isWaitingRoom {
		p.URLs = append(p.URLs, scoped...)
		return p.finalize(origin)
	}

	// Always probe scoped[0] sequentially before fanning out workers so Concert
	// or waiting-room presence is detected even on targets without sitemap.xml.
	first := scoped[0]
	firstRes, err := fetchIndexURL(ctx, client, first)
	if firstRes.isConcert {
		isConcert = true
	}
	if firstRes.isWaitingRoom {
		p.URLs = append(p.URLs, scoped...)
		opts.progress("waiting room detected while indexing %s — preserving %d URLs without baseline checksums", SafeURL(first), len(scoped))
		return p.finalize(origin)
	}
	if err != nil {
		opts.progress("skipping %s: %v", SafeURL(first), err)
	} else {
		p.URLs = append(p.URLs, first)
		p.Checksums[first] = SHA512Hex(firstRes.body)
	}

	remaining := scoped[1:]
	if len(remaining) == 0 {
		return p.finalize(origin)
	}

	// Concert detected: hydrate sequentially (1 worker) so boot indexing holds
	// at most 1 slot at a time and never overflows Concert's -cap >= 1.
	if isConcert {
		for i, u := range remaining {
			if ctx.Err() != nil {
				break
			}
			res, err := fetchIndexURL(ctx, client, u)
			if res.isWaitingRoom {
				p.URLs = append(p.URLs, remaining[i:]...)
				opts.progress("waiting room detected while indexing %s — preserving %d remaining URLs without baseline checksums", SafeURL(u), len(remaining)-i)
				break
			}
			if err != nil {
				opts.progress("skipping %s: %v", SafeURL(u), err)
				continue
			}
			p.URLs = append(p.URLs, u)
			p.Checksums[u] = SHA512Hex(res.body)
		}
		return p.finalize(origin)
	}

	// Ordinary non-Concert target: hydrate remaining URLs in parallel (up to
	// indexConcurrency workers), aborting cleanly if any worker hits a waiting room.
	hydrateCtx, cancelHydrate := context.WithCancel(ctx)
	defer cancelHydrate()

	type result struct {
		url         string
		checksum    string
		waitingRoom bool
		skipped     bool
	}

	var waitingRoomHit atomic.Bool
	workers := min(indexConcurrency, len(remaining))
	jobs := make(chan string, workers)
	results := make(chan result, workers)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				if waitingRoomHit.Load() {
					return
				}
				res, err := fetchIndexURL(hydrateCtx, client, u)
				if res.isWaitingRoom {
					waitingRoomHit.Store(true)
					cancelHydrate()
					select {
					case results <- result{url: u, waitingRoom: true}:
					case <-ctx.Done():
					}
					return
				}
				if err != nil {
					if waitingRoomHit.Load() {
						return
					}
					opts.progress("skipping %s: %v", SafeURL(u), err)
					select {
					case results <- result{url: u, skipped: true}:
					case <-ctx.Done():
					}
					continue
				}
				select {
				case results <- result{url: u, checksum: SHA512Hex(res.body)}:
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
		for _, u := range remaining {
			select {
			case jobs <- u:
			case <-hydrateCtx.Done():
				return
			}
		}
	}()

	go func() {
		feedWg.Wait()
		wg.Wait()
		close(results)
	}()

	seenDone := make(map[string]bool, len(remaining))
	skipped := make(map[string]bool)
	for r := range results {
		if r.waitingRoom {
			continue
		}
		if r.skipped {
			skipped[r.url] = true
			continue
		}
		seenDone[r.url] = true
		p.URLs = append(p.URLs, r.url)
		p.Checksums[r.url] = r.checksum
	}

	if waitingRoomHit.Load() {
		for _, u := range remaining {
			if !seenDone[u] && !skipped[u] {
				p.URLs = append(p.URLs, u)
			}
		}
	}

	return p.finalize(origin)
}

func (p *URLPool) finalize(origin string) (*URLPool, error) {
	if len(p.URLs) == 0 {
		return nil, fmt.Errorf("pool is empty — no URLs could be fetched from %s", origin)
	}
	sort.Strings(p.URLs)
	return p, nil
}

// IndexSitemap fetches and parses a sitemap XML document (including nested
// sitemap indexes, bounded against cycles and off-origin links).
func IndexSitemap(ctx context.Context, client *http.Client, sitemapURL string) ([]string, error) {
	urls, _, _, err := indexSitemap(ctx, client, sitemapURL)
	return urls, err
}

func indexSitemap(ctx context.Context, client *http.Client, sitemapURL string) ([]string, bool, bool, error) {
	seen := map[string]bool{}
	remaining := indexMaxURLs
	isConcert := false
	isWaitingRoom := false

	var walk func(string, int) ([]string, error)
	walk = func(u string, depth int) ([]string, error) {
		if isWaitingRoom || depth > 8 || len(seen) >= 100 || seen[u] || !SameOrigin(sitemapURL, u) {
			return nil, nil
		}
		seen[u] = true
		res, err := fetchIndexURL(ctx, client, u)
		if res.isConcert {
			isConcert = true
		}
		if res.isWaitingRoom {
			isWaitingRoom = true
			isConcert = true
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if res.status != http.StatusOK {
			return nil, fmt.Errorf("sitemap returned %d", res.status)
		}
		urls := ExtractSitemapLocs(string(res.body))
		if IsSitemapIndex(string(res.body)) {
			var all []string
			for _, v := range urls {
				if remaining <= 0 || isWaitingRoom {
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

	urls, err := walk(sitemapURL, 0)
	return urls, isConcert, isWaitingRoom, err
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
	sitemapURL, _, _, err := findSitemapInRobots(ctx, client, robotsURL)
	return sitemapURL, err
}

func findSitemapInRobots(ctx context.Context, client *http.Client, robotsURL string) (string, bool, bool, error) {
	res, err := fetchIndexURL(ctx, client, robotsURL)
	isConcert := res.isConcert
	if res.isWaitingRoom {
		return "", true, true, nil
	}
	if err != nil {
		return "", isConcert, false, err
	}
	if res.status != http.StatusOK {
		return "", isConcert, false, fmt.Errorf("robots.txt returned %d", res.status)
	}
	for _, line := range strings.Split(string(res.body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "sitemap:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				candidate := strings.TrimSpace(parts[1])
				if strings.HasPrefix(candidate, "//") {
					candidate = "https:" + candidate
				}
				if candidate != "" {
					return candidate, isConcert, false, nil
				}
			}
		}
	}
	return "", isConcert, false, fmt.Errorf("no sitemap directive found in robots.txt")
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
	urls, _, _, err := crawlOrigin(ctx, client, origin, start, maxDepth)
	return urls, err
}

func crawlOrigin(
	ctx context.Context,
	client *http.Client,
	origin string,
	start string,
	maxDepth int,
) ([]string, bool, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	visited := make(map[string]bool)
	var results []string
	isConcert := false
	isWaitingRoom := false

	var crawl func(u string, depth int)
	crawl = func(u string, depth int) {
		if isWaitingRoom || depth > maxDepth || len(visited) >= indexMaxURLs || visited[u] || isConcertInternalURL(u) {
			return
		}
		if ctx.Err() != nil {
			return
		}
		visited[u] = true

		res, err := fetchIndexURL(ctx, client, u)
		if res.isConcert {
			isConcert = true
		}
		if res.isWaitingRoom {
			isWaitingRoom = true
			isConcert = true
			results = append(results, u)
			return
		}
		if err != nil || res.status != http.StatusOK {
			return
		}
		results = append(results, u)

		for _, link := range ExtractHTMLLinks(res.body, u) {
			if isWaitingRoom {
				return
			}
			if isConcertInternalURL(link) {
				continue
			}
			crawl(link, depth+1)
		}
	}

	crawl(start, 0)
	if len(results) == 0 {
		return nil, isConcert, isWaitingRoom, fmt.Errorf("crawl found no reachable URLs from %s", SafeURL(start))
	}
	return results, isConcert, isWaitingRoom, nil
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

// ScopeToOrigin filters urls to those matching origin, excludes internal
// Concert /_concert/ paths, and deduplicates them.
func ScopeToOrigin(origin string, urls []string) []string {
	seen := make(map[string]bool)
	scoped := make([]string, 0)
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if !SameOrigin(u, origin) || isConcertInternalURL(u) || seen[u] {
			continue
		}
		seen[u] = true
		scoped = append(scoped, u)
	}
	return scoped
}

func isConcertInternalURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := u.Path
	if p == "/_concert" || strings.HasPrefix(p, "/_concert/") {
		return true
	}
	cleaned := path.Clean("/" + p)
	return cleaned == "/_concert" || strings.HasPrefix(cleaned, "/_concert/")
}

// inspectIndexResponse inspects an HTTP index response for Concert and waiting-room
// indicators:
//   - Set-Cookie headers containing "concert_admit=" or "room_ticket=" imply isConcert=true.
//   - Concert waiting-room HTML (/_concert/waiting.js or class="queue-page" with id="position"),
//     legacy waiting-room HTML (/queue/status with id="position"), or HTTP 429 JSON with
//     {"queued": true} imply both isConcert=true and isWaitingRoom=true.
func inspectIndexResponse(status int, headers http.Header, body []byte) (isConcert, isWaitingRoom bool) {
	if headers != nil {
		for _, sc := range headers.Values("Set-Cookie") {
			if strings.Contains(sc, "concert_admit=") || strings.Contains(sc, "room_ticket=") {
				isConcert = true
				break
			}
		}
	}

	if len(body) == 0 {
		return isConcert, false
	}

	if status == http.StatusTooManyRequests {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && len(trimmed) <= maxWaitingRoomJSONBytes && trimmed[0] == '{' {
			var payload struct {
				Queued bool `json:"queued"`
			}
			if err := json.Unmarshal(trimmed, &payload); err == nil && payload.Queued {
				return true, true
			}
		}
	}

	if status == 0 || (status >= 200 && status < 300) || status == http.StatusTooManyRequests {
		s := string(body)
		hasPosition := strings.Contains(s, `id="position"`) || strings.Contains(s, `id='position'`)
		if hasPosition && (strings.Contains(s, `/_concert/waiting.js`) ||
			strings.Contains(s, `class="queue-page"`) ||
			strings.Contains(s, `class='queue-page'`) ||
			strings.Contains(s, `/queue/status`)) {
			return true, true
		}
	}

	return isConcert, false
}

type indexResponse struct {
	body          []byte
	status        int
	bytes         int64
	headers       http.Header
	isConcert     bool
	isWaitingRoom bool
}

func fetchIndexURL(ctx context.Context, client *http.Client, u string) (indexResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return indexResponse{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", indexUserAgent)
	req.Header.Set("Accept", "text/html,application/xml,application/xhtml+xml,*/*")

	resp, err := client.Do(req)
	if err != nil {
		return indexResponse{}, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close() // read side

	body, err := io.ReadAll(io.LimitReader(resp.Body, indexMaxBodyBytes+1))
	if len(body) > indexMaxBodyBytes {
		isConcert, _ := inspectIndexResponse(resp.StatusCode, resp.Header, nil)
		return indexResponse{
			status:    resp.StatusCode,
			bytes:     int64(len(body)),
			headers:   resp.Header,
			isConcert: isConcert,
		}, fmt.Errorf("index body exceeds 4 MiB")
	}
	if err != nil {
		isConcert, _ := inspectIndexResponse(resp.StatusCode, resp.Header, nil)
		return indexResponse{
			status:    resp.StatusCode,
			headers:   resp.Header,
			isConcert: isConcert,
		}, fmt.Errorf("read body: %w", err)
	}

	isConcert, isWaitingRoom := inspectIndexResponse(resp.StatusCode, resp.Header, body)
	if !isConcert && client != nil && client.Jar != nil && resp.Request != nil && resp.Request.URL != nil {
		for _, c := range client.Jar.Cookies(resp.Request.URL) {
			if c != nil && (c.Name == "concert_admit" || c.Name == "room_ticket") {
				isConcert = true
				break
			}
		}
	}

	return indexResponse{
		body:          body,
		status:        resp.StatusCode,
		bytes:         int64(len(body)),
		headers:       resp.Header,
		isConcert:     isConcert,
		isWaitingRoom: isWaitingRoom,
	}, nil
}

// FetchURL performs a bounded GET request during pool indexing.
func FetchURL(ctx context.Context, client *http.Client, u string) ([]byte, int, int64, error) {
	res, err := fetchIndexURL(ctx, client, u)
	return res.body, res.status, res.bytes, err
}
