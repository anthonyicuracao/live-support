// js/shared.js
// Shared utilities for both auth and guest pages.
// Exposed as window.Shared. Uses window.Realtime + window.DB (see config.js).

// VENDORED from hy-rag/public/js-lib.js (window.getInput) — keep in sync. A
// styled prompt() replacement (promise → string|false), brightness-aware. Used
// for the guest name prompt instead of the native prompt().
window.getInput = window.getInput || function getInput(message, defaultValue, placeholder, icon, container) {
  return new Promise((resolve) => {
    defaultValue = (defaultValue == null) ? '' : String(defaultValue);
    placeholder = (placeholder == null) ? '' : String(placeholder);
    icon = typeof icon === 'string' ? icon : null;
    const parent = (container instanceof Element) ? container : document.body;
    const bodyBg = window.getComputedStyle(parent).backgroundColor;
    const rgb = bodyBg.match(/\d+/g);
    const [r, g, b] = rgb ? rgb.map(Number) : [255, 255, 255];
    const isDark = (0.2126 * r + 0.7152 * g + 0.0722 * b) < 128;
    const overlayBg = isDark ? 'rgba(0,0,0,0.7)' : 'rgba(0,0,0,0.5)';
    const dialogBg = isDark ? '#2c2c2c' : '#ffffff';
    const textColor = isDark ? '#e0e0e0' : '#212529';
    const inputBg = isDark ? '#1e1e1e' : '#ffffff';
    const borderCol = isDark ? '#555555' : '#ced4da';
    const okBg = isDark ? '#3a6ea5' : '#2563eb';
    const iconMap = { EXCLAMATION: '⚠️', HAND: '✋', QUESTION: '❓' };
    const iconChar = icon ? (iconMap[icon] || icon) : '';
    const overlay = document.createElement('div');
    Object.assign(overlay.style, { position: 'fixed', inset: '0', background: overlayBg, zIndex: '2147483647', display: 'flex', alignItems: 'center', justifyContent: 'center' });
    const dialog = document.createElement('div');
    Object.assign(dialog.style, { background: dialogBg, color: textColor, borderRadius: '10px', padding: '20px', minWidth: '280px', maxWidth: '90vw', boxShadow: '0 10px 40px rgba(0,0,0,0.3)', fontFamily: 'system-ui, -apple-system, sans-serif' });
    const msg = document.createElement('div');
    msg.textContent = (iconChar ? iconChar + '  ' : '') + (message || '');
    Object.assign(msg.style, { marginBottom: '14px', fontSize: '15px', lineHeight: '1.4' });
    const input = document.createElement('input');
    input.type = 'text'; input.value = defaultValue; input.placeholder = placeholder;
    Object.assign(input.style, { width: '100%', boxSizing: 'border-box', padding: '10px 12px', fontSize: '15px', border: '1px solid ' + borderCol, borderRadius: '6px', background: inputBg, color: textColor, outline: 'none', marginBottom: '16px' });
    const row = document.createElement('div');
    Object.assign(row.style, { display: 'flex', gap: '8px', justifyContent: 'flex-end' });
    const cancelBtn = document.createElement('button');
    cancelBtn.textContent = 'Cancel';
    Object.assign(cancelBtn.style, { padding: '8px 16px', fontSize: '14px', borderRadius: '6px', cursor: 'pointer', border: '1px solid ' + borderCol, background: 'transparent', color: textColor });
    const okBtn = document.createElement('button');
    okBtn.textContent = 'OK';
    Object.assign(okBtn.style, { padding: '8px 16px', fontSize: '14px', borderRadius: '6px', cursor: 'pointer', border: 'none', background: okBg, color: '#ffffff' });
    function cleanup() { document.removeEventListener('keydown', keyH); overlay.remove(); }
    function submit() { const v = input.value; cleanup(); resolve(v); }
    function cancel() { cleanup(); resolve(false); }
    function keyH(e) { if (e.key === 'Enter') { e.preventDefault(); submit(); } else if (e.key === 'Escape') { e.preventDefault(); cancel(); } }
    okBtn.addEventListener('click', submit);
    cancelBtn.addEventListener('click', cancel);
    overlay.addEventListener('click', (e) => { if (e.target === overlay) cancel(); });
    document.addEventListener('keydown', keyH);
    row.appendChild(cancelBtn); row.appendChild(okBtn);
    dialog.appendChild(msg); dialog.appendChild(input); dialog.appendChild(row);
    overlay.appendChild(dialog); parent.appendChild(overlay);
    input.focus(); input.select();
  });
};

