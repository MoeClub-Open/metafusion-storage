import { test } from "node:test";
import assert from "node:assert/strict";
import { connectAdminWorkspace, prepareAdminNavigation, isAdminWorkspaceEmbedded, announceAdminNavigation, requestAdminWorkspaceLogin } from "../src/lib/adminWorkspace.ts";

test("embedded workspace validates its host, synchronizes preferences and cleans up", (context) => {
  const originals = Object.fromEntries(["window", "document", "Element", "HTMLAnchorElement"].map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  context.after(() => {
    for (const [key, descriptor] of Object.entries(originals)) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else delete globalThis[key];
    }
  });
  const messages = [], locales = [], exits = [], attributes = new Map(), windowEvents = new Map(), documentEvents = new Map();
  const parent = { location: { origin: "https://site.invalid", pathname: "/admin", assign: (url) => exits.push(url) }, postMessage: (message, origin) => messages.push({ message, origin }) };
  const windowMock = {
    name: "metafusion-admin-storage", parent,
    location: { origin: "https://site.invalid", pathname: "/admin/storage/", search: "?q=a", hash: "#section", href: "https://site.invalid/admin/storage/?q=a#section" },
    addEventListener: (name, callback) => windowEvents.set(name, callback),
    removeEventListener: (name) => windowEvents.delete(name),
  };
  class MockElement {}
  class MockAnchor extends MockElement {
    constructor(href, target = "") { super(); this.href = href; this.target = target; }
    closest() { return this; }
    hasAttribute() { return false; }
  }
  Object.assign(globalThis, {
    window: windowMock, Element: MockElement, HTMLAnchorElement: MockAnchor,
    document: { documentElement: { setAttribute: (key, value) => attributes.set(key, value), classList: { toggle() {} }, style: {} },
      addEventListener: (name, callback) => documentEvents.set(name, callback), removeEventListener: (name) => documentEvents.delete(name) },
  });
  assert.equal(isAdminWorkspaceEmbedded(), true);
  windowMock.name = "other";
  assert.equal(connectAdminWorkspace((value) => locales.push(value)), null);
  windowMock.name = "metafusion-admin-storage";
  parent.location.origin = "https://foreign.invalid";
  assert.equal(isAdminWorkspaceEmbedded(), false);
  parent.location.origin = "https://site.invalid";
  parent.location.pathname = "/home";
  assert.equal(isAdminWorkspaceEmbedded(), false);
  parent.location.pathname = "/admin";
  const disconnect = connectAdminWorkspace((value) => locales.push(value));
  assert.ok(disconnect);
  assert.deepEqual(messages[0], { message: { channel: "metafusion.admin.v1", workspace: "storage", type: "ready", history: "replace", href: "/admin/storage/?q=a#section" }, origin: "https://site.invalid" });
  const receive = windowEvents.get("message");
  const data = { channel: "metafusion.admin.v1", workspace: "storage", type: "preferences", locale: "ja-JP", mode: "light", accent: "blue", tone: "slate" };
  const event = { source: parent, origin: "https://site.invalid", data };
  receive({ ...event, source: {} });
  receive({ ...event, origin: "https://foreign.invalid" });
  receive({ ...event, data: { ...data, workspace: "other" } });
  receive({ ...event, data: { ...data, locale: "unsupported" } });
  assert.equal(locales.length, 0);
  receive(event);
  assert.deepEqual(locales, ["ja-JP"]);
  assert.equal(attributes.get("data-theme-mode"), "light");
  assert.equal(attributes.get("data-theme-accent"), "blue");
  assert.equal(attributes.get("data-theme-tone"), "slate");
  announceAdminNavigation();
  assert.equal(messages.at(-1).message.type, "navigate");
  assert.equal(messages.at(-1).message.history, "replace");
  prepareAdminNavigation("/admin/storage/next");
  windowMock.location.pathname = "/admin/storage/next/";
  windowMock.location.search = ""; windowMock.location.hash = "";
  announceAdminNavigation();
  assert.equal(messages.at(-1).message.history, "push");
  announceAdminNavigation();
  assert.equal(messages.at(-1).message.history, "replace");
  assert.equal(requestAdminWorkspaceLogin(), true);
  assert.equal(messages.at(-1).message.type, "login");
  const click = (href, target = "") => {
    const event = { target: new MockAnchor(href, target), button: 0, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
    documentEvents.get("click")(event);
    return event.defaultPrevented;
  };
  assert.equal(click("https://site.invalid/admin/storage/page/"), false);
  assert.equal(click("https://site.invalid/catalog/fixture"), true);
  assert.deepEqual(exits, ["https://site.invalid/catalog/fixture"]);
  assert.equal(click("https://site.invalid/login?redirect=old"), true);
  assert.equal(messages.at(-1).message.type, "login");
  assert.equal(click("https://site.invalid/catalog/fixture", "_blank"), false);
  disconnect();
  assert.equal(windowEvents.size, 0);
  assert.equal(documentEvents.size, 0);
  windowMock.parent = windowMock;
  assert.equal(connectAdminWorkspace(() => {}), null);
  assert.equal(requestAdminWorkspaceLogin(), false);
});
