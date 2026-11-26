# Plans, Personas, Navigation & Page Checks

Lemmings v2.0.0 unifies visitor navigation, multi-method HTTP scripting (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`), weighted multi-persona suites, automatic rogue traffic, and deep response assertions into one strict **YAML `-plan` engine** — while continuing to verify page health, optional same-origin static assets (`-assets`), and Concert waiting rooms automatically.

> **Upgrading from v1.0?** Legacy `-journey` (`-j`), JSON journeys, `internal/journey/`, `examples/journeys/`, and `docs/journeys-and-checks.md` were removed in v2.0.0 and replaced by `-plan` (`-pl`), YAML plans, `internal/plan/`, `examples/plans/`, and this guide.

---

## Navigation Modes & The Hybrid Rogue Split

| Mode | Flag | How Lemmings Move |
| --- | --- | --- |
| **Link Navigation** *(default without `-plan`)* | `-navigation links` | Each lemming lands on `-hit`, parses the HTML response, and follows a random navigable same-origin link on the current page (sending `Referer`), just like a person clicking around. |
| **Pool Sampling** | `-navigation random` | Each lemming picks URLs uniformly from the indexed `URLPool` (sitemap / `robots.txt` / crawl). Ideal for broad coverage across deep archives. |
| **YAML Plans & Multi-Persona Suites** | `-plan <spec>` (`-pl`) | The plan cohort (default **95%**) walks deterministic, weight-assigned YAML personas with stateful cookies, custom headers/bodies, per-step pacing (`chill`), and strict assertions. The remaining **rogue cohort** (default **5%**, configurable via `-rogue-rate`) roams freely using `-navigation` (`links`/`random`) to uncover unscripted regressions. |

> **Safe Link Filtering (Rogue & Non-Plan Navigation):** In `links` mode, Lemmings automatically refuses to click destructive, internal, or non-page links: paths containing `/logout`, `/log-out`, `/signout`, `/sign-out`, `/delete`, `/remove`, `/destroy`, `/unsubscribe`, Concert internal paths (`/_concert/*`), or binary/archive download extensions (`.zip`, `.pdf`, `.dmg`, `.exe`, `.tar`, etc.), and never leaves the `-hit` origin. (Explicit plan steps *can* test `/logout` or `DELETE` endpoints whenever you declare them.)

---

## Built-In Page Health Checks (Always Active)

Even without assertions on a plan step (or during organic `links`/`random` browsing), every visit automatically verifies:

1. **HTTP Status:** Defaults to any `2xx` (`200..299`) unless overridden by `want-status`. Unexpected statuses fail the visit and trigger Error Forensics capture.
2. **Not-Blank HTML Check (`not-blank`):** An HTML response with zero visible text *and* zero `<script>`, `<img>`, `<svg>`, `<canvas>`, `<video>`, `<audio>`, or `<iframe>` elements fails the `not-blank` check (SPA shells with `<script>` tags are **not** falsely flagged).
   - **Automatic Skip Rules:** `not-blank` is automatically skipped on `HEAD` requests, `204 No Content` / `304 Not Modified` responses, and any plan step whose `want-status` expects *exclusively* non-`2xx` codes (such as `want-status: 404`, `want-status: 401`, or `want-status: 301`).
3. **Body Size & Truncation Integrity:** Responses exceeding `-max-body-bytes` (default `2 MiB`) fail cleanly without unbounded memory growth, and uncompressed responses whose bytes read fall short of `Content-Length` fail with `ErrTruncatedBody` (`error_kind: "truncated"`).
4. **Optional Checksum Drift (`-strict-checksum`):** Compares each page's SHA-512 hash against the baseline captured at index time. By default, drift is recorded as informational; with `-strict-checksum`, drift fails the visit.

---

## Loading Plans (`-plan` / `-pl` & `-max-plan-items` / `-mpi`)

Pass `-plan` (alias `-pl`) as:

- **A single YAML file:** `-plan examples/plans/buy-tokens.yaml`
- **A directory of YAML files:** `-plan ./plans/` — loads all direct `*.yaml` and `*.yml` files in **lexicographic order** (errors if the directory contains no YAML files).
- **A comma-separated mix of files and directories:** `-plan ./plans/core.yaml,./plans/api/,./plans/probes.yml`

### Strict Loader Limits & Validation

| Rule | Limit / Behavior |
| --- | --- |
| **Strict YAML schema** | Decoded with `KnownFields(true)` — any unknown or misspelled field fails validation at startup (exit `1`). |
| **Single document per file** | Multi-document YAML streams (`---` separating multiple documents in one file) are rejected. Use a `plans:` bundle or separate files in a directory. |
| **Steps per plan** | `1` to `-max-plan-items` (`-mpi`, default `999`, valid range `1..1000000`). Exceeding the cap exits `1` with an explicit error showing the override flag. |
| **File read cap** | Automatically scales with `-max-plan-items`: `max(1 MiB, min(maxItems × 4096 bytes, 64 MiB))`. |
| **Plans per suite** | `1` to `100` (`MaxSuitePlans = 100`) across all loaded files and directories. |
| **Unique plan names** | Every plan in the loaded suite must have a unique `name`; duplicates exit `1`. |
| **Same-origin paths** | Every step `path` is resolved against `-hit`; off-origin URLs fail validation at startup. |

---

## File Formats: Single-Plan vs. Multi-Persona Bundle

### Format 1: Single-Plan File (`plan:`)

Top-level keys for a single persona file:

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `name` | `string` | filename stem | Unique persona name shown in CLI summaries, dashboard, traces, and `PlanReport`. |
| `weight` | `int` | `1` | Relative traffic share (`>= 1`; `0` defaults to `1`; negative values fail validation). |
| `stop-on-fail` | `bool` | `false` | When `true`, any failed step terminates that lemming immediately with exit reason `plan-failed`. |
| `loop` | `bool` | `false` | When `true`, lemmings repeat the plan from step 1 until `-until` or `-max-pages` expires. |
| `plan` | `[]PlanItem` | *(required)* | Ordered list of `1..max-plan-items` steps. Mutually exclusive with top-level `plans:`. |

```yaml
name: checkout-happy-path
weight: 10
stop-on-fail: true
loop: false
plan:
  - name: landing
    path: /
    want-status: 200
    want-title: Neon Arcade
  - name: checkout
    path: /checkout
    want-status: 200
    want-body:
      - contains: Tokens are on the way
```

### Format 2: Multi-Persona Bundle File (`plans:`)

Define multiple weighted personas in a single YAML file using top-level `plans:` (mutually exclusive with top-level `plan:`):

```yaml
plans:
  - name: anonymous-browser
    weight: 70
    plan:
      - name: home
        path: /
        want-status: 200
      - name: games
        path: /games
        want-status: 200
  - name: token-buyer
    weight: 30
    stop-on-fail: true
    plan:
      - name: checkout
        path: /checkout
        want-status: 200
      - name: tokens
        path: /tokens
        want-status: 200
        want-body:
          - contains: 10 tokens
            not-contains: Something went wrong
```

---

## Multi-Persona Weighting & Rogue Split Math

When `-plan` is active, Lemmings splits every spawned lemming deterministically using its global 1-based lemming number (`n = 1, 2, 3, ...`) — zero randomness, completely reproducible across CI runs.

### 1. Rogue Cohort Selection (`-rogue-rate` / `-rr`)

- Default `-rogue-rate` is **`-1` (auto)**: resolves to **`0.05` (5%)** when `-plan` is set, and **`0`** when `-plan` is not set. Valid explicit range: `0..1`.
- Lemming `#n` is assigned to the **rogue cohort** if and only if:

```text
floor(n * rate + 1e-9) > floor((n - 1) * rate + 1e-9)
```

| `-rogue-rate` | Total Lemmings | Rogue Lemmings Assigned | Plan Cohort |
| --- | --- | --- | --- |
| Auto (`0.05`) | `1` (`-terrain 1 -pack 1`) | `0` (none — tiny runs stay 100% on-plan) | `1` (`#1`) |
| Auto (`0.05`) | `19` | `0` | `19` (`#1..#19`) |
| Auto (`0.05`) | `20` | `1` (`#20`) | `19` |
| Auto (`0.05`) | `100` | `5` (`#20, #40, #60, #80, #100`) | `95` |
| `0` | Any | `0` (100% strict plan adherence) | 100% |
| `0.25` | `20` | `5` (`#4, #8, #12, #16, #20`) | `15` |
| `1.0` | Any | 100% (all roam via `-navigation`) | `0` |

Rogue lemmings roam using `-navigation` (`links` or `random`), are reported separately in `PlanReport`, and are counted in Prometheus `lemmings_rogue_lemmings_total`. They do **not** dilute `-min-plan-pass-rate`.

### 2. Weighted Persona Assignment

Every non-rogue lemming receives a sequential `planCohortIndex` (`0, 1, 2, ...`) and is assigned to a persona via:

```text
slot = planCohortIndex % TotalWeight
```

where `TotalWeight = sum(plan.Weight)`. Slots map linearly across cumulative weights in suite order:

| Persona | `weight` | Assigned Slots (`TotalWeight = 100`) | Share of Plan Cohort |
| --- | :---: | --- | :---: |
| `anonymous-browser` | `50` | `0..49` | `50%` |
| `returning-customer` | `30` | `50..79` | `30%` |
| `json-rest-api-client` | `20` | `80..99` | `20%` |

---

## Complete `PlanItem` Reference

### Identity, Routing & Flow Control

| Field | Type | Default | Rules & Behavior |
| --- | --- | --- | --- |
| `name` | `string` | `step-N` | Step label shown in `PlanStepReport`, Admin Portal, traces, and forensics. Defaults to 1-indexed `step-1`, `step-2`, etc. |
| `path` | `string` | *(required)* | Relative path (`/checkout?game=outrun`) or full same-origin URL resolved against `-hit`. Off-origin URLs fail at startup. |
| `method` | `string` | `GET` | Case-insensitive; normalized uppercase. Allowed: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`. |
| `chill` | duration string | *(omitted)* | Go duration string (`30s`, `500ms`, `0s`, `1m30s`; rejects negative). Pauses **after** this step completes, overriding `-think-min`/`-think-max`. When omitted, falls back to randomized think time. After the final step of a non-looping plan (`loop: false`), the pause is skipped *unless* `chill` is explicitly set on that final step. |
| `follow-redirects` | `bool` | `true` *(auto `false` for `3xx`)* | Controls whether HTTP `3xx` redirects are followed on this step. Automatically defaults to `false` when `want-status` expects **exclusively** `3xx` codes (`301`, `"301,302"`, `"3xx"`), allowing direct assertion on status + `Location` header. Explicit `true` or `false` always wins. |
| `stop-on-fail` | `bool` | plan `stop-on-fail` | Overrides plan-level `stop-on-fail` for this individual step. When effective `stop-on-fail` is `true` and the step fails, the lemming stops immediately with exit reason `plan-failed`. |

### Request Shaping (`with-*` & `body`)

All four request-shaping fields support `${ENV_VAR}` expansion (`$$` literal escape) and are tagged `json:"-"` so values never appear in reports, traces, or the Admin Portal (SEC-10).

#### `with-cookies`
List of `{name, value}` pairs (unambiguous alias `key` accepted for `name`; setting both `name` and `key` on one entry fails validation). Scalar YAML values like `1` or `true` decode cleanly to strings `"1"` / `"true"`. Empty names and CR/LF/NUL characters in names or values are rejected.

- **Wire behavior:** Seeds each cookie into the lemming's `InspectingJar` at origin `/` *before* issuing the step's request. Because cookies live in the jar rather than raw headers, they persist cleanly across redirects and subsequent steps without ever producing duplicate `Cookie` headers.

```yaml
with-cookies:
  - name: session_override
    value: "${TEST_SESSION_COOKIE}"
  - name: beta_flag
    value: true
```

#### `with-headers`
List of `{key, value}` pairs (unambiguous alias `name` accepted for `key`; setting both `key` and `name` fails validation). Empty keys, CR/LF/NUL characters, and the `Host` header (case-insensitive) are rejected.

- **Wire behavior:** Applied **per-step only** (never leaked to subsequent steps). Overrides default headers such as `Accept`, `User-Agent`, or `Content-Type`.

```yaml
with-headers:
  - key: Authorization
    value: "Bearer ${API_TOKEN}"
  - key: Content-Type
    value: application/json
  - key: Accept
    value: application/json
```

#### `with-body` vs. `body`
Mutually exclusive on a single step (setting both fails validation at startup).

- **`with-body`** — list of `{key, value}` pairs (alias `name` for `key`; empty key rejected; scalar values coerce to strings):
  - **Default encoding:** `application/x-www-form-urlencoded` (automatically sets `Content-Type: application/x-www-form-urlencoded` unless overridden).
  - **JSON object encoding:** When `with-headers` sets `Content-Type` to `application/json` or any `application/*+json` media type (e.g., `application/merge-patch+json`, with or without `; charset=utf-8`), Lemmings encodes `with-body` entries as a JSON object (`{"key": "value"}`).
- **`body`** — raw string payload (ideal for nested JSON objects, JSON arrays, GraphQL queries, XML, or webhook bodies):
  - Defaults `Content-Type: text/plain; charset=utf-8` unless `with-headers` specifies a `Content-Type` (such as `application/json`).

```yaml
# Form submission (application/x-www-form-urlencoded)
- name: login-form
  path: /login
  method: POST
  with-body:
    - key: email
      value: "${TEST_EMAIL}"
    - key: password
      value: "${TEST_PASSWORD}"

# JSON object via with-body
- name: create-item-flat
  path: /api/items
  method: POST
  with-headers:
    - key: Content-Type
      value: application/json
  with-body:
    - key: sku
      value: NEON-01
    - key: quantity
      value: 2

# Nested JSON via raw body
- name: create-order-nested
  path: /api/orders
  method: POST
  with-headers:
    - key: Content-Type
      value: application/json
    - key: Authorization
      value: "Bearer ${API_TOKEN}"
  body: |
    {"customer":{"tier":"gold"},"items":[{"sku":"NEON-01","qty":2}],"note":"$$5 promo"}
```

---

### Assertions (`want-*`)

#### `want-status`
Accepts any of these YAML forms (codes must be `100..599` with `min <= max`; omitted defaults to any `2xx`):

| YAML Form | Example | Matches |
| --- | --- | --- |
| Scalar integer | `want-status: 200` | `200` |
| Single code string | `want-status: "201"` | `201` |
| Comma-separated codes | `want-status: "200,201,204"` | `200`, `201`, or `204` |
| Status class wildcard | `want-status: "2xx"` | `200..299` |
| Numeric range | `want-status: "200-299"` | `200..299` |
| Combined string | `want-status: "200,204,3xx"` | `200`, `204`, or `300..399` |
| YAML list | `want-status: [200, 201]` or `["200", "3xx"]` | Any element in the list |

#### `want-headers`
List of header expectations checked case-insensitively across all response header values (`Headers.Values(key)`):

| Field | Type | Description |
| --- | --- | --- |
| `key` *(alias `name`)* | `string` | Required header name (rejects setting both `key` and `name`). |
| `is` | `string` | Exact value match. |
| `contains` | `string` | Substring must appear in header value. |
| `not-contains` | `string` | Substring must **not** appear in *any* value of this header. |
| `begins-with` | `string` | Prefix match. |
| `ends-with` | `string` | Suffix match. |
| `is-one` | `string` or `[]string` | Exact match against any allowed option (accepts `"DENY,SAMEORIGIN"` or `["DENY", "SAMEORIGIN"]`). |

- **Presence-only check:** Omit all matchers (provide only `key`) to assert the header is present.
- **Multi-value semantics:** Positive matchers pass if *at least one* value of the header satisfies *all* positive matchers on that entry; `not-contains` passes if *no* value contains the forbidden substring.
- **Redaction:** Failure details pass through `secrets.ScrubText`, and sensitive header names (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Auth-Token`, `X-Csrf-Token`, `Phpsessid`, or names containing `token`/`secret`/`session`/`password`) never echo actual header values in `CheckResult.Detail`.

#### `want-cookies`
List of cookie assertions evaluated against the lemming's active `InspectingJar` for the **final URL** (after redirects, if followed):

| Field | Type | Description |
| --- | --- | --- |
| `name` *(alias `key`)* | `string` | Required cookie name (rejects setting both `name` and `key`). |
| `value` / `is` | `string` | Exact cookie value match (`value` is an alias for `is`; setting both fails validation). |
| `contains` | `string` | Cookie value must contain substring. |
| `not-contains` | `string` | Cookie value must not contain substring. |
| `begins-with` | `string` | Cookie value prefix match. |
| `ends-with` | `string` | Cookie value suffix match. |
| `is-one` | `string` or `[]string` | Exact match against any allowed option (comma-separated string or YAML list). |
| `absent` | `bool` | When `true`, asserts the cookie is **not** active for the final URL (e.g., after logout). Rejects combining `absent: true` with any value matcher. |

- **Presence-only check:** Provide only `name` to assert the cookie exists and is active.
- **SEC-10 guarantee:** Raw cookie values **never** appear in `CheckResult.Detail`, reports, trace files, or console output.

#### `want-body`
List of body substring assertions (each entry must specify `contains` and/or `not-contains`):

```yaml
want-body:
  - contains: 10 tokens
    not-contains: Something went wrong
  - not-contains: Internal Server Error
```

#### HTML Parity Assertions (`want-content-type`, `want-title`, `want-selectors`)

| Field | Type | Description |
| --- | --- | --- |
| `want-content-type` | `string` | Expected MIME media type (`text/html`, `application/json`), matched case-insensitively without charset parameters. |
| `want-title` | `string` | Substring required inside `<title>...</title>`. |
| `want-selectors` | `[]string` | Simple DOM selectors that must exist in parsed HTML: tag names (`nav`, `main`, `form`, `ul`), IDs (`#checkout-form`), or single classes (`.grid`, `.price`). Complex combinators (`div > span`) fail validation at startup. |

---

## Secrets, `${ENV_VAR}` Expansion & SEC-10 Hardening

1. **Environment variable interpolation:** `${ENV_VAR}` (matching `${[A-Za-z_][A-Za-z0-9_]*}`) is expanded in `with-cookies` values, `with-headers` values, `with-body` values, and `body`.
2. **Literal `$` escape:** Write `$$` anywhere you need a literal dollar sign (`$$19.99` → `$19.99`, `$${RAW}` → `${RAW}`).
3. **Fail-closed expansion:** If any referenced `${ENV_VAR}` is unset, Lemmings exits `1` immediately naming the missing variable (never printing values).
4. **Zero secret persistence (`SEC-10`):** `with-cookies`, `with-headers`, `with-body`, and `body` are tagged `json:"-"` and excluded from JSON/HTML/Markdown reports, JSONL trace files, and Admin Portal APIs.
5. **Safe Error Forensics for mutating steps:** Failed **non-GET** steps (or steps carrying a request body) automatically capture **HTTP-only forensics** (session state, cookie jar status, lemming `#number`, extracted error body, and check failures) and skip headless CDP replay so Chromium never fires a bogus `GET` request at a mutating `POST`/`PUT`/`DELETE` endpoint. Failed `GET` steps without a body retain full CDP screenshot + JS console replay.
6. **Real-Browser Scouts (`-scouts N`):** Scouts automatically replay a GET-only `-plan` suite; if any plan in the suite contains a non-GET step (`Suite.HasNonGET()`), Scouts automatically fall back to browsing the indexed `URLPool`.

---

## 10 User-Type Persona Blueprints

Below are 10 complete, strict-schema-valid YAML blueprints covering the core visitor archetypes of any modern web application. Adapt the paths, selectors, and environment variables to your origin — save them as individual files inside a `./plans/` directory (run with `-plan ./plans/`) or combine them under one `plans:` bundle. Together, their weights sum to **`100`**.

### 1. Anonymous Browser (`01-anonymous-browser.yaml`, weight `30`)

Models casual visitors reading landing, listing, and detail pages with human reading pauses (`chill`).

```yaml
name: anonymous-browser
weight: 30
stop-on-fail: false
loop: false
plan:
  - name: landing-page
    path: /
    method: GET
    chill: 2s
    want-status: 200
    want-content-type: text/html
    want-title: Welcome
    want-selectors: ["nav", "main", "footer"]
    want-body:
      - not-contains: Something went wrong
  - name: catalog-list
    path: /products
    method: GET
    chill: 3s
    want-status: 200
    want-content-type: text/html
    want-selectors: ["main", ".product-grid"]
    want-headers:
      - key: Cache-Control
  - name: product-detail
    path: /products/featured
    method: GET
    want-status: 200
    want-selectors: ["h1", ".price"]
    want-body:
      - contains: Add to cart
        not-contains: Out of stock error
```

### 2. New Account Signup (`02-new-account-signup.yaml`, weight `5`)

Exercises registration form fetch, URL-encoded `POST` submission, session cookie minting, and onboarding verification with `stop-on-fail: true`.

```yaml
name: new-account-signup
weight: 5
stop-on-fail: true
loop: false
plan:
  - name: load-signup-form
    path: /signup
    method: GET
    chill: 1s
    want-status: 200
    want-content-type: text/html
    want-selectors: ["form", "#signup-form"]
  - name: submit-registration
    path: /signup
    method: POST
    chill: 500ms
    with-headers:
      - key: X-Test-Mode
        value: staging-ephemeral
    with-body:
      - key: email
        value: "${SIGNUP_TEST_EMAIL}"
      - key: password
        value: "${SIGNUP_TEST_PASSWORD}"
      - key: accept_terms
        value: true
    want-status: "200,201"
    want-cookies:
      - name: session_id
    want-body:
      - contains: Account created
        not-contains: Validation failed
  - name: onboarding-welcome
    path: /welcome
    method: GET
    want-status: 200
    want-cookies:
      - name: session_id
    want-body:
      - contains: Get started
```

### 3. Returning Customer Login, Profile & Logout (`03-returning-customer.yaml`, weight `15`)

Verifies full authenticated lifecycle: login with `${ENV_VAR}` credentials, authenticated profile access, logout, and confirmation that the session cookie is cleared (`absent: true`).

```yaml
name: returning-customer
weight: 15
stop-on-fail: true
loop: false
plan:
  - name: login-page
    path: /login
    method: GET
    chill: 500ms
    want-status: 200
    want-selectors: ["form"]
  - name: authenticate
    path: /login
    method: POST
    chill: 1s
    with-body:
      - key: username
        value: "${DEMO_USER}"
      - key: password
        value: "${DEMO_PASS}"
    want-status: 200
    want-cookies:
      - name: session_id
    want-body:
      - contains: Dashboard
        not-contains: Invalid credentials
  - name: view-profile
    path: /account/profile
    method: GET
    chill: 2s
    want-status: 200
    want-content-type: text/html
    want-selectors: ["main", "#profile-details"]
    want-headers:
      - key: Cache-Control
        contains: private
        not-contains: public
    want-cookies:
      - name: session_id
    want-body:
      - contains: Account Settings
  - name: logout
    path: /logout
    method: POST
    want-status: "200,204"
    want-cookies:
      - name: session_id
        absent: true
```

### 4. Checkout & Form Submitter (`04-checkout-submitter.yaml`, weight `10`)

Seeds an experiment/cart cookie via `with-cookies`, loads checkout (including waiting-room support if fronted by Concert), submits a form `POST`, and asserts confirmation without soft errors.

```yaml
name: checkout-submitter
weight: 10
stop-on-fail: true
loop: false
plan:
  - name: view-cart
    path: /cart
    method: GET
    chill: 1s
    with-cookies:
      - name: cart_id
        value: "${DEMO_CART_ID}"
      - name: ab_checkout
        value: v2
    want-status: 200
    want-selectors: ["main", ".cart-summary"]
  - name: load-checkout
    path: /checkout
    method: GET
    chill: 1500ms
    want-status: 200
    want-cookies:
      - name: cart_id
    want-body:
      - contains: Order Summary
        not-contains: Cart is empty
  - name: submit-order
    path: /checkout/confirm
    method: POST
    with-body:
      - key: shipping_method
        value: standard
      - key: promo_code
        value: "$$SAVE10"
      - key: confirm
        value: true
    want-status: "200,201"
    want-body:
      - contains: Order confirmed
        not-contains: Payment failed
```

### 5. JSON REST API Client (`05-json-rest-api-client.yaml`, weight `15`)

Demonstrates Bearer auth headers, `with-body` automatic JSON object encoding, raw `body` nested JSON payloads, `PUT`/`PATCH`/`DELETE` (`204` auto-skipping `not-blank`), and strict header assertions.

```yaml
name: json-rest-api-client
weight: 15
stop-on-fail: true
loop: false
plan:
  - name: list-items
    path: /api/v1/items?limit=20
    method: GET
    chill: 250ms
    with-headers:
      - key: Authorization
        value: "Bearer ${API_TOKEN}"
      - key: Accept
        value: application/json
    want-status: 200
    want-content-type: application/json
    want-headers:
      - key: X-Request-Id
      - key: X-Content-Type-Options
        is: nosniff
    want-body:
      - contains: '"items"'
        not-contains: '"error"'
  - name: create-item-flat-json
    path: /api/v1/items
    method: POST
    chill: 250ms
    with-headers:
      - key: Authorization
        value: "Bearer ${API_TOKEN}"
      - key: Content-Type
        value: application/json
      - key: Accept
        value: application/json
    with-body:
      - key: sku
        value: ARCADE-TOKEN-10
      - key: quantity
        value: 1
    want-status: [200, 201]
    want-content-type: application/json
    want-body:
      - contains: ARCADE-TOKEN-10
  - name: patch-item-nested-json
    path: /api/v1/items/sandbox-item
    method: PATCH
    chill: 250ms
    with-headers:
      - key: Authorization
        value: "Bearer ${API_TOKEN}"
      - key: Content-Type
        value: application/merge-patch+json
    body: '{"metadata":{"source":"lemmings","priority":2},"tags":["loadtest","v2"]}'
    want-status: 200
    want-content-type: application/json
  - name: delete-sandbox-item
    path: /api/v1/items/sandbox-item
    method: DELETE
    with-headers:
      - key: Authorization
        value: "Bearer ${API_TOKEN}"
    want-status: "200,204"
```

### 6. Search & Pagination Explorer (`06-search-pagination.yaml`, weight `10`)

Exercises query-string filtering, multi-page pagination, and graceful zero-result empty states.

```yaml
name: search-pagination
weight: 10
stop-on-fail: false
loop: false
plan:
  - name: search-page-1
    path: /search?q=synth&page=1
    method: GET
    chill: 1s
    want-status: 200
    want-content-type: text/html
    want-selectors: ["main", ".search-results"]
    want-body:
      - contains: synth
        not-contains: SQL syntax
  - name: search-page-2
    path: /search?q=synth&page=2
    method: GET
    chill: 1s
    want-status: 200
    want-selectors: ["main", ".pagination"]
    want-body:
      - not-contains: Internal Server Error
  - name: search-empty-state
    path: /search?q=zzznomatch99999
    method: GET
    want-status: 200
    want-body:
      - contains: No results
        not-contains: panic
```

### 7. Unauthorized / Auth-Guard Prober (`07-auth-guard-prober.yaml`, weight `3`)

Continuously verifies access-control boundaries so protected admin or API routes never accidentally open up under load. Because `want-status` expects exclusively non-`2xx` codes, the built-in `not-blank` HTML check is skipped automatically.

```yaml
name: auth-guard-prober
weight: 3
stop-on-fail: false
loop: false
plan:
  - name: admin-without-session
    path: /admin
    method: GET
    follow-redirects: false
    chill: 200ms
    want-status: "302,401,403"
    want-cookies:
      - name: session_id
        absent: true
    want-body:
      - not-contains: Admin Control Panel
  - name: api-without-token
    path: /api/v1/private/account
    method: GET
    chill: 200ms
    with-headers:
      - key: Accept
        value: application/json
    want-status: "401,403"
    want-headers:
      - key: WWW-Authenticate
    want-body:
      - not-contains: secret_key
  - name: api-with-invalid-token
    path: /api/v1/private/account
    method: GET
    with-headers:
      - key: Authorization
        value: "Bearer invalid-token-probe"
      - key: Accept
        value: application/json
    want-status: 401
```

### 8. Redirect & Canonical-URL Verifier (`08-redirect-canonical.yaml`, weight `3`)

Verifies legacy redirects directly (relying on automatic `follow-redirects: false` when `want-status` is exclusively `3xx`), then verifies end-to-end redirect resolution with `follow-redirects: true`.

```yaml
name: redirect-canonical
weight: 3
stop-on-fail: false
loop: false
plan:
  - name: legacy-permanent-redirect
    path: /old-catalog
    method: GET
    chill: 200ms
    # follow-redirects auto-defaults to false because want-status is exclusively 3xx
    want-status: 301
    want-headers:
      - key: Location
        is-one: ["/products", "/products/"]
  - name: trailing-slash-canonical
    path: /about/
    method: GET
    chill: 200ms
    want-status: "301,308"
    want-headers:
      - key: Location
        ends-with: /about
  - name: follow-redirect-to-destination
    path: /old-catalog
    method: GET
    follow-redirects: true
    want-status: 200
    want-content-type: text/html
    want-selectors: ["main"]
```

### 9. 404 / Dead-Link & Health Prober (`09-dead-link-404-probe.yaml`, weight `2`)

Verifies custom `404` handling and `HEAD`/`OPTIONS` probes without triggering false failures (`not-blank` is automatically skipped on `404` and `HEAD`).

```yaml
name: dead-link-404-probe
weight: 2
stop-on-fail: false
loop: false
plan:
  - name: nonexistent-page-404
    path: /this-route-does-not-exist-lemmings-probe
    method: GET
    chill: 200ms
    want-status: 404
    want-content-type: text/html
    want-body:
      - contains: Not found
        not-contains: stack trace
  - name: health-check-head
    path: /healthz
    method: HEAD
    chill: 200ms
    want-status: 200
  - name: api-cors-options
    path: /api/v1/items
    method: OPTIONS
    want-status: "200,204"
    want-headers:
      - key: Allow
        contains: GET
```

### 10. High-Pace Looping Reader (`10-looping-reader.yaml`, weight `7`)

Sets `loop: true` and tight `chill` intervals so assigned lemmings cycle continuously through high-traffic content pages until `-until` or `-max-pages` expires.

```yaml
name: looping-reader
weight: 7
stop-on-fail: false
loop: true
plan:
  - name: feed-top
    path: /
    method: GET
    chill: 150ms
    want-status: 200
    want-selectors: ["main"]
  - name: latest-posts
    path: /news
    method: GET
    chill: 150ms
    want-status: 200
    want-body:
      - not-contains: Service Unavailable
  - name: status-summary
    path: /status
    method: GET
    chill: 0s
    want-status: 200
    want-headers:
      - key: X-Frame-Options
        is-one: "DENY,SAMEORIGIN"
```

---

## Bundled Example Plans (Neon Arcade)

Lemmings ships five ready-to-run YAML plans under [`examples/plans/`](../examples/plans/) targeting Neon Arcade (`make demo`):

| File | Purpose |
| --- | --- |
| [`examples/plans/neon-arcade-auth.yaml`](../examples/plans/neon-arcade-auth.yaml) | Exercises stateful session/cookie authentication flows and cookie assertions against Neon Arcade (run with `make run-plan`). |
| [`examples/plans/neon-arcade-personas.yaml`](../examples/plans/neon-arcade-personas.yaml) | Multi-persona weighted suite (`plans:`) demonstrating weighted cohorts and rogue split. |
| [`examples/plans/buy-tokens.yaml`](../examples/plans/buy-tokens.yaml) | Ordered checkout and token purchase funnel with waiting-room traversal and soft-error checks. |
| [`examples/plans/forensics-demo.yaml`](../examples/plans/forensics-demo.yaml) | Exercises healthy pages, `/tokens` soft errors, `/api/tokens` structured JSON `500` responses, and `/arcade-glitch` client-side JS crashes (run with `make run-forensics`). |
| [`examples/plans/known-bad.yaml`](../examples/plans/known-bad.yaml) | Intentional failure suite demonstrating `404`, blank-page, and redirect diagnostics. |

```bash
# Terminal 1 — launch Neon Arcade demo target
make demo

# Terminal 2 — walk the Neon Arcade auth plan with CI gates
make run-plan

# Or run the forensics showcase with a browser Scout and -review hold
make run-forensics
```

---

## Same-Origin Static Asset Tier (`-assets`)

Real browsers don't just fetch HTML — they fetch stylesheets, scripts, images, and fonts, often behind edge asset gating (such as Concert's `-assets "/assets/*"` requiring an admitted `concert_admit` pass cookie).

Enable opt-in asset fetching with `-assets` (and tune the per-page budget with `-assets-per-page`, default `8`, range `1..64`):

```bash
lemmings -hit http://127.0.0.1:8081/ \
  -terrain 4 -pack 5 -until 30s -ramp 3s \
  -assets -assets-per-page 8
```

- **Pure-Stdlib HTML & CSS Extraction:** After every successful HTML visit, `plan.ExtractAssets` extracts same-origin `<link rel="stylesheet|icon|shortcut icon|apple-touch-icon|preload|modulepreload|manifest">`, `<script src>`, `<img src|data-src|srcset>`, `<source src|srcset>`, `<video src|poster>`, `<audio src>`, `<embed src>`, `<object data>`, `<iframe src>`, and inline `<style>`/`style=""` `url(...)`/`@import` references while honoring `<base href>` and excluding `/_concert/*`. For fetched CSS responses, `plan.ExtractCSSAssets` also discovers referenced fonts and images within the per-page budget.
- **Per-Lemming Browser Cache:** Each lemming caches fetched asset URLs in a bounded 2,048-entry map so repeat page views do not re-download already-cached assets.
- **Cookie Jar Forwarding & Strict Separation:** Assets are fetched through the lemming's `http.Client` (automatically sending `concert_admit` and session cookies) and recorded as `AssetVisit` telemetry (`AssetReport` by class: `style`, `script`, `image`, `font`, `audio`, `video`, `data`, `other`), tracking `403` denials (`denied_403`) and asset latency separately from page visits and `-p95-budget`.

---

## Think Time, Rate Limits & Concert Waiting Rooms

- **Human Think Time (`-think-min`, `-think-max`):** Defaults to `300ms – 1200ms` randomized per page pause. Set both to `0` (`-think-min 0 -think-max 0`) for maximum saturation testing. On YAML plan steps, an explicit `chill` duration overrides think time for that step.
- **`Retry-After` & Ban Tracking:** On HTTP `429` or `503`, lemmings honor numeric or HTTP-date `Retry-After` headers (bounded by the lemming's remaining lifespan) and back off on consecutive errors. Non-queue `429` and `403` ban responses are tracked separately in `ConcertReport` (`bans_429`, `bans_403`) and Prometheus (`lemmings_bans_total`).
- **Native Concert Waiting Room Protocol:** Lemmings detects and follows three waiting-room formats automatically:
  1. **Concert HTML (`waiting.html`):** matches `/_concert/waiting.js` + `id="position"` (or `class="queue-page"` + `id="position"`), reads initial queue position, and polls `GET /queue/status`.
  2. **Legacy HTML (Neon Arcade / `room`):** matches `/queue/status` + `id="position"` and polls `GET /queue/status`.
  3. **Concert `-api-json` HTTP `429` JSON:** parses `{"queued": true, "status_url": "...", "retry_after_seconds": N}` (enforcing same-origin `status_url`) and respects `retry_after_seconds`.
- **Queue Polling & Admission Lifecycle:** While queued, each lemming polls `GET <origin><statusURL>` with its `room_ticket` cookie and `Accept: application/json`, emitting `events.QueuePoll` per poll (separate from page visits). It handles `ready: true`, position updates, `waiting: false` (ticket expired → `Room.Expired`), `cookies_required`, poll `429` (`3s + jitter` backoff), `403` ban termination (`Room.Denied` + `Bans403`), and network errors (exponential backoff up to `10s`). Once `ready: true`, the lemming reloads the target URL once to receive the admitted origin page and `concert_admit` cookie — reporting **queue wait time (`RoomReport`) strictly separate from origin page latency (`P50`/`P95`/`P99`)**.
