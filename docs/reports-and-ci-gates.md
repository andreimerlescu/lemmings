# Reports, Delivery & CI Gates

Every Lemmings run produces a complete multi-format report bundle containing aggregate latency distributions, per-path breakdowns, timeline charts, waiting-room statistics, notable lemmings, and all redacted **Error Forensics artifacts** (including screenshots and admin triage verdicts).

![Lemmings HTML Report](images/report.jpg)

---

## Output Formats

For every target directory or S3 prefix, Lemmings writes:

| File | Description |
| --- | --- |
| `<name>.html` | Self-contained Synthwave '84 dark/light interactive report with inline SVG traffic/latency charts, separated Page vs. Asset class tables, enriched Waiting Room / Concert pass & ban metrics, Browser Scout totals, notable lemmings, prominent `DATA DROPPED` integrity warnings when backpressure drops occur, and inline base64 screenshots (capped at `12 MiB` for Local/S3; `2 MiB` for SMTP email). |
| `<name>.md` | GitHub-friendly Markdown report with ASCII sparklines, `mdCell`-escaped tables, adaptive `mdFence`-escaped error body excerpts, extracted error details, JS console summaries, and relative image links (`./<name>.artifacts/<id>.jpg`) so screenshots render directly in PRs and repositories. |
| `<name>.json` | Machine-readable report (`schema_version: "2.0"`) for CI pipelines, archival, and `lemmings compare`. Contains exact counters (`lemmings_started`, `lemmings_completed`, `lemmings_not_started`), high-precision percentiles (`exactGlobalSamples = 8192`, `exactPathSamples = 1024`, then `<= 0.5%` 4096-bin `uint64` histogram), `assets`, `waiting_room`, `concert`, `scouts`, gates, and forensics metadata (`image_path`, `sha256`, triage verdicts, console entries, extracted errors) with **zero base64 blobs** and **zero secrets or raw cookie values**. |
| `<name>.artifacts/<id>.jpg` | Standalone compressed screenshot images written beside Local and S3 reports. |

### Report Naming

By default, reports are named `lemmings.YYYY.MM.DD.<domain>` under `<save-to>/lemmings/<domain>/`.

- **`-unique-report` (`-ur`):** Uses UTC timestamp `lemmings.YYYY.MM.DD.HHMMSSZ.<domain>` so multiple runs on the same day never overwrite each other.
- **`-report-name <slug>` (`-rn`):** Sets an explicit filename slug matching `[a-zA-Z0-9._-]+` (no `..` or path separators), e.g., `-report-name release-2.4.0-rc1`.

### Streaming Per-Visit Trace (`-trace-file`)

Pass `-trace-file /path/to/visits.jsonl` to stream every individual visit (pages, `-assets` fetches, and browser Scout navigations) as newline-delimited JSON while the swarm runs:
- Created with `O_CREATE|O_EXCL` and permissions **`0600`** — Lemmings never overwrites an existing trace file.
- Records `kind` (`"page"`, `"asset"`, `"scout"`), `asset_class`, `queue_polls`, `queue_admitted`, `concert_admit`, `room_ticket`, and `banned` alongside status, latency, and check failure metadata.
- Every URL in the trace is passed through `discovery.SafeURL` (stripping credentials, query strings, and fragments).
- Any dropped trace entries (`trace_dropped > 0`) trigger a prominent `DATA DROPPED` integrity warning in the CLI summary and reports.

---

## Delivery Targets (`-save-to` / `LEMMINGS_SAVE_TO`)

Specify one or more comma-separated destinations via `-save-to` (and/or `LEMMINGS_SAVE_TO`):

### 1. Local Filesystem (`./reports`, `/var/reports`, `file:///tmp/reports`)
Writes `.md`, `.html`, `.json`, and `<name>.artifacts/` inside `<path>/lemmings/<domain>/` using atomic temp-file + `Sync` + `Close` + `Rename` writes confined inside Go's `os.OpenRoot` (`0750` dirs, `0640` files) with strict `validateSafeComponent` checks so partial writes, path traversal, or symlink escapes are impossible.

### 2. Amazon S3 (`s3://bucket/prefix`)
Validates `Domain`, `Filename`, and every artifact filename with `validateSafeComponent` before uploading `.md`, `.html`, `.json`, and all `<name>.artifacts/<id>.<ext>` objects using the standard AWS SDK v2 credential chain (`AWS_PROFILE`, IAM role, `AWS_REGION` / `AWS_DEFAULT_REGION`).

```bash
AWS_REGION=us-east-1 lemmings -hit https://staging.example.com/ \
  -save-to ./reports,s3://ops-load-reports/staging
```

### 3. Hardened SMTP Email (`mailto:ops@example.com?cc=dev@example.com&subject=Nightly%20Run`)
Sends a MIME `multipart/mixed` email with the `2 MiB`-bounded inline HTML report plus the Markdown report attached.

