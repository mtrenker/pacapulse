// The shared protocol fixtures.
//
// docs/architecture/collection-protocol-fixtures.json is contract evidence for
// both the Go collector and this beacon. Loading it here rather than
// transcribing cases into JavaScript is the point: a fixture the two
// implementations disagree about has to be resolved in the protocol, not in
// one side's test file.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));

export const FIXTURES_PATH = join(HERE, "..", "..", "..", "docs", "architecture", "collection-protocol-fixtures.json");

export const fixtures = JSON.parse(readFileSync(FIXTURES_PATH, "utf8"));

/** All browser-extraction cases, which is what this asset is accountable for. */
export const browserCases = fixtures.browser_extraction;

/** Looks a case up by its fixture name, failing loudly if it was renamed. */
export function browserCase(name) {
  const found = browserCases.find((entry) => entry.name === name);
  if (!found) {
    throw new Error(`browser fixture ${JSON.stringify(name)} is missing from ${FIXTURES_PATH}`);
  }
  return found;
}

/** The optional payload members a fixture may pin. */
export const OPTIONAL_FIELDS = ["referrer_host", "utm_source", "utm_medium", "utm_campaign"];

/**
 * Rebuilds a URL that produces the given [path, source, medium, campaign]
 * route-key tuple, so a fixture's route_key_fields can be asserted by
 * behaviour instead of by reaching into the asset's private state.
 */
export function urlForRouteKey(fields, origin = "https://shop.example") {
  const [path, source, medium, campaign] = fields;
  const query = new URLSearchParams();
  if (source) query.set("utm_source", source);
  if (medium) query.set("utm_medium", medium);
  if (campaign) query.set("utm_campaign", campaign);
  const search = query.toString();
  return origin + path + (search ? `?${search}` : "");
}
