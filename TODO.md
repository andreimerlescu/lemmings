# Lemmings TODO

> Outstanding items identified during the initial development session.
> Work through this list during code review before tagging v0.0.1.
> Items are grouped by priority and file. Check off each item as it is resolved.

---

## Priority 1 — Will Not Compile

These are confirmed or near-confirmed compile errors that must be fixed
before go build ./... can pass. Start here.

### target.go

- [ ] smtp.NewClient returns (*smtp.Client, error) but the error return
      is not captured in sendTLS. Current code:

          client, err := smtp.NewClient(conn, host)

      Must become:

          client, err := smtp.NewClient(conn, host)
          if err != nil {
              return fmt.Errorf("smtp new client: %w", err)
          }
          return t.sendViaSMTPConn(client, msg)

- [ ] Verify all error returns from smtp.Client methods in sendSTARTTLS
      and sendPlain are captured and wrapped correctly.

- [ ] Verify that the net/smtp, crypto/tls, net, mime/multipart,
      mime/quotedprintable, and net/textproto imports are all present
      in target.go. These are easy to miss during generation.

### pool_test.go

- [ ] TestIndexSitemap_Index uses import_srv as a variable name.
      Hyphens are not valid in Go identifiers. Rename to importSrv
      or testSrv throughout the test function.

- [ ] The same test declares innerSrv as an *http.Server that is
      never started and serves no purpose. Remove it entirely.

- [ ] Verify net/http/httptest is in the pool_test.go import block —
      the file uses httptest.NewServer directly in TestIndexSitemap_Index
      rather than going through the testServer builder.

### observer_test.go

- [ ] TestPrometheusObserver_Attach_StartsServer references bus without
      declaring it. Add:

          bus := NewEventBus()

      before the Attach call. Scan the entire file for any other function
      that references bus without a declaration in scope.

### report_test.go

- [ ] Verify net/http is in the import block. The file uses http.StatusOK
      in makeLifeLog.

- [ ] Verify fmt is in the import block. The file uses fmt.Errorf in
      TestReporter_Ingest_RecordsErrors and fmt.Sprintf in multiple helpers.

### dashboard_test.go

- [ ] Verify net is in the import block. freePort uses net.Listen and
      net.TCPAddr directly.

### lemming_test.go

- [ ] newRNG() returns *rand.Rand. Verify math/rand is explicitly imported
      in lemming_test.go. The file may only pull in crypto-related packages
      transitively and miss math/rand.

---

## Priority 2 — Will Compile But Will Panic or Produce Wrong Results

These issues will not be caught by the compiler but will cause runtime
failures, data corruption, or silently wrong output.

### lemming.go

- [ ] Verify the import block correctly aliases math/rand and crypto/rand
      to avoid collision. Suggested aliases:

          import (
              crand "crypto/rand"
              mrand "math/rand"
          )

      Then verify every usage of rand.New, rand.NewSource in lemming.go
      and every usage of rand.Read in swarm.go uses the correct alias.

- [ ] Verify that the EventWaitingRoom emit has been moved to after
      waitInRoom returns, not before. The emit must include the Duration
      field from visit.WaitingRoom.Duration. If the emit is still at the
      top of the waiting room detection block, Duration will always be zero.

- [ ] Verify that EventVisitComplete emit includes both BytesIn and
      Duration fields populated from the visit struct. If either is missing,
      the Prometheus observer will record zero for all visit durations
      and zero for all byte counts.

### swarm.go

- [ ] SwarmMetrics contains atomic.Int64 fields. Atomic types must not
      be copied. Verify that SwarmMetrics is always passed and stored as
      a pointer (*SwarmMetrics) and never as a value. Check every struct
      that embeds or stores SwarmMetrics and every function that receives it.

- [ ] The Swarm struct currently has:

          metrics SwarmMetrics

      This should be:

          metrics *SwarmMetrics

      And the construction should be:

          metrics: &SwarmMetrics{}

      Verify this is consistent across swarm.go, terrain.go, dashboard.go,
      and observer.go.

