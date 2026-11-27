# Lemmings vs Apache Benchmark (`ab`): The Modern Drop-In Replacement for Real Web & API Load Testing

[![GitHub Repo stars](https://img.shields.io/github/stars/andreimerlescu/lemmings?style=social)](https://github.com/andreimerlescu/lemmings)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-36f9f6)](../../LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)

> **TL;DR Verdict:** **Lemmings is the decisively better load testing tool than Apache Benchmark (`ab`) for modern websites, web apps, and HTTP APIs.** While `ab` is a single-threaded, HTTP/1.0-era static-URL blaster with zero cookies, zero redirects, zero assertions, and zero failure visibility, **Lemmings v2.0.1** ships as a single 20–23 MB Apache-2.0 Go binary that simulates stateful multi-page visitors, runs weighted YAML user personas, captures automatic 4-part Chrome DevTools Protocol (CDP) error screenshots + JS console traces (with zero Node.js), serves a live Admin Triage Portal, and enforces deterministic CI regression gates (`exit 2`) with `lemmings compare`.
>
> ⭐ **Like data-driven load testing with visual receipts?** [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to help more developers and AI agents discover it.

| Quick Decision Summary | **Lemmings (`v2.0.1`)** | **Apache Benchmark (`ab`)** |
| --- | --- | --- |
| **Best For** | Realistic stateful web apps, APIs, multi-step funnels, CI gates, visual error forensics | Quick one-off single-URL raw HTTP/1.0 smoke checks on a local socket |
| **Traffic Model** | Stateful visitors with per-lemming cookies, link crawling, multi-persona YAML plans + 5% rogue cohort | Stateless single-URL loop repeating one fixed request |
| **When Something Breaks** | Automatic screenshot + JS console + cookie jar + parsed error body + Admin Triage Portal | A `Non-2xx responses` counter and nothing else |
| **Install in 10 Seconds** | `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest` | `apt install apache2-utils` / bundled macOS binary |

---

## Side-by-Side Feature Comparison (`Lemmings` vs `ab`)

| Capability | Lemmings (`v2.0.1`) | Apache Benchmark (`ab`) |
| --- | :---: | :---: |
| Permissive Apache-2.0 open-source license | ✅ | ✅ |
| Single standalone binary (no JVM / Python / Node.js) | ✅ (`20–23 MB` Go binary) | ✅ (C binary, requires APR libs) |
| Multi-core concurrency across modern CPUs | ✅ (Go goroutines + bounded terrains) | ❌ (single-threaded event loop) |
| Modern HTTP/1.1 & HTTP/2 + explicit TLS 1.2+ | ✅ | ❌ (HTTP/1.0 default; no HTTP/2) |
| Per-visitor stateful RFC 6265 cookie jars (`InspectingJar`) | ✅ | ❌ (static `-C` string only) |
| Multi-page site crawling & link navigation (`Referer` trail) | ✅ (`-crawl`, `-navigation links`) | ❌ (single static URL only) |
| Automatic redirect following (`301`/`302`/`308`) | ✅ (smart auto-off for `3xx` checks) | ❌ (never follows redirects) |
| Multi-persona weighted YAML plans (`GET`..`OPTIONS`) | ✅ (up to 100 weighted personas) | ❌ |
| Built-in `5%` rogue wanderer cohort (`-rogue-rate`) | ✅ | ❌ |
| Response assertions (status, headers, cookies, body, DOM) | ✅ (`want-*` + automatic `not-blank`) | ❌ (bytes-length diff only) |
| Automatic `200 OK` soft-error detection | ✅ | ❌ (counts broken `200 OK` as healthy) |
| Automatic error screenshots (pure-Go CDP, zero Node.js) | ✅ | ❌ |
| JavaScript console & client-side render error capture (`-scouts`) | ✅ | ❌ |
| Live Admin Triage Portal (`Bug`/`Flaky`/`Expected`/`Ignored`) & `-review` | ✅ | ❌ |
| Ramp-up pacing curves (`linear`, `fibonacci`, `step`) | ✅ | ❌ (instant thundering herd) |
| Human think time & saturation mode (`-think-min 0 -think-max 0`) | ✅ | ❌ (saturation only) |
| Opt-in same-origin static asset tier (`-assets`) | ✅ (isolated from page SLOs) | ❌ |
| Native virtual waiting-room protocol (Concert queue separation) | ✅ | ❌ (queues corrupt latency & fail) |
| Self-contained HTML, GitHub Markdown & JSON (`schema 2.0`) reports | ✅ | ❌ (plain text / TSV gnuplot dump) |
| Built-in CI pass/fail gates (`exit 2`) & `lemmings compare` diffs | ✅ | ❌ |
| Isolated Prometheus `/metrics` exporter (`24` metrics) | ✅ | ❌ |
| Security hardened (SSRF dial guard, secret scrubbing, SBOM) | ✅ (`SEC-01`–`SEC-10`) | ❌ |
| AI agent plan-authoring runbook (`SKILL.md`) | ✅ | ❌ |

---

## Performance & Real-World Output Side by Side

Numbers without context lie. Below are **real verified production-grade runs from Lemmings (`counthis.app` and `toktail.app`)** contrasted against Apache Benchmark's documented runtime architecture and what `ab` actually measures—or misses—on the exact same targets.

### Real Verified Lemmings Run Data (`396` Stateful Visitors · `3m0s`)

| Metric | Lemmings on `counthis.app` *(Standard Web App)* | Lemmings on `toktail.app` *(Concert Queue + 503 Chaos)* | What `ab` Would Show on These Targets |
| --- | --- | --- | --- |
| **Load Configuration** | `33` terrains × `12` pack (`396` visitors), `-limit 434`, `90s` ramp, `90s` lifespan, `600ms–6s` think, `-crawl` | Identical: `33×12` (`396` visitors), `90s` ramp, `90s` lifespan, `600ms–6s` think, `-crawl` | Can only hit **1 single URL** (e.g., `/`); cannot crawl 10 paths, cannot ramp over 90s, cannot hold per-visitor sessions |
| **Visitors Completed / Rescued** | `396/396` completed · **`100.0%` rescued** (`384` peak alive) | `396/396` completed · **`89.1%` rescued** (`353/396` flawless, `384` peak alive) | No concept of visitor sessions or rescued users |
| **Page Visits & Throughput** | **`10,831` visits** across `10` paths · `60.2` visits/s avg (**`144` visits/s peak**) · `76.15 MB` | **`3,110` visits** across `4` paths · `17.3` visits/s avg (`77` visits/s peak) · `36.87 MB` | Single-threaded C loop hits 1 cached URL repeatedly; stalls on single core under TLS |
| **Origin Page Latency** | **p50 `35.8ms`** · p90 `123.8ms` · **p95 `185.4ms`** · p99 `607.8ms` · **TTFB p95 `144.5ms`** (`<= 0.5%` 4096-bin histogram) | **p50 `43.0ms`** · p90 `69.3ms` · **p95 `186.2ms`** · p99 `206.0ms` · **TTFB p95 `156.5ms`** *(cleanly separated from queue wait)* | Blends waiting-room responses, redirects, and errors into one coarse `50%/95%/99%` table; HTTP/1.0 keep-alive quirks skew tail latency |
| **Waiting Room & Queueing** | `0` held | **`1,093` held** (`729` admitted, `364` died in line, **`11,685` `/queue/status` polls**, max pos `359`, mean wait `24s`, `3,026` passes) | **Completely blind** — `ab` never polls `/queue/status` or carries `room_ticket`/`concert_admit` cookies; every queued request either counts as a fake `200 OK` or raw `Non-2xx` |
| **Error Forensics & Visual Receipts** | `0` errors · `0` dropped lifelogs | `313` HTTP `503`s → **`12` deduplicated 4-part artifacts** + **`12` Chrome CDP screenshots** (`301` deduped, `0` dropped), plus live JS console `404` caught on `/favicon.ico` | Prints `Non-2xx responses: 313`. **Zero screenshots, zero HTML body, zero cookie state, zero JS console output** |

### Runtime & Architecture Footprint Comparison

| Dimension | Lemmings (`v2.0.1`) | Apache Benchmark (`ab`) |
| --- | --- | --- |
| **Binary & Footprint** | `20–23 MB` static Go binary (`CGO_ENABLED=0`), zero external runtime dependencies | Tiny C binary (~100 KB), dynamically linked to APR / OpenSSL libraries |
| **Concurrency Scaling** | Multi-core Go runtime; bounded `32 KiB` 4096-bin histogram (`<= 0.5%` error from 10µs to >24h) | Single-threaded `poll`/`select`/`kqueue` loop; cannot utilize multi-core CPUs for TLS handshakes |
| **Memory Bounds** | Fixed bounds on cookies (`256`/lemming), dashboard sprites (`160`), exact samples (`8192`), and captures (`-capture-max`) | Low memory, but unbounded risk of `apr_socket_recv` crashes at high concurrency on macOS |
| **Saturation Capability** | Pass `-think-min 0 -think-max 0 -navigation random` for raw multi-core saturation across HTTP/1.1 & HTTP/2 | Raw single-core HTTP/1.0 burst only |

### What You Actually See When `toktail.app` Hits `503`s

On the `toktail.app` run, `ab` would have ended with a single line: `Non-2xx responses: 313`. You would not know whether the server crashed, returned a CDN error, or served a Concert waiting room—nor would you see the broken `favicon.ico` 404 in the browser console.

Lemmings automatically streamed all 12 deduplicated 4-part forensic dossiers (screenshot, cookie jar health `3 active, 0 expired`, visitor breadcrumb trail `/ -> /terms.html`, extracted HTML headline `Good things. Worth the wait.`, and JS console output) into the live Admin Portal and self-contained HTML/Markdown reports:

| Live Admin Triage Portal (`http://localhost:4000`) | Self-Contained HTML Report with Embedded Screenshots |
| :---: | :---: |
| ![Lemmings Admin Portal — Dark Mode](../images/dashboard-dark.jpg) | ![Lemmings HTML Report](../images/report.jpg) |

---

## Conventions Translation: How `ab` Flags Map to Lemmings

Switching from `ab` to Lemmings takes less than a minute. Every `ab` flag maps cleanly to an explicit, safer Lemmings flag or YAML `-plan` directive:

| Apache Benchmark (`ab`) Convention | Lemmings (`v2.0.1`) Equivalent | Why Lemmings Is Better |
| --- | --- | --- |
| `ab -c 50` *(50 concurrent sockets)* | `-terrain 5 -pack 10 -limit 50` | Scales across all CPU cores with optional ramp pacing (`-ramp`) |
| `ab -n 1000` *(fixed request count)* | `-max-pages 20` per lemming or `-until 30s` | Models realistic user lifespans and page budgets instead of raw hit counts |
| `ab -t 30` *(time limit in seconds)* | `-until 30s -ramp 5s` | Separates ramp-up duration from individual visitor lifespan cleanly |
| `ab -k` *(HTTP Keep-Alive)* | Enabled automatically per lemming | Each visitor owns a persistent HTTP/1.1 & HTTP/2 connection pool like a real browser |
| `ab -r` *(don't exit on socket receive error)* | Default resilient transport tracking | Transport errors are categorized (`timeout`, `tls`, `eof`, `dns`) and captured in forensics without aborting the run |
| `ab -H "Authorization: Bearer ..."` | YAML `with-headers` with `"Bearer ${API_TOKEN}"` | Never leaks tokens into `ps aux`, shell history, reports, or traces (`SEC-10`) |
| `ab -C "session=abc"` | Automatic `InspectingJar` + YAML `with-cookies` | Dynamic per-user RFC 6265 cookies that update across login, redirects, and logout |
| `ab -p body.json -T application/json` | YAML `method: POST` + `with-body` or raw `body:` | Supports `GET`/`POST`/`PUT`/`PATCH`/`DELETE`/`HEAD`/`OPTIONS` in multi-step journeys |
| `ab -g out.tsv` *(gnuplot TSV)* | `-save-to ./reports` + `-trace-file visits.jsonl` | Automatic HTML, Markdown, JSON (`schema 2.0`), `.artifacts/` screenshots, and `0600` JSONL traces |
| *(None — manual bash `awk` parsing)* | `-max-failure-rate 0.01 -p95-budget 500ms` + `lemmings compare` | Native CI gates returning deterministic exit code `2` on breach or regression |

### Side-by-Side Example: Saturation Smoke Test

**Apache Benchmark (`ab`):**
```bash
# Single-threaded, HTTP/1.0, static URL only, no redirects, no assertions
ab -n 5000 -c 50 -k -H "Accept: text/html" https://staging.example.com/
```

**Lemmings (Saturation Mode — Multi-Core HTTP/2 + Crawling + CI Gates):**
```bash
lemmings \
  -hit https://staging.example.com/ \
  -terrain 5 -pack 10 -limit 50 \
  -until 30s -ramp 0s \
  -think-min 0s -think-max 0s \
  -crawl -navigation random \
  -max-failure-rate 0.01 \
  -p95-budget 500ms \
  -save-to ./reports
```

### Side-by-Side Example: Authenticated `POST` API Endpoint

**Apache Benchmark (`ab` — leaks token into `ps aux`, cannot verify JSON body or login first):**
```bash
ab -n 500 -c 20 -k \
  -p payload.json \
  -T "application/json" \
  -H "Authorization: Bearer super-secret-token" \
  https://staging.example.com/api/v1/items
```

**Lemmings (Strict SEC-10 YAML Plan — Zero Secret Leakage + Body/Header Assertions):**

Save as `api-items.yaml`:
```yaml
name: api-items-write
weight: 10
stop-on-fail: true
loop: true
plan:
  - name: create-item
    path: /api/v1/items
    method: POST
    chill: 0s
    with-headers:
      - key: Authorization
        value: "Bearer ${API_TOKEN}"
      - key: Content-Type
        value: application/json
      - key: Accept
        value: application/json
    with-body:
      - key: sku
        value: NEON-01
      - key: quantity
        value: 1
    want-status: [200, 201]
    want-content-type: application/json
    want-body:
      - contains: NEON-01
        not-contains: '"error"'
```

Validate safely with `-dry-run`, then execute:
```bash
export API_TOKEN="tok_staging_example"
lemmings -hit https://staging.example.com/ -plan api-items.yaml -dry-run
lemmings -hit https://staging.example.com/ -plan api-items.yaml \
  -terrain 4 -pack 5 -limit 20 -until 30s -ramp 5s \
  -rogue-rate 0 -min-plan-pass-rate 0.99 -p95-budget 300ms \
  -save-to ./reports
```

---

## Apache Benchmark (`ab`) Gotchas & Pitfalls — Solved Out of the Box by Lemmings

1. **The macOS `apr_socket_recv: Connection reset by peer (54)` Crash**
   - **`ab` pitfall:** Running `ab` with moderate concurrency (`-c 100+`) on macOS routinely aborts mid-run with `apr_socket_recv: Connection reset by peer (54)` or hits the low default `ulimit -n` file descriptor ceiling.
   - **How Lemmings solves it:** Built on Go's production `net/http` transport with per-lemming connection pools, explicit dial/TLS/header timeouts (`-request-timeout`), and categorized transport error accounting—so a single reset connection is recorded as a forensic artifact rather than aborting your test.
2. **Single-URL Cache Illusion**
   - **`ab` pitfall:** Hammering `https://example.com/` 10,000 times measures only your CDN edge cache or warm homepage template while leaving `/search`, `/checkout`, and `/account` completely untested.
   - **How Lemmings solves it:** Automatically indexes `robots.txt` / `sitemap.xml` or crawls same-origin HTML links (`-crawl`), follows links organically (`-navigation links`), and runs weighted multi-persona YAML suites alongside a deterministic `5%` rogue cohort (`-rogue-rate`).
3. **HTTP/1.0 Default & Zero HTTP/2 Support**
   - **`ab` pitfall:** `ab` speaks HTTP/1.0 by default (missing `Host`/keep-alive nuances unless `-k` is passed) and has no HTTP/2 support, misrepresenting modern CDNs, ALBs, and Go/Nginx origins.
   - **How Lemmings solves it:** Speaks modern HTTP/1.1 and HTTP/2 over TLS 1.2+ (`MinTLSConfig`) automatically with realistic desktop and mobile browser `User-Agent` and `Accept-Language` profiles.
4. **Silent Redirect & `200 OK` Soft-Error Blindness**
   - **`ab` pitfall:** `ab` never follows `301`/`302` redirects (so testing `/dashboard` just benchmarks the tiny `302 -> /login` response!) and treats any HTTP `200`—even a blank white screen or `"Database unavailable"` error page—as a 100% success.
   - **How Lemmings solves it:** Follows same-origin redirects by default (auto-disabling only when `want-status` expects exclusively `3xx`), enforces a built-in SPA-safe `not-blank` check on every HTML page, and verifies `want-title`, `want-selectors`, `want-headers`, `want-cookies`, and `want-body` (`contains`/`not-contains`).
5. **No Ramp-Up (Instant Thundering Herd)**
   - **`ab` pitfall:** Launches all `-c` connections at `t = 0ms`, slamming cold autoscalers and connection pools unrealistically and masking the exact concurrency knee where latency degrades.
   - **How Lemmings solves it:** Ramps terrains smoothly over `-ramp` using `-ramp-shape linear`, `fibonacci`, or `step`, while recording a per-second timeline of `alive`, `rps`, `p50`, `p95`, and `queued` visitors.
6. **Secrets Exposed in `ps aux` & Zero CI Regression Tooling**
   - **`ab` pitfall:** Passing `-H "Authorization: ..."` or `-C "session=..."` exposes credentials to every local process (`ps aux`) and shell history, and `ab` exits `0` even if 100% of responses returned HTTP `500`.
   - **How Lemmings solves it:** Expands `${ENV_VAR}` fail-closed from environment variables (tagged `json:"-"` under `SEC-10`), enforces `-max-failure-rate`, `-p95-budget`, and `-min-plan-pass-rate` with deterministic exit code `2`, and diffs candidate vs. baseline releases with `lemmings compare`.

---

## Common Load-Testing Problems & How Lemmings Resolves Them

| Real-World Load Testing Problem | Why It Happens | How Lemmings Solves It Out of the Box |
| --- | --- | --- |
| **"Everything returned `200 OK`, yet users saw a broken page"** | SSR frameworks and SPAs frequently render error banners or crash in client JS while returning HTTP `200`. | Built-in `not-blank` HTML check, `want-body` `not-contains` soft-error assertions, and `-scouts N` real headless Chromium visitors catching JS `console.error` & uncaught exceptions on `200 OK` pages. |
| **"We got 300 HTTP `503`s at minute 2, but can't reproduce what users saw"** | Load tools discard error HTML, cookie state, and browser visuals at scale. | Automatic 4-part Error Forensics (`-capture-max`, `-capture-per-error`) captures deduplicated Chrome CDP screenshots, JS console logs, cookie jar health, and parsed error bodies without stalling the HTTP hot path. |
| **"Virtual waiting rooms or rate limiters wrecked our p95 numbers"** | Queue polls and `24s` waits get mixed into origin response times, or starved users are silently dropped as "cancelled". | Native Concert waiting-room protocol separates origin page latency (`186.2ms` p95 on `toktail.app`) from queue wait (`24s` mean), classifies starved waits as failed `queue-timeout` (`v2.0.1`), and adds `-min-queue-held` / `-max-queue-wait` CI gates. |
| **"Cold TLS handshakes on `/` make our homepage look 4× slower than it is"** | Initial connection setup inflates p50 on the entry URL when mixed with reused keep-alive requests. | Lemmings `v2.0.1` breaks out `cold_hits`, `setup_p50_ns` (`DNS + Connect + TLS`), `warm_p50_ns`, and `ttfb_p50_ns` per path. |
| **"Static CSS/JS/images weren't tested—or distorted page SLOs"** | Either tests skip assets completely or lump tiny icons into page p95. | Opt-in `-assets -assets-per-page 8` fetches uncached same-origin HTML/CSS assets using the visitor's cookie jar and reports them in `AssetReport`, completely separate from `-p95-budget`. |
| **"Our load test accidentally hammered production or leaked credentials in CI artifacts"** | Unbounded crawlers follow external links or dump raw auth headers/cookies into HTML reports. | Strict `SameOrigin` scoping, dial-time `SafeDialContext` blocking cloud metadata/IMDS (`169.254.169.254`), `SEC-10` redaction (`json:"-"` on `with-*`/`body` and cookies), and remote S3/email screenshot opt-in. |

---

## Getting Started with Load Testing: Frequently Asked Questions

### How do I start load testing a web application from scratch?
Follow the safe four-stage Lemmings workflow (also runnable locally in 60 seconds against the bundled **Neon Arcade** target via `make demo` + `make run-forensics`):
1. **Install the single binary:** `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest`
2. **Pre-flight validate with zero traffic (`-dry-run`):** `lemmings -hit https://staging.example.com/ -crawl -dry-run` checks flags, YAML schema, `${ENV_VAR}` placeholders, indexed URLs, and Chromium/CDP availability without sending load.
3. **Run a small deterministic smoke test:** Start with `-terrain 1 -pack 5 -limit 5 -until 30s -ramp 0s -review` to inspect the live Admin Portal (`http://localhost:4000/#token=...`) and verify zero unexpected `4xx`/`5xx` or JS errors.
4. **Ramp to realistic peak load with CI gates:** Scale `-terrain`, `-pack`, `-limit`, and `-ramp` (`linear` or `fibonacci`), enable `-max-failure-rate` and `-p95-budget`, and save baseline JSON for `lemmings compare`.

### How many concurrent users (`-terrain`, `-pack`, `-limit`) should I start with?
Start at **10–25% of your target peak concurrency** (e.g., `-terrain 4 -pack 10 -limit 20 -until 60s -ramp 15s`), verify your staging database CPU, connection pools, and error forensics remain clean, and double `-limit` on subsequent runs until you find the latency knee where `p95` climbs sharply. Remember that `total visitors = terrain × pack` while `-limit` caps simultaneous active visitors.

### Can I still run a raw saturation benchmark like `ab` in Lemmings?
Yes. Pass `-think-min 0s -think-max 0s -ramp 0s -navigation random` (or a single-step `loop: true` YAML plan with `chill: 0s`) to saturate your target across all CPU cores using modern HTTP/2 while still retaining exact 4096-bin histograms, response validation, and CI gates.

### How do I test authenticated user flows safely without hardcoding passwords?
Write a YAML `-plan` with `with-body` or `with-headers` referencing `${ENV_VAR}` placeholders (e.g., `"${LOAD_PASS}"`, `"Bearer ${API_TOKEN}"`). Lemmings fails closed at startup if a variable is unset, tags all `with-*`/`body` fields `json:"-"` so secrets never enter reports or traces, and skips headless CDP replay on failed non-GET steps so Chromium never sends a bogus `GET` to a mutating `POST`/`DELETE` endpoint. See [Plans, Personas & Page Checks](../plans.md) and the AI agent runbook [SKILL.md](../../SKILL.md).

### Why is Lemmings a better default choice than Apache Benchmark (`ab`)?
Because `ab` only tells you how fast one cached HTTP/1.0 URL responds when hammered by a single CPU thread. Lemmings models how real users actually browse your site—maintaining cookies, following links, executing weighted API/HTML personas, respecting waiting rooms—and hands you visual screenshots, JS console traces, and `lemmings compare` regression diffs the moment anything breaks, all from one zero-Node Apache-2.0 Go binary.

> **When `ab` still fits:** If you are SSH'd into an air-gapped Linux box where `apache2-utils` is already preinstalled and you only need a 3-second raw HTTP/1.0 ping against a single local `localhost` health endpoint, `ab` is already on `PATH`. For any real staging benchmark, multi-route test, authenticated workflow, or CI pipeline, switch to Lemmings.

---

## Try Lemmings in 60 Seconds

```bash
# Install the single Apache-2.0 Go binary (zero Node.js, JVM, or Python)
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest

# Validate your target safely before sending load
lemmings -hit https://staging.example.com/ -crawl -dry-run

# Run a stateful swarm with live Error Forensics & Admin Review Mode
lemmings -hit https://staging.example.com/ \
  -terrain 4 -pack 10 -limit 40 -until 60s -ramp 15s \
  -crawl -scouts 1 -review -save-to ./reports
```

**Using an AI coding agent?** Point your agent at [`SKILL.md`](../../SKILL.md) to automatically inspect your codebase and author a 10-persona SEC-10-safe Lemmings YAML suite in minutes.

---

⭐ **If switching from `ab` to Lemmings gave you real error receipts instead of guesswork, [star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings)!**

**More Comparisons:** [All Tools Hub](./README.md) · [vs Bombardier](./lemmings-vs-bombardier.md) · [vs NBomber](./lemmings-vs-nbomber.md) · [vs k6](./lemmings-vs-k6.md) · [vs Gatling](./lemmings-vs-gatling.md) · [vs Locust](./lemmings-vs-locust.md) · [vs JMeter](./lemmings-vs-jmeter.md)
