self.addEventListener("push", (event) => {
  if (!event.data) return;

  let payload;
  try {
    payload = event.data.json();
  } catch {
    payload = null;
  }

  if (!payload || typeof payload !== "object") {
    payload = {
      title: "nostr-bridge",
      body: event.data.text(),
    };
  }

  const title = payload.title || "nostr-bridge";
  const options = {
    body: payload.body || "",
    tag: payload.tag || "nostr-bridge-notification",
    data: payload.data || { url: "/" },
  };

  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const rawUrl = event.notification.data?.url || "/";
  let targetUrl = "/";
  try {
    const resolved = new URL(rawUrl, self.location.origin);
    if (resolved.origin === self.location.origin) {
      targetUrl = resolved.href;
    }
  } catch (_err) {
    targetUrl = "/";
  }

  event.waitUntil(
    (async () => {
      const clientList = await clients.matchAll({ type: "window", includeUncontrolled: true }).catch(() => null);
      for (const client of clientList || []) {
        if (client.url && "focus" in client) {
          if ("navigate" in client && client.url !== targetUrl) {
            await client.navigate(targetUrl).catch(() => {});
          }
          return client.focus();
        }
      }
      if (clients.openWindow) {
        return clients.openWindow(targetUrl);
      }
    })(),
  );
});