window.Shared = (() => {
  // ─── URL Params ──────────────────────────────────────────────────────────
  function getUrlParams() {
    const p = new URLSearchParams(window.location.search);
    return {
      auth: p.get("auth") === "true",
      admin: p.get("admin") === "true",
      ref: p.get("ref") || "",
      name: p.get("name") || "",
      email: p.get("email") || "Unknown",
    };
  }

  /**
   * If the `name` URL parameter is missing, prompt the user for their name
   * and reload the page with it added to the URL.
   * Returns true if a redirect is happening (caller should stop execution).
   */
  async function ensureNameParam() {
    const p = new URLSearchParams(window.location.search);
    if (!p.get("name")) {
      const name = window.getInput
        ? await window.getInput("Please enter your name:", "", "Your name")
        : prompt("Please enter your name:");
      if (name && name.trim()) {
        p.set("name", name.trim());
        window.location.search = p.toString();
      }
      return true; // redirect in progress or user cancelled
    }
    return false;
  }

  // ─── ID Generation ───────────────────────────────────────────────────────
  function generateId() {
    if (typeof crypto !== "undefined" && crypto.randomUUID) {
      return crypto.randomUUID();
    }
    return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
      const r = (Math.random() * 16) | 0;
      return (c === "x" ? r : (r & 0x3) | 0x8).toString(16);
    });
  }

  // ─── HTML Escaping ───────────────────────────────────────────────────────
  function escapeHtml(str) {
    return String(str).replace(
      /[&<>"']/g,
      (c) =>
        ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])
    );
  }

  // ─── Avatar markup ─────────────────────────────────────────────────────────
  // Returns a small avatar element: the uploaded picture if `picture` is set,
  // otherwise a circular placeholder showing the name's first initial. `size`
  // adds a modifier class (e.g. "avatar--lg") so callers can scale it via CSS.
  // All inputs are escaped — `picture` is a same-origin /api/avatar URL.
  function avatarHtml(name, picture, size) {
    const sizeClass = size ? ` avatar--${escapeHtml(size)}` : "";
    const initial = (String(name || "?").trim().charAt(0) || "?").toUpperCase();
    if (picture) {
      return `<span class="avatar${sizeClass}"><img class="avatar__img" src="${escapeHtml(picture)}" alt="" /></span>`;
    }
    return `<span class="avatar avatar--placeholder${sizeClass}">${escapeHtml(initial)}</span>`;
  }

  // ─── Dashboard refresh ───────────────────────────────────────────────────
  // The Go server broadcasts a "refresh" event on dashboard:<ref> after every
  // DB write, so client-side notification is no longer needed. Kept as a
  // no-op so existing call sites don't break.
  let currentRef = "";
  function rememberRef(ref) {
    if (ref) currentRef = ref;
  }
  function knownRef() {
    return currentRef || getUrlParams().ref || "";
  }
  async function notifyDashboardRefresh(_ref, _table) {
    // Handled server-side now. No-op.
  }

  // ─── Session Management ──────────────────────────────────────────────────
  async function createSession({ sessionId, ref, email, name, role, hasCamera, hasMic }) {
    rememberRef(ref);
    const { error } = await window.DB.upsertSession({
      session_id: sessionId,
      ref,
      email,
      name,
      role,
      status: "available",
      has_camera: hasCamera,
      has_mic: hasMic,
    });
    if (error) console.error("[Session] Create error:", error.message);
  }

  async function updateSessionStatus(sessionId, status) {
    const { error } = await window.DB.updateSession(sessionId, { status });
    if (error) console.error("[Session] Update status error:", error.message);
  }

  async function updateSessionCapabilities(sessionId, hasCamera, hasMic) {
    const { error } = await window.DB.updateSession(sessionId, {
      has_camera: hasCamera,
      has_mic: hasMic,
    });
    if (error) console.error("[Session] Update capabilities error:", error.message);
  }

  function setupHeartbeat(sessionId) {
    return setInterval(async () => {
      // Empty PATCH bumps last_seen_at server-side.
      const { error } = await window.DB.updateSession(sessionId, {});
      if (error) console.error("[Heartbeat] Error:", error.message);
    }, 30000);
  }

  // ─── Media Permissions ───────────────────────────────────────────────────
  async function checkMediaPermissions() {
    const result = { hasMic: false, hasCamera: false };
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      result.hasMic = true;
      stream.getTracks().forEach((t) => t.stop());
    } catch (e) {
      return result; // No mic — stop here
    }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ video: true });
      result.hasCamera = true;
      stream.getTracks().forEach((t) => t.stop());
    } catch (e) {
      // No camera is OK
    }
    return result;
  }

  // ─── Presence ────────────────────────────────────────────────────────────
  function joinPresenceChannel(ref, sessionData, onSync) {
    const channel = window.Realtime.channel(`presence:${ref}`, {
      config: { presence: { key: sessionData.session_id } },
    });

    channel
      .on("presence", { event: "sync" }, () => {
        const state = channel.presenceState();
        // Deduplicate by session_id so the UI never shows a user twice.
        const seen = new Set();
        const users = Object.values(state)
          .flat()
          .filter((u) => {
            if (seen.has(u.session_id)) return false;
            seen.add(u.session_id);
            return true;
          });
        onSync(users);
      })
      .subscribe(async (status) => {
        if (status === "SUBSCRIBED") {
          await channel.track(sessionData);
        }
      });

    return channel;
  }

  async function updatePresence(channel, sessionData) {
    try {
      await channel.track(sessionData);
    } catch (e) {
      console.error("[Presence] Update error:", e.message);
    }
  }

  // ─── Inbox (point-to-point messaging) ───────────────────────────────────
  // Each user subscribes to their own inbox channel to receive call invitations.
  // announcePresence: write-only presence, for a guest.
  //
  // Presence is asymmetric now — a directory everyone writes and only agents
  // read. A visitor must still announce itself or it would never appear in the
  // console's waiting list, but it no longer subscribes, because reading the
  // roster back was the leak. The channel object is returned so the caller can
  // keep updating its own entry.
  function announcePresence(ref, sessionData) {
    const channel = window.Realtime.channel(`presence:${ref}`);
    // Deliberately no .subscribe() — track() alone is the write.
    channel.track(sessionData);
    return channel;
  }

  // guestSession: mint a visitor's session id AND the capability for their
  // private inbox, together. Server-minted because an agent can see a session
  // id in presence; if the client picked it, seeing it would be enough to ask
  // for its token.
  async function guestSession(ref) {
    const resp = await fetch("/api/guest/session", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ref }),
    });
    if (!resp.ok) return null;
    return await resp.json(); // { sessionId, token, channel }
  }

  // ─── Conversations (private, capability-gated) ──────────────────────────
  //
  // Replaces the inbox:<session_id> carrier. The old model made the channel
  // NAME the capability, which only worked while an agent's session_id stayed
  // private — and /api/agents/available later published it. Now the channel is
  // keyed by a random conversation id that appears in no public response, and
  // using it requires a token issued only to the two participants.
  //
  // Nothing here ever puts a token in a URL. Links get shared, logged, and leak
  // through referrer headers; the capability is minted over POST after the
  // guest has chosen who and how.

  // Guest side: ask the server to create a conversation. The server decides
  // whether it may exist (agent available, takes this modality, has chat
  // capacity) and mints the guest's capability.
  async function startConversation({ ref, agentUserId, callType, guestSession, guestName }) {
    const resp = await fetch("/api/conversation/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ref, agentUserId, callType, guestSession, guestName }),
    });
    if (!resp.ok) {
      let reason = "unavailable";
      try { reason = (await resp.json()).error || reason; } catch (e) { /* keep default */ }
      return { error: reason, status: resp.status };
    }
    return await resp.json(); // { cid, token, channel }
  }

  // Agent side: open contact with a visitor. The server creates the
  // conversation and delivers the invitation (with the visitor's own
  // capability) to their private inbox, because an agent holds no grant for
  // someone else's inbox and must not be able to write into one.
  async function inviteGuest({ guestSession, guestName, callType, callerName, callId }) {
    const resp = await fetch("/api/conversation/invite", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ guestSession, guestName, callType, callerName, callId }),
    });
    if (!resp.ok) return { error: "invite failed", status: resp.status };
    return await resp.json(); // { cid, token, channel, callId }
  }

  // Agent side: exchange a conversation id for this agent's capability. The
  // server issues it only for a conversation they actually own.
  async function agentConversationToken(cid) {
    const resp = await fetch(`/api/conversation/token?cid=${encodeURIComponent(cid)}`);
    if (!resp.ok) return { error: "not found", status: resp.status };
    return await resp.json(); // { cid, token, channel, callType, guestName }
  }

  // Subscribe to a conversation. The token rides on the channel so it can be
  // re-presented on reconnect — grants belong to the connection, not the name.
  function openConversation({ channel, token }, handlers = {}) {
    const ch = window.Realtime.channel(channel, { token });
    if (handlers.onMessage) ch.on("broadcast", { event: "message" }, ({ payload }) => handlers.onMessage(payload));
    if (handlers.onSignal) ch.on("broadcast", { event: "signal" }, ({ payload }) => handlers.onSignal(payload));
    ch.subscribe(handlers.onStatus);
    return ch;
  }

  // Send a chat message: persisted first, then broadcast live. Persist-then-
  // broadcast rather than the reverse, so a message the other side sees is
  // always one the transcript already has — an agent woken by push must never
  // find a gap where a delivered message should be.
  async function sendConversationMessage({ ref, cid, token, sender, body }) {
    const resp = await fetch("/api/conversation/message", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ref, cid, token, sender, body }),
    });
    if (!resp.ok) return { error: "send failed", status: resp.status };
    const saved = await resp.json();
    await window.Realtime.publish(`conv:${cid}`, "message", saved.message, { token });
    return saved;
  }

  // The transcript, so a fresh console shows what the guest already said.
  async function loadTranscript({ ref, cid, token }) {
    const qs = new URLSearchParams({ ref, cid, token });
    const resp = await fetch(`/api/conversation/messages?${qs}`);
    if (!resp.ok) return { messages: [] };
    return await resp.json();
  }

  async function endConversation({ ref, cid, token }) {
    try {
      await fetch("/api/conversation/end", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ref, cid, token }),
      });
    } catch (e) { /* best effort: the governor also ages rows out */ }
  }

  function subscribeToInbox(sessionId, onMessage) {
    const channel = window.Realtime.channel(`inbox:${sessionId}`);
    channel
      .on("broadcast", { event: "message" }, ({ payload }) => {
        onMessage(payload);
      })
      .subscribe();
    return channel;
  }

  async function sendToInbox(targetSessionId, data) {
    // The in-process hub relays broadcasts to current subscribers without
    // requiring the sender to subscribe first — no subscribe/publish race.
    try {
      const ok = await window.Realtime.publish(`inbox:${targetSessionId}`, "message", data);
      if (!ok) console.warn("[Inbox] Send failed (socket not open)");
    } catch (e) {
      console.error("[Inbox] sendToInbox error:", e.message);
    }
  }

  // ─── Instant Messaging (text chat over the inbox channel) ───────────────
  // IM reuses the same point-to-point inbox channel as call invitations. A
  // chat line is just an inbox payload with type:"im", so a recipient's
  // existing subscribeToInbox handler can branch on payload.type.
  //
  // Security note: the carrier is the recipient's own inbox channel keyed by
  // their session_id. A guest therefore only ever learns an admin's
  // session_id because that admin messaged them first — guests are never sent
  // the admin roster and cannot enumerate or cold-message admins.
  async function sendIM(targetSessionId, { fromId, fromName, fromRole, fromPicture, text }) {
    return sendToInbox(targetSessionId, {
      type: "im",
      fromId,
      fromName,
      fromRole,
      fromPicture: fromPicture || "",
      text,
      ts: new Date().toISOString(),
    });
  }

  // ─── Call Signaling Channel ──────────────────────────────────────────────
  function setupCallChannel(callId, onMessage) {
    const channel = window.Realtime.channel(`call:${callId}`);
    channel
      .on("broadcast", { event: "signal" }, ({ payload }) => {
        onMessage(payload);
      })
      .subscribe();
    return channel;
  }

  async function sendCallSignal(callChannel, data) {
    try {
      await callChannel.send({
        type: "broadcast",
        event: "signal",
        payload: data,
      });
    } catch (e) {
      console.error("[Signal] Send error:", e.message);
    }
  }

  // ─── ICE Config ──────────────────────────────────────────────────────────
  async function getIceConfig() {
    try {
      const resp = await fetch("/ice-config");
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      return await resp.json();
    } catch (e) {
      console.warn("[ICE] Falling back to public STUN:", e.message);
      return {
        iceServers: [
          { urls: "stun:stun.cloudflare.com:3478" },
          { urls: "stun:stun.l.google.com:19302" },
        ],
      };
    }
  }

  // ─── WebRTC ──────────────────────────────────────────────────────────────
  function createPeerConnection({ iceConfig, onIceCandidate, onTrack, onConnectionStateChange }) {
    const pc = new RTCPeerConnection(iceConfig);

    pc.onicecandidate = ({ candidate }) => {
      if (candidate) onIceCandidate(candidate);
    };

    pc.ontrack = ({ streams }) => {
      if (streams && streams[0]) onTrack(streams[0]);
    };

    if (onConnectionStateChange) {
      pc.onconnectionstatechange = () => onConnectionStateChange(pc.connectionState);
    }

    return pc;
  }

  // ─── Call Records ─────────────────────────────────────────────────────────
  async function createCallRecord({
    callId,
    ref,
    callerSessionId,
    callerName,
    calleeSessionId,
    calleeName,
    callType,
  }) {
    const { error } = await window.DB.insertCall({
      call_id: callId,
      ref,
      caller: callerSessionId,
      caller_name: callerName,
      callee: calleeSessionId,
      callee_name: calleeName,
      type: callType,
    });
    if (error) console.error("[Call] Create record error:", error.message);
  }

  async function updateCallRecord(callId, updates) {
    const { error } = await window.DB.updateCall(callId, updates);
    if (error) console.error("[Call] Update record error:", error.message);
  }

  // ─── Message Records ──────────────────────────────────────────────────────
  async function createMessage({ ref, name, contact, message }) {
    const messageId = generateId();
    const { error } = await window.DB.insertMessage({
      message_id: messageId,
      ref,
      name,
      contact,
      message,
    });
    if (error) {
      console.error("[Message] Create error:", error.message);
      return false;
    }
    return true;
  }

  // ─── Ringtone ─────────────────────────────────────────────────────────────
  let ringtoneAudio = null;

  function ensureRingtone() {
    if (!ringtoneAudio) {
      ringtoneAudio = new Audio("/public/ring.mp3");
      ringtoneAudio.loop = true;
    }
    return ringtoneAudio;
  }

  // Browsers block audio that isn't started from a user gesture. Call this from a
  // click (e.g. Go-Available) to UNLOCK playback — a silenced play/pause — so the
  // ring can sound when a call later arrives asynchronously. Silenced through
  // BOTH muted and volume=0: some browsers ignore one or the other on an audio
  // element that isn't in the DOM, which made priming audibly blip the ringtone.
  // Primed at most once per page — repeat toggles never replay it.
  let ringtonePrimed = false;
  function primeRingtone() {
    const a = ensureRingtone();
    if (ringtonePrimed || !a.paused) return;
    ringtonePrimed = true;
    a.muted = true;
    a.volume = 0;
    a.play()
      .then(() => {
        // If a real ring started while priming was in flight, leave it alone.
        if (!a.muted && a.volume > 0) return;
        a.pause();
        a.currentTime = 0;
        a.muted = false;
        a.volume = 1;
      })
      .catch(() => {
        ringtonePrimed = false; // gesture didn't unlock — allow a retry
        a.muted = false;
        a.volume = 1;
      });
  }

  // Resolves true when the ring is audibly playing, false when the browser
  // blocked it (no user gesture yet — e.g. a console that resumed Available on
  // load). Callers use false to escalate to an OS notification instead.
  function playRingtone() {
    const a = ensureRingtone();
    a.muted = false;
    a.volume = 1;
    a.currentTime = 0;
    return a.play().then(
      () => true,
      (e) => {
        console.warn("[Ringtone] Play failed:", e.message);
        return false;
      }
    );
  }

  // playNotice: one short blip for an arriving chat message.
  //
  // Deliberately NOT the ringtone: a ring means "answer me now, there is a
  // deadline", and repeating it for typed messages is nagging. Synthesised via
  // WebAudio rather than an asset so it cannot 404, and wrapped because a
  // browser with no gesture yet will refuse to start an AudioContext — a
  // silent notice is acceptable, a thrown error in the delivery path is not.
  let noticeCtx = null;
  function playNotice() {
    try {
      const Ctx = window.AudioContext || window.webkitAudioContext;
      if (!Ctx) return false;
      noticeCtx = noticeCtx || new Ctx();
      if (noticeCtx.state === "suspended") noticeCtx.resume();
      const now = noticeCtx.currentTime;
      const osc = noticeCtx.createOscillator();
      const gain = noticeCtx.createGain();
      osc.type = "sine";
      osc.frequency.setValueAtTime(880, now);
      osc.frequency.setValueAtTime(1175, now + 0.09);
      gain.gain.setValueAtTime(0.0001, now);
      gain.gain.exponentialRampToValueAtTime(0.18, now + 0.015);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + 0.22);
      osc.connect(gain).connect(noticeCtx.destination);
      osc.start(now);
      osc.stop(now + 0.24);
      return true;
    } catch (e) {
      return false; // no audio available; the unread badge still tells the story
    }
  }

  function stopRingtone() {
    if (!ringtoneAudio) return;
    ringtoneAudio.pause();
    ringtoneAudio.currentTime = 0;
    // Keep the element so it stays primed for the next call.
  }

  // ─── Desktop notifications (backgrounded-tab fallback for the ring) ────────
  // A hidden/blurred tab throttles audio, so the ring may not be heard until
  // the tab is focused. Request permission on a user gesture (Go-Available),
  // then raise an OS notification when a call arrives while the tab is hidden.
  function requestNotifyPermission() {
    try {
      if ("Notification" in window && Notification.permission === "default") {
        Notification.requestPermission().catch(() => {});
      }
    } catch (e) {}
  }

  let activeCallNotification = null;
  function notifyIncomingCall(title, body) {
    try {
      if (!("Notification" in window) || Notification.permission !== "granted") return;
      // Only nag when the tab isn't already in front of the user.
      if (!document.hidden && document.hasFocus()) return;
      activeCallNotification = new Notification(title, {
        body,
        icon: "/public/favicon.svg",
        tag: "incoming-call",
        renotify: true,
        requireInteraction: true,
      });
      activeCallNotification.onclick = () => {
        try { window.focus(); } catch (e) {}
        if (activeCallNotification) activeCallNotification.close();
      };
    } catch (e) {}
  }
  function clearIncomingNotification() {
    try { if (activeCallNotification) { activeCallNotification.close(); activeCallNotification = null; } } catch (e) {}
    // Also close any "incoming-call" notifications raised through the service
    // worker (by a push, or by the page's silent-ring escalation), so a call
    // answered here doesn't leave a stale banner behind.
    try {
      if (navigator.serviceWorker && navigator.serviceWorker.getRegistration) {
        navigator.serviceWorker.getRegistration().then((reg) => {
          if (!reg || !reg.getNotifications) return;
          reg.getNotifications({ tag: "incoming-call" }).then((list) => {
            list.forEach((n) => n.close());
          });
        }).catch(() => {});
      }
    } catch (e) {}
  }

  // ─── Device enumeration (mic/camera pickers) ──────────────────────────────
  // Device labels are only exposed once a media permission has been granted,
  // and enumerateDevices() can briefly return blank labels right after the
  // grant. listInputDevices() returns the current list; callers should re-run
  // it on a short retry and on `devicechange` (see populateDevices in the page
  // scripts) so the names fill in reliably.
  async function listInputDevices() {
    let devices = [];
    try { devices = await navigator.mediaDevices.enumerateDevices(); } catch (e) { return { mics: [], cams: [] }; }
    return {
      mics: devices.filter((d) => d.kind === "audioinput"),
      cams: devices.filter((d) => d.kind === "videoinput"),
    };
  }

  // ─── Time Formatting ──────────────────────────────────────────────────────
  function formatWaitTime(isoString) {
    const ms = Date.now() - new Date(isoString).getTime();
    const totalSeconds = Math.max(0, Math.floor(ms / 1000));
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`;
  }

  function formatDuration(seconds) {
    if (seconds == null) return "";
    const m = Math.floor(seconds / 60);
    const s = seconds % 60;
    return `${String(m).padStart(2, "0")}m ${String(s).padStart(2, "0")}s`;
  }

  // ─── Section Visibility ────────────────────────────────────────────────────
  function showSection(selector) {
    const el = document.querySelector(selector);
    if (el) el.style.display = "";
  }

  function hideSection(selector) {
    const el = document.querySelector(selector);
    if (el) el.style.display = "none";
  }

  function hideAllSections() {
    document.querySelectorAll("main > section").forEach((s) => {
      s.style.display = "none";
    });
  }

  // ─── Canonical agent state ────────────────────────────────────────────────
  // The single source of truth for how an agent's state is derived and labeled,
  // so every view (the Agents list, future dashboards) agrees. Two orthogonal
  // axes: connection (Online = holds a live presence entry) and availability
  // (the agent's own toggle). An agent with no live presence is Offline, but
  // may still be Reachable via push (durable availability + a push sub) — the
  // closed-laptop case. A stale Offline·Reachable record that never answers is
  // the "ghost".
  //   presenceMember: the agent's live presence entry, or null/undefined if not
  //                   currently connected. Its `status` is "available" | "paused"
  //                   | "in-call".
  //   durablyReachable: true if the agent is durably available AND push-subscribed
  //                     (i.e. appears in GET /api/agents/available) — only
  //                     meaningful when offline.
  // Returns { online, reachable, key, label } where key is a stable token for
  // styling and label is the human string.
  function agentState(presenceMember, durablyReachable) {
    if (presenceMember) {
      const a = presenceMember.status;
      if (a === "in-call") return { online: true, reachable: false, key: "in-call", label: "In call" };
      if (a === "available") return { online: true, reachable: true, key: "available", label: "Available" };
      return { online: true, reachable: false, key: "paused", label: "Paused" };
    }
    if (durablyReachable) {
      return { online: false, reachable: true, key: "reachable", label: "Reachable (push)" };
    }
    return { online: false, reachable: false, key: "offline", label: "Offline" };
  }

  return {
    agentState,
    getUrlParams,
    ensureNameParam,
    generateId,
    escapeHtml,
    avatarHtml,
    createSession,
    updateSessionStatus,
    updateSessionCapabilities,
    setupHeartbeat,
    checkMediaPermissions,
    joinPresenceChannel,
    updatePresence,
    subscribeToInbox,
    sendToInbox,
    announcePresence,
    guestSession,
    inviteGuest,
    startConversation,
    agentConversationToken,
    openConversation,
    endConversation,
    sendConversationMessage,
    loadTranscript,
    sendIM,
    setupCallChannel,
    sendCallSignal,
    getIceConfig,
    createPeerConnection,
    createCallRecord,
    updateCallRecord,
    createMessage,
    notifyDashboardRefresh,
    primeRingtone,
    playRingtone,
    stopRingtone,
    playNotice,
    requestNotifyPermission,
    notifyIncomingCall,
    clearIncomingNotification,
    listInputDevices,
    formatWaitTime,
    formatDuration,
    showSection,
    hideSection,
    hideAllSections,
  };
})();
