const statusURL = "/api/status";
const pollIntervalMs = 5000;
const requestErrorMessage = "同期状況を取得できません。しばらくしてから再試行します。";
const providerCardTemplate = document.getElementById("provider-card-template");
const providerList = document.getElementById("provider-list");
const providerEmptyState = document.getElementById("provider-empty-state");
const statusError = document.getElementById("status-error");
const actionError = document.getElementById("action-error");
const oauthCallbackStatus = document.getElementById("oauth-callback-status");
const overallStatus = document.getElementById("overall-status");
const notificationControls = document.getElementById("notification-controls");
const notificationStatus = document.getElementById("notification-status");
const notificationToggleBtn = document.getElementById("notification-toggle-btn");
const dateTimeFormat = new Intl.DateTimeFormat("ja-JP", {
  dateStyle: "short",
  timeStyle: "medium",
});

let pollTimer = 0;
let statusRequest = null;
let oauthCallbackPending = false;
let notificationRegistration = null;
let notificationActionInFlight = false;
let notificationNeedsResubscribe = false;
let vapidPublicKeyCache = null;
let vapidPublicKeyCheckedAt = 0;
const oauthInFlight = { bluesky: false, mastodon: false };
let blueskyHandle = "";

function setText(id, value) {
  document.getElementById(id).textContent = value;
}

function formatTime(value) {
  if (value === null || value === undefined || value === "") return "未取得";

  const timestamp = new Date(value);
  if (Number.isNaN(timestamp.getTime())) return "未取得";

  return dateTimeFormat.format(timestamp);
}

function connectionLabel(connected, required) {
  if (!required) return "対象外";
  return connected ? "接続中" : "切断";
}

function providerState(provider) {
  if (provider.reauth_required) return { label: "再認証が必要", className: "status-warning" };
  if (provider.access_token_expired === true) return { label: "アクセストークン期限切れ", className: "status-warning" };
  if (provider.authorization_available === false) return { label: "未認証", className: "status-warning" };
  if (provider.bootstrapped === false) return { label: "初期同期中", className: "status-working" };
  if (provider.pending_work > 0) return { label: "処理中", className: "status-working" };
  if (provider.target_count > 0 && provider.stream_connected === false) return { label: "ストリーム切断", className: "status-warning" };
  if (provider.degraded === true) return { label: "同期に問題あり", className: "status-warning" };
  if (provider.ready === true) return { label: "同期中", className: "status-ready" };
  return { label: "要確認", className: "status-warning" };
}

function overallState(status, states) {
  if (!status.ready || states.some((state) => state.className === "status-warning")) {
    return { label: "要確認", className: "status-warning" };
  }
  if (status.pending_work > 0 || states.some((state) => state.label === "処理中" || state.label === "初期同期中")) {
    return { label: "処理中", className: "status-working" };
  }
  return { label: "稼働中", className: "status-ready" };
}

function renderStatus(status) {
  const providers = status.providers && typeof status.providers === "object" ? status.providers : {};
  const states = Object.values(providers).map(providerState);
  const state = overallState(status, states);

  const overallText = `全体状態: ${state.label}`;
  if (overallStatus.textContent !== overallText || overallStatus.className !== state.className) {
    overallStatus.textContent = overallText;
    overallStatus.className = state.className;
  }
  setText("database-status", status.database ? "利用可能" : "要確認");
  setText("oauth-status", status.oauth_connected ? "接続済み" : "要確認");
  setText("jetstream-status", connectionLabel(status.jetstream_connected, status.jetstream_required));
  setText("dispatcher-status", status.dispatcher_running ? "稼働中" : "停止中");
  setText("outbox-status", `${status.outbox?.count ?? 0} / ${status.outbox?.limit ?? 0}${status.outbox?.at_limit ? "（上限到達）" : ""}${status.outbox?.ready === false ? "（要確認）" : ""}`);
  setText("pending-work-status", String(status.pending_work ?? 0));
  setText("last-jetstream-event", formatTime(status.last_jetstream_event_at));
  setText("last-sync", formatTime(status.last_sync_at));
  setText("last-relay-delivery", formatTime(status.last_relay_delivery_at));

  const cards = reconcileProviderCards(Object.keys(providers).sort(), providers);
  syncOAuthControls();
  providerEmptyState.hidden = cards.length > 0;
  if (oauthCallbackPending) {
    oauthCallbackStatus.textContent = "OAuth 認証が完了しました。同期状況を更新しました。";
    oauthCallbackPending = false;
  }
}

