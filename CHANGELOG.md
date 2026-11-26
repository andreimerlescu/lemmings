# Changelog

All notable changes to lemmings are recorded here. Versions follow
[Semantic Versioning](https://semver.org).

## 2.0.0 — 2026-11-25

A major architectural, scripting, security, and forensics upgrade: Lemmings is
now structured into clean Go bounded contexts (`cmd/lemmings` + `internal/*`),
introduces a unified multi-method YAML `-plan` & multi-persona suite engine
with deterministic rogue traffic, captures live error screenshots, JS console
logs, cookie jar state, and extracted error bodies, streams them into the Admin
Portal for triage review, and embeds everything cleanly into final reports.

### Unified YAML Plans & Multi-Persona Suites

- **Breaking Change:** Removed legacy `-journey` (`-j`), JSON journey files,
  `internal/journey/`, `examples/journeys/`, and `docs/journeys-and-checks.md`,
  replacing them with `-plan` (`-pl`), strict YAML plans, `internal/plan/`,
  `examples/plans/`, and `docs/plans.md`.
- **Single-plan & multi-persona YAML suites (`-plan` / `-pl`).** Accepts a
  single YAML file, a directory of `*.yaml`/`*.yml` files (loaded in
  lexicographic order), or a comma-separated list of files and directories.
  Decoded with strict `KnownFields(true)` and single-document enforcement,
  supporting both single-plan (`plan: [...]`) and multi-persona bundle
  (`plans: [...]`, up to 100 unique plans per suite) formats with
  `-max-plan-items` / `-mpi` (default `999`, range `1..1000000`, file read cap
  scaling up to `64 MiB`).
- **Multi-method stateful execution & request shaping.** Supports `GET`,
  `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, and `OPTIONS`, jar-seeded
  `with-cookies` (origin `/` persistence across redirects & steps), per-step
  `with-headers` (rejecting `Host` and CR/LF/NUL), `with-body` (auto
  `application/x-www-form-urlencoded`, or JSON object when `Content-Type` is
  `application/json` / `application/*+json`), raw `body` (default
  `text/plain; charset=utf-8`), fail-closed `${ENV_VAR}` expansion (`$$` literal
  escape), per-step `chill` pacing, smart `follow-redirects` (auto-`false` when
  `want-status` expects exclusively `3xx`), `stop-on-fail` (exit reason
  `plan-failed`), and `loop`.
- **Deep response assertions.** Supports `want-status` (int, string, ranges,
  `2xx`, or YAML list; defaults to `2xx`), case-insensitive multi-value
  `want-headers` (`is`, `contains`, `not-contains`, `begins-with`, `ends-with`,
  `is-one`, presence-only, sensitive header redaction), jar-backed
  `want-cookies` (`value`/`is`, `contains`, `not-contains`, `begins-with`,
  `ends-with`, `is-one`, `absent: true`, presence-only, zero raw cookie values
  in details), `want-body`, `want-content-type`, `want-title`, `want-selectors`,
  and automatic `not-blank` skip on `HEAD`, `204`/`304`, or exclusively
  non-`2xx` `want-status`.
- **Deterministic weighting, rogue cohort (`-rogue-rate` / `-rr`) & CI gate
  (`-min-plan-pass-rate` / `-mppr`).** Assigns non-rogue lemmings (default 95%)
  deterministically across personas in proportion to `weight`, while a
  deterministic rogue fraction (`floor(n*rate+1e-9) > floor((n-1)*rate+1e-9)`,
  auto `0.05` when `-plan` is set) wanders via `-navigation` (`links`/`random`).
  Adds `PlanReport` (cohort pass rate, rogue cohort, per-persona ordered
  `PlanStepReport` tables) across CLI, Dashboard, JSON, HTML, and Markdown, plus
  exit code `2` gating via `-min-plan-pass-rate`.
- **Bundled examples & AI Agent Skill.** Added `SKILL.md` at repository root
  (`name: lemmings-plans`), exhaustive `docs/plans.md` with 10 user-type persona
  blueprints, five Neon Arcade example plans under `examples/plans/`
  (`neon-arcade-auth.yaml`, `neon-arcade-personas.yaml`, `buy-tokens.yaml`,
  `forensics-demo.yaml`, `known-bad.yaml`), and `make run-plan`.

### Live Error Forensics & Screenshots

- **Zero-Node pure-Go Chrome DevTools Protocol (CDP) engine.** Auto-detects
  local Chrome/Chromium (or connects to `-cdp-url`), replays failing visits
  inside isolated ephemeral incognito contexts with the lemming's cookies, user
  agent, language, and viewport, and captures compressed screenshots, JS console
  logs/exceptions, and failed subresources — falling back cleanly to HTTP-only
  forensics when no browser is present.
- **Four-part error artifacts** for every captured failure:
  1. Session state, sequential lemming number (`#1..N`), breadcrumb trail, and
     cookie jar status (`InspectingJar` metadata + HMAC fingerprint; raw cookie
     values always redacted in reports).
  2. JavaScript console errors, warnings, uncaught exceptions, and failed
     subresources.
  3. Visual screenshot (JPEG/PNG) bounded and deduplicated (`-capture-max`,
     `-capture-per-error`, `-capture-workers`, `-capture-timeout`).
  4. Parsed & extracted error page body (HTML, JSON/RFC-7807 Problem Details,
     XML, or plain text with secret scrubbing and allowlisted diagnostic headers).
- **Real-browser Scouts (`-scouts N`).** Optional browser-backed lemmings run
  concurrently with the HTTP swarm to surface pure client-side JS and rendering
  errors on HTTP `200 OK` pages.

### Admin Portal Triage & Review Mode

- **Live Error Forensics Gallery** streaming into the portal via SSE as
  screenshots finish, with filter tabs (`All`, `Unreviewed`, `5xx`, `4xx`,
  `JS Errors`, `Check Failures`) and search.
- **4-Panel Artifact Inspector Modal** (Screenshot, Session & Cookie Jar,
  Extracted Error & Body, JS Console) with triage actions (`Bug`, `Flaky`,
  `Expected`, `Ignored`, `Unreviewed` + admin note) persisted directly into the
  final HTML, Markdown, and JSON reports.
- **Post-run Review Mode (`-review`).** Holds the portal open after the swarm
  completes so the admin can review and tag screenshots before clicking
  **Finalize & Write Report** (or pressing Ctrl-C).

### Concert Waiting Room Protocol & Opt-In Asset Tier

- **Native Concert protocol support.** Detects Concert `waiting.html`
  (`/_concert/waiting.js` + `#position` / `.queue-page`), legacy HTML
  (`/queue/status` + `#position`), and Concert `-api-json` HTTP `429` JSON
  queues (same-origin `status_url` + `retry_after_seconds`); polls
  `GET /queue/status` with `room_ticket` cookie and `Accept: application/json`,
  handles `ready`, `position`, `waiting: false` (expired), `cookies_required`,
  poll `429` backoff, `403` ban termination, and transient network backoff;
  reloads once upon admission to mint `concert_admit` and separates origin
  service latency from queue wait duration; never follows `/_concert/*` links
  as origin pages.
- **Opt-in static asset tier (`-assets`, `-assets-per-page 1..64`).** Pure-stdlib
  HTML and CSS same-origin asset extractor (`plan.ExtractAssets`,
  `plan.ExtractCSSAssets`, `FuzzExtractAssets`) honoring `<base href>`,
  per-lemming 2048-entry cache, and `concert_admit` pass enforcement — reported
  in `AssetReport` strictly separate from page visits and `-p95-budget`.
- **Neon Arcade static assets.** Added `/assets/arcade.css` and
  `/assets/logo.svg` static endpoints in `examples/demo/main.go` exempt from
  demo chaos and waiting rooms.

### Architecture, Precision, Reports & Supply Chain

- **Go v2 Module Path (`github.com/andreimerlescu/lemmings/v2`).** Aligned
  `go.mod`, internal imports, `Makefile`, and `go install` documentation with
  Go Semantic Import Versioning (`go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest`).
- **DDD Package Refactor.** Flat root `package main` refactored into
  `cmd/lemmings/main.go` and 12 cohesive `internal/*` packages (`version`,
  `secrets`, `events`, `discovery`, `plan`, `forensics`, `swarm`, `report`,
  `delivery`, `dashboard`, `metrics`, `cli`).
- **High-Precision Histograms & Report Schema `"2.0"`.** Tightened latency
  histograms to `<= 0.5%` relative error across 4096 `uint64` bins
  (`histogramGrowth = 1.005`), raised exact sample buffers to
  `exactGlobalSamples = 8192` and `exactPathSamples = 1024`, added
  `schema_version: "2.0"`, separated Page vs. Asset telemetry, enriched
  `PlanReport`, `RoomReport`, `ConcertReport`, `ScoutReport`, distinct
  Started/Completed/Unborn counts, prominent `DATA DROPPED` warnings across CLI,
  dashboard, HTML, and Markdown reports, truncated-body detection
  (`ErrTruncatedBody`), and excluded cancelled visits from HTTP status classes.
- **24 Prometheus Metrics (`-observe`).** Added `lemmings_plan_lemmings_total`
  (`outcome="passed|failed"`), `lemmings_rogue_lemmings_total`,
  `lemmings_assets_total`, `lemmings_asset_duration_seconds`,
  `lemmings_queue_polls_total`, `lemmings_bans_total`, and
  `lemmings_scout_visits_total` alongside existing metrics, excluding cancelled
  visits from `lemmings_visits_total{status_class}`.
- **Hybrid Forensics Reports & Naming.** Self-contained HTML embeds bounded
  base64 screenshots (12 MiB cap for local/S3, 2 MiB cap for email); Local and
  S3 targets also write `.artifacts/<id>.<ext>` files for Markdown relative
  image links and lean JSON. Added `-unique-report`, `-report-name`, and
  Markdown table cell (`mdCell`) & adaptive code-fence (`mdFence`) escaping.
- **Regression Comparison (`lemmings compare`).** Compare baseline vs candidate
  JSON reports (including `AssetP95DeltaMS` and `QueueHeldDelta`) with
  `-max-failure-delta`, `-max-p95-delta`, `-max-rescued-drop`, new/resolved
  error fingerprints, and CI exit code `2` on regression.
- **Ramp Shapes & Dry Run.** Added `-ramp-shape` (`linear`, `fibonacci`, `step`)
  and `-dry-run` pre-flight validation.
- **Build Supply Chain.** Added zero-dependency deterministic CycloneDX 1.5 SBOM
  generator (`scripts/generate-sbom.py`), `make vulncheck`, `make sbom`, and
  `make checksums` (`bin/SHA256SUMS`), plus `go mod verify`, `govulncheck`, JS
  syntax checks, and SBOM validation in `.github/workflows/go.yml`.
- **Documentation Suite & Agent Skill.** Complete end-user and security
  documentation under `docs/` (including `docs/plans.md` with 10 user-type
  persona blueprints), root-level `SKILL.md` agent runbook, and refreshed
  `README.md`.

### Security Hardening (`SEC-01`..`SEC-10`)

- **Breaking Change:** Removed `-smtp-pass` / `-spw` CLI flag so SMTP
  credentials never appear in process `argv` (`ps aux`). Use
  `-smtp-pass-file` (requires `0600` permissions), `-smtp-pass-stdin`,
  `LEMMINGS_SMTP_PASS_FILE`, or `LEMMINGS_SMTP_PASS`.
- **Dial-Time SSRF Guard & TLS 1.2+ Everywhere.** Added `discovery.SafeDialContext`
  and `discovery.MinTLSConfig()` across discovery, swarm HTTP transport, and
  remote CDP (`http`/`https`/`ws`/`wss`), blocking `169.254.0.0/16`, `fe80::/10`
  (including zoned addresses), `fd00:ec2::254`, `0.0.0.0`/`::`, multicast, and
  IPv4-mapped IPv6 equivalents before dialing validated literal IPs.
- **Fail-Closed Loopback CDP (`-allow-remote-cdp`).** `-cdp-url` is restricted
  to loopback endpoints by default; non-loopback endpoints require explicitly
  passing `-allow-remote-cdp` (metadata/link-local IPs remain blocked).
- **RFC 6265 Bounded Cookie Jar & CDP Cookie/Origin Guards.** Enforced RFC 6265
  domain/public-suffix scoping and a `256`-entry cap on `InspectingJar`,
  deterministic `(domain, path, name)` replay, domain validation before CDP
  `Network.setCookies`, and Scout off-origin redirect capture blocking.
- **Ctrl-C Drain & CDP Grace Budget.** Detached coordinator `workCtx` via
  `context.WithoutCancel` so post-Ctrl-C drain completes queued captures, and
  reserved `min(2s, timeout/4)` screenshot/DOM grace budget for slow pages.
- Introduced redacting `secrets.Secret` value type and `secrets.ScrubText`
  across error excerpts, JS console logs, and reports.
- Hardened `MailTarget` against SMTP header CRLF injection (`SEC-02`) and
  fail-closed TLS (`MinVersion: tls.VersionTLS12`, `-smtp-require-tls`, `SEC-03`).
- Added loopback `Host`-header guard to Prometheus `/metrics` (`SEC-04`).
- Replaced raw dashboard token cookies with HMAC-SHA256 signed random session
  cookies, added per-remote-IP (`15/min`, `1024`-IP bounded cap, ignoring
  `X-Forwarded-For`) plus global (`60/min`) auth rate limiting (`429`), and
  added `http.NewCrossOriginProtection()` CSRF checks on all state-changing
  routes (`SEC-05`).
- Confined `LocalTarget` writes via atomic temp-file + `Sync` + `Rename` inside
  Go's `os.OpenRoot`, enforced `validateSafeComponent` in both `LocalTarget` and
  `S3Target` (`SEC-06`), and restricted trace files to `0600`.
- Bounded sitemap indexing concurrency to a fixed worker pool
  (`indexConcurrency = 8`, `SEC-07`) and joined every dashboard/metrics/CDP
  goroutine cleanly on shutdown (`SEC-08`).
- Scrubbed spawned headless Chromium child environment to a minimal allowlist
  so ambient host tokens never reach the browser process (`SEC-09`).
- Hardened YAML plan inputs and forensics (`SEC-10`): marked `with-cookies`,
  `with-headers`, `with-body`, and `body` as `json:"-"` so secrets never appear
  in reports, traces, or the dashboard; redacted sensitive `want-headers` and
  all raw `want-cookies` values in check failure details; failed non-GET /
  request-body plan steps capture HTTP-only forensics (skipping bogus GET CDP
  replay); and `-scouts` falls back to `URLPool` if any plan contains non-GET
  steps.

## 1.0.0 — 2026-09-27

The first stable release. Lemmings now behaves like a crowd of real
visitors, shows you their lives while they happen, and tells CI whether the
run passed.

### Watch the swarm

- **An animated dashboard.** Lemmings drop from the entrance hatch onto a
  synthwave terrain, react to every page they read — a hop for 2xx, a bounce
  for redirects, a stumble for 4xx or a failed check, a knockback for 5xx, a
  glitch when nothing answered — queue at the waiting room with their live
  position overhead, and walk into the exit when their life ends.
- **Follow a lemming.** Click any lemming (or press `f`) to follow its life in
  the inspector: its name, browser, language, every page with status, latency,
  time to first byte, redirects, queue time and why it failed.
- **Live charts** for traffic, failures, lemmings alive and latency, a terrain
  map that lights up as the ramp brings terrains online, the busiest paths, a
  rate-limited feed of what is happening, and a "level complete" finale.
- **Synthwave '84, dark and light.** Follows the system theme; `t` toggles.
  Motion respects `prefers-reduced-motion`; `x` toggles effects.
- **One-click sign-in.** The terminal prints a link carrying the token in the
  URL fragment, which never reaches a server and is removed from history.
- **Scale-proof.** Events are batched into five frames a second over a fixed
  stage of 160 lemmings, so the browser's work is the same for 10 lemmings or
  100,000.

### Behave like people

- Each lemming keeps one user agent (desktop and mobile), one language and one
  cookie jar for its whole life, and its own connection pool, like a browser.
- **Link navigation** (default): land on `-hit`, then follow a link on the
  page just read, sending it as the Referer. `-navigation random` keeps the
  old behaviour. Log-out, delete and download links are never followed.
- **Think time** between pages: `-think-min` / `-think-max`, 300 ms–1.2 s by
  default. Set both to `0` for a saturation test.
- `Retry-After` on 429 and 503 is honoured; failures back off even with no
  think time.
- `-request-timeout`, `-max-body-bytes` and `-max-pages` bound each visit and
  each life.

### Check what they saw

- **Journeys** (`-journey file.json`): an ordered list of pages with checks
  for status, content type, required and forbidden text, title and
  `tag`/`#id`/`.class` selectors. A 200 with an error page now fails.
- Every HTML page is checked for being blank. Scripts and media count as
  content, so client-rendered apps are not flagged.
- `-strict-checksum` fails a visit whose body changed since indexing;
  otherwise checksum changes are reported as information.

### Report and gate

- **Rescued**: the share of lemmings whose whole life had no failed page, in
  the spirit of the game's end-of-level score.
- A new HTML report in Synthwave '84 light and dark with traffic and latency
  charts, notable lemmings (the Explorer, Patient Zero, the Unlucky One, the
  Patient One) and their lives page by page, exact status codes, failure
  causes, waiting room statistics and per-path p50/p95/p99.
- A JSON report beside the markdown and HTML, everywhere reports are saved.
- `-trace-file` streams every visit to JSONL without ever overwriting a file.
- **CI gates**: `-max-failure-rate` and `-p95-budget`. Exit codes are `0`
  passed, `1` error, `2` a gate failed, `130` interrupted (the partial report
  is still delivered).
- A colourful terminal: a live sparkline and p95 in the ticker, and the
  rescued score at the end. Colour follows `NO_COLOR` and `-color`.

### Fixed

- v0.0.2 could not start: `-crawl-depth` reused the `-crawl` alias.
- Flag names drifted from the documentation. The documented names now work:
  `-metrics-url-label`, `-s`, `-obs`, `-sh`, `-sp`, `-su`, `-spw`, `-sf`.
- The dashboard, ticker and Prometheus only counted a lemming's visits when it
  died. Visits are now counted the moment they complete.
- Every death was announced twice, so `lemmings_alive` went negative and
  `lemmings_completed_total` doubled.
- `lemmings_bytes_total` was always zero; it now counts visit bytes. The
  Prometheus duration histogram no longer starts with a fabricated sample.
- `EventTerrainDone` was never emitted, so terrains online only went up.
- Per-path percentiles were read from unsorted samples.
- Waiting-room time was counted as latency (one queue could put p99 at
  seconds). Queue time is now reported separately.
- A lemming whose life ended mid-request counted as a failure; it is now
  "cancelled" and excluded from failure rate and latency.
- The JSON report would have included the SMTP password; delivery settings
  are now excluded from every report.
- Report delivery used the already-cancelled context after Ctrl-C.
- A failed STARTTLS handshake silently resent the report in plaintext.
- Non-ASCII email subjects were sent unencoded.
- The dashboard rendered target URLs with `innerHTML`; a hostile sitemap
  could inject script. The page now has a strict hash-based CSP and no
  innerHTML, and rejects non-loopback Host headers.
- Off-origin checks compared string prefixes, so `example.com.evil.test`
  passed as `example.com`. Origins are now compared by scheme, host and port;
  redirects and robots.txt sitemaps off the origin are not followed.
- Sitemap recursion, crawl size and response bodies are bounded; XML entities
  in `<loc>` are decoded; relative links resolve against the page they are on.
- The overflow channel preallocated 500,000 life records (~100 MB) per run.
- `formatInt` rendered `-1000` as `-,1000`.
- The live ticker could overwrite the final summary.

### Changed

- Default `-max-pages` is `0` (a lemming browses until `-until`), and lemmings
  now pause between pages by default. Pass `-think-min 0 -think-max 0
  -navigation random` for the pre-1.0 saturation behaviour.
- `ReportTarget.Deliver` takes a `RenderedReport` (markdown, HTML and JSON).
- `EventBus.Emit` is lock-free and allocation-free; `EventLog` is a ring
  buffer. Events carry the full `Visit`, the newborn's `Identity` and the
  dead lemming's `LifeLog`.

### Removed

- The optional Node/Playwright browser cohort from the pre-release upgrade.
  Its script was loaded by a relative path, so it could not work after
  `go install`, and it made a Go tool depend on Node and Chromium. Lemmings
  stays a single self-contained binary.
- The Python smoke test, replaced by Go end-to-end tests that build and run
  the real binary.

### Try it

    make demo    # Neon Arcade, a local target with a waiting room and some chaos
    make run     # in another terminal; open the dashboard link it prints

## 0.0.2 and earlier

The first versions, before this changelog.
