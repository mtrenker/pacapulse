/*! PacaPulse browser beacon, protocol v1. AGPL-3.0-only. Served byte for
 * byte as GET /beacon/v1.js with no build step in between, inside a hard
 * 12 KiB / 4 KiB compressed budget. Every rule below is explained in
 * docs/architecture/collection-protocol.md.
 */
(function () {
  "use strict";

  // A pre-existing global stops this instance before it reads the DOM, makes
  // randomness, or reaches the network. A marked function already owns
  // collection; an unrelated value belongs to the page.
  if (window.pacapulse !== undefined) return;

  var win = window;
  var doc = document;
  var webcrypto = win.crypto;

  // A browser that cannot make the exact documented request sends nothing.
  // There is no degraded transport and no insecure-randomness fallback.
  var required = ["fetch", "Request", "AbortController", "AbortSignal", "URL",
    "URLSearchParams", "TextEncoder", "Promise"];
  for (var i = 0; i < required.length; i++) {
    if (typeof win[required[i]] !== "function") return;
  }
  if (!win.JSON || !webcrypto ||
      (typeof webcrypto.randomUUID !== "function" &&
        typeof webcrypto.getRandomValues !== "function")) return;

  // Everything configurable comes from this script's own tag, so no
  // hostname is hard-coded. Malformed input stops silently.
  var script = doc.currentScript;
  if (!script || typeof script.getAttribute !== "function") return;
  var siteKey = script.getAttribute("data-site");
  if (typeof siteKey !== "string" || !/^pp_[A-Za-z0-9_-]{32}$/.test(siteKey)) return;
  // Only the exact string "true" enables the one-way DNT override.
  var collectDNT = script.getAttribute("data-collect-dnt") === "true";
  var endpoint;
  try {
    var asset = new win.URL(script.src);
    if (asset.protocol !== "https:" && asset.protocol !== "http:") return;
    endpoint = asset.origin + "/api/v1/pageviews";
  } catch {
    return;
  }

  var encoder = new win.TextEncoder();
  var CAMPAIGNS = ["utm_source", "utm_medium", "utm_campaign"];

  // Ordinary memory that dies with the document; nothing is persisted.
  var routeKey = null; // last route accepted for delivery
  var awaitingActivation = false; // one prerenderingchange listener is armed
  var retryTimer = 0; // at most one pending retry
  var frozen = false; // set by pagehide, cleared by pageshow

  // Runs first on every attempt: before URL access, before a UUID exists,
  // before any network. null suppresses collection.
  function privacyGate() {
    var nav = win.navigator;
    // GPC has no override, here or on the server.
    if (nav && nav.globalPrivacyControl === true) return null;
    var signals = [nav ? nav.doNotTrack : null, win.doNotTrack, nav ? nav.msDoNotTrack : null];
    var enabled = signals.indexOf("1") !== -1 || signals.indexOf("yes") !== -1;
    if (enabled && !collectDNT) return null;
    return { dnt: enabled };
  }

  function utf8Length(value) {
    return encoder.encode(value).length;
  }

  // location.pathname only; the full URL is never read for analytics.
  function canonicalPath(value) {
    if (typeof value !== "string" || value.charAt(0) !== "/") return null;
    if (value.indexOf("?") !== -1 || value.indexOf("#") !== -1) return null;
    // Every % must begin a complete two-hex escape.
    if (/%(?![0-9A-Fa-f]{2})/.test(value)) return null;
    // Uppercased but never decoded, so /a%2Fb and /a/b stay distinct routes.
    var out = value.replace(/%[0-9A-Fa-f]{2}/g, function (hex) {
      return hex.toUpperCase();
    });
    return utf8Length(out) > 512 ? null : out;
  }

  // Three exact lowercase names; every other parameter is ignored, not sent.
  function campaignValues(search) {
    var params = new win.URLSearchParams(search);
    var values = [];
    for (var n = 0; n < CAMPAIGNS.length; n++) {
      // A repeat drops the dimension, not the view; toLowerCase is
      // locale-independent, so one campaign is one aggregate row.
      var all = params.getAll(CAMPAIGNS[n]);
      var value = all.length === 1 ? all[0].normalize("NFC").trim().toLowerCase() : "";
      values.push(utf8Length(value) > 100 ? "" : value);
    }
    return values;
  }

  // Hostname only, http(s) only, never the original referrer URL.
  function referrerHost() {
    var raw = doc.referrer;
    if (typeof raw !== "string" || raw === "") return "";
    var parsed;
    try {
      parsed = new win.URL(raw);
    } catch {
      return "";
    }
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return "";
    var host = parsed.hostname;
    return host === "" || utf8Length(host) > 253 ? "" : host;
  }

  // Web Crypto only, reused solely for the one permitted retry.
  function eventID() {
    if (typeof webcrypto.randomUUID === "function") return webcrypto.randomUUID();
    var bytes = webcrypto.getRandomValues(new Uint8Array(16));
    bytes[6] = (bytes[6] & 0x0f) | 0x40; // version 4
    bytes[8] = (bytes[8] & 0x3f) | 0x80; // RFC 4122 variant
    var out = "";
    for (var n = 0; n < 16; n++) {
      out += (bytes[n] + 0x100).toString(16).slice(1);
      if (n === 3 || n === 5 || n === 7 || n === 9) out += "-";
    }
    return out;
  }

  // 250-750 ms of Web Crypto jitter.
  function retryDelay() {
    var value = typeof webcrypto.getRandomValues === "function"
      ? webcrypto.getRandomValues(new Uint32Array(1))[0]
      : parseInt(webcrypto.randomUUID().slice(0, 8), 16);
    return 250 + (value % 501);
  }

  // "initial" is the one automatic view -- at script execution or prerender
  // activation -- and the only kind that may carry a referrer hostname.
  function attempt(kind) {
    var gate = privacyGate();
    if (gate === null) return;

    // Prerendering defers every attempt into one at activation; a document
    // that is never activated records nothing.
    if (doc.prerendering === true) {
      if (!awaitingActivation) {
        awaitingActivation = true;
        doc.addEventListener("prerenderingchange", onActivation, { once: true });
      }
      return;
    }

    var path = canonicalPath(win.location.pathname);
    if (path === null) return;
    var campaigns = campaignValues(win.location.search);

    // Pathname plus campaigns, so a hash-only change is not a new route.
    var key = [path].concat(campaigns).join("\n");
    if (key === routeKey) return;

    var payload = { v: 1, site: siteKey, event_id: eventID(), path: path };
    if (kind === "initial") {
      var host = referrerHost();
      if (host !== "") payload.referrer_host = host;
    }
    for (var n = 0; n < CAMPAIGNS.length; n++) {
      if (campaigns[n] !== "") payload[CAMPAIGNS[n]] = campaigns[n];
    }
    // One-way: this can only suppress collection, never enable it.
    if (gate.dnt) payload.dnt = true;

    var body = JSON.stringify(payload);
    if (utf8Length(body) > 4096) return;

    // Marked on acceptance for delivery, not on network success.
    routeKey = key;
    transmit(body, false);
  }

  function onActivation() {
    awaitingActivation = false;
    attempt("initial");
  }

  // Best effort: the option is omitted rather than set to false.
  function keepaliveSupported() {
    try {
      return "keepalive" in new win.Request(win.location.href);
    } catch {
      return false;
    }
  }

  function transmit(body, isRetry) {
    var controller = new win.AbortController();
    var timedOut = false;
    var timer = win.setTimeout(function () {
      timedOut = true;
      controller.abort();
    }, 5000);

    // mode "cors" is mandatory: under "no-cors" the required no-referrer
    // policy can serialize the origin as opaque null, which is rejected.
    var options = {
      method: "POST",
      mode: "cors",
      credentials: "omit",
      referrerPolicy: "no-referrer",
      headers: { "Content-Type": "text/plain;charset=UTF-8" },
      body: body,
      signal: controller.signal
    };
    if (keepaliveSupported()) options.keepalive = true;

    win.fetch(endpoint, options).then(
      function (response) {
        win.clearTimeout(timer);
        // Only a visible 429 or 503 earns the one retry.
        if (!isRetry && (response.status === 429 || response.status === 503)) {
          scheduleRetry(body);
        }
      },
      function () {
        win.clearTimeout(timer);
        // A generic TypeError -- opaque CORS failure, rejected origin,
        // dropped connection -- is final: retrying would double exactly the
        // rejected traffic. Only this script's own bound retries, hence the
        // flag rather than an error name.
        if (!isRetry && timedOut) scheduleRetry(body);
      }
    );
  }

  function scheduleRetry(body) {
    if (!active()) return;
    retryTimer = win.setTimeout(function () {
      retryTimer = 0;
      // Byte-identical body and event UUID: a duplicate is a server no-op.
      if (active()) transmit(body, true);
    }, retryDelay());
  }

  function active() {
    return !frozen && doc.defaultView !== null;
  }

  // No history API, router, link, or observer is patched.
  win.addEventListener("pageshow", function (event) {
    frozen = false;
    // Script execution already owns the initial view, so only a bfcache
    // restoration counts; resetting the route key lets the same path recount.
    if (event && event.persisted === true) {
      routeKey = null;
      attempt("restore");
    }
  });

  win.addEventListener("pagehide", function () {
    // A destroyed or frozen document stops scheduling; a keepalive request
    // already handed to the browser is left alone.
    frozen = true;
    if (retryTimer !== 0) {
      win.clearTimeout(retryTimer);
      retryTimer = 0;
    }
  });

  // One command function with a read-only integer marker, installed before
  // the automatic view. Unknown commands and arguments are no-ops.
  var pacapulse = function (command) {
    if (command === "pageview") attempt("spa");
  };
  Object.defineProperty(pacapulse, "protocolVersion", { value: 1, enumerable: true });
  win.pacapulse = pacapulse;

  attempt("initial");
})();
