// The browser privacy gate, and the ordering guarantee behind it.
//
// "No network activity" is the weak claim. The strong one is that a
// suppressed page view never reaches the point of *having* anything to send:
// no UUID exists, no pathname, query, or referrer was read. The `poison`
// harness mode turns each of those reads into a thrown error, so a gate that
// runs one step too late fails loudly instead of passing on the outcome.

import assert from "node:assert/strict";
import test from "node:test";

import { createPage, settle } from "./harness.mjs";

/** Runs a suppressed case with every tracked read poisoned. */
async function suppressed(options) {
  const page = createPage({ ...options, poison: true, autoLoad: false });
  assert.doesNotThrow(() => page.load(), "the beacon acted before its privacy gate");
  await settle();

  assert.equal(page.fetches.length, 0, "a request was sent");
  assert.equal(page.reads.fetch, 0, "fetch was called");
  assert.equal(page.reads.randomUUID + page.reads.getRandomValues, 0, "a UUID or jitter was generated");
  assert.equal(page.reads.pathname, 0, "the pathname was read");
  assert.equal(page.reads.search, 0, "the query string was read");
  assert.equal(page.reads.href, 0, "the full URL was read");
  assert.equal(page.reads.referrer, 0, "the referrer was read");
  return page;
}

test("GPC suppresses before randomness, URL access, and the network", async () => {
  await suppressed({ globalPrivacyControl: true });
});

test("GPC cannot be overridden by the snippet attribute", async () => {
  await suppressed({ globalPrivacyControl: true, dataCollectDnt: "true" });
});

test("only the exact boolean true enables the GPC gate", async () => {
  for (const value of ["true", 1, "1", {}]) {
    const page = createPage({ globalPrivacyControl: value });
    await settle();
    assert.equal(page.fetches.length, 1, `globalPrivacyControl ${String(value)} suppressed collection`);
  }
});

test("DNT suppresses by default through every exposed property", async () => {
  await suppressed({ doNotTrack: "1" });
  await suppressed({ doNotTrack: "yes" });
  await suppressed({ windowDoNotTrack: "1" });
  await suppressed({ windowDoNotTrack: "yes" });
  await suppressed({ msDoNotTrack: "1" });
});

test("DNT values that do not mean enabled collect normally", async () => {
  for (const value of ["0", "unspecified", null, "", "2", "YES"]) {
    const page = createPage({ doNotTrack: value });
    await settle();
    assert.equal(page.fetches.length, 1, `doNotTrack ${JSON.stringify(value)} suppressed collection`);
    assert.equal("dnt" in page.payload(0), false, "dnt was serialized without DNT being enabled");
  }
});

test("the explicit override sends the one-way DNT field", async () => {
  const page = createPage({ doNotTrack: "1", dataCollectDnt: "true" });
  await settle();

  assert.equal(page.fetches.length, 1);
  assert.equal(page.payload(0).dnt, true);
});

test("no other attribute value enables the override", async () => {
  for (const value of ["TRUE", "True", "1", "yes", "", " true", "true ", null]) {
    await suppressed({ doNotTrack: "1", dataCollectDnt: value });
  }
});

test("the override never adds a DNT field when DNT is off", async () => {
  const page = createPage({ dataCollectDnt: "true" });
  await settle();

  assert.equal(page.fetches.length, 1);
  assert.equal("dnt" in page.payload(0), false);
});

test("the gate runs again for explicit SPA views", async () => {
  const page = createPage({});
  await settle();
  assert.equal(page.fetches.length, 1);

  page.navigator.globalPrivacyControl = true;
  page.navigate("https://shop.example/after-consent-change").pageview();
  await settle();

  assert.equal(page.fetches.length, 1, "an SPA view was sent after GPC was switched on");
});

test("the gate runs again for bfcache restoration", async () => {
  const page = createPage({});
  await settle();
  assert.equal(page.fetches.length, 1);

  page.navigator.doNotTrack = "1";
  page.pageshow(true);
  await settle();

  assert.equal(page.fetches.length, 1, "a restored document was collected after DNT was switched on");
});

test("the gate runs again at prerender activation", async () => {
  const page = createPage({ prerendering: true });
  await settle();
  assert.equal(page.fetches.length, 0);

  page.navigator.globalPrivacyControl = true;
  page.activatePrerender();
  await settle();

  assert.equal(page.fetches.length, 0, "activation collected despite GPC");
});
