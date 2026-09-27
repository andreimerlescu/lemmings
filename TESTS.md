# Lemmings Test Suite

> Proving that what lemmings measures is true, and that what it reports can be trusted.

---

## Purpose

Lemmings exists to answer one question before it matters: **can your application
handle the traffic you are about to send to it?**

The answer is only useful if the tool producing it is itself trustworthy. A load
testing tool that produces inaccurate metrics, silently drops data, or behaves
differently under test conditions than under production conditions is worse than
no tool at all — it produces false confidence.

This document explains what the lemmings test suite proves, what each category
of test demonstrates, and why organizations can trust the reports lemmings
generates when making launch decisions.

As of v1.0.0 the suite contains 438 tests: 397 unit, integration and
end-to-end tests, 9 fuzz targets and 32 benchmarks, all passing under the race
detector on Linux, macOS and Windows.

---

## Test Categories

Lemmings uses four categories of tests, each chosen for a specific reason.

### Unit Tests

Unit tests verify that every function and method does exactly what its
documentation says it does, for all inputs in its defined range. They are the
foundation of the suite. A unit test failure means a contract has been broken —
either the implementation drifted from its specification, or the specification
was wrong to begin with.

Unit tests in lemmings are written to be:

- **Deterministic.** Every unit test produces the same result on every run,
  on every machine, in every environment. There are no sleeps, no random seeds
  shared across tests, no dependencies on external services. Network calls go
  to local httptest servers that are constructed and torn down within the test
  itself.

- **Named after contracts, not implementations.** A test named
  TestDetectWaitingRoom_MalformedPosition tells you exactly which contract
  is being verified — the function must handle malformed position content
  gracefully — not how the implementation works internally. When this test
  fails, the failure message identifies the broken promise, not a line number.

- **Minimal in scope.** Each test verifies exactly one contract. Tests that
  verify multiple unrelated behaviours in a single function produce ambiguous
  failures and make root cause analysis harder.

### Fuzz Tests

Fuzz tests verify that functions which parse externally-sourced data never
panic, never produce logically impossible output, and always satisfy their
invariants regardless of what bytes they receive.

Lemmings applies fuzz testing only where it adds value: functions that parse
arbitrary bytes from live HTTP servers, user-supplied configuration values,
and numeric formatting routines that appear in dashboard HTML. These are the
functions that, in production, receive content from sources lemmings does not
control. A web server under load may return truncated responses, malformed
HTML, corrupted XML, or waiting room pages with unexpected structure. The
fuzz tests prove that lemmings handles all of these gracefully.

The fuzz targets in lemmings and the invariants each one enforces:

| Function | Invariants verified |
|---|---|
| detectWaitingRoom | Never panics. Position always >= 0. Position is 0 when not detected. |
| sha512sum | Never panics. Always returns exactly 128 hex characters. |
| extractSitemapLocs | Never panics. Never returns empty strings in the result slice. |
| extractHTMLLinks | Never panics. All returned links have the origin as prefix. |
| resolveURL | Never panics. Never returns URLs containing fragment identifiers. Every result stays on the base URL's origin. |
| handleAuth | Never panics. Always returns a valid HTTP status code. |
| dashboardHTML | Never panics. Always returns non-empty output. |
| formatInt | Never panics. Always returns a non-empty string. |
| formatBytes | Never panics. Always returns a string with a recognised unit suffix. |

Each fuzz target includes a seed corpus of known interesting inputs — empty
bodies, malformed structures, boundary values, and real-world examples drawn
from the room package's actual HTML. The seed corpus ensures that even without
extended fuzzing runs, the most likely production edge cases are covered.
Inputs that once failed are kept under `testdata/fuzz/` and replayed by every
plain `go test` run: FuzzResolveURL found that a host containing an invalid
byte re-escapes into a different origin, and that input now guards the fix.

### Benchmark Tests

Benchmark tests verify that the functions on the hot path of the swarm perform
within the bounds required for lemmings to scale. A load testing tool that
itself becomes the bottleneck produces results that describe the tool, not the
target. Benchmarks make that failure mode visible before it affects a real test.

Benchmarks are compiled by every `go test` run but only executed when asked
for with `-bench`:

    go test -run='^$' -bench=. -benchmem ./...

