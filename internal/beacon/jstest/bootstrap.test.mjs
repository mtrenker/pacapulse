// Bootstrap, platform baseline, and duplicate-injection behaviour.
//
// Every case here must fail the same way: silently. Nothing is installed,
// nothing is thrown into the tracked page, no randomness is drawn, no URL
// field is read, and no request is made.

import assert from "node:assert/strict";
import test from "node:test";

import { ASSET_URL, COLLECTOR_ORIGIN, createPage, ENDPOINT, settle } from "./harness.mjs";

/** Asserts a page stopped without leaving any trace in the document. */
function assertStoppedSilently(page) {
  assert.equal(page.fetches.length, 0, "a request was sent");
  assert.equal(page.reads.fetch, 0, "fetch was called");
  assert.equal(page.reads.randomUUID + page.reads.getRandomValues, 0, "randomness was drawn");
  assert.equal(page.reads.pathname + page.reads.search + page.reads.referrer, 0, "a tracked URL field was read");
  assert.equal(page.listeners.window.count("pageshow"), 0, "a lifecycle listener was registered");
}

test("a valid installation installs the command and takes one view", async () => {
  const page = createPage({});
  await settle();

  assert.equal(typeof page.api, "function");
  assert.equal(page.api.protocolVersion, 1);
  assert.equal(page.fetches.length, 1);
  assert.equal(page.fetches[0].url, ENDPOINT);
});

test("the protocol version marker is a read-only integer", async () => {
  const page = createPage({});
  await settle();

  const descriptor = Object.getOwnPropertyDescriptor(page.api, "protocolVersion");
  assert.equal(descriptor.value, 1);
  assert.equal(Number.isInteger(descriptor.value), true);
  assert.equal(descriptor.writable, false);
  assert.equal(descriptor.configurable, false);

  assert.throws(() => {
    "use strict";
    page.api.protocolVersion = 2;
  }, TypeError);
  assert.equal(page.api.protocolVersion, 1);
});

test("the collector origin is derived from the script src, never hard-coded", async () => {
  for (const [src, endpoint] of [
    [ASSET_URL, `${COLLECTOR_ORIGIN}/api/v1/pageviews`],
    ["https://analytics.internal.example:8443/beacon/v1.js", "https://analytics.internal.example:8443/api/v1/pageviews"],
    ["http://localhost:8080/beacon/v1.js", "http://localhost:8080/api/v1/pageviews"]
  ]) {
    const page = createPage({ scriptSrc: src });
    await settle();
    assert.equal(page.fetches.length, 1, `no request for ${src}`);
    assert.equal(page.fetches[0].url, endpoint);
  }
});

test("malformed bootstrap input stops silently", async () => {
  const cases = {
    "no current script": { currentScript: null },
    "current script without getAttribute": { currentScript: { src: ASSET_URL } },
    "missing data-site": { dataSite: null },
    "empty data-site": { dataSite: "" },
    "wrong key prefix": { dataSite: "xx_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ" },
    "short key": { dataSite: "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPz" },
    "long key": { dataSite: "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQQ" },
    "key with a non-base64url character": { dataSite: "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPz+" },
    "key with surrounding whitespace": { dataSite: " pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ " },
    "missing src": { scriptSrc: "" },
    "relative src": { scriptSrc: "/beacon/v1.js" },
    "non-http src": { scriptSrc: "data:text/javascript,void 0" }
  };

  for (const [name, options] of Object.entries(cases)) {
    const page = createPage(options);
    await settle();
    assert.equal(page.api, undefined, `${name}: the API was installed`);
    assertStoppedSilently(page);
  }
});

test("an unsupported platform stops silently", async () => {
  const features = ["fetch", "Request", "AbortController", "AbortSignal", "URL", "URLSearchParams", "TextEncoder", "Promise", "JSON", "crypto"];

  for (const feature of features) {
    const page = createPage({ missingFeature: feature });
    await settle();
    assert.equal(page.api, undefined, `missing ${feature}: the API was installed`);
    assertStoppedSilently(page);
  }

  // Web Crypto present but with neither source of secure randomness.
  const noRandomness = createPage({ noRandomUUID: true, noGetRandomValues: true });
  await settle();
  assert.equal(noRandomness.api, undefined);
  assertStoppedSilently(noRandomness);
});

test("a marked instance makes a second injection a complete no-op", async () => {
  for (const version of [1, 2, 7]) {
    const existing = function () {};
    existing.protocolVersion = version;

    const page = createPage({ existingGlobal: existing });
    await settle();

    assert.equal(page.api, existing, `protocolVersion ${version}: the marker was replaced`);
    assertStoppedSilently(page);
  }
});

test("an unrelated existing global is preserved and collection stops", async () => {
  for (const value of [{ analytics: true }, "already taken", 42, null, function () {}]) {
    const page = createPage({ existingGlobal: value });
    await settle();

    assert.equal(page.api, value, "the tracked application's value was overwritten");
    assertStoppedSilently(page);
  }
});

test("unsupported commands and arguments are silent no-ops", async () => {
  const page = createPage({});
  await settle();
  assert.equal(page.fetches.length, 1);

  page.navigate("https://shop.example/next");
  for (const command of [undefined, null, "", "pageView", "event", 1, {}, ["pageview"], Symbol.iterator]) {
    assert.doesNotThrow(() => page.window.pacapulse(command));
  }
  page.window.pacapulse("pageview", { extra: "ignored" }, 3);
  await settle();

  // Only the one recognized command produced a request.
  assert.equal(page.fetches.length, 2);
});
