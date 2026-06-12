// sw.js — service worker for Web Push (Phase 2). Served at /sw.js (scope "/").
// Wakes on an incoming-call push and rings the agent with an OS notification
// even when the console tab is backgrounded, minimised, or closed — something a
// WebSocket can't do. Clicking the notification focuses (or opens) the console,
// which then re-hydrates the ringing call from GET /api/call/pending.

self.addEventListener("install", () => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch (e) {
    /* ignore malformed payloads */
  }
  if (data.type !== "incoming-call") return;

  const callType = data.callType === "video" ? "Video" : "Audio";
  const body = `${callType} call from ${data.callerName || "Someone"}`;

  event.waitUntil(
    (async () => {
      // If a console tab is already focused, the page itself rings — don't
      // double-alert with an OS notification.
      const clients = await self.clients.matchAll({
        type: "window",
        includeUncontrolled: true,
      });
      const focused = clients.some(
        (c) => c.visibilityState === "visible" && c.focused
      );
      if (focused) return;

      await self.registration.showNotification("Incoming call", {
        body,
        icon: "/public/favicon.svg",
        badge: "/public/favicon.svg",
        tag: "incoming-call",
        renotify: true,
        requireInteraction: true,
        vibrate: [200, 100, 200, 100, 200],
        data: { ref: data.ref, callId: data.callId },
      });
    })()
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const ref = event.notification.data && event.notification.data.ref;
  const target = "/auth.html" + (ref ? "?ref=" + encodeURIComponent(ref) : "");

  event.waitUntil(
    (async () => {
      const clients = await self.clients.matchAll({
        type: "window",
        includeUncontrolled: true,
      });
      // Focus an existing console tab if one is open…
      for (const c of clients) {
        if (c.url.includes("/auth.html") && "focus" in c) {
          return c.focus();
        }
      }
      // …otherwise open a fresh one (the page checks /api/call/pending on load).
      if (self.clients.openWindow) return self.clients.openWindow(target);
    })()
  );
});