### terrain.go

- [ ] Verify spawnLemming has exactly one wg.Done path via defer at the
      top of the function. There must be zero explicit wg.Done() calls
      anywhere in the function body. A double-Done will panic at runtime
      and is easy to miss in code review because it only fires on the
      error paths.

### observer.go

- [ ] The alive metric is declared as *prometheus.GaugeVec but accessed
      with empty labels:

          o.alive.With(prometheus.Labels{}).Inc()

      A GaugeVec with no label dimensions should be a plain *prometheus.Gauge.
      Change the declaration to:

          o.alive prometheus.Gauge

      And the construction to:

          o.alive = prometheus.NewGauge(prometheus.GaugeOpts{
              Name: "lemmings_alive",
              Help: "Number of lemmings currently running.",
          })

      And all usages to:

          o.alive.Inc()
          o.alive.Dec()

      Update observer_test.go gaugeValue calls accordingly — gaugeValue
      currently takes a prometheus.Gauge, so the test helper may already
      be correct if the declaration is fixed.

### dashboard.go

- [ ] Verify every % character in the JavaScript section of dashboardHTML
      is written as %% to survive the fmt.Sprintf call. A single % followed
      by any letter will produce a runtime formatting artifact in the HTML
      silently. The modulo operators in the elapsed() JS function are the
      most likely culprits:

          const h = Math.floor(secs / 3600);
          const m = Math.floor((secs %% 3600) / 60);
          const s = secs %% 60;

      Verify both %% are present and not accidentally corrected to % by
      an editor or linter.

### pool.go

- [ ] Verify the indexing HTTP client and the lemming HTTP client both
      send the same Accept-Encoding header. If the indexing client allows
      gzip but a lemming client does not (or vice versa), the server may
      return different byte sequences for the same URL and every checksum
      will fail to match. Go's http.Client handles gzip decompression
      transparently when the server sends Content-Encoding: gzip, but only
      if the client sent Accept-Encoding: gzip in the request. Verify
      consistency between fetchURL in pool.go and request in lemming.go.

---

## Priority 3 — Correctness and Behaviour Gaps

These will not cause crashes but will produce incorrect or misleading
results in the report or dashboard.

### report.go

- [ ] Verify resolveSavePath has been completely removed. Nothing should
      still call it. The function's logic now lives in LocalTarget.resolveDir
      and any remaining call to resolveSavePath will be a compile error —
      but verify the function body itself is gone to avoid confusion.

- [ ] Verify Write(ctx context.Context) is the only Write signature in
      report.go. The old Write(saveTo string) must not exist anywhere in
      the file.

- [ ] Verify buildFilename produces the correct format. The expected output
      is lemmings.YYYY.MM.DD.domain — four dot-separated components. Test
      this manually with a localhost:8080 hit URL to confirm the colon
      replacement in domainFromURL produces localhost-8080 and not
      localhost:8080 which would be an invalid filename on some systems.

### main.go

- [ ] Verify figs.List("save-to") dereferences correctly. figtree may
      return *[]string or []string depending on the version. Confirm the
      return type against the figtree source before dereferencing.

- [ ] Verify the LEMMINGS_SAVE_TO environment variable appends to the
      list from -save-to rather than replacing it. The intended behaviour
      is additive — both the flag and the env var contribute destinations.

- [ ] Verify the deduplication logic for saveTo correctly handles the case
      where the same destination appears in both the flag and the env var.

- [ ] Verify the boot summary correctly handles the case where SaveTo has
      only one entry — the output should still use the arrow format:

          save-to:
            → .

      And not revert to the old single-line format.

### swarm.go

- [ ] Verify applyFlagSMTPOverrides is called for every MailTarget in the
      targets list, not just the first one. If the user specifies two mailto:
      destinations, both need the SMTP overrides applied.

