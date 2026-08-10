// The single permitted retry.
//
// The rule this file exists to defend is an anti-amplification one: exactly
// the outcomes a legitimate installation can recover from — the client's own
// five-second bound, and a CORS-visible 429 or 503 — get one replay. Every
// silent rejection path (unknown key, disabled site, rejected origin, early
// body or media-type refusal, instance limiting) reaches the browser as an
// opaque TypeError, and retrying those would double exactly the traffic the
// collector already decided not to accept.

import assert from "node:assert/strict";
import test from "node:test";

import { createPage, settle } from "./harness.mjs";

/** Loads a page and returns it once its initial request is captured. */
async function pageWithRequest(options = {}) {
  const page = createPage(options);
  await settle();
  assert.equal(page.fetches.length, 1);
  return page;
}

/** Fires whatever retry timer is pending. */
async function runRetryTimer(page) {
  page.clock.advance(1000);
  await settle();
}

for (const status of [429, 503]) {
  test(`a visible ${status} is retried once with a byte-identical request`, async () => {
    const page = await pageWithRequest();
    const original = page.fetches[0];

    await page.respond(0, status);
    assert.equal(page.fetches.length, 1, "the retry must wait out its jitter");
    await runRetryTimer(page);

    assert.equal(page.fetches.length, 2, `a visible ${status} was not retried`);
    const replay = page.fetches[1];
    assert.equal(replay.body, original.body, "the retry body is not byte-identical");
    assert.equal(JSON.parse(replay.body).event_id, JSON.parse(original.body).event_id);
    assert.equal(replay.url, original.url);
    assert.equal(replay.options.method, "POST");
    assert.equal(replay.options.mode, "cors");
    assert.equal(replay.options.credentials, "omit");
    assert.equal(replay.options.referrerPolicy, "no-referrer");

    // And no further replay, whatever the retry itself observes.
    await page.respond(1, status);
    await runRetryTimer(page);
    assert.equal(page.fetches.length, 2, "the beacon retried more than once");
  });
}

test("the five-second timeout is retried once with the same body", async () => {
  const page = await pageWithRequest();
  const original = page.fetches[0].body;

  await page.timeOut();
  assert.equal(page.fetches.length, 1, "the retry must wait out its jitter");
  await runRetryTimer(page);

  assert.equal(page.fetches.length, 2);
  assert.equal(page.fetches[1].body, original);

  await page.timeOut();
  await runRetryTimer(page);
  assert.equal(page.fetches.length, 2, "a timed-out retry was retried again");
});

test("terminal statuses are never retried", async () => {
  for (const status of [204, 400, 413, 415, 401, 404, 500, 502]) {
    const page = await pageWithRequest();
    await page.respond(0, status);
    await runRetryTimer(page);

    assert.equal(page.fetches.length, 1, `status ${status} was retried`);
    assert.deepEqual(page.clock.pending, [], `status ${status} left a timer pending`);
  }
});

test("a generic TypeError is final, so rejected traffic cannot double itself", async () => {
  const page = await pageWithRequest();
  await page.failNetwork(0);
  await runRetryTimer(page);

  assert.equal(page.fetches.length, 1, "an opaque CORS or network failure was retried");
  assert.deepEqual(page.clock.pending, []);
});

test("the retry delay stays inside the 250-750 ms window", async () => {
  for (const [randomValue, expected] of [[0, 250], [250, 500], [500, 750]]) {
    const page = await pageWithRequest({ randomValue });
    await page.respond(0, 503);

    assert.deepEqual(page.clock.pending, [expected], `randomValue ${randomValue}`);
    assert.ok(expected >= 250 && expected <= 750);
  }
});

test("jitter comes from Web Crypto, not from Math.random", async () => {
  const page = await pageWithRequest();
  const before = page.reads.getRandomValues;

  await page.respond(0, 503);

  assert.equal(page.reads.getRandomValues, before + 1, "the retry delay did not draw secure randomness");
});

test("pagehide cancels a scheduled retry", async () => {
  const page = await pageWithRequest();
  await page.respond(0, 503);
  assert.equal(page.clock.pending.length, 1);

  page.pagehide();
  assert.deepEqual(page.clock.pending, [], "the pending retry survived pagehide");

  await runRetryTimer(page);
  assert.equal(page.fetches.length, 1);
});

test("a destroyed document does not fire its scheduled retry", async () => {
  const page = await pageWithRequest();
  await page.respond(0, 503);
  assert.equal(page.clock.pending.length, 1);

  page.destroy();
  await runRetryTimer(page);

  assert.equal(page.fetches.length, 1, "a destroyed document sent a retry");
});

test("an inactive document schedules no retry at all", async () => {
  const page = await pageWithRequest();
  page.destroy();

  await page.respond(0, 429);

  assert.deepEqual(page.clock.pending, [], "a retry was scheduled for an inactive document");
  assert.equal(page.fetches.length, 1);
});

test("a restored document may retry again for its own new attempt", async () => {
  const page = await pageWithRequest();
  page.pagehide();
  page.pageshow(true);
  await settle();

  assert.equal(page.fetches.length, 2, "the restoration did not create its own attempt");
  await page.respond(1, 503);
  await runRetryTimer(page);

  assert.equal(page.fetches.length, 3, "a restored document could not retry its own attempt");
  assert.equal(page.fetches[2].body, page.fetches[1].body);
});

test("each route attempt gets its own retry budget", async () => {
  const page = await pageWithRequest({ url: "https://shop.example/a" });
  await page.respond(0, 503);
  await runRetryTimer(page);
  assert.equal(page.fetches.length, 2);

  page.navigate("https://shop.example/b").pageview();
  await settle();
  assert.equal(page.fetches.length, 3);

  await page.respond(2, 503);
  await runRetryTimer(page);
  assert.equal(page.fetches.length, 4, "a new route attempt inherited the previous one's spent retry");
  assert.notEqual(JSON.parse(page.fetches[3].body).event_id, JSON.parse(page.fetches[1].body).event_id);
});
