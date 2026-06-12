// js/push.js — Web Push client (Phase 2). Exposes window.Push:
//   registerServiceWorker()        → ensure /sw.js is registered (for notifications)
//   enablePush(sessionId)          → permission + subscribe + POST /api/push/subscribe
//   disablePush()                  → POST /api/push/unsubscribe (stop server-side push)
//
// Self-contained: it fetches the VAPID public key from /api/connect-config, so
// callers just pass their presence sessionId. Everything degrades gracefully —
// if the browser lacks push, the key is unset, or permission is denied, the
// functions return false and the app falls back to the inbox-over-WS path.
(function () {
  function urlB64ToUint8Array(base64String) {
    const padding = "=".repeat((4 - (base64String.length % 4)) % 4);
    const base64 = (base64String + padding).replace(/-/g, "+").replace(/_/g, "/");
    const raw = atob(base64);
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  let registration = null;

  async function registerServiceWorker() {
    if (!("serviceWorker" in navigator)) return null;
    if (registration) return registration;
    try {
      registration = await navigator.serviceWorker.register("/sw.js");
      return registration;
    } catch (e) {
      console.warn("[Push] service worker registration failed:", e.message);
      return null;
    }
  }

  async function fetchVapidKey() {
    try {
      const r = await fetch("/api/connect-config");
      if (!r.ok) return "";
      const cfg = await r.json();
      return (cfg && cfg.vapidPublicKey) || "";
    } catch (e) {
      return "";
    }
  }

  async function enablePush(sessionId) {
    if (!("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
      return false;
    }
    const vapidPublicKey = await fetchVapidKey();
    if (!vapidPublicKey) return false; // push not configured on the server

    if (Notification.permission === "default") {
      try { await Notification.requestPermission(); } catch (e) {}
    }
    if (Notification.permission !== "granted") return false;

    try {
      const reg = await registerServiceWorker();
      if (!reg) return false;
      await navigator.serviceWorker.ready;
      let sub = await reg.pushManager.getSubscription();
      if (!sub) {
        sub = await reg.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: urlB64ToUint8Array(vapidPublicKey),
        });
      }
      const json = sub.toJSON(); // { endpoint, keys: { p256dh, auth } }
      const resp = await fetch("/api/push/subscribe", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          sessionId: sessionId || "",
          subscription: { endpoint: json.endpoint, keys: json.keys },
        }),
      });
      return resp.ok;
    } catch (e) {
      console.warn("[Push] enable failed:", e.message);
      return false;
    }
  }

  async function disablePush() {
    try {
      const reg = registration || (await registerServiceWorker());
      if (!reg) return;
      const sub = await reg.pushManager.getSubscription();
      if (!sub) return;
      const json = sub.toJSON();
      await fetch("/api/push/unsubscribe", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ endpoint: json.endpoint }),
      });
      // Keep the browser-side subscription so re-enabling is instant; we only
      // tell the server to stop pushing to it.
    } catch (e) {
      /* best effort */
    }
  }

  window.Push = { registerServiceWorker, enablePush, disablePush };
})();