This keeps the default test run fast while CI runs the benchmarks in their own
job.

The benchmarked functions and what a regression in each one means:

| Benchmark | What a regression means |
|---|---|
| BenchmarkSha512sum_1KB / _1MB | SHA-512 is called on every page body by every lemming on every visit. A regression here degrades throughput across the entire swarm proportionally to visit count. |
| BenchmarkDetectWaitingRoom_Normal | This executes on every HTTP response body. A regression here reduces the maximum sustainable visit rate. |
| BenchmarkEventBus_Emit_Concurrent | The EventBus is the shared nervous system of the swarm. Emit takes no lock and allocates nothing; a regression here stalls lemming goroutines across all terrains simultaneously. |
| BenchmarkClientRegistry_Broadcast_OneClient / _HundredClients | Broadcast runs several times a second per connected dashboard client. A regression here delays what the dashboard shows without affecting the swarm itself. |
| BenchmarkReporter_Ingest_Concurrent | Ingest runs once per lemming death. At scale, lock contention here creates a queue of goroutines waiting to record their results, artificially extending run time. |
| BenchmarkSendLifeLog_Concurrent | The non-blocking send is the critical handoff between a dying lemming and the result collector. Any latency here holds a goroutine open past its intended deadline, corrupting timing metrics. |

---

## What the Tests Prove About Lemmings Reports

### Metric Accuracy

The test suite proves that every counter in a lemmings report reflects reality.

TestIngest_StatusCodeBuckets verifies that HTTP status codes 200, 201, 301,
302, 404, 500, and 503 land in the correct 2xx, 3xx, 4xx, and 5xx buckets.
TestReporter_Ingest_AccumulatesBytes verifies that transferred bytes are
summed correctly across visits. TestReporter_Ingest_ConcurrentSafe verifies
under the race detector that concurrent ingestion from multiple goroutines
produces accurate totals — not totals that are off by the number of goroutines
that raced on an unprotected counter.

An organization reading a lemmings report can trust that the number labeled
"2xx" is the exact count of responses with status codes between 200 and 299,
not an approximation, not a value that varies between runs of the same test.

### Lifecycle Event Accuracy

The test suite proves that the `alive`, `completed`, and `failed` counters —
visible in the STDOUT ticker, the live dashboard, the Prometheus
`lemmings_alive` gauge, and the final report — increment exactly once per
lemming and decrement exactly once per lemming.

This is not trivial. Both the `Terrain` and `Lemming` components could
plausibly emit birth and death events, and an early implementation did emit
`EventLemmingBorn` from both. The result was that every counter-based
subscriber — the dashboard's alive counter, the Prometheus gauge, and any
test that tracked concurrent lemmings — drifted upward by one per lemming,
reporting 12 alive when only 2 were actually running. That kind of bug is
invisible in small test runs and catastrophic in real ones: a report showing
5,000 alive lemmings might actually describe 2,500 of them.

The test suite enforces a single-owner contract for lifecycle events:
`EventLemmingBorn`, `EventLemmingDied`, and `EventLemmingFailed` are emitted
exclusively by the `Terrain` (`Terrain.live`, and `Terrain.failUnborn` for
lemmings that never got a slot). `Lemming.Run` emits only per-visit and
waiting room events that describe its in-flight work.

Before 1.0 the swarm's collector emitted a second `EventLemmingDied` for every
lemming, so the Prometheus `lemmings_alive` gauge went negative and
`lemmings_completed_total` doubled. TestSwarm_StreamsVisitsAndOwnsLifecycleOnce
runs a real swarm and counts exactly one birth and one death per lemming.

TestSpawnLemming_EmitsDiedEvent verifies that the Terrain emits Died for its
lemmings, and TestSpawnLemming_EmitsFailedOnSemaphoreTimeout that every
lemming that never started is counted as failed. TestRun_DoesNotEmitBornEvent verifies the negative contract — that
`Lemming.Run` does not emit Born, preventing any future contributor from
accidentally reintroducing the double-emit bug.
TestTerrain_Launch_SemaphoreRespected uses the event bus to count
concurrent live lemmings and verifies the count never exceeds the semaphore
limit — which is only true if Born fires exactly once per lemming.

