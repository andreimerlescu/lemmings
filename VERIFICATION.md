# Verification of this source bundle

Verified on September 26, 2026 using Go 1.26.1, Node, Playwright 1.62.1 and Chromium
151.0.7922.34 (headless shell). The supplied Go module and application VERSION are
retained. No production target, external email recipient or S3 bucket was used.

## Executed checks

- `make all`: `go vet`, formatting, `go test -race ./...`, and a successful build.
- Real Chromium regression test: healthy page, stable user agent, persistent
  cookie, blank page, uncaught JavaScript error, missing image, resource status
  counts, screenshot creation and JSONL trace integrity.
- `python3 scripts/smoke.py --browser`: actual executable startup/flags, complete
  HTTP and browser journeys, exact result counts, report delivery, CI exit codes,
  and cancellation with report preservation.
- Rendered the resulting HTML report in Chromium at 1440px and 390px widths.
  Confirmed the failure summary, expandable evidence and no horizontal overflow.

The smoke test produced these deterministic journey results:

| Scenario | HTTP visits | Failed HTTP visits | Browser visits | Failed browser visits | Exit |
|---|---:|---:|---:|---:|---:|
| Healthy browsing | 18 | 0 | 6 | 0 | 0 |
| Deliberately broken pages | 24 | 18 | 8 | 6 | 2 |
| Interrupted workload | 16 | not an error-rate benchmark | disabled | — | 130 |

The interruption test saved its report and HTTP trace before exiting. Its visit
count is timing-dependent; 16 is the count from this recorded run.

`examples/sample-results/` includes the actual reports and traces from these
local fixtures, with artifact paths made relative for portability. Broken-page
browser evidence includes screenshots. These are correctness fixtures, not
production throughput benchmarks.

## Reproduce

```sh
make all
make browser-install
make browser-test
python3 scripts/smoke.py --browser --output ./smoke-results
```

`--output` must refer to a fresh destination on repeated smoke runs because HTTP
trace files intentionally refuse to overwrite existing evidence. Without
`--output`, the smoke test uses and cleans up a temporary directory.

Live S3/SMTP delivery was not exercised. Their existing implementations and tests
are retained; JSON delivery is additionally implemented for local and S3 targets.
For the exact guarantees and remaining limits, read [UPGRADE.md](UPGRADE.md).