- [ ] Verify that Report(ctx) passes the swarm's context and not
      context.Background(). If the swarm context is already cancelled by
      the time Report is called (due to signal handling), S3 uploads and
      SMTP sends should still complete using a fresh context with a timeout.
      Consider using context.WithTimeout(context.Background(), 30*time.Second)
      for the report delivery context rather than the swarm's potentially
      cancelled context.

### target.go

- [ ] Verify that MailTarget.Deliver passes the context to sendTLS,
      sendSTARTTLS, and sendPlain. Currently these methods do not accept
      a context parameter. For v0.0.1 this is acceptable since net/smtp
      does not natively support context cancellation, but add a TODO comment
      noting this gap.

- [ ] Verify that S3Target.Deliver correctly handles the case where both
      AWS_DEFAULT_REGION and AWS_REGION are unset and the SDK cannot detect
      the region automatically. The error message from the SDK in this case
      is not user-friendly — consider wrapping it with a diagnostic hint.

---

## Priority 4 — Test Coverage Gaps

These are gaps identified in the test suite that should be addressed before
the test suite can be considered complete.

### observer_test.go

- [ ] The four swarm integration tests (TestSwarm_RegisterObserver_*  and
      TestSwarm_Run_DetachesObservers) were written in observer_test.go but
      belong in swarm_test.go. Move them and remove the duplicates. The
      mockObserver struct must move with them.

- [ ] TestPrometheusObserver_Attach_StartsServer is missing a bus declaration.
      After fixing the compile error, verify the test actually exercises the
      server starting — not just that Attach returns nil.

### swarm_test.go