function reconcileProviderCards(names, providers) {
  const existingCards = new Map(
    Array.from(providerList.children, (card) => [card.dataset.provider, card]),
  );
  const cards = names.map((name) => {
    const card = existingCards.get(name) || createProviderCard(name);
    updateProviderCard(card, name, providers[name]);
    return card;
  });

  cards.forEach((card, index) => {
    const current = providerList.children[index] || null;
    if (current === card) return;
    if (typeof providerList.moveBefore === "function" && card.isConnected && providerList.isConnected && card.ownerDocument === providerList.ownerDocument) {
      providerList.moveBefore(card, current);
    } else {
      providerList.insertBefore(card, current);
    }
  });
  while (providerList.children.length > cards.length) {
    providerList.lastElementChild.remove();
  }
  return cards;
}

function syncOAuthControls() {
  const blueskyButton = providerList.querySelector("[data-bluesky-oauth-form] button[type=submit]");
  if (blueskyButton) blueskyButton.disabled = oauthInFlight.bluesky;

  const mastodonButton = providerList.querySelector("[data-mastodon-oauth-start]");
  if (mastodonButton) mastodonButton.disabled = oauthInFlight.mastodon;
}

function createProviderCard(name) {
  const card = providerCardTemplate.content.firstElementChild.cloneNode(true);
  card.dataset.provider = name;
  const setup = card.querySelector("[data-provider-setup]");
  if (name === "bluesky") setup.append(createBlueskySetup());
  if (name === "mastodon") setup.append(createMastodonSetup());
  return card;
}

function createBlueskySetup() {
  const form = document.createElement("form");
  form.dataset.blueskyOauthForm = "";

  const label = document.createElement("label");
  label.append("Bluesky ハンドル");

  const handleInput = document.createElement("input");
  handleInput.name = "handle";
  handleInput.autocomplete = "username";
  handleInput.required = true;
  label.append(handleInput);

  const button = document.createElement("button");
  button.type = "submit";
  button.textContent = "セットアップを開始";
  form.append(label, button);

  form.addEventListener("input", () => {
    blueskyHandle = handleInput.value;
  });
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (oauthInFlight.bluesky) return;

    const handle = handleInput.value.trim();
    if (!handle) {
      showActionError("Bluesky ハンドルを入力してください。");
      return;
    }

    clearActionError();
    blueskyHandle = handle;
    oauthInFlight.bluesky = true;
    syncOAuthControls();
    try {
      await startBluesky(handle);
    } catch (_error) {
      oauthInFlight.bluesky = false;
      syncOAuthControls();
      showActionError("OAuth のセットアップを開始できません。しばらくしてから再試行します。");
    }
  });

  return form;
}

function createMastodonSetup() {
  const button = document.createElement("button");
  button.type = "button";
  button.dataset.mastodonOauthStart = "";
  button.textContent = "Mastodon のセットアップを開始";
  button.addEventListener("click", async () => {
    if (oauthInFlight.mastodon) return;

    clearActionError();
    oauthInFlight.mastodon = true;
    syncOAuthControls();
    try {
      await startMastodon();
    } catch (_error) {
      oauthInFlight.mastodon = false;
      syncOAuthControls();
      showActionError("OAuth のセットアップを開始できません。しばらくしてから再試行します。");
    }
  });
  return button;
}

function updateProviderCard(card, name, provider) {
  provider = provider || {};
  const state = providerState(provider);
  const status = card.querySelector("[data-provider-status]");

  card.querySelector("[data-provider-name]").textContent = name;
  status.textContent = state.label;
  status.className = state.className;
  card.querySelector("[data-target-count]").textContent = String(provider.target_count ?? 0);
  card.querySelector("[data-bootstrap-state]").textContent = provider.bootstrapped ? "完了" : "実行中";
  card.querySelector("[data-pending-work]").textContent = String(provider.pending_work ?? 0);
  card.querySelector("[data-stream-state]").textContent = connectionLabel(provider.stream_connected, (provider.target_count ?? 0) > 0);
  card.querySelector("[data-last-event]").textContent = formatTime(provider.last_event_at);
  card.querySelector("[data-last-reconciliation]").textContent = formatTime(provider.last_reconciliation_at);

  if (name === "bluesky") {
    const form = card.querySelector("[data-bluesky-oauth-form]");
    const handleInput = form.elements.handle;
    if (document.activeElement !== handleInput && handleInput.value !== blueskyHandle) {
      handleInput.value = blueskyHandle;
    }
  }
}

