---
name: lemmings-plans
description: Authors, validates, executes, and troubleshoots Lemmings v2.0.0 YAML plans and multi-persona load-testing suites for any website or web application. Use when asked to inspect a site and design realistic user personas, write stateful HTTP/HTML/API YAML plans with cookies, headers, bodies, and assertions, validate plans with -dry-run, configure CI reliability gates (-min-plan-pass-rate, -max-failure-rate, -p95-budget), or diagnose Lemmings plan failures.
---

# Lemmings YAML Plan Authoring & Operations Runbook

Use this skill to turn any authorized website or codebase into a realistic, stateful, SEC-10-safe **Lemmings v2.0.0** YAML plan suite covering 10+ distinct user personas, validate it safely before sending load, run it with deterministic CI gates, and diagnose failures from `PlanReport` and Error Forensics artifacts.

> **Authorization Guard:** Only inspect or send traffic to origins the user owns or has explicit written authorization to test. Prefer local or staging environments over production, especially for mutating (`POST`/`PUT`/`PATCH`/`DELETE`) steps.

---

## Workflow Overview

1. **Inspect the target** (codebase routes, OpenAPI/templates, or safe live probes) to map endpoints, methods, auth/session cookies, content types, redirects, and error surfaces.
2. **Design a multi-persona portfolio** (10+ user archetypes with realistic traffic `weight` values and appropriate `-rogue-rate`).
3. **Author strict SEC-10-safe YAML** (`plan:` single files or `plans:` bundle / directory) using `${ENV_VAR}` for all credentials and tokens.
4. **Validate pre-flight** with `lemmings -hit <origin> -plan <path> -dry-run`, then run a small deterministic smoke test (`-rogue-rate 0`).
5. **Execute load & CI gates** (`-min-plan-pass-rate`, `-max-failure-rate`, `-p95-budget`, `-assets`, `-scouts`, `lemmings compare`).
6. **Diagnose failures** using `PlanReport` step tables, exit reasons (`plan-failed`), and 4-part Error Forensics artifacts.

---

## Step 1: Inspect the Target Site or Codebase

Before writing YAML, collect ground-truth facts so every assertion matches real application behavior (avoid guessing selectors, status codes, or cookie names).

### When Source Code Is Available
Inspect routers, controllers, templates, and tests to extract:
- **Public read routes:** landing page (`/`), catalog/listings, detail pages, search & pagination query params, health checks (`/healthz`), static assets (`/assets/*`).
- **Auth & session mechanics:** login/signup/logout endpoints, form vs. JSON payload field names, session cookie name(s) and `Set-Cookie` attributes, CSRF header/cookie requirements, post-login redirect targets.
- **Mutating & API routes:** `POST`/`PUT`/`PATCH`/`DELETE` endpoints, required `Content-Type` (`application/x-www-form-urlencoded` vs. `application/json` vs. `application/*+json`), expected `200`/`201`/`204` responses, and whether test fixtures/idempotent endpoints exist.
- **Guards, redirects & edge routes:** auth-protected routes returning `401`/`403` or `302` to `/login`, legacy/canonical `301`/`308` redirects, `404` pages, and waiting-room/rate-limited routes (e.g., Concert `/queue/status`).
- **Soft-error patterns:** phrases rendered inside `200 OK` HTML templates when an upstream fails (e.g., `"Something went wrong"`, `"Temporarily unavailable"`, `"Traceback"`).

### When Only a URL Is Available
1. Check `robots.txt` and `sitemap.xml` (or run `lemmings -hit <origin> -crawl -dry-run` to inspect the discovered URL pool safely).
2. Inspect representative `GET`/`HEAD` responses to confirm actual `<title>`, stable DOM selectors (`nav`, `main`, `#id`, `.class`), response headers (`Cache-Control`, `Content-Type`, `X-Request-Id`), redirect `Location` headers, and cookies set on first visit.
3. Ask the user for staging test credentials, API tokens, and safe test/sandbox endpoints before authoring any mutating `POST`/`PUT`/`PATCH`/`DELETE` steps.

