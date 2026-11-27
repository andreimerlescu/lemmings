# Lemmings vs Locust: Compiled Multi-Core Go with Visual Forensics & Hardened Portal vs. Python GIL Load Scripts

[![GitHub Repo stars](https://img.shields.io/github/stars/andreimerlescu/lemmings?style=social)](https://github.com/andreimerlescu/lemmings)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-36f9f6)](../../LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)

> **TL;DR Verdict:** **Lemmings is the better load testing tool than Locust for web applications, HTTP APIs, and production CI pipelines.** While Locust relies on single-core Python (`gevent`)—forcing `HttpUser` -> `FastHttpUser` rewrites and `--master`/`--worker` multi-process orchestration just to saturate one modern CPU—and gives you an unauthenticated metrics UI with **zero error screenshots, zero JS console capture, and zero baseline regression diffing**, **Lemmings v2.0.1** runs natively across all CPU cores from one `20–23 MB` Apache-2.0 Go binary, replaces `locustfile.py` boilerplate with strict multi-persona YAML (`-plan`) + a `5%` rogue crawler cohort, captures automatic 4-part Chrome DevTools Protocol (CDP) error screenshots & JS console traces, ships an HMAC-authenticated Synthwave Admin Triage Portal (`-review`), and enforces deterministic CI gates + `lemmings compare` diffs out of the box.
>
> ⭐ **Ready to leave Python GIL bottlenecks and blind failure tables behind?** [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to boost open-source visibility.

| Quick Decision Summary | **Lemmings (`v2.0.1`)** | **Locust** |
| --- | --- | --- |
| **Concurrency & Runtime** | Compiled Go goroutines across all CPU cores in one process — single `20–23 MB` binary, zero runtime dependencies | Python + `gevent` (1 core per process due to GIL; scales across cores on Linux/macOS via `--processes -1` multi-process `fork()`, not supported on Windows) |
| **Test Authoring** | Zero-code `-crawl` mode + strict `KnownFields(true)` multi-persona YAML (`-plan`) | Imperative Python `locustfile.py` + `virtualenv` / `pip` dependencies |
| **When Something Breaks** | Automatic CDP screenshot + JS console + cookie jar + parsed error body + 4-panel Triage Modal (`Bug`/`Flaky`/`Expected`/`Ignored`) | Exception/status string counted in Locust's Failures tab (zero screenshots, zero JS logs, zero cookie inspector) |
| **Install in 10 Seconds** | `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest` | `python3 -m venv .venv && pip install locust` |

---

## Side-by-Side Feature Comparison (`Lemmings` vs `Locust`)

| Capability | Lemmings (`v2.0.1`) | Locust |
| --- | :---: | :---: |
| Permissive open-source license | ✅ (Apache-2.0) | ✅ (MIT) |
| Single zero-dependency binary (no Python / venv / pip drift) | ✅ (`20–23 MB` Go binary) | ❌ (requires Python 3.9+ & pip packages) |
| Multi-core CPU scaling | ✅ (native Go goroutine scheduler in 1 process across all OSes) | 🟡 (1 core/process GIL; scales across cores via `--processes -1` `fork()` master+workers on Linux/macOS only — unsupported on Windows) |
| Zero-script automatic site crawl & organic link navigation | ✅ (`-crawl`, `-navigation links`) | ❌ (must write `locustfile.py` for every route) |
| Strict schema-validated YAML plans (`-dry-run` pre-flight) | ✅ (`KnownFields(true)` up to 100 personas) | ❌ (untyped Python runtime errors discovered mid-test) |
| Automatic `5%` rogue wanderer cohort (`-rogue-rate`) | ✅ | ❌ |
| Per-visitor stateful RFC 6265 cookie jars (`InspectingJar`) | ✅ (with cookie health + HMAC audit) | ✅ (`requests.Session` / `FastHttpSession`, no audit UI) |
| Built-in `200 OK` blank-page & soft-error detection | ✅ (automatic `not-blank` + `want-*`) | ❌ (must write `with client.get(..., catch_response=True)` boilerplate) |
| Automatic error screenshots (pure-Go CDP, zero Node.js) | ✅ | ❌ |
| Real-browser Scouts (`-scouts N`) for client-side JS/render crashes | ✅ | ❌ |
| Authenticated live Admin Portal with 4-panel Error Triage & `-review` | ✅ (loopback HMAC-SHA256 + strict CSP + CSRF) | ❌ (Web UI `:8089` has metrics/charts only; unauthenticated by default, no screenshots or triage) |
| Self-contained HTML (inline screenshots), GitHub Markdown & JSON reports | ✅ (all 3 + `.artifacts/` automatically) | ✅ (HTML + CSV stats only; no Markdown, no screenshots) |
| Built-in CI pass/fail gates (`exit 2`) & `lemmings compare` diffs | ✅ (`-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`, `compare`) | ❌ (manual Python event listeners needed for custom thresholds; no CLI diff) |
| Native virtual waiting-room protocol (Concert queue separation) | ✅ | ❌ |
| Opt-in same-origin static asset tier (`-assets`, isolated from page SLOs) | ✅ | ❌ |
| Built-in Prometheus `/metrics` scrape exporter (`24` metrics) | ✅ (`-observe`) | ❌ (requires third-party `locust-plugins` / custom Flask endpoint) |
| Built-in S3 & hardened TLS 1.2+ SMTP report delivery | ✅ (`-save-to s3://...,mailto:...`) | ❌ |
| Security hardened (SSRF dial guard, secret scrubbing, CycloneDX SBOM) | ✅ (`SEC-01`–`SEC-10`) | ❌ |
| Built-in multi-machine master/worker distributed clustering | ❌ (single-node multi-core Go handles huge concurrency cleanly) | ✅ (`--master` / `--worker` across nodes) |
| AI agent plan-authoring runbook (`SKILL.md`) | ✅ | ❌ |

---

## Performance & Real-World Output Side by Side

Locust's biggest real-world surprise is hitting **100% CPU on a single core** with `HttpUser` (which wraps Python's blocking/heavy `requests` + `urllib3` stack under `gevent`), forcing engineers to rewrite scripts for `FastHttpUser` (`geventhttpclient`) and spin up multi-process `--master`/`--worker` topologies even on a single 8-core laptop. Lemmings uses compiled Go goroutines across every core natively—and produces far richer real-world diagnostics.

### Real Verified Lemmings Run Data (`396` Stateful Visitors · `3m0s`)

| Metric | Lemmings on `counthis.app` *(Standard Web App)* | Lemmings on `toktail.app` *(Concert Queue + 503 Chaos)* | What Locust Requires / Captures on These Targets |
| --- | --- | --- | --- |
| **Execution Footprint** | **1 Go binary, 1 process**, all CPU cores utilized automatically (`0` dropped lifelogs) | **1 Go binary, 1 process** (`0` dropped lifelogs, `0` dropped captures) | Python venv + `locustfile.py`; at 396+ active users with TLS & HTML parsing, single-process `HttpUser` approaches Python GIL saturation and requires `--processes` / `--worker` multi-process fanout |
| **Visitors & Rescued Score** | `396/396` completed · **`100.0%` rescued** (`384` peak alive) | `396/396` completed · **`89.1%` rescued** (`353/396` flawless, `384` peak alive) | Reports aggregate RPS & failure %; no per-visitor rescued journey score or clickable notable-user trails |
| **Visits & Throughput** | **`10,831` visits** across `10` auto-crawled routes · `60.2` visits/s avg (**`144` visits/s peak**) · `76.15 MB` | **`3,110` visits** across `4` routes · `17.3` visits/s avg (`77` visits/s peak) · `36.87 MB` | Only visits hardcoded `@task` routes unless you add BeautifulSoup/lxml HTML link parsing (which increases Python CPU cost sharply) |
| **Origin Page Latency** | **p50 `35.8ms`** · p90 `123.8ms` · **p95 `185.4ms`** · p99 `607.8ms` · **TTFB p95 `144.5ms`** (`<= 0.5%` 4096-bin histogram) | **p50 `43.0ms`** · p90 `69.3ms` · **p95 `186.2ms`** · p99 `206.0ms` · **TTFB p95 `156.5ms`** *(isolated from queue wait)* | Locust tracks per-endpoint percentiles, but Python GIL scheduling jitter inflates client-side p99 under heavy load and waiting-room holds distort page latency |
| **Waiting Room & Queueing** | `0` held | **`1,093` held** (`729` admitted, `364` died in line, **`11,685` `/queue/status` polls**, max pos `359`, mean wait `24s`, `3,026` passes) | **Must be hand-coded in Python** (detect waiting HTML/JSON, poll `/queue/status`, track custom events, avoid counting starved waits as clean passes) |
| **Error Forensics & Visual Receipts** | `0` errors · `0` dropped lifelogs | `313` HTTP `503`s (`11.4%`) → **`12` deduplicated 4-part artifacts** + **`12` Chrome CDP screenshots** (`301` deduped, `0` dropped) + JS console `404` on `/favicon.ico` | Failures tab shows `HTTPError('503 Server Error') × 313`. **Zero screenshots, zero extracted error body, zero cookie jar state, zero JS console output, zero triage tags** |

### Runtime & Architecture Footprint Comparison

| Dimension | Lemmings (`v2.0.1`) | Locust |
| --- | --- | --- |
| **Distribution & CI Speed** | Single `20–23 MB` static Go binary; zero `pip install` or `venv` setup | Python interpreter + `pip install locust` (plus C extensions like `gevent`/`greenlet`/`msgpack`/`zope`) |
| **Multi-Core Efficiency** | True OS-thread M:N goroutine scheduling across every core in one process (Linux, macOS, Windows) | Single-core Python GIL per process; `locust --processes -1` forks 1 master + N worker processes across cores on Linux/macOS (unsupported on Windows; manual `--master`/`--worker` otherwise) |
| **Web UI Security** | Loopback `127.0.0.1` only, 256-bit boot token in `#fragment`, HMAC-SHA256 cookie, strict hash CSP, CSRF guard, auth rate limiter (`SEC-05`) | `:8089` binds openly unless configured; no built-in auth by default |
| **Browser Forensics** | Built-in zero-Node pure-Go CDP screenshot & JS console capture + `-scouts` | None built in (would require bolting on Playwright/Selenium Python processes) |

### Authenticated Live Triage Portal vs. Metrics-Only Failures Tab

Locust's Web UI is popular because seeing live charts beats staring at raw terminal text—which is exactly why Lemmings takes the live web portal **ten steps further**. When `toktail.app` threw `313` HTTP `503`s, Lemmings streamed live charts *plus* 12 deduplicated 4-part forensic cards (screenshot, cookie jar health `3 active, 0 expired`, visitor breadcrumb trail, extracted HTML body, and browser JS console `404` on `/favicon.ico`) into an authenticated Synthwave Admin Portal where you can tag `Bug`/`Flaky`/`Expected`/`Ignored` before clicking **Finalize & Write Report**:

| Live Admin Triage Portal (`http://localhost:4000`) | Self-Contained HTML Report with Embedded Screenshots |
| :---: | :---: |
| ![Lemmings Admin Portal — Dark Mode](../images/dashboard-dark.jpg) | ![Lemmings HTML Report](../images/report.jpg) |

---

## Conventions Translation: How Locust Concepts Map to Lemmings

Every Locust idiom maps directly to clean, strict Lemmings YAML and CLI flags—zero Python classes or `catch_response=True` boilerplate:

| Locust Convention (`locustfile.py`) | Lemmings (`v2.0.1`) Equivalent | Why Lemmings Is Better |
| --- | --- | --- |
| `class BuyerUser(HttpUser): weight = 3` | YAML `plans:` entry (`name: buyer`, `weight: 3`) | Deterministic weight allocation (`slot = planCohortIndex % TotalWeight`), up to 100 personas |
| `SequentialTaskSet` | Ordered YAML `plan: [...]` with `stop-on-fail: true` | Guaranteed sequential execution + per-step `PlanStepReport` tables |
| `wait_time = between(0.6, 6)` | `-think-min 600ms -think-max 6s` or per-step `chill: 1s` | Global randomized think time + exact per-step overrides (`chill: 0s` for saturation) |
| `self.client.post("/login", data={...})` | `method: POST`, `path: /login`, `with-body: [...]` | Auto form/JSON encoding + fail-closed `${ENV_VAR}` secrets (`SEC-10`) |
| `with self.client.get("/x", catch_response=True) as r:` | `want-status`, `want-body`, `want-headers`, `want-cookies`, `want-selectors`, `want-title` | Declarative assertions + built-in `not-blank` + automatic CDP forensics on failure |
| `locust --headless -u 100 -r 10 -t 60s` | `lemmings -terrain 10 -pack 10 -limit 100 -ramp 10s -until 60s -tty=false` | Supports `linear`, `fibonacci`, and `step` ramp shapes natively |
| `locust --html report.html --csv out` | `-save-to ./reports` (emits `.html`, `.md`, `.json`, `.artifacts/`) | Self-contained HTML with inline screenshots + GitHub-ready Markdown + `schema 2.0` JSON |
| Custom `@events.quitting` Python hook | `-max-failure-rate 0.01 -p95-budget 750ms -min-plan-pass-rate 0.99` + `lemmings compare` | Built-in deterministic exit code `2` CI gating and baseline diffing |
| Web UI (`http://localhost:8089`) | Authenticated Admin Portal (`http://localhost:4000/#token=...`) + `-review` | Adds live Error Forensics Gallery, 4-panel Inspector Modal, and triage verdicts |

### Side-by-Side Example: Weighted Multi-User Web App Test

**Locust (`locustfile.py` — requires Python/pip, `catch_response` boilerplate, GIL single-core limit, zero screenshots):**
```python
import os
from locust import HttpUser, SequentialTaskSet, task, between

class BrowseTasks(SequentialTaskSet):
    @task
    def home(self):
        with self.client.get("/", catch_response=True) as r:
            if r.status_code != 200 or "Something went wrong" in r.text:
                r.failure("Bad landing page")

    @task
    def catalog(self):
        self.client.get("/products")

class AnonymousBrowser(HttpUser):
    weight = 7
    wait_time = between(1, 2)
    tasks = [BrowseTasks]

class ReturningCustomer(HttpUser):
    weight = 3
    wait_time = between(1, 2)

    @task
    def login_and_profile(self):
        r = self.client.post("/login", data={
            "username": os.environ["DEMO_USER"],
            "password": os.environ["DEMO_PASS"],
        })
        if r.status_code == 200:
            with self.client.get("/account/profile", catch_response=True) as p:
                if "Account Settings" not in p.text:
                    p.failure("Missing profile content")
```

**Lemmings (Zero Python, All CPU Cores, Automatic 5% Rogues, Chrome CDP Forensics, Free CI Gates):**

Save as `personas.yaml`:
```yaml
plans:
  - name: anonymous-browser
    weight: 7
    loop: true
    plan:
      - name: home
        path: /
        method: GET
        chill: 1s
        want-status: 200
        want-content-type: text/html
        want-selectors: ["nav", "main"]
        want-body:
          - not-contains: Something went wrong
      - name: catalog
        path: /products
        method: GET
        chill: 1500ms
        want-status: 200

  - name: returning-customer
    weight: 3
    stop-on-fail: true
    loop: true
    plan:
      - name: authenticate
        path: /login
        method: POST
        chill: 1s
        with-body:
          - key: username
            value: "${DEMO_USER}"
          - key: password
            value: "${DEMO_PASS}"
        want-status: 200
        want-cookies:
          - name: session_id
      - name: view-profile
        path: /account/profile
        method: GET
        chill: 1500ms
        want-status: 200
        want-cookies:
          - name: session_id
        want-body:
          - contains: Account Settings
            not-contains: Something went wrong
```

Validate with `-dry-run`, then run across every CPU core with live visual forensics:
```bash
export DEMO_USER="player1"
export DEMO_PASS="staging-pass"
lemmings -hit https://staging.example.com/ -plan personas.yaml -dry-run

lemmings \
  -hit https://staging.example.com/ \
  -plan personas.yaml \
  -terrain 10 -pack 10 -limit 100 -until 60s -ramp 15s \
  -scouts 2 \
  -min-plan-pass-rate 0.99 -max-failure-rate 0.01 -p95-budget 750ms \
  -review -unique-report -save-to ./reports
```

---

## Locust Gotchas & Pitfalls — Solved Out of the Box by Lemmings

1. **The Python GIL & `HttpUser` CPU Bottleneck ("Why Is My Load Generator at 100% CPU?")**
   - **Locust pitfall:** Each Locust OS process is bound to 1 CPU core by Python's GIL, and default `HttpUser` (`requests`) saturates a core quickly—triggering Locust's `CPU usage above 90%` warning. Scaling across cores requires switching to `FastHttpUser` (which alters cookie/TLS/API semantics) and spawning multi-process workers via `--processes -1` (which uses `fork()`, so it is **not supported on Windows**, and splits execution into a non-user-running master + N worker processes) or manual `--master`/`--worker` orchestration.
   - **How Lemmings solves it:** Compiled Go goroutines saturate every CPU core natively from a single static binary across Linux, macOS, and Windows—with zero GIL, zero multi-process `fork()` overhead, and zero `FastHttpUser` rewrite traps.
2. **Metrics-Only Web UI with Zero Visual Error Forensics**
   - **Locust pitfall:** Locust's Web UI shows request rate charts and exception counts, but when `/checkout` returns `503` or a `200 OK` broken React page, you have zero screenshot, zero JS console output, zero cookie jar table, and zero parsed error body.
   - **How Lemmings solves it:** Streams live 4-part Chrome CDP Error Forensics cards directly into the Admin Portal, lets you zoom screenshots and inspect cookie health (`InspectingJar`) & JS console errors, and persists `Bug`/`Flaky`/`Expected`/`Ignored` tags into final reports via `-review`.
3. **Unauthenticated Default Web UI & Security Gaps**
   - **Locust pitfall:** Starting Locust's web server opens port `8089` without built-in authentication or CSRF hardening—risky on shared dev hosts or staging networks—and custom Python scripts have zero built-in SSRF protection or report secret scrubbing.
   - **How Lemmings solves it:** Audited `SEC-01`–`SEC-10`: Admin Portal binds strictly to `127.0.0.1` with a 256-bit `#token` fragment, HMAC-SHA256 `SameSite=Strict` cookies, strict hash-based CSP, per-IP + global auth rate limiting, `SafeDialContext` blocking cloud IMDS (`169.254.0.0/16`), and `SEC-10` redaction.
4. **Silent Runtime Typos Inside Untyped Python Scripts**
   - **Locust pitfall:** A typo in an infrequent `@task(1)` or error-handling branch only crashes 20 minutes into a load test—or silently misses an assertion because `catch_response=True` was forgotten.
   - **How Lemmings solves it:** Strict `KnownFields(true)` YAML validation + fail-closed `${ENV_VAR}` checks + `-dry-run` catch every typo, invalid selector, off-origin URL, and missing secret in milliseconds *before* sending a single request.
5. **No Native CI Thresholds or Release Regression Diffing**
   - **Locust pitfall:** Locust `--headless` exits `0` even when p95 latency triples—unless you write custom `@events.quitting.add_listener` Python hooks to inspect `environment.stats.total` and set `environment.process_exit_code = 1`, and you still have no CLI tool to diff candidate vs. baseline runs.
   - **How Lemmings solves it:** First-class CLI CI gates (`-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`, `-min-queue-held`, `-max-queue-wait`) with exit code `2`, plus `lemmings compare` for automated regression detection.
6. **`gevent` Monkey-Patching Surprises & Dependency Rot**
   - **Locust pitfall:** Importing third-party Python libraries alongside `gevent` monkey-patching can silently block the event loop and freeze virtual users; CI runners waste time rebuilding virtualenvs as Python minor versions change.
   - **How Lemmings solves it:** Zero virtualenvs, zero monkey-patching, one reproducible static Go binary with a CycloneDX 1.5 SBOM and `SHA256SUMS`.

---

## Common Load-Testing Problems & How Lemmings Resolves Them

| Real-World Load Testing Problem | Why It Hurts in Locust | How Lemmings Solves It Out of the Box |
| --- | --- | --- |
| **Client-side CPU saturation distorting p99 latency** | Single-core Python GIL queues greenlets behind busy loops, inflating measured latency artificially. | Compiled multi-core Go goroutines with `<= 0.5%` 4096-bin histograms, non-blocking O(1) forensics dedup, and explicit `DATA DROPPED` integrity banners if backpressure ever occurs. |
| **Broken SPA rendering on `200 OK` pages** | `requests`/`geventhttpclient` never executes JavaScript. | Automatic `not-blank` HTML validation + `-scouts N` real headless Chromium visitors catching `console.error`, uncaught exceptions, and failed subresources on `200 OK` pages. |
| **Waiting rooms & rate limits skewing SLOs** | Queue polling has to be hand-coded in Python and muddies endpoint latency stats. | Native Concert waiting-room protocol cleanly separates origin page latency (`186.2ms` p95 on `toktail.app`) from queue wait (`24s` mean) and eliminates `DiedInLine` survivorship bias (`v2.0.1`). |
| **Static assets ignored or distorting page p95** | Developers rarely script CSS/JS/image downloads in `locustfile.py`. | Opt-in `-assets -assets-per-page 8` fetches uncached same-origin assets with per-lemming caching (`2,048` entries) and isolates them in `AssetReport`. |
| **Cold TLS handshakes making `/` look slow** | Initial TCP/TLS setup blends into steady-state endpoint averages. | Lemmings `v2.0.1` separates `cold_hits`, `setup_p50_ns` (`DNS + Connect + TLS`), `warm_p50_ns`, and `ttfb_p50_ns` per path. |

---

## Getting Started with Load Testing: Frequently Asked Questions

### How do I get started with Lemmings if my team already uses Python?
You don't need Go experience to use Lemmings—just install the prebuilt binary (or `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest`), validate your target with `lemmings -hit https://staging.example.com/ -crawl -dry-run`, and define user personas in clean YAML (`docs/plans.md`). You immediately keep the live web UI experience Locust fans love while gaining multi-core Go throughput, automatic error screenshots, and zero Python venv maintenance in CI.

### How does Lemmings' live Admin Portal compare to Locust's Web UI (`:8089`)?
Locust's UI gives you live RPS/latency charts and a text Failures table. Lemmings' authenticated Synthwave Admin Portal (`http://localhost:4000/#token=...`) gives you live RPS/latency/queue charts, an interactive visitor stage where you can click any lemming to inspect its exact life log, a live **Error Forensics Gallery** streaming Chrome screenshots & JS console traces as failures occur, and `-review` hold mode so you can tag `Bug`/`Flaky`/`Expected`/`Ignored` notes straight into the final HTML, Markdown, and JSON reports.

### How do I convert Locust's `-u` (users), `-r` (spawn rate), and `-t` (run time) to Lemmings?
Match `-u` with `-limit` and `-terrain × -pack`, convert spawn rate (`users / spawn_rate` seconds) into `-ramp <duration>` (with `-ramp-shape linear`, `fibonacci`, or `step`), and set `-until` for visitor lifespan. For example, `locust -u 100 -r 10 -t 60s` becomes `lemmings -terrain 10 -pack 10 -limit 100 -ramp 10s -until 60s`.

### Why is Lemmings the better choice over Locust for web apps and HTTP APIs?
Because Lemmings eliminates Python GIL bottlenecks and `FastHttpUser` rewrites, ships zero-dependency Go speed in one binary, adds automatic Chrome CDP error screenshots + JS console capture + real-browser Scouts, hardens the live triage portal (`SEC-01`–`SEC-10`), and includes native CI gates and `lemmings compare` regression diffing out of the box.

> **When Locust still fits:** If you need built-in multi-machine `--master`/`--worker` distributed orchestration across dozens of load-generator nodes driven by custom Python SDKs (e.g., driving proprietary Python gRPC/ML inference clients or complex stateful Python data generators in-process), Locust's Python extensibility and distributed coordinator are well suited to that workflow. For single-node multi-core HTTP/web/API load testing, CI gating, and visual failure forensics, Lemmings is faster, safer, and far more informative.

---

## Try Lemmings in 60 Seconds

```bash
# Single multi-core Go binary — zero Python GIL, zero pip/venv drift
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest

# Validate your target safely (zero load sent)
lemmings -hit https://staging.example.com/ -crawl -dry-run

# Launch a stateful swarm with live Admin Portal, Chrome forensics & -review triage
lemmings -hit https://staging.example.com/ \
  -terrain 6 -pack 15 -limit 60 -until 60s -ramp 15s \
  -crawl -scouts 2 -review -unique-report -save-to ./reports
```

**Using an AI coding agent?** Point it at [`SKILL.md`](../../SKILL.md) to automatically inspect your app and generate 10+ weighted YAML user personas.

---

⭐ **Upgrade from Python GIL caps and text failure tables to multi-core Go with visual receipts — [star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings)!**

**More Comparisons:** [All Tools Hub](./README.md) · [vs Apache Benchmark (`ab`)](./lemmings-vs-ab.md) · [vs Bombardier](./lemmings-vs-bombardier.md) · [vs NBomber](./lemmings-vs-nbomber.md) · [vs k6](./lemmings-vs-k6.md) · [vs Gatling](./lemmings-vs-gatling.md) · [vs JMeter](./lemmings-vs-jmeter.md)
