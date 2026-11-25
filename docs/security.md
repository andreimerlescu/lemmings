# Security Posture & Integrated Audit Report

Lemmings handles sensitive session cookies, untrusted HTML/JSON responses from target servers, local headless Chromium processes, SMTP/S3 credentials, and a localhost Admin Portal. This document summarizes the threat model, security architecture, and all 10 findings (`SEC-01`–`SEC-10`) audited and remediated in Lemmings v2.0.

---

## Trust Boundaries & Threat Model

| Boundary | Untrusted Input / Risk | Mitigations Enforced |
| --- | --- | --- |
| **Target Origin Responses** | Malicious sitemaps, off-origin redirects, huge bodies, XSS in titles/URLs, secrets in error dumps. | Strict scheme/host/port `SameOrigin` checks; `4 MiB` index & `2 MiB` visit body caps; fixed worker pool (`16`); zero `innerHTML` in portal; strict hash-based CSP; `secrets.ScrubText` across all extracted error bodies and JS console logs. |
| **Loopback Admin Portal & Prometheus** | DNS rebinding from external websites, cross-origin CSRF POSTs, token brute force, XSS. | Bound strictly to `127.0.0.1`; `Host` header allowlist (`localhost`/`127.0.0.1`/`::1`); HMAC-SHA256 signed random session cookies (`HttpOnly`, `SameSite=Strict`); per-window auth rate limit (`429` after 15 failures/min); Go 1.25+ `http.NewCrossOriginProtection()` + JSON `Content-Type` enforcement on all `POST` endpoints. |
| **Headless Chromium Process (CDP)** | Ambient host credentials leaking into browser renderer, cross-lemming cookie bleed, arbitrary remote DevTools origins. | Strictly allowlisted `minimalBrowserEnv` (strips all host env secrets); ephemeral `0700` temp profile deleted on exit; `--remote-debugging-address=127.0.0.1` + `--remote-allow-origins=http://127.0.0.1` (no wildcard `*`); fresh incognito `BrowserContext` created and disposed per capture. |
| **Report Delivery (Local / S3 / SMTP)** | Path traversal / symlink escapes, SMTP CRLF header injection, plaintext TLS downgrade, secret leakage into argv/reports. | `os.OpenRoot` filesystem confinement (`0750`/`0640`); `0600` `O_EXCL` trace files; strict CR/LF address validation; TLS 1.2+ fail-closed SMTP (`RequireTLS`); `secrets.Secret` redacting value type (`[REDACTED]`); `-smtp-pass` CLI flag removed. |

---

## Integrated Security Audit Findings (`SEC-01` – `SEC-10`)