> **Security Note (v2.0):** Lemmings never accepts an SMTP password as a CLI flag so credentials never appear in `ps aux` or shell history. Provide SMTP credentials via `-smtp-pass-file` (must be `0600`), `-smtp-pass-stdin`, `LEMMINGS_SMTP_PASS_FILE`, or `LEMMINGS_SMTP_PASS`.

```bash
chmod 600 ~/.config/lemmings/smtp.pass
lemmings -hit https://staging.example.com/ \
  -save-to "mailto:ops@example.com?subject=Release%20Check" \
  -smtp-host smtp.example.com \
  -smtp-port 587 \
  -smtp-user mailer@example.com \
  -smtp-from lemmings@example.com \
  -smtp-pass-file ~/.config/lemmings/smtp.pass \
  -smtp-require-tls
```

- Port `465` uses implicit TLS (`MinVersion: TLS 1.2`); other ports negotiate `STARTTLS` (`MinVersion: TLS 1.2`) and **never** downgrade to plaintext on a failed handshake.
- Remote SMTP hosts require TLS by default; pass `-smtp-require-tls` to enforce TLS even on loopback test relays.

---

## CI Gates

Fail a build automatically (exit code `2`) if reliability or tail latency breaches your SLO:

```bash
lemmings -hit https://staging.example.com/ \
  -terrain 10 -pack 20 -limit 100 -until 60s -ramp 15s \
  -journey journeys/checkout.json \
  -max-failure-rate 0.01 \
  -p95-budget 750ms \
  -tty=false \
  -unique-report \
  -save-to ./artifacts
```

| Gate Flag | Default | Condition to Pass |
| --- | --- | --- |
| `-max-failure-rate` (`-mfr`) | `-1` *(disabled)* | Observed `failed_visits / (total_visits - cancelled_visits) <= value` (`0..1`). |
| `-p95-budget` (`-p95`) | `0` *(disabled)* | Observed global `p95` **page** visit latency `<= duration` (excludes waiting-room wait and `-assets` fetches). |

---

## Regression Comparison (`lemmings compare`)

Compare a baseline JSON report against a candidate release JSON report to catch latency regressions, rescued-rate drops, and newly introduced error signatures:

```bash
lemmings compare \
  -max-failure-delta 0.01 \
  -max-p95-delta 100ms \
  -max-rescued-drop 2.0 \
  reports/baseline.json reports/candidate.json
```

### `compare` Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-max-failure-delta` | `-1` *(disabled)* | Exit `2` if `candidate.failure_rate - baseline.failure_rate` exceeds this share (`0..1`). |
| `-max-p95-delta` | `0` *(disabled)* | Exit `2` if `candidate.p95 - baseline.p95` exceeds this duration (e.g. `100ms`). |
| `-max-rescued-drop` | `-1` *(disabled)* | Exit `2` if `baseline.rescued_percent - candidate.rescued_percent` exceeds this many percentage points. |
| `-json` | `false` | Emit structured JSON diff to stdout instead of a terminal table. |
| `-tty` / `-color` | `true` | Control terminal formatting and colors. |

Output includes:
- Overall deltas for `rescued %`, `failure rate`, `p50`, `p95`, `p99`, `asset_p95_delta_ms`, and `queue_held_delta` (backward-compatible with older JSON reports).
- **New error signatures (`+`)** and **Resolved error signatures (`-`)** diffed by forensic `Fingerprint`.
- Top 10 paths sorted by p95 latency regression.

---

## CI Pipeline Recipes

### GitHub Actions (with Sidecar Chromium & Artifact Upload)

```yaml
name: Staging Load & Forensics Check
on:
  workflow_dispatch:
jobs:
  lemmings:
    runs-on: ubuntu-latest
    services:
      chrome:
        image: chromedp/headless-shell:latest
        ports:
          - 9222:9222
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26.x'
      - name: Install Lemmings
        run: go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest
      - name: Run swarm with live CDP forensics
        run: |
          lemmings \
            -hit https://staging.example.com/ \
            -cdp-url http://127.0.0.1:9222 \
            -terrain 6 -pack 15 -limit 60 -until 45s -ramp 10s \
            -scouts 2 \
            -max-failure-rate 0.02 \
            -p95-budget 800ms \
            -tty=false \
            -report-name ci-run \
            -save-to ./lemmings-out
      - name: Upload HTML, Markdown, JSON & Screenshots
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: lemmings-forensics-report
          path: ./lemmings-out
```

### GitLab CI (with Regression Compare)

```yaml
load-test:
  image: golang:1.26
  script:
    - go install github.com/andreimerlescu/lemmings/v2/cmd/lemmings@latest
    - lemmings -hit "$STAGING_URL" -terrain 5 -pack 10 -until 30s -ramp 5s
        -max-failure-rate 0.02 -p95-budget 750ms -tty=false
        -report-name candidate -save-to ./reports
    - |
      if [ -f baseline/candidate.json ]; then
        lemmings compare -max-p95-delta 150ms -max-failure-delta 0.01 \
          baseline/candidate.json reports/lemmings/*/candidate.json
      fi
  artifacts:
    when: always
    paths:
      - reports/
```
