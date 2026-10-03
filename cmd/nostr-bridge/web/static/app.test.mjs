import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

class TestElement {
  constructor() {
    this.hidden = false;
    this.disabled = false;
    this.textContent = "";
    this.className = "";
    this.dataset = {};
    this.children = [];
    this.listeners = new Map();
  }

  addEventListener(type, listener) {
    this.listeners.set(type, listener);
  }
}

test("notification UI preserves in-flight actions and reports denied existing subscriptions", async () => {
  const elements = new Map();
  const getElementById = (id) => {
    if (!elements.has(id)) elements.set(id, new TestElement());
    return elements.get(id);
  };
  const notificationToggle = getElementById("notification-toggle-btn");
  const pushRequests = [];
  let vapidRequests = 0;
  let failVapidRequest = false;
  let allowUnsubscribe = false;
  let subscriptionPresent = true;
  let rejectVapidRequest;
  const registration = {
    pushManager: {
      getSubscription: async () => null,
      subscribe: async () => {
        throw new Error("subscribe should not be reached in this test");
      },
    },
  };
  const document = {
    visibilityState: "hidden",
    activeElement: null,
    getElementById,
    createElement: () => new TestElement(),
    addEventListener() {},
  };
  const window = {
    isSecureContext: true,
    PushManager: function PushManager() {},
    Notification: { permission: "default", requestPermission: async () => "granted" },
    location: { search: "", origin: "https://bridge.example" },
    setTimeout: () => 1,
    clearTimeout() {},
  };
  const context = vm.createContext({
    document,
    window,
    navigator: { serviceWorker: { register: async () => registration } },
    Notification: { permission: "default", requestPermission: async () => "granted" },
    fetch: async (url) => {
      if (url === "/api/status") return { ok: false, status: 503 };
      if (url === "/api/push/vapid-public-key") {
        vapidRequests++;
        if (failVapidRequest) return Promise.reject(new Error("VAPID key unavailable"));
        return new Promise((_, reject) => {
          rejectVapidRequest = reject;
        });
      }
      if (url === "/api/push/unsubscribe" || url === "/api/push/subscribe") {
        pushRequests.push(url);
        return { ok: true };
      }
      throw new Error(`unexpected request: ${url}`);
    },
    URL,
    URLSearchParams,
    Intl,
    Date,
    Uint8Array,
    Promise,
  });

  const source = await readFile(new URL("./app.js", import.meta.url), "utf8");
  vm.runInContext(source, context);

  for (let attempt = 0; attempt < 20 && !notificationToggle.listeners.has("click"); attempt++) {
    await new Promise(setImmediate);
  }
  const clickHandler = notificationToggle.listeners.get("click");
  assert.ok(clickHandler, "notification control should be initialized");

  const action = clickHandler();
  for (let attempt = 0; attempt < 20 && !rejectVapidRequest; attempt++) {
    await new Promise(setImmediate);
  }
  assert.ok(rejectVapidRequest, "subscription action should be waiting for the VAPID key");
  assert.equal(notificationToggle.disabled, true);

  await context.refreshNotificationUI();
  assert.equal(notificationToggle.disabled, true, "a background refresh must preserve the in-flight disabled state");

  rejectVapidRequest(new Error("test request failure"));
  await action;

  const subscription = {
    endpoint: "https://push.example/subscription",
    toJSON: () => ({
      endpoint: "https://push.example/subscription",
      keys: { p256dh: "public-key", auth: "auth-secret" },
    }),
    unsubscribe: async () => {
      if (!allowUnsubscribe) return false;
      subscriptionPresent = false;
      return true;
    },
  };
  registration.pushManager.getSubscription = async () => subscriptionPresent ? subscription : null;
  await assert.rejects(
    context.removeSubscription(subscription),
    /failed to remove browser subscription/,
  );
  assert.deepEqual(pushRequests, ["/api/push/unsubscribe", "/api/push/subscribe"]);

  context.Notification.permission = "denied";
  context.updateNotificationUI(subscription, true);
  assert.equal(getElementById("notification-status").textContent, "ブラウザ通知: ブロック中（登録済み）");
  assert.equal(notificationToggle.textContent, "通知を解除する");
  assert.equal(notificationToggle.disabled, false);

  allowUnsubscribe = true;
  failVapidRequest = true;
  await context.refreshNotificationUI();
  assert.equal(vapidRequests, 1, "denied permission should not require a VAPID key refresh");
  await clickHandler();
  assert.equal(vapidRequests, 1, "a denied subscription should not fetch a key to unsubscribe");
  assert.deepEqual(pushRequests, [
    "/api/push/unsubscribe",
    "/api/push/subscribe",
    "/api/push/unsubscribe",
  ]);

  subscriptionPresent = true;
  context.Notification.permission = "granted";
  context.updateNotificationUI(subscription, true);
  await clickHandler();
  assert.equal(vapidRequests, 2);
  assert.deepEqual(pushRequests, [
    "/api/push/unsubscribe",
    "/api/push/subscribe",
    "/api/push/unsubscribe",
    "/api/push/unsubscribe",
  ]);

  subscriptionPresent = true;
  context.updateNotificationUI(subscription, false);
  await clickHandler();
  assert.equal(pushRequests.length, 4, "a failed key lookup should not remove a subscription marked for re-registration");
});