function showStatusError(message) {
  if (!statusError.hidden && statusError.textContent === message) return;
  statusError.textContent = message;
  statusError.hidden = false;
}

function clearStatusError() {
  statusError.textContent = "";
  statusError.hidden = true;
}

function showActionError(message) {
  if (!actionError.hidden && actionError.textContent === message) return;
  actionError.textContent = message;
  actionError.hidden = false;
}

function clearActionError() {
  actionError.textContent = "";
  actionError.hidden = true;
}

function navigateToAuthorization(authorizationURL) {
  const destination = new URL(authorizationURL, window.location.origin);
  if (destination.protocol !== "https:" && destination.protocol !== "http:") {
    throw new Error("invalid authorization URL");
  }
  window.location.assign(destination.href);
}

async function startBluesky(handle) {
  const response = await fetch("/oauth/bluesky/start", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ handle: handle.trim() }),
  });
  const body = await response.json();
  if (!response.ok || typeof body.authorization_url !== "string") {
    throw new Error("Bluesky OAuth start failed");
  }
  navigateToAuthorization(body.authorization_url);
}

async function startMastodon() {
  const response = await fetch("/oauth/mastodon/start", { method: "POST" });
  const body = await response.json();
  if (!response.ok || typeof body.authorization_url !== "string") {
    throw new Error("Mastodon OAuth start failed");
  }
  navigateToAuthorization(body.authorization_url);
}

function loadStatus() {
  if (statusRequest) return statusRequest;

  statusRequest = (async () => {
    try {
      const response = await fetch(statusURL, { cache: "no-store" });
      if (!response.ok) throw new Error(`status request failed: ${response.status}`);
      renderStatus(await response.json());
      void refreshNotificationUI();
      clearStatusError();
    } catch (_error) {
      showStatusError(requestErrorMessage);
    } finally {
      statusRequest = null;
      scheduleStatusPoll();
    }
  })();
  return statusRequest;
}

function scheduleStatusPoll() {
  window.clearTimeout(pollTimer);
  if (document.visibilityState === "visible") {
    pollTimer = window.setTimeout(loadStatus, pollIntervalMs);
  }
}

function showOAuthCallbackStatus() {
  if (new URLSearchParams(window.location.search).get("oauth") === "success") {
    oauthCallbackPending = true;
    oauthCallbackStatus.textContent = "OAuth 認証が完了しました。同期状況を確認しています。";
    oauthCallbackStatus.hidden = false;
  }
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") loadStatus();
  else window.clearTimeout(pollTimer);
});

showOAuthCallbackStatus();
loadStatus();
initNotifications();

function urlBase64ToUint8Array(base64String) {
  const padding = "=".repeat((4 - (base64String.length % 4)) % 4);
  const base64 = (base64String + padding).replace(/-/g, "+").replace(/_/g, "/");
  return Uint8Array.from(window.atob(base64), (c) => c.charCodeAt(0));
}

function updateNotificationUI(subscription, keyMatches = null) {
  const isDenied = Notification.permission === "denied";
  if (subscription) {
    notificationNeedsResubscribe = !isDenied && keyMatches === false;
    if (isDenied) {
      notificationStatus.textContent = "ブラウザ通知: ブロック中（登録済み）";
    } else if (keyMatches === false) {
      notificationStatus.textContent = "ブラウザ通知: 再登録が必要です（VAPID 鍵が変更されています）";
    } else if (keyMatches === null) {
      notificationStatus.textContent = "ブラウザ通知: 有効（鍵を確認できません）";
    } else {
      notificationStatus.textContent = "ブラウザ通知: 有効";
    }
    notificationToggleBtn.textContent = notificationNeedsResubscribe ? "通知を再登録する" : "通知を解除する";
    notificationToggleBtn.disabled = false;
  } else {
    notificationNeedsResubscribe = false;
    notificationStatus.textContent = isDenied ? "ブラウザ通知: ブロック中" : "ブラウザ通知: 未設定";
    notificationToggleBtn.textContent = "通知を有効にする";
    notificationToggleBtn.disabled = isDenied;
  }
}

async function currentVapidPublicKey(forceRefresh = false) {
  const cacheAgeMs = Date.now() - vapidPublicKeyCheckedAt;
  if (!forceRefresh && vapidPublicKeyCache && cacheAgeMs < 60_000) {
    return vapidPublicKeyCache;
  }

  const response = await fetch("/api/push/vapid-public-key", { cache: "no-store" });
  if (!response.ok) throw new Error("failed to fetch VAPID public key");
  const keyData = await response.json();
  if (typeof keyData.public_key !== "string" || !keyData.public_key) {
    throw new Error("missing VAPID public key");
  }
  vapidPublicKeyCache = keyData.public_key;
  vapidPublicKeyCheckedAt = Date.now();
  return vapidPublicKeyCache;
}

