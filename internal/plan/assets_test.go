package plan

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/andreimerlescu/lemmings/v2/internal/discovery"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func TestExtractAssets_AllElementsAndRules(t *testing.T) {
	base := mustURL(t, "https://arcade.example.com/games/index.html")
	doc := []byte(`<!doctype html>
<html>
<head>
  <!-- <link rel="stylesheet" href="/commented.css"> -->
  <link rel="stylesheet" href="/assets/app.css#v1">
  <link rel="icon" href="/favicon.ico">
  <link rel="shortcut icon" href="/shortcut.png">
  <link rel="apple-touch-icon" href="/apple-icon.png">
  <link rel="preload" as="font" href="/fonts/arcade">
  <link rel="modulepreload" href="/js/mod">
  <link rel="manifest" href="/site.webmanifest">
  <link rel="canonical" href="https://arcade.example.com/canonical">
  <link rel="stylesheet" href="https://cdn.other.test/offsite.css">
  <link rel="stylesheet" href="/_concert/waiting.css">
  <script src="/js/bundle.js?a=1&amp;b=2"></script>
  <script>var fake = '<img src="/inside-script.png">';</script>
  <style>
    /* url(/commented-in-style.png) */
    @import "/css/reset.css";
    body { background: url("/img/bg.webp#hero"); content: "url(/inside-string.png)"; }
    .skip { background: url(data:image/png;base64,AAAA); }
  </style>
</head>
<body style="background-image: url('/img/inline-style.jpg')">
  <img src="logo.svg" data-src="/img/lazy.png" srcset="/img/1x.png 1x, /img/2x.png 2x, /img/cdn/w_400,h_300/photo.webp 400w">
  <img src="data:image/gif;base64,R0lGODlhAQABAIAAAP///wAAACH5BAEAAAAALAAAAAABAAEAAAICRAEAOw==">
  <img src="/_concert/logo.svg">
  <picture>
    <source srcset="/img/pic.avif 1x, /img/pic2.avif 2x" type="image/avif">
  </picture>
  <video src="/media/intro.mp4" poster="/media/poster">
    <source src="/media/intro.webm" type="video/webm">
  </video>
  <audio src="/media/theme.mp3">
    <source src="/media/theme-fallback" type="audio/ogg">
  </audio>
  <embed src="/media/widget.swf" type="application/x-shockwave-flash">
  <object data="/docs/spec.json" type="application/json"></object>
  <iframe src="/embed/scoreboard"></iframe>
  <!-- Duplicate should be ignored -->
  <link rel="stylesheet" href="/assets/app.css#v2">
</body>
</html>`)

	got := ExtractAssets(base, doc)
	want := []AssetRef{
		{URL: "https://arcade.example.com/assets/app.css", Class: "style"},
		{URL: "https://arcade.example.com/favicon.ico", Class: "image"},
		{URL: "https://arcade.example.com/shortcut.png", Class: "image"},
		{URL: "https://arcade.example.com/apple-icon.png", Class: "image"},
		{URL: "https://arcade.example.com/fonts/arcade", Class: "font"},
		{URL: "https://arcade.example.com/js/mod", Class: "script"},
		{URL: "https://arcade.example.com/site.webmanifest", Class: "data"},
		{URL: "https://arcade.example.com/js/bundle.js?a=1&b=2", Class: "script"},
		{URL: "https://arcade.example.com/css/reset.css", Class: "style"},
		{URL: "https://arcade.example.com/img/bg.webp", Class: "image"},
		{URL: "https://arcade.example.com/img/inline-style.jpg", Class: "image"},
		{URL: "https://arcade.example.com/games/logo.svg", Class: "image"},
		{URL: "https://arcade.example.com/img/lazy.png", Class: "image"},
		{URL: "https://arcade.example.com/img/1x.png", Class: "image"},
		{URL: "https://arcade.example.com/img/2x.png", Class: "image"},
		{URL: "https://arcade.example.com/img/cdn/w_400,h_300/photo.webp", Class: "image"},
		{URL: "https://arcade.example.com/img/pic.avif", Class: "image"},
		{URL: "https://arcade.example.com/img/pic2.avif", Class: "image"},
		{URL: "https://arcade.example.com/media/intro.mp4", Class: "video"},
		{URL: "https://arcade.example.com/media/poster", Class: "image"},
		{URL: "https://arcade.example.com/media/intro.webm", Class: "video"},
		{URL: "https://arcade.example.com/media/theme.mp3", Class: "audio"},
		{URL: "https://arcade.example.com/media/theme-fallback", Class: "audio"},
		{URL: "https://arcade.example.com/media/widget.swf", Class: "other"},
		{URL: "https://arcade.example.com/docs/spec.json", Class: "data"},
		{URL: "https://arcade.example.com/embed/scoreboard", Class: "other"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractAssets mismatch:\ngot  (%d): %+v\nwant (%d): %+v", len(got), got, len(want), want)
	}
}

