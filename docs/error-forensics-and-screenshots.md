# Error Forensics & Screenshots

When a simulated visitor runs into trouble on your website, HTTP status codes alone rarely tell the full story. Lemmings v2.0 automatically captures a **four-part forensic dossier** for failing sessions, streams each artifact live into the Admin Portal for visual inspection and triage, and embeds every redacted artifact into the final reports.

---

## The Four Captured Error Artifacts

Every captured error artifact (`forensics.Artifact`) answers four operational questions:

### 1. Session State, Cookie Jar Status & Lemming Number
- **Global Lemming Number (`#1..N`)**, unique ID, friendly name (e.g., `Hyper Viper`), terrain/pack slot, browser persona, full User-Agent, and `Accept-Language`.
- **Journey & Trail Context:** active navigation mode (`links`, `random`, `journey`, `browser-scout`), journey name & step, visit sequence number, session age, waiting room queue status, `Referer`, and the last 8 visited paths (`RecentTrail`).
- **Cookie Jar Status (`InspectingJar`):** total cookies, active vs. expired counts, overall health (`empty`, `healthy`, `has-expired`), and per-cookie metadata (`Name`, `Domain`, `Path`, `Secure`, `HttpOnly`, `SameSite`, `ExpiresAt`, `SessionOnly`, `ValueBytes`, and a per-run HMAC-SHA256 `Fingerprint` so you can verify whether a cookie changed without ever exposing raw token values).

### 2. JavaScript Console Output
Captured via Chrome DevTools Protocol (`Runtime.consoleAPICalled`, `Runtime.exceptionThrown`, `Log.entryAdded`, and `Network.loadingFailed`):
- `console.error`, `console.warn`, `console.log`, and `console.info` entries with source URL, line, and column.
- Uncaught JavaScript exceptions with stack traces.
- Failed subresource loads (broken images, failed XHR/fetch scripts, missing CSS).
- All console text and stack traces are automatically scrubbed via `secrets.ScrubText` before storage.

### 3. Visual Screenshot
- Rendered inside a freshly created **ephemeral incognito Chromium `BrowserContext`** sized to the lemming's viewport (`1280×800` desktop or `390×844` mobile based on User-Agent).
- Before navigating, Lemmings injects the failing lemming's cookies, User-Agent, `Accept-Language`, and `Referer` so the screenshot reflects what *that exact visitor session* saw.
- Encoded as compressed JPEG (`-capture-format jpeg`, default, ~20–60 KB per image) or lossless PNG (`-capture-format png`), capped at `2 MiB` per screenshot, with SHA-256 digest verification.
- The ephemeral `BrowserContext` is disposed immediately after capture so cookies never leak between lemmings.

### 4. Parsed & Extracted Error Page Body
Even when no browser is available (HTTP-only mode), Lemmings parses up to `512 KiB` of the failing response body and extracts structured diagnostics:
- **HTML:** `<title>`, error/alert containers (`role="alert"`, `.error`, `.alert`, `.danger`), `<h1>`/`<h2>` headings, paragraphs, and `<pre>` stack traces (stripping `<script>`, `<style>`, `<svg>`, and `<template>`).
- **JSON / RFC 7807 Problem Details:** `title`, `error`, `message`, `detail`, `code`/`error_code`, `request_id`/`trace_id`, and validation error arrays.
- **XML (SOAP / S3 style):** leaf `<Code>`/`<FaultCode>`, `<Message>`/`<FaultString>`, and `<RequestId>`/`<TraceId>` elements.
- **Plain Text:** first headline line, stack trace lines, and bounded body excerpt.
- **Allowlisted Diagnostic Headers Only:** retains safe headers (`X-Request-Id`, `X-Correlation-Id`, `Traceparent`, `Cf-Ray`, `X-Amz-Request-Id`, `Retry-After`, `Cache-Control`, `Server`, `Via`, `X-Cache`) while explicitly excluding `Set-Cookie`, `Authorization`, and custom auth headers.
- Every extracted field is scrubbed via `secrets.ScrubText` before leaving memory.

---

## Pure-Go Headless Chromium Engine (Zero Node Dependencies)

Lemmings speaks the Chrome DevTools Protocol directly in pure Go using `golang.org/x/net/websocket`. There is no Node.js runtime, no Playwright install step, and no external helper script.

### Browser Resolution Order

