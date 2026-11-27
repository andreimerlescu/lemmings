# Lemmings Load-Testing Comparisons: Choosing the Best HTTP & Web Load Tester

[![GitHub Repo stars](https://img.shields.io/github/stars/andreimerlescu/lemmings?style=social)](https://github.com/andreimerlescu/lemmings)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-36f9f6)](../../LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)

> **TL;DR:** **Lemmings v2.0.1** (`github.com/andreimerlescu/lemmings`) is the single-binary, Apache-2.0 Go load tester built for real web applications, stateful user journeys, and HTTP APIs. Instead of handing you blind status-code tables—or locking live dashboards, run comparisons, and commercial use behind paid cloud plans—Lemmings pairs high-scale Go load generation with **automatic 4-part Chrome DevTools Protocol (CDP) error screenshots & JS console forensics (zero Node.js)**, **strict multi-persona YAML plans + a `5%` rogue crawler cohort**, an **authenticated live Admin Triage Portal (`-review`)**, **self-contained HTML/Markdown/JSON reports**, and **`lemmings compare` CI regression gates**.
>
> ⭐ **Evaluating load testers right now?** [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to bookmark the project and help other developers discover it.

---

## Deep-Dive Tool Comparisons

Pick the tool you are currently using or evaluating to see full `✅`/`❌` matrices, verified real-run data, exact CLI/YAML translations, pitfalls solved out of the box, and migration FAQs:

| Alternative Tool | Deep-Dive Comparison Guide | Core Reason Developers Switch to Lemmings |
| --- | --- | --- |
| **Apache Benchmark (`ab`)** | [**Lemmings vs Apache Benchmark (`ab`)**](./lemmings-vs-ab.md) | Upgrade from single-threaded HTTP/1.0 single-URL pings to multi-core HTTP/2 stateful visitors, crawling, YAML plans, CDP screenshots, and CI gates. |
| **Bombardier** | [**Lemmings vs Bombardier**](./lemmings-vs-bombardier.md) | Keep single-binary Go speed while adding HTTP/2 by default, RFC 6265 cookie jars, multi-persona YAML journeys, soft-error assertions, visual forensics, and `lemmings compare`. |
| **NBomber** | [**Lemmings vs NBomber**](./lemmings-vs-nbomber.md) | Replace commercial-licensed (~$1,500–$2,500/yr) `.NET` C#/F# projects with a **100% free Apache-2.0** zero-SDK Go binary, declarative YAML plans, and automatic browser screenshots. |
| **Grafana k6** | [**Lemmings vs Grafana k6**](./lemmings-vs-k6.md) | Ditch AGPL-3.0 copyleft, Goja JS VM overhead, `check()` false-green traps, and Grafana Cloud paywalls for pure-Go speed, free live triage UI, HTML/MD reports, and `lemmings compare`. |
| **Gatling** | [**Lemmings vs Gatling**](./lemmings-vs-gatling.md) | Eliminate JDK/Maven/Gradle build tax and Gatling Enterprise paywalls with instant single-binary runs, free real-time Admin Portal, CDP error screenshots, and strict YAML. |
| **Locust** | [**Lemmings vs Locust**](./lemmings-vs-locust.md) | Break free from Python's single-core GIL ceiling and `FastHttpUser` rewrites with multi-core Go goroutines, hardened loopback UI (`SEC-05`), automatic error screenshots, and CI gates. |
| **Apache JMeter** | [**Lemmings vs Apache JMeter**](./lemmings-vs-jmeter.md) | Leave unreviewable XML `.jmx` merge conflicts and heavy JVM thread-per-VU RAM bloat behind for Git-native strict YAML, safe live visual triage, and deterministic exit code `2` CI gates. |

---

## Master 8-Tool Capability Matrix

| Capability | **Lemmings** | `ab` | Bombardier | NBomber | k6 | Gatling | Locust | JMeter |
| --- | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **Free Permissive Commercial License** | ✅ *(Apache-2.0)* | ✅ | ✅ *(MIT)* | ❌ *(Paid org)* | ❌ *(AGPL-3.0)* | ✅ *(OSS only)* | ✅ *(MIT)* | ✅ |
| **Single Zero-Runtime Binary** | ✅ *(20–23 MB Go)* | ✅ *(C/APR)* | ✅ *(Go)* | ❌ *(.NET)* | ✅ *(Go+Goja)* | ❌ *(JVM)* | ❌ *(Python)* | ❌ *(JVM)* |
| **Multi-Core Native Concurrency (1 Process)** | ✅ | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ *(GIL)* | ✅ *(OS threads)* |
| **Zero-Code Site Crawl & Link Navigation** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Git-Friendly Strict YAML Multi-Persona Plans** | ✅ | ❌ | ❌ | ❌ *(C#/F#)* | ❌ *(JS)* | ❌ *(Java/Scala)* | ❌ *(Python)* | ❌ *(XML `.jmx`)* |
| **Built-In `5%` Rogue Wanderer Cohort** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Per-Visitor RFC 6265 Cookie Jar by Default** | ✅ | ❌ | ❌ | ❌ *(manual)* | ✅ | ✅ | ✅ | ❌ *(manual)* |
| **Automatic `200 OK` Blank & Soft-Error Checks** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Automatic Error Screenshots (Pure-Go CDP)** | ✅ | ❌ | ❌ | ❌ | ❌ *(manual JS)* | ❌ | ❌ | ❌ |
| **Real-Browser JS/Render Scouts (`-scouts`)** | ✅ | ❌ | ❌ | ❌ | ❌ *(manual JS)* | ❌ | ❌ | ❌ |
| **Free Live Triage Portal & `-review` Tags** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ *(Enterprise)* | ❌ *(no triage)* | ❌ *(GUI unsafe)* |
| **Built-In HTML + GitHub MD + JSON Reports** | ✅ | ❌ | ❌ | ✅ *(no shots)* | ❌ *(custom JS)* | ✅ *(HTML only)* | ✅ *(HTML/CSV)* | ✅ *(HTML/JTL)* |
| **Built-In CLI Baseline Diff (`compare`)** | ✅ | ❌ | ❌ | ❌ | ❌ *(Cloud)* | ❌ *(Enterprise)* | ❌ | ❌ |
| **Deterministic CI Gates (`exit 2`)** | ✅ | ❌ | ❌ | ✅ *(in C#)* | ✅ *(exit 99)* | ✅ *(in DSL)* | ❌ *(custom)* | ❌ *(exits 0)* |
| **Native Virtual Waiting Room Protocol** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **Security Audited (`SEC-01`–`SEC-10`, SBOM)** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| **AI Coding Agent Skill Runbook (`SKILL.md`)** | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |

---

## Verified Real-World Performance Highlights (`counthis.app` & `toktail.app`)

Every comparison guide in this directory is grounded in two real production-grade Lemmings runs (`33` terrains × `12` pack = `396` stateful visitors, `90s` ramp, `90s` lifespan, `3m0s` total duration, `600ms–6s` human think time, zero dropped lifelogs):

| Verified Run Metric | `counthis.app` *(Standard Web App)* | `toktail.app` *(Concert Waiting Room + 503 Chaos)* |
| --- | --- | --- |
| **Visitors & Rescued Score** | `396/396` completed · **`100.0%` rescued** (`384` peak alive) | `396/396` completed · **`89.1%` rescued** (`353/396` flawless, `384` peak alive) |
| **Page Visits & Throughput** | **`10,831` visits** across `10` paths · `60.2` visits/s avg (**`144` visits/s peak**) · `76.15 MB` | **`3,110` visits** across `4` paths · `17.3` visits/s avg (`77` visits/s peak) · `36.87 MB` |
| **Origin Page Latency** | **p50 `35.8ms`** · **p95 `185.4ms`** · p99 `607.8ms` · **TTFB p95 `144.5ms`** (`<= 0.5%` 4096-bin histogram) | **p50 `43.0ms`** · **p95 `186.2ms`** · p99 `206.0ms` · **TTFB p95 `156.5ms`** *(isolated from queue wait)* |
| **Waiting Room Telemetry** | `0` held | **`1,093` held** (`729` admitted, `364` died in line, `11,685` `/queue/status` polls, mean wait `24s`, `3,026` admit passes) |
| **Automatic 4-Part Error Forensics** | `0` errors · `0` dropped lifelogs | `313` HTTP `503`s → **`12` deduplicated Chrome CDP screenshots** + JS console traces (`favicon.ico` `404`) + cookie jars (`3 active, 0 expired`) + parsed HTML bodies (`0` dropped) |

| Live Admin Triage Portal (`http://localhost:4000`) | Self-Contained HTML Report with Embedded Screenshots |
| :---: | :---: |
| ![Lemmings Admin Portal — Dark Mode](../images/dashboard-dark.jpg) | ![Lemmings HTML Report](../images/report.jpg) |

---

## Get Started in 60 Seconds

```bash
# Install the single Apache-2.0 Go binary
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest

# Validate your target pre-flight (zero load sent)
lemmings -hit https://staging.example.com/ -crawl -dry-run

# Run a stateful swarm with live Error Forensics, Browser Scouts & -review triage
lemmings -hit https://staging.example.com/ \
  -terrain 4 -pack 10 -limit 40 -until 60s -ramp 15s \
  -crawl -scouts 1 -review -unique-report -save-to ./reports
```

- **Full YAML Plan Reference & 10 Persona Blueprints:** [Plans, Personas, Navigation & Page Checks](../plans.md)
- **AI Coding Agent Runbook:** [`SKILL.md`](../../SKILL.md) — point any coding agent here to auto-author a 10-persona SEC-10-safe YAML load suite for your repo.
- **CI Gates & Regression Diffing:** [Reports, Delivery & CI Gates](../reports-and-ci-gates.md)

---

⭐ **Found these comparisons helpful? [Star `andreimerlescu/lemmings` on GitHub](https://github.com/andreimerlescu/lemmings) to help more developers and AI agents choose stateful load testing with real visual receipts!**
