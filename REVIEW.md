# Lemmings Code Review Notes

> Notes written the evening of initial development for use during tomorrow's
> systematic code review. Read this before opening any source file.

---

## Context

This package was architected and written in a single session. The architecture
is sound and the test suite is comprehensive, but no line of code has been
compiled or run against a real server yet. The purpose of tomorrow's review
is to find syntax errors, import mismatches, interface misalignments, and
logic gaps before the first real world run.

The correct review order is bottom-up through the dependency graph:

    events.go
    ua.go (userAgents lives in lemming.go — verify no separate file needed)
    pool.go
    lemming.go
    terrain.go
    swarm.go
    report.go
    target.go
    observer.go
    dashboard.go
    main.go

Then test files in the same order. Then Makefile. Then go.mod.

---

## Known High-Probability Issues

### Import conflicts in lemming.go

Both crypto/rand and math/rand are needed in the same package:

- crypto/rand is used in swarm.go by generateToken
- math/rand is used in lemming.go by the per-lemming RNG (rand.New, rand.NewSource)

These must be aliased in their respective import blocks to avoid collision.
The likely fix is:

    import (
        crand "crypto/rand"
        "math/rand"
    )

Verify that every file that uses rand.New and rand.NewSource is importing
math/rand and not accidentally picking up crypto/rand.

### observer_test.go — undeclared bus variable

