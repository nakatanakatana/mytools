import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

test("notification click waits for navigation and handles failure and no-client paths", async () => {
  let notificationClickHandler;
  let resolveNavigation;
  let rejectNavigation;
  let focused = 0;
  let openedURL = "";
  let openCount = 0;
  let failMatchAll = false;
  let failOpenWindow = false;
  let navigation = new Promise((resolve) => {
    resolveNavigation = resolve;
  });
  let matchedClients;
  const client = {
    url: "https://bridge.example/old",
    navigate: (url) => {
      assert.equal(url, "https://bridge.example/new");
      return navigation;
    },
    focus: async () => {
      focused++;
      return client;
    },
  };
  const self = {
    location: { origin: "https://bridge.example" },
    registration: { showNotification: async () => {} },
    addEventListener(type, handler) {
      if (type === "notificationclick") notificationClickHandler = handler;
    },
  };
  const context = vm.createContext({
    self,
    clients: {
      matchAll: async () => {
        if (failMatchAll) throw new Error("matchAll failed");
        return matchedClients;
      },
      openWindow: async (url) => {
        openCount++;
        openedURL = url;
        if (failOpenWindow) throw new Error("openWindow failed");
      },
    },
    URL,
    Promise,
  });

  const source = await readFile(new URL("./sw.js", import.meta.url), "utf8");
  vm.runInContext(source, context);
  const clickAndWait = async () => {
    let clickCompletion;
    notificationClickHandler({
      notification: { data: { url: "/new" }, close() {} },
      waitUntil(promise) {
        clickCompletion = promise;
      },
    });
    await clickCompletion;
  };

  matchedClients = [client];
  const firstClick = clickAndWait();
  await new Promise(setImmediate);
  assert.equal(focused, 0, "the client should not be focused before navigation finishes");
  resolveNavigation(client);
  await firstClick;
  assert.equal(focused, 1);

  navigation = new Promise((_, reject) => {
    rejectNavigation = reject;
  });
  const failedNavigationClick = clickAndWait();
  await new Promise(setImmediate);
  rejectNavigation(new Error("navigation failed"));
  await failedNavigationClick;
  assert.equal(focused, 2, "a failed navigation should still focus the existing client");

  matchedClients = [];
  await clickAndWait();
  assert.equal(openedURL, "https://bridge.example/new");
  failMatchAll = true;
  await clickAndWait();
  assert.equal(openCount, 2, "a failed client lookup should fall back to opening the target URL");

  failMatchAll = false;
  failOpenWindow = true;
  await assert.rejects(clickAndWait(), /openWindow failed/);
  assert.equal(openCount, 3, "a failed openWindow call should not be retried by the lookup fallback");
});
