# Lemmings vs Bombardier: Stateful Go Load Testing with Visual Forensics vs. Single-URL Microbenchmarking

[![GitHub Repo stars](https://img.shields.io/github/stars/andreimerlescu/lemmings?style=social)](https://github.com/andreimerlescu/lemmings)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-36f9f6)](../../LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)

> **TL;DR Verdict:** **Lemmings is the better Go load-testing binary than Bombardier (`github.com/codesenberg/bombardier`) for testing real web applications, user journeys, and multi-endpoint APIs.** Both ship as fast, dependency-free Go binaries—but Bombardier stops at stateless single-URL `fasthttp` blasting with zero cookies, zero response assertions, zero crawling, zero CI regression gates, and zero failure visibility. **Lemmings v2.0.1** gives you the same single-binary Go ergonomics *plus* stateful RFC 6265 visitor sessions, weighted multi-persona YAML plans, a `5%` rogue crawler cohort, automatic zero-Node Chrome DevTools Protocol (CDP) error screenshots + JS console traces, a live Admin Triage Portal, and `lemmings compare` CI regression gates.
>
> ⭐ **Want Go speed with real-world visibility?** [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to support open-source load testing with receipts.

| Quick Decision Summary | **Lemmings (`v2.0.1`)** | **Bombardier (`codesenberg/bombardier`)** |
| --- | --- | --- |
| **Best For** | Full web apps, multi-step authenticated flows, realistic API suites, CI regression gates, visual error forensics | Raw maximum-RPS microbenchmarking of a single static HTTP endpoint |
| **HTTP Engine** | Go stdlib `net/http` with HTTP/1.1 + HTTP/2, explicit TLS 1.2+, per-visitor pools & RFC 6265 cookie jars | `fasthttp` by default (no HTTP/2; requires `--http2` flag to fall back to Go `net/http`) |
| **When Something Breaks** | 4-part forensic dossier (CDP screenshot + JS console + cookie jar + parsed error body) + live Admin Portal | Status code bucket (`2xx`/`4xx`/`5xx`) and error counter only |
| **Install in 10 Seconds** | `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest` | `go install github.com/codesenberg/bombardier@latest` |

---

## Side-by-Side Feature Comparison (`Lemmings` vs `Bombardier`)

| Capability | Lemmings (`v2.0.1`) | Bombardier |
| --- | :---: | :---: |
| Single static Go binary (zero Node.js / JVM / Python) | ✅ (`20–23 MB`, 6 OS/arch targets) | ✅ (single Go binary) |
| Permissive open-source license | ✅ (Apache-2.0) | ✅ (MIT) |
| Native HTTP/2 enabled by default | ✅ (Go `net/http` TLS 1.2+) | ❌ (`fasthttp` default lacks HTTP/2; requires `--http2`) |
| Per-visitor stateful RFC 6265 cookie jars (`InspectingJar`) | ✅ | ❌ (stateless; static `-H "Cookie: ..."` only) |
| Multi-page site crawling & organic link navigation | ✅ (`-crawl`, `-navigation links`) | ❌ (single URL per invocation) |
| Multi-step, multi-method YAML plans (`GET`..`OPTIONS`) | ✅ (up to 100 weighted personas) | ❌ (single method `-m` + single body `-b`/`-f`) |
| Built-in `5%` rogue discovery cohort (`-rogue-rate`) | ✅ | ❌ |
| Ramp-up curves (`linear`, `fibonacci`, `step`) | ✅ | ❌ (instant fixed `-c` concurrency) |
| Human think time & saturation mode (`-think-min 0`) | ✅ | ❌ (constant rate `-r` or full blast only) |
| Response assertions (status, headers, cookies, body, DOM) | ✅ (`want-*` + automatic `not-blank`) | ❌ (status class counts only) |
| Automatic `200 OK` soft-error detection | ✅ | ❌ |
| Automatic error screenshots (pure-Go CDP, zero Node.js) | ✅ | ❌ |
| Real-browser Scouts (`-scouts N`) for JS/render crashes | ✅ | ❌ |
| Live Admin Triage Portal (`Bug`/`Flaky`/`Expected`/`Ignored`) & `-review` | ✅ | ❌ |
| Opt-in same-origin static asset tier (`-assets`) | ✅ (isolated from page SLOs) | ❌ |
| Native virtual waiting-room protocol (Concert queue separation) | ✅ | ❌ |
| Self-contained HTML, GitHub Markdown & JSON (`schema 2.0`) reports | ✅ | ❌ (stdout text or raw JSON/template via `-o`) |
| Built-in CI pass/fail gates (`exit 2`) & `lemmings compare` diffs | ✅ | ❌ |
| Isolated Prometheus `/metrics` exporter (`24` metrics) | ✅ | ❌ |
| Security hardened (SSRF dial guard, secret scrubbing, CycloneDX SBOM) | ✅ (`SEC-01`–`SEC-10`) | ❌ |
| AI agent plan-authoring runbook (`SKILL.md`) | ✅ | ❌ |

---

## Performance & Real-World Output Side by Side

Bombardier is widely respected because `fasthttp` allocates almost nothing when blasting one URL in a tight loop. The catch: **raw single-URL RPS tells you almost nothing about how a real web application behaves under stateful multi-page load.** Below is real verified output from Lemmings on `counthis.app` and `toktail.app`, contrasted against Bombardier's documented runtime model and what Bombardier misses on the exact same targets.

### Real Verified Lemmings Run Data (`396` Stateful Visitors · `3m0s`)

| Metric | Lemmings on `counthis.app` *(Standard Web App)* | Lemmings on `toktail.app` *(Concert Queue + 503 Chaos)* | What Bombardier Would Show on These Targets |
| --- | --- | --- | --- |
| **Load Configuration** | `33` terrains × `12` pack (`396` visitors), `-limit 434`, `90s` linear ramp, `90s` lifespan, `600ms–6s` think, `-crawl` | Identical: `33×12` (`396` visitors), `90s` ramp, `90s` lifespan, `600ms–6s` think, `-crawl` | Fixed `-c` connections hitting **1 single URL** (`/` or `/terms.html`); no ramp curve, no sitemap crawl, no multi-route journey |
| **Visitors & Rescued Score** | `396/396` started & completed · **`100.0%` rescued** (`384` peak alive) | `396/396` started & completed · **`89.1%` rescued** (`353/396` flawless, `384` peak alive) | No visitor sessions or per-user rescued score |
| **Visits & Throughput** | **`10,831` page visits** across `10` crawled routes · `60.2` visits/s avg (**`144` visits/s peak**) · `76.15 MB` | **`3,110` page visits** across `4` routes · `17.3` visits/s avg (`77` visits/s peak) · `36.87 MB` | High RPS against 1 warm edge-cached route only; misses uncached deep routes and session churn |
| **Origin Page Latency** | **p50 `35.8ms`** · p90 `123.8ms` · **p95 `185.4ms`** · p99 `607.8ms` · **TTFB p95 `144.5ms`** (`<= 0.5%` 4096-bin log histogram) | **p50 `43.0ms`** · p90 `69.3ms` · **p95 `186.2ms`** · p99 `206.0ms` · **TTFB p95 `156.5ms`** *(isolated from queue wait)* | Single global latency histogram (`-l`) over 1 URL; mixes redirects, soft errors, and queue pages into one aggregate |
| **Waiting Room & Queueing** | `0` held | **`1,093` held** (`729` admitted, `364` died in line, **`11,685` `/queue/status` polls**, max pos `359`, mean wait `24s`, `3,026` admit passes) | **Cannot traverse queues** — never stores `room_ticket` cookies, never polls `/queue/status`, and never earns `concert_admit` passes |
| **Error Forensics & Visual Receipts** | `0` errors · `0` dropped lifelogs | `313` HTTP `503`s (`11.4%`) → **`12` deduplicated 4-part artifacts** + **`12` Chrome CDP screenshots** (`301` deduped, `0` dropped) + JS console `404` on `/favicon.ico` | Prints `5xx - 313`. **Zero screenshots, zero extracted error body, zero cookie jar state, zero JS console logs** |

### Runtime & Architecture Footprint Comparison

| Dimension | Lemmings (`v2.0.1`) | Bombardier |
| --- | --- | --- |
| **Language & Deployment** | Pure Go (`20–23 MB` single binary, CycloneDX 1.5 SBOM + `SHA256SUMS`) | Pure Go (~10 MB single binary) |
| **HTTP Client Semantics** | RFC-compliant Go `net/http` (HTTP/1.1 + HTTP/2, per-lemming connection pools, `SafeDialContext` SSRF guard, `MinTLSConfig` TLS 1.2+) | `fasthttp` default (zero-alloc speed, but non-standard HTTP/1.1 quirks & no HTTP/2 unless switching to `--http2` Go `net/http`) |
| **Latency Precision** | Exact sorted samples up to `8,192` global (`1,024`/path), then `32 KiB` 4096-bin `uint64` histogram (`<= 0.5%` relative error) | HDR histogram when `-l` is enabled |
| **Backpressure & Integrity** | Bounded channels, non-blocking O(1) forensics dedup (`TryReserve`), prominent `DATA DROPPED` integrity banners if saturated | Lightweight stdout counters; no forensics pipeline |

### Visual Receipts vs. Blind Status Counters

When `toktail.app` returned `313` HTTP `503`s under peak load, Bombardier would print `5xx - 313` and exit `0`. Lemmings automatically captured 12 deduplicated Chrome CDP screenshots showing the exact Concert waiting room screen (`YOUR PLACE IN LINE: 354`), the failing lemming's cookie jar (`healthy: 3 active, 0 expired`), and browser console errors (`favicon.ico` `404`)—streamed live into the Admin Portal and bundled into self-contained HTML & GitHub Markdown reports:

| Live Admin Triage Portal (`http://localhost:4000`) | Self-Contained HTML Report with Embedded Screenshots |
| :---: | :---: |
| ![Lemmings Admin Portal — Dark Mode](../images/dashboard-dark.jpg) | ![Lemmings HTML Report](../images/report.jpg) |

---

## Conventions Translation: How Bombardier Flags Map to Lemmings

Because both tools are Go CLIs, moving your commands from Bombardier to Lemmings is effortless:

| Bombardier Convention | Lemmings (`v2.0.1`) Equivalent | Why Lemmings Is Better |
| --- | --- | --- |
| `bombardier -c 125` | `-terrain 5 -pack 25 -limit 125` | Supports progressive ramping (`-ramp`, `-ramp-shape`) and stateful per-visitor sessions |
| `bombardier -d 30s` | `-until 30s -ramp 0s` | Cleanly separates ramp-up duration from individual visitor lifespan |
| `bombardier -n 10000` | `-max-pages` per lemming (or `-until`) | Bounds realistic visitor sessions instead of raw single-URL blasts |
| `bombardier --http2` | Automatic (default Go `net/http` TLS 1.2+) | No toggling between `fasthttp` and `net/http` backends |
| `bombardier -t 2s` | `-request-timeout 2s` | Covers full request, redirects, and bounded body read (`-max-body-bytes`) |
| `bombardier -m POST -b '...' -H 'Content-Type: ...'` | YAML `-plan` step (`method: POST`, `body:` / `with-body`, `with-headers`) | Supports multi-step workflows, `${ENV_VAR}` secrets, and `want-*` response validation |
| `bombardier -l -o json` | Always-on percentiles + `-save-to ./reports` (HTML, MD, JSON `schema 2.0`) | Zero custom Go template hacking; includes per-path p50/p95/p99 and `cold_hits` vs `warm_p50` |
| *(None — manual jq/bash scripting)* | `-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`, `lemmings compare` | Built-in deterministic exit code `2` CI gating and baseline release diffing |

### Side-by-Side Example: Benchmarking an HTTP/2 Endpoint

**Bombardier:**
```bash
# Single URL only, no assertions, exits 0 even if server returns 100% HTTP 500
bombardier -c 100 -d 30s --http2 -l \
  -H "Accept: application/json" \
  https://staging.example.com/api/v1/catalog
```

**Lemmings (Same Go Single-Binary Speed + Assertions + CI Gate + Visual Forensics):**

Save as `catalog-api.yaml`:
```yaml
name: catalog-api-reader
weight: 10
loop: true
plan:
  - name: fetch-catalog
    path: /api/v1/catalog
    method: GET
    chill: 0s
    with-headers:
      - key: Accept
        value: application/json
    want-status: 200
    want-content-type: application/json
    want-body:
      - contains: '"items"'
        not-contains: '"error"'
```

Run with built-in CI gates (exits `2` on breach) and multi-format reports:
```bash
lemmings \
  -hit https://staging.example.com/ \
  -plan catalog-api.yaml \
  -terrain 10 -pack 10 -limit 100 \
  -until 30s -ramp 5s -rogue-rate 0 \
  -max-failure-rate 0.01 \
  -p95-budget 250ms \
  -min-plan-pass-rate 0.99 \
  -tty=false -unique-report -save-to ./reports
```

---

## Bombardier Gotchas & Pitfalls — Solved Out of the Box by Lemmings

1. **`fasthttp` Default Breaks HTTP/2 and Real-World Header/TLS Behavior**
   - **Bombardier pitfall:** Bombardier defaults to `fasthttp`, which does not support HTTP/2 and deviates from standard RFC HTTP client behavior on certain `Host` headers, redirects, and TLS edge cases—forcing you to remember `--http2` or `--http1` (which puts you back on Go's `net/http` anyway!).
   - **How Lemmings solves it:** Uses hardened Go stdlib `net/http` everywhere with HTTP/2, explicit TLS 1.2+, RFC 6265 cookie jars, and dial-time SSRF protection (`SafeDialContext`) out of the box.
2. **Exit Code `0` on Catastrophic `500` Outages**
   - **Bombardier pitfall:** Bombardier is a benchmark printer, not a CI gate—if your staging deploy returns 100% HTTP `500`s or `5000ms` p95 latency, Bombardier prints the summary and exits `0`, leaving you to write fragile `jq` wrappers in CI.
   - **How Lemmings solves it:** Ships first-class CI gates (`-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`, `-min-queue-held`, `-max-queue-wait`) returning deterministic exit code `2`, plus `lemmings compare` to fail builds on release-to-release regressions.
3. **Zero State Between Requests (No Login → Action → Logout Flows)**
   - **Bombardier pitfall:** Cannot fetch a login page, submit credentials, store the minted session cookie, call a protected endpoint, and verify logout. Every connection replays one identical payload.
   - **How Lemmings solves it:** Every lemming carries an isolated RFC 6265 `InspectingJar` across ordered YAML steps with `with-cookies`, `with-body`, `stop-on-fail: true`, and `want-cookies` (including `absent: true` after logout).
4. **Silent `200 OK` Soft-Error & Empty-Response Blindness**
   - **Bombardier pitfall:** Counts a `200 OK` HTML page displaying `"Upstream Redis timeout"` or an empty `0`-byte response as a healthy `2xx`.
   - **How Lemmings solves it:** Automatically runs `not-blank` HTML validation, truncated-body detection (`ErrTruncatedBody`), `want-body` `contains`/`not-contains`, `want-selectors`, and `-scouts N` real-browser JS crash detection.
5. **No Ramping or Traffic Realism**
   - **Bombardier pitfall:** Opens all `-c` connections immediately with zero ramp-up and zero multi-persona traffic mix—missing gradual saturation curves, session churn, and unscripted dead-link bugs.
   - **How Lemmings solves it:** Ramps terrains smoothly (`linear`, `fibonacci`, `step`), mixes up to `100` weighted YAML personas, and dedicates a deterministic `5%` rogue cohort (`-rogue-rate`) to explore crawled links organically.
6. **Zero Visual Debugging or Team-Shareable Reports**
   - **Bombardier pitfall:** Outputs terminal text or raw JSON. Sharing failures with frontend/product teammates requires manually reproducing errors in a browser and pasting screenshots into Slack/Jira.
   - **How Lemmings solves it:** Generates self-contained Synthwave HTML reports (embedded base64 screenshots), GitHub-ready Markdown reports (`.artifacts/` relative images), lean JSON (`schema 2.0`), S3/SMTP delivery, and an authenticated live Admin Portal with `-review` triage tagging.

---

## Common Load-Testing Problems & How Lemmings Resolves Them

| Real-World Load Testing Problem | Why Single-URL Blasters Miss It | How Lemmings Solves It Out of the Box |
| --- | --- | --- |
| **Session & cookie leaks under concurrency** | Stateless tools send zero or shared static cookies across all workers. | Per-lemming `InspectingJar` (bounded at 256 cookies, RFC 6265 scoped) tracks cookie health, expiry, and HMAC fingerprints (`SEC-10`) in every forensic artifact. |
| **Frontend JS errors or blank screens on `200 OK`** | HTTP clients never parse DOM or execute JavaScript. | Built-in `not-blank` check + `want-selectors` + real headless Chromium Scouts (`-scouts 2`) catching `console.error` & uncaught exceptions on `200 OK` pages. |
| **Waiting rooms & rate limits skewing SLOs** | `429`/`503` queue screens pollute page latency histograms or break the run. | Native Concert protocol separates origin latency from queue wait, tracks `queue_polls`/`admit_passes`/`bans_429`, and prevents survivorship bias (`queue-timeout` in `v2.0.1`). |
| **Latency regressions slipping past PRs** | Raw stdout dumps aren't diffed against yesterday's baseline. | `lemmings compare -max-p95-delta 100ms -max-failure-delta 0.01 baseline.json candidate.json` diffs overall SLOs, top-10 regressed paths, and newly introduced error fingerprints (`+`/`-`). |
| **Secrets leaking into CI logs & artifacts** | CLI `-H "Authorization: ..."` flags appear in shell history and process lists. | Fail-closed `${ENV_VAR}` expansion in YAML plans, `json:"-"` exclusion (`SEC-10`), `0600` trace files, and regex secret scrubbing (`secrets.ScrubText`). |

---

## Getting Started with Load Testing: Frequently Asked Questions

### How do I begin load testing if I've only ever used CLI benchmarkers like Bombardier?
Start with the same single-binary command-line workflow you already love—no Node, Java, or Python required:
1. Run `lemmings -hit https://staging.example.com/ -crawl -dry-run` to preview discovered routes and check Chromium availability without sending traffic.
2. Run a short crawled swarm with `-review`: `lemmings -hit https://staging.example.com/ -terrain 4 -pack 8 -limit 32 -until 45s -ramp 10s -crawl -review -save-to ./reports`.
3. Open the printed `http://localhost:4000/#token=...` Admin Portal to inspect live latency curves and any captured error screenshots.
4. Once you want repeatable checkout/API journeys in CI, move from `-crawl` to a version-controlled YAML `-plan` (or have your coding agent generate a 10-persona suite using [`SKILL.md`](../../SKILL.md)).

### How do I translate Bombardier's `-c` (connections) and `-d` (duration) into Lemmings?
Set `-limit` to your desired concurrent visitor cap (matching `-c`), choose `-terrain × -pack` to control total visitors spawned across `-ramp`, and set `-until` to each visitor's lifespan (matching `-d`). For saturation testing without think pauses, add `-think-min 0s -think-max 0s -ramp 0s`.

### Why does Lemmings use Go's `net/http` instead of `fasthttp`?
Because real browsers and API clients speak HTTP/2, follow strict RFC 6265 cookie rules, negotiate TLS 1.2+, and reuse per-visitor connection pools. Standard `net/http` avoids `fasthttp`'s HTTP/2 absence and header edge cases while still effortlessly driving hundreds of concurrent stateful visitors and hundreds of RPS from a single laptop or CI runner (e.g., `396` stateful visitors, `10,831` visits, `144` peak visits/s, `0` dropped lifelogs on `counthis.app`).

### Why is Lemmings the better choice over Bombardier for most teams?
Because once your endpoint is behind real authentication, redirects, multiple routes, a CDN, a waiting room, or a frontend framework, single-URL `fasthttp` RPS is a vanity metric. Lemmings stays a single static Go binary while upgrading you to stateful user journeys, strict YAML assertions, automatic Chrome error screenshots, a live triage portal, Prometheus metrics, and `lemmings compare` CI regression gates.

> **When Bombardier still fits:** If you are microbenchmarking raw kernel/socket throughput of a single stateless microservice route (e.g., testing 50,000+ RPS of `GET /ping` over HTTP/1.1 loopback to tune a reverse proxy or serialization hot path), Bombardier's `fasthttp` loop is a great specialized microbenchmarker. For end-to-end web app, multi-route, authenticated, or CI load testing, Lemmings is the clear winner.

---

## Try Lemmings in 60 Seconds

```bash
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest

# Pre-flight validation (zero load sent)
lemmings -hit https://staging.example.com/ -crawl -dry-run

# Stateful multi-route load test with CI gates and HTML/MD/JSON reports
lemmings -hit https://staging.example.com/ \
  -terrain 6 -pack 15 -limit 60 -until 60s -ramp 15s -ramp-shape fibonacci \
  -crawl -scouts 1 \
  -max-failure-rate 0.01 -p95-budget 500ms \
  -unique-report -save-to ./reports
```

---

⭐ **Built in Go, hardened with `SEC-01`–`SEC-10`, and Apache-2.0 forever. [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to boost visibility!**

**More Comparisons:** [All Tools Hub](./README.md) · [vs Apache Benchmark (`ab`)](./lemmings-vs-ab.md) · [vs NBomber](./lemmings-vs-nbomber.md) · [vs k6](./lemmings-vs-k6.md) · [vs Gatling](./lemmings-vs-gatling.md) · [vs Locust](./lemmings-vs-locust.md) · [vs JMeter](./lemmings-vs-jmeter.md)
