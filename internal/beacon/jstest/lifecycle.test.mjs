// Document lifecycle and in-document duplicate suppression.

import assert from "node:assert/strict";
import test from "node:test";

import { createPage, settle } from "./harness.mjs";

const paths = (page) => page.fetches.map((call) => JSON.parse(call.body).path);

test("script execution takes exactly one automatic view", async () => {
  const page = createPage({ url: "https://shop.example/pricing" });
  await settle();

  assert.deepEqual(paths(page), ["/pricing"]);
  assert.equal(page.payload(0).referrer_host, undefined);
});

test("a to b to a records three attempts with three event UUIDs", async () => {
  const page = createPage({ url: "https://shop.example/a" });
  await settle();
  page.navigate("https://shop.example/b").pageview();
  await settle();
  page.navigate("https://shop.example/a").pageview();
  await settle();

  assert.deepEqual(paths(page), ["/a", "/b", "/a"]);
  const ids = page.fetches.map((call) => JSON.parse(call.body).event_id);
  assert.equal(new Set(ids).size, 3, "each attempt needs its own event UUID");
});

test("a repeated callback for the same route is suppressed", async () => {
  const page = createPage({ url: "https://shop.example/a" });
  await settle();

  page.navigate("https://shop.example/b").pageview();
  page.pageview();
  page.pageview();
  await settle();

  assert.deepEqual(paths(page), ["/a", "/b"]);
});

test("the framework's initial callback does not duplicate the automatic view", async () => {
  const page = createPage({ url: "https://shop.example/dashboard" });
  await settle();
  page.pageview();
  await settle();

  assert.deepEqual(paths(page), ["/dashboard"]);
});

test("a hash-only or unrelated-query change is not a new route", async () => {
  const page = createPage({ url: "https://shop.example/docs?utm_source=news" });
  await settle();

  for (const next of [
    "https://shop.example/docs?utm_source=news#install",
    "https://shop.example/docs?utm_source=news#usage",
    "https://shop.example/docs?utm_source=news&session=abc123",
    "https://shop.example/docs?session=changed&utm_source=news"
  ]) {
    page.navigate(next).pageview();
  }
  await settle();

  assert.equal(page.fetches.length, 1);
});

test("a campaign change is a new route", async () => {
  const page = createPage({ url: "https://shop.example/offer?utm_source=news" });
  await settle();
  page.navigate("https://shop.example/offer?utm_source=social").pageview();
  await settle();

  assert.equal(page.fetches.length, 2);
  assert.equal(page.payload(1).utm_source, "social");
});

test("trailing slashes and repeated slashes stay distinct routes", async () => {
  const page = createPage({ url: "https://shop.example/pricing" });
  await settle();
  page.navigate("https://shop.example/pricing/").pageview();
  await settle();
  page.navigate("https://shop.example//pricing").pageview();
  await settle();

  assert.deepEqual(paths(page), ["/pricing", "/pricing/", "//pricing"]);
});

test("an ordinary pageshow is ignored", async () => {
  const page = createPage({});
  await settle();
  page.pageshow(false);
  page.pageshow(undefined);
  await settle();

  assert.equal(page.fetches.length, 1);
});

test("bfcache restoration recounts the same path and drops the referrer", async () => {
  const page = createPage({ url: "https://shop.example/account", referrer: "https://search.example/results" });
  await settle();
  assert.equal(page.payload(0).referrer_host, "search.example");

  page.pageshow(true);
  await settle();

  assert.equal(page.fetches.length, 2);
  assert.equal(page.payload(1).path, "/account");
  assert.equal("referrer_host" in page.payload(1), false);
  assert.notEqual(page.payload(1).event_id, page.payload(0).event_id);
});

test("bfcache restoration after a route change recounts the restored route", async () => {
  const page = createPage({ url: "https://shop.example/a" });
  await settle();
  page.navigate("https://shop.example/b").pageview();
  await settle();

  page.pagehide();
  page.pageshow(true);
  await settle();

  assert.deepEqual(paths(page), ["/a", "/b", "/b"]);
});

test("a prerendered document that is never activated records nothing", async () => {
  const page = createPage({ prerendering: true });
  page.pageview();
  page.pageview();
  await settle();

  assert.equal(page.fetches.length, 0);
  assert.equal(page.reads.fetch, 0);
  assert.equal(page.listeners.document.count("prerenderingchange"), 1);
});

test("every deferred attempt collapses into one view at activation", async () => {
  const page = createPage({ url: "https://shop.example/one", prerendering: true, referrer: "https://search.example/q" });
  page.pageview();
  page.navigate("https://shop.example/two").pageview();
  page.navigate("https://shop.example/three").pageview();
  await settle();
  assert.equal(page.fetches.length, 0);

  page.activatePrerender();
  await settle();

  assert.deepEqual(paths(page), ["/three"], "activation must report the current route once");
  assert.equal(page.payload(0).referrer_host, "search.example");
});

test("SPA views after activation behave normally", async () => {
  const page = createPage({ url: "https://shop.example/one", prerendering: true });
  await settle();
  page.activatePrerender();
  await settle();

  page.navigate("https://shop.example/two").pageview();
  await settle();

  assert.deepEqual(paths(page), ["/one", "/two"]);
  assert.equal("referrer_host" in page.payload(1), false);
});

test("an invalid pathname produces no attempt", async () => {
  const page = createPage({ url: "https://shop.example/", autoLoad: false });
  // A pathname the URL parser would never produce, but the beacon must still
  // refuse rather than send an uncanonicalizable mandatory field.
  Object.defineProperty(page.window.location, "pathname", { get: () => "/bad/%zz" });
  page.load();
  await settle();

  assert.equal(page.fetches.length, 0);
  assert.equal(typeof page.api, "function", "the API is still installed for later routes");
});

test("an oversized pathname produces no attempt", async () => {
  const page = createPage({ url: `https://shop.example/${"a".repeat(512)}` });
  await settle();

  assert.equal(page.fetches.length, 0);
});

test("a 512-byte pathname is still collected", async () => {
  const page = createPage({ url: `https://shop.example/${"a".repeat(511)}` });
  await settle();

  assert.equal(page.fetches.length, 1);
  assert.equal(page.payload(0).path.length, 512);
});
