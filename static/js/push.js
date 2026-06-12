// js/push.js — Web Push client. Exposes window.Push:
//   supported()              → is push usable in this browser at all
//   permission()             → "granted" | "denied" | "default" | "unsupported"
//   requestPermission()      → MUST be called from a user gesture (Safari);
//                              returns the resulting permission
//   enablePush(sessionId)    → subscribe + POST /api/push/subscribe (permission
//                              must already be granted). Returns a status string:
//                              "armed" | "blocked" | "unavailable" | "unsupported" | "error"
//   disablePush()            → POST /api/push/unsubscribe (stop server-side push)
//   registerServiceWorker()  → ensure /sw.js is registered
//
// Safari notes that shape this design:
//  - Notification.requestPermission() only prompts from a *direct* user gesture,
//    and the gesture is consumed by the first permission request — so we never
//    bundle it with getUserMedia. The console exposes a dedicated "Enable" action.
//  - The VAPID key is pre-fetched at load so subscribe() never sits behind an
//    await that would drop the gesture context.
(function () {
  function urlB64ToUint8Array(base64String) {
    const padding = "=".repeat((4 - (base64String.length % 4)) % 4);
    const base64 = (base64String + padding).replace(/-/g, "+").replace(/_/g, "/");
    const raw = atob(base64);
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  const supported =
    "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;

  // Pre-fetch the VAPID public key at load (not behind the gesture).
  let vapidKey = "";
  const vapidReady = (async () => {
    try {
      const r = await fetch("/api/connect-config");
      if (r.ok) {
        const cfg = await r.json();
        vapidKey = (cfg && cfg.vapidPublicKey) || "";
      }
    } catch (e) {
      /* leave empty */
    }
    return vapidKey;
  })();

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

  function permission() {
    if (!("Notification" in window)) return "unsupported";
    return Notification.permission; // granted | denied | default
  }

  // Call from a user gesture (e.g. an "Enable" button click) so Safari prompts.
  async function requestPermission() {
    if (!("Notification" in window)) return "unsupported";
    if (Notification.permission === "default") {
      try {
        return await Notification.requestPermission();
      } catch (e) {
        return Notification.permission;
      }
    }
    return Notification.permission;
  }

  // Subscribe and register with the server. Permission must already be granted.
  async function enablePush(sessionId) {
    if (!supported) return "unsupported";
    await vapidReady;
    if (!vapidKey) return "unavailable"; // server has no VAPID configured
    if (Notification.permission !== "granted") return "blocked";
    try {
      const reg = await registerServiceWorker();
      if (!reg) return "error";
      await navigator.serviceWorker.ready;
      let sub = await reg.pushManager.getSubscription();
      if (!sub) {
        sub = await reg.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: urlB64ToUint8Array(vapidKey),
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
      return resp.ok ? "armed" : "error";
    } catch (e) {
      console.warn("[Push] enable failed:", e.message);
      return "error";
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

  window.Push = {
    supported: () => supported,
    permission,
    requestPermission,
    enablePush,
    disablePush,
    registerServiceWorker,
  };
})();
