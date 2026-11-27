# Lemmings vs Grafana k6: Permissive Apache-2.0, Built-In Visual Forensics & Triage Portal vs. AGPL JS Scripting

[![GitHub Repo stars](https://img.shields.io/github/stars/andreimerlescu/lemmings?style=social)](https://github.com/andreimerlescu/lemmings)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-36f9f6)](../../LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)

> **TL;DR Verdict:** **Lemmings is the better all-in-one load testing tool than Grafana k6 for web applications, HTTP APIs, and CI teams that want visual debugging without commercial cloud lock-in or AGPL-3.0 licensing.** While k6 forces you to maintain Goja JavaScript scripts, wire custom `handleSummary()` hooks for HTML reports, compile custom `xk6` binaries for extra sinks, and pay for Grafana Cloud k6 to get a team dashboard and historical diffing, **Lemmings v2.0.1** ships everything in one **Apache-2.0** Go binary: strict multi-persona YAML plans + `5%` automatic rogue crawling, automatic 4-part Chrome DevTools Protocol (CDP) error screenshots & JS console capture (zero Node.js), a free authenticated live Admin Triage Portal (`-review`), self-contained HTML/Markdown/JSON reports, and built-in `lemmings compare` regression diffing.
>
> ⭐ **Want k6-grade Go performance with Apache-2.0 freedom and automatic visual receipts?** [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to help more developers and AI agents discover it.

| Quick Decision Summary | **Lemmings (`v2.0.1`)** | **Grafana k6** |
| --- | --- | --- |
| **License & Paywalls** | **Apache-2.0** — live Admin Portal, HTML/MD/JSON reports, S3/SMTP delivery, Prometheus, and `lemmings compare` are all 100% free | **AGPL-3.0** — team UI, historical comparison, and rich cloud reporting push you toward paid Grafana Cloud k6 |
| **Test Authoring** | Zero-code `-crawl` mode + strict `KnownFields(true)` YAML `-plan` suites (up to 100 personas) | JavaScript / TypeScript scripts executed inside an embedded **Goja** JS VM (not Node.js) |
| **When Something Breaks** | Automatic CDP error screenshot + JS console + cookie jar + parsed error body + live Admin Triage Modal | Terminal check count (`checks... 91%`); screenshots require manually writing `k6/browser` Playwright-like scripts |
| **Install in 10 Seconds** | `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest` | `brew install k6` / OS package / Docker |

---

## Side-by-Side Feature Comparison (`Lemmings` vs `k6`)

| Capability | Lemmings (`v2.0.1`) | Grafana k6 |
| --- | :---: | :---: |
| Permissive commercial-friendly license (no copyleft) | ✅ (Apache-2.0) | ❌ (AGPL-3.0 copyleft) |
| Single standalone Go binary (zero Node.js / JVM / Python) | ✅ (`20–23 MB`) | ✅ (Go binary + embedded Goja JS VM) |
| Zero-script automatic site crawl & organic link navigation | ✅ (`-crawl`, `-navigation links`) | ❌ (every URL must be scripted in JS) |
| Strict declarative multi-persona YAML plans (up to 100 personas) | ✅ (`KnownFields(true)` + `-dry-run`) | ❌ (imperative JavaScript only) |
| Automatic `5%` rogue wanderer cohort (`-rogue-rate`) | ✅ | ❌ |
| Per-visitor stateful RFC 6265 cookie jars (`InspectingJar`) | ✅ (with cookie health + HMAC audit) | ✅ (`http.cookieJar()`, no forensic audit UI) |
| Automatic `200 OK` blank-page & soft-error detection | ✅ (built-in `not-blank` + `want-*`) | ❌ (manual `check()` JS code on every call) |
| Automatic error screenshots on HTTP failures (zero manual browser script) | ✅ (pure-Go CDP replay + dedup) | ❌ (must hand-code `k6/browser` + `page.screenshot()`) |
| Automatic JS console & render error capture alongside HTTP swarm | ✅ (`-scouts N` + CDP replay) | ❌ (manual `k6/browser` scenario scripting required) |
| Live local Admin Triage Portal (`Bug`/`Flaky`/`Expected`/`Ignored`) & `-review` | ✅ (built-in, authenticated loopback) | ❌ (basic `K6_WEB_DASHBOARD` metrics only; no triage or screenshots) |
| Self-contained HTML (inline screenshots), GitHub Markdown & JSON reports | ✅ (built-in out of the box) | ❌ (requires custom `handleSummary()` JS & third-party templates) |
| Built-in CLI baseline vs. candidate regression diff (`lemmings compare`) | ✅ (exits `2` on regressions + error diffs) | ❌ (requires Grafana Cloud k6 or custom external diff scripts) |
| Deterministic CI pass/fail gates (`exit 2`) | ✅ (`-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`, queue gates) | ✅ (`thresholds` in JS, exits `99`) |
| Native virtual waiting-room protocol (Concert queue separation) | ✅ | ❌ (must hand-code queue polling & custom Trend metrics) |
| Opt-in same-origin static asset tier (`-assets`, isolated from page SLOs) | ✅ | ❌ (manual `http.batch()` scripting) |
| Built-in Prometheus `/metrics` scrape exporter (no `xk6` rebuild) | ✅ (`-observe`, `24` metrics, 999-URL cap) | ❌ (remote-write push or custom `xk6` build required) |
| Built-in S3 & hardened TLS 1.2+ SMTP report delivery | ✅ (`-save-to s3://...,mailto:...`) | ❌ (custom CI upload scripts required) |
| Security hardened (SSRF dial guard, secret scrubbing, CycloneDX SBOM) | ✅ (`SEC-01`–`SEC-10`) | ❌ (no built-in IMDS dial guard or report secret scrubber) |
| Non-HTTP protocols (gRPC, WebSockets, custom `xk6` extensions) | ❌ (HTTP/HTTPS web & API focused) | ✅ (built-in gRPC/WebSockets + `xk6` ecosystem) |
| AI agent plan-authoring runbook (`SKILL.md`) | ✅ | ❌ |

---

## Performance & Real-World Output Side by Side

Both Lemmings and k6 are compiled in Go—giving both strong single-node concurrency. The critical engineering difference is **per-VU memory/runtime overhead** (Lemmings runs lightweight pure-Go goroutines with zero JavaScript VM, whereas k6 embeds a Goja ES5/ES6 JavaScript runtime per Virtual User) and **what happens when a target fails under real load**.

### Real Verified Lemmings Run Data (`396` Stateful Visitors · `3m0s`)

| Metric | Lemmings on `counthis.app` *(Standard Web App)* | Lemmings on `toktail.app` *(Concert Queue + 503 Chaos)* | What k6 Requires / Captures on These Targets |
| --- | --- | --- | --- |
| **Authoring & Setup** | **0 lines of JS** (`-crawl -navigation links`); or declarative YAML `-plan` | **0 lines of JS** (`-crawl -navigation links`) | Must write JavaScript `default function()`, parse HTML links manually with `k6/html`, and author custom `handleSummary()` for HTML/MD output |
| **Visitors & Rescued Score** | `396/396` completed · **`100.0%` rescued** (`384` peak alive) | `396/396` completed · **`89.1%` rescued** (`353/396` flawless, `384` peak alive) | Reports raw `iterations` and `http_reqs`; no built-in per-visitor rescued journey metric without custom `Rate` counters |
| **Visits & Throughput** | **`10,831` page visits** across `10` crawled paths · `60.2` visits/s avg (**`144` visits/s peak**) · `76.15 MB` | **`3,110` page visits** across `4` paths · `17.3` visits/s avg (`77` visits/s peak) · `36.87 MB` | Strong Go HTTP engine, but each VU allocates a Goja JS runtime (typically ~1–5 MB/VU vs. kilobytes per pure-Go lemming) |
| **Origin Page Latency** | **p50 `35.8ms`** · p90 `123.8ms` · **p95 `185.4ms`** · p99 `607.8ms` · **TTFB p95 `144.5ms`** (`<= 0.5%` 4096-bin histogram) | **p50 `43.0ms`** · p90 `69.3ms` · **p95 `186.2ms`** · p99 `206.0ms` · **TTFB p95 `156.5ms`** *(cleanly separated from queue wait)* | Tracks `http_req_duration` globally; mixes waiting-room responses into `http_req_duration` unless you manually tag custom `Trend` metrics in JS |
| **Waiting Room & Queueing** | `0` held | **`1,093` held** (`729` admitted, `364` died in line, **`11,685` `/queue/status` polls**, max pos `359`, mean wait `24s`, `3,026` passes) | **Must be hand-coded in JS** (DOM/JSON detection, `room_ticket` polling loop, `Retry-After` backoff, admission reload, and survivorship-bias handling) |
| **Error Forensics & Visual Receipts** | `0` errors · `0` dropped lifelogs | `313` HTTP `503`s (`11.4%`) → **`12` deduplicated 4-part artifacts** + **`12` Chrome CDP screenshots** (`301` deduped, `0` dropped) + JS console `404` on `/favicon.ico` | Prints `http_req_failed: 11.4%`. **Zero screenshots, zero extracted HTML bodies, zero cookie jar snapshot, zero JS console errors** unless you rewrite your test in `k6/browser` |

### Runtime & Architecture Footprint Comparison

| Dimension | Lemmings (`v2.0.1`) | Grafana k6 |
| --- | --- | --- |
| **Per-VU Engine** | Pure Go goroutines (zero JS interpreter, zero GC churn from JS objects); `256`-cookie cap & `2048`-asset cache per visitor | Embedded Goja JavaScript VM initialized per VU (noticeable RAM climb at 500–2,000+ VUs) |
| **Histogram Precision** | Exact sorted samples up to `8,192` global (`1,024`/path), then fixed `32 KiB` 4096-bin `uint64` histogram (`<= 0.5%` error) | Exact/reservoir summary stats at end of run (high memory when exporting raw JSON metrics) |
| **Browser Diagnostics** | Hybrid architecture: high-scale Go HTTP swarm triggers O(1)-deduped async Chrome CDP replays on failure + optional `-scouts` | Either protocol-only (`k6/http`, zero visuals) OR heavy full-browser (`k6/browser`, manual Playwright API scripting) |
| **Release Diffing** | Native `lemmings compare baseline.json candidate.json` with exit code `2` & error-fingerprint diff | Not built into OSS CLI; requires Grafana Cloud k6 or custom scripting |

### Automatic Visual Receipts vs. Terminal `http_req_failed` Percentages

When `toktail.app` suffered `313` HTTP `503`s, k6 would print `http_req_failed: 11.40%` and leave your team digging through server logs. Lemmings immediately streamed 12 deduplicated Chrome CDP screenshots, cookie jar states, visitor trails, extracted HTML error headlines, and browser console errors (`favicon.ico` `404`) into the built-in Admin Triage Portal and self-contained HTML reports:

| Live Admin Triage Portal (`http://localhost:4000`) | Self-Contained HTML Report with Embedded Screenshots |
| :---: | :---: |
| ![Lemmings Admin Portal — Dark Mode](../images/dashboard-dark.jpg) | ![Lemmings HTML Report](../images/report.jpg) |

---

## Conventions Translation: How k6 Concepts Map to Lemmings

Every core k6 concept maps cleanly to concise, strict Lemmings flags and YAML fields—with zero JavaScript boilerplate:

| Grafana k6 Convention (JavaScript) | Lemmings (`v2.0.1`) Equivalent | Why Lemmings Is Better |
| --- | --- | --- |
| `options.vus` / `options.duration` | `-terrain`, `-pack`, `-limit`, `-until` | Bounded goroutine terrains with explicit start/complete/unborn accounting |
| `options.stages` (ramp up/down) | `-ramp 30s -ramp-shape linear\|fibonacci\|step` | Zero script edits needed to change load pacing curves |
| `options.scenarios` (multi-workload) | Multi-persona YAML `plans:` with `weight` + automatic `5%` `-rogue-rate` | Deterministic weight math (`slot = planCohortIndex % TotalWeight`) + organic crawl coverage |
| `http.get()` / `http.post()` / `http.patch()` | YAML `method: GET\|POST\|PUT\|PATCH\|DELETE\|HEAD\|OPTIONS` | Auto form/JSON `with-body`, raw `body:`, and per-step `with-headers` |
| `http.cookieJar()` | Automatic per-lemming `InspectingJar` + `with-cookies` / `want-cookies` | Enforces RFC 6265 scoping, tracks cookie health, and redacts raw values in reports (`SEC-10`) |
| `check(res, { ... })` | `want-status`, `want-headers`, `want-cookies`, `want-body`, `want-title`, `want-selectors` | Automatically triggers 4-part forensics & counts toward `-min-plan-pass-rate` (unlike k6 `check()`!) |
| `options.thresholds` (`http_req_duration: ['p(95)<500']`) | `-p95-budget 500ms -max-failure-rate 0.01 -min-plan-pass-rate 0.99` | Native CLI flags + waiting-room gates (`-min-queue-held`, `-max-queue-wait`) + `lemmings compare` |
| `sleep(1.5)` | `chill: 1500ms` (per-step) or `-think-min 300ms -think-max 1200ms` | Realistic randomized think time by default, overrideable per step |
| `__ENV.API_TOKEN` | `"Bearer ${API_TOKEN}"` in YAML `with-headers`/`with-body`/`body` | Fails closed at startup if unset (exit `1`) and tagged `json:"-"` so secrets never leak |
| `k6/browser` manual script | Built-in `-capture=true` (auto CDP replay on failures) + `-scouts 2` | Zero Playwright-style browser code to write or maintain |
| `handleSummary(data)` custom JS | Built-in `-save-to ./reports,s3://...,mailto:...` | Emits self-contained HTML, GitHub Markdown, and JSON (`schema 2.0`) automatically |

### Side-by-Side Example: Weighted Multi-Persona Auth & Checkout Suite

**Grafana k6 (`script.js` — imperative JS, manual checks don't fail run unless duplicated into thresholds, zero screenshots, zero HTML report):**
```javascript
import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  scenarios: {
    browsers: { executor: 'constant-vus', vus: 28, duration: '60s', exec: 'browse' },
    buyers:   { executor: 'constant-vus', vus: 12, duration: '60s', exec: 'buy' },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<750'],
    checks: ['rate>0.99'],
  },
};

export function browse() {
  const res = http.get('https://staging.example.com/');
  check(res, { 'status 200': (r) => r.status === 200, 'no soft error': (r) => !r.body.includes('Something went wrong') });
  sleep(1);
}

export function buy() {
  const login = http.post('https://staging.example.com/login', {
    username: __ENV.DEMO_USER,
    password: __ENV.DEMO_PASS,
  });
  check(login, { 'logged in': (r) => r.status === 200 && r.cookies.session_id !== undefined });
  const checkout = http.get('https://staging.example.com/checkout');
  check(checkout, { 'checkout ok': (r) => r.status === 200 && r.body.includes('Order Summary') });
  sleep(1);
}
```

**Lemmings (Strict Multi-Persona YAML + Automatic 5% Rogues + CDP Screenshots + HTML/MD/JSON + CI Gates):**

Save as `suite.yaml`:
```yaml
plans:
  - name: anonymous-browser
    weight: 70
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

  - name: checkout-buyer
    weight: 30
    stop-on-fail: true
    loop: true
    plan:
      - name: authenticate
        path: /login
        method: POST
        chill: 500ms
        with-body:
          - key: username
            value: "${DEMO_USER}"
          - key: password
            value: "${DEMO_PASS}"
        want-status: 200
        want-cookies:
          - name: session_id
      - name: load-checkout
        path: /checkout
        method: GET
        chill: 1s
        want-status: 200
        want-cookies:
          - name: session_id
        want-body:
          - contains: Order Summary
            not-contains: Something went wrong
```

Run with pre-flight `-dry-run`, Browser Scouts, CI gates, and baseline diff:
```bash
export DEMO_USER="player1"
export DEMO_PASS="staging-pass"
lemmings -hit https://staging.example.com/ -plan suite.yaml -dry-run

lemmings \
  -hit https://staging.example.com/ \
  -plan suite.yaml \
  -terrain 4 -pack 10 -limit 40 -until 60s -ramp 10s \
  -scouts 2 \
  -min-plan-pass-rate 0.99 -max-failure-rate 0.01 -p95-budget 750ms \
  -tty=false -unique-report -save-to ./reports
```

---

## Grafana k6 Gotchas & Pitfalls — Solved Out of the Box by Lemmings

1. **The Famous "`check()` Failed, Yet k6 Exited `0`!" Trap**
   - **k6 pitfall:** In k6, failing `check()` assertions do **not** fail the test or return a non-zero exit code unless you remember to wire a separate `checks: ['rate>0.99']` threshold in `options.thresholds`. Countless teams have shipped green CI pipelines whose `check()` assertions silently failed.
   - **How Lemmings solves it:** Every failed `want-*` assertion immediately increments `failed_visits`, marks the lemming's plan outcome as failed (gated by `-min-plan-pass-rate` and `-max-failure-rate` with exit code `2`), and automatically triggers a 4-part Error Forensics capture.
2. **Goja Is NOT Node.js (Silent npm Compatibility Traps)**
   - **k6 pitfall:** k6's embedded Goja JS VM lacks Node built-ins (`fs`, `crypto`, `path`, async `fetch`), forcing awkward esbuild/webpack bundler pipelines just to share helper libraries—while still burning megabytes of RAM per VU for JS contexts.
   - **How Lemmings solves it:** Pure Go runtime with zero JavaScript VM overhead and strict `KnownFields(true)` YAML plans validated in milliseconds via `-dry-run`.
3. **AGPL-3.0 Copyleft & Grafana Cloud Upsell for Visuals / Diffs**
   - **k6 pitfall:** Core k6 is licensed under **AGPL-3.0** (raising legal flags if embedded in internal SaaS or developer portals), and rich historical reports, team dashboards, and run-to-run diffs nudge you toward paid Grafana Cloud k6.
   - **How Lemmings solves it:** **Apache-2.0** forever, with a free local Synthwave Admin Triage Portal, self-contained HTML/Markdown/JSON reports, S3/SMTP delivery, Prometheus `/metrics`, and `lemmings compare` included in the single binary.
4. **No Automatic Failure Screenshots (`k6/browser` Is Heavy Manual Scripting)**
   - **k6 pitfall:** Running `k6/http` gives zero screenshots or JS console diagnostics when a page breaks. Turning on `k6/browser` means rewriting your load test in Playwright-style browser code and paying full browser CPU/RAM for every VU.
   - **How Lemmings solves it:** Combines the best of both worlds automatically—your high-scale pure-Go HTTP swarm runs at full speed, triggers bounded async Chrome CDP replays *only* on failures (deduplicated & non-blocking), and optionally runs `1..8` real-browser `-scouts` concurrently to catch client-side JS errors on `200 OK` pages.
5. **No Native HTML/Markdown Reporting or `xk6` Recompile Pain**
   - **k6 pitfall:** Generating a clean shareable HTML report in k6 requires pulling third-party `k6-reporter` scripts from remote URLs inside `handleSummary()`. Exporting Prometheus scrapes or custom sinks often requires building a custom binary with `xk6`.
   - **How Lemmings solves it:** Every single run emits self-contained HTML (inline screenshots), GitHub-ready Markdown (`.artifacts/` images), and lean JSON (`schema 2.0`) with zero external plugins, plus `-observe` exposes 24 Prometheus metrics directly.
6. **Zero Unscripted Discovery (No Crawling or Rogue Cohort)**
   - **k6 pitfall:** k6 only hits URLs explicitly coded in JavaScript—broken links, dead routes, and CMS regressions outside your hardcoded script stay invisible.
   - **How Lemmings solves it:** Supports instant zero-script `-crawl` exploration AND pairs every YAML `-plan` suite with a deterministic `5%` rogue cohort (`-rogue-rate`) that roams your site organically while being tracked separately from `-min-plan-pass-rate`.

---

## Common Load-Testing Problems & How Lemmings Resolves Them

| Real-World Load Testing Problem | Why It Hurts in k6 | How Lemmings Solves It Out of the Box |
| --- | --- | --- |
| **Debugging `5xx` or failed checks in CI** | CI logs show `http_req_failed: 4.2%` with no screenshot, cookie state, or DOM dump. | Upload the `./reports` artifact from GitHub Actions (`docs/reports-and-ci-gates.md`) and open the self-contained HTML or Markdown report with screenshots, JS console errors, cookie jar health, and parsed error bodies already attached. |
| **Keeping load tests up to date as routes change** | Hand-written JS scripts rot quickly when frontend/API endpoints evolve. | Clean YAML plans + [`SKILL.md`](../../SKILL.md) let AI coding agents inspect your routes/templates and generate 10+ SEC-10-safe personas automatically. |
| **Waiting rooms & rate limiters skewing p95** | Queued responses inflate `http_req_duration` or force complex custom JS state machines. | Native Concert waiting-room protocol separates origin page latency (`186.2ms` p95 on `toktail.app`) from queue wait (`24s` mean), prevents survivorship bias (`v2.0.1`), and adds `-min-queue-held` / `-max-queue-wait` CI gates. |
| **Catching release-to-release regressions automatically** | OSS k6 lacks a built-in CLI diff tool for comparing baseline vs. candidate runs. | `lemmings compare -max-p95-delta 100ms -max-failure-delta 0.01 -max-rescued-drop 2.0 baseline.json candidate.json` fails CI with exit code `2` and lists new (`+`) vs. resolved (`-`) error fingerprints. |
| **Preventing secret leaks & SSRF in shared CI runners** | Raw `console.log` or custom summary scripts easily leak tokens; open redirects can hit cloud IMDS. | Audited `SEC-01`–`SEC-10`: `SafeDialContext` blocks `169.254.0.0/16` IMDS, `json:"-"` excludes all `with-*`/`body` and cookies from reports/traces, and `secrets.ScrubText` redacts extracted error bodies & console logs. |

---

## Getting Started with Load Testing: Frequently Asked Questions

### How do I start load testing my website with Lemmings if I've never used a load tester before?
1. Install Lemmings (`go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest`).
2. Try the built-in local demo first (`make demo` + `make run-forensics`) to watch real lemmings, error screenshots, JS console crashes, and triage tags in the Admin Portal (`http://localhost:4000/#token=...`).
3. Run `lemmings -hit https://staging.example.com/ -crawl -dry-run` against your own staging app to validate with zero traffic.
4. Start small (`-terrain 2 -pack 5 -limit 10 -until 30s -crawl -review`), then graduate to version-controlled YAML plans (`docs/plans.md`) and CI gates.

### How do I migrate my existing k6 JavaScript tests to Lemmings?
Map each k6 `scenario` to a persona inside a `plans:` YAML bundle (using `weight` instead of manual VU splits), convert `http.get`/`http.post` calls to `PlanItem` steps (`method`, `path`, `with-headers`, `with-body`), convert `check()` predicates into `want-status`/`want-body`/`want-headers`/`want-cookies`/`want-selectors`, and move `options.thresholds` to CLI flags (`-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`). Validate every line in one second with `-dry-run`.

### Does Lemmings support client-side browser testing like `k6/browser`?
Even better for most teams: instead of forcing you to write separate Playwright-style browser load scripts, Lemmings **automatically replays failed HTTP `GET` visits in isolated headless Chromium contexts over pure-Go CDP** (capturing screenshots + JS console traces with zero Node.js), AND lets you add `-scouts 1`..`8` real-browser visitors alongside your HTTP swarm to catch pure JS/hydration errors on `200 OK` pages automatically.

### Why is Lemmings a better default choice than k6 for web apps and HTTP APIs?
Because Lemmings gives you pure-Go speed under a permissive **Apache-2.0** license without JavaScript VM bloat, eliminates `check()` false-green traps, requires zero custom summary/reporting scripts, delivers automatic visual error screenshots and a live triage portal out of the box, and includes free local `lemmings compare` CI regression diffing.

> **When k6 still fits:** If you need native non-HTTP protocol load testing such as **gRPC**, **WebSockets**, or custom `xk6` extensions (e.g., Kafka/Redis/SQL drivers inside JS scripts), or you are already standardized on paid Grafana Cloud k6 for distributed cloud-managed execution, k6 has strong ecosystem integrations for those protocols. For HTTP/HTTPS websites, web apps, and REST APIs, Lemmings delivers far faster time-to-value and superior failure diagnostics with zero paywalls.

---

## Try Lemmings in 60 Seconds

```bash
# Permissive Apache-2.0 single binary — zero Node.js, zero AGPL, zero cloud paywalls
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest

# Validate your YAML plan suite pre-flight (zero load)
lemmings -hit https://staging.example.com/ -plan examples/plans/neon-arcade-personas.yaml -dry-run

# Run with real-browser Scouts, CI gates, and self-contained HTML/MD/JSON reports
lemmings -hit https://staging.example.com/ \
  -plan examples/plans/neon-arcade-personas.yaml \
  -terrain 8 -pack 15 -limit 80 -until 60s -ramp 15s -ramp-shape fibonacci \
  -scouts 2 -min-plan-pass-rate 0.99 -max-failure-rate 0.01 -p95-budget 750ms \
  -tty=false -unique-report -save-to ./reports
```

**Using an AI coding agent?** Point it at [`SKILL.md`](../../SKILL.md) to auto-generate a complete 10-persona SEC-10-safe YAML plan suite from your codebase.

---

⭐ **Skip AGPL lock-in and blind `http_req_failed` logs — [star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings)!**

**More Comparisons:** [All Tools Hub](./README.md) · [vs Apache Benchmark (`ab`)](./lemmings-vs-ab.md) · [vs Bombardier](./lemmings-vs-bombardier.md) · [vs NBomber](./lemmings-vs-nbomber.md) · [vs Gatling](./lemmings-vs-gatling.md) · [vs Locust](./lemmings-vs-locust.md) · [vs JMeter](./lemmings-vs-jmeter.md)
