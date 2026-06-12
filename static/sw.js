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
  let parseFailed = false;
  try {
    data = event.data ? event.data.json() : {};
  } catch (e) {
    // Payload didn't decrypt/parse — still show SOMETHING below: a push the
    // user never sees is a missed call, and Safari penalizes (and may revoke
    // push for) sites whose push events display no notification.
    parseFailed = true;
  }
  if (!parseFailed && data.type !== "incoming-call") return;

  const callType = data.callType === "video" ? "Video" : "Audio";
  const body = parseFailed
    ? "Open the agent console to answer."
    : `${callType} call from ${data.callerName || "Someone"}`;

  event.waitUntil(
    (async () => {
      // If a console tab is already focused, the page itself rings — don't
      // double-alert with an OS notification.
      let focused = false;
      try {
        const clients = await self.clients.matchAll({
          type: "window",
          includeUncontrolled: true,
        });
        focused = clients.some(
          (c) => c.visibilityState === "visible" && c.focused
        );
      } catch (e) {
        /* if we can't tell, alert anyway */
      }
      if (focused) return;

      // An exception here would silently eat the notification — the one
      // failure mode this handler must never have. If the full option set is
      // refused (engines differ on option support), retry minimal.
      try {
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
      } catch (e) {
        await self.registration.showNotification("Incoming call", { body });
      }
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
