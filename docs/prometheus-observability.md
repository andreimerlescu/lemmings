# Prometheus Observability

Pass `-observe` (alias `-obs`) to expose an isolated Prometheus `/metrics` endpoint on `127.0.0.1:9090` (configurable via `-metrics-port`).

```bash
lemmings -hit https://staging.example.com/ \
  -terrain 10 -pack 25 -until 2m -ramp 20s \
  -observe -metrics-port 9090 -metrics-url-label path
```

- **Isolated Registry:** Uses a dedicated `prometheus.NewRegistry()` without global Go/process collector pollution.
- **DNS-Rebinding Protection:** Bound exclusively to `127.0.0.1` and guarded by a `Host` allowlist (`localhost`, `127.0.0.1`, `::1`); non-loopback `Host` headers return `403 Forbidden`.
- **Cardinality Protection:** `-metrics-url-label` (`path`, `full`, or `none`) caps distinct URL label values at **999**; any URL beyond the cap is grouped under `url="other"` to protect your Prometheus TSDB.

---

## Metric Catalog (22 Metrics)

### Swarm & Lifecycle

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `lemmings_alive` | Gauge | — | Number of lemmings currently active. |
| `lemmings_completed_total` | Counter | — | Lemmings that completed their lifespan normally. |
| `lemmings_failed_total` | Counter | — | Lemmings that failed to start. |
| `lemmings_terrains_online` | Gauge | — | Terrain goroutine groups currently active. |
| `lemmings_overflow_logs_total` | Counter | — | Completed `LifeLog`s routed to the overflow channel. |
| `lemmings_dropped_logs_total` | Counter | — | Completed `LifeLog`s dropped because both primary and overflow channels were full. |

### Traffic, Latency & Waiting Rooms

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `lemmings_visits_total` | Counter | `status_class` | Completed non-cancelled page visits grouped by `2xx`, `3xx`, `4xx`, `5xx`, or `000` (cancelled mid-visit reads are excluded and counted exclusively in `lemmings_cancelled_visits_total`). |
| `lemmings_failed_visits_total` | Counter | — | Page visits failing HTTP status, transport, truncation, blank-page, or journey checks. |
| `lemmings_cancelled_visits_total` | Counter | — | Page visits interrupted because a lemming's lifespan or the run ended. |
| `lemmings_visit_duration_seconds` | Histogram | `url` *(omitted when `-metrics-url-label none`)* | End-to-end page visit latency distribution in seconds (excludes waiting-room wait and asset fetches). |
| `lemmings_bytes_total` | Counter | — | Total decoded page response bytes transferred. |
| `lemmings_waiting_room_total` | Counter | — | Total waiting room stays detected. |
| `lemmings_waiting_room_duration_seconds` | Histogram | — | Time spent waiting in virtual queues (tracked separately from HTTP page latency). |
| `lemmings_queue_polls_total` | Counter | — | Total waiting room `/queue/status` poll requests sent. |
| `lemmings_bans_total` | Counter | `status` | Non-queue ban or rate-limit responses observed by HTTP status (`429`, `403`). |

### Static Asset Tier (`-assets`)

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `lemmings_assets_total` | Counter | `class`, `status_class` | Same-origin static asset fetches by bounded class (`style`, `script`, `image`, `font`, `audio`, `video`, `data`, `other`) and status class (`2xx`, `3xx`, `4xx`, `5xx`, `000`). |
| `lemmings_asset_duration_seconds` | Histogram | — | Same-origin static asset fetch latency distribution in seconds. |

### Error Forensics & Browser Scouts

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `lemmings_artifacts_captured_total` | Counter | `mode` | Error forensic artifacts stored, by capture mode (`cdp-replay`, `cdp-scout`, `http-only`). |
| `lemmings_artifacts_dropped_total` | Counter | `reason` | Error captures skipped or dropped (`deduped`, `queue_full`, `cap_reached`). |
| `lemmings_js_console_errors_total` | Counter | — | Total JavaScript console errors and uncaught exceptions captured across Chromium replays and Scouts. |
| `lemmings_scouts_alive` | Gauge | — | Real-browser Scout lemmings currently running. |
| `lemmings_scout_visits_total` | Counter | `outcome` | Real-browser Scout page navigations by outcome (`ok`, `failed`). |

---

## Example Prometheus Scrape Config & Queries

```yaml
scrape_configs:
  - job_name: lemmings
    scrape_interval: 2s
    static_configs:
      - targets: ["127.0.0.1:9090"]
```

Useful PromQL expressions during a load test:

```promql
# Live requests per second by status class
sum by (status_class) (rate(lemmings_visits_total[15s]))

# Live failure ratio
rate(lemmings_failed_visits_total[30s]) / clamp_min(sum(rate(lemmings_visits_total[30s])), 1)

# 95th percentile page latency by path
histogram_quantile(0.95, sum by (le, url) (rate(lemmings_visit_duration_seconds_bucket[30s])))

# Frontend JS error rate observed by Chromium captures & Scouts
rate(lemmings_js_console_errors_total[30s])

# Static asset throughput by class & status (when -assets is enabled)
sum by (class, status_class) (rate(lemmings_assets_total[15s]))

# Concert queue poll rate & ban rate
rate(lemmings_queue_polls_total[15s])
sum by (status) (rate(lemmings_bans_total[30s]))
```
