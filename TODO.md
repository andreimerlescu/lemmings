# Lemmings TODO

> Open work after 1.0.0. What 1.0 resolved is at the bottom; the full list of
> changes is in [CHANGELOG.md](CHANGELOG.md).

---

## Next — 1.1

- [ ] **Report comparison.** `lemmings compare old.json new.json` renders the
  difference between two JSON reports — rescued share, failure rate, p95 per
  path — and exits 2 on a regression beyond a threshold. The JSON report exists
  now; this makes it a regression gate across deploys.
- [ ] **`-dry-run`.** Index the target, print the plan and the URL pool, and
  exit without sending a lemming.
- [ ] **Unique report names.** An opt-in `-report-name` (or time in the
  filename) so two runs on the same day do not overwrite each other.
- [ ] **CI recipes.** GitHub Actions, GitLab CI and CircleCI examples that run
  a journey with gates and upload the HTML report as an artifact.
- [ ] **Monitor FuzzHandleAuth and FuzzDashboardHTML in CI.** Both start HTTP
  handlers; if either shows port contention or timeouts, give them a job of
  their own.

## Later

- [ ] Forms in journeys: `POST` steps that read a CSRF token from the previous
  page, so journeys can log in and check out.
- [ ] Fibonacci ramp shape as an alternative to the linear ramp.
- [ ] Prometheus remote write support.
- [ ] Geographically distributed load via coordinated lemmings instances,
  reporting into one dashboard.
- [ ] A companion project for real-browser checks (JavaScript errors, broken
  images, layout shift) that reads the same journey files. Kept out of this
  binary so lemmings stays a single Go executable with no Node or Chromium.
- [ ] Fuzz tests for ParseTarget (mailto: and s3:// URI parsers) and
  buildMessage (MIME construction with arbitrary inputs).
- [ ] Replace net/smtp with a context-aware SMTP library, and retry transient
  SMTP failures with backoff.
- [ ] S3 bucket creation behind an explicit `-s3-create-bucket` flag.
- [ ] Database Navigator implementations (MySQL, PostgreSQL, MongoDB, SQLite).
- [ ] Helm chart for running lemmings as a Kubernetes Job.

---

## Notes for Future Contributors

When reviewing the package, read in this dependency order:

1. events.go — the EventBus; fewest dependencies
2. pool.go — URL indexing and origin scoping
3. request.go, journey.go — one HTTP visit and the checks applied to it
4. lemming.go — the navigating agent
5. terrain.go — depends on lemming; owns all lifecycle events
6. stats.go, report.go, charts.go, templates/ — aggregates and reports
7. target.go — report delivery
8. swarm.go — depends on everything above
9. observer.go — Prometheus
10. observatory.go, dashboard.go, web/ — the live dashboard
11. cli.go, main.go — terminal output and wiring

Read each test file immediately after its source file. If a change touches
Terrain or Lemming, the lifecycle ownership contract (EventLemmingBorn /
EventLemmingDied / EventLemmingFailed emitted ONLY by the Terrain) is a hard
invariant — emitting them anywhere else breaks every counter-based subscriber
in the package.

The EventBus is synchronous: every subscriber runs on the lemming goroutine
that emitted. Subscribers must be O(1) and hand anything slow — disk, network
— to their own goroutine, as the trace writer does.

---

## Resolved in 1.0.0

- [x] Accept-Encoding consistency between indexing and lemmings: neither sets
  it, so Go decompresses gzip transparently for both.
- [x] UA-based content variation is documented: checksum changes are
  informational unless `-strict-checksum` is set.
- [x] Waiting room poll interval matches the room package's 3 seconds.
- [x] LEMMINGS_SAVE_TO is additive to -save-to, deduplicated, and tested.
- [x] SMTP overrides apply to every mailto: target.
- [x] Report delivery uses a fresh, bounded context after Ctrl-C.
- [x] MailTarget honours its context for dialing and I/O deadlines.
- [x] S3Target explains a missing region instead of a raw SDK error.
- [x] README flags, LEMMINGS_SAVE_TO section and dependency table are current.
- [x] TESTS.md covers report delivery, the fuzz targets and the lifecycle
  contract.
- [x] `make run`, `make run-journey`, `make demo` and `make build-check`.
- [x] CI runs on demand (workflow_dispatch): lint, then race tests on Linux
  or on Linux, macOS and Windows, with optional benchmarks and fuzzing.
- [x] Release gates: build, vet, gofmt, race tests, a real run with reports,
  the dashboard during a run, the live ticker, dropped_logs 0, and report paths
  printed at the end.
