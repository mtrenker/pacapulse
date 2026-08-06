# MVP browser collection protocol

Status: accepted architecture decision for [issue #10](https://github.com/mtrenker/pacapulse/issues/10) and the [`v0.1.0` MVP](https://github.com/mtrenker/pacapulse/issues/1).

This document defines the public browser asset, page-view request, privacy gate, origin policy, retry behavior, abuse controls, and transient visitor-input boundary for the first useful PacaPulse analytics loop. It is normative for the browser beacon and collector implementations. The accepted persistence transaction remains defined by the [MVP analytics data model](data-model.md).

The words **must**, **must not**, **should**, and **may** describe protocol requirements.

## Decisions at a glance

- The MVP collects browser page views only. Reverse-proxy importing and custom events are post-MVP.
- Sites install the transparent major-version asset `/beacon/v1.js`; page views use `POST /api/v1/pageviews`.
- The dependency-free script sends one strict JSON object as `text/plain;charset=UTF-8` using Fetch Keepalive.
- The JSON payload contains only a public collection key, random event UUID, pathname, optional external referrer hostname, three allowlisted UTM values, and the one-way DNT signal when needed.
- The payload never contains a full page URL, full referrer URL, claimed IP address, user-agent value, timezone, screen characteristics, language, arbitrary properties, cookies, or browser-storage identifiers. Ordinary network transport still exposes a source address and browser-generated headers transiently to the trusted ingress boundary.
- Global Privacy Control always prevents collection. Do Not Track prevents collection by default and can be overridden only by explicit matching browser-snippet and server-side site configuration.
- One normal page view and explicit SPA route changes are supported; the script does not patch browser history APIs.
- The collection key and `Origin` reduce accidental pollution but are not authentication and cannot prevent forged non-browser traffic.
- Delivery is best effort: one initial attempt and at most one in-memory retry reuse the same event UUID.
- Fixed global, site, source, body, and writer-queue bounds protect the single-writer service.

## Public installation contract

The default installation snippet is:

```html
<script
  defer
  referrerpolicy="no-referrer"
  src="https://pulse.pacabytes.io/beacon/v1.js"
  data-site="pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ">
</script>
```

The configured PacaPulse base URL replaces `https://pulse.pacabytes.io` for another installation. The script reads `document.currentScript`, validates its `data-site` value against the canonical collection-key form, and derives the collector origin from that element's `src`; it must not hard-code the production hostname. A missing current script, malformed key, or malformed script URL stops silently before installing the API or sending a request.

Tracked production sites must themselves use HTTPS. Plain-HTTP production pages cannot use this protocol because their origins are rejected; installation verification must report that limitation rather than failing silently.

A site with the explicit server-side `collect_dnt` policy enabled receives this generated form:

```html
<script
  defer
  referrerpolicy="no-referrer"
  src="https://pulse.pacabytes.io/beacon/v1.js"
  data-site="pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ"
  data-collect-dnt="true">
</script>
```

No other value enables the browser-side DNT override. The generated snippet must omit the attribute by default.

Place the beacon before a deferred or module application script that calls its SPA API. The recommended guarded call is `window.pacapulse?.("pageview")`. The MVP does not require an inline queue shim, so it remains compatible with a Content Security Policy that disallows inline JavaScript.

### Collection keys

A collection key has the exact form `pp_` followed by 32 unpadded base64url characters generated from 24 cryptographically random bytes. It is a random public site identifier, not a secret, bearer token, or authorization credential.

One key is active per site in the MVP. Rotation creates a new key and installation snippet and immediately invalidates the previous key. There is no grace period and no multiple-active-key mode.

The key must travel only in the request body. It must not appear in an asset URL, endpoint path, query string, log, trace, metric label, error message, or response.

Installation verification belongs to the authenticated administration experience; the public collector does not provide a diagnostic or key-validation response. For an owner-authorized site, administration may show the latest accepted-event time and rolling one-hour counts for `rejected_origin`, `rejected_invalid`, and `rejected_limited`. These are in-memory count buckets keyed by internal site ID, contain no rejected values, expire after one hour, and are not exported as labeled metrics. This distinguishes a recent policy rejection from no request reaching PacaPulse while preserving payload privacy.

## Browser asset

`GET /beacon/v1.js` returns classic JavaScript that has no runtime dependency, inline companion, `eval`, dynamic import, source-map request, cookie access, or browser-storage access.

Required response properties:

```text
Content-Type: application/javascript; charset=utf-8
Cache-Control: public, max-age=3600, stale-while-revalidate=86400
ETag: <content validator>
Vary: Accept-Encoding
X-Content-Type-Options: nosniff
Cross-Origin-Resource-Policy: cross-origin
```

The server must offer Brotli or gzip according to ordinary HTTP negotiation. The initial asset budget is at most 12 KiB uncompressed and 4 KiB compressed. CI must measure the production source, Brotli output, gzip output, cache headers, and `Vary` behavior.

`/beacon/v1.js` is a stable major-version URL. Compatible correctness, browser-compatibility, security, and privacy fixes may update its content. A breaking installation, JavaScript API, payload, or transport change requires `/beacon/v2.js` and `/api/v2/...`.

The default snippet does not use Subresource Integrity because the stable major-version asset must receive compatible fixes without requiring every tracked site to update its markup. Immutable fingerprinted assets and SRI are a possible post-MVP opt-in, not a second MVP installation mode.

Tracked sites that use CSP must allow both loading and collection:

```http
Content-Security-Policy: script-src 'self' https://pulse.pacabytes.io; connect-src 'self' https://pulse.pacabytes.io
```

The script tag's `referrerpolicy="no-referrer"` suppresses the tracked page URL on the asset request. The collection fetch separately sets `referrerPolicy: "no-referrer"`. Sites using nonce- or hash-based CSP with `'strict-dynamic'` must add their own valid nonce to the external beacon tag or load it from an already trusted application script; a host allowlist alone is ignored by supporting browsers when `'strict-dynamic'` is active.

## Browser compatibility boundary

The beacon requires these baseline platform features:

- `fetch`, `Request`, `AbortController`, and `AbortSignal`;
- `URL` and `URLSearchParams`;
- `TextEncoder`;
- Web Crypto secure randomness through `crypto.randomUUID()` or `crypto.getRandomValues()`;
- JSON, Promises, and ordinary modern DOM APIs.

When a required feature is unavailable, the script must silently stop without sending a degraded payload. It must not use `Math.random()`, synchronous XHR, image pixels, form submission, or persistent state as a fallback.

Keepalive is best effort rather than an admission gate. Detect it with `"keepalive" in new Request(location.href)`; use `keepalive: true` when supported and make the same bounded Fetch request without that option otherwise. The fallback may be canceled during unload but still collects ordinary initial and SPA views.

The transport is Fetch, preferably with Keepalive, rather than `navigator.sendBeacon()`. Fetch permits an explicit CORS mode, no-referrer policy, omitted credentials, bounded timeout, and response status handling while retaining unload-oriented delivery where supported. The protocol does not claim guaranteed delivery.

## Privacy gate in the browser

The privacy gate runs before event-ID generation, URL extraction, or network activity.

1. If `navigator.globalPrivacyControl === true`, stop. There is no override.
2. Detect DNT through the standard or legacy browser properties. Values `"1"` and `"yes"` mean enabled.
3. If DNT is enabled and the script does not have exactly `data-collect-dnt="true"`, stop.
4. If the gate allows collection, construct the page-view candidate.

When DNT is enabled and the explicit snippet override is present, the payload includes `"dnt": true`. A browser-side override is not sufficient by itself. The server suppresses when either that payload field or `DNT: 1` reports DNT unless the site's stored `collect_dnt` policy is true. This remains correct when a browser exposes DNT to JavaScript but omits the request header, or when a formerly enabled site policy leaves a stale snippet behind.

Client-side suppression is intentionally no-network. Server-side checks remain mandatory for manually constructed requests, browsers with incomplete JavaScript exposure, stale snippets, and defense in depth.

## Page-load and SPA behavior

### Initial page view and document lifecycle

The deferred script automatically attempts one page view when it executes, except during prerendering. While `document.prerendering === true`, **every** automatic or explicit attempt is deferred without network activity. The script registers one `prerenderingchange` listener and collapses all deferred calls into one attempt for the current route at activation; a prerendered document that is never activated produces no event. Only this first activated-document view may include the hostname derived from `document.referrer`.

A `pageshow` event with `event.persisted === true` represents a bfcache restoration and records a fresh page-view attempt with a new UUID and no external referrer. It resets the in-document route key first so returning to the same pathname is countable. Ordinary `pageshow` events are ignored because script execution already owns the initial view. The privacy gate runs again before a prerender activation, bfcache restoration, or explicit SPA attempt.

### Explicit SPA page views

The script installs one public command function with a read-only integer marker set before automatic collection:

```js
window.pacapulse.protocolVersion === 1
window.pacapulse?.("pageview")
```

On load, any existing PacaPulse function with an integer `protocolVersion` marker makes the new script instance a complete no-op, preventing duplicate CMS, tag-manager, hydration, or cross-major injection from creating a second automatic view. Major-version migration requires replacing the old tag rather than temporarily loading both. An unrelated pre-existing `window.pacapulse` value is never overwritten; collection stops to avoid breaking the tracked application.

An SPA calls the guarded function after a route change has committed and `location` reflects the new route. Unsupported commands, arguments, or event properties are silent no-ops and must not throw or create a request.

The beacon does not monkey-patch `history.pushState`, `history.replaceState`, `popstate`, framework routers, links, or DOM observers. Framework integration remains explicit and predictable.

SPA page views omit `document.referrer`; otherwise every route in one document would be attributed repeatedly to the original external site.

### In-document duplicate suppression

The script keeps one route key in ordinary process memory. It consists of the canonical pathname and the three normalized campaign values. Hashes and all non-allowlisted query parameters are excluded.

Consecutive automatic or explicit attempts with the same route key produce one event attempt. The route key is marked when a valid payload is accepted by the script for delivery, not when the network succeeds. This prevents a framework's initial callback from duplicating the automatic view and keeps failure behavior bounded.

A sequence `/a → /b → /a` records three attempts. A full browser refresh and bfcache restoration each record a fresh attempt. A hash-only or unrelated-query-only change does not create a distinct route key.

## Event identity

Generate one canonical lowercase RFC 4122 UUIDv4 per page-view attempt:

1. Prefer `crypto.randomUUID()`.
2. Otherwise construct UUIDv4 with `crypto.getRandomValues()` and set the version and variant bits explicitly.
3. If secure randomness is unavailable, do not collect.

The UUID is reused only for the one permitted retry of the exact same serialized request. A new route attempt receives a new UUID. No ID survives document destruction or enters cookies, local storage, session storage, IndexedDB, Cache Storage, service-worker state, URLs, logs, or metrics.

The server treats `(site_id, event_id)` as the idempotency key. The first committed payload wins; a duplicate with different content remains a successful no-op.

## Browser extraction and canonicalization

All browser-derived values are attacker-controlled. Client processing minimizes transmission and improves normal behavior; the server must repeat validation and canonicalization.

### Path

The browser reads only `location.pathname`.

- Require `/` as the first character; the home page is `/`.
- Reject literal `?` and `#`.
- Require each `%` to begin a complete two-hex-digit percent escape.
- Uppercase the two hex digits without decoding the escaped octet.
- Preserve trailing slashes, repeated slashes, and distinctions such as `/pricing` versus `/pricing/`.
- Encode as UTF-8 and reject a canonical representation longer than 512 bytes.
- Never read or send the complete `location.href`.

### External referrer hostname

Only the automatic initial view considers `document.referrer`.

- Parse it with `URL`.
- Accept only `http:` or `https:`.
- Extract only `.hostname`; never serialize the original URL.
- Omit the field when parsing fails, the hostname is empty, or its UTF-8 form exceeds 253 bytes.
- The server converts the hostname to lowercase IDNA ASCII, removes a final dot and port, and discards the field when normalization fails, the result exceeds 253 ASCII bytes, or it matches any configured host for the tracked site.

The request's HTTP `Referer` header is never a source for analytics attribution. Both asset and collection requests explicitly suppress it.

### Campaign values

Inspect only these exact lowercase query parameter names:

- `utm_source`
- `utm_medium`
- `utm_campaign`

For each name:

1. If it occurs more than once, omit it as ambiguous.
2. Normalize the decoded string to Unicode NFC.
3. Trim surrounding Unicode whitespace.
4. Apply locale-independent Unicode lowercase conversion.
5. Omit an empty result.
6. Omit that campaign field, but retain the page view, when the result exceeds 100 UTF-8 bytes.

Ignore unknown parameters, differently cased names, `utm_term`, `utm_content`, advertising click identifiers, and every non-allowlisted query value. Invalid or oversized referrer/campaign values lose only that optional dimension, never the otherwise valid page view. Campaign values apply only to the page view carrying them; the script never persists attribution across navigation.

## Request contract

The beacon sends:

```text
POST /api/v1/pageviews HTTP/1.1
Content-Type: text/plain;charset=UTF-8
Origin: https://tracked-site.example
```

The fetch options are semantically equivalent to:

```js
fetch(collectorURL, {
  method: "POST",
  mode: "cors",
  body: serializedPayload,
  ...(supportsKeepalive ? {keepalive: true} : {}),
  credentials: "omit",
  referrerPolicy: "no-referrer",
  headers: {"Content-Type": "text/plain;charset=UTF-8"},
  signal: timeoutSignal
})
```

`mode: "cors"` is mandatory. With `mode: "no-cors"`, the required no-referrer policy can serialize the request origin as opaque `null`, which the collector rejects.

The request body must be no more than 4,096 bytes. The endpoint accepts exactly one UTF-8 JSON object despite the CORS-safelisted media type. Parse the media type rather than comparing raw header bytes: its case-insensitive essence must be `text/plain`, and an optional `charset` parameter must case-insensitively equal `utf-8`; other media types receive `415`. `application/json` is not accepted because it would introduce a preflight for the normal cross-origin request.

### Payload schema

A complete example is:

```json
{
  "v": 1,
  "site": "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ",
  "event_id": "550e8400-e29b-41d4-a716-446655440000",
  "path": "/pricing",
  "referrer_host": "search.example",
  "utm_source": "newsletter",
  "utm_medium": "email",
  "utm_campaign": "spring-launch"
}
```

An explicitly DNT-overridden request adds `"dnt": true`; ordinary requests omit it.

| Field | Required | Type and bound | Browser responsibility | Server responsibility |
| --- | --- | --- | --- | --- |
| `v` | yes | integer, exactly `1` | Emit exactly `1` | Require exactly `1` |
| `site` | yes | string, exactly `pp_` plus 32 base64url characters | Copy and validate the exact `data-site` value | Repeat format validation and resolve an enabled site |
| `event_id` | yes | string, canonical 36-character UUIDv4 | Generate secure canonical UUIDv4 | Repeat canonical lowercase UUIDv4 validation |
| `path` | yes | string, 1–512 UTF-8 bytes | Extract and canonicalize pathname only | Repeat canonicalization; reject the event when invalid |
| `referrer_host` | no | string, at most 253 UTF-8 bytes in the browser and 253 IDNA ASCII bytes on the server | Initial view only; extract hostname or omit invalid/oversized value | Normalize IDNA host; discard invalid, oversized, or same-site value while retaining the event |
| `utm_source` | no | string, at most 100 normalized UTF-8 bytes | Extract one exact lowercase parameter; omit invalid/oversized value | Repeat NFC/trim/lowercase; discard invalid/oversized value while retaining the event |
| `utm_medium` | no | string, at most 100 normalized UTF-8 bytes | Same as source | Same as source |
| `utm_campaign` | no | string, at most 100 normalized UTF-8 bytes | Same as source | Same as source |
| `dnt` | no | boolean, only `true` is valid | Include only when DNT is detected and the explicit snippet override is present | Treat true like `DNT: 1`; suppress unless the stored site override is enabled |

Optional fields are omitted rather than serialized as `null`. An optional campaign member that normalizes to empty, invalid, or oversized is equivalent to absence. Optional-value failure never rejects an otherwise valid page view. At least one non-empty campaign member is required to create a campaign aggregate.

The server must reject:

- unknown object members;
- duplicate JSON object names;
- trailing JSON values or non-whitespace bytes;
- malformed UTF-8;
- wrong JSON types, including numeric strings for `v`;
- noncanonical collection keys or event UUIDs;
- invalid or oversized mandatory paths;
- `dnt: false` or a non-boolean DNT value;
- any body that exceeds 4,096 bytes before parsing.

Invalid or oversized optional referrer/campaign values are discarded rather than included in this rejection list.

The protocol contains no extension-property bag. A future field requires a reviewed protocol revision; silently accepting unknown input risks accidental personal-data collection.

## Origin and CORS policy

The browser must send exactly one syntactically valid `Origin`. The server normalizes its hostname to lowercase IDNA ASCII before comparing it with the site's canonical and allowed hosts.

- Production origins must use `https`.
- `http` is accepted only for `localhost`, `127.0.0.1`, or `[::1]` development origins.
- Loopback hosts must belong to a separate development site configuration and collection key. Administration must warn rather than adding a loopback host to a production site's allowlist; any local application on any port could otherwise submit under that production key.
- Any port is part of the echoed origin but host authorization compares the normalized hostname.
- Missing, opaque `null`, malformed, multiple, or disallowed origins are rejected as silent no-collection outcomes.
- A non-browser client can forge `Origin`; the check is pollution resistance, not authentication.

After resolving an enabled site and accepting the origin, the collector adds:

```text
Access-Control-Allow-Origin: <exact accepted Origin>
Vary: Origin
```

Credentialed CORS is never enabled. Normal collection is a CORS-simple request, so the endpoint does not expose a permissive generic `OPTIONS` path. Unknown sites and disallowed origins receive no CORS approval.

## Server processing order

The collector follows this order so expensive and privacy-sensitive work occurs only after cheaper gates:

1. Require the POST route, enforce generic connection controls, and apply the instance token bucket.
2. Read at most 4,096 bytes, parse the safelisted media type, and require one strict JSON object.
3. Validate protocol version, collection-key form, event UUID, mandatory path shape/bounds, and JSON types. Discard semantically invalid optional dimensions rather than rejecting the page view.
4. Resolve an enabled site from the public key. Unknown or disabled sites become silent no-collection outcomes.
5. Validate the request `Origin` against the site's configured hosts and establish the exact CORS response value.
6. Apply the site's token bucket, now with a visible CORS-approved `429` when limited.
7. If `Sec-GPC: 1` is present, return successful no-collection.
8. If either `DNT: 1` or payload `dnt: true` is present and the site's stored `collect_dnt` is false, return successful no-collection.
9. Obtain a trustworthy canonical client address and apply the transient-source token bucket. An unavailable address skips both source limiting and visitor estimation; global and site bounds still apply.
10. Repeat full path, referrer-host, campaign, and UUID canonicalization. A mandatory-value error creates no write; an optional-value error drops that dimension.
11. Capture one server instant and derive both `received_at_ms` and the UTC aggregate day.
12. Classify the request user agent transiently into the approved coarse browser/device classes and derive the site/day visitor key when a trustworthy address exists.
13. Call the existing atomic page-view storage API and return only after its durable result or error.

Request bodies, raw headers, remote addresses, forwarding chains, full user-agent values, collection keys, event UUIDs, paths, referrers, campaigns, and visitor keys must not be logged while processing any stage.

## GPC and DNT on the server

`Sec-GPC: 1` always produces successful no-collection after site and origin validation. No configuration or snippet attribute can override it.

`DNT: 1` or payload `dnt: true` produces successful no-collection unless the stored site has `collect_dnt = true`. The payload field can only make collection more restrictive; it never enables an override. The server does not receive or trust `data-collect-dnt`; that attribute only controls whether the official script sends the request.

A privacy-suppressed request must not:

- canonicalize or HMAC an address;
- classify a user agent;
- call the storage writer;
- create an event, visitor key, or aggregate;
- log request or payload data.

Suppression returns the same empty success response as accepted and duplicate collection.

## Client address and transient visitor inputs

The server never accepts an address in the JSON payload.

- Trust `X-Forwarded-For` only when the immediate TCP peer belongs to configured trusted-proxy CIDRs.
- Parse every chain member as an IP address; do not use string slicing.
- Walk a valid chain from right to left, skipping configured trusted proxies. The first untrusted address is the client.
- Ignore `Forwarded`, `X-Real-IP`, and every forwarding header received from an untrusted immediate peer.
- For a direct unproxied request, use the actual peer address.
- Unmap IPv4-mapped IPv6 and retain the canonical 4- or 16-byte value only in request memory.
- If the chain is malformed or no trustworthy address exists, accept the page view without a visitor estimate.

Collection readiness requires a random base64-encoded visitor master secret containing at least 32 decoded bytes. The secret is never stored in SQLite, backups, logs, traces, metrics, or issue evidence.

Visitor derivation uses the HMAC and coarse browser/device-class contract in the data-model document. Full user-agent and address values are discarded after the request. Secret or classifier changes occur only at a UTC boundary because a mid-day change can count returning visitors twice.

## Abuse resistance and bounded state

The MVP uses fixed in-memory token buckets:

| Boundary | Sustained rate | Burst |
| --- | ---: | ---: |
| Whole instance | 100 requests/second | 500 requests |
| One resolved site | 50 requests/second | 200 requests |
| One transient source within a site | 10 requests/second | 50 requests |

The instance limit applies before site lookup. The site limit applies after key resolution and allowed-origin establishment, so a valid browser can observe its `429`. GPC/DNT is checked before deriving the source key.

The source limiter key uses a separate site/day HMAC context over the canonical address; it is not the analytics visitor key and includes no browser/device class. Raw addresses never become map keys. Source entries expire after at most ten minutes and the map is capped at 10,000 entries. At the cap, inserting a new key evicts the least-recently-used entry rather than denying every new visitor. Global and site buckets still bound an attacker who churns source keys. An unlabeled `source_limiter_evictions` counter makes sustained churn visible without retaining source data.

The storage layer's fixed 256-event queue is final write backpressure. A full queue is retryable unavailable; collection must not create another unbounded queue in the HTTP layer.

Ingress may impose stricter generic connection and body controls. It must not parse analytics payloads, label traffic with collection values, persist source addresses, or enable access logging for `/beacon/` or `/api/v1/pageviews`.

These limits protect a modest single-writer service, not a high-traffic analytics platform. Sustained legitimate limiting means the installation exceeds the MVP envelope and requires an explicit capacity decision rather than silent limit expansion.

## Responses and retry behavior

Every collector response has an empty body and `Cache-Control: no-store`.

| Outcome | HTTP status | CORS behavior | Browser action |
| --- | ---: | --- | --- |
| Durably committed | `204` | exact allowed origin | finish |
| Duplicate event | `204` | exact allowed origin | finish |
| GPC/DNT suppressed | `204` | exact allowed origin | finish |
| Unknown or disabled site | `204` | no approval | finish on opaque CORS failure; do not retry |
| Missing or disallowed origin | `204` | no approval | finish on opaque CORS failure; do not retry |
| Invalid payload | `400` | exact origin only when already established | finish; do not retry |
| Unsupported media type | `415` | normally no approval | finish on opaque CORS failure; do not retry |
| Body over 4,096 bytes | `413` | normally no approval | finish on opaque CORS failure; do not retry |
| Instance rate limited before site resolution | `429` | no approval | finish on opaque CORS failure; do not retry |
| Site or source rate limited | `429` | exact allowed origin | retry once |
| Writer queue full | `503` | exact allowed origin | retry once |
| Storage unavailable | `503` | exact allowed origin | retry once |

Unsupported methods receive an empty `405` without a permissive CORS response. Internal failures are mapped to an empty `503`; raw driver errors, SQL text, filesystem paths, identifiers, or payload content never enter a public response.

The browser:

1. Serializes the payload once and starts a five-second bounded Fetch attempt, with Keepalive when supported.
2. Finishes on visible `204`, `400`, `413`, or `415`.
3. Treats a generic Fetch `TypeError`, including an opaque CORS or ordinary network failure, as final and does not retry; otherwise invalid keys and rejected origins would double their own load.
4. For an `AbortError` caused by the five-second timeout or a visible `429`/`503`, waits a random 250–750 milliseconds if the document remains active.
5. Retries exactly once with the byte-identical body and event UUID.
6. Performs no further retry, persistence, console logging, or unload guarantee. Document destruction cancels retry scheduling but does not deliberately abort a Keepalive request already handed to the browser.

A CORS failure intentionally does not reveal whether the failure was an unknown key, disabled site, rejected origin, early body/media rejection, or instance limiting. Authenticated installation verification supplies site-scoped aggregate diagnostics instead. The no-retry rule also keeps these early rejection paths at one request rather than invalidating the abuse-rate calculations with automatic amplification.

## Operational privacy

Default application and ingress behavior must not record:

- source or forwarding addresses;
- `Origin`, `Referer`, DNT, GPC, user-agent, or raw headers;
- request bodies or body fragments;
- collection keys, event UUIDs, paths, referrer hosts, campaigns, or visitor keys;
- SQL statements, arguments, database paths, or driver errors.

Metrics may expose only aggregate counters and histograms such as total requests, broad outcomes, body-size buckets, queue saturation, and latency. Site, origin, address, route value, event, and collection-key labels are prohibited. Traces must follow the same rule and must not capture HTTP headers or bodies automatically.

Operational errors use stable categories such as invalid, limited, busy, unavailable, or internal. They do not echo attacker-controlled values.

## Representative flows

### Normal page view

1. The script privacy gate passes.
2. It derives a route key, generates a UUIDv4, and extracts bounded fields.
3. Fetch sends one 4 KiB-or-smaller credentialless request with no referrer.
4. The server enforces instance/site/source limits, site and origin policy, and privacy headers.
5. It derives server time and an optional site/day visitor key from transient request metadata.
6. The storage writer commits the ledger and aggregates atomically.
7. The server returns empty `204`; the script finishes.

### Browser-side privacy suppression

1. GPC or default DNT is detected before UUID generation.
2. No URL fields are extracted and no request is made.
3. No server or storage state changes.

### Server-side privacy suppression

1. A manually constructed or stale client reaches the server with `Sec-GPC: 1` or applicable `DNT: 1`.
2. Site, site rate, and origin checks pass.
3. The server returns empty `204` before source-address or user-agent processing.
4. No storage state changes.

### Duplicate delivery

1. The first attempt exceeds the browser's five-second timeout but commits before cancellation reaches storage.
2. The one timeout retry sends byte-identical content and UUID.
3. Storage recognizes `(site_id, event_id)` and commits a successful no-op.
4. Counts remain incremented once.

### Disallowed origin

1. The key resolves, but normalized `Origin` does not match an allowed host.
2. The server returns empty `204` without `Access-Control-Allow-Origin`.
3. The browser observes only a generic CORS failure and does not retry.
4. No visitor inputs are derived and no storage state changes.

### Writer backpressure

1. The storage queue is full.
2. The collector returns empty `503` with CORS approval for the valid site and origin.
3. The script retries once with the same UUID after jitter.
4. If still unavailable, it stops without claiming collection.

### SPA navigation

1. The initial automatic page view records path A and the external referrer hostname.
2. The router commits path B and calls `window.pacapulse?.("pageview")`.
3. The route key differs, so the script records B without a referrer.
4. A repeated callback for B is suppressed.
5. A later route back to A records A again without a referrer.

### Prerender and bfcache

1. A prerendered document defers collection while `document.prerendering` is true.
2. If activated, `prerenderingchange` produces its initial page view; if discarded, it produces none.
3. A later bfcache restoration receives `pageshow` with `persisted` true, resets the route key, and creates a fresh no-referrer page view after repeating the privacy gate.

### Duplicate script injection

1. The first valid script instance installs a version-tagged command function and owns automatic collection.
2. A second instance observes that marker and stops before UUID generation.
3. Exactly one initial page-view attempt is created.

## Contract fixtures

[`collection-protocol-fixtures.json`](collection-protocol-fixtures.json) contains representative browser extraction, accepted, suppressed, duplicate, origin, and invalid-input cases. Later JavaScript and Go implementations must consume equivalent fixtures rather than translating examples independently.

### Fixture format

The JSON document has these top-level members:

- `protocol_version`: protocol major version under test.
- `collection_key`: canonical default fixture key.
- `site`, `dnt_override_site`, `development_site`, and `disabled_site`: server-side configurations referenced by cases.
- `browser_extraction`: URL/referrer/lifecycle inputs and the fields or send decision a browser implementation must produce.
- `server_requests`: valid, suppressed, limited, unavailable, and silent-rejection requests with required status, CORS, storage, and transient-processing outcomes.
- `invalid_requests`: malformed or unsupported envelopes with expected status, CORS, and proof that storage is not called.

`route_key_fields` is the ordered `[path, utm_source, utm_medium, utm_campaign]` tuple. `omitted` names fields that must not be serialized. `precondition` describes setup required before the request. `generated_body_bytes`, `generated_path_utf8_bytes`, and `generated_utm_campaign_utf8_bytes` instruct a harness to generate exactly that byte length. `raw_body_base64` carries non-UTF-8 envelope bytes. Lifecycle fields such as `wait_for`, `fresh_event_id`, `reset_route_key`, and `install_api` are direct browser assertions. Expected `storage` values are `committed`, `duplicate_no_op`, `retryable_unavailable`, and `not_called`; `browser_retry` defines the sole allowed replay. A null `cors_origin` means no `Access-Control-Allow-Origin` header.

Fixtures describe protocol behavior; they do not authorize tests to bypass the public storage API or weaken database fixture coverage.

## Rejected alternatives

### `navigator.sendBeacon()` as the primary transport

Rejected because it cannot set the required referrer policy, credentials mode, or ordinary headers and cannot inspect the response status. Fetch Keepalive preserves unload-oriented best effort with the controls this privacy boundary needs.

### `application/json`

Rejected for the browser request because it triggers CORS preflight. The strict JSON object remains the body syntax while the safelisted `text/plain;charset=UTF-8` media type avoids an additional request and permissive `OPTIONS` policy.

### Collection key in a URL or header

Query and path values are commonly retained by access logs and infrastructure. A custom header triggers preflight. The bounded body is the least exposed compatible location.

### Full page or referrer URLs

Rejected because the product needs only pathname, three campaign values, and external hostname. Parsing in the browser prevents full URLs and unrelated query parameters from crossing the network at all.

### Automatic History API patching

Rejected because it can conflict with routers and double-count framework callbacks. One automatic initial view plus an explicit route API has a smaller, testable contract.

### Persistent retries or identities

Rejected because cookies or browser storage would weaken the core privacy promise. Directional analytics accepts best-effort loss rather than retaining delivery or visitor state in the browser.

### Permissive CORS

Rejected because exact allowed-origin reflection prevents ordinary cross-site browser use of a copied collection key. It remains pollution resistance rather than authentication.

### Configurable or disguised collector names

Rejected because they complicate support and imply intentional blocker evasion. The public names are deliberately transparent.

### Immutable default asset with SRI

Rejected for the default because compatible security and privacy fixes would require every tracked site to update its snippet. A stable major-version asset is the single MVP mode; immutable opt-in delivery may be evaluated later.

### Silent acceptance of unknown payload fields

Rejected because arbitrary or accidentally added values can carry personal data and create undocumented behavior. Protocol evolution is explicit.

### Browser-supplied timestamps, IPs, or user-agent classes

Rejected because they are forgeable and create inconsistent day boundaries or expanded collection. Server receive time and transient request metadata own those values.

## Follow-up implementation outcomes

After approval, publish only the first executable wave:

1. Implement the strict collector handler, origin/privacy gates, transient visitor derivation, rate limits, and contract-fixture tests against the existing storage API.
2. Implement and serve the dependency-free browser asset with browser tests, size checks, and CSP/install documentation.
3. Connect authenticated site administration to key creation/rotation, allowed hosts, DNT policy, generated snippets, and installation verification.

The collector handler and browser asset may be separate issues if their file boundaries and review remain independent. Neither may be marked agent-ready until this decision is accepted.