func TestExtractAssets_BaseHref(t *testing.T) {
	base := mustURL(t, "https://example.com/app/page.html")

	// Same-origin <base href> changes relative resolution.
	doc := []byte(`<html><head>
		<base href="/static/v2/">
		<base href="/ignored-second-base/">
		<link rel="stylesheet" href="theme.css">
		<script src="app.js"></script>
	</head><body><img src="hero.png"></body></html>`)

	got := ExtractAssets(base, doc)
	want := []AssetRef{
		{URL: "https://example.com/static/v2/theme.css", Class: "style"},
		{URL: "https://example.com/static/v2/app.js", Class: "script"},
		{URL: "https://example.com/static/v2/hero.png", Class: "image"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("base href mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}

	// Off-origin <base href> must NOT allow off-origin assets to sneak through.
	evilBase := []byte(`<html><head>
		<base href="https://evil.test/static/">
		<script src="steal.js"></script>
		<script src="https://example.com/safe.js"></script>
	</head></html>`)
	gotEvil := ExtractAssets(base, evilBase)
	wantEvil := []AssetRef{
		{URL: "https://example.com/safe.js", Class: "script"},
	}
	if !reflect.DeepEqual(gotEvil, wantEvil) {
		t.Fatalf("off-origin base href mismatch:\ngot:  %+v\nwant: %+v", gotEvil, wantEvil)
	}
}

func TestExtractAssets_CapAt256AndNilInputs(t *testing.T) {
	base := mustURL(t, "http://127.0.0.1:8080/")
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := range 400 {
		fmt.Fprintf(&b, `<img src="/img/%d.png">`, i)
	}
	b.WriteString("</body></html>")

	got := ExtractAssets(base, []byte(b.String()))
	if len(got) != 256 {
		t.Fatalf("expected cap at 256 refs, got %d", len(got))
	}
	if got[0].URL != "http://127.0.0.1:8080/img/0.png" || got[255].URL != "http://127.0.0.1:8080/img/255.png" {
		t.Errorf("unexpected boundary refs: first=%+v last=%+v", got[0], got[255])
	}

	if refs := ExtractAssets(nil, []byte(`<img src="/a.png">`)); len(refs) != 0 {
		t.Errorf("nil baseURL should return empty, got %+v", refs)
	}
	if refs := ExtractAssets(mustURL(t, "ftp://example.com/"), []byte(`<img src="/a.png">`)); len(refs) != 0 {
		t.Errorf("non-http baseURL should return empty, got %+v", refs)
	}
}

func TestExtractCSSAssets(t *testing.T) {
	cssURL := mustURL(t, "https://example.com/assets/css/main.css")
	css := []byte(`
/* @import "/ignored-comment.css"; url(/ignored.png) */
@import "base.css";
@IMPORT url('../theme/dark.css#frag');
@import '/_concert/queue.css';
@import "https://evil.test/bad.css";

@font-face {
  font-family: "Arcade";
  src: url("../fonts/arcade.woff2?#iefix") format("woff2"),
       url('../fonts/arcade.woff') format('woff');
}

.hero {
  background: url(/assets/img/hero.png#top);
  content: "url(/not-a-real-asset.png)";
  cursor: url(data:image/png;base64,AAA),
          url(blob:https://example.com/123),
          url(about:blank),
          url(javascript:alert(1)),
          url(https://cdn.evil.test/x.png),
          url(/_concert/spin.svg),
          url(../../_concert/traversal.svg),
          url(  ../img/cursor.svg  ),
          auto;
}
/* Duplicate should be deduped */
.dup { background: url(/assets/img/hero.png#other); }
`)

	got := ExtractCSSAssets(cssURL, css)
	want := []AssetRef{
		{URL: "https://example.com/assets/css/base.css", Class: "style"},
		{URL: "https://example.com/assets/theme/dark.css", Class: "style"},
		{URL: "https://example.com/assets/fonts/arcade.woff2?", Class: "font"},
		{URL: "https://example.com/assets/fonts/arcade.woff", Class: "font"},
		{URL: "https://example.com/assets/img/hero.png", Class: "image"},
		{URL: "https://example.com/assets/img/cursor.svg", Class: "image"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractCSSAssets mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}

	// Cap at 256 refs.
	var big strings.Builder
	for i := range 300 {
		fmt.Fprintf(&big, ".c%d { background: url(/i/%d.png); }\n", i, i)
	}
	capped := ExtractCSSAssets(cssURL, []byte(big.String()))
	if len(capped) != 256 {
		t.Fatalf("expected CSS cap at 256 refs, got %d", len(capped))
	}
}

func TestClassifyAsset(t *testing.T) {
	cases := []struct {
		rawURL      string
		contentType string
		want        string
	}{
		// MIME takes precedence over extension.
		{"/download/blob.bin", "text/css; charset=utf-8", "style"},
		{"/api/endpoint", "application/javascript", "script"},
		{"/api/endpoint", "text/javascript; charset=utf-8", "script"},
		{"/api/endpoint", "application/wasm", "script"},
		{"/avatar/123", "image/webp", "image"},
		{"/vector.xml", "image/svg+xml", "image"},
		{"/f/1", "font/woff2", "font"},
		{"/f/2", "application/font-woff", "font"},
		{"/m/1", "audio/mpeg", "audio"},
		{"/m/2", "video/mp4", "video"},
		{"/m/3", "application/vnd.apple.mpegurl", "video"},
		{"/d/1", "application/manifest+json", "data"},
		{"/d/2", "application/problem+json", "data"},
		{"/d/3", "application/xml", "data"},
		// Generic MIME falls back to extension.
		{"/static/font.woff2?v=3", "application/octet-stream", "font"},
		{"/static/app.CSS", "", "style"},
		{"/static/app.mjs", "", "script"},
		{"/static/photo.jpeg?w=400", "", "image"},
		{"/static/icon.ico", "", "image"},
		{"/static/font.ttf", "", "font"},
		{"/static/song.opus", "", "audio"},
		{"/static/clip.webm", "", "video"},
		{"/static/config.json", "", "data"},
		{"/static/manifest.webmanifest", "", "data"},
		{"/static/unknown", "", "other"},
		{"/static/unknown", "application/octet-stream", "other"},
	}

	for _, tc := range cases {
		if got := ClassifyAsset(tc.rawURL, tc.contentType); got != tc.want {
			t.Errorf("ClassifyAsset(%q, %q) = %q, want %q", tc.rawURL, tc.contentType, got, tc.want)
		}
	}
}

func FuzzExtractAssets(f *testing.F) {
	f.Add("https://example.com/dir/page.html", []byte(`<link rel="stylesheet" href="/app.css"><script src="app.js"></script><img src="/a.png" srcset="/b.png 2x">`))
	f.Add("http://127.0.0.1:8080/", []byte(`<base href="/sub/"><style>@import "a.css"; body{background:url(img.png)}</style><div style="background:url('/x.svg')"></div>`))
	f.Add("https://example.com/", []byte(`<!-- unclosed <script src="/a.js"><style>url(`))
	f.Add("https://example.com/", []byte(`<img src="/_concert/waiting.js"><link rel="stylesheet" href="https://evil.test/x.css">`))

	f.Fuzz(func(t *testing.T, rawBase string, body []byte) {
		base, err := url.Parse(rawBase)
		if err != nil {
			return
		}
		refs := ExtractAssets(base, body)
		if len(refs) > maxExtractedAssets {
			t.Fatalf("ExtractAssets returned %d refs, exceeding cap %d", len(refs), maxExtractedAssets)
		}
		seen := make(map[string]bool, len(refs))
		for _, r := range refs {
			if !discovery.SameOrigin(base.String(), r.URL) {
				t.Fatalf("off-origin asset extracted: %q (base=%q)", r.URL, rawBase)
			}
			u, err := url.Parse(r.URL)
			if err != nil {
				t.Fatalf("extracted invalid URL %q: %v", r.URL, err)
			}
			if u.Fragment != "" {
				t.Fatalf("fragment not stripped: %q", r.URL)
			}
			if isConcertInternalPath(u.Path) {
				t.Fatalf("Concert internal path leaked: %q", r.URL)
			}
			if seen[r.URL] {
				t.Fatalf("duplicate asset extracted: %q", r.URL)
			}
			seen[r.URL] = true
		}

		cssRefs := ExtractCSSAssets(base, body)
		if len(cssRefs) > maxExtractedAssets {
			t.Fatalf("ExtractCSSAssets returned %d refs, exceeding cap %d", len(cssRefs), maxExtractedAssets)
		}
		for _, r := range cssRefs {
			if !discovery.SameOrigin(base.String(), r.URL) {
				t.Fatalf("off-origin CSS asset extracted: %q (base=%q)", r.URL, rawBase)
			}
		}
	})
}
