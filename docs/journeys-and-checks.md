# Journeys, Navigation & Page Checks

Lemmings supports three visitor navigation strategies and automatically verifies what each visitor actually received — catching silent `200 OK` error pages, broken templates, blank responses, and waiting-room bottlenecks.

---

## Navigation Modes

| Mode | Flag | How Lemmings Move |
| --- | --- | --- |
| **Link Navigation** *(default)* | `-navigation links` | Each lemming lands on `-hit`, parses the HTML response, and follows a random navigable same-origin link on the current page (sending `Referer`), just like a person clicking around. |
| **Pool Sampling** | `-navigation random` | Each lemming picks URLs uniformly from the indexed `URLPool` (sitemap / robots.txt / crawl). Ideal for broad coverage across deep archives. |
| **Ordered Journeys** | `-journey file.json` | Every lemming walks an exact multi-step funnel in order, validating step-specific assertions on every page and carrying its own cookies across steps. |

> **Safe Link Filtering:** In `links` mode, Lemmings automatically refuses to click destructive, internal, or non-page links: paths containing `/logout`, `/log-out`, `/signout`, `/sign-out`, `/delete`, `/remove`, `/destroy`, `/unsubscribe`, Concert internal paths (`/_concert/*`), or binary/archive download extensions (`.zip`, `.pdf`, `.dmg`, `.exe`, `.tar`, etc.), and never leaves the `-hit` origin.

---

## Built-In Page Health Checks (Always Active)

Even without a `-journey` file, every HTML visit automatically checks:

1. **HTTP Status:** `2xx` (or redirected to a `2xx` on the same origin) succeeds; `4xx`/`5xx` fails and triggers error forensics capture.
2. **Not-Blank Check:** An HTML response with zero visible text *and* zero `<script>`, `<img>`, `<svg>`, `<canvas>`, `<video>`, `<audio>`, or `<iframe>` elements fails the `not-blank` check (SPA shells with `<script>` tags are **not** falsely flagged).
3. **Body Size & Truncation Integrity:** Responses exceeding `-max-body-bytes` (default `2 MiB`) fail cleanly without unbounded memory growth, and uncompressed responses whose bytes read fall short of `Content-Length` fail with `ErrTruncatedBody` (`error_kind: "truncated"`).
4. **Optional Checksum Drift (`-strict-checksum`):** Compares each page's SHA-512 hash against the baseline captured at index time. By default, drift is recorded as informational; with `-strict-checksum`, drift fails the visit.

---

## Writing a Journey File (`-journey`)

A journey is a UTF-8 JSON file (up to `1 MiB`, `1..100` steps) defining a `name` and ordered `steps`:

```json
{
  "name": "buy-tokens",
  "steps": [
    {
      "name": "arrive",
      "path": "/",
      "expect": {
        "status": [200],
        "content_type": "text/html",
        "title_contains": "Neon Arcade",
        "selectors": ["nav", ".grid"],
        "contains": ["Insert coin"]
      }
    },
    {
      "name": "browse",
      "path": "/games",
      "expect": {
        "status": [200],
        "selectors": ["ul"],
        "contains": ["Outrun Horizon"]
      }
    },
    {
      "name": "checkout",
      "path": "/checkout",
      "expect": {
        "status": [200],
        "contains": ["Tokens are on the way"]
      }
    },
    {
      "name": "tokens",
      "path": "/tokens",
      "expect": {
        "status": [200],
        "contains": ["10 tokens"],
        "not_contains": ["Something went wrong"]
      }
    }
  ]
}
```

### Step & Expectation Reference

| Field | Type | Description |
| --- | --- | --- |
| `name` | `string` | Human-readable step label shown in the Admin Portal, trace JSONL, and reports. |
| `path` | `string` | Relative path (`/checkout?game=outrun`) or full URL **on the same origin as `-hit`** (off-origin URLs fail validation at startup). |
| `expect.status` | `[]int` | Allowed final HTTP status codes (defaults to any `2xx` if omitted). |
| `expect.content_type` | `string` | Expected MIME media type (e.g., `"text/html"`, `"application/json"`), matched case-insensitively without charset parameters. |
| `expect.title_contains` | `string` | Case-sensitive substring required inside `<title>...</title>`. |
| `expect.contains` | `[]string` | Substrings that **must** appear in the response body. |
| `expect.not_contains` | `[]string` | Substrings that **must NOT** appear in the response body (ideal for catching `200 OK` soft errors such as `"Something went wrong"` or `"Internal error"`). |
| `expect.selectors` | `[]string` | Simple DOM selectors that must exist in parsed HTML: tag names (`nav`, `main`, `ul`), IDs (`#checkout-form`), or single classes (`.grid`, `.price`). Complex combinators (`div > span`) are rejected at validation time. |

