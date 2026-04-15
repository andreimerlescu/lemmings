# Lemmings

> Simulating real-world NPC traffic during high-load events. Inspired by a beloved childhood [videogame](https://en.wikipedia.org/wiki/Lemmings_(video_game)).

[![Go Reference](https://pkg.go.dev/badge/github.com/andreimerlescu/lemmings.svg)](https://pkg.go.dev/github.com/andreimerlescu/lemmings)
[![Go Report Card](https://goreportcard.com/badge/github.com/andreimerlescu/lemmings)](https://goreportcard.com/report/github.com/andreimerlescu/lemmings)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

---

Your company is about to spend $50,000 on a TV commercial. Traffic is going to
spike the moment it airs. Does your application actually survive that?

Most teams find out the answer at 8PM on a Tuesday in front of their entire
customer base.

Lemmings lets you find out before that moment, for free, from your own machine.

---

## What Lemmings Is

Lemmings is a **stateful, session-aware, navigating load simulator** written in Go.

It is not a request blaster. Tools like wrk, vegeta, and k6 fire dumb HTTP
requests at a fixed rate. Lemmings does something different: it simulates real
users. Each lemming is a virtual browser session with its own cookie jar, its
own identity, its own lifespan, and its own navigation behaviour. It lands on
your site, picks a link, follows it, dwells on the page, picks another link,
and keeps going until its time runs out.

The result is a load test that reveals what actually happens to your application
when real people use it — not what happens when a script hammers one endpoint
in a tight loop.

---

## The Geographic Metaphor

Lemmings organises concurrency using a three-tier geographic model that maps
directly to how engineers think about real-world traffic events.

**Terrain** is the top-level goroutine group. Think of it as a region or state.
You control how many terrains are active with -terrain.

**Pack** is the number of lemmings per terrain. Think of it as the population
density of each region. You control pack size with -pack.

**Limit** is the semaphore ceiling — the system protection valve that prevents
terrain * pack goroutines from all spawning simultaneously on a machine that
cannot support them. You control it with -limit.

A systems engineer planning for a product launch can say: "I expect traffic
from 50 regions, with 100 concurrent users per region, sustained for 30 seconds
per session." That translates directly to:

    lemmings -hit https://myapp.com -terrain 50 -pack 100 -until 30s -ramp 5m

The total load — 5,000 lemmings, each making multiple page visits during their
30-second lifespan — is predictable, auditable, and reproducible.

---

## What a Lemming Does

Each lemming lives for exactly the duration you specify with -until. During that
time it:

1. Picks a random URL from the indexed pool of your application's pages
2. Makes an HTTP GET request with a randomised but realistic browser User-Agent
3. Reads the response body and computes its SHA-512 checksum
4. Compares the checksum against what was indexed at boot time
5. Records the status code, bytes transferred, duration, and checksum match
6. If it encounters a waiting room, it waits patiently — recording its queue
   position and the duration it was held — until it is admitted or its time runs out
7. Picks the next URL and repeats until its context deadline expires
8. Dies, sending its complete LifeLog to the results collector

Every lemming has its own isolated cookie jar. Session cookies set for one
lemming never leak into another. This is the behaviour of real users, not
the behaviour of a shared HTTP client.

---

## URL Discovery

Before a single lemming moves, lemmings indexes your application's URL surface
using a three-strategy waterfall:

**Strategy 1 — sitemap.xml.** If your application serves a sitemap at the
standard path, lemmings fetches it, parses all loc entries (including nested
sitemap index files), fetches each URL to compute its baseline checksum, and
builds the shared URLPool. This is the preferred strategy.

**Strategy 2 — robots.txt.** If no sitemap is found at the standard path,
lemmings checks robots.txt for a Sitemap: directive and follows it.

**Strategy 3 — crawl.** If neither sitemap strategy yields results and
-crawl is enabled, lemmings performs a depth-bounded crawl from the origin,
following anchor links and building the URL pool from what it discovers.
The crawl runs for a maximum of 5 minutes.

**Fallback.** If no strategy produces results, lemmings uses the origin URL
alone. The test still runs — it is just limited to one endpoint.

Every URL in the pool has a SHA-512 checksum computed at index time. A lemming
that receives a different body than expected records Match: false. A sustained
increase in mismatch rate during a load test is a signal that your application
is serving error pages or degraded content under pressure.

---

## Waiting Room Integration

Lemmings is built with first-class support for the
[room](https://github.com/andreimerlescu/room) package.

When a lemming receives a response containing the room package's waiting room
HTML, it detects this automatically — no configuration required. It extracts
the queue position from the page, records when it entered the queue, and polls
the same URL on the room package's 3-second interval until it is admitted or
its -until deadline expires.

The final report tells you:

- How many lemmings were placed in the waiting room
- What their queue positions were
- How long they waited before admission
- How many died in the queue without ever reaching your application

This is the data you need to tune room capacity for a real traffic event. Run
lemmings, read the report, adjust room parameters, run lemmings again. Repeat
until the wait duration is acceptable.

---

## The Ramp

Real traffic does not arrive all at once. A TV commercial airs, interest builds,
and traffic climbs over minutes before sustaining at peak. Lemmings models this
with -ramp.

When -ramp is set, lemmings brings terrain groups online linearly over the ramp
duration rather than spawning everything simultaneously. A 5-minute ramp with
50 terrain groups means one new terrain group comes online every 6 seconds,
each bringing its full pack of lemmings with it.

This lets you observe how your application behaves during the climb to peak load
— which is often where failures first appear — not just at the peak itself.

---

## The Live Dashboard

While lemmings runs, a live dashboard is available at http://localhost:4000.

When the swarm boots, a one-time authentication token is printed to the terminal.
Enter that token in the browser to unlock the dashboard. The token is a
cryptographically random 256-bit value. Its SHA-512 hash is stored in memory —
the raw token is never retained after printing.

The dashboard shows in real time:

- Lemmings alive and completed
- Terrains online
- Total visits and bytes transferred
- 2xx, 3xx, 4xx, and 5xx response counts
- Waiting room hits
- Overflow and dropped log counts with warnings when present
- A live event stream showing lemming births, deaths, and waiting room events

Engineers who open the dashboard mid-run see recent event history replayed
from the last 1,000 events — the view is never empty on a cold join.

---

## The Report

When the swarm completes, lemmings writes two report files to disk:

    lemmings.YYYY.MM.DD.domain.md
    lemmings.YYYY.MM.DD.domain.html

The HTML report is fully self-contained — no external stylesheets, no CDN
dependencies, no internet connection required to open it. It can be archived,
emailed, or attached to an incident ticket and will render correctly anywhere.

Both reports contain:

- Full configuration summary
- Total lemmings, visits, and bytes
- Response code breakdown (2xx / 3xx / 4xx / 5xx)
- Timing percentiles: fastest, p50, p90, p95, p99, slowest
- Per-path breakdown with individual hit counts, byte totals, and percentiles
- Waiting room summary
- Dropped and overflow log counts with remediation guidance

The default save location is the current directory. Override it with -save-to
or the LEMMINGS_SAVE_TO environment variable. S3 URI prefixes are supported
for teams that archive reports centrally.

---

## Quick Start

    go install github.com/andreimerlescu/lemmings@latest

Run a basic test against a local server:

    lemmings -hit http://localhost:8080/ -terrain 10 -pack 10 -until 30s

Run with a ramp-up period:

    lemmings -hit https://myapp.com -terrain 50 -pack 50 -until 30s -ramp 5m

Run with crawl enabled for sites without a sitemap:

    lemmings -hit https://myapp.com -terrain 10 -pack 10 -until 30s -crawl

Run in CI with TTY disabled so output is line-per-second rather than overwrite:

    lemmings -hit https://staging.myapp.com -terrain 5 -pack 5 -until 10s -tty=false

---

## All Flags

| Flag | Alias | Default | Description |
|---|---|---|---|
| -hit | -h | http://localhost:8080/ | Origin URL to load test |
| -terrain | -t | 50 | Number of terrain goroutine groups |
| -pack | -p | 50 | Number of lemmings per terrain |
| -limit | -l | 100 | Semaphore ceiling. -1 disables it entirely (dangerous) |
| -until | -u | 30s | How long each lemming lives |
| -ramp | -r | 5m | Duration to bring all terrains online |
| -crawl | -c | false | Crawl origin for links when no sitemap is found |
| -crawl-depth | -cd | 3 | How many links deep to crawl |
| -save-to | -s | . | Local path or s3:// URI for report output |
| -dashboard-port | -dp | 4000 | Port for the live dashboard |
| -tty | | true | Use carriage return for live output. Set false for CI |

---

## Understanding the Output

The boot summary printed before any lemming moves tells you exactly what is
about to happen:

    lemmings v1.0.0
    ─────────────────────────────────────────
      target:        https://myapp.com/

      terrain:       50 groups
      pack:          50 lemmings per terrain
      total:         2,500 lemmings

      until:         30s per lemming
      ramp:          5m0s to full concurrency
      est. duration: ~5m30s wall clock

      limit:         100 goroutines
      dashboard:     http://localhost:4000

    ─────────────────────────────────────────
      lemmings are gathering...

The live ticker updates every second:

    [1m23s] terrains: 14 | alive: 700 | done: 12 | visits: 4,821 | 2xx: 4,788 4xx: 22 5xx: 11

The final summary closes the run:

    ─────────────────────────────────────────
      lemmings v1.0.0 — final summary
    ─────────────────────────────────────────
      target:         https://myapp.com/
      total lemmings: 2,500
      completed:      2,500
      failed:         0

      total visits:   41,337
      total bytes:    1.24 GB

      2xx:            40,892
      3xx:            211
      4xx:            187
      5xx:            47

      waiting room:   312 lemmings held
    ─────────────────────────────────────────
      report: ./lemmings/myapp.com/

---

## When to Use Lemmings

**Before a major traffic event.** A product launch, a sale, a TV spot, a viral
social post. Run lemmings with parameters that match your expected traffic shape.
If it completes with dropped_logs: 0, 2xx rates above your threshold, and p99
latency below your SLA, you are ready. If it does not, you have time to fix it.

**For waiting room capacity planning.** If you use the room package, lemmings
tells you exactly how many users will see the waiting room at a given traffic
volume, how long they will wait, and how many will give up. Tune room parameters
and re-run until the numbers are acceptable.

**For infrastructure sizing.** Increase -terrain and -pack until the first 5xx
responses appear. That is your capacity ceiling. Add infrastructure and re-run
to verify the ceiling moved.

**As a daily CI health check.** Run lemmings with a small -terrain and -pack
against staging on every deploy. A change in 2xx rate, checksum match rate, or
p99 latency is an early signal of regression before customers see it.

**For post-incident analysis.** Reconstruct the traffic parameters from your
incident logs and re-run. The per-path breakdown shows which routes degraded
first and under what conditions.

---

## How Results Can Be Trusted

Lemmings ships with a comprehensive test suite that proves the accuracy of
every number in every report. See [TESTS.md](TESTS.md) for the full breakdown.

The short version:

- Metric counters are protected by atomic operations verified under the race
  detector. The 2xx count in your report is exact, not approximate.

- Percentiles are computed using the nearest-rank method on a fully sorted
  slice, verified against analytically known datasets.

- The dual-channel result collection system (primary + overflow) ensures that
  no LifeLog is silently discarded. If any data was not collected, dropped_logs
  in the report will be non-zero with an explicit remediation message.

- Waiting room detection, position extraction, and duration timing are all
  verified against real room package HTML, not against a mock.

- Each lemming's cookie jar isolation is structurally verified — session state
  never crosses between virtual users.

---

## Dependencies

| Package | Purpose |
|---|---|
| [figtree](https://github.com/andreimerlescu/figtree) | CLI configuration management with validators and aliases |
| [sema](https://github.com/andreimerlescu/sema) | Semaphore primitive for goroutine ceiling enforcement |
| [room](https://github.com/andreimerlescu/room) | Waiting room integration (optional — detected automatically) |
| golang.org/x/net/html | HTML link extraction during crawl |
| golang.org/x/net/publicsuffix | Cookie jar domain scoping |

---

## v1.1.0 Roadmap

The Navigator interface is designed for extension. The following targets are
planned for v1.1.0:

- **MySQL** — connect via DSN, discover tables, issue read and write queries
- **PostgreSQL** — same as MySQL with postgres-specific query patterns
- **MongoDB** — discover collections, issue find and insert operations
- **SQLite** — local database load testing for embedded applications
- **Prometheus** — plug the EventBus into a metrics exporter directly
- **S3 report upload** — fully implemented upload, not just path resolution

---

## Contributing

Lemmings is open source under the Apache 2.0 license. Contributions are welcome.

Before opening a pull request:

- Run go test -race ./... and confirm all tests pass
- Add tests for any new functions following the patterns in the existing test files
- Read TESTS.md to understand what the test suite is trying to prove and why

The test suite is not optional. Every function in the package has a corresponding
test that verifies its contract. New functions without tests will not be merged.

---

## License

Apache 2.0. See [LICENSE](LICENSE).

---

Built with care for the engineers who find out their application crashes
the hard way — and for the ones who use lemmings so they never have to.​​​​​​​​​​​​​​​​
