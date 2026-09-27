# Changelog

All notable changes to lemmings are recorded here. Versions follow
[Semantic Versioning](https://semver.org).

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