An organization reading `alive: 5,000` in a lemmings report or on the
dashboard can trust that exactly 5,000 lemming goroutines are running, not
twice that many and not half.

### Percentile Accuracy

The test suite proves that timing percentiles are computed correctly using the
nearest-rank method on a fully sorted duration slice while every sample is
kept, and within a proven 2% bound once a run has more samples than lemmings
keeps (4,096 overall, 512 per path).

Two pre-1.0 defects are pinned by tests. Per-path percentiles were read from
an unsorted slice, so a path's p50 was simply whichever sample arrived in the
middle; TestReporter_PercentilesStayExactThenBounded feeds unsorted samples
and checks the exact answer. And a visit held in a waiting room counted its
whole queue time as latency, so one queue could put p99 at nine seconds on a
server answering in half a second; visits now carry a separate Latency, the
server's time for the page it finally served, and the report shows queue
time on its own. TestHistogram_QuantileErrorBound checks the histogram
against exact answers across three orders of magnitude.

TestPercentile_P50_KnownDataset and TestPercentile_P99_KnownDataset verify
the percentile computation against datasets where the correct answer is known
analytically. TestPercentile_PreSortedAssumption documents the pre-sort
contract so that any future contributor who passes an unsorted slice understands
why they are getting wrong results.

An organization using p99 latency from a lemmings report to make infrastructure
decisions can trust that the number is the 99th percentile of actual observed
request durations, computed correctly, and the report states which method
produced it.

### No Silent Data Loss

The test suite proves that lemmings never silently loses result data, even under
extreme load conditions.

Since 1.0 every visit is counted the moment it completes, so visit totals do
not depend on a lemming's life record reaching the collector at all.
TestSwarm_StreamsVisitsAndOwnsLifecycleOnce checks the live counter is
already non-zero while the first visit is being announced.

TestSendLifeLog_IncrementsDroppedWhenBothFull verifies that when both the
primary and overflow result channels are full — a condition that indicates the
collector is genuinely overwhelmed — lemmings increments the dropped_logs
counter rather than blocking the lemming goroutine or discarding data
silently. A dropped life record loses only that lemming's per-visit detail;
the counter appears in the STDOUT ticker, the dashboard and the report.

TestCollectResults_DrainsPrimaryAndOverflow verifies that after the primary
channel closes, the collector drains the overflow channel completely before
exiting. No LifeLog that reached either channel is discarded.

An organization can inspect the dropped_logs value in a lemmings report and
know definitively whether any session detail was not collected. Visit totals,
status codes and percentiles are exact either way. The optional -trace-file
reports how many lines it wrote and how many it could not.

### Waiting Room Accuracy

The test suite proves that lemmings correctly detects, records, and times
waiting room experiences.

TestRun_WaitingRoom_Detected verifies detection using a server that returns
actual waiting room HTML — the same HTML structure produced by the room
package. TestRun_WaitingRoom_Position verifies that the queue position number
embedded in the page is correctly extracted. TestRun_WaitingRoom_DurationRecorded
verifies that the duration from first waiting room encounter to admission is
recorded as a positive value. TestWaitInRoom_ExitsOnContextCancellation
verifies that a lemming whose -until timer expires while in the waiting room
records the incomplete wait rather than hanging indefinitely.

FuzzDetectWaitingRoom extends this guarantee to adversarial input: no matter
what bytes a server returns — truncated HTML, nested position markers,
malformed numeric content — the detector never panics and never returns a
negative position. This means a compromised or misbehaving upstream cannot
crash the swarm through the waiting room detection path.

TestWaitingRoom_AdmitsDynamicHTML verifies admission when the queue page
disappears even though the admitted page carries a fresh nonce that no longer
matches the index-time checksum, and that the queue position is announced as
it changes so the dashboard can show it over each lemming's head.

An organization that uses both room and lemmings together can determine from
a lemmings report how many lemmings were placed in the waiting room, what their
average and peak queue positions were, how long they waited before admission, and
how many died in the queue without ever reaching the application. This is
precisely the data needed to tune waiting room capacity against expected traffic.

### Behavioral Realism

The test suite proves that each lemming behaves like a realistic browser session.

