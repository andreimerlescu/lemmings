// Neon Arcade is a local demo target for lemmings. It includes healthy pages,
// a slow flaky leaderboard (503 + Retry-After), a soft error page (200 OK error),
// a structured JSON 500 endpoint (/api/tokens), a JS-fault page (/arcade-glitch)
// for real-browser Scouts & console capture, a blank page, a broken link, a
// redirect, and a waiting room.
//
//	go run ./examples/demo                   # http://127.0.0.1:8080
//	go run ./examples/demo -chaos 0          # everything healthy
//	go run ./examples/demo -chaos 1 -room 3  # more failures, a busier queue
//
// It only listens on the loopback interface.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var games = []struct{ slug, name, blurb string }{
	{"outrun", "Outrun Horizon", "Chase the sunset down an endless neon highway."},
	{"laser", "Laser Lagoon", "Bounce photons across a midnight lagoon."},
	{"pixel", "Pixel Drift", "Drift a chrome hovercar through a pixel storm."},
	{"vapor", "Vapor Knights", "Duel in the marble halls of a vaporwave castle."},
	{"cosmic", "Cosmic Cassette", "Rewind time before the tape runs out."},
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address (loopback only)")
	chaos := flag.Float64("chaos", 0.5, "how often things break, 0 (never) to 1 (often)")
	room := flag.Int("room", 6, "visitors the checkout serves at once before queueing; 0 disables the waiting room")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &arcade{chaos: *chaos, capacity: *room}
	a.room.served.Store(int64(*room))
	go a.admit(ctx)

	log.Printf("Neon Arcade is open at http://%s (chaos %.2f, waiting room capacity %d)", *addr, *chaos, *room)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type arcade struct {
	chaos    float64
	capacity int
	visitors atomic.Int64
	room     waitingRoom
}

type waitingRoom struct {
	mu      sync.Mutex
	tickets map[string]int64
	next    atomic.Int64
	served  atomic.Int64
}

func (a *arcade) admit(ctx context.Context) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.room.served.Load() < a.room.next.Load()+int64(a.capacity) {
				a.room.served.Add(1)
			}
		}
	}
}

func (a *arcade) broken(p float64) bool {
	return rand.Float64() < p*a.chaos*2
}