| ID | Severity | Finding (v1.0) | Remediation (v2.0) | Verification |
| --- | --- | --- | --- | --- |
| `SEC-01` | High | `-smtp-pass` / `-spw` exposed SMTP passwords in process `argv` (`ps aux`) and shell history; plain `string` field risked accidental logging. | Removed `-smtp-pass`/`-spw`; added `-smtp-pass-file` (`0600` TOCTOU-safe check), `-smtp-pass-stdin`, `LEMMINGS_SMTP_PASS_FILE`, `LEMMINGS_SMTP_PASS`, and redacting `secrets.Secret` type implementing `fmt.Formatter`, `json.Marshaler`, `TextMarshaler`, and `slog.LogValuer`. | `TestSecret_RedactsInAllSinks`, `TestFromFile_PermissionsAndLoad`, `TestE2E_VersionAndNoSecretCLIFlag` |
| `SEC-02` | High | `MailTarget.buildMessage` interpolated `From`, `To`, `Cc`, and attachment filename directly into MIME headers without CR/LF validation, enabling SMTP header injection. | Validated all envelope and header addresses with `net/mail.ParseAddress` + CR/LF/NUL rejection; encoded subjects with `mime.QEncoding` after CR/LF stripping; formatted attachment headers via `mime.FormatMediaType`. | `TestMailTarget_RejectsCRLFHeaderInjection` |
| `SEC-03` | Medium | `MailTarget` silently sent plaintext email if remote SMTP did not advertise `STARTTLS`, and lacked explicit `MinVersion: tls.VersionTLS12` and dial/write timeouts. | Enforced `MinVersion: tls.VersionTLS12`, context-bounded dialing, `RequireTLS` defaulting to `true` for non-loopback SMTP hosts (`-smtp-require-tls`), and zero plaintext fallback on failed `STARTTLS`. | `TestMailTarget_RequireTLSFailsClosedOnPlaintextServer` |
| `SEC-04` | Medium | Prometheus `/metrics` bound `:port` without a loopback-only `Host` header check, exposing metrics to DNS-rebinding pages. | Bound strictly to `127.0.0.1:<port>` synchronously and wrapped handler with `localOnlyHost` returning `403 Forbidden` for non-loopback `Host` headers. | `TestPrometheusObserver_MetricsCardinalityHostGuardAndShutdown` |
| `SEC-05` | Medium | Dashboard stored raw `token` on `Swarm`, used `tokenHash` directly as the cookie value, had no login brute-force throttling, and no CSRF guard on future POST routes. | Removed raw token from `Swarm`; issued random 128-bit session IDs signed with ephemeral HMAC-SHA256 (`SameSite=Strict`, `HttpOnly`); added sliding-window auth limiter (`429`); added `http.NewCrossOriginProtection()` + JSON content-type checks on `POST /api/artifacts/{id}/review` and `POST /api/finalize`. | `TestDashboard_AuthHMACRateLimitLocalOnlyAndCSRF`, `TestDashboard_CSPAllowsExactlyTheInlineAssets` |
| `SEC-06` | Medium | `LocalTarget` wrote reports with raw `filepath.Join` + `os.WriteFile` (`0644`); trace file used `0644`. | Confined `LocalTarget` writes inside Go's `os.OpenRoot` with strict filename/domain allowlists (`0750`/`0640`) and restricted `TraceWriter` to `0600` (`O_EXCL`). | `TestLocalTarget_ConfinedRootAndArtifacts`, `TestTraceWriter_PermissionsAndNoOverwrite` |
| `SEC-07` | Low | `BuildURLPool` spawned one goroutine per sitemap URL with a semaphore (unbounded goroutine creation on large sitemaps) and lacked explicit `http.Transport` timeouts. | Replaced with fixed worker pool (`indexConcurrency = 16`) over a channel and explicit `DialContext`/`TLSHandshakeTimeout`/`ResponseHeaderTimeout` transport. | `TestBuildURLPool_SitemapAndBoundedConcurrency` |
| `SEC-08` | Low | Dashboard server and broadcaster goroutines were launched unjoined and relied on `time.Sleep(300ms)` during shutdown. | Replaced with synchronous `net.Listen`, `sync.WaitGroup`-joined server/broadcaster/watcher goroutines, and clean SSE drain channel (`closing`). | `TestDashboard_StartAndShutdownClean`, `go test -race ./...` |
| `SEC-09` | High *(new v2.0 surface)* | Spawning local Chromium via `exec.Command` inherits `os.Environ()` by default — exposing host tokens (AWS, vault, SMTP) to browser renderers rendering untrusted target pages. | Constructed an explicit `minimalBrowserEnv` allowlist (`HOME`, `TMPDIR`, `LANG`, `PATH`, plus OS display vars on Linux/Windows) — zero host secrets ever enter Chrome's environment. | `TestMinimalBrowserEnv_ExcludesHostSecrets` |
| `SEC-10` | High *(new v2.0 surface)* | Error bodies, JS console output, and session cookie jars can contain Bearer tokens, JWTs, API keys, or session IDs that must not leak into shared reports. | Implemented `secrets.ScrubText` across all extracted error bodies and JS console entries; allowlisted diagnostic response headers; marked `CookieStatus.RawValue` `json:"-"` and stripped it in `Store.SnapshotForReport`; raw cookie inspection requires explicit `-reveal-session-cookies` opt-in on authenticated localhost only. | `TestStore_CapsDedupReviewAndRedaction`, `TestScrubText_RedactsSensitiveTokens`, `TestRenderData_HybridForensicsPackagingAndSecretRedaction` |

---

## Secret Handling Best Practices

1. **SMTP passwords:** Store in a `0600` file (`chmod 600 smtp.pass`) and pass `-smtp-pass-file /path/to/smtp.pass`, or pipe via `-smtp-pass-stdin`, or set `LEMMINGS_SMTP_PASS_FILE` / `LEMMINGS_SMTP_PASS` from your secret manager.
2. **Cookie inspection:** Leave `-reveal-session-cookies` off (the default) unless actively debugging session-token corruption locally on your own workstation. Even when enabled, raw cookies are never persisted to reports.
3. **Target authorization:** Only point Lemmings at origins you own or have explicit written authorization to load-test.
