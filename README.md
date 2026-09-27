# Lemmings

**Send users through your website. Find out where their experience breaks.**

Lemmings is a Go load tester with stateful HTTP sessions, paced navigation,
assertable user journeys, a live dashboard, Prometheus metrics, and optional real
Chromium verification. It records what each session encountered—not just how many
requests the generator could send.

An HTTP 200 can contain an error page. A fast response can render a blank screen.
Lemmings lets you measure those failures separately from throughput and latency.

[Quick start](#quick-start) · [Journeys](#journeys) · [Real browser checks](#real-browser-checks)
· [Flags](#flags) · [Measurement limits](UPGRADE.md#evidence-limits)

## What you get

| Evidence | HTTP swarm | Chromium cohort |
|---|---|---|
| Stateful users | Isolated cookie jars, stable UA/language, referrers | Isolated browser contexts, desktop/narrow touch viewports |
| Navigation | Same-origin links, random pool, or ordered journey | Same-origin links or the same ordered journey |
| Response codes | Final code and every redirect/queue response | Document and resource response codes |
| Content checks | Raw response content, title, tag/ID/class existence | Visible body content, title, visible tag/ID/class |
| Page execution | HTML response parsing only | JavaScript execution, visible content, failed images/resources |
| Timing | DNS/TCP/TLS, final-hop TTFB, total visit duration | Navigation, paint and layout observations |
| Diagnosis | Session tails, failed assertions, transport categories | Console/JS errors, resource failures, failure screenshots |
| Output | HTML, Markdown, JSON; optional JSONL | Combined report plus browser JSONL, JSON and PNGs |

No screenshot comparison or HTTP parser can establish every aspect of visual
correctness here. Configure assertions for the content and elements that matter.

## Quick start

Requires Go compatible with the supplied `go.mod` (Go 1.25 or newer). The regular
HTTP engine requires no Node or browser installation.

```sh
go build -trimpath -o bin/lemmings .

# Terminal 1: bundled local demonstration website
go run ./examples/demo

# Terminal 2: six users, three at a time, with a finite browsing journey
./bin/lemmings \
  -hit http://127.0.0.1:8080/ \
  -terrain 2 -pack 3 -limit 3 \
  -ramp 1s -until 15s \
  -scenario examples/browse.json \
  -save-to ./runs/first \
  -trace-file ./first-visits.jsonl
```

Open `http://localhost:4000` while the run is active. Enter the token printed at
startup. Reports are written beneath `runs/first/lemmings/<host>/`.

Use `-hit` to select your test environment and replace the example journey with
its paths and expected content.

## A lemming's life

A **swarm** contains **terrain groups**, each containing a **pack** of users.
`terrain × pack` is the planned session count. `-limit` caps simultaneously active
HTTP sessions; waiting sessions do not consume one goroutine each. Browser users
are an additional small cohort with their own limit.

Each HTTP user gets an isolated cookie jar and a stable browser user agent. It
starts at `-hit`, navigates a same-origin link from the preceding page, and pauses
for a randomized think time. At a dead end it chooses a URL from the indexed pool.
A session ends when its journey completes, its page limit is reached, its lifespan
expires, or the run is cancelled.

Indexing tries the standard sitemap, a same-origin robots.txt sitemap, optional
crawling, and finally the target URL. Indexing traffic is not included in measured
visits. Nested sitemaps are cycle-checked and bounded.

Default behavior pauses **300–1,200 ms** between visits and ends after **20 pages
or 30 seconds**. The existing default population is 50 terrains × 50 users, with a
100-user concurrency ceiling and a five-minute ramp. Start with a smaller explicit
population when developing a journey.

For an unpaced saturation run:

```sh
./bin/lemmings -hit http://127.0.0.1:8080/ \
  -terrain 2 -pack 10 -limit 20 -ramp 1s -until 20s \
  -think-min 0 -think-max 0 -max-pages 0 -navigation random
```

Failed visits still pause at least 100 ms. HTTP 429/503 `Retry-After` is respected
up to five minutes, bounded by the remaining session lifespan.

## Journeys

A journey is a finite sequence of GET requests. All paths must stay on the target
origin. No forms are submitted implicitly.

```json
{
  "name": "read-product",
  "steps": [
    {
      "name": "landing",
      "path": "/",
      "expect": {
        "status": [200],
        "content_type": "text/html",
        "title_contains": "Store",
        "contains": ["Welcome"],
        "not_contains": ["Something went wrong"],
        "selectors": ["main", "#app", ".navigation"]
      }
    },
    {
      "name": "product",
      "path": "/products/boots",
      "expect": { "status": [200], "contains": ["In stock"] }
    }
  ]
}
```

Use `-scenario journey.json`. Unknown JSON fields fail validation, helping catch
misspelled assertions. Selectors deliberately support only a tag, `#id`, or
`.class`, giving both engines the same contract. HTTP checks element existence;
Chromium checks visibility. HTTP `contains` checks raw HTML; Chromium checks body
text after execution. For text with HTML entities, use assertions appropriate to
both representations or separate scenarios.

Without an explicit status assertion, final 2xx is successful. A 404 can be an
expected result when explicitly listed. Request errors and failed assertions are
reported even when another check passed.

Body checksums remain available as a diagnostic. Dynamic HTML does not fail just
because it differs from its indexed body. `-strict-checksum=true` enables that
stronger requirement, including requiring an indexed baseline.

## Real browser checks

The optional runner uses Node 20+ and the pinned Playwright dependency:

```sh
npm ci --prefix browser
npm --prefix browser run install-browser
npm --prefix browser run check

./bin/lemmings -hit http://127.0.0.1:8080/ \
  -terrain 2 -pack 3 -limit 3 -ramp 1s -until 15s \
  -scenario examples/browse.json \
  -browser-users 2 -browser-until 15s \
  -browser-output ./runs/with-browser/browser \
  -save-to ./runs/with-browser
```

Run from the repository root, or pass an absolute `-browser-script` path. To use
an already installed compatible Chromium binary, set `LEMMINGS_CHROMIUM_PATH`.
On Linux, Playwright may require its documented system packages
(`npx --prefix browser playwright install --with-deps chromium`).

The CLI checks browser availability before indexing, waits for the browser runner
to become ready, and starts the HTTP workload. It waits for both cohorts to finish.
The browser uses separate sessions and does not share the Go users' cookies.
Its traffic is additional; it is never counted as Go HTTP traffic.

Browser checks detect missing visible content, missing expected elements,
uncaught JavaScript exceptions, console errors, failed resource loads, HTTP errors
on resources, and completed images that failed to decode. Failures save viewport
screenshots in a unique run directory alongside `visits.jsonl` and `summary.json`.

**Scope:** external requests are blocked and recorded as failed dependencies.
A CDN-dependent page can therefore fail in this mode. Browser routing disables
its HTTP cache; this cohort is useful for cold-load correctness checks, not a
production cache-hit distribution. Service workers are blocked. Measurements
observe a 500 ms settle window after DOM readiness/selector checks. LCP/CLS are
snapshots within that window, not full Web Vitals; INP is not measured.

To see failures intentionally:

```sh
./bin/lemmings -hit http://127.0.0.1:8080/ \
  -terrain 1 -pack 2 -limit 2 -ramp 1s -until 15s \
  -request-timeout 1s -scenario examples/broken.json \
  -browser-users 1 -browser-until 15s \
  -max-failure-rate 0 -save-to ./runs/broken
```

## Read the results

The live dashboard shows current visits, bytes, status classes, failures,
cancellations and session counts. These update during user lifetimes.

The final HTML report expands each retained session into a chronological visit
tail with page names, statuses, durations and failed checks. The JSON companion
contains structured timing, cookies **by name only**, hop statuses, check results
and browser evidence. Markdown summarizes the same findings for email or review.

- **Failed visit:** request error or unmet status/content assertion.
- **Cancelled visit:** an in-flight request interrupted by session/run cancellation;
  excluded from failure-rate and latency calculations.
- **Status 0:** no HTTP response received.
- **Checksum change:** informational unless strict mode is enabled.
- **Session started / not started:** actual versus planned population. Sessions
  waiting when cancelled are not presented as users who completed a journey.
- **Dropped lifelog / trace record:** incomplete detailed evidence. Streamed visit
  aggregates remain complete; the run fails its evidence-integrity gate.

Every visit contributes to aggregates. Memory retains at most 100 session tails
of 100 visits each, 1,000 distinct path buckets plus `other`, and 4,096 exact latency
values per series. Larger latency series use a bounded histogram with at most 5%
upper-bucket error above 1 microsecond. The report names this method explicitly.
TTFB always uses the histogram. Browser reports embed the most recent 200 visits;
the browser JSONL contains every visit.

Enable `-trace-file` to retain every HTTP visit. Its buffer is bounded and reports
any lost records. Existing trace files are never overwritten. URLs in evidence
omit userinfo, query strings and fragments. Response bodies, cookie values and
request headers are not stored. URL paths, titles, browser error messages and
screenshots may still contain application data.

This is a closed-loop load model: users wait for responses before continuing.
Slow responses reduce request arrival rate. See [measurement limits](UPGRADE.md#evidence-limits)
before treating this as a constant-arrival-rate benchmark.

## CI budgets and shutdown

```sh
./bin/lemmings -hit http://127.0.0.1:8080/ \
  -terrain 1 -pack 10 -limit 10 -ramp 1s -until 20s \
  -scenario examples/browse.json \
  -max-failure-rate 0.01 -p95-budget 750ms \
  -tty=false -save-to ./runs/ci
```

| Exit code | Meaning |
|---|---|
| 0 | Run and report delivery succeeded; enabled gates passed |
| 1 | Configuration, startup, or report delivery error |
| 2 | Failure-rate/p95 budget, browser verification, or evidence-integrity gate failed |
| 130 | Interrupted; collected evidence was reported before exit |

Use `-max-failure-rate 0` for zero tolerance, `-1` to disable that budget. Browser
failures fail their gate whenever a browser cohort is enabled. Reports are written
before a failing gate returns its exit code. Ctrl-C cancels traffic, drains
collected results and gives delivery a separate one-minute timeout.

## Dashboard, Prometheus and delivery

The token-protected dashboard binds to localhost on `-dashboard-port` (4000).
The existing SSE event stream, metrics JSON and replay endpoints remain.

```sh
./bin/lemmings -hit http://127.0.0.1:8080/ \
  -terrain 1 -pack 5 -limit 5 -ramp 1s -until 20s \
  -observe=true -metrics-port 9090 -metrics-url path
```

Prometheus exposes `/metrics`, including live session gauges, status classes,
visit duration, bytes, waiting rooms, queue pressure, failed page visits and
cancelled page visits. URL labels have a cardinality cap.

`-save-to` accepts comma-separated destinations: local paths, `file://` paths,
`s3://bucket/prefix`, and `mailto:` destinations. `LEMMINGS_SAVE_TO` adds destinations.
Local and S3 outputs include Markdown, HTML and JSON. SMTP retains the existing
Markdown/HTML mail delivery. Browser PNGs/JSONL and HTTP trace files stay in their
configured local directories; they are not automatically attached or uploaded.

S3 uses the existing AWS SDK credential chain. SMTP supports the existing flag
and `LEMMINGS_SMTP_*` environment overrides. Delivery to other configured targets
continues when one target fails; any delivery error yields exit 1.

Report names retain `lemmings.YYYY.MM.DD.<host>.*`. **Choose a separate output
directory per run** if you want to retain multiple runs on the same day.

## Flags

Existing flags and aliases:

| Flag | Default | Purpose |
|---|---|---|
| `-hit`, `-h` | `http://localhost:8080/` | Target URL |
| `-terrain`, `-t` | 50 | Terrain groups |
| `-pack`, `-p` | 50 | Users per group |
| `-limit`, `-l` | 100 | Active HTTP sessions; `-1` removes ceiling |
| `-until`, `-u` | 30s | Lifespan of each HTTP session |
| `-ramp`, `-r` | 5m | Terrain launch ramp |
| `-crawl`, `-c` | false | Crawl if sitemap discovery fails |
| `-crawl-depth`, `-cd` | 3 | Maximum crawl depth |
| `-save-to`, `-st` | `.` | Report destinations |
| `-dashboard-port`, `-dp` | 4000 | Local dashboard |
| `-tty` | true | Rewrite terminal progress line |
| `-observe`, `-o` | false | Prometheus exporter |
| `-metrics-port`, `-mp` | 9090 | Exporter port |
| `-metrics-url`, `-mul` | path | `full`, `path`, or `none` |
| `-version`, `-v` | false | Print supplied version |
| `-smtp-host`, `-sho` | empty | SMTP host override |
| `-smtp-port`, `-spo` | 0 | SMTP port override |
| `-smtp-from`, `-sfr` | empty | Sender override |
| `-smtp-user`, `-sus` | empty | SMTP user override |
| `-smtp-pass`, `-spa` | empty | SMTP password override |

Experience flags:

| Flag | Default | Purpose |
|---|---|---|
| `-think-min` / `-think-max` | 300ms / 1.2s | Randomized pause bounds |
| `-request-timeout` | 10s | Independent request timeout |
| `-max-body-bytes` | 2097152 | Decoded HTTP body cap; max 64 MiB |
| `-max-pages` | 20 | Per-session page limit; 0 uses lifespan |
| `-navigation` | links | `links` or `random` |
| `-strict-checksum` | false | Require exact indexed body match |
| `-scenario` | empty | Ordered GET journey JSON |
| `-trace-file` | empty | New HTTP JSONL trace file |
| `-max-failure-rate` | -1 | Fraction 0..1; -1 disables |
| `-p95-budget` | 0 | HTTP visit p95 budget; 0 disables |
| `-browser-users` | 0 | Additional Chromium users, up to 32 |
| `-browser-until` | 30s | Browser-session lifespan |
| `-browser-script` | `browser/runner.cjs` | Runner path |
| `-browser-output` | `browser-results` | Browser evidence directory |

## Develop and verify

```sh
make all                   # vet, format, race-tested Go suite, build
npm --prefix browser test  # real Chromium regression fixture
python3 scripts/smoke.py   # actual CLI, reports, CI gates and cancellation
python3 scripts/smoke.py --browser --output ./smoke-results
```

The browser fixture checks healthy and broken HTML, session continuity, JavaScript
exceptions, broken resources, blank content and screenshots. The smoke test checks
exact known visit/failure counts and actual exit codes against the local demo.

See [UPGRADE.md](UPGRADE.md) for the repair list, compatibility changes and precise
limits. Older design-review documents remain in the source as historical context.

Implementation references: [Go HTTP tracing](https://pkg.go.dev/net/http/httptrace),
[Playwright browser contexts](https://playwright.dev/docs/browser-contexts),
[Playwright page events](https://playwright.dev/docs/api/class-page), and
[Playwright network routing](https://playwright.dev/docs/api/class-browsercontext#browser-context-route).

## License

Apache License 2.0. See [LICENSE](LICENSE).