TestRequest_SetsUserAgent verifies that every HTTP request carries a User-Agent
string drawn from a pool of realistic desktop and mobile browser identities.
TestRandomUA_NotAlwaysSame verifies that lemmings are born with varied
identities. TestLemming_SessionPersonaAndNavigation verifies that a lemming
then keeps its user agent, language and cookies for its whole life, sends the
page it came from as its Referer, and follows a link from the page it just
read — as a person does.

TestInspectPage_NavigableLinks verifies that lemmings never follow download
links, log-out or delete links, or links that leave the origin, so a load
test cannot sign its own sessions out or mutate data by wandering.
TestThink_HonoursRetryAfter and TestThink_BacksOffFailuresWithZeroThinkTime
verify that lemmings pause between pages, respect Retry-After on 429 and
503, and back off a failing origin instead of spinning against it.

TestSpawnLemming_CookieJarIsolation verifies that each lemming receives its own
isolated cookie jar via cookiejar.New(), meaning that session cookies set by
the target application for one lemming do not leak into another lemming's
requests. This matches the behaviour of real users who each have their own
browser session.

TestPickURL_UsesVariation verifies that lemmings navigate to different pages
across visits rather than hammering a single endpoint. This produces a realistic
distribution of load across the application's URL surface, which is necessary
for identifying bottlenecks in specific routes rather than only in shared
infrastructure.

An organization can trust that a lemmings test approximates the behaviour of
real users, not the behaviour of a simple request replay tool.

### Checksum-Based Change Detection

The test suite proves that lemmings correctly identifies when a page's content
has changed since indexing.

TestRun_ChecksumMatch verifies that when a server returns the same content
that was indexed at boot time, the visit is recorded with Match: true.
TestRun_ChecksumMismatch verifies that when a server returns different content
— simulating a page that changed, an A/B variant, or an error page — the visit
is recorded with Match: false. The checksum for each page is computed using
SHA-512, which FuzzSha512sum proves always produces a 128-character hex string
regardless of input.

An organization running lemmings as a CI health check can detect content changes
— including unintentional ones — by observing match: false visits in the
report. A sudden increase in mismatch rate during a load test is a signal that
the application is returning error pages or degraded responses under load.

### URL Pool Construction

The test suite proves that the URL pool — the shared read-only navigation
surface every lemming reads from — is constructed correctly from whatever
discovery mechanism the target supports.

TestBuildURLPool_UsesSitemap verifies that sitemap.xml at the standard path
is preferred when available. TestBuildURLPool_FallsBackToRobots verifies
that a Sitemap: directive in robots.txt is used when the standard path
returns 404. TestBuildURLPool_FallsBackToCrawl verifies that when neither
sitemap source is available and `-crawl` is enabled, origin-scoped anchor
links are discovered via depth-bounded HTML traversal.
TestBuildURLPool_FallsBackToOriginOnly verifies that the swarm still runs
with just the origin URL when nothing else is available — a swarm never
fails to launch because of an empty pool.

FuzzExtractSitemapLocs, FuzzExtractHTMLLinks, and FuzzResolveURL extend
these guarantees to adversarial input: malformed XML, truncated HTML, and
hostile href values (javascript:, data:, off-origin URLs with scheme
confusion) never crash the pool builder, never produce off-origin URLs,
and never produce URLs with fragment identifiers.

An organization can trust that the URL list in the pool — visible in the
boot summary as "indexed N URLs" — accurately represents the reachable,
origin-scoped navigation surface of the target application.

### Report Delivery Reliability

The test suite proves that report delivery is robust to partial failure
and never silently drops output.

TestReporter_Write_DeliversToAllTargets verifies that when multiple
destinations are configured via `-save-to`, every destination receives the
rendered report. TestReporter_Write_ContinuesOnPartialFailure verifies the
critical partial-failure guarantee: if one target fails (an unreachable S3
bucket, a misconfigured SMTP server, a read-only local directory), the
other targets still receive the report. A failure in S3 delivery does not
prevent the local `.md` and `.html` files from being written.
TestReporter_Write_ContextCancellation verifies that cancelling the swarm
context aborts in-progress deliveries cleanly without hanging.

