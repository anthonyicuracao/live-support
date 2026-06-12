// js/config.js
// Replaces the Supabase client with:
//   window.Realtime — WebSocket pub/sub client (channels, presence, broadcast)
//   window.DB       — small fetch wrapper for the Go server's REST API
// Exposes window.configReady (a Promise) that other scripts must await.
//
// The Realtime channel object mimics the subset of the Supabase channel API
// used by this app: .on("broadcast"...), .on("presence",{event:"sync"}...),
// .subscribe(cb), .send({type:"broadcast",...}), .track(state),
// .presenceState(), .unsubscribe().

window.configReady = (async function () {
  // ─── Branding (color + favicon) ──────────────────────────────────────────
  // Apply the optional PRIMARY_COLOR and FAVICON_URL from the server (.env) as
  // early as possible so overrides take effect before the rest of the app
  // renders. The server validates both; empty/invalid values leave the
  // stylesheet default (--primary) and the embedded favicon in place.
  // Fire-and-forget: a failure here must never block the WS/DB setup below.
  (async function applyBranding() {
    try {
      const r = await fetch("/api/connect-config");
      if (!r.ok) return;
      const cfg = await r.json();
      if (cfg && cfg.primaryColor) {
        document.documentElement.style.setProperty("--primary", cfg.primaryColor);
      }
      if (cfg && cfg.faviconUrl) {
        let link = document.querySelector('link[rel="icon"]');
        if (!link) {
          link = document.createElement("link");
          link.rel = "icon";
          document.head.appendChild(link);
        }
        // Let the browser infer the type from the URL; drop the SVG-specific
        // hints so non-SVG overrides (png/ico) aren't mislabeled.
        link.removeAttribute("type");
        link.removeAttribute("sizes");
        link.href = cfg.faviconUrl;
      }
    } catch (e) {
      /* defaults stay */
    }
  })();

  // ─── WebSocket connection with auto-reconnect ───────────────────────────
  const wsProto = location.protocol === "https:" ? "wss:" : "ws:";
  const wsUrl = `${wsProto}//${location.host}/ws`;

  let ws = null;
  let wsOpen = false;
  const channels = new Map(); // name -> Channel
  let openWaiters = [];

  // Broadcasts that happen while the socket is down (tab frozen by Safari,
  // network blip, laptop sleep) are simply lost — reconnect re-subscribes the
  // channels but can't replay missed events. Pages register a reconnect hook
  // to refetch whatever those events would have told them about.
  let everOpened = false;
  const reconnectListeners = [];

  function connect() {
    ws = new WebSocket(wsUrl);

    ws.onopen = () => {
      wsOpen = true;
      openWaiters.forEach((fn) => fn());
      openWaiters = [];
      // Re-establish all channel subscriptions and presence tracks
      for (const ch of channels.values()) {
        rawSend({ action: "subscribe", channel: ch.name });
        if (ch._trackedState) {
          rawSend({ action: "track", channel: ch.name, key: ch._presenceKey, state: ch._trackedState });
        }
      }
      if (everOpened) {
        reconnectListeners.forEach((fn) => {
          try { fn(); } catch (e) { /* listener errors must not break the socket */ }
        });
      }
      everOpened = true;
    };

    ws.onmessage = (ev) => {
      let msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      const ch = channels.get(msg.channel);
      if (msg.type === "ack" && msg.action === "subscribe") {
        if (ch) ch._fireStatus("SUBSCRIBED");
      } else if (msg.type === "broadcast") {
        if (ch) ch._fireBroadcast(msg.event, msg.payload);
      } else if (msg.type === "presence") {
        if (ch) ch._firePresence(msg.users || []);
      }
    };

    ws.onclose = () => {
      wsOpen = false;
      setTimeout(connect, 1000); // reconnect with backoff-lite
    };

    ws.onerror = () => {
      try { ws.close(); } catch (e) {}
    };
  }

  function whenOpen() {
    if (wsOpen) return Promise.resolve();
    return new Promise((resolve) => openWaiters.push(resolve));
  }

  function rawSend(obj) {
    if (wsOpen && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
      return true;
    }
    return false;
  }

  async function send(obj) {
    await whenOpen();
    return rawSend(obj);
  }

  // ─── Channel ─────────────────────────────────────────────────────────────
  class Channel {
    constructor(name, opts = {}) {
      this.name = name;
      this._presenceKey = opts?.config?.presence?.key || null;
      this._broadcastHandlers = {}; // event -> [fn]
      this._presenceSyncHandlers = [];
      this._statusCallbacks = [];
      this._subscribed = false;
      this._trackedState = null;
      this._lastPresenceUsers = [];
    }

    on(type, filter, handler) {
      if (type === "broadcast") {
        const ev = filter.event;
        (this._broadcastHandlers[ev] = this._broadcastHandlers[ev] || []).push(handler);
      } else if (type === "presence" && filter.event === "sync") {
        this._presenceSyncHandlers.push(handler);
      }
      return this;
    }

    subscribe(cb) {
      if (cb) this._statusCallbacks.push(cb);
      channels.set(this.name, this);
      send({ action: "subscribe", channel: this.name });
      return this;
    }

    _fireStatus(status) {
      this._subscribed = status === "SUBSCRIBED";
      this._statusCallbacks.forEach((cb) => {
        try { cb(status); } catch (e) { console.error(e); }
      });
    }

    _fireBroadcast(event, payload) {
      (this._broadcastHandlers[event] || []).forEach((fn) => {
        try { fn({ payload }); } catch (e) { console.error(e); }
      });
    }

    _firePresence(users) {
      this._lastPresenceUsers = users;
      this._presenceSyncHandlers.forEach((fn) => {
        try { fn(); } catch (e) { console.error(e); }
      });
    }

    presenceState() {
      // Supabase shape: { key: [state, ...] } — emulate with session_id keys.
      const state = {};
      for (const u of this._lastPresenceUsers) {
        const key = u.session_id || JSON.stringify(u);
        (state[key] = state[key] || []).push(u);
      }
      return state;
    }

    async track(state) {
      this._trackedState = state;
      if (!this._presenceKey) this._presenceKey = state.session_id || null;
      await send({ action: "track", channel: this.name, key: this._presenceKey, state });
    }

    async sendMessage({ type, event, payload }) {
      if (type !== "broadcast") return "error";
      const ok = await send({ action: "broadcast", channel: this.name, event, payload });
      return ok ? "ok" : "error";
    }

    // Supabase API name
    async send(msg) {
      return this.sendMessage(msg);
    }

    unsubscribe() {
      channels.delete(this.name);
      rawSend({ action: "unsubscribe", channel: this.name });
      this._subscribed = false;
    }
  }

  // ─── Public Realtime API ─────────────────────────────────────────────────
  window.Realtime = {
    channel(name, opts) {
      // Reuse existing channel object if one is already registered with this
      // name (Supabase creates separate ones; our hub handles either way, but
      // reuse avoids subscriber clobbering for the dashboard channel).
      const existing = channels.get(name);
      if (existing) return existing;
      return new Channel(name, opts);
    },
    // One-shot publish to a channel without subscribing to it. The hub relays
    // broadcasts to current subscribers regardless of sender subscription.
    async publish(channelName, event, payload) {
      return send({ action: "broadcast", channel: channelName, event, payload });
    },
    // Run fn after every RE-connect (not the initial open) — use it to refetch
    // state whose change events were lost while the socket was down.
    onReconnect(fn) {
      reconnectListeners.push(fn);
    },
  };

  // ─── REST wrapper ────────────────────────────────────────────────────────
  async function api(method, path, body) {
    try {
      const resp = await fetch(path, {
        method,
        headers: body ? { "Content-Type": "application/json" } : undefined,
        body: body ? JSON.stringify(body) : undefined,
      });
      const data = await resp.json().catch(() => ({}));
      if (!resp.ok) {
        return { data: null, error: { message: data.error || `HTTP ${resp.status}` }, count: 0 };
      }
      return { data: data.data !== undefined ? data.data : data, error: null, count: data.count || 0 };
    } catch (e) {
      return { data: null, error: { message: e.message }, count: 0 };
    }
  }

  window.DB = {
    // sessions
    upsertSession: (s) => api("POST", "/api/sessions", s),
    updateSession: (sessionId, updates) =>
      api("PATCH", `/api/sessions?session_id=${encodeURIComponent(sessionId)}`, updates),
    listSessions: ({ ref, page = 0, pageSize = 5, role, notRole }) => {
      let q = `/api/sessions?ref=${encodeURIComponent(ref)}&page=${page}&pageSize=${pageSize}`;
      if (role) q += `&role=${encodeURIComponent(role)}`;
      if (notRole) q += `&notRole=${encodeURIComponent(notRole)}`;
      return api("GET", q);
    },
    deleteSessionById: (id) => api("DELETE", `/api/sessions/${id}`),
    deleteSessions: ({ ref, role, notRole }) => {
      let q = `/api/sessions?ref=${encodeURIComponent(ref)}`;
      if (role) q += `&role=${encodeURIComponent(role)}`;
      if (notRole) q += `&notRole=${encodeURIComponent(notRole)}`;
      return api("DELETE", q);
    },

    // calls
    insertCall: (c) => api("POST", "/api/calls", c),
    updateCall: (callId, updates) =>
      api("PATCH", `/api/calls?call_id=${encodeURIComponent(callId)}`, updates),
    listCalls: ({ ref, page = 0, pageSize = 5 }) =>
      api("GET", `/api/calls?ref=${encodeURIComponent(ref)}&page=${page}&pageSize=${pageSize}`),
    deleteCallById: (id) => api("DELETE", `/api/calls/${id}`),
    deleteCalls: ({ ref }) => api("DELETE", `/api/calls?ref=${encodeURIComponent(ref)}`),

    // messages
    insertMessage: (m) => api("POST", "/api/messages", m),
    listMessages: ({ ref, page = 0, pageSize = 5 }) =>
      api("GET", `/api/messages?ref=${encodeURIComponent(ref)}&page=${page}&pageSize=${pageSize}`),
    deleteMessageById: (id) => api("DELETE", `/api/messages/${id}`),
    deleteMessages: ({ ref }) => api("DELETE", `/api/messages?ref=${encodeURIComponent(ref)}`),

    // avatars (profile pictures — authenticated users only)
    // The stable GET URL for a session's avatar. Append &v=<token> to bust the
    // browser cache when the picture changes (upload returns such a URL).
    avatarUrl: (ref, sessionId) =>
      `/api/avatar?ref=${encodeURIComponent(ref)}&session_id=${encodeURIComponent(sessionId)}`,
    // Upload a File/Blob as the avatar for sessionId. Returns
    // { url, error }: url is a cache-busted URL on success.
    uploadAvatar: async (ref, sessionId, file) => {
      try {
        const fd = new FormData();
        fd.append("avatar", file);
        const resp = await fetch(
          `/api/avatar?ref=${encodeURIComponent(ref)}&session_id=${encodeURIComponent(sessionId)}`,
          { method: "POST", body: fd }
        );
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) return { url: null, error: { message: data.error || `HTTP ${resp.status}` } };
        return { url: data.url || null, error: null };
      } catch (e) {
        return { url: null, error: { message: e.message } };
      }
    },
    deleteAvatar: (ref, sessionId) =>
      api("DELETE", `/api/avatar?ref=${encodeURIComponent(ref)}&session_id=${encodeURIComponent(sessionId)}`),
  };

  connect();
  await whenOpen();
  console.log("[Config] Realtime + DB client initialized.");
})();
