package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseOriginAndSameOrigin(t *testing.T) {
	origin, err := ParseOrigin("https://example.com:443/path?token=secret#frag")
	if err != nil {
		t.Fatalf("ParseOrigin: %v", err)
	}
	if !SameOrigin(origin, "https://example.com/other") {
		t.Error("default https port 443 should match implicit port")
	}
	// Evil prefix bypass check (v1.0 security regression).
	if SameOrigin(origin, "https://example.com.evil.test/") {
		t.Error("evil suffix domain must NOT match origin")
	}
	if SameOrigin(origin, "http://example.com/") {
		t.Error("different scheme must NOT match origin")
	}
	if _, err := ParseOrigin("ftp://example.com/"); err == nil {
		t.Error("non-http(s) scheme must fail")
	}
}

func TestSafeURL_RedactsUserinfoAndQuery(t *testing.T) {
	got := SafeURL("https://" + "alice:" + "demo@" + "example.com/reset?token=demo123#top")
	if strings.Contains(got, "alice") || strings.Contains(got, "demo") || strings.Contains(got, "top") {
		t.Errorf("SafeURL leaked sensitive components: %q", got)
	}
	if got != "https://example.com/reset" {
		t.Errorf("SafeURL = %q, want https://example.com/reset", got)
	}
}

func TestResolveURL(t *testing.T) {
	base := "https://example.com/dir/page.html"
	cases := []struct {
		href string
		want string
	}{
		{"/about", "https://example.com/about"},
		{"sub.html", "https://example.com/dir/sub.html"},
		{"https://evil.test/x", ""},
		{"javascript:alert(1)", ""},
		{"mailto:a@b.com", ""},
		{"#section", ""},
	}
	for _, tc := range cases {
		got := ResolveURL(base, tc.href)
		if got != tc.want {
			t.Errorf("ResolveURL(%q) = %q, want %q", tc.href, got, tc.want)
		}
	}
}

func TestBuildURLPool_SitemapAndBoundedConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<urlset>`)
		for i := range 30 {
			fmt.Fprintf(w, `<url><loc>http://%s/page/%d</loc></url>`, r.Host, i)
		}
		// Off-origin URL must be filtered out.
		fmt.Fprint(w, `<url><loc>http://evil.example.test/steal</loc></url>`)
		fmt.Fprint(w, `</urlset>`)
	})
	mux.HandleFunc("/page/", func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			prev := peak.Load()
			if cur <= prev || peak.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		fmt.Fprintf(w, "<html><body>page %s</body></html>", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
	if err != nil {
		t.Fatalf("BuildURLPool: %v", err)
	}
	if len(pool.URLs) != 30 {
		t.Errorf("expected 30 same-origin URLs, got %d", len(pool.URLs))
	}
	for _, u := range pool.URLs {
		if strings.Contains(u, "evil.example.test") {
			t.Errorf("off-origin URL leaked into pool: %q", u)
		}
	}
	if peak.Load() > indexConcurrency {
		t.Errorf("SEC-07 regression: indexing concurrency %d exceeded limit %d", peak.Load(), indexConcurrency)
	}
}

func TestBuildURLPool_CrawlFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/a">A</a><a href="https://offsite.test/x">Off</a></body></html>`)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/b">B</a></body></html>`)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body>Leaf</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{Crawl: true, CrawlDepth: 3})
	if err != nil {
		t.Fatalf("BuildURLPool crawl: %v", err)
	}
	if len(pool.URLs) != 3 {
		t.Errorf("expected 3 crawled URLs (/, /a, /b), got %d: %v", len(pool.URLs), pool.URLs)
	}
}

