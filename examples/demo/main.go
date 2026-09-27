// Neon Arcade is a local demo target for lemmings. It has healthy pages,
// a slow page, a flaky page, a soft error, a blank page, a broken link, a
// redirect and a waiting room, so every part of the dashboard and report
// has something to show.
//
//	go run ./examples/demo                   # http://127.0.0.1:8080
//	go run ./examples/demo -chaos 0          # everything healthy
//	go run ./examples/demo -chaos 1 -room 3  # more failures, a busier queue
//
// It only listens on the loopback interface.
package main

import (
	"flag"
	"fmt"
	"html"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// games are the arcade's catalog pages.
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

	a := &arcade{chaos: *chaos, capacity: *room}
	a.room.served.Store(int64(*room))
	go a.admit()

	log.Printf("Neon Arcade is open at http://%s (chaos %.2f, waiting room capacity %d)", *addr, *chaos, *room)
	srv := &http.Server{Addr: *addr, Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// arcade holds the demo's state.
type arcade struct {
	chaos    float64
	capacity int
	visitors atomic.Int64
	room     waitingRoom
}

// waitingRoom hands every checkout visitor a ticket and admits tickets in
// order as the room serves them, like the room package does.
type waitingRoom struct {
	mu      sync.Mutex
	tickets map[string]int64
	next    atomic.Int64
	served  atomic.Int64
}

// admit serves one more ticket every 400ms.
func (a *arcade) admit() {
	for range time.Tick(400 * time.Millisecond) {
		if a.room.served.Load() < a.room.next.Load()+int64(a.capacity) {
			a.room.served.Add(1)
		}
	}
}

// broken reports whether a failure with base probability p should happen.
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
		w.WriteHeader(http.StatusNotFound)
		a.page(w, r, "Not found", `<h1>404</h1><p>That cabinet was unplugged in 1987.</p>`)
	})
	mux.HandleFunc("GET /scores", func(w http.ResponseWriter, r *http.Request) {
		// The leaderboard is slow: it adds up every score ever played.
		time.Sleep(time.Duration(150+rand.IntN(int(600*a.chaos)+50)) * time.Millisecond)
		if a.broken(0.06) {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			a.page(w, r, "Leaderboard", `<h1>503</h1><p>The leaderboard is recalculating. Try again shortly.</p>`)
			return
		}
		a.page(w, r, "Leaderboard", `<h1>Leaderboard</h1><ol><li>AAA — 999,999</li><li>LEM — 812,440</li><li>RAD — 777,777</li></ol>`)
	})
	mux.HandleFunc("GET /tokens", func(w http.ResponseWriter, r *http.Request) {
		if a.broken(0.12) {
			// A soft error: 200 OK with an error page.
			a.page(w, r, "Tokens", `<h1>Something went wrong</h1><p>Token machine jammed.</p>`)
			return
		}
		a.page(w, r, "Tokens", `<h1>Tokens</h1><p>10 tokens for $5. Welcome back, player one.</p>`)
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
		a.page(w, r, "About", `<h1>About</h1><p>Neon Arcade is a demo target for lemmings. Nothing here is real, including the high scores.</p><p><a href="/old-arcade">The old arcade</a> · <a href="/blank">The back room</a> · <a href="/logout">Log out</a></p>`)
	})
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		// Lemmings never follow log-out links; if one gets here, it is a bug.
		http.SetCookie(w, &http.Cookie{Name: "player", Value: "", Path: "/", MaxAge: -1})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		base := "http://" + r.Host
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
		for _, p := range []string{"/", "/games", "/scores", "/tokens", "/about"} {
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

// session gives every new visitor a player cookie, like a real site.
func (a *arcade) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("player"); err != nil {
			n := a.visitors.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "player", Value: "p" + strconv.FormatInt(n, 36), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		}
		next.ServeHTTP(w, r)
	})
}

// queuePosition returns the visitor's place in line, or 0 once admitted.
func (a *arcade) queuePosition(w http.ResponseWriter, r *http.Request) int64 {
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

// page writes a styled page.
func (a *arcade) page(w http.ResponseWriter, r *http.Request, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, pageHTML, html.EscapeString(title), body)
}

const pageHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s · Neon Arcade</title><style>
body{margin:0;min-height:100vh;font:17px/1.6 system-ui,sans-serif;color:#f7f1ff;background:linear-gradient(#171520,#2a1747 60%%,#4b1d52)}
nav{display:flex;gap:18px;padding:18px 28px;border-bottom:1px solid #ff7edb44}nav a{color:#36f9f6;text-decoration:none}
main{max-width:860px;margin:0 auto;padding:36px 28px}h1{font-style:italic;letter-spacing:.06em;color:#ff7edb;text-shadow:0 0 8px #f92aad}
a{color:#fede5d}.lead{color:#b893ce}.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:14px}
.tile{display:block;padding:16px;border:1px solid #36f9f666;border-radius:12px;text-decoration:none;color:#f7f1ff}.tile b{display:block;color:#36f9f6}
</style></head><body><nav><a href="/">Neon Arcade</a><a href="/games">Games</a><a href="/scores">Leaderboard</a><a href="/tokens">Tokens</a><a href="/checkout">Checkout</a><a href="/about">About</a></nav>
<main>%s</main></body></html>`

// queueHTML mimics the room package's waiting room: lemmings recognise it
// by the /queue/status poll URL and read the position element.
const queueHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Waiting room · Neon Arcade</title>
<style>body{font:18px system-ui;background:#241b2f;color:#f7f1ff;display:grid;place-items:center;min-height:100vh;margin:0}b{color:#ff8b39;font-size:48px}</style></head>
<body><main><h1>You're in line for checkout</h1><p>Your position: <b id="position">%d</b></p>
<script>setInterval(()=>fetch('/queue/status').then(r=>r.json()).then(s=>{if(!s.position)location.reload();else document.getElementById('position').textContent=s.position}),3000)</script>
</main></body></html>`
