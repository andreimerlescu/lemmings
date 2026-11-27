# Lemmings vs NBomber: Free Apache-2.0 Single Binary with Visual Forensics vs. Commercial .NET Code-First Load Testing

[![GitHub Repo stars](https://img.shields.io/github/stars/andreimerlescu/lemmings?style=social)](https://github.com/andreimerlescu/lemmings)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-36f9f6)](../../LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)

> **TL;DR Verdict:** **Lemmings is the better choice than NBomber for HTTP web apps, REST APIs, and cross-functional engineering teams.** NBomber requires a `.NET` SDK, C#/F# compilation boilerplate, and a **paid commercial license (~$1,500–$2,500/yr) for organizational use**—and still gives you zero browser error screenshots, zero JS console capture, and no interactive triage UI. **Lemmings v2.0.1** is **100% free under Apache-2.0 for commercial and enterprise teams**, ships as one 20–23 MB zero-dependency Go binary, models multi-persona journeys in clean Git-reviewable YAML, automatically captures 4-part Chrome DevTools Protocol (CDP) error screenshots & JS console traces, and includes an authenticated Admin Triage Portal plus `lemmings compare` CI regression gates out of the box.
>
> ⭐ **Prefer free Apache-2.0 tooling with built-in visual forensics?** [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to help more developers find it.

| Quick Decision Summary | **Lemmings (`v2.0.1`)** | **NBomber** |
| --- | --- | --- |
| **Commercial License** | **100% Free — Apache-2.0** (unlimited teams, CI, commercial use) | **Paid commercial subscription** required for companies (~$1,500–$2,500/yr; free for personal/academic only) |
| **Runtime & Authoring** | Single 20–23 MB Go binary + declarative strict YAML `-plan` (zero SDK required) | Requires `.NET` SDK + C#/F# project + NuGet packages (`NBomber`, `NBomber.Http`) |
| **When Something Breaks** | Automatic CDP screenshot + JS console + cookie jar + parsed error body + Admin Triage Portal | HTTP status & response metrics in HTML/MD/CSV reports (no browser screenshots or JS forensics) |
| **Install in 10 Seconds** | `go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest` | `dotnet new console` + `dotnet add package NBomber` + `NBomber.Http` |

---

## Side-by-Side Feature Comparison (`Lemmings` vs `NBomber`)

| Capability | Lemmings (`v2.0.1`) | NBomber |
| --- | :---: | :---: |
| Free for commercial & enterprise organizations | ✅ (Apache-2.0) | ❌ (paid Business/Enterprise license required) |
| Single zero-dependency binary (no .NET / JVM / Node / Python) | ✅ (`20–23 MB` Go binary) | ❌ (requires .NET runtime & build pipeline) |
| Language-agnostic test definitions (readable by any dev or AI) | ✅ (strict `KnownFields(true)` YAML) | ❌ (C# / F# compiled code required) |
| Instant zero-script site crawl & link navigation | ✅ (`-crawl`, `-navigation links`) | ❌ (must write C# scenarios for every route) |
| Per-visitor stateful RFC 6265 cookie jars (`InspectingJar`) | ✅ (built-in & audited) | ❌ (manual `HttpClientHandler` / `CookieContainer` wiring) |
| Multi-persona weighted traffic + built-in `5%` rogue cohort | ✅ (`plans:` + `-rogue-rate`) | ❌ (manual scenario weights; no rogue crawler) |
| Declarative HTTP/HTML/DOM/cookie/header assertions | ✅ (`want-*` + automatic `not-blank`) | ❌ (must write imperative C# `Response.Ok`/`Fail` code) |
| Automatic error screenshots (pure-Go CDP, zero Node.js) | ✅ | ❌ |
| JavaScript console & client-side render crash detection (`-scouts`) | ✅ | ❌ |
| Live Admin Triage Portal (`Bug`/`Flaky`/`Expected`/`Ignored`) & `-review` | ✅ | ❌ (NBomber Studio is Enterprise paid add-on; no screenshot triage) |
| Opt-in same-origin static asset tier (`-assets`) | ✅ (isolated from page SLOs) | ❌ |
| Native virtual waiting-room protocol (Concert queue separation) | ✅ | ❌ |
| Self-contained HTML, GitHub Markdown & JSON (`schema 2.0`) reports | ✅ (with embedded screenshots) | ✅ (HTML/MD/CSV/TXT metrics; no screenshots) |
| Built-in CI pass/fail gates (`exit 2`) & `lemmings compare` diffs | ✅ | ✅ (thresholds in C#; no built-in CLI baseline diff) |
| Built-in Prometheus `/metrics` exporter (`24` metrics) | ✅ (`-observe`, zero extra packages) | ✅ (via separate `NBomber.Sinks.*` / OpenTelemetry setup) |
| Security hardened (SSRF dial guard, secret scrubbing, CycloneDX SBOM) | ✅ (`SEC-01`–`SEC-10`) | ❌ (user-managed inside C# code) |
| Non-HTTP protocol / in-process .NET library testing (gRPC/SQL/MQTT) | ❌ (HTTP/HTTPS web & API focused) | ✅ (protocol-agnostic .NET SDK) |
| AI agent plan-authoring runbook (`SKILL.md`) | ✅ | ❌ |

---

## Performance & Real-World Output Side by Side

Below are **verified real-world Lemmings runs (`counthis.app` and `toktail.app`)** compared side-by-side with NBomber's documented `.NET` CLR runtime profile and what NBomber actually captures—or misses—on the same scenarios.

### Real Verified Lemmings Run Data (`396` Stateful Visitors · `3m0s`)

| Metric | Lemmings on `counthis.app` *(Standard Web App)* | Lemmings on `toktail.app` *(Concert Queue + 503 Chaos)* | What NBomber Requires / Captures on These Targets |
| --- | --- | --- | --- |
| **Setup Overhead** | **1 CLI command**, 0 lines of code (`-crawl -navigation links`) | **1 CLI command**, 0 lines of code | Create `.csproj`, install `NBomber` + `NBomber.Http`, manage `HttpClient` pool & cookie containers, compile C#, configure commercial license key |
| **Visitors & Rescued Score** | `396/396` completed · **`100.0%` rescued** (`384` peak alive) | `396/396` completed · **`89.1%` rescued** (`353/396` flawless, `384` peak alive) | Tracks scenario OK/Fail counts; no built-in per-visitor rescued journey metric without custom C# tracking |
| **Visits & Throughput** | **`10,831` visits** across `10` auto-crawled routes · `60.2` visits/s avg (**`144` visits/s peak**) · `76.15 MB` | **`3,110` visits** across `4` routes · `17.3` visits/s avg (`77` visits/s peak) · `36.87 MB` | High `.NET` async throughput, but only hits routes you explicitly hardcode in C# |
| **Origin Page Latency** | **p50 `35.8ms`** · p90 `123.8ms` · **p95 `185.4ms`** · p99 `607.8ms` · **TTFB p95 `144.5ms`** (`<= 0.5%` 4096-bin histogram) | **p50 `43.0ms`** · p90 `69.3ms` · **p95 `186.2ms`** · p99 `206.0ms` · **TTFB p95 `156.5ms`** *(isolated from queue wait)* | HdrHistogram percentiles (`p50`/`p75`/`p95`/`p99`), but queue wait & origin latency blend unless you manually author custom waiting-room C# state machines |
| **Waiting Room & Queueing** | `0` held | **`1,093` held** (`729` admitted, `364` died in line, **`11,685` `/queue/status` polls**, max pos `359`, mean wait `24s`, `3,026` passes) | **Must be hand-coded in C#** from scratch (HTML/JSON detection, `room_ticket` jar, poll backoff, admission reload, separate histograms) |
| **Error Forensics & Visual Receipts** | `0` errors · `0` dropped lifelogs | `313` HTTP `503`s (`11.4%`) → **`12` deduplicated 4-part artifacts** + **`12` Chrome CDP screenshots** (`301` deduped, `0` dropped) + JS console `404` on `/favicon.ico` | Logs HTTP status code & optional custom error strings. **Zero screenshots, zero JS console traces, zero live Admin Triage UI** |

### Runtime & Architecture Footprint Comparison

| Dimension | Lemmings (`v2.0.1`) | NBomber |
| --- | --- | --- |
| **Binary & Runtime** | `20–23 MB` single compiled Go binary; zero runtime install | Requires `.NET 8+` SDK/runtime (~200+ MB in CI images) + NuGet restore + MSBuild compile step |
| **Cost at Scale** | **$0 / year** (Apache-2.0) across unlimited developers, repos, and CI runners | **~$1,500–$2,500 / year** for organizational Business/Enterprise licenses |
| **CI Cold Start** | Instant binary execution + `-dry-run` pre-flight check | `dotnet restore` + `dotnet build` + JIT warmup |
| **Forensics Overhead** | Zero hot-path blocking: O(1) `TryReserve` dedup + bounded async CDP worker pool | No built-in browser forensics engine |

### Visual Evidence Out of the Box

When `toktail.app` served `313` HTTP `503`s under load, Lemmings streamed 12 deduplicated 4-part forensic artifacts—complete with Chrome screenshots, cookie jar metadata, visitor trails, parsed HTML bodies, and browser JS console errors—straight into the authenticated Admin Portal and self-contained HTML reports:

| Live Admin Triage Portal (`http://localhost:4000`) | Self-Contained HTML Report with Embedded Screenshots |
| :---: | :---: |
| ![Lemmings Admin Portal — Dark Mode](../images/dashboard-dark.jpg) | ![Lemmings HTML Report](../images/report.jpg) |

---

## Conventions Translation: How NBomber Concepts Map to Lemmings

Moving HTTP/API tests out of compiled C# NBomber projects into Lemmings slashes hundreds of lines of boilerplate down to clean, version-controlled YAML:

| NBomber Convention (C#) | Lemmings (`v2.0.1`) Equivalent | Why Lemmings Is Better |
| --- | --- | --- |
| `Scenario.Create("checkout", ...)` | YAML `plans:` entry (`name: checkout`) | Readable by backend, frontend, QA, SRE, and AI agents without knowing C# |
| `Step.Run("login", ctx, ...)` | Ordered `plan:` item (`name: login`) | Built-in `stop-on-fail`, `chill`, redirects, and per-step `PlanStepReport` tables |
| `Http.CreateRequest("POST", url).WithHeader(...).WithBody(...)` | `method: POST`, `with-headers`, `with-body` / `body` | Auto form/JSON encoding + fail-closed `${ENV_VAR}` secrets (`SEC-10`) |
| `Simulation.KeepConstant(copies: 50, during: ...)` | `-terrain 5 -pack 10 -limit 50 -until 60s` | Built-in per-visitor cookie jar & connection pool isolation |
| `Simulation.RampingInject(...)` / `RampingConstant(...)` | `-ramp 30s -ramp-shape linear\|fibonacci\|step` | Zero code edits to adjust traffic curves between runs |
| `Task.Delay(Random.Shared.Next(...))` | `-think-min 600ms -think-max 6s` or step `chill: 1s` | Built-in human pacing + per-step overrides |
| `Response.Ok()` / `Response.Fail()` manual checks | `want-status`, `want-headers`, `want-cookies`, `want-body`, `want-selectors`, `want-title` | Declarative assertions + automatic `not-blank` + automatic 4-part forensics on failure |
| `NBomberRunner.RegisterScenarios(...).WithReportFormats(...)` | `-save-to ./reports,s3://...` | Emits HTML (inline screenshots), GitHub Markdown, and JSON (`schema 2.0`) automatically |
| `.WithThresholds(...)` | `-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate` + `lemmings compare` | Deterministic exit code `2` + built-in baseline vs. candidate regression diffing |

### Side-by-Side Example: Multi-Step Authenticated User Journey

**NBomber (C# — requires `.csproj`, NuGet packages, manual `HttpClient` + `CookieContainer`, commercial license for work):**
```csharp
using NBomber.CSharp;
using NBomber.Http.CSharp;

using var httpClient = new HttpClient(new HttpClientHandler { UseCookies = true });

var scenario = Scenario.Create("returning_customer", async context =>
{
    var loginForm = new FormUrlEncodedContent(new[]
    {
        new KeyValuePair<string, string>("username", Environment.GetEnvironmentVariable("DEMO_USER")!),
        new KeyValuePair<string, string>("password", Environment.GetEnvironmentVariable("DEMO_PASS")!)
    });

    var step1 = await Step.Run("authenticate", context, async () =>
    {
        var req = Http.CreateRequest("POST", "https://staging.example.com/login")
            .WithBody(loginForm);
        return await Http.Send(httpClient, req);
    });
    if (step1.IsError) return Response.Fail();

    var step2 = await Step.Run("view_profile", context, async () =>
    {
        var req = Http.CreateRequest("GET", "https://staging.example.com/account/profile");
        var res = await Http.Send(httpClient, req);
        var body = await res.Payload.Value.Content.ReadAsStringAsync();
        return body.Contains("Account Settings") ? res : Response.Fail();
    });

    return step2.IsError ? Response.Fail() : Response.Ok();
})
.WithLoadSimulations(Simulation.KeepConstant(copies: 40, during: TimeSpan.FromSeconds(60)));

NBomberRunner.RegisterScenarios(scenario).Run();
```
*(Notice a subtle bug in the standard C# snippet above: sharing one `HttpClient` across 40 concurrent copies shares ONE `CookieContainer` across all 40 virtual users!)*

**Lemmings (Zero SDK, Zero Shared-Cookie Bugs, Automatic Forensics, Free Apache-2.0):**

Save as `returning-customer.yaml`:
```yaml
name: returning-customer
weight: 10
stop-on-fail: true
loop: false
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
    want-body:
      - contains: Dashboard
        not-contains: Invalid credentials
  - name: view-profile
    path: /account/profile
    method: GET
    want-status: 200
    want-content-type: text/html
    want-selectors: ["main", "#profile-details"]
    want-cookies:
      - name: session_id
    want-body:
      - contains: Account Settings
        not-contains: Something went wrong
```

Validate with `-dry-run`, then run with CI gates and a Browser Scout:
```bash
export DEMO_USER="player1"
export DEMO_PASS="staging-pass"
lemmings -hit https://staging.example.com/ -plan returning-customer.yaml -dry-run
lemmings \
  -hit https://staging.example.com/ \
  -plan returning-customer.yaml \
  -terrain 4 -pack 10 -limit 40 -until 60s -ramp 10s \
  -scouts 1 -min-plan-pass-rate 0.99 -max-failure-rate 0.01 -p95-budget 750ms \
  -tty=false -unique-report -save-to ./reports
```

---

## NBomber Gotchas & Pitfalls — Solved Out of the Box by Lemmings

1. **Commercial Licensing Friction for Companies**
   - **NBomber pitfall:** NBomber is free for personal/academic projects, but commercial teams must purchase an annual Business (~$1,500/yr) or Enterprise (~$2,500/yr) license before running it legally at work—creating procurement delays for every team that adopts it.
   - **How Lemmings solves it:** Permissive **Apache-2.0** license with zero feature paywalls, zero seat fees, and full CycloneDX 1.5 SBOM + SHA-256 checksums ready for enterprise security review.
2. **Shared `HttpClient` / `CookieContainer` Cross-Session Contamination**
   - **NBomber pitfall:** Reusing one `HttpClient` (standard `.NET` advice to prevent socket exhaustion) silently shares a single `CookieContainer` across all virtual users—causing User #17's logout to invalidate User #4's session! Creating one `HttpClient` per iteration instead causes `TIME_WAIT` port exhaustion.
   - **How Lemmings solves it:** Every lemming automatically owns an isolated RFC 6265 `InspectingJar` (bounded at 256 cookies) paired with a clean connection pool for its entire lifespan—zero shared-cookie bugs and zero socket leaks.
3. **Heavy `.NET` Toolchain Tax for Non-.NET Teams & CI Runners**
   - **NBomber pitfall:** Go, Rust, Python, Ruby, Node.js, and SRE teams have to install the `.NET` SDK, restore NuGet packages, and maintain C# projects just to load-test a website or REST API.
   - **How Lemmings solves it:** One `20–23 MB` standalone binary (`go install` or prebuilt binary for Linux/macOS/Windows `amd64`/`arm64`) plus declarative YAML plans validated with `KnownFields(true)` and `-dry-run`.
4. **Zero Visual Error Forensics or JS Console Capture**
   - **NBomber pitfall:** When an HTTP step fails under load, NBomber logs a status code or exception message. It cannot open the failing session in Chromium, capture what the broken page looked like, or detect React/Vue hydration crashes on `200 OK` pages.
   - **How Lemmings solves it:** Pure-Go Chrome DevTools Protocol capture (zero Node.js) automatically replays failing `GET` sessions in ephemeral incognito contexts—delivering screenshots, JS console traces, cookie jar status, and parsed error bodies, while `-scouts` catches client-side crashes on `200 OK` pages.
5. **No Live Interactive Triage Workflow Without Enterprise Add-Ons**
   - **NBomber pitfall:** Local runs emit static files after completion; rich dashboards require wiring external Prometheus/Grafana/InfluxDB sinks or purchasing NBomber Studio Enterprise.
   - **How Lemmings solves it:** Every run launches a zero-setup authenticated local Admin Portal (`http://localhost:4000/#token=...`) with live charts, a 4-panel Artifact Inspector Modal, `-review` hold mode, and `Bug`/`Flaky`/`Expected`/`Ignored` tags embedded directly into final HTML/Markdown/JSON reports.
6. **Unscripted Regressions Slip Through Hand-Coded Scenarios**
   - **NBomber pitfall:** C# scenarios only ever touch the exact URLs engineers remember to code; broken footer links, stale sitemaps, or misconfigured pages go completely untested.
   - **How Lemmings solves it:** Automatically assigns a deterministic `5%` rogue cohort (`-rogue-rate`) to roam your sitemap and crawled HTML links alongside your `95%` scripted multi-persona YAML suite.

---

## Common Load-Testing Problems & How Lemmings Resolves Them

| Real-World Load Testing Problem | Why It Happens | How Lemmings Solves It Out of the Box |
| --- | --- | --- |
| **Test suites drift out of sync with app routes** | Maintaining compiled load-test code is slow, so teams stop updating tests after sprint 2. | Strict YAML plans validated by `-dry-run`, `5%` automatic rogue crawling, and [`SKILL.md`](../../SKILL.md) letting AI coding agents regenerate/update 10+ personas directly from your codebase. |
| **Hidden `200 OK` error banners & broken frontend bundles** | Protocol clients see HTTP `200` and mark the step green. | Automatic `not-blank`, `want-body` `not-contains`, `want-selectors`, and real-browser `-scouts N` catching JS console errors on `200 OK` responses. |
| **Waiting rooms or rate-limiters corrupting p95 latency** | Queue holds look like slow origin responses, and timeouts drop worst-case waits. | Native Concert waiting-room protocol separates origin page latency from queue wait, tracks `queue-timeout` without survivorship bias (`v2.0.1`), and provides `-min-queue-held` / `-max-queue-wait` CI gates. |
| **No repeatable way to spot release-to-release regressions** | Teams eyeball HTML reports manually instead of gating PRs. | `lemmings compare -max-p95-delta 100ms -max-failure-delta 0.01 baseline.json candidate.json` exits `2` on regressions and diffs new (`+`) vs. resolved (`-`) error fingerprints. |
| **Accidental secret leakage or SSRF during staging tests** | Custom scripts log headers/cookies or follow open redirects to internal cloud IPs. | `SEC-01`–`SEC-10` audited: dial-time `SafeDialContext` blocking `169.254.0.0/16` IMDS, `json:"-"` on `with-*`/`body` and cookies, fail-closed `${ENV_VAR}`, and `secrets.ScrubText`. |

---

## Getting Started with Load Testing: Frequently Asked Questions

### How do I start load testing my web app or API without setting up a C#/.NET project?
Install Lemmings in one command (`go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest`), run `lemmings -hit https://staging.example.com/ -crawl -dry-run` to inspect discovered URLs with zero load, then run a short swarm with `-review` to visually triage results in the Admin Portal before scaling up and locking in YAML `-plan` CI gates.

### Can non-.NET teams and QA engineers author Lemmings plans easily?
Yes. Lemmings uses strict, self-documenting YAML (`KnownFields(true)`) where typos fail fast at startup with exact error messages. Even better, `docs/plans.md` includes **10 copy-ready user-persona blueprints** (anonymous browser, signup, returning customer, checkout, JSON REST API, search, auth-guard prober, redirect verifier, 404/health prober, looping reader), and [`SKILL.md`](../../SKILL.md) lets any coding agent author them automatically.

### How does Lemmings compare to NBomber for CI/CD quality gates?
Lemmings enforces `-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`, `-min-queue-held`, and `-max-queue-wait` natively via CLI flags with deterministic exit code `2`, and adds `lemmings compare` to diff candidate JSON reports against baselines—all in one 20 MB binary with zero `dotnet restore` or build time in GitHub Actions / GitLab CI.

### Why is Lemmings the better choice over NBomber for HTTP/web load testing?
You pay **$0** (Apache-2.0 vs. ~$1,500–$2,500/yr), install **zero runtimes** (one 20–23 MB Go binary vs. `.NET` SDK), write **zero cookie-container or HTTP boilerplate** (strict YAML + automatic `InspectingJar`), and gain **automatic Chrome error screenshots, JS console traces, real-browser Scouts, and a live Admin Triage Portal** that NBomber doesn't offer at any tier.

> **When NBomber still fits:** If you are a `.NET` shop load-testing non-HTTP protocols directly in-process—such as raw SQL/PostgreSQL connection pools, RabbitMQ/Kafka brokers, MQTT streams, or custom C# gRPC/domain libraries where reusing existing `.NET` client assemblies inside xUnit/NUnit is essential—NBomber's protocol-agnostic .NET SDK is purpose-built for that niche. For HTTP websites, web apps, and REST APIs, Lemmings is faster to adopt, free commercially, and far richer in diagnostics.

---

## Try Lemmings in 60 Seconds

```bash
# 100% free Apache-2.0 single binary — zero .NET SDK or license keys
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest

# Validate your YAML suite with zero traffic sent
lemmings -hit https://staging.example.com/ -plan examples/plans/neon-arcade-personas.yaml -dry-run

# Run locally against the bundled Neon Arcade demo with live visual forensics
git clone https://github.com/andreimerlescu/lemmings.git && cd lemmings
make demo            # Terminal 1
make run-forensics   # Terminal 2 — open http://localhost:4000/#token=...
```

---

⭐ **Zero commercial paywalls, zero SDK tax, full visual receipts. [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings)!**

**More Comparisons:** [All Tools Hub](./README.md) · [vs Apache Benchmark (`ab`)](./lemmings-vs-ab.md) · [vs Bombardier](./lemmings-vs-bombardier.md) · [vs k6](./lemmings-vs-k6.md) · [vs Gatling](./lemmings-vs-gatling.md) · [vs Locust](./lemmings-vs-locust.md) · [vs JMeter](./lemmings-vs-jmeter.md)
