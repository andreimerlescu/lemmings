# CLI Reference

```text
lemmings [flags]
lemmings compare [flags] <baseline.json> <candidate.json>
```

---

## Core Swarm & Target Flags

| Flag | Alias | Default | Range / Values | Description |
| --- | --- | --- | --- | --- |
| `-hit` | `-h` | `http://localhost:8080/` | `http://` or `https://` | Target origin URL. All crawls, links, redirects, and captures stay strictly on this origin. |
| `-terrain` | `-t` | `50` | `1..10000` | Number of terrain goroutine scheduler groups. |
| `-pack` | `-p` | `50` | `0..100000` | Number of lemmings per terrain (`total = terrain × pack`). |
| `-limit` | `-l` | `100` | `-1` or `1..1000000` | Semaphore ceiling for concurrent lemmings (`-1` disables cap). |
| `-until` | `-u` | `30s` | `> 0` and `< 24h` | Lifespan of each individual lemming. |
| `-ramp` | `-r` | `5m` | `>= 0` and `< 24h` | Duration over which terrains come online. |
| `-ramp-shape` | `-rs` | `linear` | `linear`, `fibonacci`, `step` | Terrain launch curve across `-ramp`. |
| `-crawl` | `-c` | `false` | `bool` | Crawl origin HTML links when no sitemap is available. |
| `-crawl-depth` | `-cd` | `3` | `1..100` | Maximum link depth when `-crawl` is enabled. |
| `-dry-run` | — | `false` | `bool` | Validate config (including YAML `-plan` schema and `${ENV_VAR}` expansion), probe browser, index target URL pool, print execution summary, and exit `0` without sending load. |

---

## Visitor Behaviour, Plans & Bounds

| Flag | Alias | Default | Range / Values | Description |
| --- | --- | --- | --- | --- |
| `-navigation` | `-nav` | `links` | `links`, `random` | Follow links on each visited page (`links`) or sample the indexed pool (`random`). Also controls how rogue lemmings roam when `-plan` is active. |
| `-plan` | `-pl` | `""` | file, dir, or comma-separated list | YAML plan file, directory of `*.yaml`/`*.yml` files (loaded lexicographically), or comma-separated list of files/directories (up to 100 unique plans per suite). |
| `-max-plan-items` | `-mpi` | `999` | `1..1000000` | Maximum `PlanItem` steps per plan (exits `1` if exceeded). Loader file read cap scales up to `64 MiB`. |
| `-rogue-rate` | `-rr` | `-1` *(auto)* | `-1` or `0..1` | Fraction of lemmings assigned to wander sitemap/crawled links (`-navigation` `links`/`random`) instead of following a plan. Auto (`-1`) resolves to `0.05` when `-plan` is set and `0` otherwise. Deterministic by global lemming number (`floor(n*rate+1e-9) > floor((n-1)*rate+1e-9)`). |
| `-think-min` | `-tmin` | `300ms` | `>= 0` | Minimum pause between pages (`0` with `-think-max 0` saturates). |
| `-think-max` | `-tmax` | `1200ms` | `>= -think-min` | Maximum pause between pages. |
| `-max-pages` | `-mpg` | `0` | `0..1000000` | Maximum pages visited per lemming (`0` browses until `-until`). |
| `-request-timeout` | `-rt` | `10s` | `> 0` | Per-request timeout including redirects and body read. |
| `-max-body-bytes` | `-mbb` | `2097152` *(2 MiB)* | `1..67108864` *(64 MiB)* | Maximum decoded response body read per visit. |
| `-strict-checksum` | `-sc` | `false` | `bool` | Fail visits whose body SHA-512 differs from the baseline recorded at index time. |
| `-assets` | — | `false` | `bool` | Fetch up to `-assets-per-page` uncached same-origin static assets (CSS, JS, images, fonts, media) per HTML page visit using the lemming's cookie jar (e.g. `concert_admit`). Tracked separately from page visits and `-p95-budget`. |
| `-assets-per-page` | — | `8` | `1..64` *(when `-assets` enabled)* | Maximum uncached same-origin static assets fetched per HTML page visit (including CSS sub-assets). |

---

## Error Forensics, Screenshots, Scouts & Admin Portal

| Flag | Alias | Default | Range / Values | Description |
| --- | --- | --- | --- | --- |
| `-capture` | — | `true` | `bool` | Enable automatic headless Chromium screenshot & console capture on failures (graceful HTTP-only fallback if no browser is installed). |
| `-capture-max` | `-cmax` | `50` | `1..500` | Maximum error artifacts captured per run. |
| `-capture-per-error` | `-cpe` | `3` | `1..50` | Maximum artifacts per unique `(path, status, trigger_reason)` fingerprint. |
| `-capture-workers` | `-cw` | `2` | `1..8` | Concurrent headless Chromium capture workers. |
| `-capture-timeout` | `-ct` | `8s` | `> 0` | Per-capture deadline inside Chromium. |
| `-capture-format` | `-cf` | `jpeg` | `jpeg`, `png` | Screenshot image format. |
| `-chrome-bin` | — | `""` | executable path | Explicit path to Chrome/Chromium binary (overrides auto-detection; fails closed if unusable). |
| `-cdp-url` | — | `""` | `ws://`, `wss://`, `http://`, `https://` | Connect to a Chrome DevTools Protocol endpoint instead of launching local Chrome. Restricted to loopback hosts (`localhost`, `127.0.0.1`, `::1`) unless `-allow-remote-cdp` is set. |
| `-allow-remote-cdp` | — | `false` | `bool` | Allow non-loopback `-cdp-url` endpoints. Cloud-metadata, link-local, unspecified, and multicast IPs remain unconditionally blocked by `SafeDialContext`. |
| `-scouts` | — | `0` | `0..8` | Real-browser Scout lemmings running alongside the HTTP swarm to detect pure JS/render errors on HTTP `200 OK` pages. Walks GET-only `-plan` suites or falls back automatically to the indexed `URLPool` when any plan contains non-GET steps. |
| `-dashboard-port` | `-dp` | `4000` | `1024..65535` | Loopback (`127.0.0.1`) port for the live Admin Portal. |
| `-review` | — | `false` | `bool` | Keep the Admin Portal open after the swarm finishes until **Finalize & Write Report** is clicked or Ctrl-C is pressed. |
| `-reveal-session-cookies` | — | `false` | `bool` | Allow the authenticated localhost portal to show raw session cookie values during live inspection (never written to any saved/exported report). |