- [ ] After moving the observer integration tests from observer_test.go,
      verify makeMinimalSwarm initialises the observers field correctly.
      The zero value (nil slice) is valid for []Observer but verify the
      deferred detach loop in Run handles a nil observers slice without
      panicking:

          for _, o := range s.observers {

      ranging over a nil slice is safe in Go — this is fine. But document
      it explicitly with a comment so future contributors do not add a
      nil check unnecessarily.

### target_test.go

- [ ] TestMailTarget_Deliver_LocalSMTP uses acceptOneSMTPMessage which
      implements a bare-minimum SMTP state machine. Verify it handles the
      EHLO/HELO negotiation correctly for Go's net/smtp client, which sends
      EHLO first and falls back to HELO. The current implementation sends
      250 OK for any unrecognised command which should work, but verify
      against an actual net/smtp dial.

- [ ] Add a test that verifies ParseTarget correctly handles a comma in
      the destination string — this would indicate the user passed a full
      -save-to list as a single string rather than using the list flag.
      ParseTarget should treat the whole string as one URI and return a
      LocalTarget, not split on the comma. The splitting happens at the
      figtree level, not at ParseTarget.

### main_test.go

- [ ] testConfig() was updated with SaveTo: []string{"."} but verify this
      does not cause any existing test that passes cfg to NewSwarm to
      attempt creating a LocalTarget and writing files during the test.
      NewSwarm now calls ParseTarget for each SaveTo entry — tests that
      do not want file I/O should either use an empty SaveTo list or a
      temp directory.

---

## Priority 5 — Documentation and Polish

Address these after the code compiles and tests pass.

### All source files

- [ ] Verify every exported type, function, and method has a godoc comment
      following the structure agreed during development:
      what it is, how to use it, gotchas and warnings.

- [ ] Verify every unexported function has at minimum a single-line comment
      describing what it does and any gotchas worth calling out.

### README.md

- [ ] The All Flags table has -save-to listed with the old single-string
      description. Update to reflect the comma-separated list format and
      give an example that shows all three target types.

- [ ] Add a section on the LEMMINGS_SAVE_TO environment variable and how
      it interacts with -save-to additively.

- [ ] The Dependencies table is missing github.com/prometheus/client_model
      which is used in observer_test.go for the DTO metric reading helpers.

### TESTS.md

- [ ] Add a section covering target_test.go and what it proves about
      report delivery reliability — specifically the partial failure
      guarantee that a failing S3 upload does not prevent local delivery.

- [ ] Add target.go to the fuzz targets table. The mailto: URI parser
      and the s3:// URI parser are parsing externally-provided strings
      and should have fuzz coverage in v0.0.2.

### Makefile

- [ ] Add a make run-local target that runs lemmings against localhost:8080
      with the smallest possible parameters for quick iteration:

          run-local:
              $(GO) run . \
                  -hit http://localhost:8080/ \
                  -terrain 2 \
                  -pack 2 \
                  -until 10s \
                  -ramp 5s \
                  -tty=true \
                  -save-to /tmp/lemmings-test

- [ ] Add a make build-check target that runs go build, go vet, and
      gofmt -l in sequence and exits 1 if any fails. This is the
      pre-commit gate:

          build-check: vet fmt-check
              $(GO) build ./...

---

## Priority 6 — v0.0.1 Release Gates

The following must all be true before tagging v0.0.1. This is the
definition of done for the initial release.

- [ ] go build ./... exits 0 with no output
- [ ] go vet ./... exits 0 with no output
- [ ] gofmt -l . returns no filenames
- [ ] go test -race ./... exits 0 with all tests passing
- [ ] make test exits 0
- [ ] make lint exits 0
- [ ] A real run against localhost produces a non-empty .md and .html report
- [ ] The HTML report opens in a browser without errors
- [ ] The dashboard is accessible at localhost:4000 during a real run
- [ ] The live ticker updates every second during a real run
- [ ] At least one URL in the report shows Match: true
- [ ] dropped_logs is 0 in the first real run
- [ ] The report file path is printed to STDOUT at the end of the run

---

## Deferred to v0.0.2 and Beyond

Do not attempt to address these during the v0.0.1 review. They are recorded
here so they are not lost.

- [ ] Fuzz tests for ParseTarget (mailto: and s3:// URI parsers)
- [ ] Fuzz tests for buildMessage (MIME construction with arbitrary inputs)
- [ ] S3 bucket creation if bucket does not exist (with -s3-create-bucket flag)
- [ ] SMTP retry logic with exponential backoff on transient failures
- [ ] -local flag to force local copy alongside S3 or email delivery
- [ ] Fibonacci ramp shape as an alternative to linear ramp
- [ ] Per-lemming timing breakdown in the HTML report
- [ ] Time series graph in HTML report showing visit rate over the run duration
- [ ] Prometheus remote write support
- [ ] Database Navigator implementations (MySQL, PostgreSQL, MongoDB, SQLite)
- [ ] Geographically distributed load via coordinated lemmings instances
- [ ] Report comparison mode — diff two report files and highlight regressions
- [ ] -dry-run flag that indexes the URL pool and prints the plan without
      spawning any lemmings
- [ ] CI integration examples (GitHub Actions, GitLab CI, CircleCI)
- [ ] Helm chart for running lemmings as a Kubernetes Job

---

## Notes for the Reviewer

When you open a new chat thread tomorrow, paste REVIEW.md first so the
reviewer has full context before seeing any code. Then paste one source
file at a time in dependency order. Do not paste all files at once.

The most productive review sequence is:

1. Paste REVIEW.md — establish context
2. Paste events.go — simplest file, fewest dependencies
3. Paste pool.go — URL indexing, no swarm dependencies
4. Paste lemming.go — the heart of the package
5. Paste terrain.go — depends on lemming
6. Paste swarm.go — depends on everything
7. Paste report.go + target.go together — they are coupled
8. Paste observer.go — depends on events and swarm
9. Paste dashboard.go — depends on events and swarm
10. Paste main.go last — the wiring layer
11. Paste each test file immediately after its source file

Fix compile errors before moving to the next file. A clean compile is
the gate to every other kind of review.
