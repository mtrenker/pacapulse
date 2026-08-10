// A dependency-free browser harness for the production beacon.
//
// The asset is loaded from disk and evaluated in a node:vm context whose
// global object is a fake window. Nothing is stubbed inside the asset itself:
// the tests exercise the exact bytes the service serves, through the same
// globals a browser would supply.
//
// Everything the beacon can observe is deterministic here — the clock, secure
// randomness, and every fetch — so a test can prove ordering ("no UUID was
// generated") rather than only outcomes.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const HERE = dirname(fileURLToPath(import.meta.url));

export const ASSET_PATH = join(HERE, "..", "beacon.js");
export const ASSET_SOURCE = readFileSync(ASSET_PATH, "utf8");

export const COLLECTOR_ORIGIN = "https://pulse.pacabytes.io";
export const ASSET_URL = `${COLLECTOR_ORIGIN}/beacon/v1.js`;
export const ENDPOINT = `${COLLECTOR_ORIGIN}/api/v1/pageviews`;
export const DEFAULT_URL = "https://shop.example/";

/** A minimal EventTarget with the one option the beacon uses. */
class Listeners {
  #byType = new Map();

  addEventListener(type, handler, options) {
    if (!this.#byType.has(type)) this.#byType.set(type, []);
    this.#byType.get(type).push({ handler, once: Boolean(options && options.once) });
  }

  removeEventListener(type, handler) {
    const registered = this.#byType.get(type) ?? [];
    this.#byType.set(type, registered.filter((entry) => entry.handler !== handler));
  }

  count(type) {
    return (this.#byType.get(type) ?? []).length;
  }

  dispatch(type, event) {
    const registered = [...(this.#byType.get(type) ?? [])];
    this.#byType.set(type, registered.filter((entry) => !entry.once));
    for (const entry of registered) entry.handler(event);
  }
}

/** A deterministic replacement for setTimeout/clearTimeout. */
function createClock() {
  let now = 0;
  let nextID = 0;
  const timers = new Map();

  return {
    setTimeout(handler, delay) {
      const id = ++nextID;
      timers.set(id, { handler, at: now + Number(delay ?? 0) });
      return id;
    },
    clearTimeout(id) {
      timers.delete(id);
    },
    /** Runs every timer due within `ms`, in scheduled order. */
    advance(ms) {
      const until = now + ms;
      for (;;) {
        const due = [...timers.entries()]
          .filter(([, timer]) => timer.at <= until)
          .sort((a, b) => a[1].at - b[1].at || a[0] - b[0]);
        if (due.length === 0) break;
        const [id, timer] = due[0];
        timers.delete(id);
        now = timer.at;
        timer.handler();
      }
      now = until;
    },
    get pending() {
      return [...timers.values()].map((timer) => timer.at - now);
    },
    get now() {
      return now;
    }
  };
}

/** Lets pending host promise callbacks inside the asset run. */
export async function settle(rounds = 4) {
  for (let round = 0; round < rounds; round++) {
    await new Promise((resolve) => setImmediate(resolve));
  }
}

/**
 * Builds a page and runs the beacon in it.
 *
 * Options:
 *   url, referrer, dataSite, dataCollectDnt, scriptSrc, currentScript,
 *   prerendering, globalPrivacyControl, doNotTrack, windowDoNotTrack,
 *   msDoNotTrack, existingGlobal, missingFeature, keepalive, uuids,
 *   randomValue, poison, autoLoad
 */
export function createPage(options = {}) {
  const {
    url = DEFAULT_URL,
    referrer = "",
    dataSite = "pp_q3D9xK2mW7cN4rT8vB6yH1sL5aF0jPzQ",
    dataCollectDnt = null,
    scriptSrc = ASSET_URL,
    currentScript,
    prerendering = false,
    globalPrivacyControl,
    doNotTrack,
    windowDoNotTrack,
    msDoNotTrack,
    existingGlobal,
    missingFeature = null,
    keepalive = true,
    uuids,
    randomValue = 0,
    noRandomUUID = false,
    noGetRandomValues = false,
    poison = false,
    autoLoad = true
  } = options;

  const clock = createClock();
  const fetches = [];
  // Every privacy-sensitive read the asset can make is counted, so a
  // suppressed case can assert that none of them happened.
  const reads = { pathname: 0, search: 0, href: 0, referrer: 0, randomUUID: 0, getRandomValues: 0, fetch: 0 };

  /** Throws in "poison" mode, so an ordering violation fails loudly. */
  function read(name, value) {
    reads[name]++;
    if (poison) throw new Error(`the beacon read ${name} behind a privacy gate`);
    return value;
  }

  let location = new URL(url);
  const locationView = {
    get pathname() {
      return read("pathname", location.pathname);
    },
    get search() {
      return read("search", location.search);
    },
    get href() {
      return read("href", location.href);
    }
  };

  const uuidQueue = [...(uuids ?? [])];
  let uuidCounter = 0;
  const webcrypto = {
    randomUUID() {
      reads.randomUUID++;
      if (poison) throw new Error("the beacon generated a UUID behind a privacy gate");
      if (uuidQueue.length > 0) return uuidQueue.shift();
      uuidCounter++;
      return `550e8400-e29b-41d4-a716-${String(uuidCounter).padStart(12, "0")}`;
    },
    getRandomValues(array) {
      reads.getRandomValues++;
      if (poison) throw new Error("the beacon drew randomness behind a privacy gate");
      array.fill(randomValue);
      return array;
    }
  };
  if (noRandomUUID) delete webcrypto.randomUUID;
  if (noGetRandomValues) delete webcrypto.getRandomValues;

  const documentListeners = new Listeners();
  const windowListeners = new Listeners();

  const script = currentScript === undefined
    ? {
        src: scriptSrc,
        getAttribute(name) {
          if (name === "data-site") return dataSite;
          if (name === "data-collect-dnt") return dataCollectDnt;
          return null;
        }
      }
    : currentScript;

  const doc = {
    currentScript: script,
    prerendering,
    defaultView: null, // replaced with the window below
    get referrer() {
      return read("referrer", referrer);
    },
    addEventListener: documentListeners.addEventListener.bind(documentListeners),
    removeEventListener: documentListeners.removeEventListener.bind(documentListeners)
  };

  const navigator = { userAgent: "harness" };
  if (globalPrivacyControl !== undefined) navigator.globalPrivacyControl = globalPrivacyControl;
  if (doNotTrack !== undefined) navigator.doNotTrack = doNotTrack;
  if (msDoNotTrack !== undefined) navigator.msDoNotTrack = msDoNotTrack;

  // Request either has keepalive (the platform default) or a stub that lacks
  // it, which is how the unsupported-Keepalive path is exercised.
  const RequestStub = class {
    constructor(input) {
      this.url = String(input);
    }
  };

  const win = {
    document: doc,
    navigator,
    location: locationView,
    crypto: webcrypto,
    JSON,
    URL,
    URLSearchParams,
    TextEncoder,
    Promise,
    AbortController,
    AbortSignal,
    Object,
    Array,
    Uint8Array,
    Uint32Array,
    parseInt,
    Request: keepalive ? Request : RequestStub,
    fetch(input, init) {
      reads.fetch++;
      if (poison) throw new Error("the beacon reached the network behind a privacy gate");
      const call = { url: String(input), options: init, body: init && init.body, settled: false };
      call.promise = new Promise((resolve, reject) => {
        call.resolve = (value) => {
          call.settled = true;
          resolve(value);
        };
        call.reject = (error) => {
          call.settled = true;
          reject(error);
        };
      });
      if (init && init.signal) {
        init.signal.addEventListener("abort", () => {
          if (call.settled) return;
          const error = new Error("The operation was aborted.");
          error.name = "AbortError";
          call.reject(error);
        });
      }
      fetches.push(call);
      return call.promise;
    },
    setTimeout: clock.setTimeout,
    clearTimeout: clock.clearTimeout,
    addEventListener: windowListeners.addEventListener.bind(windowListeners),
    removeEventListener: windowListeners.removeEventListener.bind(windowListeners)
  };
  if (windowDoNotTrack !== undefined) win.doNotTrack = windowDoNotTrack;
  if (existingGlobal !== undefined) win.pacapulse = existingGlobal;
  win.window = win;
  doc.defaultView = win;

  vm.createContext(win);
  // Shadowed with undefined rather than deleted, and only after contextifying:
  // the context's own realm supplies the ECMAScript intrinsics, so deleting
  // JSON or Promise from the sandbox would just uncover the realm's copy.
  if (missingFeature !== null) win[missingFeature] = undefined;

  const page = {
    window: win,
    document: doc,
    navigator,
    clock,
    fetches,
    reads,
    listeners: { window: windowListeners, document: documentListeners },

    /** Evaluates the production asset in this page. */
    load() {
      vm.runInContext(ASSET_SOURCE, win, { filename: "beacon.js" });
      return page;
    },

    /** The installed public command, or undefined when nothing installed. */
    get api() {
      return win.pacapulse;
    },

    /** Moves the document to another URL without reloading. */
    navigate(next) {
      location = new URL(next, location);
      return page;
    },

    pageview() {
      win.pacapulse("pageview");
      return page;
    },

    pageshow(persisted) {
      windowListeners.dispatch("pageshow", { persisted });
      return page;
    },

    pagehide() {
      windowListeners.dispatch("pagehide", {});
      return page;
    },

    destroy() {
      doc.defaultView = null;
      return page;
    },

    activatePrerender() {
      doc.prerendering = false;
      documentListeners.dispatch("prerenderingchange", {});
      return page;
    },

    /** The parsed body of the nth captured request. */
    payload(index = 0) {
      return JSON.parse(fetches[index].body);
    },

    /** Resolves the nth captured request with an HTTP status. */
    async respond(index, status) {
      fetches[index].resolve({ status, ok: status >= 200 && status < 300 });
      await settle();
      return page;
    },

    /** Fails the nth captured request the way an opaque CORS error does. */
    async failNetwork(index) {
      fetches[index].reject(new TypeError("Failed to fetch"));
      await settle();
      return page;
    },

    /** Runs the clock far enough to trip the five-second request bound. */
    async timeOut(ms = 5000) {
      clock.advance(ms);
      await settle();
      return page;
    }
  };

  if (autoLoad) page.load();
  return page;
}
