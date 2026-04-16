# Lemmings TODO

> Outstanding items as of the review session that brought all 432 tests
> to passing. The pre-review "will not compile" and "will panic" sections
> have all been resolved — see the Resolved log at the bottom for history.
> Remaining items are grouped by priority.

---

## Priority 1 — Behavioural Verification for First Real-World Run

The test suite proves internal correctness. These items verify behaviour
that only surfaces against a real HTTP target. Address them during the
first localhost run before tagging v0.0.1.

### pool.go / lemming.go — Accept-Encoding consistency

- [ ] Verify the indexing HTTP client (pool.go fetchURL) and the lemming
  HTTP client (lemming.go request) send the same Accept-Encoding
  header. Go's http.Client auto-decompresses gzip only when it sent
  Accept-Encoding: gzip in the request. If the two clients disagree,
  every checksum will mismatch and every visit records Match: false.
  Confirm by running against a gzip-enabled target and checking the
  report's match rate.

### pool.go — UA-based content variation

- [ ] The indexing client uses userAgents[0] as a fixed UA. The lemming
  client uses randomUA(). If the target server returns different
  content per UA (A/B testing, bot detection), every lemming will
  see a mismatch. This is intentional — it surfaces real behaviour —
  but document it in the first-run notes so it is not confused
  with a bug.

### lemming.go — Waiting room poll interval

- [ ] waitingRoomPollInterval is hardcoded to 3 seconds to match the
  room package's client-side poll interval. If the target does not
  use room, this constant is irrelevant. If it does, verify the
  lemming and the room JS client poll at the same rate — a mismatch
  causes either over-polling (wasted connections) or under-polling
  (missed admission signals).

---

## Priority 2 — Correctness and Behaviour Gaps

These do not cause crashes or test failures but will produce misleading
results or poor user experience.

### main.go

- [ ] Verify the LEMMINGS_SAVE_TO environment variable appends to the
  list from -save-to rather than replacing it. The intended behaviour
  is additive — both flag and env var contribute destinations.

- [ ] Verify deduplication logic for saveTo correctly handles the case
  where the same destination appears in both the flag and the env var.

- [ ] Verify the boot summary correctly handles the case where SaveTo
  has only one entry — output should still use the arrow format:

          save-to:
            → .

      and not revert to a single-line format.

### swarm.go

- [ ] Verify applyFlagSMTPOverrides is called for every MailTarget in
  the targets list, not just the first one. If the user specifies
  two mailto: destinations, both need the SMTP overrides applied.

- [ ] Review Report(ctx) cancellation semantics. If the swarm context
  is already cancelled by the time Report is called (e.g. due to
  SIGINT), S3 uploads and SMTP sends should still complete. Consider
  wrapping the report delivery with a fresh 30-second timeout
  context rather than reusing the swarm's potentially-cancelled
  context.

### target.go

- [ ] Add a TODO comment to MailTarget.Deliver, sendTLS, sendSTARTTLS,
  and sendPlain noting that net/smtp does not natively support
  context cancellation. For v0.0.1 this is acceptable; v0.0.2 may
  replace net/smtp with a context-aware library.

- [ ] Verify S3Target.Deliver handles the case where both
  AWS_DEFAULT_REGION and AWS_REGION are unset and the SDK cannot
  detect the region automatically. The raw SDK error is not
  user-friendly — wrap it with a diagnostic hint pointing at
  the environment variables.

---

## Priority 3 — Documentation and Polish

### All source files

- [ ] Verify every exported type, function, and method has a godoc
  comment following the structure agreed during development:
  what it is, how to use it, gotchas and warnings.

- [ ] Verify every unexported function has at minimum a single-line
  comment describing what it does and any gotchas worth calling out.

### README.md

- [ ] The All Flags table has -save-to listed with the old single-string
  description. Update to reflect the comma-separated list format
  and give an example that shows all three target types.

- [ ] Add a section on the LEMMINGS_SAVE_TO environment variable and
  how it interacts with -save-to additively.

- [ ] The Dependencies table is missing github.com/prometheus/client_model
  which is used in observer_test.go for the DTO metric reading helpers.

### TESTS.md

- [ ] Add a section covering target_test.go and what it proves about
  report delivery reliability — specifically the partial failure
  guarantee that a failing S3 upload does not prevent local delivery.

- [ ] Add target.go to the fuzz targets table. The mailto: and s3://
  URI parsers are parsing externally-provided strings and should
  have fuzz coverage in v0.0.2.

- [ ] Document the Born/Died lifecycle event ownership contract:
  EventLemmingBorn, EventLemmingDied, and EventLemmingFailed are
  emitted exclusively by Terrain.spawnLemming, never by Lemming.Run.
  This was the root cause of the double-emit bug resolved during
  the review session.

### Makefile

- [ ] Add a make run-local target for quick iteration against localhost:

          run-local:
              $(GO) run . \
                  -hit http://localhost:8080/ \
                  -terrain 2 \
                  -pack 2 \
                  -until 10s \
                  -ramp 5s \
                  -tty=true \
                  -save-to /tmp/lemmings-test

- [ ] Add a make build-check target as the pre-commit gate:

          build-check: vet fmt-check
              $(GO) build ./...

### CI

