TODO.md

# Lemmings TODO

> Outstanding items identified during initial development session.
> Work through this list during code review before v0.0.1 tag.
> Items are grouped by priority and file. Check off as completed.

---

## Priority 1 — Will Not Compile

These are definite compile errors identified during generation.
Fix these before attempting go build.

### target.go

- [ ] smtp.NewClient returns (*smtp.Client, error) but the error is not
      captured in sendTLS. Current code:

          smtp.NewClient(conn, host)

      Must be:

          client, err := smtp.NewClient(conn, host)
          if err != nil {
              return fmt.Errorf("smtp new client: %w", err)
          }
          return t.sendViaSMTPConn(client, msg)

### pool_test.go

- [ ] import_srv is not a valid Go identifier — hyphens are not permitted.
      Rename to importSrv or testSrv throughout TestIndexSitemap_Index.

- [ ] TestIndexSitemap_Index declares innerSrv as an *http.Server that is
      never started and serves no purpose. Remove it entirely or replace
      with the correct httptest.Server reference.

### observer_test.go

- [ ] TestPrometheusObserver_Attach_StartsServer references bus without
      declaring it in scope. Add:

          bus := NewEventBus()

      before the Attach call. Scan the entire file for any other function
      with the same issue.

---

## Priority 2 — Will Compile But Will Panic or Produce Wrong Results

These issues will not be caught by the compiler but will surface at runtime
or produce silently wrong output.

### lemming.go

- [ ] Verify import block correctly aliases crypto/rand and math/rand.
      Both are needed in the package. math/rand is used for per-lemming
      RNG. crypto/rand is used in swarm.go for generateToken. If both
      appear in the same file without aliasing the build will fail.
      Expected correct form in any file needing both:

          import (
              crand "crypto/rand"
              "math/rand"
          )

- [ ] Verify that the Accept-Encoding header behaviour of the indexing
      http.Client in pool.go matches the lemming http.Client. If one
      client requests gzip and the other does not, SHA-512 checksums
      will never match and every visit will record Match: false.
      Both clients should either both omit Accept-Encoding (letting Go
      handle decompression transparently) or both set it explicitly.

### observer.go

- [ ] alive is declared as *prometheus.GaugeVec with no label dimensions
      and accessed via o.alive.With(prometheus.Labels{}).Inc(). This
      works but wastes a map allocation on every increment. Change the
      declaration to a plain prometheus.Gauge:

          o.alive = prometheus.NewGauge(prometheus.GaugeOpts{
              Name: "lemmings_alive",
              Help: "Number of lemmings currently running.",
          })

      And update all call sites from:

          o.alive.With(prometheus.Labels{}).Inc()
          o.alive.With(prometheus.Labels{}).Dec()

      To:

          o.alive.Inc()
          o.alive.Dec()

      Update observer_test.go gaugeValue call accordingly.

### dashboard.go

- [ ] Verify every % character in the JavaScript section of dashboardHTML
      is written as %% to survive the fmt.Sprintf call. A single % that
      is a modulo operator in JS will produce runtime output like
      %!(EXTRA string=...) in the rendered dashboard HTML silently.
      Known JS modulo uses in the template: elapsed() function uses %
      for hours and minutes calculation. Verify both are %%.

### swarm.go

- [ ] SwarmMetrics contains atomic.Int64 fields. Atomic types must not
      be copied. Verify that SwarmMetrics is always passed and stored as
      *SwarmMetrics (pointer) and never as a value type. Check every
      function signature that accepts or returns SwarmMetrics. If Swarm
      embeds it as a value:

          metrics SwarmMetrics

      Change to:

          metrics *SwarmMetrics

      And update NewSwarm to initialise it as &SwarmMetrics{}.

### terrain.go

- [ ] Verify spawnLemming has exactly one wg.Done path via defer and
      zero explicit wg.Done() calls in the function body. During
      architecture discussion a double-Done bug was caught and fixed.
      Confirm the fix made it into the final written version. Search
      the function body for any wg.Done() call that is not the deferred
      one at the top.