---

## Bundled Example Journeys

Lemmings includes two ready-to-run journeys against Neon Arcade (`make demo`):

- [`examples/journeys/buy-tokens.json`](../examples/journeys/buy-tokens.json) — 5-step checkout and token purchase funnel (run with `make run-journey`).
- [`examples/journeys/forensics-demo.json`](../examples/journeys/forensics-demo.json) — exercises healthy pages, `/tokens` soft errors, `/api/tokens` structured JSON `500` responses, and `/arcade-glitch` client-side JS crashes (run with `make run-forensics`).

---

## Same-Origin Static Asset Tier (`-assets`)

Real browsers don't just fetch HTML — they fetch stylesheets, scripts, images, and fonts, often behind edge asset gating (such as Concert's `-assets "/assets/*"` requiring an admitted `concert_admit` pass cookie).

Enable opt-in asset fetching with `-assets` (and tune the per-page budget with `-assets-per-page`, default `8`, range `1..64`):

```bash
lemmings -hit http://127.0.0.1:8081/ \
  -terrain 4 -pack 5 -until 30s -ramp 3s \
  -assets -assets-per-page 8
```

- **Pure-Stdlib HTML & CSS Extraction:** After every successful HTML visit, `journey.ExtractAssets` extracts same-origin `<link rel="stylesheet|icon|shortcut icon|apple-touch-icon|preload|modulepreload|manifest">`, `<script src>`, `<img src|data-src|srcset>`, `<source src|srcset>`, `<video src|poster>`, `<audio src>`, `<embed src>`, `<object data>`, `<iframe src>`, and inline `<style>`/`style=""` `url(...)`/`@import` references while honoring `<base href>` and excluding `/_concert/*`. For fetched CSS responses, `journey.ExtractCSSAssets` also discovers referenced fonts and images within the per-page budget.
- **Per-Lemming Browser Cache:** Each lemming caches fetched asset URLs in a bounded 2,048-entry map so repeat page views do not re-download already-cached assets.
- **Cookie Jar Forwarding & Strict Separation:** Assets are fetched through the lemming's `http.Client` (automatically sending `concert_admit` and session cookies) and recorded as `AssetVisit` telemetry (`AssetReport` by class: `style`, `script`, `image`, `font`, `audio`, `video`, `data`, `other`), tracking `403` denials (`denied_403`) and asset latency separately from page visits and `-p95-budget`.

---

## Think Time, Rate Limits & Concert Waiting Rooms

- **Human Think Time (`-think-min`, `-think-max`):** Defaults to `300ms – 1200ms` randomized per page pause. Set both to `0` (`-think-min 0 -think-max 0`) for maximum saturation testing.
- **`Retry-After` & Ban Tracking:** On HTTP `429` or `503`, lemmings honor numeric or HTTP-date `Retry-After` headers (bounded by the lemming's remaining lifespan) and back off on consecutive errors. Non-queue `429` and `403` ban responses are tracked separately in `ConcertReport` (`bans_429`, `bans_403`) and Prometheus (`lemmings_bans_total`).
- **Native Concert Waiting Room Protocol:** Lemmings detects and follows three waiting-room formats automatically:
  1. **Concert HTML (`waiting.html`):** matches `/_concert/waiting.js` + `id="position"` (or `class="queue-page"` + `id="position"`), reads initial queue position, and polls `GET /queue/status`.
  2. **Legacy HTML (Neon Arcade / `room`):** matches `/queue/status` + `id="position"` and polls `GET /queue/status`.
  3. **Concert `-api-json` HTTP `429` JSON:** parses `{"queued": true, "status_url": "...", "retry_after_seconds": N}` (enforcing same-origin `status_url`) and respects `retry_after_seconds`.
- **Queue Polling & Admission Lifecycle:** While queued, each lemming polls `GET <origin><statusURL>` with its `room_ticket` cookie and `Accept: application/json`, emitting `events.QueuePoll` per poll (separate from page visits). It handles `ready: true`, position updates, `waiting: false` (ticket expired -> `Room.Expired`), `cookies_required`, poll `429` (`3s + jitter` backoff), `403` ban termination (`Room.Denied` + `Bans403`), and network errors (exponential backoff up to `10s`). Once `ready: true`, the lemming reloads the target URL once to receive the admitted origin page and `concert_admit` cookie — reporting **queue wait time (`RoomReport`) strictly separate from origin page latency (`P50`/`P95`/`P99`)**.