- [ ] Align the Go version in .github/workflows/go.yml with go.mod.
  go.mod declares go 1.25.0; CI should pin 1.25.x on all three
  runners (ubuntu-latest, macos-latest, windows-latest).

- [ ] Monitor FuzzHandleAuth and FuzzDashboardHTML in CI. Both bind
  to ports and spin up HTTP servers. If either shows flakiness
  from port contention or timeouts, move to a dedicated job with
  a longer fuzztime or serialise them on a single runner.

---

## Priority 4 — v0.0.1 Release Gates

The following must all be true before tagging v0.0.1. Check off as
each is verified on the release candidate.

- [ ] go build ./... exits 0 with no output
- [ ] go vet ./... exits 0 with no output
- [ ] gofmt -l . returns no filenames
- [x] go test -race ./... exits 0 with all tests passing (432/432)
- [ ] make test exits 0
- [ ] make lint exits 0
- [ ] A real run against localhost produces a non-empty .md and .html
  report
- [ ] The HTML report opens in a browser without errors
- [ ] The dashboard is accessible at localhost:4000 during a real run
- [ ] The live ticker updates every second during a real run
- [ ] At least one URL in the report shows Match: true
- [ ] dropped_logs is 0 in the first real run
- [ ] The report file path is printed to STDOUT at the end of the run

---

## Deferred to v0.0.2 and Beyond

Do not attempt to address these during the v0.0.1 cycle. They are
recorded here so they are not lost.

- [ ] Fuzz tests for ParseTarget (mailto: and s3:// URI parsers)
- [ ] Fuzz tests for buildMessage (MIME construction with arbitrary inputs)
- [ ] Replace net/smtp with a context-aware SMTP library
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
- [ ] -dry-run flag that indexes the URL pool and prints the plan
  without spawning any lemmings
- [ ] CI integration examples for GitLab CI and CircleCI
- [ ] Helm chart for running lemmings as a Kubernetes Job

---

## Resolved During Review Session

Kept as a historical record. These items were outstanding at the start
of the review and have since been addressed.

### Compile errors

- [x] target.go — smtp.NewClient error return captured in sendTLS
- [x] target.go — error returns from smtp.Client methods captured
  and wrapped in sendSTARTTLS and sendPlain
- [x] target.go — net/smtp, crypto/tls, net, mime/multipart,
  mime/quotedprintable, net/textproto imports all present
- [x] pool_test.go — import_srv renamed to importSrv
- [x] pool_test.go — unused innerSrv removed
- [x] pool_test.go — net/http/httptest imported
- [x] observer_test.go — bus declaration added to
  TestPrometheusObserver_Attach_StartsServer
- [x] report_test.go — net/http and fmt imports present
- [x] dashboard_test.go — net imported
- [x] lemming_test.go — math/rand imported

### Runtime bugs

- [x] lemming.go — math/rand and crypto/rand aliased correctly
- [x] lemming.go — EventWaitingRoom emit moved to after waitInRoom
  returns so Duration is populated
- [x] lemming.go — EventVisitComplete emit carries BytesIn and Duration
- [x] lemming.go — EventLemmingBorn emit removed from Run; lifecycle
  events are now exclusively the responsibility of Terrain
  (root cause of double-counted alive gauge)
- [x] swarm.go — SwarmMetrics passed and stored as *SwarmMetrics
  consistently; atomic fields never copied
- [x] terrain.go — spawnLemming has exactly one wg.Done path via defer
- [x] terrain.go — spawnLemming acquire error handling corrected; Release
  is only deferred after Acquire succeeds, preventing semaphore
  corruption from ghost releases
- [x] observer.go — alive metric changed from GaugeVec to plain Gauge
- [x] dashboard.go — %% escape verified in the JS section of dashboardHTML

### Correctness gaps

- [x] report.go — resolveSavePath removed; logic lives in LocalTarget.resolveDir
- [x] report.go — Write(ctx context.Context) is the only Write signature
- [x] report.go — buildFilename produces lemmings.YYYY.MM.DD.domain
  with colon-to-dash replacement for localhost:PORT
- [x] main.go — figs.List("save-to") dereferences correctly

### Test coverage

- [x] observer_test.go — swarm integration tests moved to swarm_test.go;
  mockObserver moved with them
- [x] swarm_test.go — makeMinimalSwarm initialises observers correctly
  (zero value is valid; ranging over nil slice is safe)
- [x] target_test.go — acceptOneSMTPMessage handles EHLO/HELO correctly
- [x] main_test.go — testConfig() SaveTo value verified not to cause
  accidental file I/O in tests

---

## Notes for Future Contributors

When reviewing the package, read in this dependency order:

1. REVIEW.md — high-level context
2. events.go — fewest dependencies
3. pool.go — URL indexing
4. lemming.go — the navigating agent
5. terrain.go — depends on lemming; owns all lifecycle events
6. swarm.go — depends on everything above
7. report.go + target.go — tightly coupled
8. observer.go — depends on events and swarm
9. dashboard.go — depends on events and swarm
10. main.go — wiring only

Read each test file immediately after its source file. If a change
touches Terrain or Lemming, the lifecycle ownership contract
(EventLemmingBorn / EventLemmingDied / EventLemmingFailed emitted
ONLY by Terrain) is a hard invariant — duplicating emission from
Lemming.Run breaks every counter-based subscriber in the package.