func TestBuildURLPool_NestedSitemapCyclesEntitiesAndFallbacks(t *testing.T) {
	// 1. Nested sitemap index with self-referencing cycle + XML entity decoding (&amp;).
	t.Run("nested_cycle_and_entities", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<sitemapindex><sitemap><loc>http://%[1]s/sitemap.xml</loc></sitemap><sitemap><loc>http://%[1]s/sub.xml</loc></sitemap></sitemapindex>`, r.Host)
		})
		mux.HandleFunc("/sub.xml", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<urlset><url><loc>http://%[1]s/items?a=1&amp;b=2</loc></url><url><loc>http://%[1]s/ok</loc></url></urlset>`, r.Host)
		})
		mux.HandleFunc("/items", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "<html><body>items</body></html>") })
		mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "<html><body>ok</body></html>") })
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil {
			t.Fatalf("BuildURLPool nested cycle: %v", err)
		}
		if len(pool.URLs) != 2 {
			t.Fatalf("expected 2 URLs after cycle termination, got %v", pool.URLs)
		}
		foundDecoded := false
		for _, u := range pool.URLs {
			if strings.HasSuffix(u, "/items?a=1&b=2") {
				foundDecoded = true
			}
		}
		if !foundDecoded {
			t.Errorf("expected XML-decoded &amp; URL in pool, got %v", pool.URLs)
		}
	})

	// 2. robots.txt Sitemap fallback (when /sitemap.xml is 404) & origin-only fallback.
	t.Run("robots_and_origin_only_fallbacks", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "User-agent: *\nSitemap: http://%s/custom-map.xml\n", r.Host)
		})
		mux.HandleFunc("/custom-map.xml", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `<urlset><url><loc>http://%s/from-robots</loc></url></urlset>`, r.Host)
		})
		mux.HandleFunc("/from-robots", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "<html><body>robots</body></html>") })
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil || len(pool.URLs) != 1 || !strings.HasSuffix(pool.URLs[0], "/from-robots") {
			t.Fatalf("robots fallback failed: err=%v urls=%v", err, pool.URLs)
		}

		// Origin-only fallback when neither sitemap, robots, nor crawl is available.
		bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				fmt.Fprint(w, "<html><body>home</body></html>")
				return
			}
			http.NotFound(w, r)
		}))
		defer bare.Close()

		originPool, err := BuildURLPool(context.Background(), bare.URL+"/", BuildOptions{Crawl: false})
		if err != nil || len(originPool.URLs) != 1 {
			t.Fatalf("origin-only fallback failed: err=%v urls=%v", err, originPool.URLs)
		}
	})
}

func FuzzSHA512Hex(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		got := SHA512Hex(b)
		if len(got) != 128 {
			t.Fatalf("expected 128 hex chars, got %d", len(got))
		}
	})
}

func FuzzExtractSitemapLocs(f *testing.F) {
	f.Add(`<urlset><url><loc>https://example.com/a</loc></url></urlset>`)
	f.Add(`not xml`)
	f.Fuzz(func(t *testing.T, s string) {
		_ = ExtractSitemapLocs(s)
	})
}

func FuzzExtractHTMLLinks(f *testing.F) {
	f.Add([]byte(`<a href="/foo">Foo</a><a href="bar?x=1">Bar</a>`), "https://example.com/")
	f.Add([]byte(`<html><body>broken <a href=`), "https://example.com/")
	f.Fuzz(func(t *testing.T, b []byte, base string) {
		for _, u := range ExtractHTMLLinks(b, base) {
			if !SameOrigin(base, u) {
				t.Fatalf("off-origin link extracted: %q (base=%q)", u, base)
			}
		}
	})
}

func FuzzResolveURL(f *testing.F) {
	f.Add("https://example.com/root/", "/path/to/page")
	f.Add("https://example.com/root/", "https://evil.test/x")
	f.Add("https://example.com/root/", "javascript:void(0)")
	f.Fuzz(func(t *testing.T, base, href string) {
		if got := ResolveURL(base, href); got != "" && !SameOrigin(base, got) {
			t.Fatalf("off-origin resolved URL: %q (base=%q href=%q)", got, base, href)
		}
	})
}