---

## Priority 3 — Missing Imports

These files are likely missing imports that will cause compile errors.
Verify each import block before running go build.

### report_test.go

- [ ] Verify net/http is imported (needed for http.StatusOK)
- [ ] Verify fmt is imported (needed for fmt.Errorf in makeLifeLog)
- [ ] Verify sort is imported (needed for sort.Slice in BenchmarkPercentile_1M)

### dashboard_test.go

- [ ] Verify net is imported (needed for net.Listen and net.TCPAddr
      in freePort)
- [ ] Verify golang.org/x/net/publicsuffix is imported (needed for
      cookiejar in TestServe_AuthFlow)

### pool_test.go

- [ ] Verify net/http/httptest is imported (needed for httptest.NewServer
      in TestIndexSitemap_Index which bypasses the testServer builder)
- [ ] Verify net/http is imported (needed for http.HandlerFunc in the
      same test)

### lemming_test.go

- [ ] Verify math/rand is imported (needed for newRNG() which returns
      *rand.Rand)
- [ ] Verify net/http/httptest is imported (needed for httptest.NewServer
      in TestRequest_SetsUserAgent and TestRequest_ContextCancelled)

### observer_test.go

- [ ] Verify github.com/prometheus/client_golang/prometheus is imported
- [ ] Verify github.com/prometheus/client_model/go is imported as dto
- [ ] Verify io is imported (needed for io.ReadAll in
      TestObserver_Metrics_Scrapeable)

### terrain_test.go

- [ ] Verify net/http is imported (needed for http.HandlerFunc and
      http.ResponseWriter in TestTerrain_Launch_SemaphoreRespected)
- [ ] Verify github.com/andreimerlescu/sema is imported (needed for
      newTestSema)

### swarm_test.go

- [ ] Verify context is imported
- [ ] Verify fmt is imported (needed for fmt.Errorf in
      TestSwarm_RegisterObserver_AttachError)
- [ ] Verify os is imported (needed for os.ReadDir in
      TestSwarm_Report_WritesFiles)

### target_test.go

- [ ] Verify net is imported (needed for net.Listener and net.TCPAddr
      in startTestSMTPServer)
- [ ] Verify sync is imported (needed for sync.Mutex in
      TestReporter_Write_DeliversToAllTargets)
- [ ] Verify context is imported

---

## Priority 4 — Logic and Correctness Issues

These will compile and may not panic but produce incorrect results
or fail tests unexpectedly.

### pool.go

- [ ] indexSitemap recursion guard: the sitemap index handler recurses
      one level deep into child sitemaps. If a child sitemap is itself
      a sitemap index (two levels of nesting), the recursion continues
      unbounded. Add a depth parameter or a visited URL set to prevent
      infinite recursion on malformed or adversarial sitemaps.

- [ ] crawlOrigin is recursive but uses a shared visited map with a
      mutex. Verify that the recursive goroutine spawning pattern does
      not create a goroutine explosion on a site with many links at
      every depth level. Consider converting to an iterative BFS with
      a work queue bounded by a semaphore.

### report.go

- [ ] buildReportData sorts the global allDurations slice for percentile
      calculation. This sort mutates the slice in place. If Write is
      called more than once (which it should not be, but defensive
      programming matters), the second call will sort an already-sorted
      slice which is harmless but wasteful. More importantly, verify
      that the per-path durations slice in pathStats is also pre-sorted
      before being passed to percentile(). Currently it is not sorted
      before the per-path P50 and P99 calculations. This will produce
      wrong percentile values for per-path breakdown. The fix:

          sort.Slice(ps.durations, func(i, j int) bool {
              return ps.durations[i] < ps.durations[j]
          })

      This sort must happen inside buildReportData before percentile()
      is called on ps.durations.

### lemming.go

