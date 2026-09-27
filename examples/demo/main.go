// A local-only demonstration target with healthy and deliberately broken pages.
// Run: go run ./examples/demo (defaults to 127.0.0.1:8080).
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()
	var sessions atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("demo_session"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "demo_session", Value: fmt.Sprint(sessions.Add(1)), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var content string
		switch r.URL.Path {
		case "/":
			content = `<main id="app"><h1>Welcome to Lemmings Outfitters</h1><p>Follow a trail. Find the failure.</p></main>`
		case "/catalog":
			content = `<main><h1>Catalog</h1><p>Trail boots</p><a href="/product">View product</a></main>`
		case "/product":
			content = `<main id="product"><h1>Trail boots</h1><p>In stock</p></main>`
		case "/broken":
			content = `<main><h1>Something went wrong</h1><img alt="product" src="/missing.png"><script>throw new Error('Product component failed to mount')</script></main>`
		case "/blank":
			fmt.Fprint(w, `<!doctype html><html><head><title>Lemmings blank</title></head><body></body></html>`)
			return
		case "/unavailable":
			w.WriteHeader(503)
			content = `<main><h1>Service unavailable</h1></main>`
		case "/redirect":
			http.Redirect(w, r, "/catalog", 302)
			return
		case "/favicon.ico":
			w.WriteHeader(204)
			return
		case "/sitemap.xml":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<urlset><url><loc>http://%s/</loc></url><url><loc>http://%s/catalog</loc></url><url><loc>http://%s/product</loc></url></urlset>`, r.Host, r.Host, r.Host)
			return
		default:
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Lemmings Outfitters</title><style>body{font:18px system-ui;background:#0f172a;color:#e2e8f0;margin:3rem auto;max-width:900px;padding:1rem}a{color:#6ee7b7;margin-right:1rem}main{padding:3rem 0}small{color:#94a3b8}</style></head><body><nav><a href="/">Home</a><a href="/catalog">Catalog</a><a href="/product">Product</a></nav>%s<small>Dynamic response: %d</small></body></html>`, content, time.Now().UnixNano())
	})
	log.Printf("Local demo: http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