func TestInspectIndexResponse(t *testing.T) {
	hdrWith := func(cookies ...string) http.Header {
		h := make(http.Header)
		for _, c := range cookies {
			h.Add("Set-Cookie", c)
		}
		return h
	}

	cases := []struct {
		name          string
		status        int
		headers       http.Header
		body          string
		wantConcert   bool
		wantWaitingRm bool
	}{
		{
			name:          "normal html page",
			status:        http.StatusOK,
			body:          `<html><body><h1>Hello</h1></body></html>`,
			wantConcert:   false,
			wantWaitingRm: false,
		},
		{
			name:          "concert_admit cookie alone",
			status:        http.StatusOK,
			headers:       hdrWith("concert_admit=pass123; Path=/; HttpOnly"),
			body:          `<html><body><h1>Admitted</h1></body></html>`,
			wantConcert:   true,
			wantWaitingRm: false,
		},
		{
			name:          "room_ticket cookie alone",
			status:        http.StatusOK,
			headers:       hdrWith("room_ticket=t1; Path=/"),
			body:          `<html><body><h1>Clear</h1></body></html>`,
			wantConcert:   true,
			wantWaitingRm: false,
		},
		{
			name:          "concert waiting.js html",
			status:        http.StatusOK,
			headers:       hdrWith("room_ticket=t1; Path=/"),
			body:          `<!doctype html><html><head><script src="/_concert/waiting.js" defer></script></head><body><span id="position">4</span></body></html>`,
			wantConcert:   true,
			wantWaitingRm: true,
		},
		{
			name:          "concert queue-page html without script",
			status:        http.StatusOK,
			body:          `<html><body class="queue-page"><div id="position">2</div></body></html>`,
			wantConcert:   true,
			wantWaitingRm: true,
		},
		{
			name:          "legacy waiting room html",
			status:        http.StatusOK,
			body:          `<html><body><span id="position">7</span><script>fetch('/queue/status')</script></body></html>`,
			wantConcert:   true,
			wantWaitingRm: true,
		},
		{
			name:          "concert api-json 429 queued true",
			status:        http.StatusTooManyRequests,
			body:          `{"queued":true,"status_url":"/queue/status"}`,
			wantConcert:   true,
			wantWaitingRm: true,
		},
		{
			name:          "generic 429 queued false is not waiting room",
			status:        http.StatusTooManyRequests,
			body:          `{"queued":false,"error":"rate_limited"}`,
			wantConcert:   false,
			wantWaitingRm: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotConcert, gotWR := inspectIndexResponse(tc.status, tc.headers, []byte(tc.body))
			if gotConcert != tc.wantConcert || gotWR != tc.wantWaitingRm {
				t.Errorf("inspectIndexResponse = (concert=%v, wr=%v), want (concert=%v, wr=%v)",
					gotConcert, gotWR, tc.wantConcert, tc.wantWaitingRm)
			}
		})
	}
}

func TestBuildURLPool_ConcertSequentialHydrationAndCookieJar(t *testing.T) {
	recordPeak := func(inFlight, peak *atomic.Int64) func() {
		cur := inFlight.Add(1)
		for {
			prev := peak.Load()
			if cur <= prev || peak.CompareAndSwap(prev, cur) {
				break
			}
		}
		return func() { inFlight.Add(-1) }
	}

	t.Run("concert_admit_on_sitemap", func(t *testing.T) {
		var inFlight, peak, cookieReplayed atomic.Int64
		const totalPages = 12

		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
			defer recordPeak(&inFlight, &peak)()
			http.SetCookie(w, &http.Cookie{Name: "concert_admit", Value: "pass-sitemap", Path: "/"})
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<urlset>`)
			for i := range totalPages {
				fmt.Fprintf(w, `<url><loc>http://%s/page/%d</loc></url>`, r.Host, i)
			}
			fmt.Fprint(w, `</urlset>`)
		})
		mux.HandleFunc("/page/", func(w http.ResponseWriter, r *http.Request) {
			defer recordPeak(&inFlight, &peak)()
			if c, err := r.Cookie("concert_admit"); err == nil && c.Value == "pass-sitemap" {
				cookieReplayed.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
			fmt.Fprintf(w, "<html><body>concert page %s</body></html>", r.URL.Path)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil {
			t.Fatalf("BuildURLPool: %v", err)
		}
		if peak.Load() > 1 {
			t.Errorf("Concert target exceeded sequential concurrency: peak inFlight=%d, want <= 1", peak.Load())
		}
		if cookieReplayed.Load() != totalPages {
			t.Errorf("expected cookie jar to replay concert_admit on all %d pages, got %d", totalPages, cookieReplayed.Load())
		}
		if len(pool.URLs) != totalPages {
			t.Fatalf("expected %d URLs, got %d", totalPages, len(pool.URLs))
		}
		for _, u := range pool.URLs {
			idx := strings.TrimPrefix(u, srv.URL)
			wantBody := fmt.Sprintf("<html><body>concert page %s</body></html>", idx)
			if got, want := pool.Checksums[u], SHA512Hex([]byte(wantBody)); got != want {
				t.Errorf("checksum mismatch for %s: got %q, want %q", u, got, want)
			}
		}
	})

	t.Run("concert_admit_only_on_first_page_probe", func(t *testing.T) {
		var inFlight, peak, cookieReplayed atomic.Int64
		const totalPages = 10

		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
			defer recordPeak(&inFlight, &peak)()
			// Intentionally no Concert header on /sitemap.xml to verify scoped[0] probe catches it.
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<urlset>`)
			fmt.Fprintf(w, `<url><loc>http://%s/</loc></url>`, r.Host)
			for i := 1; i < totalPages; i++ {
				fmt.Fprintf(w, `<url><loc>http://%s/page/%d</loc></url>`, r.Host, i)
			}
			fmt.Fprint(w, `</urlset>`)
		})
		mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
			defer recordPeak(&inFlight, &peak)()
			http.SetCookie(w, &http.Cookie{Name: "concert_admit", Value: "pass-root", Path: "/"})
			time.Sleep(5 * time.Millisecond)
			fmt.Fprint(w, "<html><body>concert root</body></html>")
		})
		mux.HandleFunc("/page/", func(w http.ResponseWriter, r *http.Request) {
			defer recordPeak(&inFlight, &peak)()
			if c, err := r.Cookie("concert_admit"); err == nil && c.Value == "pass-root" {
				cookieReplayed.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
			fmt.Fprintf(w, "<html><body>concert sub %s</body></html>", r.URL.Path)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil {
			t.Fatalf("BuildURLPool: %v", err)
		}
		if peak.Load() > 1 {
			t.Errorf("Concert target (root probe) exceeded sequential concurrency: peak inFlight=%d, want <= 1", peak.Load())
		}
		if cookieReplayed.Load() != totalPages-1 {
			t.Errorf("expected cookie jar to replay concert_admit on remaining %d pages, got %d", totalPages-1, cookieReplayed.Load())
		}
		if len(pool.URLs) != totalPages {
			t.Fatalf("expected %d URLs, got %d", totalPages, len(pool.URLs))
		}
		for _, u := range pool.URLs {
			if pool.Checksums[u] == "" {
				t.Errorf("expected non-empty SHA-512 checksum for %s", u)
			}
		}
	})
}