func (a *arcade) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		b = fmt.Appendf(b, `<h1>Insert coin.</h1><p class="lead">The arcade at the end of the highway. %s visitors and counting.</p><div class="grid">`, strconv.FormatInt(a.visitors.Load(), 10))
		for _, g := range games {
			b = fmt.Appendf(b, `<a class="tile" href="/games/%s"><b>%s</b><span>%s</span></a>`, g.slug, g.name, g.blurb)
		}
		b = append(b, `</div>`...)
		a.page(w, r, "Neon Arcade", string(b))
	})
	mux.HandleFunc("GET /games", func(w http.ResponseWriter, r *http.Request) {
		body := `<h1>All games</h1><ul>`
		for _, g := range games {
			body += fmt.Sprintf(`<li><a href="/games/%s">%s</a> — %s</li>`, g.slug, g.name, g.blurb)
		}
		a.page(w, r, "Games", body+`</ul><p><a href="/games/lost-highway">Lost Highway</a> (coming soon)</p>`)
	})
	mux.HandleFunc("GET /games/{slug}", func(w http.ResponseWriter, r *http.Request) {
		for _, g := range games {
			if g.slug == r.PathValue("slug") {
				a.page(w, r, g.name, fmt.Sprintf(`<h1>%s</h1><p class="lead">%s</p><p>High score: %d</p><p><a href="/checkout?game=%s">Buy tokens</a> · <a href="/scores">Leaderboard</a></p>`,
					g.name, g.blurb, rand.IntN(999_999), g.slug))
				return
			}
		}
		w.Header().Set("X-Request-Id", "req-404-"+strconv.Itoa(rand.IntN(99999)))
		w.WriteHeader(http.StatusNotFound)
		a.page(w, r, "Not found", `<div class="errbox"><h1>404 — Cabinet Unplugged</h1><p>That cabinet was unplugged in 1987.</p></div>`)
	})
	mux.HandleFunc("GET /scores", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(150+rand.IntN(int(600*a.chaos)+50)) * time.Millisecond)
		if a.broken(0.06) {
			w.Header().Set("Retry-After", "2")
			w.Header().Set("X-Request-Id", "req-503-"+strconv.Itoa(rand.IntN(99999)))
			w.WriteHeader(http.StatusServiceUnavailable)
			a.page(w, r, "Leaderboard Unavailable", `<div class="errbox"><h1>503 — Leaderboard Overloaded</h1><p>The leaderboard is recalculating. Try again shortly.</p></div>`)
			return
		}
		a.page(w, r, "Leaderboard", `<h1>Leaderboard</h1><ol><li>AAA — 999,999</li><li>LEM — 812,440</li><li>RAD — 777,777</li></ol>`)
	})
	mux.HandleFunc("GET /tokens", func(w http.ResponseWriter, r *http.Request) {
		if a.broken(0.12) {
			// A soft error: 200 OK with an error page.
			a.page(w, r, "Tokens", `<div class="errbox"><h1>Something went wrong</h1><p>Token machine jammed. Please flag an attendant.</p></div>`)
			return
		}
		a.page(w, r, "Tokens", `<h1>Tokens</h1><p>10 tokens for $5. Welcome back, player one.</p><p><a href="/api/tokens">Inspect token API</a></p>`)
	})
	mux.HandleFunc("GET /api/tokens", func(w http.ResponseWriter, r *http.Request) {
		reqID := "req-api-" + strconv.Itoa(10000+rand.IntN(89999))
		w.Header().Set("X-Request-Id", reqID)
		if a.broken(0.35) || a.chaos > 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":       "https://neon-arcade.local/errors/coin-hopper-fault",
				"title":      "Coin Hopper Fault",
				"status":     500,
				"code":       "E_HOPPER_JAM",
				"error":      "Token dispenser coil timed out while reading balance",
				"detail":     "Upstream coin-ledger service did not acknowledge slot #7 within 250ms.",
				"request_id": reqID,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": 10, "currency": "USD", "price": 5})
	})
	mux.HandleFunc("GET /arcade-glitch", func(w http.ResponseWriter, r *http.Request) {
		// Returns HTTP 200 OK while triggering real client-side console errors,
		// an uncaught TypeError, and a 404 subresource so headless browser Scouts
		// and CDP screenshots demonstrate frontend forensics.
		a.page(w, r, "CRT Diagnostics", `<div class="errbox">
<h1>Arcade Cabinet Glitch</h1>
<p class="lead">HTTP 200 OK — but client-side JavaScript is crashing on render.</p>
<img src="/assets/missing-sprite.png" alt="Missing cabinet sprite" width="120" height="60">
<p id="status">Booting diagnostics…</p>
</div>
<script>
console.warn("CRT horizontal hold drifting (+14Hz)");
console.error("CRT phosphor sync fault on cabinet #4: framebuffer checksum mismatch");
document.getElementById("status").textContent = "Diagnostics halted: JS runtime fault";
window.arcadeCabinet.insertCoin();
</script>`)
	})
	mux.HandleFunc("GET /checkout", func(w http.ResponseWriter, r *http.Request) {
		if pos := a.queuePosition(w, r); pos > 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, queueHTML, pos)
			return
		}
		a.page(w, r, "Checkout", `<h1>Checkout</h1><p>You're in. Tokens are on the way.</p><p><a href="/tokens">View tokens</a></p>`)
	})
	mux.HandleFunc("GET /queue/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"position":%d}`, a.queuePosition(w, r))
	})
	mux.HandleFunc("GET /blank", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Neon Arcade</title></head><body></body></html>`)
	})
	mux.HandleFunc("GET /old-arcade", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/games", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /about", func(w http.ResponseWriter, r *http.Request) {
		a.page(w, r, "About", `<h1>About</h1><p>Neon Arcade is a demo target for lemmings. Nothing here is real, including the high scores.</p><p><a href="/old-arcade">The old arcade</a> · <a href="/blank">The back room</a> · <a href="/arcade-glitch">CRT Diagnostics</a> · <a href="/api/tokens">Token API</a> · <a href="/logout">Log out</a></p>`)
	})
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "player", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /assets/arcade.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		fmt.Fprint(w, arcadeCSS)
	})
	mux.HandleFunc("GET /assets/logo.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=300")
		fmt.Fprint(w, arcadeLogoSVG)
	})
	mux.HandleFunc("GET /sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		base := "http://" + r.Host
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
		for _, p := range []string{"/", "/games", "/scores", "/tokens", "/about", "/arcade-glitch"} {
			fmt.Fprintf(w, "<url><loc>%s%s</loc></url>", base, p)
		}
		for _, g := range games {
			fmt.Fprintf(w, "<url><loc>%s/games/%s</loc></url>", base, g.slug)
		}
		fmt.Fprint(w, `</urlset>`)
	})
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "User-agent: *\nDisallow: /logout\nSitemap: http://%s/sitemap.xml\n", r.Host)
	})
	return a.session(mux)
}

func (a *arcade) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Keep /assets/* stateless and exempt from session minting, chaos, and waiting room.
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			if _, err := r.Cookie("player"); err != nil {
				n := a.visitors.Add(1)
				http.SetCookie(w, &http.Cookie{Name: "player", Value: "p" + strconv.FormatInt(n, 36), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *arcade) queuePosition(_ http.ResponseWriter, r *http.Request) int64 {
	if a.capacity <= 0 {
		return 0
	}
	id := r.RemoteAddr
	if c, err := r.Cookie("player"); err == nil {
		id = c.Value
	}
	a.room.mu.Lock()
	if a.room.tickets == nil {
		a.room.tickets = make(map[string]int64)
	}
	ticket, ok := a.room.tickets[id]
	if !ok {
		ticket = a.room.next.Add(1)
		a.room.tickets[id] = ticket
	}
	a.room.mu.Unlock()
	return max(ticket-a.room.served.Load(), 0)
}

func (a *arcade) page(w http.ResponseWriter, _ *http.Request, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, pageHTML, html.EscapeString(title), body)
}

const pageHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s · Neon Arcade</title><link rel="stylesheet" href="/assets/arcade.css"><style>
body{margin:0;min-height:100vh;font:17px/1.6 system-ui,sans-serif;color:#f7f1ff;background:linear-gradient(#171520,#2a1747 60%%,#4b1d52)}
nav{display:flex;align-items:center;gap:18px;padding:18px 28px;border-bottom:1px solid #ff7edb44}nav a{color:#36f9f6;text-decoration:none}
main{max-width:860px;margin:0 auto;padding:36px 28px}h1{font-style:italic;letter-spacing:.06em;color:#ff7edb;text-shadow:0 0 8px #f92aad}
a{color:#fede5d}.lead{color:#b893ce}.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:14px}
.tile{display:block;padding:16px;border:1px solid #36f9f666;border-radius:12px;text-decoration:none;color:#f7f1ff}.tile b{display:block;color:#36f9f6}
.errbox{padding:20px 24px;border:1px solid #fe4450aa;border-radius:12px;background:#3a1426cc}
</style></head><body><nav><img class="arcade-logo" src="/assets/logo.svg" alt="Neon Arcade" width="24" height="24"><a href="/">Neon Arcade</a><a href="/games">Games</a><a href="/scores">Leaderboard</a><a href="/tokens">Tokens</a><a href="/checkout">Checkout</a><a href="/arcade-glitch">Diagnostics</a><a href="/about">About</a></nav>
<main>%s</main></body></html>`

const arcadeCSS = `/* Neon Arcade Synthwave static stylesheet */
.arcade-logo{display:inline-block;vertical-align:middle;filter:drop-shadow(0 0 6px #ff7edb)}
.tile{transition:transform .15s ease,border-color .15s ease;background:rgba(29,23,39,.55)}
.tile:hover{transform:translateY(-2px);border-color:#ff7edb}
nav a:hover{color:#fede5d}
`

const arcadeLogoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="24" height="24" role="img" aria-label="Neon Arcade">
<defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1"><stop offset="0%" stop-color="#fede5d"/><stop offset="55%" stop-color="#ff8b39"/><stop offset="100%" stop-color="#f92aad"/></linearGradient></defs>
<circle cx="12" cy="12" r="10" fill="url(#g)"/>
<rect x="2" y="13" width="20" height="1.5" fill="#171520"/>
<rect x="2" y="16" width="20" height="1.5" fill="#171520"/>
<rect x="2" y="19" width="20" height="2" fill="#171520"/>
</svg>`

const queueHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Waiting room · Neon Arcade</title>
<style>body{font:18px system-ui;background:#241b2f;color:#f7f1ff;display:grid;place-items:center;min-height:100vh;margin:0}b{color:#ff8b39;font-size:48px}</style></head>
<body><main><h1>You're in line for checkout</h1><p>Your position: <b id="position">%d</b></p>
<script>setInterval(()=>fetch('/queue/status').then(r=>r.json()).then(s=>{if(!s.position)location.reload();else document.getElementById('position').textContent=s.position}),3000)</script>
</main></body></html>`