- [ ] waitInRoom accumulates BytesIn across all polls:

          visit.BytesIn += bytesIn

      But the initial request before the waiting room was detected also
      set visit.BytesIn. Verify this does not double-count the initial
      request body. The current logic sets BytesIn from the first
      request, then adds to it in the poll loop. This means the first
      request body is counted once (correct) and each subsequent poll
      body is added (correct). Trace through the flow once manually to
      confirm the accumulation is right.

- [ ] The third exit path from waitInRoom — body changed but is not a
      waiting room and does not match expected — exits the wait loop and
      records the visit as-is. But ExitedAt and Duration are set in
      this path. Verify that ExitedAt is set before Duration is computed
      from it, otherwise Duration will be zero.

### swarm.go

- [ ] ramp() sleeps between terrain launches using time.After. If the
      number of terrains is 1, the interval calculation is:

          cfg.Ramp / time.Duration(1)

      Which equals cfg.Ramp. This means a single-terrain run with
      -ramp=5m will wait 5 minutes before declaring the ramp complete
      even though there is nothing left to ramp. Add a guard:

          if n == 1 {
              terrains[0].Launch(ctx)
              return nil
          }

      Or change the sleep to only occur when i < n-1 (which is already
      in the code — verify this guard is present in the final version).

### target.go

- [ ] MailTarget.buildMessage writes the Content-Type multipart header
      directly to buf before calling mw.CreatePart. But multipart.NewWriter
      also writes its own boundary markers when CreatePart is called.
      Verify that the manually written Content-Type header and the
      multipart writer's output do not produce a malformed MIME structure.
      The correct pattern is to let the multipart writer own the boundary
      entirely and not write the Content-Type header manually. Test with
      a real email client or use mime/multipart's own test suite as a
      reference.

- [ ] sendViaSMTPConn calls net.SplitHostPort on a string it constructs
      itself from smtpCfg.Host and smtpCfg.Port. This is redundant — it
      already has host and port as separate values. Simplify to just use
      t.smtpCfg.Host directly rather than round-tripping through a
      formatted string and SplitHostPort.

---

## Priority 5 — Test Coverage Gaps

Issues in the test suite that reduce confidence in the implementation.

### observer_test.go

- [ ] The four swarm integration tests (TestSwarm_RegisterObserver_*)
      were written in observer_test.go but belong in swarm_test.go.
      Move them and the mockObserver type to swarm_test.go and remove
      from observer_test.go to avoid duplicate declarations.

- [ ] TestObserver_Metrics_Scrapeable and
      TestObserver_Metrics_ContainsExpectedNames both start a real HTTP
      server and make real HTTP requests. These tests will fail if the
      metrics port is already in use. The freeMetricsPort helper uses
      an incrementing counter starting at 19000 which is fragile across
      parallel test runs. Consider using the freePort helper pattern
      (net.Listen on :0) instead.

### report_test.go

- [ ] There is no test that verifies the per-path duration slice is
      sorted before percentile() is called on it. Add
      TestBuildReportData_PerPathPercentilesAreSorted that ingests
      visits in reverse duration order and verifies P50 and P99 are
      still correct.

### pool_test.go

- [ ] There is no test for the sitemap index recursion depth limit.
      If the recursion guard is added (see Priority 4), add a test
      that verifies a two-level nested sitemap index does not recurse
      infinitely.

### lemming_test.go

- [ ] TestRun_WaitingRoom_DurationRecorded has a t.Log at the end that
      says "test inconclusive" if the lemming was not admitted. This
      should be t.Skip not t.Log so the test is clearly marked as
      skipped rather than appearing to pass when it did not run the
      assertion.

### main_test.go

- [ ] testConfig() sets DashboardPort: 0 and MetricsPort: 0. Verify
      that passing port 0 to the dashboard and observer servers causes
      them to either not start or to bind to a random port. If port 0
      causes ListenAndServe to return an error immediately, tests that
      call NewSwarm with Observe: false will be fine but any test that
      sets Observe: true will fail. Add a note or guard in NewSwarm
      that skips observer initialisation when MetricsPort is 0.

