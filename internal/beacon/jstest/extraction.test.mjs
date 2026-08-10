// Every browser_extraction fixture, driven straight from the shared JSON.
//
// The runner interprets the fixture vocabulary described in
// docs/architecture/collection-protocol.md — kind, lifecycle_event,
// previous_route_key_fields, send, network, omitted, wait_for,
// fresh_event_id, reset_route_key, install_api — so adding a case to the
// fixture file adds a case here without touching this test.

import assert from "node:assert/strict";
import test from "node:test";

import { browserCases, OPTIONAL_FIELDS, urlForRouteKey } from "./fixtures.mjs";
import { createPage, DEFAULT_URL, settle } from "./harness.mjs";

const UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** A pre-existing global that is recognizable after the asset has run. */
function existingGlobalFor(input) {
  if (input.existing_pacapulse_version === undefined) return undefined;
  const existing = function () {};
  existing.protocolVersion = input.existing_pacapulse_version;
  existing.installedByTheFixture = true;
  return existing;
}

/**
 * Sets a page up so that the attempt the fixture describes is the *next*
 * thing that happens, and performs it.
 *
 * The document has to reach a plausible prior state first: a SPA route change
 * only means something after some other route, and a bfcache restoration only
 * after an initial view. `baseline` records how many requests that setup made
 * so the fixture's own assertions can look past them.
 */
async function runFixture(fixture) {
  const input = fixture.input;
  const url = input.url ?? DEFAULT_URL;
  const previous = input.previous_route_key_fields;
  const origin = new URL(url).origin;

  // A prerender-activation fixture describes the state *at* the event, so the
  // document starts prerendering and is activated below.
  const prerendering = input.document_prerendering === true || input.lifecycle_event === "prerenderingchange";

  let startURL = url;
  if (previous) {
    startURL = urlForRouteKey(previous, origin);
  } else if (input.kind === "spa") {
    startURL = `${origin}/pacapulse-fixture-start`;
  }

  const existing = existingGlobalFor(input);
  const page = createPage({
    url: startURL,
    referrer: input.document_referrer ?? "",
    dataCollectDnt: input.data_collect_dnt ?? null,
    prerendering,
    globalPrivacyControl: input.global_privacy_control,
    doNotTrack: input.do_not_track,
    existingGlobal: existing,
    autoLoad: false
  });

  if (input.kind === "spa" || input.kind === "pageshow" || previous) {
    // The setup navigation is the automatic initial view.
    page.load();
    await settle();
  }
  const baseline = page.fetches.length;

  switch (input.kind) {
    case "spa":
      page.navigate(url).pageview();
      break;
    case "pageshow":
      page.pageshow(input.persisted === true);
      break;
    default:
      // "initial" and "script_load": running the asset is the attempt.
      page.load();
      await settle();
      if (input.lifecycle_event === "prerenderingchange") page.activatePrerender();
      break;
  }
  await settle();

  return { page, existing, baseline, sent: page.fetches.length - baseline };
}

for (const fixture of browserCases) {
  test(`fixture: ${fixture.name}`, async () => {
    const expected = fixture.expected;
    const { page, existing, baseline, sent } = await runFixture(fixture);

    if (expected.network === false) {
      assert.equal(page.reads.fetch, 0, "the fixture forbids all network activity");
    }
    if (expected.wait_for === "prerenderingchange") {
      assert.equal(
        page.listeners.document.count("prerenderingchange"),
        1,
        "exactly one prerenderingchange listener must be armed"
      );
    }
    if (expected.install_api === false) {
      assert.equal(page.api, existing, "the pre-existing global was replaced");
      assert.equal(page.api.installedByTheFixture, true);
    }
    if (expected.send === false) {
      assert.equal(sent, 0, "the fixture forbids a request but one was sent");
      return;
    }

    assert.equal(sent, 1, "the fixture expects exactly one request");
    const payload = page.payload(baseline);

    assert.equal(payload.v, 1);
    assert.equal(payload.site, "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ");
    assert.match(payload.event_id, UUID_V4);

    if (expected.path !== undefined) assert.equal(payload.path, expected.path);
    for (const field of OPTIONAL_FIELDS) {
      if (expected[field] !== undefined) assert.equal(payload[field], expected[field]);
    }
    for (const field of expected.omitted ?? []) {
      assert.equal(field in payload, false, `${field} must not be serialized`);
    }
    for (const [field, value] of Object.entries(expected.payload_fields ?? {})) {
      assert.equal(payload[field], value);
    }
    if (expected.fresh_event_id === true) {
      assert.notEqual(
        payload.event_id,
        page.payload(baseline - 1).event_id,
        "a restored document must use a new event UUID"
      );
    }
    if (expected.reset_route_key === true) {
      // The setup view used this exact route, so a request happening at all
      // is what proves the key was cleared before the attempt.
      assert.equal(payload.path, page.payload(baseline - 1).path);
    }

    // route_key_fields is asserted behaviourally: replaying the tuple must be
    // suppressed, and changing the path must not be.
    if (expected.route_key_fields !== undefined) {
      const fields = expected.route_key_fields;
      const origin = new URL(fixture.input.url ?? DEFAULT_URL).origin;

      page.navigate(urlForRouteKey(fields, origin)).pageview();
      await settle();
      assert.equal(
        page.fetches.length,
        baseline + 1,
        `replaying route key ${JSON.stringify(fields)} was not suppressed`
      );

      page.navigate(urlForRouteKey([`${fields[0]}changed`, ...fields.slice(1)], origin)).pageview();
      await settle();
      assert.equal(page.fetches.length, baseline + 2, "a different route key must not be suppressed");
    }
  });
}

// The fixture list is contract evidence; a truncated file would make every
// generated case above vacuously pass.
test("the fixture file supplies browser cases", () => {
  assert.ok(browserCases.length >= 13, `only ${browserCases.length} browser fixtures were loaded`);
});
