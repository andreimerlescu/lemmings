package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
