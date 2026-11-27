# Lemmings vs Gatling: Instant Zero-JVM Go Binary & Free Live Triage Portal vs. Heavy JVM Toolchains & Enterprise Paywalls

[![GitHub Repo stars](https://img.shields.io/github/stars/andreimerlescu/lemmings?style=social)](https://github.com/andreimerlescu/lemmings)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-36f9f6)](../../LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)

> **TL;DR Verdict:** **Lemmings is the better load testing tool than Gatling for modern web applications, HTTP APIs, and agile CI/CD teams.** Gatling requires a full JDK/JVM + Maven/Gradle/sbt (or Node.js) build project, suffers JVM startup/GC overhead, gates live real-time dashboards and run-to-run comparisons behind paid **Gatling Enterprise**, and offers zero browser error screenshots or JS console forensics. **Lemmings v2.0.1** runs instantly from a single `20–23 MB` Apache-2.0 Go binary with zero JVM, models weighted multi-persona traffic in strict YAML (`-plan`) + a `5%` rogue crawler cohort, captures automatic 4-part Chrome DevTools Protocol (CDP) error screenshots & JS console traces, serves a free live Admin Triage Portal (`-review`), and diffs candidate releases natively with `lemmings compare`.
>
> ⭐ **Done waiting on Maven/Gradle builds just to load-test a staging URL?** [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to help more engineers find it.

| Quick Decision Summary | **Lemmings (`v2.0.1`)** | **Gatling (OSS)** |
| --- | --- | --- |
| **Runtime & Build Tax** | Single `20–23 MB` Go binary — zero JDK, zero Maven/Gradle/sbt, zero compilation | Requires JDK 17+ / JVM + Maven, Gradle, sbt, or Node.js SDK project + compilation |
| **Live UI & Run Diffing** | **100% Free** — authenticated live Admin Portal + `-review` triage + `lemmings compare` CLI | Live real-time dashboards, team views & trend comparison require paid **Gatling Enterprise** |
| **When Something Breaks** | Automatic CDP screenshot + JS console + cookie jar + parsed error body + 4-panel Inspector Modal | Static post-run HTML report showing HTTP status/assertion counts (zero screenshots or JS logs) |
| **Install in 10 Seconds** | `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest` | Clone Maven/Gradle/JS starter repo + install JDK + resolve dependencies |

---

## Side-by-Side Feature Comparison (`Lemmings` vs `Gatling`)

| Capability | Lemmings (`v2.0.1`) | Gatling (OSS) |
| --- | :---: | :---: |
| Permissive Apache-2.0 open-source license | ✅ | ✅ (Enterprise features proprietary/paid) |
| Single standalone binary (zero JVM / Node.js / build tools) | ✅ (`20–23 MB` Go binary) | ❌ (requires JDK + Maven/Gradle/sbt or Node.js) |
| Zero-script site crawl & organic link navigation | ✅ (`-crawl`, `-navigation links`) | ❌ (every request must be coded in DSL) |
| Strict declarative YAML plans (readable by any dev or AI) | ✅ (`KnownFields(true)` + `-dry-run`) | ❌ (Scala / Java / Kotlin / JS DSL code) |
| Weighted multi-persona suites + built-in `5%` rogue cohort | ✅ (`plans:` + `-rogue-rate`) | ❌ (manual population setup; no rogue crawler) |
| Per-visitor stateful RFC 6265 cookie jars (`InspectingJar`) | ✅ (with cookie health + HMAC audit) | ✅ (per-user session cookies; no forensic UI) |
| Response assertions (status, headers, cookies, body, DOM) | ✅ (`want-*` + automatic `not-blank`) | ✅ (`check(status(), substring(), css())` in DSL) |
| Automatic error screenshots (pure-Go CDP, zero Node.js) | ✅ | ❌ |
| Real-browser Scouts (`-scouts N`) for client-side JS/render crashes | ✅ | ❌ |
| Free live Admin Triage Portal (`Bug`/`Flaky`/`Expected`/`Ignored`) & `-review` | ✅ (included in binary) | ❌ (paid Gatling Enterprise only; no screenshot triage) |
| Self-contained HTML, GitHub Markdown & JSON (`schema 2.0`) reports | ✅ (HTML + MD + JSON + `.artifacts/`) | ✅ (multi-file HTML report only; no Markdown/screenshots) |
| Built-in CLI baseline vs. candidate regression diff (`lemmings compare`) | ✅ (free in CLI, exits `2` on regression) | ❌ (paid Gatling Enterprise only) |
| Deterministic CI pass/fail gates (`exit 2`) | ✅ (CLI flags for failure, p95, plan pass, queues) | ✅ (DSL `assertions` compiled into simulation) |
| Native virtual waiting-room protocol (Concert queue separation) | ✅ | ❌ |
| Opt-in same-origin static asset tier (`-assets`, isolated from page SLOs) | ✅ | ✅ (`inferHtmlResources()`, mixes unless filtered) |
| Isolated Prometheus `/metrics` scrape exporter (`24` metrics) | ✅ (`-observe`) | ❌ (Graphite/Enterprise metrics; no native `/metrics`) |
| Security hardened (SSRF dial guard, secret scrubbing, CycloneDX SBOM) | ✅ (`SEC-01`–`SEC-10`) | ❌ |
| Non-HTTP protocols (JMS, MQTT, gRPC via Gatling plugins) | ❌ (HTTP/HTTPS web & API focused) | ✅ (Enterprise / JVM protocol modules) |
| AI agent plan-authoring runbook (`SKILL.md`) | ✅ | ❌ |

---

## Performance & Real-World Output Side by Side

Gatling's Netty/JVM async engine is capable of high throughput once warmed up, but pays a steep operational tax: heavy JDK container images, build/compile cycles, JVM heap tuning (`-Xmx`), garbage-collection pause risk, and post-run-only static HTML charts with zero browser forensics. Below is **verified real Lemmings run data (`counthis.app` and `toktail.app`)** contrasted against Gatling OSS's documented architecture and visibility gaps.

### Real Verified Lemmings Run Data (`396` Stateful Visitors · `3m0s`)

| Metric | Lemmings on `counthis.app` *(Standard Web App)* | Lemmings on `toktail.app` *(Concert Queue + 503 Chaos)* | What Gatling OSS Requires / Captures on These Targets |
| --- | --- | --- | --- |
| **Time to First Run** | **10 seconds** (`go install` + 1 CLI command; zero code via `-crawl`) | **10 seconds** (1 CLI command) | Install JDK 17+, scaffold Maven/Gradle project, author `Simulation` class, compile, run `mvn gatling:test` |
| **Visitors & Rescued Score** | `396/396` completed · **`100.0%` rescued** (`384` peak alive) | `396/396` completed · **`89.1%` rescued** (`353/396` flawless, `384` peak alive) | Tracks OK/KO request counts; no per-visitor rescued journey score or notable-user traces |
| **Visits & Throughput** | **`10,831` visits** across `10` auto-crawled routes · `60.2` visits/s avg (**`144` visits/s peak**) · `76.15 MB` | **`3,110` visits** across `4` routes · `17.3` visits/s avg (`77` visits/s peak) · `36.87 MB` | High async Netty throughput once JVM warms up, but hits only explicitly coded endpoints |
| **Origin Page Latency** | **p50 `35.8ms`** · p90 `123.8ms` · **p95 `185.4ms`** · p99 `607.8ms` · **TTFB p95 `144.5ms`** (`<= 0.5%` 4096-bin histogram) | **p50 `43.0ms`** · p90 `69.3ms` · **p95 `186.2ms`** · p99 `206.0ms` · **TTFB p95 `156.5ms`** *(isolated from queue wait)* | HdrHistogram percentiles in static post-run HTML; waiting-room loops distort response time unless manually isolated in custom groups |
| **Waiting Room & Queueing** | `0` held | **`1,093` held** (`729` admitted, `364` died in line, **`11,685` `/queue/status` polls**, max pos `359`, mean wait `24s`, `3,026` passes) | **Must be coded manually** with `asLongAs` polling loops, regex/JSON extractors, session variables, and custom backoff logic |
| **Error Forensics & Visual Receipts** | `0` errors · `0` dropped lifelogs | `313` HTTP `503`s (`11.4%`) → **`12` deduplicated 4-part artifacts** + **`12` Chrome CDP screenshots** (`301` deduped, `0` dropped) + JS console `404` on `/favicon.ico` | Lists `status.find.in(200..210), but actually found 503 (313)` in HTML table. **Zero screenshots, zero extracted error bodies, zero JS console output, zero live triage UI** |

### Runtime & Architecture Footprint Comparison

| Dimension | Lemmings (`v2.0.1`) | Gatling (OSS) |
| --- | --- | --- |
| **Binary & CI Container Size** | `20–23 MB` single static Go binary (`CGO_ENABLED=0`) | `300–500+ MB` (JDK + Maven/Gradle + `.m2` dependency cache) |
| **Startup & Cold Start** | Instant (<50ms) + `-dry-run` pre-flight validation | Seconds to minutes for Maven/Gradle dependency resolution, compilation, and JVM startup |
| **Memory & GC Predictability** | Bounded Go channels, `32 KiB` 4096-bin histogram, `256`-cookie cap, zero JVM heap `-Xmx` tuning | Requires JVM heap sizing (`JAVA_OPTS="-Xmx..."`); heavy GC pauses under large simulation logs can skew client latency |
| **Live Visibility & Diffing** | Free live Admin Portal (`http://localhost:4000`) + free `lemmings compare` CLI | Static HTML generated *only after* run ends; live monitoring & diffs require paid Gatling Enterprise |

### Free Live Triage Portal & Visual Screenshots vs. Static Post-Run Tables

On `toktail.app`, Gatling OSS would leave you staring at a terminal ticker during the 3-minute run and then generate a static HTML table saying `KO: 313`. Lemmings streamed all 12 deduplicated Chrome CDP screenshots, cookie jar health (`3 active, 0 expired`), visitor trails, parsed HTML bodies, and browser JS console errors (`favicon.ico` `404`) live into the authenticated Admin Portal and self-contained HTML/Markdown reports:

| Live Admin Triage Portal (`http://localhost:4000`) | Self-Contained HTML Report with Embedded Screenshots |
| :---: | :---: |
| ![Lemmings Admin Portal — Dark Mode](../images/dashboard-dark.jpg) | ![Lemmings HTML Report](../images/report.jpg) |

---

## Conventions Translation: How Gatling Concepts Map to Lemmings

Moving from Gatling's Scala/Java DSL to Lemmings eliminates build projects and translates every primitive into clean CLI flags and strict YAML:

| Gatling Convention (Java / Scala DSL) | Lemmings (`v2.0.1`) Equivalent | Why Lemmings Is Better |
| --- | --- | --- |
| `http.baseUrl("https://...")` | `-hit https://staging.example.com/` | Enforces strict `SameOrigin` + `SafeDialContext` SSRF protection automatically |
| `scenario("Buyer").exec(...)` | YAML `plans:` entry (`name: buyer`, `plan: [...]`) | Strict `KnownFields(true)` YAML — zero compilation, instant Git PR reviews |
| `http("login").post("/login").formParam(...)` | `method: POST`, `path: /login`, `with-body: [...]` | Auto form-urlencoded or JSON object encoding + fail-closed `${ENV_VAR}` |
| `pause(1, 3)` | `-think-min 1s -think-max 3s` or per-step `chill: 1500ms` | Realistic randomized think pauses out of the box |
| `check(status().is(200), substring("..."), css("#id"))` | `want-status: 200`, `want-body`, `want-selectors: ["#id"]`, `want-headers`, `want-cookies` | Plus automatic `not-blank` check + automatic 4-part forensics capture on any failure |
| `rampUsers(200).during(60)` | `-terrain 20 -pack 10 -ramp 60s -ramp-shape linear\|fibonacci\|step` | Change ramp shapes via CLI without recompiling a single line of code |
| `assertions(global().responseTime().percentile(95).lt(500))` | `-p95-budget 500ms -max-failure-rate 0.01 -min-plan-pass-rate 0.99` | Native CLI flags + waiting-room gates + `lemmings compare` release diffing |
| `inferHtmlResources()` | `-assets -assets-per-page 8` | Strictly separates `AssetReport` from page `-p95-budget` SLOs and caches 2,048 assets per visitor |
| `mvn gatling:test` | `lemmings -hit ... -plan ./plans/ ...` | Zero Maven/Gradle/JDK install; one 20 MB binary |

### Side-by-Side Example: Authenticated Web & API Simulation

**Gatling (Java DSL — requires `pom.xml`/`build.gradle`, JDK, Maven build, zero screenshots, no live OSS dashboard):**
```java
public class CheckoutSimulation extends Simulation {
  HttpProtocolBuilder httpProtocol = http.baseUrl("https://staging.example.com");

  ScenarioBuilder scn = scenario("CheckoutFlow")
    .exec(http("Login")
      .post("/login")
      .formParam("username", System.getenv("DEMO_USER"))
      .formParam("password", System.getenv("DEMO_PASS"))
      .check(status().is(200))
      .check(substring("Dashboard")))
    .pause(1)
    .exec(http("Checkout")
      .get("/checkout")
      .check(status().is(200))
      .check(css("#order-summary").exists())
      .check(substring("Order Summary")));

  {
    setUp(scn.injectOpen(rampUsers(80).during(15)))
      .protocols(httpProtocol)
      .assertions(
        global().failedRequests().percent().lt(1.0),
        global().responseTime().percentile(95.0).lt(750)
      );
  }
}
```

**Lemmings (Zero JDK, Zero Build Tool, Automatic CDP Error Screenshots, Free Live Portal & `lemmings compare`):**

Save as `checkout.yaml`:
```yaml
name: checkout-flow
weight: 10
stop-on-fail: true
loop: false
plan:
  - name: login
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
    want-body:
      - contains: Dashboard
        not-contains: Invalid credentials
  - name: checkout
    path: /checkout
    method: GET
    want-status: 200
    want-content-type: text/html
    want-selectors: ["#order-summary"]
    want-cookies:
      - name: session_id
    want-body:
      - contains: Order Summary
        not-contains: Something went wrong
```

Validate with `-dry-run`, then execute with CI gates and a live Browser Scout:
```bash
export DEMO_USER="player1"
export DEMO_PASS="staging-pass"
lemmings -hit https://staging.example.com/ -plan checkout.yaml -dry-run

lemmings \
  -hit https://staging.example.com/ \
  -plan checkout.yaml \
  -terrain 8 -pack 10 -limit 80 -until 60s -ramp 15s \
  -scouts 2 \
  -min-plan-pass-rate 0.99 -max-failure-rate 0.01 -p95-budget 750ms \
  -tty=false -unique-report -save-to ./reports
```

---

## Gatling Gotchas & Pitfalls — Solved Out of the Box by Lemmings

1. **The Gatling Enterprise Paywall for Live Dashboards & Run Comparisons**
   - **Gatling pitfall:** Gatling OSS gives you no real-time web dashboard during execution and no built-in way to compare today's release against yesterday's baseline—both are core selling points locked behind paid **Gatling Enterprise**.
   - **How Lemmings solves it:** Ships an authenticated real-time Admin Portal (`http://localhost:4000/#token=...`) with live charts, animated visitor stage, live Error Forensics Gallery, `-review` triage mode, and `lemmings compare` regression diffing—100% free under Apache-2.0.
2. **Heavy JVM + Maven/Gradle/sbt Boilerplate in CI/CD**
   - **Gatling pitfall:** Every CI pipeline needs a JDK image, Maven/Gradle dependency caching, and compilation time before a single HTTP request is sent. Non-JVM teams (Go, Python, Rust, TypeScript, Ruby) hate maintaining a Java/Scala repo just to run load tests.
   - **How Lemmings solves it:** Single `20–23 MB` standalone binary with zero runtime dependencies and human-/AI-readable YAML plans validated instantaneously with `-dry-run`.
3. **Zero Visual Screenshots or Client-Side JavaScript Diagnostics**
   - **Gatling pitfall:** Gatling operates strictly at the HTTP protocol layer. When a `500` HTML error page renders—or when a React/Next.js/Vue bundle throws an uncaught `TypeError` on a `200 OK` response—Gatling gives you zero screenshot and zero JS console trace.
   - **How Lemmings solves it:** Pure-Go Chrome DevTools Protocol capture automatically screenshots failing `GET` sessions in isolated incognito contexts with the failing lemming's exact cookies and viewport, while `-scouts N` catches JS crashes on `200 OK` pages.
4. **Slow Feedback Loops Debugging Assertion Failures**
   - **Gatling pitfall:** To inspect why a response body failed a `check()`, you often have to enable verbose `logback.xml` HTTP trace logging (flooding gigabytes of disk and slowing the load generator) and rerun.
   - **How Lemmings solves it:** Captures bounded, deduplicated (`-capture-max`, `-capture-per-error`) 4-part error artifacts automatically on the very first run—extracting structured HTML, RFC 7807 JSON, XML, or plain-text error details with secret scrubbing (`SEC-10`) without slowing down the HTTP hot path.
5. **Static Asset Distortion (`inferHtmlResources`)**
   - **Gatling pitfall:** Enabling `inferHtmlResources()` fetches subresources, which can inflate aggregate request counts and skew global latency stats unless carefully filtered into custom groups.
   - **How Lemmings solves it:** `-assets -assets-per-page 8` fetches same-origin HTML/CSS subresources using a per-lemming 2,048-entry cache and reports them in a strictly isolated `AssetReport`—never distorting page `-p95-budget` SLOs.
6. **Hardcoded Scenarios Miss Unscripted Dead Links**
   - **Gatling pitfall:** Gatling simulations only hit the paths explicitly written in Scala/Java/JS code.
   - **How Lemmings solves it:** Runs a deterministic `5%` rogue cohort (`-rogue-rate`) crawling your sitemap and HTML links alongside your `95%` scripted multi-persona YAML suite.

---

## Common Load-Testing Problems & How Lemmings Resolves Them

| Real-World Load Testing Problem | Why It Hurts in Gatling OSS | How Lemmings Solves It Out of the Box |
| --- | --- | --- |
| **Teams can't inspect failures while a test is running** | OSS Gatling prints text snapshots to stdout; HTML report only renders after completion. | Live SSE Admin Portal (`http://localhost:4000`) streams real-time charts, live error screenshots, and interactive triage (`-review`) while the swarm runs. |
| **Non-Java developers avoid updating load tests** | Scala/Java DSL + Maven/Gradle creates a specialist bottleneck. | Strict YAML plans (`docs/plans.md` ships 10 blueprints) + [`SKILL.md`](../../SKILL.md) let any developer or AI coding agent author & validate suites in minutes. |
| **Waiting rooms break latency metrics & assertions** | Custom `asLongAs` polling loops are brittle and skew aggregate percentiles. | Native Concert waiting-room protocol separates origin page latency (`186.2ms` p95 on `toktail.app`) from queue wait (`24s` mean), removes `DiedInLine` survivorship bias (`v2.0.1`), and gates queues via `-min-queue-held` / `-max-queue-wait`. |
| **Release regressions go unnoticed across sprints** | Comparing two static Gatling HTML folders is manual without Gatling Enterprise. | `lemmings compare -max-p95-delta 100ms -max-failure-delta 0.01 baseline.json candidate.json` automates regression gating in CI (`exit 2`) with error-fingerprint diffs. |
| **Security teams audit load generators for SSRF & credential leaks** | Custom JVM code lacks dial-time metadata guards and structured report redaction. | Audited `SEC-01`–`SEC-10`: `SafeDialContext` blocks cloud IMDS (`169.254.169.254`), `${ENV_VAR}` fails closed, `with-*`/`body` & raw cookies are `json:"-"`, and CycloneDX 1.5 SBOM ships with every build. |

---

## Getting Started with Load Testing: Frequently Asked Questions

### How do I begin load testing a web app without installing Java, Maven, or Gradle?
Install Lemmings with one command (`go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest`), run `lemmings -hit https://staging.example.com/ -crawl -dry-run` to validate your target with zero load, then run a short crawled swarm with `-review` (`-terrain 4 -pack 8 -limit 32 -until 45s -ramp 10s -crawl -review`) to watch live telemetry and error screenshots in the Admin Portal.

### How do I share load test reports cleanly in GitHub PRs?
Unlike Gatling's multi-file JS/CSS HTML bundle that requires hosting an artifact viewer, every Lemmings run outputs **three formats simultaneously**: a **single self-contained HTML file** (inline screenshots), a **GitHub-ready Markdown report** (`.md` + `.artifacts/*.jpg` that renders natively inside GitHub PRs and repos), and a lean **`schema_version: "2.0"` JSON** report for `lemmings compare` — plus built-in S3 and TLS 1.2+ SMTP delivery (`-save-to`).

### How does Lemmings handle per-path cold TLS handshakes vs. steady-state warm latency?
Lemmings `v2.0.1` tracks `cold_hits`, `setup_p50_ns` (`DNS + Connect + TLS`), `warm_p50_ns`, and `ttfb_p50_ns` on every path in JSON, HTML, and Markdown reports—so first-hit TLS setup cost on `/` never misleads your engineering team about steady-state server performance.

### Why is Lemmings the better choice over Gatling for HTTP web apps and APIs?
You eliminate the JVM/Maven/Gradle toolchain, unlock free real-time visual monitoring and `lemmings compare` regression diffs without paying for Gatling Enterprise, gain automatic Chrome CDP error screenshots and JS console forensics, and define multi-persona traffic in strict YAML that both humans and AI agents can maintain effortlessly.

> **When Gatling still fits:** If your organization is an enterprise Java/JVM shop already paying for **Gatling Enterprise** (multi-region managed cloud generators, enterprise RBAC, and JVM-specific JMS/MQTT protocol extensions) and your team prefers authoring simulations in Java/Scala/Kotlin, Gatling Enterprise is a mature commercial platform. For free, fast, self-hosted HTTP/web/API load testing with built-in visual forensics and CI regression gates, Lemmings wins hands down.

---

## Try Lemmings in 60 Seconds

```bash
# Single 20–23 MB Apache-2.0 Go binary — zero JDK, zero Maven, zero Enterprise paywall
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest

# Pre-flight validation (zero load sent)
lemmings -hit https://staging.example.com/ -crawl -dry-run

# Run with live Admin Portal, Chrome forensics, CI gates, and HTML/MD/JSON reports
lemmings -hit https://staging.example.com/ \
  -terrain 6 -pack 15 -limit 60 -until 60s -ramp 15s -ramp-shape fibonacci \
  -crawl -scouts 2 -max-failure-rate 0.01 -p95-budget 750ms \
  -review -unique-report -save-to ./reports
```

---

⭐ **Ditch the JVM build tax and get live visual forensics for free — [star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings)!**

**More Comparisons:** [All Tools Hub](./README.md) · [vs Apache Benchmark (`ab`)](./lemmings-vs-ab.md) · [vs Bombardier](./lemmings-vs-bombardier.md) · [vs NBomber](./lemmings-vs-nbomber.md) · [vs k6](./lemmings-vs-k6.md) · [vs Locust](./lemmings-vs-locust.md) · [vs JMeter](./lemmings-vs-jmeter.md)
