# Getting Started with Lemmings

Lemmings unleashes a crowd of stateful simulated visitors onto an HTTP(S) website, streams live error screenshots and diagnostics into an authenticated local Admin Portal, and produces shareable HTML, Markdown, and JSON reports with CI pass/fail gates.

---

## Requirements

- **Go 1.26+** (to install or build from source; compiled release binaries have zero runtime dependencies)
- **Optional — Chrome, Chromium, Brave, or Edge** (for automatic headless screenshots, JavaScript console capture, and real-browser Scouts). If no browser is installed locally, you can point Lemmings at a remote Chrome DevTools endpoint with `-cdp-url` or run seamlessly in HTTP-only forensics mode.

---

## Installation

### Install with Go

```bash
go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest
lemmings -version
```

### Build from Source

```bash
git clone https://github.com/andreimerlescu/lemmings.git
cd lemmings
make build-local
./bin/lemmings -version
```

Cross-compile release binaries for Linux, macOS, and Windows and verify supply-chain artifacts:

```bash
make build
make sbom checksums     # emits bin/sbom.cdx.json (CycloneDX 1.5) and bin/SHA256SUMS
make vulncheck          # runs govulncheck ./... across module dependencies
```

---

## 60-Second Quickstart (Neon Arcade Demo)

Lemmings ships with **Neon Arcade** (`examples/demo`), a loopback-only demo website containing healthy pages, static assets (`/assets/arcade.css` and `/assets/logo.svg` exempt from chaos/queues for `-assets` and Concert testing), a slow flaky leaderboard (`503` + `Retry-After`), a soft error page (`200 OK` with an error banner), a structured JSON `500` API endpoint (`/api/tokens`), a client-side JS crash page (`/arcade-glitch`), a blank page, and a waiting room.

### 1. Start the demo target

In terminal 1:

```bash
make demo
```

Neon Arcade listens only on `http://127.0.0.1:8080`.

### 2. Validate your run with `-dry-run` (optional)

Before sending load to any site, validate your YAML plan(s) and `${ENV_VAR}` placeholders, preview the indexed URL surface, ramp estimate, and browser probe without launching a single lemming:

```bash
go run ./cmd/lemmings \
  -hit http://127.0.0.1:8080/ \
  -plan examples/plans/forensics-demo.yaml \
  -dry-run
```

### 3. Run a live forensics swarm (or YAML auth plan)

In terminal 2, launch the forensics showcase with Admin Review Mode:

```bash
make run-forensics
```

Or walk the bundled Neon Arcade auth plan (`examples/plans/neon-arcade-auth.yaml`) with CI gates (`-min-plan-pass-rate`, `-max-failure-rate`, `-p95-budget`):

```bash
make run-plan
```

The equivalent direct command for `make run-forensics`:

```bash
go run ./cmd/lemmings \
  -hit http://127.0.0.1:8080/ \
  -terrain 4 \
  -pack 6 \
  -limit 24 \
  -until 30s \
  -ramp 5s \
  -plan examples/plans/forensics-demo.yaml \
  -scouts 1 \
  -review \
  -unique-report \
  -save-to bin/reports
```

### 4. Open the Admin Portal

Lemmings prints a one-click loopback sign-in link containing a random 256-bit boot token in the URL fragment (`#token=...`):

```text
  dashboard:        http://localhost:4000/#token=<64-hex-chars>
```

Open that link in your browser to:
1. Watch lemmings traverse the live stage, queue in the waiting room, and react to `2xx`/`3xx`/`4xx`/`5xx` pages.
2. See **Error Forensics cards** stream into the gallery live as screenshots and JS console errors arrive.
3. Click any card to inspect the **4-panel Forensics Modal** (Screenshot, Session & Cookie Jar, Extracted Error & Body, JS Console).
4. Tag each failure (`Bug`, `Flaky`, `Expected`, or `Ignored`) with an admin note.
5. Click **Finalize & Write Report** to save the HTML, Markdown, JSON, and `.artifacts/` screenshot bundle with your triage verdicts embedded.

---

## Core Mental Model

| Concept | Flag | Meaning |
| --- | --- | --- |
| **Target Origin** | `-hit` | Starting URL (`http://` or `https://`). Lemmings never follow links or redirects off this origin. |
| **Terrain** | `-terrain` | Independent scheduler goroutine groups brought online across `-ramp`. |
| **Pack** | `-pack` | Stateful lemmings launched by each terrain (`total = terrain × pack`). |
| **Concurrency Limit** | `-limit` | Global semaphore capping how many lemmings run simultaneously (`-1` disables cap). |
| **Lifespan** | `-until` | Maximum time each individual lemming browses before exiting. |
| **Ramp & Shape** | `-ramp`, `-ramp-shape` | Duration and pacing curve (`linear`, `fibonacci`, `step`) for bringing terrains online. |
| **YAML Plans & Rogues** | `-plan`, `-rogue-rate` | Weighted multi-persona YAML suites (`GET`/`POST`/`PUT`/`PATCH`/`DELETE`/`HEAD`/`OPTIONS`) with cookies, headers, bodies, and assertions. By default 95% follow plans while 5% roam freely as rogues (`-rogue-rate 0` for 100% plan adherence). |
| **Browser Scouts** | `-scouts` | Optional real-browser lemmings running alongside the HTTP swarm to catch pure JS/render errors on HTTP `200 OK` pages. |
| **Rescued Score** | *(report)* | Percentage of lemmings that lived their entire session with zero failed pages. |

---

## Exit Codes

Lemmings uses deterministic exit codes for CI pipelines and automation:

| Code | Meaning |
| --- | --- |
| `0` | Run (or `lemmings compare`) completed and all configured gates passed. |
| `1` | Configuration or YAML plan validation error (including unset `${ENV_VAR}` or exceeded `-max-plan-items`), origin indexing failure, browser initialization error (when explicitly required), or report delivery failure. |
| `2` | Run completed, but a CI gate (`-max-failure-rate`, `-p95-budget`, `-min-plan-pass-rate`) or `lemmings compare` regression threshold failed. |
| `130` | Interrupted by `SIGINT` (Ctrl-C) or `SIGTERM` during the swarm run; partial reports were still saved. |

---

## Next Steps

- [Plans, Personas & Page Checks](plans.md) — full YAML `-plan` specification, multi-persona weights, rogue split, assertions, and 10 ready-to-adapt persona blueprints.
- [Agent Skill (`SKILL.md`)](../SKILL.md) — step-by-step runbook for coding agents to inspect any website and author 10+ user-type YAML plans.
- [Error Forensics & Screenshots](error-forensics-and-screenshots.md) — understand all 4 captured error artifacts, headless CDP replay, and Scouts.
- [Admin Portal Guide](dashboard-guide.md) — live gallery, 4-panel inspector, triage tags, and `-review` hold mode.
- [Reports, Delivery & CI Gates](reports-and-ci-gates.md) — HTML/Markdown/JSON formats, `PlanReport`, S3/SMTP delivery, and `lemmings compare`.
- [CLI Reference](cli-reference.md) — every flag, alias, default, and environment variable.