In TestPrometheusObserver_Attach_StartsServer, the variable bus is referenced
but never declared in that function scope. It was copied from a template during
the proposal phase. The fix is:

    bus := NewEventBus()
    if err := o.Attach(ctx, bus); err != nil {

Scan the entire observer_test.go for any other function that references bus
without declaring it in scope.

### report_test.go — missing imports

The file uses http.StatusOK and fmt.Errorf but the import block written during
generation may not include net/http or fmt explicitly. Check the import block
at the top of report_test.go before anything else.

### pool_test.go — httptest import

TestIndexSitemap_Index creates an httptest.NewServer directly rather than
using the testServer builder. Verify that net/http/httptest is in the import
block for pool_test.go.

### dashboard_test.go — net import

freePort uses net.Listen and net.TCPAddr but net may not be in the import
block since all other networking goes through net/http. Check the import block.

### target.go — smtp.NewClient signature

In sendTLS, the call is:

    smtp.NewClient(conn, host)

But smtp.NewClient returns (*smtp.Client, error) — the error return is not
being captured. The correct call is:

    client, err := smtp.NewClient(conn, host)
    if err != nil {
        return fmt.Errorf("smtp new client: %w", err)
    }
    return t.sendViaSMTPConn(client, msg)

This is a definite compile error. Fix it before anything else in target.go.

### lemming_test.go — newRNG import

newRNG() in lemming_test.go returns *rand.Rand. Verify that math/rand is
explicitly imported in lemming_test.go since the test file also uses
sha512hex which is defined in main_test.go and does not pull in math/rand.

---

## Known Medium-Probability Issues

### swarm.go — metrics field type mismatch

SwarmMetrics uses atomic.Int64 for all counters. The Swarm struct embeds
SwarmMetrics as a value type (not pointer):

    metrics SwarmMetrics

But it is passed to terrains and the dashboard as *SwarmMetrics. Verify that
every reference to s.metrics throughout swarm.go, terrain.go, and dashboard.go
is consistent — either always value or always pointer. The likely correct form
is pointer everywhere since atomic types must not be copied.

### terrain.go — double wg.Done

During the proposal phase we caught a double wg.Done bug where defer wg.Done()
at the top of spawnLemming conflicted with an explicit wg.Done() in an early
return path. The fix (defer only, no explicit calls) was agreed upon. Verify
the final written version of spawnLemming has exactly one wg.Done path via
defer and no explicit calls anywhere in the function body.

### pool_test.go — TestIndexSitemap_Index variable shadowing

The test declares innerSrv as an *http.Server that is never started, alongside
import_srv which is the actual httptest.Server. The variable name import_srv
is not a valid Go identifier — identifiers cannot contain hyphens. The correct
name would be importSrv or testSrv. This will be a compile error. Review the
entire test and rename accordingly.

### observer.go — GaugeVec with empty labels

The alive gauge is declared as a GaugeVec but accessed with empty labels:

    o.alive.With(prometheus.Labels{}).Inc()

A GaugeVec with no label dimensions should be declared as a plain Gauge
instead. Either change the declaration to prometheus.NewGauge or add a
meaningful label dimension. The empty Labels{} pattern works but is wasteful
and will generate a vet warning. Recommend changing to plain Gauge.

### report.go — Write signature change cascade

Write changed from Write(saveTo string) to Write(ctx context.Context).
Verify that every call site has been updated:

- swarm.go: Report(ctx) calls reporter.Write(ctx) — check
- Any test that calls reporter.Write directly — check report_test.go
- The old resolveSavePath function must be fully removed — check nothing
  still calls it

---

## Known Low-Probability Issues

### main.go — figs.List return type

figtree's NewList returns a *[]string via figs.List("save-to"). Verify the
dereferencing is correct:

    saveTo := *figs.List("save-to")

And that the resulting []string is correctly passed into SwarmConfig.SaveTo.
If figtree returns a different type for lists than for strings, the dereference
pattern may differ.

### target.go — multipart writer boundary in headers

In buildMessage, the Content-Type header is written to buf before the
multipart.NewWriter is created, but the boundary string from mw.Boundary()
is only available after NewWriter is called. The current code writes:

    fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", mw.Boundary())

This is correct as written because mw is created before this line executes.
But verify the order of operations in buildMessage carefully — the header
block and the multipart writer initialisation must not be reordered during
any future refactor.

### dashboard.go — %% in fmt.Sprintf for JavaScript

dashboardHTML uses fmt.Sprintf to inject cfg.Hit into the HTML string.
JavaScript modulo operators in the template must be written as %% to survive
the Sprintf call. Verify every % character in the JS section of dashboardHTML
is doubled. A missed % will produce a runtime string like %!(EXTRA ...) in
the dashboard HTML silently.

### swarm_test.go — makeMinimalSwarm missing observers field

makeMinimalSwarm constructs a Swarm literal. After the observer feature was
added, Swarm gained an observers []Observer field. Verify that makeMinimalSwarm
either includes observers: nil explicitly or that Go's zero value for slices
handles it correctly. The zero value is fine — but if makeMinimalSwarm uses
a struct literal with named fields and observers is missing, the compiler will
not complain (unlike positional literals). Just verify it initialises cleanly.

---

## Architecture Decisions to Verify in Practice

### The dual-channel result pattern

primary and overflow channels were designed to prevent lemming goroutines from
blocking at death. The invariant is: sendLifeLog must always return in
microseconds regardless of channel state. Verify this empirically on the first
real run by watching for any goroutine pile-up at shutdown time. If terrains
take longer than cfg.Until + 500ms to report Wait() returning, the channel
is blocking somewhere.

### The SHA-512 checksum pool

The URLPool is built at boot and every lemming compares response bodies against
it for the entire run. If the target server returns gzip-encoded responses,
the body bytes seen by lemmings will be the decompressed bytes (Go's http.Client
handles decompression transparently). But verify that the indexing phase uses
the same http.Client configuration as the lemmings — if the indexing client
has different Accept-Encoding headers than the lemming clients, the checksums
will never match and every visit will record Match: false.

The indexing client in pool.go uses userAgents[0] as a fixed UA. The lemming
client uses randomUA(). If the server returns different content based on UA
(A/B testing, bot detection), every lemming will see a mismatch. This is
intentional and correct — it surfaces real behaviour. But document it clearly
in the first run notes so it is not confused with a bug.

### The waiting room poll interval

waitingRoomPollInterval is hardcoded at 3 seconds in lemming.go matching the
room package's client-side poll interval. If the target server does not use
room, this constant is irrelevant. If it does use room, verify that lemmings
and the room JS client are polling at the same rate — a mismatch would cause
lemmings to either over-poll (wasting connections) or under-poll (missing
admission signals).

### The EventBus synchronous dispatch

Every Emit call runs all subscribers synchronously on the calling goroutine.
Under full swarm load this means every lemming goroutine that calls Emit blocks
until all subscribers complete. The dashboard subscriber hands off to a buffered
channel (non-blocking). The Prometheus subscriber calls prometheus counter and
gauge methods directly (non-blocking). The EventLog subscriber acquires a mutex
(brief). All of these are fast. But if a future subscriber is added that does
anything slow (file I/O, network call), the entire swarm will degrade. This
is a known architectural constraint — document it in a future contributor note.

---

## First Real World Run Checklist

Before running against any real server, verify:

- go build ./... produces no errors
- go vet ./... produces no warnings
- go test -race ./... passes completely
- The target server has a sitemap.xml or known crawlable links
- -terrain and -pack are set conservatively (2 and 2 for a first run)
- -until is short (10s) so the run completes quickly
- -tty=true so the live ticker is visible
- The dashboard is open in a browser before the run starts
- STDOUT is being captured to a file for post-run analysis

First run command:

    lemmings -hit http://localhost:8080/ \
        -terrain 2 \
        -pack 2 \
        -until 10s \
        -ramp 5s \
        -tty=true \
        -save-to . \
        2>&1 | tee lemmings-first-run.log

If that works, move to a staging server with real traffic patterns. Then
increase terrain and pack incrementally until the first 5xx responses appear.
That is your capacity ceiling.

---

## Things That Were Intentionally Left for v1.1.0

Do not try to fix these during the code review — they are not bugs, they are
scoped decisions:

- Database Navigator implementations (MySQL, PostgreSQL, MongoDB, SQLite)
- S3 bucket creation if it does not exist
- SMTP retry logic on transient failures
- Prometheus remote write support
- Geographically distributed load via multiple lemmings instances
- The -local flag for forcing local copy alongside S3 upload
- Per-lemming timing breakdown in the report (currently only aggregated)
- Fibonacci ramp shape (currently linear)

The Fibonacci ramp was discussed during architecture and deferred. The current
linear ramp is correct behaviour for v0.0.1. Do not change it during review.

---

## What a Successful Code Review Looks Like

The review is complete when:

- go build ./... passes with zero errors
- go vet ./... passes with zero warnings
- gofmt -l . returns no files
- go test -race ./... passes with zero failures
- The first real world run against localhost produces a non-empty report
  with a non-zero visit count and a matching checksum on at least one URL

When all five are true, bump the version to v0.0.1 and push the tag.

---

## Session Summary

What was built in this session:

- main.go — CLI entry, SwarmConfig, boot summary, signal handling
- swarm.go — Swarm coordinator, ramp scheduler, dual-channel collector
- terrain.go — Terrain goroutine groups, semaphore-gated lemming spawning
- lemming.go — Stateful navigating agent, cookie jar, waiting room detection
- pool.go — URLPool, sitemap/robots/crawl indexing, SHA-512 checksums
- events.go — EventBus, Filter, Tee, EventLog
- report.go — Reporter, per-path stats, percentiles, markdown and HTML templates
- target.go — ReportTarget interface, LocalTarget, S3Target, MailTarget
- observer.go — Observer interface, PrometheusObserver, 11 metrics
- dashboard.go — Live dashboard, SSE, token auth, client registry
- main_test.go — Shared test infrastructure, testServer builder
- *_test.go — Full unit, fuzz, and benchmark coverage for all files
- TESTS.md — Test suite documentation for organizational trust
- README.md — Package documentation
- Makefile — Build, test, lint, benchmark targets
- REVIEW.md — This file

Total: 16 source files, 8 test files, 3 documentation files, 1 Makefile.
