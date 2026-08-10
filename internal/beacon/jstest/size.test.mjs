// The asset size gate.
//
// This runs in Node because Node has a Brotli encoder and Go's standard
// library does not. The protocol requires CI to measure all three
// representations; it separately permits the service itself to satisfy
// ordinary negotiation with gzip and identity, which is what it does.
//
// The bytes measured here are the exact bytes the Go binary embeds and
// serves — beacon.js itself, with no build step in between.

import assert from "node:assert/strict";
import test from "node:test";
import zlib from "node:zlib";

import { ASSET_PATH, ASSET_SOURCE } from "./harness.mjs";

const MAX_SOURCE_BYTES = 12 * 1024;
const MAX_COMPRESSED_BYTES = 4 * 1024;

const source = Buffer.from(ASSET_SOURCE, "utf8");

const gzip = zlib.gzipSync(source, { level: zlib.constants.Z_BEST_COMPRESSION });
const brotli = zlib.brotliCompressSync(source, {
  params: {
    [zlib.constants.BROTLI_PARAM_QUALITY]: zlib.constants.BROTLI_MAX_QUALITY,
    [zlib.constants.BROTLI_PARAM_SIZE_HINT]: source.length
  }
});

test(`${ASSET_PATH} is within its size budget`, () => {
  const report = [
    ["source", source.length, MAX_SOURCE_BYTES],
    ["gzip", gzip.length, MAX_COMPRESSED_BYTES],
    ["brotli", brotli.length, MAX_COMPRESSED_BYTES]
  ];

  for (const [name, size, budget] of report) {
    console.log(`  beacon.js ${name}: ${size} bytes of ${budget} (${budget - size} to spare)`);
  }
  for (const [name, size, budget] of report) {
    assert.ok(size <= budget, `${name} representation is ${size} bytes, over the ${budget} byte budget`);
  }
});

test("both compressed representations decode to the production source", () => {
  assert.equal(zlib.gunzipSync(gzip).toString("utf8"), ASSET_SOURCE);
  assert.equal(zlib.brotliDecompressSync(brotli).toString("utf8"), ASSET_SOURCE);
});
