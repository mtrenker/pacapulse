// The request the beacon makes, and the payload it is allowed to contain.

import assert from "node:assert/strict";
import test from "node:test";

import { createPage, ENDPOINT, settle } from "./harness.mjs";

const ALLOWED_MEMBERS = new Set([
  "v", "site", "event_id", "path", "referrer_host", "utm_source", "utm_medium", "utm_campaign", "dnt"
]);

test("the fetch call matches the protocol exactly", async () => {
  const page = createPage({ url: "https://shop.example/pricing" });
  await settle();

  const { url, options } = page.fetches[0];
  assert.equal(url, ENDPOINT);
  assert.deepEqual(
    new Set(Object.keys(options)),
    new Set(["method", "mode", "credentials", "referrerPolicy", "headers", "body", "signal", "keepalive"])
  );
  assert.equal(options.method, "POST");
  assert.equal(options.mode, "cors");
  assert.equal(options.credentials, "omit");
  assert.equal(options.referrerPolicy, "no-referrer");
  assert.equal(options.keepalive, true);
  // Spread first: the options object is built inside the page's realm, so a
  // strict deep comparison would otherwise fail on the prototype alone.
  assert.deepEqual({ ...options.headers }, { "Content-Type": "text/plain;charset=UTF-8" });
  assert.equal(typeof options.signal.aborted, "boolean");
});

test("keepalive is omitted rather than falsified when unsupported", async () => {
  const page = createPage({ keepalive: false });
  await settle();

  const { options } = page.fetches[0];
  assert.equal("keepalive" in options, false, "an unsupported option must not be sent at all");
  assert.equal(options.mode, "cors");
  assert.equal(options.method, "POST");
});

test("the body carries only documented members", async () => {
  const page = createPage({
    url: "https://shop.example/pricing?utm_source=news&utm_medium=email&utm_campaign=spring&gclid=xyz&q=secret",
    referrer: "https://search.example/results?q=private",
    doNotTrack: "1",
    dataCollectDnt: "true"
  });
  await settle();

  const payload = page.payload(0);
  for (const member of Object.keys(payload)) {
    assert.ok(ALLOWED_MEMBERS.has(member), `undocumented payload member ${member}`);
  }
  assert.deepEqual(payload, {
    v: 1,
    site: "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ",
    event_id: payload.event_id,
    path: "/pricing",
    referrer_host: "search.example",
    utm_source: "news",
    utm_medium: "email",
    utm_campaign: "spring",
    dnt: true
  });

  const raw = page.fetches[0].body;
  assert.equal(typeof raw, "string");
  assert.equal(raw.includes("secret"), false, "a non-allowlisted query value crossed the network");
  assert.equal(raw.includes("gclid"), false, "a click identifier crossed the network");
  assert.equal(raw.includes("/results"), false, "the referrer path crossed the network");
  assert.equal(raw.includes("https://"), false, "a full URL crossed the network");
  assert.equal(raw.includes(page.navigator.userAgent), false, "the user agent crossed the network");
});

test("optional members are omitted rather than serialized as null", async () => {
  const page = createPage({ url: "https://shop.example/" });
  await settle();

  assert.equal(page.fetches[0].body, JSON.stringify({
    v: 1,
    site: "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ",
    event_id: page.payload(0).event_id,
    path: "/"
  }));
});

test("oversized and unusable optional values drop only their own dimension", async () => {
  const long = "x".repeat(101);
  const page = createPage({
    url: `https://shop.example/keep?utm_source=${long}&utm_medium=ok`,
    referrer: `https://${"a".repeat(250)}.example/`
  });
  await settle();

  const payload = page.payload(0);
  assert.equal(payload.path, "/keep");
  assert.equal(payload.utm_medium, "ok");
  assert.equal("utm_source" in payload, false, "an oversized campaign value was sent");
  assert.equal("referrer_host" in payload, false, "an oversized referrer host was sent");
});

test("a 100-byte campaign value is still collected", async () => {
  const page = createPage({ url: `https://shop.example/?utm_source=${"x".repeat(100)}` });
  await settle();

  assert.equal(page.payload(0).utm_source.length, 100);
});

test("non-http referrers and unparseable referrers are omitted", async () => {
  for (const referrer of ["about:blank", "android-app://com.example", "not a url", "", "ftp://files.example/x"]) {
    const page = createPage({ referrer });
    await settle();
    assert.equal("referrer_host" in page.payload(0), false, `referrer ${JSON.stringify(referrer)} produced a host`);
  }
});

test("campaign values are NFC-normalized, trimmed, and lowercased", async () => {
  // U+0041 U+0301 (A + combining acute) must normalize to U+00C1, then
  // lowercase to U+00E1, so both spellings aggregate as one campaign.
  const page = createPage({ url: "https://shop.example/?utm_campaign=%20%41%CC%81ction%20" });
  await settle();

  assert.equal(page.payload(0).utm_campaign, "áction");
});

test("the event UUID falls back to getRandomValues when randomUUID is absent", async () => {
  const page = createPage({ noRandomUUID: true, randomValue: 0 });
  await settle();

  assert.equal(page.reads.getRandomValues >= 1, true);
  assert.equal(page.payload(0).event_id, "00000000-0000-4000-8000-000000000000");
});

test("the five-second bound aborts the request", async () => {
  const page = createPage({});
  await settle();

  assert.deepEqual(page.clock.pending, [5000]);
  assert.equal(page.fetches[0].options.signal.aborted, false);

  page.clock.advance(4999);
  assert.equal(page.fetches[0].options.signal.aborted, false);
  page.clock.advance(1);
  assert.equal(page.fetches[0].options.signal.aborted, true);
});

test("a settled response cancels the timeout", async () => {
  const page = createPage({});
  await settle();
  await page.respond(0, 204);

  assert.deepEqual(page.clock.pending, [], "the abort timer outlived the response");
  assert.equal(page.fetches[0].options.signal.aborted, false);
});