---

## Priority 6 — Documentation and Consistency

Non-blocking issues that should be addressed before v0.0.1 tag.

### All source files

- [ ] Run gofmt -w . on all files after compile errors are fixed.
      Do not run gofmt before fixing compile errors — it will reformat
      broken code and make diffs harder to read.

- [ ] Run go vet ./... and address every warning. Pay particular
      attention to printf format string warnings which often indicate
      mismatched % arguments.

- [ ] Verify that every exported type and function has a godoc comment.
      The godoc standard established during generation was:
      1. What it is
      2. How to use it
      3. Gotchas and warnings if applicable
      Unexported functions should have shorter comments covering
      gotchas only.

### README.md

- [ ] The All Flags table currently shows -save-to with default "." and
      description "Local path or s3:// URI for report output". Update
      the description to reflect that it now accepts a comma-separated
      list of URIs including mailto: targets.

- [ ] The Dependencies table does not include net/smtp (standard library
      but worth noting) or mime/multipart (standard library). Consider
      adding a note that email delivery uses only stdlib — no external
      mail library dependency.

### TESTS.md

- [ ] Add a section covering target_test.go and observer_test.go since
      these files were added after TESTS.md was written. Specifically
      document what TestMailTarget_Deliver_LocalSMTP proves about email
      delivery reliability and what TestURLLabel_CardinalityCap proves
      about Prometheus cardinality safety.

### Makefile

- [ ] Add a make docs target that runs go doc ./... and pipes to a file.
      Useful for verifying godoc coverage before a release.

- [ ] Add a make check target that runs build + vet + fmt-check + test
      in sequence without the bench tag. This becomes the single command
      for CI pipelines to run.

- [ ] The current make run target hardcodes http://localhost:8080/ as
      the hit URL. Add a HIT variable so engineers can override it:

          HIT ?= http://localhost:8080/
          run:
              $(GO) run . -hit $(HIT) ...

---

## Priority 7 — Pre-Release Checklist

Do these last, after all above items are resolved.

- [ ] go build ./... — zero errors
- [ ] go vet ./... — zero warnings
- [ ] gofmt -l . — no files listed
- [ ] go test -race ./... — zero failures, zero data races
- [ ] go test -race -count=3 ./... — verify no flaky tests on repeat runs
- [ ] First local run produces a non-empty report file on disk
- [ ] First local run dashboard loads and displays live metrics
- [ ] Report HTML opens correctly in a browser with no external requests
- [ ] git tag v0.0.1 and push
- [ ] Verify pkg.go.dev picks up the module within 10 minutes of tag push
- [ ] Update README.md badges from placeholder to real passing/failing state

---

## Items Explicitly Out of Scope for v0.0.1

Do not open issues or PRs for these during the review. They are tracked
in README.md under v1.1.0 Roadmap.

- Database Navigator implementations (MySQL, PostgreSQL, MongoDB, SQLite)
- Fibonacci ramp shape (current linear ramp is correct for v0.0.1)
- S3 bucket auto-creation
- SMTP retry logic
- Prometheus remote write
- Per-lemming timing breakdown in reports
- Geographically distributed load via multiple instances
- The -local flag for forcing local copy alongside S3 upload
- gRPC target support
- Message queue target support (Kafka, SQS, RabbitMQ)

---

## How to Use This File

Work top to bottom. Fix Priority 1 items first — nothing else matters
until the package compiles. Then Priority 2, then Priority 3, and so on.

When an item is resolved, check the box and commit with a message that
references the item. For example:

    git commit -m "fix: smtp.NewClient error capture in target.go (TODO P1)"

When every box in Priority 1 through 5 is checked, run the Priority 7
checklist. When that passes, tag v0.0.1.

Do not delete this file after the review. Archive it as REVIEW_v0.0.1.md
so the decisions made during initial development are preserved for future
contributors who need to understand why certain choices were made.​​​​​​​​​​​​​​​​