TestLocalTarget_Deliver_CreatesFiles verifies that local delivery creates
both the markdown and HTML files in the expected directory tree, and
TestLocalTarget_WritesJSON the machine-readable JSON beside them.
TestReportJSON_NeverContainsDeliverySecrets verifies that SMTP credentials,
email addresses, bucket names, URL userinfo and query strings never appear
in any report format — before 1.0 the JSON report included the SMTP password.
TestMailTarget_FailedSTARTTLSIsNeverDowngraded verifies that a server
offering STARTTLS but failing the handshake gets an error, not the report in
plaintext, and TestMailTarget_DeliverHonoursContext that a stalled mail
server cannot hang the end of a run.
TestLocalTarget_Deliver_CreatesDirectoryTree verifies that the destination
directory is created if it does not exist.
TestMailTarget_BuildMessage_IsValidMIME verifies that email delivery
produces a multipart/mixed MIME message containing both the HTML report
inline and the markdown report as an attachment, with correct headers and
quoted-printable encoding.

An organization running lemmings in a CI pipeline that writes locally AND
uploads to S3 AND emails stakeholders can trust that a transient failure
in any one of those paths will not cost them the report. The partial
failure is logged, the other paths complete successfully, and the CI job
exit status accurately reflects which deliveries succeeded.

### Dashboard Security

The test suite proves that the dashboard's authentication cannot be bypassed and
that the raw token is never stored in memory after printing.

TestNewDashboard_TokenHashNotRaw verifies that d.tokenHash is not equal to
the raw token passed to NewDashboard — only the SHA-512 hash is stored.
TestHandleAuth_RejectsWrongToken verifies that an incorrect token returns 401.
TestRequireAuth_RejectsNoCookie and TestRequireAuth_RejectsWrongCookieValue
verify that protected endpoints are inaccessible without a valid session.
TestServe_AuthFlow verifies the complete flow end-to-end against a real running
server using a cookie jar.

The constant-time comparison used in handleAuth and requireAuth prevents
timing attacks where an attacker could determine the correct token length or
prefix by measuring response time differences. This is tested structurally by
verifying that crypto/subtle.ConstantTimeCompare is used rather than a direct
equality check.

TestDashboard_CSPAllowsExactlyTheInlineAssets hashes the script and style
actually served and checks the Content-Security-Policy allows exactly those,
with `default-src 'none'`, no `unsafe-inline`, no inline styles and no
innerHTML anywhere in the page. TestDashboard_ConfigCannotBreakOutOfItsScriptTag
checks that a hostile -hit value stays escaped, and
TestDashboard_LocalOnlyRejectsForeignHosts that a DNS-rebinding page cannot
reach the dashboard through a public hostname pointed at 127.0.0.1.

FuzzHandleAuth extends this guarantee to adversarial token values: the
authentication handler never panics regardless of what bytes arrive in the
POST body, and always returns one of a small set of valid HTTP status codes
(200, 303, 400, 401, 405). An attacker cannot crash the dashboard through
the auth endpoint.

### Journeys and Page Checks

The test suite proves that a page which answers 200 but shows an error, or
nothing at all, is caught.

TestInspectPage_DetectsSoftError feeds an error page served with 200 and
checks that the journey's contains, not-contains, title and selector checks
fail. TestInspectPage_NotBlank checks the blank-page rule: text, images or
scripts count as content, so client-rendered apps are not flagged, while a
page with nothing a person could see is. TestJourney_WalksStepsInOrderAndStops
checks that every lemming walks the steps once, in order, with each step's
checks applied, and TestParseJourney_Validation that journeys leaving the
origin, using unsupported selectors or containing typos are rejected before
the first lemming moves.

### The Dashboard's Live Picture

The observatory behind the dashboard sees every event, so its contracts are
tested directly. TestObservatory_StageSlotsAreBoundedAndReused checks that
no more than 160 lemmings are animated whatever the swarm size, and that
slots are reused. TestObservatory_CollapsesVisitsPerFrame checks that many
visits in one frame become one event that keeps the worst outcome, attributed
to the right lemming when a slot changes hands. TestObservatory_FeedIsRateLimited
and TestObservatory_TerrainTilesGroupLargeSwarms bound the feed and the
terrain map, so the browser's work stays constant at any scale.