func TestBuildURLPool_WaitingRoomHTMLProtection(t *testing.T) {
	waitingHTML := `<!doctype html><html><head><script src="/_concert/waiting.js" defer></script></head><body class="queue-page"><div id="position">3</div></body></html>`
	waitingHash := SHA512Hex([]byte(waitingHTML))

	t.Run("halts_on_first_page_probe_and_preserves_urls", func(t *testing.T) {
		var pageHits atomic.Int64
		const totalPages = 10

		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<urlset>`)
			for i := range totalPages {
				fmt.Fprintf(w, `<url><loc>http://%s/page/%d</loc></url>`, r.Host, i)
			}
			fmt.Fprint(w, `</urlset>`)
		})
		mux.HandleFunc("/page/", func(w http.ResponseWriter, _ *http.Request) {
			pageHits.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "room_ticket", Value: "t1", Path: "/"})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, waitingHTML)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil {
			t.Fatalf("BuildURLPool: %v", err)
		}
		if pageHits.Load() != 1 {
			t.Errorf("expected hydration to halt after 1 waiting-room probe, got %d page requests", pageHits.Load())
		}
		if len(pool.URLs) != totalPages {
			t.Fatalf("expected all %d discovered URLs preserved in pool.URLs, got %d", totalPages, len(pool.URLs))
		}
		for u, sum := range pool.Checksums {
			if sum == waitingHash || sum != "" {
				t.Errorf("waiting.html checksum leaked into pool.Checksums[%s] = %q", u, sum)
			}
		}
	})

	t.Run("halts_when_sitemap_itself_returns_waiting_html", func(t *testing.T) {
		var sitemapHits, robotsHits, rootHits atomic.Int64
		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
			sitemapHits.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "room_ticket", Value: "t1", Path: "/"})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, waitingHTML)
		})
		mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
			robotsHits.Add(1)
			http.NotFound(w, r)
		})
		mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
			rootHits.Add(1)
			fmt.Fprint(w, waitingHTML)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{Crawl: true, CrawlDepth: 2})
		if err != nil {
			t.Fatalf("BuildURLPool: %v", err)
		}
		if sitemapHits.Load() != 1 || robotsHits.Load() != 0 || rootHits.Load() != 0 {
			t.Errorf("expected immediate halt after sitemap waiting room: sitemap=%d robots=%d root=%d",
				sitemapHits.Load(), robotsHits.Load(), rootHits.Load())
		}
		if len(pool.URLs) != 1 || pool.URLs[0] != srv.URL+"/" {
			t.Errorf("expected origin URL preserved in pool.URLs, got %v", pool.URLs)
		}
		if len(pool.Checksums) != 0 {
			t.Errorf("expected empty checksums when waiting room detected, got %v", pool.Checksums)
		}
	})

	t.Run("legacy_waiting_room_html_halts_and_preserves", func(t *testing.T) {
		legacyHTML := `<html><body><h1>Queue</h1><span id="position">5</span><script>fetch("/queue/status")</script></body></html>`
		legacyHash := SHA512Hex([]byte(legacyHTML))
		var pageHits atomic.Int64

		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `<urlset><url><loc>http://%[1]s/a</loc></url><url><loc>http://%[1]s/b</loc></url><url><loc>http://%[1]s/c</loc></url></urlset>`, r.Host)
		})
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			pageHits.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, legacyHTML)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil {
			t.Fatalf("BuildURLPool: %v", err)
		}
		if pageHits.Load() != 1 {
			t.Errorf("expected 1 page request before halt, got %d", pageHits.Load())
		}
		if len(pool.URLs) != 3 {
			t.Errorf("expected 3 URLs preserved, got %v", pool.URLs)
		}
		for u, sum := range pool.Checksums {
			if sum == legacyHash || sum != "" {
				t.Errorf("legacy waiting room checksum leaked for %s: %q", u, sum)
			}
		}
	})

	t.Run("halts_mid_sequential_hydration_preserving_valid_and_remaining", func(t *testing.T) {
		var pageHits atomic.Int64
		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "concert_admit", Value: "pass", Path: "/"})
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<urlset>
				<url><loc>http://%[1]s/p/0</loc></url>
				<url><loc>http://%[1]s/p/1</loc></url>
				<url><loc>http://%[1]s/p/2</loc></url>
				<url><loc>http://%[1]s/p/3</loc></url>
			</urlset>`, r.Host)
		})
		mux.HandleFunc("/p/0", func(w http.ResponseWriter, _ *http.Request) {
			pageHits.Add(1)
			fmt.Fprint(w, "<html><body>page 0</body></html>")
		})
		mux.HandleFunc("/p/", func(w http.ResponseWriter, _ *http.Request) {
			pageHits.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "room_ticket", Value: "t2", Path: "/"})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, waitingHTML)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil {
			t.Fatalf("BuildURLPool: %v", err)
		}
		if pageHits.Load() != 2 {
			t.Errorf("expected exactly 2 page hits (/p/0 ok + /p/1 waiting room), got %d", pageHits.Load())
		}
		if len(pool.URLs) != 4 {
			t.Fatalf("expected all 4 URLs preserved, got %v", pool.URLs)
		}
		wantP0 := SHA512Hex([]byte("<html><body>page 0</body></html>"))
		if got := pool.Checksums[srv.URL+"/p/0"]; got != wantP0 {
			t.Errorf("expected valid checksum for /p/0, got %q", got)
		}
		for _, suffix := range []string{"/p/1", "/p/2", "/p/3"} {
			if got := pool.Checksums[srv.URL+suffix]; got != "" {
				t.Errorf("expected empty checksum for %s after waiting room, got %q", suffix, got)
			}
		}
	})
}

func TestBuildURLPool_WaitingRoomAPIJSON429Protection(t *testing.T) {
	jsonBody := `{"queued":true,"status_url":"/queue/status"}`
	jsonHash := SHA512Hex([]byte(jsonBody))
	var pageHits atomic.Int64
	const totalPages = 8

	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<urlset>`)
		for i := range totalPages {
			fmt.Fprintf(w, `<url><loc>http://%s/api/%d</loc></url>`, r.Host, i)
		}
		fmt.Fprint(w, `</urlset>`)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		pageHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, jsonBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
	if err != nil {
		t.Fatalf("BuildURLPool: %v", err)
	}
	if pageHits.Load() != 1 {
		t.Errorf("expected hydration to halt after 1 HTTP 429 queued probe, got %d requests", pageHits.Load())
	}
	if len(pool.URLs) != totalPages {
		t.Fatalf("expected all %d URLs preserved in pool.URLs, got %d", totalPages, len(pool.URLs))
	}
	for u, sum := range pool.Checksums {
		if sum == jsonHash || sum != "" {
			t.Errorf("429 queued JSON checksum leaked into pool.Checksums[%s] = %q", u, sum)
		}
	}
}

