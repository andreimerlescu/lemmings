# User experience upgrade

This is a complete updated copy of the supplied Lemmings source. Its existing
version and Apache 2.0 license are retained; no release has been published.

## What changed

- A lemming retains its user agent, language and isolated cookie jar. It starts at
  `-hit`, follows same-origin links, sends a referrer, and pauses between pages.
- Ordered JSON journeys provide named steps, expected status codes, content type,
  required/forbidden content, title checks and element checks.
- Every visit records a session ID, sequence, requested/final URL, exact status,
  redirect responses, queue polls, cookie names, decoded document bytes, DNS/TCP/
  TLS timing, final-hop TTFB, total duration and a cancellation/error category.
- Live counters and report aggregates update on every visit. They no longer wait
  for the session to die. A failed page is separate from a session failing to start.
- An optional, small Playwright/Chromium cohort runs alongside the HTTP swarm.
  It measures a real DOM, visible elements, console and uncaught JavaScript errors,
  failed resources, broken images, and short-window paint/layout observations.
  Failed browser visits save a screenshot.
- HTML and Markdown reports now include user-experience diagnostics. Local and S3
  destinations also receive JSON. An optional JSONL trace records HTTP visits.
- CI can fail on a configured failure rate, a p95 budget, browser failures, or
  incomplete evidence. Ctrl-C preserves the report and returns exit code 130.
- `Retry-After` is honored for 429/503, up to five minutes and within the remaining
  session lifespan. Failed visits receive a minimum 100 ms pause.

## Defaults and compatibility

Old CLI flags, dashboard authentication, waiting-room support, Prometheus,
local/S3/SMTP delivery, and the terrain/pack/limit model remain available.

The default workload now pauses **300–1,200 ms** between pages, follows links,
and stops after **20 pages or the session lifespan**, whichever comes first.
This intentionally changes the workload from unpaced saturation to paced visits.

To request the earlier saturation pattern explicitly:

```sh
./bin/lemmings -hit http://localhost:8080/ \
  -think-min 0 -think-max 0 -max-pages 0 -navigation random
```

Checksum differences are informational by default. Use `-strict-checksum=true`
only for pages whose index-time bodies should be byte-for-byte identical.
Dynamic nonces, timestamps and personalized HTML usually make that inappropriate.

Browser sessions are **additional traffic** and maintain their own cookies and
connections. They do not replay the exact cookie state of a particular Go user.
Browser counters and performance observations are separate from HTTP percentiles.

## Correctness repairs

1. Fixed `-crawl-depth` registering the `-crawl` alias, which prevented CLI startup.
2. Removed duplicate runtime death events and updated alive/completed counts at
   their source, including when a detailed lifelog is dropped.
3. Fixed unsorted per-path percentiles and removed a fabricated initial zero
   observation from the Prometheus duration histogram.
4. Counted transferred bytes on visit events in the Prometheus exporter.
5. Released the event-bus lock before callbacks, so callbacks can unsubscribe.
6. Resolved relative links against the actual document URL.
7. Replaced origin-prefix matching with parsed scheme/host/effective-port checks.
   Off-origin redirects and robots-discovered sitemaps are not followed.
8. Bounded sitemap recursion, duplicate sitemap traversal, discovery counts,
   response body reads, queues, retained sessions, paths and latency storage.
   Parsed sitemap XML entities correctly.
9. Acquired concurrency slots before creating lemming goroutines. There is one
   waiting scheduler per terrain instead of a waiting goroutine per planned user.
10. Used escaped HTML templates for reports and escaped dashboard configuration.
11. Cleared transient queue-poll errors after a successful poll, and recognized
    admission when the queue signature disappears even if dynamic HTML changed.
12. Used an uncancelled, bounded delivery context after Ctrl-C.

The original tests remain. Two existing fixtures were corrected: one expected
the fabricated Prometheus sample; another copied an atomic-containing struct and
failed `go vet`. New tests cover the behavior above. `scripts/smoke.py` exercises
the actual binary, which the original unit tests did not do.

## Evidence limits

- HTTP parsing does **not** execute JavaScript, load subresources, validate HTML
  conformance, or prove visual correctness. The HTML5 parser repairs malformed
  input. Use explicit assertions and browser verification for meaningful checks.
- Journeys currently support ordered **GET navigation**. They do not submit login
  or checkout forms, extract CSRF tokens, or measure INP. Cookies set by GET
  responses persist within each session. No implicit account mutation is inferred.
- Browser checks cover Chromium only, with desktop and narrow touch viewports.
  The script observes 500 ms after navigation/selector checks; slow or later
  background failures may fall outside this window. LCP/CLS are observations,
  not complete field Web Vitals. There is no visual-baseline diff.
- Browser external dependencies are blocked and reported. Sites needing a CDN,
  external authentication, or third-party assets can fail under this scope.
- Navigation is a closed-loop workload: slow pages reduce arrival rate. These
  are observed latencies, not a corrected constant-arrival-rate benchmark.
- Exact visit/status/check totals are streamed independently of sampled details.
  At most 100 session tails (100 visits each) and 200 browser visits are embedded.
  Browser JSONL includes all browser visits. Enable HTTP JSONL when every HTTP
  visit is needed; its bounded buffer exposes loss rather than hiding it.
- HTTP latency samples are exact through 4,096 values per series, then use a fixed
  histogram with upper-bucket error at most 5% above 1 microsecond. Cancelled
  visits are excluded from latency percentiles and the failure-rate denominator.
  TTFB always uses the histogram. Path cardinality is capped at 1,000 plus `other`.
- DNS/TCP/TLS fields aggregate traced connection setup; concurrent dial attempts
  may overlap. They are diagnostics, not additive pieces of total request time.
  Reused connections can legitimately report zero setup time.
- HTTP byte counts represent decoded document bodies and queue polls. They do
  not represent compressed wire bytes, redirect bodies, or browser asset bytes.
- Report URLs omit userinfo, query strings and fragments. Cookie values, request
  headers and full HTTP bodies are not stored. Titles, URL paths, browser error
  text and failure screenshots can contain application data. Keep artifacts in
  the same access scope as the application being tested.
- Reports retain the existing date/domain filename convention. Use a distinct
  `-save-to` directory per run to avoid overwriting a same-day report. Trace files
  refuse overwrite; browser runs create unique subdirectories.

The retained `REVIEW.md`, `TODO.md`, `TESTS.md` and `EXAMPLES.md` describe the
earlier snapshot and are marked historical. This document and README.md describe
the updated implementation.