1. **`-cdp-url <ws:// or http://>`** — connects to an existing remote or sidecar Chromium instance (for example, a Docker container running `chromedp/headless-shell` in CI). Supports either a direct `ws://.../devtools/browser/...` URL or an HTTP base URL (`http://127.0.0.1:9222`), from which Lemmings queries `/json/version`. If `-cdp-url` is explicitly set and unreachable, Lemmings fails closed.
2. **`-chrome-bin <path>`** (or `LEMMINGS_CHROME_BIN`) — launches the specified executable locally with hardened headless flags and a strictly scrubbed environment. Fails closed if unusable.
3. **Auto-detection (default when `-capture=true`)** — probes standard install locations for Google Chrome, Chromium, Brave, and Edge on macOS, Linux, and Windows.
4. **Graceful HTTP-only fallback** — if `-capture=false` or no local browser is found (and `-scouts 0`), Lemmings runs cleanly in `http-only` mode: you still get Session State, Cookie Jar Status, Lemming `#Number`, and Parsed Error Body artifacts without screenshots or JS console output.

---

## Real-Browser Scouts (`-scouts N`)

Standard HTTP lemmings are fast and scalable (thousands of concurrent visitors), but they do not execute JavaScript. A page can return **`200 OK`** with valid HTML while a React/Vue hydration error or broken bundle crashes the UI.

Pass `-scouts 1` (up to `8`) to launch dedicated **real-browser Scout lemmings** alongside the HTTP swarm:

```bash
lemmings -hit https://staging.example.com/ \
  -terrain 10 -pack 20 -until 60s \
  -scouts 2 -review
```

Each Scout:
- Gets its own sequential lemming number (`#N`) and isolated incognito Chromium context.
- Walks the configured `-journey` or samples the indexed URL pool.
- Automatically captures a full 4-part artifact (tagged `cdp-scout`) whenever a page emits a **`console.error`**, throws an **uncaught JS exception**, fails a **subresource load**, returns **HTTP ≥ 400**, or fails a **journey check** — even on HTTP `200 OK` pages.

---

## Trigger Rules, Deduplication & Resource Caps

| Trigger | Captured By |
| --- | --- |
| HTTP `4xx` or `5xx` status | HTTP swarm (`cdp-replay` / `http-only`) & Scouts (`cdp-scout`) |
| Transport error (timeout, TLS, EOF, DNS, offsite redirect) | HTTP swarm |
| Failed journey or blank-page check (including `200 OK` soft errors) | HTTP swarm & Scouts |
| JS `console.error`, uncaught exception, or failed subresource on `200 OK` | Browser Scouts (`-scouts N`) |

To protect both your workstation and the target server during a failure storm, captures are strictly bounded:

| Flag | Default | Purpose |
| --- | --- | --- |
| `-capture` | `true` | Enable/disable headless screenshot replay (`-capture=false` forces HTTP-only forensics). |
| `-capture-max` (`-cmax`) | `50` | Maximum total artifacts stored per run (`1..500`). |
| `-capture-per-error` (`-cpe`) | `3` | Maximum artifacts per unique `(path, status, trigger_reason)` fingerprint (`1..50`). |
| `-capture-workers` (`-cw`) | `2` | Concurrent headless Chromium replay workers (`1..8`). |
| `-capture-timeout` (`-ct`) | `8s` | Per-capture deadline inside Chromium. |
| `-capture-format` (`-cf`) | `jpeg` | Screenshot encoding: `jpeg` (compact) or `png` (lossless). |

---

## Cookie & Secret Redaction Guarantees

- **Saved reports (HTML, Markdown, JSON, S3, Email, Trace JSONL) ALWAYS redact cookie values.** Only cookie metadata and ephemeral HMAC fingerprints are persisted.
- **Authenticated Localhost Portal Opt-In:** If you need to inspect a failing session's exact raw cookie values during live local debugging, start Lemmings with `-reveal-session-cookies`. Even then, raw values are served **only** over the authenticated loopback `/api/artifacts/{id}` endpoint and are **never** written to disk or exported reports.

---

## Hybrid Report Packaging

| Destination / Format | Screenshot Handling |
| --- | --- |
| **Local / S3 HTML** | Embedded as self-contained inline `data:image/jpeg;base64,...` URIs up to a `12 MiB` budget (overflow artifacts link cleanly to their `.artifacts/` file). |
| **Email HTML (`mailto:`)** | Uses a dedicated `2 MiB` SMTP-safe inline budget so reports stay deliverable through corporate mail relays. |
| **Markdown (`.md`)** | Links relative screenshot paths (`./<report-name>.artifacts/<id>.jpg`) written alongside the report on Local and S3 targets, rendering natively on GitHub and Markdown viewers. |
| **JSON (`.json`)** | Keeps JSON lean and diffable: stores `image_path`, `sha256`, dimensions, byte size, triage status/note, session metadata, console entries, and extracted error body — **zero base64 blobs**. |