---

## Reports, Delivery, SMTP & CI Gates

| Flag | Alias | Default | Range / Values | Description |
| --- | --- | --- | --- | --- |
| `-save-to` | `-s` | `.` | comma-separated list | Report targets: local paths, `file://`, `s3://bucket/prefix`, or `mailto:addr?cc=&subject=`. Merged with `LEMMINGS_SAVE_TO`. |
| `-unique-report` | `-ur` | `false` | `bool` | Include UTC timestamp in report filenames (`lemmings.YYYY.MM.DD.HHMMSSZ.<domain>`). |
| `-report-name` | `-rn` | `""` | `[a-zA-Z0-9._-]+` | Custom base filename slug (no `..` or slashes). |
| `-trace-file` | `-tf` | `""` | new file path | Stream every visit to a new `0600` JSONL file (refuses to overwrite existing files). |
| `-max-failure-rate` | `-mfr` | `-1` | `-1` or `0..1` | CI gate: exit `2` if failure share exceeds this value (negative disables). |
| `-p95-budget` | `-p95` | `0` | `>= 0` | CI gate: exit `2` if global p95 **page** visit latency exceeds this duration (`0` disables; excludes waiting-room wait and `-assets` fetches). |
| `-min-plan-pass-rate` | `-mppr` | `-1` | `-1` or `0..1` | CI gate: exit `2` if plan cohort pass share (`PlanPassed / PlanLemmings`) falls below this value. Requires `-plan` (negative disables). |
| `-smtp-host` | `-sh` | `""` | hostname | SMTP server hostname (overrides `LEMMINGS_SMTP_HOST`). |
| `-smtp-port` | `-sp` | `0` | `0..65535` | SMTP server port (`465`=implicit TLS, otherwise STARTTLS; overrides `LEMMINGS_SMTP_PORT`). |
| `-smtp-user` | `-su` | `""` | string | SMTP username (overrides `LEMMINGS_SMTP_USER`). |
| `-smtp-pass-file` | `-spf` | `""` | `0600` file path | File containing SMTP password (strict `0600` permissions enforced on Unix). |
| `-smtp-pass-stdin` | — | `false` | `bool` | Read SMTP password from one line on stdin. |
| `-smtp-require-tls` | — | `false` | `bool` | Require TLS/STARTTLS for SMTP delivery (default `true` for non-loopback hosts). |
| `-smtp-from` | `-sf` | `""` | email address | SMTP `From:` header address (overrides `LEMMINGS_SMTP_FROM`). |

---

## Observability & Terminal Output

| Flag | Alias | Default | Range / Values | Description |
| --- | --- | --- | --- | --- |
| `-observe` | `-obs` | `false` | `bool` | Enable Prometheus `/metrics` HTTP exporter on loopback. |
| `-metrics-port` | `-mp` | `9090` | `1024..65535` | Loopback port for Prometheus `/metrics`. |
| `-metrics-url-label` | `-mul` | `path` | `full`, `path`, `none` | URL label granularity on `lemmings_visit_duration_seconds` (capped at 999 unique URLs). |
| `-tty` | — | `true` | `bool` | Use carriage-return live updates on stdout (`-tty=false` for CI logs). |
| `-color` | — | `true` | `bool` | Synthwave truecolor terminal output (automatically disabled if `NO_COLOR` is set or stdout is not a TTY). |
| `-version` | `-v` | `false` | `bool` | Print version string (`v2.0.0`) and exit `0`. |

---

## Environment Variables

| Variable | Purpose |
| --- | --- |
| `LEMMINGS_SAVE_TO` | Comma-separated report delivery targets merged with `-save-to`. |
| `LEMMINGS_CHROME_BIN` | Default path to local Chrome/Chromium binary (overridden by `-chrome-bin`). |
| `LEMMINGS_SMTP_HOST` | Default SMTP server hostname (default `localhost`). |
| `LEMMINGS_SMTP_PORT` | Default SMTP server port (default `587`). |
| `LEMMINGS_SMTP_USER` | Default SMTP username. |
| `LEMMINGS_SMTP_PASS_FILE` | Path to `0600` file containing SMTP password (checked before `LEMMINGS_SMTP_PASS`). |
| `LEMMINGS_SMTP_PASS` | SMTP password fallback when neither `-smtp-pass-file` nor `LEMMINGS_SMTP_PASS_FILE` is set. |
| `LEMMINGS_SMTP_FROM` | Default SMTP `From:` address (default `lemmings@localhost`). |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | Region used for `s3://` report delivery alongside standard AWS SDK v2 credential chain. |
| `NO_COLOR` | Disables ANSI color escapes in terminal output when set (any value). |
| `COLORTERM` | Enables 24-bit Synthwave truecolor when containing `truecolor` or `24bit`. |
| Plan `${ENV_VAR}` placeholders | User-defined environment variables (`${[A-Za-z_][A-Za-z0-9_]*}`) expanded inside YAML plan `with-cookies`, `with-headers`, `with-body`, and `body` values (fails closed at startup if unset; escape literal `$` as `$$`). |