---

## Step 2: Design a Multi-Persona Portfolio (10+ User Types)

Model real production traffic instead of hammering one endpoint. Lemmings assigns the non-rogue cohort (default **95%** when `-rogue-rate` is auto) across personas in exact proportion to each plan's `weight`, while the remaining **5%** roam organically as **rogues** (`-navigation links`/`random`) to uncover unscripted dead links and crawl bugs.

Use this 10-persona baseline (total weight = `100`) and extend it with domain-specific roles (e.g., admin console operator, webhook receiver, mobile viewport shopper, waiting-room queue entrant):

| # | Persona | Suggested Weight | Methods & Behavior | Key Assertions |
| --- | --- | :---: | --- | --- |
| 1 | `anonymous-browser` | `30` | `GET` landing → catalog → detail (`chill` pauses) | `want-status: 200`, `want-title`, `want-selectors`, `want-body` |
| 2 | `new-account-signup` | `5` | `stop-on-fail: true`, `GET` signup → `POST` form → welcome | `with-body`, `want-status`, `want-cookies` session minted |
| 3 | `returning-customer` | `15` | `stop-on-fail: true`, `POST` login (`${USER}`/`${PASS}`) → `GET` profile → `POST` logout | `want-cookies` present after login, `absent: true` after logout |
| 4 | `checkout-submitter` | `10` | `stop-on-fail: true`, `with-cookies` cart/flag → `GET` checkout → `POST` order | `want-body` confirmation + `not-contains` soft errors |
| 5 | `json-rest-api-client` | `15` | `with-headers` Bearer `${API_TOKEN}` + `application/json`, `POST`/`PATCH`/`DELETE` (`204`) | `with-body` JSON or raw `body`, `want-headers`, `want-status` |
| 6 | `search-pagination` | `10` | `GET` query page 1 → page 2 → empty-result query | `want-selectors`, `want-body` matches & empty state |
| 7 | `auth-guard-prober` | `3` | Unauthenticated & bad-token `GET` on protected routes | `want-status: "401,403"`, `want-headers` `WWW-Authenticate`, `not-contains` secrets |
| 8 | `redirect-canonical` | `3` | `GET` legacy URLs (`want-status: 301` auto-disables redirect following) + final `200` | `want-headers` `Location` (`is`/`begins-with`), explicit `follow-redirects: true` |
| 9 | `dead-link-404-probe` | `2` | `GET` nonexistent path (`404`) + `HEAD /healthz` (`200`) | Verifies custom 404 & `HEAD`; built-in `not-blank` auto-skipped |
| 10 | `looping-reader` | `7` | `loop: true`, short `chill: 100ms` cycling feeds/articles until `-until` | Sustained read throughput & response stability |

