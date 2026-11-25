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

## Metric Catalog (17 Metrics)

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
| `lemmings_visits_total` | Counter | `status_class` | Completed page visits grouped by `2xx`, `3xx`, `4xx`, `5xx`, or `unknown`. |
| `lemmings_failed_visits_total` | Counter | — | Visits failing HTTP status, transport, blank-page, or journey checks. |
| `lemmings_cancelled_visits_total` | Counter | — | Visits interrupted because a lemming's lifespan or the run ended. |
| `lemmings_visit_duration_seconds` | Histogram | `url` *(omitted when `-metrics-url-label none`)* | End-to-end visit latency distribution in seconds. |
| `lemmings_bytes_total` | Counter | — | Total decoded response bytes transferred. |
| `lemmings_waiting_room_total` | Counter | — | Total waiting room stays detected. |
| `lemmings_waiting_room_duration_seconds` | Histogram | — | Time spent waiting in virtual queues (tracked separately from HTTP latency). |

### Error Forensics & Browser Scouts *(New in v2.0)*

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `lemmings_artifacts_captured_total` | Counter | `mode` | Error forensic artifacts stored, by capture mode (`cdp-replay`, `cdp-scout`, `http-only`). |
| `lemmings_artifacts_dropped_total` | Counter | `reason` | Error captures skipped or dropped (`deduped`, `queue_full`, `cap_reached`). |
| `lemmings_js_console_errors_total` | Counter | — | Total JavaScript console errors and uncaught exceptions captured across Chromium replays and Scouts. |
| `lemmings_scouts_alive` | Gauge | — | Real-browser Scout lemmings currently running. |

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
```