### The Binary, End to End

The end-to-end tests build the real binary and drive it as an engineer or CI
pipeline would. TestE2E_StartsAndReportsVersion guards against the v0.0.2
defect where two flags shared an alias and the binary could not start at all.
TestE2E_PassingRunWritesEveryFormat and TestE2E_FailingGateExitsTwo check
exit codes 0 and 2 and all three report formats. TestE2E_DashboardAndInterrupt
signs in with the one-click link, reads the live state, the inspector and a
stage frame from a running swarm, then sends Ctrl-C and checks for exit code
130 with the report still delivered. They are skipped with `-short`.

---

## How to Run the Tests

### Standard test run

Runs every test with race detection enabled, including the end-to-end tests
that build the binary. This is the command that should run in every CI
pipeline on every commit. `-count=1` disables result caching.

    go test -race -count=1 ./...

Add `-short` to skip the end-to-end tests.

### Every fuzz target

Go fuzzes one target at a time. The Makefile runs each in turn:

    make test-fuzz            # 10s each
    FUZZTIME=60s make test-fuzz

### Extended fuzzing — specific function

Runs extended fuzzing against a single function. Use this when investigating a
specific parsing function before a major release.

    go test -run='^$' -fuzz='^FuzzDetectWaitingRoom$' -fuzztime=60s .
    go test -run='^$' -fuzz='^FuzzExtractSitemapLocs$' -fuzztime=60s .
    go test -run='^$' -fuzz='^FuzzResolveURL$'         -fuzztime=60s .

### Benchmark run

Runs every benchmark. Use this before and after changes to hot path functions
to verify no performance regression was introduced.

    go test -run='^$' -bench=. -benchmem -benchtime=5s ./...

### Coverage report

Generates a coverage profile and opens it in the browser. Use this to verify
that new code added to any source file has corresponding test coverage.

    go test -coverprofile=coverage.out ./...
    go tool cover -html=coverage.out

---

## Trusting the Results in Production Decisions

A lemmings report is appropriate for use in the following production decisions:

**Go/no-go for a traffic event.** If a lemmings run with parameters matching
the expected traffic shape passes its `-max-failure-rate` and `-p95-budget`
gates, the application has demonstrated it can handle that traffic. The test
suite proves the measurement is accurate and that the exit code reflects it.

**Waiting room capacity planning.** If a lemmings run produces non-zero
waiting_room counts, the report contains queue position distributions and wait
durations that tell you exactly how long users would wait at the tested traffic
volume. Adjust room capacity parameters and re-run until the wait duration is
acceptable.

**Infrastructure sizing.** Run lemmings at increasing -terrain and -pack
values until the first 5xx responses appear. The parameters at that threshold
define the capacity ceiling of the current infrastructure. Add capacity and
re-run to verify the ceiling has moved.

**CI health monitoring.** Run lemmings daily against production or staging with
a small -terrain and -pack that complete in under a minute. A change in 2xx
rate, checksum match rate, or p99 latency between runs is an early signal of
regression before customers report it.

**Post-incident capacity analysis.** Run lemmings with parameters reconstructed
from incident traffic logs to reproduce the load that caused the incident. Use
the per-path breakdown to identify which routes degraded first and under what
conditions.

---

## A Note on What Lemmings Does Not Prove

Lemmings proves that your application can handle the traffic shape you tested
against. It does not prove:

- That your application will handle traffic shapes you did not test.
- That your application is secure against malicious traffic.
- That your application will perform correctly under geographically distributed
  load with real network latency — lemmings runs from a single origin.
- That your application's behaviour under load matches its behaviour at rest
  in all respects — only in the respects that lemmings measures.
- That pages render correctly in a browser. Lemmings reads what the server
  sends; it does not run JavaScript or load images, stylesheets or scripts.
- That the arrival rate was constant. Each lemming waits for a page before
  choosing the next, so a slow server receives fewer requests — as it would
  from real people. The report shows the rate actually achieved.

Lemmings is one instrument in a larger observability strategy. It produces
trustworthy results within its defined scope. Understanding that scope is part
of using the results responsibly.

---

lemmings is open source software released under the Apache 2.0 license.
The test suite is part of the package and is itself subject to the same license.