Complete, copy-ready YAML blueprints for all 10 personas live in [`docs/plans.md`](docs/plans.md#10-user-type-persona-blueprints).

### Choosing File Layout
- **Single-persona file (`plan:`):** Best for a focused smoke check or CI gate on one funnel.
- **Multi-persona bundle (`plans:`):** Best when all personas share one target and you want a single versioned file (up to **100** plans per suite).
- **Directory of YAML files (`-plan ./plans/`):** Loads direct `*.yaml`/`*.yml` files in lexicographic order (e.g., `01-anonymous-browser.yaml` … `10-looping-reader.yaml`). Best for large teams maintaining personas independently. You can also pass comma-separated files/directories: `-plan ./plans/core.yaml,./plans/api/`.

---

## Step 3: Author Strict SEC-10-Safe YAML Plans

Lemmings decodes YAML with **`KnownFields(true)`** and **single-document enforcement** (no `---` multi-doc streams). Every typo or disallowed combination fails fast with exit code `1`.

### Schema Cheat Sheet

```yaml
# Option A: Single-plan file                 # Option B: Multi-persona bundle
name: returning-customer                     # plans:
weight: 15           # int >= 1 (default 1)  #   - name: persona-a
stop-on-fail: true   # default false         #     weight: 60
loop: false          # default false         #     stop-on-fail: false
plan:                                        #     loop: false
  - name: login                              #     plan:
    path: /login                             #       - path: /
    method: POST     # GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS (default GET)
    chill: 500ms     # post-step pause (overrides -think-min/-think-max; 0s = no pause)
    follow-redirects: true   # default true; auto-false when want-status is exclusively 3xx
    stop-on-fail: true       # overrides plan-level stop-on-fail; exits lemming as plan-failed
    with-cookies:            # seeded into InspectingJar at origin "/"; persists across steps
      - name: ab_bucket      # alias: key (never set both name and key)
        value: control
    with-headers:            # per-step only; overrides defaults; rejects Host & CR/LF/NUL
      - key: X-Client-Mode   # alias: name (never set both key and name)
        value: loadtest
    with-body:               # form-urlencoded by default; JSON object if Content-Type is JSON
      - key: username        # alias: name; mutually exclusive with raw `body`
        value: "${LOAD_USER}"
      - key: password
        value: "${LOAD_PASS}"
    # body: '{"raw": true}'  # raw string (mutually exclusive with with-body; default text/plain)
    want-status: 200         # int, "200,201", "2xx", "200-299", or [200, 201] (default 2xx)
    want-content-type: text/html
    want-title: Account
    want-selectors: ["nav", "main", "#profile"]   # tag, #id, or .class only (no combinators)
    want-headers:            # case-insensitive multi-value; presence-only if no matcher set
      - key: Cache-Control   # alias: name
        contains: private
        not-contains: public
      - key: X-Frame-Options
        is-one: ["DENY", "SAMEORIGIN"]  # YAML list OR comma-separated string
    want-cookies:            # evaluated against active InspectingJar for final URL
      - name: session_id     # alias: key; presence-only if no matcher set
      - name: role
        is: member           # `value` is alias for `is` (never set both value and is)
      - name: guest_banner
        absent: true         # cannot be combined with any value matcher
    want-body:               # each entry needs contains and/or not-contains
      - contains: Welcome back
        not-contains: Something went wrong
```

### Mandatory Validation & Wire-Encoding Rules
1. **Unique names:** Every plan in a loaded suite must have a distinct `name` (max `100` plans per suite; max `999` steps per plan unless overridden with `-max-plan-items` / `-mpi`).
2. **Same-origin paths:** Every `path` is resolved against `-hit`; off-origin URLs fail at startup.
3. **No alias collisions:** Never set both `name` and `key` on the same `with-cookies`, `with-headers`, `with-body`, `want-headers`, or `want-cookies` item. Never set both `value` and `is` on `want-cookies`.
4. **Mutual exclusions:**
   - Top-level `plan:` and `plans:` cannot coexist in one file.
   - `with-body:` and `body:` cannot coexist on one step.
   - `absent: true` cannot coexist with `value`, `is`, `contains`, `not-contains`, `begins-with`, `ends-with`, or `is-one` on `want-cookies`.
5. **Body encoding trigger:** `with-body` sends `application/x-www-form-urlencoded` unless `with-headers` explicitly sets `Content-Type: application/json` or `application/*+json`, in which case it encodes a flat JSON object. For nested JSON objects, arrays, or XML, use raw `body:` plus an explicit `Content-Type` header.
6. **Redirects & `want-status`:** When `want-status` expects *exclusively* `3xx` codes (e.g., `301`, `"301,302"`, `"3xx"`), `follow-redirects` auto-defaults to `false` so you can assert `Location` directly. Set `follow-redirects: true` if you want to follow through to a `2xx` (and change `want-status` accordingly).
7. **Built-in `not-blank` check:** Automatically skipped on `HEAD`, `204`/`304`, and steps whose `want-status` allows no `2xx` codes (like `want-status: 404` or `401`).
8. **Final-step `chill`:** When `loop: false`, think time is skipped after the last step *unless* `chill` is explicitly set on that last step. Use `chill: 0s` on any step that should fire the next step immediately.
9. **Simple selectors only:** `want-selectors` accepts `tag`, `#id`, and `.class`. Complex CSS combinators (`div > a`, `input[name=email]`) are rejected at startup.

### Mandatory SEC-10 Security Checklist
- **Always parameterize secrets with `${ENV_VAR}`** in `with-cookies`, `with-headers`, `with-body`, and `body` (`${[A-Za-z_][A-Za-z0-9_]*}`). Escape literal dollar signs with `$$` (e.g., `$$5.00` → `$5.00`).
- **Fail-closed env expansion:** Unset variables abort startup (exit `1`) with the variable name only—never hardcode real passwords, session cookies, or bearer tokens in YAML files.
- **No secret leakage:** Lemmings tags `with-*` and `body` as `json:"-"` (never emitted in reports, traces, or the Admin Portal), never puts raw cookie values in `want-cookies` check details, and redacts sensitive `want-headers` values.
- **Forensics & Scouts awareness:**
  - Failed **non-GET** steps (or steps carrying a body) record **HTTP-only forensics** and skip CDP browser replay so Chromium never issues a bogus `GET` against a mutating endpoint.
  - Failed **GET** steps (without a body) capture full CDP screenshots and JS console output.
  - If any plan in the suite contains a non-GET step, `-scouts` automatically falls back to browsing the indexed `URLPool` instead of replaying mutating plan steps in Chromium.

---

## Step 4: Validate Pre-Flight (`-dry-run`) & Smoke Test

Never jump straight to heavy load. Always follow this two-stage verification:

```bash
# 1. Export required plan variables (never commit these)
export LOAD_USER="player1"
export LOAD_PASS="demo-pass"
export API_TOKEN="tok_staging_example"

# 2. Pre-flight check: validates YAML schema, KnownFields, env expansion,
#    same-origin paths, URL pool, and Chromium/CDP availability — sends ZERO load.
lemmings -hit https://staging.example.com/ -plan ./plans/ -dry-run

# 3. Deterministic functional smoke test (0% rogues, strict 100% plan pass gate)
lemmings \
  -hit https://staging.example.com/ \
  -plan ./plans/ \
  -terrain 1 -pack 10 -limit 10 -until 30s -ramp 0s \
  -rogue-rate 0 \
  -min-plan-pass-rate 1.0 \
  -max-failure-rate 0 \
  -tty=false \
  -report-name smoke-check \
  -save-to ./reports
```

If `-dry-run` exits `1`, fix the reported YAML/schema/env/path error before running load. If the smoke test exits `2`, inspect `./reports/lemmings/<domain>/smoke-check.md` (and `.artifacts/` screenshots) to fix step assumptions before scaling up.

---

## Step 5: Run Load, Triage & CI Gates

Once the smoke test passes, scale to realistic concurrency with the default `5%` rogue cohort (`-rogue-rate` auto), optional static assets (`-assets`), optional browser Scouts (`-scouts`), and deterministic CI gates:

```bash
lemmings \
  -hit https://staging.example.com/ \
  -plan ./plans/ \
  -terrain 8 -pack 15 -limit 80 -until 60s -ramp 15s -ramp-shape fibonacci \
  -assets -assets-per-page 8 \
  -scouts 2 \
  -min-plan-pass-rate 0.99 \
  -max-failure-rate 0.01 \
  -p95-budget 750ms \
  -tty=false \
  -unique-report \
  -save-to ./reports
```

- **Local interactive debugging:** Add `-review` (with `-tty=true`) to hold the Admin Portal open after the swarm finishes, visually inspect error screenshots/cookies/console traces, tag findings (`Bug`/`Flaky`/`Expected`/`Ignored`), and click **Finalize & Write Report**.
- **Release regression comparison:** Diff candidate vs. baseline JSON reports with `lemmings compare -max-failure-delta 0.01 -max-p95-delta 100ms -max-rescued-drop 2.0 baseline.json candidate.json`.
- **Large plans:** If a generated plan exceeds `999` steps, raise the ceiling explicitly with `-max-plan-items <N>` (`-mpi`, up to `1000000`; file read cap scales up to `64 MiB`).
- **Prometheus live monitoring:** Add `-observe` to scrape all 24 metrics including `lemmings_plan_lemmings_total{outcome="passed|failed"}` and `lemmings_rogue_lemmings_total`.

---

## Step 6: Diagnose Plan Failures Quickly

| Symptom / Report Signal | Likely Cause | Fix |
| --- | --- | --- |
| Startup exit `1`: unknown field / multi-document error | Typo in YAML key (`KnownFields(true)`) or `---` second doc in one file | Check exact hyphenated keys (`want-status`, `stop-on-fail`, `with-body`); use `plans:` bundle or separate files in a directory. |
| Startup exit `1`: missing environment variable | Unset `${ENV_VAR}` referenced in `with-*` or `body` | Export the variable or escape literal `$` as `$$`. |
| Step exits early with `plan-failed` | `stop-on-fail: true` (plan or step) tripped on an assertion or HTTP error | Inspect that step's `PlanStepReport` failure details & forensic artifact; fix upstream auth/setup step first. |
| `want-status: 301` failed with `200` | `follow-redirects: true` was set explicitly, or `want-status` also included a non-`3xx` code (e.g., `"200,301"`) | Omit `follow-redirects` with exclusively `3xx` `want-status`, or set `follow-redirects: false` explicitly. |
| API returned `400`/`415` on `with-body` | Server expects JSON, but `with-body` defaulted to `application/x-www-form-urlencoded` | Add `- key: Content-Type` / `value: application/json` under `with-headers`, or switch to raw `body:` for nested JSON. |
| `want-cookies` failed after redirect/login | Cookie scoped to a narrower path, marked `Secure` on `http://`, or cleared | Check the Session & Cookie Jar tab in the artifact inspector; verify `http://` vs. `https://` and final URL path. |
| Artifact shows `http-only` instead of screenshot | Failed step was non-GET or had a request body (intentional SEC-10 safety) | Inspect the Extracted Error & Body + Session & Cookie Jar panels; CDP replay is intentionally skipped for mutating requests. |
| `DATA DROPPED` banner in report | High throughput overflowed trace/life-log/forensics queues | Reduce `-limit` or raise `-capture-max` / `-capture-workers` if forensic coverage is incomplete. |
| Exit code `2` despite low overall failure rate | `-min-plan-pass-rate` breached because individual step failures failed many plan lemmings | Inspect per-persona `PlanStepReport` tables to pinpoint which persona/step is dragging down `PlanPassed / PlanLemmings`. |

---

## References in This Repository

- **Full YAML Plan Specification & 10 Complete Persona Blueprints:** [`docs/plans.md`](docs/plans.md)
- **All CLI Flags, Aliases & Validation Ranges:** [`docs/cli-reference.md`](docs/cli-reference.md)
- **Error Forensics, CDP Replay & Scouts:** [`docs/error-forensics-and-screenshots.md`](docs/error-forensics-and-screenshots.md)
- **Reports, `PlanReport`, CI Gates & `lemmings compare`:** [`docs/reports-and-ci-gates.md`](docs/reports-and-ci-gates.md)
- **Security Posture & `SEC-01`–`SEC-10`:** [`docs/security.md`](docs/security.md)
- **Bundled Working Examples (Neon Arcade):**
  - [`examples/plans/neon-arcade-auth.yaml`](examples/plans/neon-arcade-auth.yaml) — authenticated session/cookie flow with CI gates (`make run-plan`)
  - [`examples/plans/neon-arcade-personas.yaml`](examples/plans/neon-arcade-personas.yaml) — multi-persona weighted suite (`plans:`)
  - [`examples/plans/buy-tokens.yaml`](examples/plans/buy-tokens.yaml) — ordered checkout funnel
  - [`examples/plans/forensics-demo.yaml`](examples/plans/forensics-demo.yaml) — soft-error, JSON 500 & JS-glitch showcase (`make run-forensics`)
  - [`examples/plans/known-bad.yaml`](examples/plans/known-bad.yaml) — intentional failure & diagnostic suite
