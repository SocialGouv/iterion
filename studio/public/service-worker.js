// Iterion web-push service worker. Served unhashed at the origin root so
// its scope covers the whole SPA. Registered lazily by src/lib/webPush.ts
// when the user enables notifications — never at page load.

self.addEventListener("install", () => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

// Payload shape: pkg/usernotify/webpush sink `payload` struct —
// {kind, title, body, link, tag, run_id}.
self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    // A malformed payload still must show SOMETHING: browsers may punish
    // a push that renders no notification by revoking the subscription.
  }
  const title = data.title || "Iterion";
  event.waitUntil(
    self.registration.showNotification(title, {
      body: data.body || "",
      tag: data.tag || "iterion",
      renotify: true,
      icon: "/android-icon-192x192.png",
      badge: "/android-icon-96x96.png",
      data: { link: data.link || "/" },
    }),
  );
});

// Click → focus an existing studio tab already on the run (or navigate
// one), else open a new window on the deep link.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const link = (event.notification.data && event.notification.data.link) || "/";
  event.waitUntil(
    (async () => {
      const clientList = await self.clients.matchAll({
        type: "window",
        includeUncontrolled: true,
      });
      let target;
      try {
        target = new URL(link, self.location.origin);
      } catch {
        target = new URL("/", self.location.origin);
      }
      for (const client of clientList) {
        const url = new URL(client.url);
        if (url.origin !== target.origin) continue;
        if (url.pathname === target.pathname) {
          return client.focus();
        }
      }
      // Any same-origin studio tab: focus + navigate it.
      for (const client of clientList) {
        if (new URL(client.url).origin === target.origin && "navigate" in client) {
          await client.focus();
          return client.navigate(target.href);
        }
      }
      return self.clients.openWindow(target.href);
    })(),
  );
});