func TestBuildURLPool_ExcludesConcertInternalPaths(t *testing.T) {
	t.Run("sitemap_and_scope_exclude_concert_paths", func(t *testing.T) {
		var concertHits atomic.Int64
		mux := http.NewServeMux()
		mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<urlset>
				<url><loc>http://%[1]s/</loc></url>
				<url><loc>http://%[1]s/about</loc></url>
				<url><loc>http://%[1]s/_concert</loc></url>
				<url><loc>http://%[1]s/_concert/waiting.js</loc></url>
				<url><loc>http://%[1]s/_concert/waiting.css</loc></url>
				<url><loc>http://%[1]s/foo/../_concert/waiting.js</loc></url>
			</urlset>`, r.Host)
		})
		mux.HandleFunc("/_concert/", func(w http.ResponseWriter, _ *http.Request) {
			concertHits.Add(1)
			fmt.Fprint(w, "/* waiting.js */")
		})
		mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "<html><body>home</body></html>")
		})
		mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "<html><body>about</body></html>")
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
		if err != nil {
			t.Fatalf("BuildURLPool: %v", err)
		}
		if concertHits.Load() != 0 {
			t.Errorf("expected zero fetches to /_concert/*, got %d", concertHits.Load())
		}
		if len(pool.URLs) != 2 {
			t.Fatalf("expected 2 non-concert URLs (/, /about), got %d: %v", len(pool.URLs), pool.URLs)
		}
		for _, u := range pool.URLs {
			if strings.Contains(u, "/_concert") {
				t.Errorf("/_concert path leaked into pool.URLs: %q", u)
			}
		}
	})

	t.Run("crawl_excludes_concert_paths", func(t *testing.T) {
		var concertHits atomic.Int64
		mux := http.NewServeMux()
		mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><a href="/real">Real</a><a href="/_concert/waiting.js">WaitJS</a></body></html>`)
		})
		mux.HandleFunc("/real", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body>real page</body></html>`)
		})
		mux.HandleFunc("/_concert/", func(w http.ResponseWriter, _ *http.Request) {
			concertHits.Add(1)
			fmt.Fprint(w, "/* waiting.js */")
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{Crawl: true, CrawlDepth: 2})
		if err != nil {
			t.Fatalf("BuildURLPool crawl: %v", err)
		}
		if concertHits.Load() != 0 {
			t.Errorf("crawler should never fetch /_concert/*, got %d hits", concertHits.Load())
		}
		if len(pool.URLs) != 2 {
			t.Fatalf("expected 2 URLs (/, /real), got %v", pool.URLs)
		}
	})
}

func TestBuildURLPool_NormalNonConcertHydratesParallelWithChecksums(t *testing.T) {
	var inFlight, peak atomic.Int64
	const totalPages = 20

	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<urlset>`)
		for i := range totalPages {
			fmt.Fprintf(w, `<url><loc>http://%s/item/%d</loc></url>`, r.Host, i)
		}
		fmt.Fprint(w, `</urlset>`)
	})
	mux.HandleFunc("/item/", func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			prev := peak.Load()
			if cur <= prev || peak.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(10 * time.Millisecond)
		fmt.Fprintf(w, "<html><body>item %s</body></html>", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pool, err := BuildURLPool(context.Background(), srv.URL+"/", BuildOptions{})
	if err != nil {
		t.Fatalf("BuildURLPool: %v", err)
	}
	if len(pool.URLs) != totalPages {
		t.Fatalf("expected %d URLs, got %d", totalPages, len(pool.URLs))
	}
	if peak.Load() < 2 || peak.Load() > indexConcurrency {
		t.Errorf("expected parallel hydration in [2..%d] for non-Concert server, got peak=%d", indexConcurrency, peak.Load())
	}
	for _, u := range pool.URLs {
		idx := strings.TrimPrefix(u, srv.URL)
		want := SHA512Hex([]byte(fmt.Sprintf("<html><body>item %s</body></html>", idx)))
		if got := pool.Checksums[u]; got != want {
			t.Errorf("checksum mismatch for %s: got %q, want %q", u, got, want)
		}
	}
}