function subscriptionUsesVapidKey(subscription, publicKey) {
  const subscribedKey = subscription.options?.applicationServerKey;
  if (!subscribedKey) return null;

  const expected = urlBase64ToUint8Array(publicKey);
  const actual = new Uint8Array(subscribedKey);
  return actual.length === expected.length && actual.every((byte, index) => byte === expected[index]);
}

async function refreshNotificationUI() {
  if (!notificationRegistration || notificationActionInFlight) return;
  let subscription = null;
  try {
    subscription = await notificationRegistration.pushManager.getSubscription();
    if (notificationActionInFlight) return;
    if (!subscription) {
      updateNotificationUI(null);
      return;
    }
    if (Notification.permission === "denied") {
      updateNotificationUI(subscription);
      return;
    }
    const key = await currentVapidPublicKey();
    if (notificationActionInFlight) return;
    updateNotificationUI(subscription, subscriptionUsesVapidKey(subscription, key));
  } catch (_error) {
    if (!notificationActionInFlight) updateNotificationUI(subscription, null);
  }
}

async function saveSubscription(subscription) {
  const subJson = subscription.toJSON();
  const response = await fetch("/api/push/subscribe", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      endpoint: subJson.endpoint,
      keys: {
        p256dh: subJson.keys?.p256dh,
        auth: subJson.keys?.auth,
      },
    }),
  });
  if (!response.ok) throw new Error("failed to subscribe");
}

async function removeSubscription(subscription) {
  const response = await fetch("/api/push/unsubscribe", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ endpoint: subscription.endpoint }),
  });
  if (!response.ok) throw new Error("failed to unsubscribe");
  try {
    if (await subscription.unsubscribe()) return;
  } catch (_error) {
    // Check the browser state before deciding whether to restore the server row.
  }

  const activeSubscription = await notificationRegistration.pushManager.getSubscription().catch(() => subscription);
  if (!activeSubscription) return;

  await saveSubscription(activeSubscription).catch(() => {});
  throw new Error("failed to remove browser subscription");
}

async function subscribeWithCurrentKey(registration) {
  const publicKey = await currentVapidPublicKey(true);
  const subscription = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(publicKey),
  });
  try {
    await saveSubscription(subscription);
  } catch (error) {
    await subscription.unsubscribe().catch(() => {});
    throw error;
  }
}

async function initNotifications() {
  if (!("serviceWorker" in navigator && "PushManager" in window && "Notification" in window)) {
    return;
  }
  if (!notificationControls || !notificationStatus || !notificationToggleBtn) {
    return;
  }

  if (!window.isSecureContext) {
    notificationControls.hidden = false;
    notificationStatus.textContent = "ブラウザ通知: HTTPS環境が必要です";
    notificationToggleBtn.hidden = true;
    return;
  }

  let registration;
  try {
    registration = await navigator.serviceWorker.register("/sw.js");
  } catch (_error) {
    showActionError("ブラウザ通知の初期化に失敗しました。");
    return;
  }

  notificationRegistration = registration;
  notificationControls.hidden = false;
  await refreshNotificationUI();

  notificationToggleBtn.addEventListener("click", async () => {
    if (notificationActionInFlight) return;
    clearActionError();
    notificationActionInFlight = true;
    notificationToggleBtn.disabled = true;

    try {
      const currentSubscription = await registration.pushManager.getSubscription();
      if (!currentSubscription) {
        const permission = await Notification.requestPermission();
        if (permission !== "granted") {
          return;
        }

        await subscribeWithCurrentKey(registration);
      } else if (Notification.permission === "denied") {
        await removeSubscription(currentSubscription);
      } else {
        let publicKey = null;
        try {
          publicKey = await currentVapidPublicKey(true);
        } catch (error) {
          if (notificationNeedsResubscribe) throw error;
        }
        await removeSubscription(currentSubscription);
        if (publicKey && subscriptionUsesVapidKey(currentSubscription, publicKey) === false) {
          await subscribeWithCurrentKey(registration);
        }
      }
    } catch (_error) {
      showActionError("ブラウザ通知の設定に失敗しました。しばらくしてから再試行してください。");
    } finally {
      notificationActionInFlight = false;
      await refreshNotificationUI();
    }
  });
}
