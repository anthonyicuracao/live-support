// js/auth.js
// Auth user page logic. Runs on auth.html.

(async function () {
  await window.configReady;
  if (!window.Realtime || !window.DB) return; // Config failed to load

  const S = window.Shared;

  // ─── UI helpers for loading / denied screens ───────────────────────────
  const loadingEl = document.getElementById("auth-loading");
  const deniedEl  = document.getElementById("auth-denied");
  const mainEl    = document.querySelector(".auth-user");

  function showDenied(reason) {
    if (loadingEl) loadingEl.style.display = "none";
    if (deniedEl)  {
      deniedEl.style.display = "flex";
      const reasonEl = document.getElementById("auth-denied-reason");
      if (reasonEl && reason) reasonEl.textContent = reason;
    }
  }

  function showApp() {
    if (loadingEl) loadingEl.style.display = "none";
    if (mainEl)    mainEl.style.display = "";
  }

  // ─── Session loss must never be silent ─────────────────────────────────
  // The failure this exists to kill: the session ends, the console keeps
  // rendering "Available", WS presence keeps the agent looking live, and every
  // authed call 401s unnoticed — because a 401 is a SUCCESSFUL fetch and does
  // not throw. The availability touch stops, updated_at freezes, and about a
  // day later discoveryFreshness ages the agent out of guest discovery while
  // their screen still says they are on duty. They find out at the next reload.
  //
  // So: one place decides the session is gone, it latches, and it is loud.
  let sessionLost = false;

  // Self-contained rather than reusing the nested redirectToLogin below: this
  // runs from fetch callbacks anywhere in the file, so it must not depend on
  // that function's scope. Preserves ?ref= the same way it does.
  function loginURL() {
    const ref = new URLSearchParams(window.location.search).get("ref");
    return "/login" + (ref ? "?ref=" + encodeURIComponent(ref) : "");
  }

  function onSessionLost() {
    if (sessionLost) return; // latch — one redirect, one message
    sessionLost = true;
    showDenied(
      "Your session has ended, so you are no longer reachable by visitors. " +
      "Signing you back in…"
    );
    // Give the message a beat to be read, then go somewhere that can fix it.
    setTimeout(() => { window.location.href = loginURL(); }, 1500);
  }

  // guardAuth wraps an authed fetch response. Returns true when the response is
  // usable; false means the session is gone and the caller should stop.
  // Every authed fetch in this file goes through it — a new endpoint that
  // forgets to is exactly how the silent failure came back.
  function guardAuth(resp) {
    if (!resp) return false;          // network error — caller decides
    if (resp.status === 401 || resp.status === 403) {
      onSessionLost();
      return false;
    }
    return resp.ok;
  }

  // ─── One-time passcode gate (optional) ─────────────────────────────────
  // When the server is started with AGENT_PASSCODE set, agents must enter the
  // code once per browser before the dashboard is revealed. The code is
  // verified server-side (/api/agent-gate) and never shipped to the page; on
  // success we remember it in localStorage so it isn't asked again on this
  // device. If no passcode is configured, this resolves immediately and the
  // page behaves exactly as before.
  const GATE_STORAGE = "agent-gate-passed";
  function gateAlreadyPassed() {
    try { return localStorage.getItem(GATE_STORAGE) === "true"; }
    catch (e) { return false; }
  }
  function rememberGatePassed() {
    try { localStorage.setItem(GATE_STORAGE, "true"); } catch (e) { /* best effort */ }
  }

  // Resolves true once the gate is satisfied (or not required), false if the
  // user can't be let through (only on an unexpected error path).
  async function passPasscodeGate() {
    let required = false;
    try {
      const r = await fetch("/api/agent-gate");
      if (r.ok) required = !!(await r.json()).required;
    } catch (e) {
      // Server doesn't expose the gate (older build) → behave as before.
      return true;
    }
    if (!required || gateAlreadyPassed()) return true;

    const gateEl   = document.getElementById("auth-gate");
    const formEl   = document.getElementById("auth-gate-form");
    const inputEl  = document.getElementById("auth-gate-input");
    const errorEl  = document.getElementById("auth-gate-error");
    if (!gateEl || !formEl || !inputEl) {
      // Markup missing — fail closed (the gate was requested but we can't show
      // it), so the dashboard stays hidden.
      showDenied("This page requires a passcode but the entry form is unavailable.");
      return false;
    }

    if (loadingEl) loadingEl.style.display = "none";
    gateEl.style.display = "flex";
    inputEl.focus();

    return new Promise((resolve) => {
      formEl.addEventListener("submit", async (e) => {
        e.preventDefault();
        const code = inputEl.value;
        if (!code) return;
        if (errorEl) errorEl.textContent = "";
        let ok = false;
        try {
          const r = await fetch("/api/agent-gate", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ code }),
          });
          if (r.ok) ok = !!(await r.json()).ok;
        } catch (err) { /* ok stays false */ }
        if (ok) {
          rememberGatePassed();
          gateEl.style.display = "none";
          if (loadingEl) loadingEl.style.display = "flex";
          resolve(true);
        } else {
          if (errorEl) errorEl.textContent = "Incorrect passcode. Please try again.";
          inputEl.value = "";
          inputEl.focus();
        }
      });
    });
  }

  // ─── Identity / authentication ─────────────────────────────────────────
  // Identity comes from the server-side login session (cookie set by the
  // /login page; see auth.go on the server). /api/me reports the signed-in
  // user:
  //
  //   { ref, username, name, email, isAdmin, mustChangePassword, csrf }
  //
  // No valid session → the server already redirects /auth.html to /login;
  // the same redirect is repeated here as a fallback (e.g. for a session
  // that expires while the page is open).
  // Dev-mode bypass → identity comes from URL params (local testing only).

  // Gate the page on the optional one-time passcode before anything else.
  // No-op (resolves immediately) when AGENT_PASSCODE isn't configured.
  if (!(await passPasscodeGate())) return;

  const urlParams = new URLSearchParams(window.location.search);

  // ─── Dev-mode bypass (local testing only) ──────────────────────────────
  // Active only when BOTH the URL has ?dev=true AND the server was started
  // with DEV_MODE=true. Identity then comes from URL params:
  //   auth.html?dev=true&ref=test.example.com&name=Agent&admin=true
  let devBypass = false;
  if (urlParams.get("dev") === "true") {
    try {
      const r = await fetch("/api/dev");
      const j = await r.json();
      devBypass = !!j.devMode;
    } catch (e) { /* server doesn't support dev mode */ }
    if (!devBypass) {
      showDenied("Dev mode requested but the server is not running with DEV_MODE=true.");
      return;
    }
  }

  function redirectToLogin() {
    const refParam = urlParams.get("ref");
    window.location.href =
      "/login" + (refParam ? "?ref=" + encodeURIComponent(refParam) : "");
  }

  // ─── Derive identity ───────────────────────────────────────────────────
  let identity;
  let me = null; // server session info (stays null under the dev bypass)
  if (devBypass) {
    identity = {
      ref: urlParams.get("ref") || "test.example.com",
      name: urlParams.get("name") || "Agent",
      email: urlParams.get("email") || "dev@example.com",
      isAdmin: urlParams.get("admin") === "true",
    };
  } else {
    let resp = null;
    try {
      resp = await fetch("/api/me");
    } catch (e) { /* network error — treated as signed out below */ }
    if (!resp || !resp.ok) {
      redirectToLogin();
      return;
    }
    me = await resp.json();
    if (me.mustChangePassword) {
      window.location.href = "/account/password";
      return;
    }
    identity = { ref: me.ref, name: me.name, email: me.email, isAdmin: me.isAdmin, userId: me.userId };
  }

  const ref     = identity.ref;
  const email   = identity.email || "";
  const name    = identity.name  || "Agent";
  const isAdmin = !!identity.isAdmin;
  const userId  = identity.userId || 0;

  if (!ref) {
    showDenied("Your account does not have a configured domain. Please contact your administrator.");
    return;
  }

  // Validation passed — reveal the app
  showApp();

  // ─── Greeting ──────────────────────────────────────────────────────────
  const greetingH1 = document.querySelector(".greeting h1");
  if (greetingH1) greetingH1.textContent = `Hello, ${name}!`;

  S.hideAllSections();
  S.showSection(".greeting");

  // ─── Account bar (manage users / change password / sign out) ───────────
  // Rendered only for real server sessions; the dev bypass has nothing to
  // sign out of. "Manage users" links to the per-tenant user-management page
  // (admins only). Sign-out posts the per-session CSRF token from /api/me.
  // Settings gear: everything that isn't moment-to-moment hides behind it.
  (function wireSettingsGear() {
    const gear = document.getElementById("settings-toggle");
    const panel = document.getElementById("settings-panel");
    if (!gear || !panel) {
      console.warn("[UI] settings gear/panel missing — settings stay inline");
      return;
    }
    gear.addEventListener("click", () => {
      const open = panel.style.display !== "none";
      panel.style.display = open ? "none" : "";
      gear.setAttribute("aria-expanded", String(!open));
      gear.classList.toggle("settings-gear--open", !open);
    });
  })();

  // History (Calls) collapses by default — tap the header to expand/collapse.
  (function wireHistoryToggle() {
    const logs = document.querySelector(".logs");
    const toggle = logs?.querySelector(".logs-toggle");
    if (!logs || !toggle) return; // no-op if History isn't rendered
    toggle.addEventListener("click", () => {
      const collapsed = logs.classList.toggle("logs--collapsed");
      toggle.setAttribute("aria-expanded", String(!collapsed));
    });
  })();

  // Messages auto-expands on unread (see refreshMessagesUnread); the header is
  // also a manual toggle so the agent can open it to review already-read notes.
  (function wireMessagesToggle() {
    const section = document.querySelector(".messages");
    const toggle = section?.querySelector(".messages-toggle");
    if (!section || !toggle) return;
    toggle.addEventListener("click", () => {
      const collapsed = section.classList.toggle("messages--collapsed");
      toggle.setAttribute("aria-expanded", String(!collapsed));
    });
  })();

  (function renderAccountBar() {
    if (!me) return;
    // The account links live inside the settings panel (fall back to the
    // greeting card if the panel is missing).
    const greeting = document.getElementById("settings-panel") || document.querySelector(".greeting");
    if (!greeting) return;
    const bar = document.createElement("div");
    bar.className = "account-bar";
    bar.style.cssText =
      "margin-top:16px;display:flex;gap:14px;align-items:center;flex-wrap:wrap;";
    if (isAdmin) {
      const usersLink = document.createElement("a");
      usersLink.href = "/users";
      usersLink.textContent = "Manage users";
      bar.appendChild(usersLink);
    }
    const pwLink = document.createElement("a");
    pwLink.href = "/account/password";
    pwLink.textContent = "Change password";
    bar.appendChild(pwLink);
    const form = document.createElement("form");
    form.method = "POST";
    form.action = "/logout";
    form.style.cssText = "display:inline;margin:0;";
    const csrfInput = document.createElement("input");
    csrfInput.type = "hidden";
    csrfInput.name = "csrf";
    csrfInput.value = me.csrf || "";
    const signOutBtn = document.createElement("button");
    signOutBtn.type = "submit";
    signOutBtn.textContent = "Sign out";
    form.appendChild(csrfInput);
    form.appendChild(signOutBtn);
    bar.appendChild(form);
    greeting.appendChild(bar);
  })();

  // ─── Media permissions are deferred to "Go Available" ─────────────────
  // We no longer prompt for camera/mic on page load. `perms` is filled in when
  // the agent goes Available (requesting ONLY the modes they picked); until then
  // the agent is Paused and not callable, so nothing needs media access yet.
  let perms = { hasMic: false, hasCamera: false };

  // ─── Session setup ─────────────────────────────────────────────────────
  // Reuse the session ID across page refreshes so the auth user's presence
  // entry is replaced (same key) rather than duplicated.
  let sessionId = sessionStorage.getItem("authSessionId");
  if (!sessionId) {
    sessionId = S.generateId();
    sessionStorage.setItem("authSessionId", sessionId);
  }

  // ─── Availability toggle (durable, server-side) ───────────────────────
  // "Available until Pause or log out": the toggle's truth lives in the
  // agent_availability table, not this tab. It survives tab close, browser
  // quit, and sleep — a push-subscribed agent stays discoverable and ringable
  // the whole time. The console restores the toggle from the server on load;
  // no live media stream is needed while idle, because accepting a call
  // acquires media from the Accept click itself (a user gesture, so Safari
  // prompts cleanly even on a freshly reopened tab).
  let isAvailable = false;
  // A connected agent is always Online (they hold a live presence entry); this
  // is the availability SUB-STATE, not the connection. "paused" — not
  // "offline" — because a paused agent is still connected, just not taking
  // calls. Genuine disconnection is leaving the presence channel entirely.
  function availabilityStatus() {
    return isAvailable ? "available" : "paused";
  }
  // Push the durable state (and this tab's session id) to the server. Called
  // on every toggle flip and when resuming availability in a fresh tab, so
  // REST-discovered rings always route to the most recent live session.
  // touchOnly: update the session id/display fields of an already-available
  // record without touching the availability bit — safe to run on load, where
  // it could otherwise race a quick Pause click and revive availability.
  async function postAvailability(touchOnly) {
    try {
      const resp = await fetch("/api/availability", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          available: isAvailable,
          touch: !!touchOnly,
          sessionId,
          displayName,
          hasCamera: !!perms.hasCamera,
          picture: picture || "",
          onlineSince: presenceData.online_since,
          // Omitted on a touch: a touch re-points the session id of an already
          // available record and must not restate intent, or a slow in-flight
          // touch could revive a modality the agent has just turned off.
          modes: touchOnly ? undefined : currentModes(),
        }),
      });
      // A 401 here is the single most consequential failure in the console: it
      // means this touch did not land, so updated_at stops moving and the agent
      // silently ages out of guest discovery while still showing Available.
      // guardAuth turns it into a visible signed-out state.
      guardAuth(resp);
    } catch (e) {
      // A genuine network error is transient and self-correcting — live WS
      // presence still covers open-tab callability, and the next touch retries.
      // Deliberately NOT treated as session loss: a flaky wifi moment must not
      // sign an agent out.
    }
  }

  // ─── Video-mode preference + deferred media acquisition ────────────────
  // The agent picks whether they'll take video calls BEFORE going available;
  // permission is requested only at Go-Available, for only the picked modes.
  const STORAGE_VIDEO = "auth-user-video";
  function loadVideoPref() {
    try { return localStorage.getItem(STORAGE_VIDEO) === "true"; } catch (e) { return false; }
  }
  function saveVideoPref(on) { try { localStorage.setItem(STORAGE_VIDEO, String(on)); } catch (e) {} }
  let wantsVideo = loadVideoPref();

  // The live stream the agent holds while Available, acquired here with the
  // selected devices and REUSED by the call (so accepting doesn't re-prompt).
  // The picked mic/camera are persisted per-browser so the choice sticks
  // (deviceIds are stable per browser once a permission has been granted).
  const STORAGE_MIC = "auth-user-mic";
  const STORAGE_CAM = "auth-user-cam";
  let liveStream = null;
  let selectedMicId = "";
  let selectedCamId = "";
  try { selectedMicId = localStorage.getItem(STORAGE_MIC) || ""; } catch (e) {}
  try { selectedCamId = localStorage.getItem(STORAGE_CAM) || ""; } catch (e) {}

  // Acquire mic (+ camera if wantVideo) using the chosen devices and keep it as
  // liveStream. Returns what was granted; falls back to audio-only if video is
  // blocked. Releases any previous live stream first (e.g. on device change).
  async function acquireMedia(wantVideo) {
    const result = { hasMic: false, hasCamera: false };
    if (liveStream) { liveStream.getTracks().forEach((t) => t.stop()); liveStream = null; }
    // Try the saved device picks first, then fall back to defaults. A stale
    // pick (device unplugged, or its id rotated by the browser) must never
    // block going Available — and the picker UI already shows "Default" when
    // the pick is missing, so a hard `exact` failure here would look like a
    // toggle that inexplicably refuses to turn on.
    const audioPick = selectedMicId ? { deviceId: { exact: selectedMicId } } : true;
    const videoPick = wantVideo ? (selectedCamId ? { deviceId: { exact: selectedCamId } } : true) : false;
    const attempts = [{ audio: audioPick, video: videoPick }];
    if (selectedMicId || (wantVideo && selectedCamId)) {
      attempts.push({ audio: true, video: wantVideo });
    }
    if (wantVideo) {
      // Camera blocked/missing — stay answerable with audio only.
      attempts.push({ audio: audioPick, video: false });
      if (selectedMicId) attempts.push({ audio: true, video: false });
    }
    for (const c of attempts) {
      try {
        liveStream = await navigator.mediaDevices.getUserMedia(c);
        break;
      } catch (e) {
        liveStream = null;
      }
    }
    if (liveStream) {
      result.hasMic = liveStream.getAudioTracks().length > 0;
      result.hasCamera = liveStream.getVideoTracks().length > 0;
      dropStalePicks();
    }
    return result;
  }

  // If acquisition landed on a different device than the saved pick, the pick
  // is stale — forget it so the pickers, localStorage, and future acquisitions
  // agree instead of silently disagreeing forever.
  function dropStalePicks() {
    if (!liveStream) return;
    const a = liveStream.getAudioTracks()[0];
    if (selectedMicId && a && a.getSettings().deviceId !== selectedMicId) {
      selectedMicId = "";
      try { localStorage.removeItem(STORAGE_MIC); } catch (e) {}
      populateDevices();
    }
    const v = liveStream.getVideoTracks()[0];
    if (selectedCamId && v && v.getSettings().deviceId !== selectedCamId) {
      selectedCamId = "";
      try { localStorage.removeItem(STORAGE_CAM); } catch (e) {}
      populateDevices();
    }
  }

  // ─── Display name (public alias shown to guests) ───────────────────────
  // The real account `name` is kept for the dashboard greeting, session
  // record, and admin-side views. `displayName` is what guests see during
  // calls and chat. It defaults to the real name and is persisted per-browser
  // (like the availability toggle), scoped per ref so different tenants don't
  // collide.
  const STORAGE_DISPLAY_NAME = `auth-display-name:${ref}`;
  function loadDisplayName() {
    try {
      const v = localStorage.getItem(STORAGE_DISPLAY_NAME);
      return v && v.trim() ? v : name;
    } catch (e) {
      return name;
    }
  }
  function saveDisplayName(v) {
    try {
      if (v && v.trim()) localStorage.setItem(STORAGE_DISPLAY_NAME, v);
      else localStorage.removeItem(STORAGE_DISPLAY_NAME);
    } catch (e) {
      // Best-effort — the value still applies for this session.
    }
  }
  let displayName = loadDisplayName();

  // ─── Profile picture (avatar) ──────────────────────────────────────────
  // The uploaded image lives server-side (per-ref SQLite blob) keyed by this
  // session id. We persist the returned cache-busted URL per browser+ref so
  // the picture reappears after a refresh without re-uploading, and broadcast
  // it through presence so guests (during calls) and other agents (in the IM
  // roster) can render it. Only authenticated users reach this code path.
  const STORAGE_PICTURE = `auth-picture:${ref}`;
  function loadPicture() {
    try {
      return localStorage.getItem(STORAGE_PICTURE) || "";
    } catch (e) {
      return "";
    }
  }
  function savePicture(v) {
    try {
      if (v) localStorage.setItem(STORAGE_PICTURE, v);
      else localStorage.removeItem(STORAGE_PICTURE);
    } catch (e) {
      // Best-effort — value still applies for this session.
    }
  }
  let picture = loadPicture();
  // Filled from the server below; applied when the mode toggles are built.
  let savedModes = null;

  // Restore durable availability before presence is announced, so a reopened
  // console comes back Available (Ron-rule: until Pause or log out) without
  // re-acquiring media — Accept will do that from its own click gesture.
  try {
    const r = await fetch("/api/availability");
    if (r.ok) {
      const saved = await r.json();
      // Restore the modality choices too, or a reopened console would come back
      // Available while silently resetting what the agent had opted into.
      // The server returns sensible defaults (chat + audio) when there is no
      // row yet, so a first-time agent starts armed for everything but video.
      if (saved.modes) savedModes = saved.modes;
      if (saved.available) {
        isAvailable = true;
        perms.hasCamera = !!saved.hasCamera;
      }
    }
  } catch (e) {
    // Server unreachable — start Paused; the toggle still works.
  }

  const presenceData = {
    session_id: sessionId,
    user_id: userId, // stable identity so views can merge presence with the
                     // durable roster (a session_id churns across reopens)
    name: displayName,
    role: "auth",
    status: availabilityStatus(),
    has_camera: perms.hasCamera,
    has_mic: true,
    picture: picture,
    online_since: new Date().toISOString(),
  };

  await S.createSession({
    sessionId,
    ref,
    email,
    name,
    role: "auth",
    hasCamera: perms.hasCamera,
    hasMic: true,
  });
  // createSession always writes status=available; reconcile with the toggle
  // (a connected-but-paused agent is "paused", not "offline" — they're Online).
  if (!isAvailable) {
    await S.updateSessionStatus(sessionId, "paused");
  }

  // Reflect persisted state in the UI and wire up the change handler.
  const availabilityInput = document.getElementById("availability-toggle");
  const availabilityStateEl = document.getElementById("availability-state");
  const videoModeInput = document.getElementById("video-mode-toggle");
  const chatModeInput = document.getElementById("chat-mode-toggle");
  const audioModeInput = document.getElementById("audio-mode-toggle");

  // Per-modality intent. `isAvailable` stays the master switch ("am I
  // working"); these say what kind of work. An agent with a camera who is only
  // taking chat right now is a state the old single bit could not express.
  let modes = savedModes || { chat: true, audio: true, video: false };

  function currentModes() {
    return {
      chat: chatModeInput ? chatModeInput.checked : modes.chat,
      audio: audioModeInput ? audioModeInput.checked : modes.audio,
      video: videoModeInput ? videoModeInput.checked : modes.video,
    };
  }

  // "Available for chat, audio and video" — driven by what is actually
  // selected, so the switch never claims a modality the agent turned off, and
  // never omits chat.
  function availabilityLabelText(m) {
    const on = [];
    if (m.chat) on.push("chat");
    if (m.audio) on.push("audio");
    if (m.video) on.push("video");
    if (!on.length) return "Not accepting anything";
    const list =
      on.length === 1 ? on[0] : on.slice(0, -1).join(", ") + " and " + on[on.length - 1];
    return `Available for ${list}`;
  }

  function renderAvailabilityUI() {
    if (availabilityInput) availabilityInput.checked = isAvailable;
    if (availabilityStateEl) {
      availabilityStateEl.textContent = isAvailable ? "✅" : "🛑";
    }
    const labelEl = document.getElementById("availability-label");
    if (labelEl) labelEl.textContent = availabilityLabelText(currentModes());
    // The mode choice is locked while Available for the same reason it always
    // was: permissions are acquired at Go-Available for the picked modes, so
    // changing them mid-shift would need a re-acquire the UI has no place for.
    [chatModeInput, audioModeInput, videoModeInput].forEach((el) => {
      if (el) el.disabled = isAvailable;
    });
    if (chatModeInput) chatModeInput.checked = modes.chat;
    if (audioModeInput) audioModeInput.checked = modes.audio;
    if (videoModeInput) {
      videoModeInput.checked = wantsVideo;
      // Video additionally needs the hardware to exist. Capability and intent
      // are separate: no camera means the intent cannot be honoured.
      if (!perms.hasCamera) videoModeInput.disabled = true;
    }
  }
  renderAvailabilityUI();

  // Mode pickers: editable only while Paused; the video one is saved
  // per-browser as before.
  [chatModeInput, audioModeInput, videoModeInput].forEach((el) => {
    if (!el) return;
    el.addEventListener("change", () => {
      modes = currentModes();
      renderAvailabilityUI(); // keep the "Available for …" text honest
      if (el === videoModeInput) {
        wantsVideo = videoModeInput.checked;
        saveVideoPref(wantsVideo);
      }
      // Turning everything off is the same thing as pausing. Say so, rather
      // than leaving an "available for nothing" state the server would only
      // silently collapse anyway.
      if (isAvailable && !modes.chat && !modes.audio && !modes.video) {
        if (availabilityInput) availabilityInput.checked = false;
        availabilityInput?.dispatchEvent(new Event("change", { bubbles: true }));
      }
    });
  });

  // ─── Device pickers (mic / camera), like Zoom/Meet ─────────────────────
  // Labels are exposed only after a media permission is granted, and
  // enumerateDevices() can briefly return blank labels right after the grant —
  // so we re-run on a short retry (populateDevicesSoon) and on `devicechange`.
  const micSelect = document.getElementById("mic-select");
  const camSelect = document.getElementById("cam-select");
  function fillDeviceSelect(sel, devices, currentId, label) {
    if (!sel) return;
    const prev = sel.value;
    sel.innerHTML = "";
    const def = document.createElement("option");
    def.value = ""; def.textContent = `Default ${label.toLowerCase()}`;
    sel.appendChild(def);
    devices.forEach((d, i) => {
      const o = document.createElement("option");
      o.value = d.deviceId;
      o.textContent = d.label || `${label} ${i + 1}`;
      sel.appendChild(o);
    });
    const want = currentId || prev;
    sel.value = devices.some((d) => d.deviceId === want) ? want : "";
  }
  async function populateDevices() {
    if (!micSelect && !camSelect) return;
    const { mics, cams } = await S.listInputDevices();
    fillDeviceSelect(micSelect, mics, selectedMicId, "Microphone");
    fillDeviceSelect(camSelect, cams, selectedCamId, "Camera");
  }
  // Re-run a few times so device names fill in reliably after a grant.
  function populateDevicesSoon() {
    populateDevices();
    setTimeout(populateDevices, 400);
    setTimeout(populateDevices, 1200);
  }
  // Apply a newly-picked device: re-acquire the live stream and, if a call is
  // already in progress, hot-swap the track into the peer connection via
  // replaceTrack (no re-prompt, no dropped call).
  async function applyDeviceChange() {
    if (!isAvailable) return; // nothing live to update yet
    const got = await acquireMedia(wantsVideo);
    if (!got.hasMic) return;
    perms = got;
    if (peerConnection && liveStream) {
      const senders = peerConnection.getSenders();
      const aTrack = liveStream.getAudioTracks()[0];
      const vTrack = liveStream.getVideoTracks()[0];
      const aSender = senders.find((s) => s.track && s.track.kind === "audio");
      const vSender = senders.find((s) => s.track && s.track.kind === "video");
      try { if (aTrack && aSender) await aSender.replaceTrack(aTrack); } catch (e) {}
      try { if (vTrack && vSender) await vSender.replaceTrack(vTrack); } catch (e) {}
      localStream = liveStream; // keep endCall/preview bookkeeping consistent
      const lv = document.querySelector(".call-active .local-video");
      if (lv && vTrack) lv.srcObject = liveStream;
    }
    await S.updateSessionCapabilities(sessionId, perms.hasCamera, perms.hasMic);
  }
  if (micSelect) {
    micSelect.addEventListener("change", async () => {
      selectedMicId = micSelect.value;
      try { localStorage.setItem(STORAGE_MIC, selectedMicId); } catch (e) {}
      await applyDeviceChange();
    });
  }
  if (camSelect) {
    camSelect.addEventListener("change", async () => {
      selectedCamId = camSelect.value;
      try { localStorage.setItem(STORAGE_CAM, selectedCamId); } catch (e) {}
      await applyDeviceChange();
    });
  }
  populateDevicesSoon();
  // Refresh the list (and labels) if devices are plugged/unplugged.
  if (navigator.mediaDevices && navigator.mediaDevices.addEventListener) {
    navigator.mediaDevices.addEventListener("devicechange", populateDevices);
  }

  if (availabilityInput) {
    // Enable the input now that we're about to attach the change handler;
    // it ships disabled in HTML so an early click can't desync the UI.
    availabilityInput.disabled = false;
    availabilityInput.addEventListener("change", async () => {
      // Don't allow flipping during an active/incoming/outgoing call.
      if (state !== "ready") {
        availabilityInput.checked = isAvailable; // revert
        showAlert("You can't change availability during a call.");
        return;
      }
      const goingAvailable = availabilityInput.checked;
      if (goingAvailable) {
        // This is a user gesture — unlock audio so the ring can sound when a call
        // later arrives asynchronously. (Notification permission is handled
        // separately via the Enable action, so it never collides with the
        // getUserMedia prompt below — Safari only allows one per gesture.)
        S.primeRingtone();
        // Request permission NOW, for only the picked modes (mic always, camera
        // only if Video is on). No mic -> can't go Available.
        const got = await acquireMedia(wantsVideo);
        if (!got.hasMic) {
          availabilityInput.checked = false;
          showAlert("Microphone access is required to receive calls. Please allow it and try Go Available again.");
          return;
        }
        perms = got;
        presenceData.has_camera = perms.hasCamera;
        presenceData.has_mic = perms.hasMic;
        await S.updateSessionCapabilities(sessionId, perms.hasCamera, perms.hasMic);
        // Permission just granted — re-enumerate (with retry) so the pickers
        // show real device names.
        populateDevicesSoon();
      } else if (liveStream) {
        // Going Paused: release the camera/mic so the device indicator clears.
        // The push subscription stays armed — the server's availability gate
        // (set below) is what stops rings, and re-going-Available is instant.
        liveStream.getTracks().forEach((t) => t.stop());
        liveStream = null;
      }
      isAvailable = goingAvailable;
      await postAvailability();
      renderAvailabilityUI();
      // Arm Web Push (if permission already granted) and show the status line,
      // or hide it when pausing.
      syncPushStatus();
      const status = availabilityStatus();
      presenceData.status = status;
      // presenceChannel is initialized later in the IIFE; guard against
      // a flip happening before joinPresenceChannel has been called.
      if (presenceChannel) {
        await S.updatePresence(presenceChannel, presenceData);
      }
      await S.updateSessionStatus(sessionId, status);
    });
  }

  // ─── Display-name input ────────────────────────────────────────────────
  // Reflect the persisted display name and let the agent change it live. On
  // change we re-broadcast presence so the new name reaches guests (and other
  // agents) immediately, even mid-session.
  const displayNameInput = document.getElementById("display-name-input");
  if (displayNameInput) {
    displayNameInput.value = displayName;
    displayNameInput.disabled = false;
    const commitDisplayName = async () => {
      const v = displayNameInput.value.trim();
      displayName = v || name; // empty falls back to the real name
      saveDisplayName(v);
      presenceData.name = displayName;
      // Keep the placeholder initial in sync with the (possibly new) name.
      if (typeof renderAvatarUI === "function") renderAvatarUI();
      if (presenceChannel) {
        await S.updatePresence(presenceChannel, presenceData);
      }
      // Guests discovering us through the REST list see the durable record —
      // keep its display name in sync too (touch: never changes the bit).
      if (isAvailable) postAvailability(true);
    };
    // Commit on blur and on Enter; keeps presence in sync without spamming
    // an update on every keystroke.
    displayNameInput.addEventListener("blur", commitDisplayName);
    displayNameInput.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        displayNameInput.blur();
      }
    });
  }

  // ─── Profile-picture upload ────────────────────────────────────────────
  // Picks a JPEG/PNG, uploads it (server resizes + re-encodes), then stores
  // the cache-busted URL and re-broadcasts presence so the new picture reaches
  // guests and other agents immediately.
  const avatarInput   = document.getElementById("avatar-input");
  const avatarImg     = document.getElementById("avatar-preview");
  const avatarInitial = document.getElementById("avatar-initial");
  const avatarRemove  = document.getElementById("avatar-remove");
  const avatarStatus  = document.getElementById("avatar-status");

  function renderAvatarUI() {
    // Show the image if we have one, else an initial-letter placeholder.
    if (avatarImg) {
      if (picture) {
        avatarImg.src = picture;
        avatarImg.style.display = "";
      } else {
        avatarImg.removeAttribute("src");
        avatarImg.style.display = "none";
      }
    }
    if (avatarInitial) {
      avatarInitial.style.display = picture ? "none" : "";
      avatarInitial.textContent = (displayName || name || "?").trim().charAt(0).toUpperCase() || "?";
    }
    if (avatarRemove) avatarRemove.style.display = picture ? "" : "none";
  }
  renderAvatarUI();

  async function broadcastPicture() {
    presenceData.picture = picture;
    if (presenceChannel) {
      await S.updatePresence(presenceChannel, presenceData);
    }
  }

  if (avatarInput) {
    avatarInput.disabled = false;
    avatarInput.addEventListener("change", async () => {
      const file = avatarInput.files && avatarInput.files[0];
      if (!file) return;
      if (!/^image\/(jpe?g|png)$/i.test(file.type)) {
        if (avatarStatus) avatarStatus.textContent = "Please choose a JPEG or PNG image.";
        avatarInput.value = "";
        return;
      }
      if (avatarStatus) avatarStatus.textContent = "Uploading…";
      const { url, error } = await window.DB.uploadAvatar(ref, sessionId, file);
      avatarInput.value = ""; // allow re-selecting the same file later
      if (error) {
        if (avatarStatus) avatarStatus.textContent = error.message || "Upload failed.";
        return;
      }
      picture = url;
      savePicture(picture);
      renderAvatarUI();
      if (avatarStatus) avatarStatus.textContent = "Saved.";
      await broadcastPicture();
    });
  }

  if (avatarRemove) {
    avatarRemove.addEventListener("click", async () => {
      await window.DB.deleteAvatar(ref, sessionId);
      picture = "";
      savePicture("");
      renderAvatarUI();
      if (avatarStatus) avatarStatus.textContent = "";
      await broadcastPicture();
    });
  }

  const heartbeatTimer = S.setupHeartbeat(sessionId);

  // ─── Session revalidation ──────────────────────────────────────────────
  // /api/me was checked once, at page load, and never again — so a console left
  // open for a week had no way to learn its session had ended. Sessions no
  // longer expire on their own, but they are still revoked (admin sign-out,
  // deactivate, password reset), and an agent must find out promptly rather
  // than at the next hard reload.
  //
  // Skipped under the dev bypass, which has no server session to validate.
  const SESSION_RECHECK_MS = 5 * 60 * 1000;

  async function revalidateSession() {
    if (devBypass || sessionLost) return;
    try {
      guardAuth(await fetch("/api/me"));
    } catch (e) {
      // Offline: not session loss. The next tick or a refocus will settle it.
    }
  }

  const sessionRecheckTimer = setInterval(revalidateSession, SESSION_RECHECK_MS);

  // A laptop reopened after a week should find out immediately, not up to five
  // minutes later — that window is exactly when an agent believes they are
  // reachable and is not.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") revalidateSession();
  });
  window.addEventListener("focus", revalidateSession);

  // ─── Pagination state ──────────────────────────────────────────────────
  // Must be declared before the log-load calls below (let/const are not
  // hoisted like function declarations, so they'd be in the TDZ otherwise).
  const LOG_PAGE_SIZE = 5;
  const MSG_PREVIEW_CHARS = 80; // truncate Messages list previews; click to expand
  let callsPage = 0;
  let messagesPage = 0;

  // ─── Show main sections ────────────────────────────────────────────────
  S.showSection(".call");
  S.showSection(".messages"); // async guest contact-form notes (own section)

  // Messages (async notes) + History (Calls) are shown to all auth users.
  if (!isAdmin) {
    S.showSection(".logs");
    loadCallsLog(ref);
    loadMessagesLog(ref);
  } else {
    S.showSection(".logs");
    loadLogs(ref);
    subscribeToLogChanges(ref);

    // ─── Admin: Delete All buttons ────────────────────────────────────────
    // Destructive confirmations go through commandBox (consistent modal
    // language); fall back to the native confirm if the lib didn't load.
    async function confirmDelete(what) {
      if (window.commandBox) {
        const ans = await commandBox(
          `Delete all ${what} for this ref? This cannot be undone.`,
          ["&Delete", "&Cancel"], 2, "EXCLAMATION");
        return ans === "D";
      }
      return confirm(`Delete all ${what} for this ref? This cannot be undone.`);
    }

    const deleteAllCalls = document.getElementById("delete-all-calls");
    const deleteAllMessages = document.getElementById("delete-all-messages");

    if (deleteAllCalls) {
      deleteAllCalls.style.display = "";
      deleteAllCalls.addEventListener("click", async () => {
        if (!(await confirmDelete("call records"))) return;
        const { error } = await window.DB.deleteCalls({ ref });
        if (error) { console.error("[Delete All] Calls:", error.message); return; }
        loadCallsLog(ref, 0);
      });
    }

    if (deleteAllMessages) {
      deleteAllMessages.style.display = "";
      deleteAllMessages.addEventListener("click", async () => {
        if (!(await confirmDelete("messages"))) return;
        const { error } = await window.DB.deleteMessages({ ref });
        if (error) { console.error("[Delete All] Messages:", error.message); return; }
        loadMessagesLog(ref, 0);
      });
    }

  }

  // ─── App State ─────────────────────────────────────────────────────────
  let state = "ready"; // 'ready' | 'calling' | 'incoming' | 'active-call'
  let callRole = null; // 'caller' | 'callee'
  let currentCallId = null;
  let currentCallChannel = null;
  let currentConv = null; // { cid, token, channel } for the active conversation
  let outgoingCall = null; // { targetSessionId, targetName, callType }
  let incomingCall = null; // { callId, callerId, callerName, callType }
  let peerConnection = null;
  let localStream = null;
  let callStartTime = null;
  let callTimeoutTimer = null;
  let incomingTimeoutTimer = null; // safety net to clear a stale/unanswered ring
  let iceCandidateBuffer = [];
  let waitTimeInterval = null;
  let presenceChannel = null;
  let inboxChannel = null;
  let guestUsers = [];
  let presenceAgents = [];        // live auth members from presence (Online)
  let durableReachable = [];      // /api/agents/available (Offline·Reachable source)

  // ─── Presence ──────────────────────────────────────────────────────────
  presenceChannel = S.joinPresenceChannel(ref, presenceData, (users) => {
    // Only track guests with same ref who are available
    guestUsers = users.filter(
      (u) => u.role === "guest" && u.status === "available"
    );
    // All connected agents (Online) with their live status field.
    presenceAgents = users.filter((u) => u.role === "auth");
    if (state === "ready") renderGuestList();
    renderAgents();
    // Admins see the full roster (guests + other admins, same ref) for IM.
    // Guests never receive this list — that asymmetry is what keeps the admin
    // roster hidden from guests.
    IM.updateRoster(users);
  });

  // Offline·Reachable agents (durably available + push-subscribed, but no live
  // console) come from the REST roster — refresh periodically and merge.
  async function refreshDurableReachable() {
    try {
      const r = await fetch(`/api/agents/available?ref=${encodeURIComponent(ref)}`);
      if (r.ok) durableReachable = (await r.json()).agents || [];
    } catch (e) {
      /* keep last list */
    }
    renderAgents();
  }
  refreshDurableReachable();
  setInterval(refreshDurableReachable, 15000);

  // Keep the durable record fresh while the console is open. Discovery hides
  // records whose console hasn't been seen for the freshness window, so a
  // just-closed laptop stays reachable for the grace window after closing
  // while a truly abandoned ghost (no console for the window) ages out of
  // ringing — WITHOUT clearing the availability bit, which only Pause/logout
  // may do. Touch never flips the bit (server-guarded to available = 1).
  setInterval(() => {
    if (isAvailable) postAvailability(true);
  }, 4 * 60 * 1000);

  // Render the Agents list: every agent with their canonical state. Online
  // agents come from live presence; agents that are durably-reachable but not
  // in presence are Offline·Reachable (the closed-laptop case). Merge by
  // user_id (session ids churn across reopens).
  function renderAgents() {
    const ul = document.querySelector(".online-agents");
    if (!ul) return;
    // OTHER agents only — your own state is the availability toggle in the
    // greeting above, so listing yourself here is just noise.
    const onlineIds = new Set(presenceAgents.map((a) => a.user_id));
    const rows = [];
    presenceAgents.forEach((a) => {
      if (a.user_id !== userId) rows.push({ rec: a, st: S.agentState(a, false) });
    });
    durableReachable.forEach((a) => {
      if (a.user_id !== userId && !onlineIds.has(a.user_id)) {
        rows.push({ rec: a, st: S.agentState(null, true) });
      }
    });
    // Sort: Online first (Available, In call, Paused), then Offline·Reachable.
    const order = { available: 0, "in-call": 1, paused: 2, reachable: 3, offline: 4 };
    rows.sort((x, y) => (order[x.st.key] - order[y.st.key]) || String(x.rec.name).localeCompare(String(y.rec.name)));
    if (rows.length === 0) {
      ul.innerHTML = "<li><p class=\"online-empty\">No other agents online.</p></li>";
      return;
    }
    ul.innerHTML = rows
      .map(({ rec, st }) => `
        <li class="agent-row">
          ${S.avatarHtml(rec.name || "?", rec.picture || "", "sm")}
          <span class="agent-name">${S.escapeHtml(rec.name || "Agent")}</span>
          <span class="agent-badge agent-badge--${st.key}">${S.escapeHtml(st.label)}</span>
        </li>`)
      .join("");
  }

  // ─── Inbox ─────────────────────────────────────────────────────────────
  // The per-SESSION inbox:<sessionId> subscription that stood here is gone. Its
  // name was published in the roster, so anyone could subscribe and read this
  // agent's calls and messages. Only the user-keyed channel below remains, and
  // the server binds it to its owner's identity from the handshake session.
  //
  // User-keyed inbox: the server fans every ring out to ALL of this user's
  // live consoles through this channel, so a console opened after the ring
  // still finds it.
  if (userId) {
    window.Realtime
      .channel(`inbox:user:${ref}:${userId}`)
      .on("broadcast", { event: "message" }, ({ payload }) => {
        handleInboxMessage(payload);
      })
      .subscribe();
  }

  // ─── Web Push (Phase 2) ────────────────────────────────────────────────
  // Register the service worker so a backgrounded/closed tab can be woken and
  // rung, and re-hydrate any call still ringing for us when the page (re)gains
  // focus — e.g. right after the agent clicks a push notification.
  if (window.Push) Push.registerServiceWorker();
  // Unlock ring audio at the first interaction of ANY kind. A console that
  // resumed Available on load has had no user gesture, and Safari refuses to
  // play un-unlocked audio — so the first click/keypress anywhere primes the
  // ringtone, making every later ring audible.
  const primeOnFirstGesture = () => S.primeRingtone();
  window.addEventListener("pointerdown", primeOnFirstGesture, { once: true });
  window.addEventListener("keydown", primeOnFirstGesture, { once: true });
  checkPendingInvites();
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) checkPendingInvites();
  });
  // Safari fires focus more reliably than visibilitychange when the window
  // (not just the tab) regains attention — cover both.
  window.addEventListener("focus", () => checkPendingInvites());
  // Safety net for a silently-dead WebSocket (e.g. after laptop sleep): a
  // focused tab suppresses the OS push notification AND a dead socket never
  // delivers the WS ring — without this poll that combination misses the call
  // while the agent is looking right at the console. Guarded inside
  // checkPendingInvites (only runs when ready + Available).
  setInterval(checkPendingInvites, 10000);

  // ─── Push status indicator ─────────────────────────────────────────────
  // Make the otherwise-invisible push state visible: are background call alerts
  // armed, off (with an Enable action), or blocked by the browser? The Enable
  // button gives notification permission its own user gesture — required by
  // Safari, which won't prompt if the request is bundled with getUserMedia.
  const pushStatusEl = document.getElementById("push-status");
  const pushStatusTextEl = document.getElementById("push-status-text");
  const pushEnableBtn = document.getElementById("push-enable-btn");
  function renderPushStatus(state) {
    if (!pushStatusEl) return;
    if (state === "hidden") {
      pushStatusEl.style.display = "none";
      return;
    }
    pushStatusEl.style.display = "";
    pushStatusEl.className = "push-status";
    pushEnableBtn.style.display = "none";
    if (state === "armed") {
      pushStatusEl.classList.add("is-on");
      pushStatusTextEl.textContent =
        "🔔 You'll be rung for calls even after closing this tab — until you Pause or log out.";
    } else if (state === "blocked") {
      pushStatusEl.classList.add("is-blocked");
      pushStatusTextEl.textContent =
        "🔕 Notifications are blocked — allow them for this site in your browser settings to be alerted to calls when this tab isn’t in front.";
    } else {
      // needs-enable
      pushStatusEl.classList.add("is-off");
      pushStatusTextEl.textContent = "Get alerted to calls when this tab isn’t in front:";
      pushEnableBtn.style.display = "";
    }
  }
  // Reflect the current push state; arms push silently if permission is already
  // granted. Hidden unless we're Available (push only matters when taking calls).
  async function syncPushStatus() {
    if (!window.Push || !Push.supported() || !isAvailable) {
      renderPushStatus("hidden");
      return;
    }
    const perm = Push.permission();
    if (perm === "denied") {
      renderPushStatus("blocked");
      return;
    }
    if (perm !== "granted") {
      renderPushStatus("needs-enable");
      return;
    }
    const st = await Push.enablePush(sessionId);
    renderPushStatus(st === "armed" ? "armed" : st === "blocked" ? "blocked" : "hidden");
  }
  if (pushEnableBtn) {
    pushEnableBtn.addEventListener("click", async () => {
      const perm = await Push.requestPermission(); // this click is the gesture
      if (perm === "granted") {
        const st = await Push.enablePush(sessionId);
        renderPushStatus(st === "armed" ? "armed" : "blocked");
        // Fire a visible test notification NOW, while the user is watching.
        // Posting a real notification is what forces the browser's OS-level
        // registration (macOS prompts for the app's notification permission on
        // first post) — far better surfaced at setup time than during a missed
        // call. It also gives the agent proof the whole chain works.
        if (st === "armed") {
          try {
            const reg = await Push.registerServiceWorker();
            if (reg) {
              await reg.showNotification("Call alerts are on 🔔", {
                body: "Incoming calls will appear like this when the console isn't in front. If you don't see this as a banner, check your system notification settings for this browser.",
                icon: "/public/favicon.svg",
                tag: "push-test",
              });
            } else {
              // enablePush just returned "armed", so a registration should
              // exist — reaching here means the SW vanished in between.
              console.warn("[Push] armed but no service worker registration — test notification skipped");
            }
          } catch (e) {
            console.warn("[Push] test notification failed:", e.message);
          }
        }
      } else {
        renderPushStatus(perm === "denied" ? "blocked" : "needs-enable");
      }
    });
  }
  // Arm push on load when resuming durable availability in a fresh tab, then
  // re-point the server's availability record at THIS tab's session id so
  // REST-discovered rings route to the live session (the push subscription's
  // session id was just re-keyed by enablePush). No-op while Paused.
  syncPushStatus().then(() => {
    if (isAvailable) postAvailability(true); // touch: re-point only, never revive
  });

  // ─── Cleanup on page unload ────────────────────────────────────────────
  // This ends the TAB's live presence only — durable availability (the
  // server-side toggle) deliberately survives so a push-subscribed agent stays
  // callable after closing the tab. Safari doesn't fire beforeunload reliably,
  // so listen to pagehide as well and make the cleanup idempotent.
  let unloadDone = false;
  function unloadCleanup() {
    if (unloadDone) return;
    unloadDone = true;
    clearInterval(heartbeatTimer);
    clearInterval(sessionRecheckTimer);
    S.updateSessionStatus(sessionId, "offline");
    if (presenceChannel) presenceChannel.unsubscribe();
    if (inboxChannel) inboxChannel.unsubscribe();
    if (currentCallChannel) currentCallChannel.unsubscribe();
  }
  // persisted=true means the page is going into the back/forward cache and may
  // come back alive — don't tear down presence for that.
  window.addEventListener("pagehide", (e) => {
    if (!e.persisted) unloadCleanup();
  });
  window.addEventListener("beforeunload", unloadCleanup);

  // ─── Render guest list ─────────────────────────────────────────────────
  function renderGuestList() {
    const ul = document.querySelector(".call-guest-users");
    if (!ul) return;
    clearInterval(waitTimeInterval);
    ul.innerHTML = "";

    if (guestUsers.length === 0) {
      ul.innerHTML = "<li><p>No guests are currently online.</p></li>";
      return;
    }

    guestUsers.forEach((guest) => {
      const li = document.createElement("li");
      li.className = "call-guest-user";
      li.dataset.sessionId = guest.session_id;

      li.innerHTML = `
        <p class="call-guest-user-data">
          <span class="call-guest-user-name">${S.escapeHtml(guest.name)}</span>
          <span class="call-guest-user-status">${S.formatWaitTime(guest.online_since)}</span>
        </p>
        <ul class="call-buttons">
          <li class="call-button">
            <button class="audio-call-button"
              data-session-id="${S.escapeHtml(guest.session_id)}"
              data-name="${S.escapeHtml(guest.name)}">
              Audio call
            </button>
          </li>
          ${
            guest.has_camera
              ? `<li class="call-button">
              <button class="video-call-button"
                data-session-id="${S.escapeHtml(guest.session_id)}"
                data-name="${S.escapeHtml(guest.name)}">
                Video call
              </button>
            </li>`
              : ""
          }
        </ul>
      `;
      ul.appendChild(li);
    });

    ul.querySelectorAll(".audio-call-button").forEach((btn) => {
      btn.addEventListener("click", () =>
        initiateCall(btn.dataset.sessionId, btn.dataset.name, "audio")
      );
    });
    ul.querySelectorAll(".video-call-button").forEach((btn) => {
      btn.addEventListener("click", () =>
        initiateCall(btn.dataset.sessionId, btn.dataset.name, "video")
      );
    });

    // Live wait-time counter
    waitTimeInterval = setInterval(() => {
      ul.querySelectorAll(".call-guest-user").forEach((li) => {
        const guest = guestUsers.find((g) => g.session_id === li.dataset.sessionId);
        if (guest) {
          const statusEl = li.querySelector(".call-guest-user-status");
          if (statusEl) {
            statusEl.textContent = `${S.formatWaitTime(guest.online_since)}`;
          }
        }
      });
    }, 1000);
  }

  // ─── Initiate call (auth → guest) ──────────────────────────────────────
  async function initiateCall(targetSessionId, targetName, callType) {
    if (state !== "ready") return;

    state = "calling";
    callRole = "caller";
    outgoingCall = { targetSessionId, targetName, callType };
    currentCallId = S.generateId();

    // Update presence + session status
    presenceData.status = "in-call";
    await S.updatePresence(presenceChannel, presenceData);
    await S.updateSessionStatus(sessionId, "in-call");

    // Create call record (caller_name uses the public display name —
    // it's guest-facing and also appears in the logs).
    await S.createCallRecord({
      callId: currentCallId,
      ref: ref,
      callerSessionId: sessionId,
      callerName: displayName,
      calleeSessionId: targetSessionId,
      calleeName: targetName,
      callType,
    });

    // Open a private conversation and let the SERVER deliver the invitation —
    // including the visitor's capability for it — to their private inbox. The
    // agent holds no grant for someone else's inbox and must not: if they could
    // write into it, so could anyone who learned a session id.
    const invited = await S.inviteGuest({
      guestSession: targetSessionId,
      guestName: targetName,
      callType,
      callerName: displayName,
      callId: currentCallId,
    });
    if (invited.error) {
      await resetToReady();
      return;
    }
    currentConv = { cid: invited.cid, token: invited.token, channel: invited.channel };
    currentCallChannel = S.openConversation(currentConv, {
      onSignal: handleCallSignal,
      onMessage: (m) => IM.receive(m),
    });

    // Show outgoing call UI
    const outH1 = document.querySelector(".call-outgoing h1");
    if (outH1) outH1.textContent = `Calling ${targetName}...`;
    S.hideSection(".call");
    S.hideSection(".logs");
    S.showSection(".call-outgoing");

    // The invitation was delivered server-side by inviteGuest above; there is
    // no separate client notify, because the guest's inbox is capability-gated.

    // 5-second timeout
    callTimeoutTimer = setTimeout(async () => {
      if (state !== "calling") return;
      await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
      await S.updateCallRecord(currentCallId, { status: "timeout" });
      await resetToReady(); // resetToReady hides .alert — alert AFTER it
      showAlert("User did not answer.");
    }, 10000);
  }

  // ─── Cancel outgoing call ──────────────────────────────────────────────
  document.querySelector(".cancel-call-button").addEventListener("click", async () => {
    if (state !== "calling") return;
    clearTimeout(callTimeoutTimer);
    await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
    await S.updateCallRecord(currentCallId, { status: "cancelled" });
    await resetToReady();
  });

  // ─── Handle inbox messages (incoming calls from guests) ────────────────
  function handleInboxMessage(data) {
    if (data.type === "im") {
      IM.receive(data);
      return;
    }
    // A visitor's chat message. Not a ring: no accept/decline, no deadline, no
    // repeating alert — the message IS the event, so it just arrives.
    if (data.type === "chat-message") {
      IM.deliver(data);
      return;
    }
    if (data.type !== "incoming-call") return;

    // The same ring can arrive twice (per-session inbox + the user-keyed
    // fan-out) — if we're already presenting this exact call, swallow the
    // duplicate silently (it must NOT fall through to the busy reply).
    if (data.callId && data.callId === currentCallId) return;

    // Only ring when we're free AND actually Available this session. A Paused
    // agent (or one mid-call) must never ring — otherwise a stray/stale call
    // would start ringing and wedge the availability toggle ("can't change
    // during a call"). Tell the caller we're busy and drop it.
    if (state !== "ready" || !isAvailable) {
      // Busy is reported over the conversation channel now — there is no
      // caller session id to reply to, by design.
      if (data.cid) {
        S.agentConversationToken(data.cid).then((t) => {
          if (!t.error) {
            window.Realtime.publish(`conv:${data.cid}`, "signal",
              { type: "call-busy" }, { token: t.token });
          }
        });
      }
      return;
    }

    presentIncomingCall(data);
  }

  // Render the incoming-call UI and start ringing. Shared by the live inbox
  // path and the pending-invite re-hydration (after a push wakes the agent).
  // `data` is { callId, callType, callerName, callerId? }.
  async function presentIncomingCall(data) {
    state = "incoming";
    callRole = "callee";
    incomingCall = data;
    currentCallId = data.callId;

    // Exchange the conversation id for THIS agent's capability. The server
    // issues it only for a conversation they actually own, so a ring naming
    // someone else's conversation gets nowhere. Signalling then rides that
    // private channel instead of the guessable call:<callId> it used to.
    if (data.cid) {
      const t = await S.agentConversationToken(data.cid);
      if (t.error) {
        // Not ours, or gone. Drop the ring rather than presenting a call that
        // could never connect.
        state = "ready";
        currentCallId = null;
        return;
      }
      currentConv = { cid: data.cid, token: t.token, channel: t.channel };
      currentCallChannel = S.openConversation(currentConv, {
        onSignal: handleCallSignal,
        onMessage: (m) => IM.receive(m),
      });
      // Chat rings arrive here too: show the thread and everything already said
      // rather than an empty dock the guest cannot tell apart from being ignored.
      if (data.callType === "chat") {
        IM.open({
          cid: data.cid, token: t.token,
          name: t.guestName || data.callerName,
          guestSession: t.guestSession,
        });
        state = "ready";
        return;
      }
    }

    const callType = data.callType === "video" ? "video" : "audio";
    // Ring — and if the browser blocks the audio (a console that resumed
    // Available on load has had no gesture yet, so Safari refuses to play),
    // escalate to an OS notification via the service worker, which carries the
    // system sound even when the tab is focused. Without this a re-hydrated
    // call is completely silent: just an Accept button waiting to be noticed.
    Promise.resolve(S.playRingtone()).then((rang) => {
      if (!rang) notifyViaServiceWorker(callType, data.callerName);
    });
    // Backgrounded tabs throttle audio — also raise an OS notification so the
    // agent doesn't miss the call while the tab is hidden.
    S.notifyIncomingCall("Incoming call", `${callType} call from ${data.callerName}`);
    // Phone-style overlay: caller name front and center, avatar initial,
    // call-type label beneath.
    const nameEl = document.getElementById("incoming-name");
    if (nameEl) nameEl.textContent = data.callerName || "Guest";
    const typeEl = document.getElementById("incoming-type");
    if (typeEl) typeEl.textContent = `Incoming ${callType} call`;
    const avEl = document.getElementById("incoming-avatar");
    if (avEl) avEl.innerHTML = S.avatarHtml(data.callerName || "?", "", "lg");

    S.hideSection(".call");
    S.hideSection(".logs");
    S.showSection(".call-incoming");

    // Safety net: if the call isn't answered within the ring window — because the
    // guest already gave up, or this was a stale invite re-hydrated on reopen —
    // stop ringing and return to ready so the console can never get stuck on a
    // dead incoming call (which would also block the availability toggle).
    clearTimeout(incomingTimeoutTimer);
    incomingTimeoutTimer = setTimeout(async () => {
      if (state === "incoming") {
        await resetToReady(); // resetToReady hides .alert — alert AFTER it
        showAlert(`Missed call from ${data.callerName || "a guest"}.`);
      }
    }, 35000);
  }

  // OS notification raised from the page through the service worker — unlike
  // `new Notification(...)` this also works as the audible fallback when the
  // in-page ringtone is blocked (and is closed by clearIncomingNotification).
  async function notifyViaServiceWorker(callType, callerName) {
    try {
      if (!window.Push || !("Notification" in window) || Notification.permission !== "granted") return;
      const reg = await Push.registerServiceWorker();
      if (!reg) return;
      await reg.showNotification("Incoming call", {
        body: `${callType} call from ${callerName || "a guest"}`,
        icon: "/public/favicon.svg",
        tag: "incoming-call",
        renotify: true,
        requireInteraction: true,
      });
    } catch (e) {
      /* best effort */
    }
  }

  // After a push notification wakes/focuses the console, re-hydrate any call
  // that's still ringing for this agent (the guest waits ~30s).
  async function checkPendingInvites() {
    // Only re-hydrate a ringing call when we're actually Available this session
    // (a live mic stream exists). On a fresh reopen the agent is Paused, so we
    // must not auto-ring a leftover invite — they re-go-Available first.
    if (state !== "ready" || !isAvailable) return;
    try {
      const r = await fetch("/api/call/pending");
      if (!r.ok) return;
      const { invites } = await r.json();
      if (invites && invites.length) presentIncomingCall(invites[0]);
    } catch (e) {
      /* best effort */
    }
  }

  // Consume the server-side pending invite for a call we've answered (or
  // declined) — otherwise the pending-invite poll would ghost-re-ring it
  // within its 30s TTL after the call ends.
  function clearPendingInvite(callId) {
    if (!callId) return;
    fetch("/api/call/ring/clear", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ callId }),
    }).catch(() => {});
  }

  // ─── Accept incoming call ──────────────────────────────────────────────
  document.querySelector(".accept-call-button").addEventListener("click", async () => {
    if (state !== "incoming") return;
    clearTimeout(incomingTimeoutTimer);
    clearPendingInvite(currentCallId);
    S.stopRingtone();
    S.clearIncomingNotification();
    S.hideSection(".call-incoming");

    presenceData.status = "in-call";
    await S.updatePresence(presenceChannel, presenceData);
    await S.updateSessionStatus(sessionId, "in-call");

    // Signal accepted — caller (guest) will send the offer
    await S.sendCallSignal(currentCallChannel, { type: "call-accepted" });
    state = "active-call"; // Will get offer next; offer handler sets up the PC

    // The caller may already be gone (stale invite, guest closed the page,
    // network drop). If no offer arrives, recover instead of wedging in a
    // dead "active-call" that would also block the availability toggle.
    clearTimeout(callTimeoutTimer);
    callTimeoutTimer = setTimeout(async () => {
      if (state === "active-call" && !peerConnection) {
        await S.updateCallRecord(currentCallId, { status: "missed" });
        await resetToReady(); // resetToReady hides .alert — alert AFTER it
        showAlert("The caller is no longer there.");
      }
    }, 12000);
  });

  // ─── Decline incoming call ─────────────────────────────────────────────
  document.querySelector(".decline-call-button").addEventListener("click", async () => {
    if (state !== "incoming") return;
    clearPendingInvite(currentCallId);
    S.stopRingtone();
    S.clearIncomingNotification();
    S.hideSection(".call-incoming");
    await S.sendCallSignal(currentCallChannel, { type: "call-declined" });
    await S.updateCallRecord(currentCallId, { status: "declined" });
    await resetToReady();
  });

  // ─── Call signal handler ───────────────────────────────────────────────
  async function handleCallSignal(data) {
    switch (data.type) {
      case "call-accepted":
        // We (auth) are the caller; guest accepted → create offer
        if (state === "calling") {
          clearTimeout(callTimeoutTimer);
          state = "active-call";
          await startAsInitiator();
        } else if (state === "incoming") {
          // Another of our consoles (other tab/browser) answered this call —
          // stop ringing here and return to ready.
          await resetToReady();
        }
        break;

      case "call-declined":
        if (state === "calling") {
          clearTimeout(callTimeoutTimer);
          await S.updateCallRecord(currentCallId, { status: "declined" });
          await resetToReady(); // resetToReady hides .alert — alert AFTER it
          showAlert("User declined the call.");
        }
        break;

      case "call-cancelled":
        if (state === "incoming") {
          S.stopRingtone();
          S.hideSection(".call-incoming");
          await resetToReady();
        }
        break;

      case "offer":
        // We (auth) are the callee; guest (caller) sent offer after we accepted
        if (callRole !== "callee") return;
        await handleOffer(data.sdp);
        break;

      case "answer":
        // We (auth) are the caller; guest (callee) replied with answer
        if (peerConnection && callRole === "caller") {
          await peerConnection.setRemoteDescription({ type: "answer", sdp: data.sdp });
          await flushIceCandidateBuffer();
          callStartTime = Date.now();
          await S.updateCallRecord(currentCallId, {
            started_at: new Date().toISOString(),
            status: "answered",
          });
        }
        break;

      case "ice-candidate":
        if (!data.candidate) return;
        if (peerConnection && peerConnection.remoteDescription) {
          await peerConnection
            .addIceCandidate(data.candidate)
            .catch((e) => console.warn("[ICE] addIceCandidate error:", e.message));
        } else {
          iceCandidateBuffer.push(data.candidate);
        }
        break;

      case "hangup":
        if (state === "active-call") await endCall(false);
        break;
    }
  }

  // ─── Start as initiator (auth is caller, guest accepted) ───────────────
  async function startAsInitiator() {
    const { callType } = outgoingCall;
    const iceConfig = await S.getIceConfig();

    localStream = await getMediaStream(callType);
    if (!localStream) {
      await S.sendCallSignal(currentCallChannel, { type: "hangup" });
      await resetToReady();
      return;
    }

    peerConnection = S.createPeerConnection({
      iceConfig,
      onIceCandidate: async (candidate) => {
        await S.sendCallSignal(currentCallChannel, { type: "ice-candidate", candidate });
      },
      onTrack: (remoteStream) => {
        showActiveCallUI(outgoingCall.targetName, remoteStream, callType);
      },
      onConnectionStateChange: async (connState) => {
        if (connState === "disconnected" || connState === "failed") await endCall(false);
      },
    });

    (callType === "video" ? localStream.getTracks() : localStream.getAudioTracks()).forEach((track) => peerConnection.addTrack(track, localStream));

    const offer = await peerConnection.createOffer();
    await peerConnection.setLocalDescription(offer);
    await S.sendCallSignal(currentCallChannel, { type: "offer", sdp: offer.sdp });
  }

  // ─── Handle offer (auth is callee, guest sent offer after auth accepted) ─
  async function handleOffer(sdpString) {
    clearTimeout(callTimeoutTimer); // caller is alive — cancel the no-offer net
    const callType = incomingCall?.callType || "audio";
    const iceConfig = await S.getIceConfig();

    localStream = await getMediaStream(callType);
    if (!localStream) {
      await S.sendCallSignal(currentCallChannel, { type: "hangup" });
      await resetToReady();
      return;
    }

    peerConnection = S.createPeerConnection({
      iceConfig,
      onIceCandidate: async (candidate) => {
        await S.sendCallSignal(currentCallChannel, { type: "ice-candidate", candidate });
      },
      onTrack: (remoteStream) => {
        showActiveCallUI(incomingCall.callerName, remoteStream, callType);
      },
      onConnectionStateChange: async (connState) => {
        if (connState === "disconnected" || connState === "failed") await endCall(false);
      },
    });

    (callType === "video" ? localStream.getTracks() : localStream.getAudioTracks()).forEach((track) => peerConnection.addTrack(track, localStream));

    await peerConnection.setRemoteDescription({ type: "offer", sdp: sdpString });
    await flushIceCandidateBuffer();

    const answer = await peerConnection.createAnswer();
    await peerConnection.setLocalDescription(answer);
    await S.sendCallSignal(currentCallChannel, { type: "answer", sdp: answer.sdp });

    callStartTime = Date.now();
    await S.updateCallRecord(currentCallId, {
      started_at: new Date().toISOString(),
      status: "answered",
    });
  }

  async function flushIceCandidateBuffer() {
    for (const candidate of iceCandidateBuffer) {
      await peerConnection
        .addIceCandidate(candidate)
        .catch((e) => console.warn("[ICE] Buffer flush error:", e.message));
    }
    iceCandidateBuffer = [];
  }

  // ─── Get media stream ──────────────────────────────────────────────────
  async function getMediaStream(callType) {
    // Reuse the stream acquired at Go-Available so accepting a call doesn't
    // re-prompt. The addTrack step picks audio-only vs audio+video, so an audio
    // call from a video-capable agent still sends audio only. Only a video call
    // arriving while we hold an audio-only stream needs a fresh acquire (which
    // routing normally prevents, since video calls target camera agents).
    if (liveStream && (callType !== "video" || liveStream.getVideoTracks().length > 0)) {
      return liveStream;
    }
    // Re-acquire through the same fallback ladder as Go-Available, so a stale
    // device pick can't kill an incoming call either.
    const got = await acquireMedia(callType === "video" && perms.hasCamera);
    if (!got.hasMic) {
      showAlert("Could not access media devices.");
      return null;
    }
    return liveStream;
  }

  // ─── Show active call UI ───────────────────────────────────────────────
  function showActiveCallUI(peerName, remoteStream, callType) {
    S.hideSection(".call-incoming");
    S.hideSection(".call-outgoing");

    const callActiveEl = document.querySelector(".call-active");
    callActiveEl.classList.toggle("call-active--video", callType === "video");

    const nameEl = document.querySelector(".call-active .guest-user-name");
    if (nameEl) nameEl.textContent = peerName;

    const remoteVideo = document.querySelector(".call-active .remote-video");
    if (remoteVideo) {
      remoteVideo.srcObject = remoteStream;
      remoteVideo.play().catch(() => {});
    }

    const localVideo = document.querySelector(".call-active .local-video");
    if (localVideo) {
      if (localStream && callType === "video") {
        localVideo.srcObject = localStream;
        localVideo.muted = true;
        localVideo.play().catch(() => {});
        localVideo.style.display = "block";
      } else {
        localVideo.style.display = "none";
      }
    }

    S.showSection(".call-active");
  }

  // ─── End call button ───────────────────────────────────────────────────
  document.querySelector(".end-call-button").addEventListener("click", async () => {
    await S.sendCallSignal(currentCallChannel, { type: "hangup" });
    await endCall(true);
  });

  // ─── End call (cleanup) ────────────────────────────────────────────────
  async function endCall() {
    if (peerConnection) {
      peerConnection.close();
      peerConnection = null;
    }
    // Don't stop the live (Available) stream — it's reused across calls. Only
    // stop a one-off stream (e.g. a fresh acquire for a video call).
    if (localStream && localStream !== liveStream) {
      localStream.getTracks().forEach((t) => t.stop());
    }
    localStream = null;

    const remoteVideo = document.querySelector(".call-active .remote-video");
    if (remoteVideo) remoteVideo.srcObject = null;
    const localVideo = document.querySelector(".call-active .local-video");
    if (localVideo) { localVideo.srcObject = null; localVideo.style.display = "none"; }

    if (currentCallId && callStartTime) {
      const duration = Math.floor((Date.now() - callStartTime) / 1000);
      await S.updateCallRecord(currentCallId, { status: "answered", duration });
    }

    S.hideSection(".call-active");
    await resetToReady();

    // Reload logs after call ends
    if (isAdmin) {
      loadLogs(ref);
    } else {
      loadCallsLog(ref);
    }
  }

  // ─── Reset to ready state ──────────────────────────────────────────────
  async function resetToReady() {
    clearTimeout(incomingTimeoutTimer);
    S.stopRingtone();
    S.clearIncomingNotification();
    state = "ready";
    callRole = null;
    outgoingCall = null;
    incomingCall = null;
    currentCallId = null;
    callStartTime = null;
    iceCandidateBuffer = [];

    if (currentCallChannel) {
      currentCallChannel.unsubscribe();
      currentCallChannel = null;
    }

    const readyStatus = availabilityStatus();
    presenceData.status = readyStatus;
    await S.updatePresence(presenceChannel, presenceData);
    await S.updateSessionStatus(sessionId, readyStatus);

    S.hideSection(".call-incoming");
    S.hideSection(".call-outgoing");
    S.hideSection(".call-active");
    S.hideSection(".alert");
    S.showSection(".call");

    S.showSection(".logs");
  }

  // ─── Alert ────────────────────────────────────────────────────────────
  function showAlert(message) {
    const el = document.querySelector(".alert h1");
    if (el) el.textContent = message;
    S.showSection(".alert");
  }

  document.querySelector(".alert .close-btn").addEventListener("click", () => {
    S.hideSection(".alert");
  });

  // ─── Logs ─────────────────────────────────────────────────────────────
  // Renders ← Prev  n / total  Next → controls into a container div.
  // Hidden automatically when there is only one page.
  function renderPagination(containerId, page, total, onPageChange) {
    const el = document.getElementById(containerId);
    if (!el) return;
    el.innerHTML = "";
    const totalPages = Math.max(1, Math.ceil(total / LOG_PAGE_SIZE));
    if (totalPages <= 1) return;

    const prev = document.createElement("button");
    prev.textContent = "← Prev";
    prev.disabled = page === 0;
    prev.addEventListener("click", () => onPageChange(page - 1));

    const info = document.createElement("span");
    info.textContent = `${page + 1} / ${totalPages}`;

    const next = document.createElement("button");
    next.textContent = "Next →";
    next.disabled = page >= totalPages - 1;
    next.addEventListener("click", () => onPageChange(page + 1));

    el.append(prev, info, next);
  }

  async function loadLogs(ref) {
    callsPage = 0;
    messagesPage = 0;
    await loadCallsLog(ref, 0);
    await loadMessagesLog(ref, 0);
  }

  function subscribeToLogChanges(ref) {
    // Writers publish a "refresh" broadcast on dashboard:<ref>; refetch the
    // matching log (debounced) when it arrives so the dashboard stays live.
    const debounce = (fn, ms = 150) => {
      let t;
      return () => { clearTimeout(t); t = setTimeout(fn, ms); };
    };
    const reloaders = {
      calls:    debounce(() => loadCallsLog(ref, callsPage)),
      messages: debounce(() => loadMessagesLog(ref, messagesPage)),
    };
    window.Realtime
      .channel(`dashboard:${ref}`)
      .on("broadcast", { event: "refresh" }, ({ payload }) => {
        const fn = reloaders[payload && payload.table];
        if (fn) fn();
      })
      .subscribe();
    // Refresh broadcasts that fired while the socket was down (frozen tab,
    // sleep, network blip) are lost — refetch everything on reconnect and
    // when the tab becomes visible again, so the logs can't go stale.
    const reloadAll = debounce(() => {
      loadCallsLog(ref, callsPage);
      loadMessagesLog(ref, messagesPage);
    }, 300);
    if (window.Realtime.onReconnect) window.Realtime.onReconnect(reloadAll);
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden) reloadAll();
    });
  }

  async function loadCallsLog(ref, page = 0) {
    callsPage = page;
    const { data, error, count } = await window.DB.listCalls({
      ref, page, pageSize: LOG_PAGE_SIZE,
    });

    if (error) { console.error("[Logs] Calls:", error.message); return; }

    const tbody = document.querySelector(".log-calls-table tbody");
    if (!tbody) return;
    tbody.innerHTML = "";
    (data || []).forEach((call) => {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td>${S.escapeHtml(call.call_id || "")}</td>
        <td>${S.escapeHtml(call.ref || "")}</td>
        <td>${S.escapeHtml(call.caller_name || "")}</td>
        <td>${S.escapeHtml(call.callee_name || "")}</td>
        <td>${S.escapeHtml(call.type || "")}</td>
        <td>${call.started_at ? new Date(call.started_at).toLocaleString() : ""}</td>
        <td>${S.formatDuration(call.duration)}</td>
        <td>${S.escapeHtml(call.status || "")}</td>
        <td>${isAdmin ? `<button data-id="${call.id}" class="delete-call-btn">Delete</button>` : ""}</td>
      `;
      tbody.appendChild(tr);
    });
    if (isAdmin) {
      tbody.querySelectorAll(".delete-call-btn").forEach((btn) => {
        btn.addEventListener("click", async () => {
          await window.DB.deleteCallById(btn.dataset.id);
          btn.closest("tr").remove();
          // If we just emptied this page and it's not the first, go back one page
          if (tbody.querySelectorAll("tr").length === 0 && callsPage > 0) {
            loadCallsLog(ref, callsPage - 1);
            return;
          }
          renderPagination("calls-pagination", callsPage, (count || 1) - 1,
            (p) => loadCallsLog(ref, p));
        });
      });
    }

    renderPagination("calls-pagination", page, count || 0,
      (p) => loadCallsLog(ref, p));
    if (isAdmin) {
      const btn = document.getElementById("delete-all-calls");
      if (btn) btn.style.display = (count || 0) > 1 ? "" : "none";
    }
  }

  async function loadMessagesLog(ref, page = 0) {
    messagesPage = page;
    const { data, error, count } = await window.DB.listMessages({
      ref, page, pageSize: LOG_PAGE_SIZE,
    });

    if (error) { console.error("[Logs] Messages:", error.message); return; }

    const tbody = document.querySelector(".log-messages-table tbody");
    if (!tbody) return;
    tbody.innerHTML = "";
    (data || []).forEach((msg) => {
      const isRead = msg.is_read === 1 || msg.is_read === true;
      const full = msg.message || "";
      const truncated = full.length > MSG_PREVIEW_CHARS;
      const preview = truncated ? full.slice(0, MSG_PREVIEW_CHARS).trimEnd() + "…" : full;
      const tr = document.createElement("tr");
      tr.className = "msg-row" + (isRead ? "" : " msg-row--unread");
      tr.dataset.id = msg.id;
      tr.innerHTML = `
        <td>${S.escapeHtml(msg.name || "")}</td>
        <td>${S.escapeHtml(msg.contact || "")}</td>
        <td class="msg-cell">
          <span class="msg-preview">${S.escapeHtml(preview)}</span>
          <span class="msg-full" style="display: none">${S.escapeHtml(full)}</span>
        </td>
        <td>${msg.created_at ? new Date(msg.created_at).toLocaleString() : ""}</td>
        <td>${isAdmin ? `<button data-id="${msg.id}" class="delete-msg-btn">Delete</button>` : ""}</td>
      `;
      // Click the row (anywhere but Delete) to expand the full text; the first
      // open marks it read (server-side, so it syncs to every console/device).
      tr.addEventListener("click", async (e) => {
        if (e.target.closest(".delete-msg-btn")) return;
        const expanded = tr.classList.toggle("msg-expanded");
        const prev = tr.querySelector(".msg-preview");
        const fullEl = tr.querySelector(".msg-full");
        if (prev && fullEl) {
          prev.style.display = expanded ? "none" : "";
          fullEl.style.display = expanded ? "" : "none";
        }
        if (tr.classList.contains("msg-row--unread")) {
          tr.classList.remove("msg-row--unread");
          // Await the mark-read so the unread re-count reads the committed state
          // (otherwise the badge can re-fetch the stale pre-read count).
          await window.DB.markMessageRead(msg.id, ref).catch(() => {});
          refreshMessagesUnread(ref);
        }
      });
      tbody.appendChild(tr);
    });
    if (isAdmin) {
      tbody.querySelectorAll(".delete-msg-btn").forEach((btn) => {
        btn.addEventListener("click", async (e) => {
          e.stopPropagation(); // don't toggle/expand the row
          await window.DB.deleteMessageById(btn.dataset.id);
          btn.closest("tr").remove();
          refreshMessagesUnread(ref);
          if (tbody.querySelectorAll("tr").length === 0 && messagesPage > 0) {
            loadMessagesLog(ref, messagesPage - 1);
            return;
          }
          renderPagination("messages-pagination", messagesPage, (count || 1) - 1,
            (p) => loadMessagesLog(ref, p));
        });
      });
    }

    renderPagination("messages-pagination", page, count || 0,
      (p) => loadMessagesLog(ref, p));
    if (isAdmin) {
      const btn = document.getElementById("delete-all-messages");
      if (btn) btn.style.display = (count || 0) > 1 ? "" : "none";
    }
    refreshMessagesUnread(ref);
  }

  // Unread count drives the Messages header badge AND the auto-collapse: the
  // section opens when something is unread and folds away when the inbox is
  // clear. Server-side count, so it's consistent across the agent's devices.
  async function refreshMessagesUnread(ref) {
    const section = document.querySelector(".messages");
    if (!section) return;
    let unread = 0;
    try {
      const { data } = await window.DB.unreadMessageCount({ ref });
      unread = (data && data.unread) || 0;
    } catch (e) { /* keep prior badge/state on a transient error */ }
    const badge = section.querySelector(".messages-unread");
    if (badge) {
      badge.textContent = unread > 99 ? "99+" : String(unread);
      badge.style.display = unread > 0 ? "" : "none";
    }
    // Surface unread by EXPANDING; never auto-collapse mid-session (the agent
    // may be reading). The section starts collapsed (HTML default) and folds
    // again on the next fresh load once the inbox is clear.
    if (unread > 0) {
      section.classList.remove("messages--collapsed");
      section.querySelector(".messages-toggle")?.setAttribute("aria-expanded", "true");
    }
  }

  // (Sessions / Admin Sessions log tables retired — live agent/guest presence
  // is the truthful source now; see renderAgents + the Online section.)

  // ─── Instant Messaging (admin view) ────────────────────────────────────
  // Admins can start a chat with anyone online for this ref (guests AND other
  // admins). Multiple concurrent threads are kept, keyed by the peer's
  // session_id. Threads are ephemeral — held in memory only, gone on refresh.
  const IM = (function () {
    const section   = document.querySelector(".im");
    const rosterEl  = section?.querySelector(".im-roster");
    const threadEl  = section?.querySelector(".im-thread");
    const messagesEl = section?.querySelector(".im-messages");
    const nameEl    = section?.querySelector(".im-thread-name");
    const formEl    = section?.querySelector(".im-form");
    const inputEl   = section?.querySelector(".im-input");
    const sendBtn   = section?.querySelector(".im-send");
    const dockHeader = section?.querySelector(".im-dock-header");
    const dockUnread = section?.querySelector(".im-dock-unread");
    const dockTitle  = section?.querySelector(".im-dock-title");

    // peerId -> { id, name, role, online, messages:[{dir,text,ts}], unread }
    const threads = new Map();
    let activePeerId = null;
    let roster = []; // latest presence snapshot (excluding self)

    if (!section) {
      // IM markup not present — expose no-op hooks so callers stay simple.
      return { updateRoster() {}, receive() {}, open() {}, deliver() {}, restore() {}, refresh() {}, applyReceipt() {}, maybeMarkRead() {} };
    }
    // The IM section is always available to admins; show the dock (collapsed,
    // tucked into the bottom-right corner until the agent opens it).
    section.style.display = "";
    section.classList.add("im-collapsed");

    // No conversation selected yet — keep the thread pane visible with its
    // "Select someone to chat" placeholder (input disabled) so the expanded
    // Chats card fills its width like a normal chat app instead of leaving a
    // big empty area beside the narrow roster. openThread() activates it.
    if (threadEl) threadEl.style.display = "";

    // Click the header bar to minimize / expand, like Facebook chat.
    if (dockHeader) {
      dockHeader.addEventListener("click", () => {
        section.classList.toggle("im-collapsed");
        if (!section.classList.contains("im-collapsed")) inputEl?.focus();
      });
    }

    // Sum unread across all threads onto the collapsed dock bar.
    function renderDockUnread() {
      let total = 0;
      for (const [, t] of threads) total += t.unread || 0;
      if (!dockUnread) return;
      if (total > 0) {
        dockUnread.textContent = total > 99 ? "99+" : String(total);
        dockUnread.style.display = "";
      } else {
        dockUnread.style.display = "none";
      }
    }

    function thread(peer) {
      let t = threads.get(peer.id);
      if (!t) {
        t = { id: peer.id, name: peer.name, role: peer.role, picture: peer.picture || "", online: true, messages: [], unread: 0 };
        threads.set(peer.id, t);
      } else {
        if (peer.name) t.name = peer.name;
        if (peer.role) t.role = peer.role;
        if (peer.picture !== undefined) t.picture = peer.picture;
      }
      return t;
    }

    function updateRoster(users) {
      roster = (users || [])
        .filter((u) => u.session_id && u.session_id !== sessionId)
        .map((u) => ({ id: u.session_id, name: u.name || "Unknown", role: u.role || "guest", picture: u.picture || "" }));
      const onlineIds = new Set(roster.map((u) => u.id));

      // Mark threads online/offline so history still shows for someone who has
      // just left. A conversation thread resolves its liveness through the
      // visitor's session id — a cid has no presence record of its own, which
      // is why these threads were all rendering "(offline)".
      for (const [id, t] of threads) {
        t.online = t.guestSession ? onlineIds.has(t.guestSession) : onlineIds.has(id);
      }
      // Header label: "Chats" + a live count of people available to chat.
      if (dockTitle) {
        let convSessionCount = 0;
        for (const [, t] of threads) if (t.guestSession) convSessionCount++;
        const chatCount = roster.length - convSessionCount + convSessionCount;
        dockTitle.textContent = chatCount ? `Chats (${chatCount})` : "Chats";
      }
      renderRoster();
    }

    function renderRoster() {
      rosterEl.innerHTML = "";
      // Union of currently-online people and anyone we have an open thread with.
      const byId = new Map();
      // A visitor with an open conversation is listed by CID. Their presence
      // row is the SAME person under a different key, so it must not also be
      // listed — that is the duplicate entry.
      //
      // The dedupe lives here, in the one function that composes the list,
      // rather than in updateRoster: renderRoster is called directly from
      // delivery too, and a filter applied elsewhere leaves a stale row behind
      // until the next presence sync.
      const convSessions = new Set();
      for (const [, t] of threads) if (t.guestSession) convSessions.add(t.guestSession);

      roster.forEach((u) => {
        if (convSessions.has(u.id)) return;
        byId.set(u.id, { ...u, online: true });
      });
      for (const [id, t] of threads) {
        if (!byId.has(id)) byId.set(id, { id, name: t.name, role: t.role, picture: t.picture, online: t.online });
      }
      if (byId.size === 0) {
        const li = document.createElement("li");
        li.className = "im-empty";
        li.textContent = "No one else is online.";
        rosterEl.appendChild(li);
        return;
      }
      for (const u of byId.values()) {
        const t = threads.get(u.id);
        const hasUnread = !!(t && t.unread > 0);
        const li = document.createElement("li");
        li.className = "im-roster-item"
          + (u.id === activePeerId ? " im-active" : "")
          + (hasUnread ? " im-roster-item--unread" : ""); // bold until read
        li.dataset.peerId = u.id;
        const unread = hasUnread
          ? `<span class="im-unread">${t.unread}</span>` : "";
        // Only admins get a role pill; guests show just their name.
        const rolePill = u.role === "auth"
          ? `<span class="im-roster-role im-role-auth">admin</span>` : "";
        // Only authenticated users have profile pictures; guests fall back to
        // the initial placeholder.
        const avatar = S.avatarHtml(u.name, u.role === "auth" ? u.picture : "");
        li.innerHTML = `
          ${avatar}
          ${rolePill}
          <span class="im-roster-name">${S.escapeHtml(u.name)}${u.online ? "" : " (offline)"}</span>
          ${unread}
        `;
        li.addEventListener("click", () => openThread(u));
        rosterEl.appendChild(li);
      }
    }

    function openThread(peer) {
      const t = thread(peer);
      activePeerId = peer.id;
      t.unread = 0;
      if (threadEl) threadEl.style.display = "";
      nameEl.textContent = t.name + (t.online ? "" : " (offline)");
      inputEl.disabled = false;
      sendBtn.disabled = false;
      // Opening a thread expands the dock if it was minimized.
      section.classList.remove("im-collapsed");
      inputEl.focus();
      renderMessages();
      renderRoster();
      renderDockUnread();
      maybeMarkRead(t);
    }

    function renderMessages() {
      messagesEl.innerHTML = "";
      const t = activePeerId ? threads.get(activePeerId) : null;
      if (!t || t.messages.length === 0) {
        const li = document.createElement("li");
        li.className = "im-empty";
        li.textContent = "No messages yet.";
        messagesEl.appendChild(li);
        return;
      }
      for (const m of t.messages) {
        const li = document.createElement("li");
        li.className = "im-msg " + (m.dir === "out" ? "im-msg-out" : "im-msg-in");
        li.textContent = m.text;
        messagesEl.appendChild(li);
      }
      messagesEl.scrollTop = messagesEl.scrollHeight;
    }

    // addMessage appends unless this exact message is already present.
    //
    // A message can legitimately reach a console by two routes: the
    // conversation channel it is subscribed to, and the user-inbox
    // notification that exists for consoles which are NOT yet subscribed.
    // Rather than trying to make the routes mutually exclusive — which breaks
    // the moment one is slow — they are made idempotent by server-assigned id.
    function addMessage(t, m, dir) {
      if (!m) return false;
      t.seen = t.seen || new Set();
      if (m.id != null) {
        if (t.seen.has(m.id)) return false;
        t.seen.add(m.id);
      }
      t.messages.push({
        dir, text: m.body, ts: (m.created_at || 0) * 1000, id: m.id,
        deliveredAt: m.delivered_at || 0, readAt: m.read_at || 0,
      });
      if (dir === "in" && m.id && t.conv) {
        S.sendReceipt({ ref, cid: t.conv.cid, token: t.conv.token, upToId: m.id, kind: "delivered" });
      }
      return true;
    }

    // restore(): rebuild open conversations on console start.
    //
    // The transcript existed all along; what was missing was any way to LEARN
    // which conversations are open, so a reload showed an empty list while the
    // visitor was still typing into one.
    async function restore() {
      const res = await S.listConversations();
      for (const c of res.conversations || []) {
        if (c.callType !== "chat") continue; // a call does not survive a reload
        if (threads.has(c.cid)) continue;
        const t = {
          id: c.cid, name: c.guestName || "Visitor", picture: "", role: "guest",
          guestSession: c.guestSession || "", online: false,
          messages: [], unread: 0, conv: { cid: c.cid, token: c.token },
        };
        threads.set(c.cid, t);
        S.openConversation({ channel: c.channel, token: c.token },
          { onMessage: (m) => receive(m), onReceipt: (r) => applyReceipt(r) });
        const tr = await S.loadTranscript({ ref, cid: c.cid, token: c.token });
        t.seen = new Set();
        for (const m of tr.messages || []) {
          addMessage(t, m, m.sender === "agent" ? "out" : "in");
        }
      }
      if (threads.size) {
        S.showSection(".im");
        renderRoster();
        // Open the most recent so the agent lands somewhere usable rather than
        // on "Select someone to chat".
        if (!activePeerId) {
          const first = threads.values().next().value;
          if (first) openThread({ id: first.id, name: first.name, role: "guest", picture: first.picture });
        }
        renderDockUnread();
      }
    }

    // Rebuild every open conversation from the SERVER record.
    //
    // Same hazard as the guest side, and config.js documents it: a broadcast
    // sent while the socket is down is simply gone. Only the dashboard logs
    // registered a reconnect hook; chat did not, so an agent whose laptop slept
    // (or whose tab was throttled) silently lost whatever the visitor said in
    // the meantime — and had no way to know.
    //
    // The transcript is the record and message ids make this idempotent, so it
    // is safe to run on every reconnect and every return to visibility.
    async function refresh() {
      for (const [, t] of threads) {
        if (!t.conv) continue;
        const res = await S.loadTranscript({ ref, cid: t.conv.cid, token: t.conv.token });
        if (!res.messages) continue;
        t.seen = new Set();
        t.messages = [];
        for (const m of res.messages) addMessage(t, m, m.sender === "agent" ? "out" : "in");
      }
      renderMessages();
      renderRoster();
      renderDockUnread();
    }

    if (window.Realtime.onReconnect) window.Realtime.onReconnect(() => refresh());
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState !== "visible") return;
      refresh().then(() => maybeMarkRead(activePeerId ? threads.get(activePeerId) : null));
    });

    // Same two conditions as the guest side: a visible tab AND an open,
    // ACTIVE thread. A console left open on another visitor must not report
    // that this one's messages were read.
    function maybeMarkRead(t) {
      if (!t || !t.conv || document.visibilityState !== "visible") return;
      if (t.id !== activePeerId || section.classList.contains("im-collapsed")) return;
      let top = 0;
      for (const m of t.messages) if (m.dir === "in" && m.id > top) top = m.id;
      if (top) S.sendReceipt({ ref, cid: t.conv.cid, token: t.conv.token, upToId: top, kind: "read" });
    }

    // See the guest side: a receipt can beat our own POST response, so the
    // marks are remembered per thread and re-applied when an id lands.
    function applyMarks(t, m) {
      if (!m || m.dir !== "out" || !m.id) return;
      if (t.ackDelivered && m.id <= t.ackDelivered) m.deliveredAt = m.deliveredAt || t.ackDeliveredAt;
      if (t.ackRead && m.id <= t.ackRead) {
        m.readAt = m.readAt || t.ackReadAt;
        m.deliveredAt = m.deliveredAt || t.ackReadAt;
      }
    }

    function applyReceipt(r) {
      if (!r || r.by === "agent") return; // our own acks are not news
      for (const [, t] of threads) {
        if (!t.conv) continue;
        if (r.kind === "delivered" && r.upToId > (t.ackDelivered || 0)) {
          t.ackDelivered = r.upToId; t.ackDeliveredAt = r.at;
        }
        if (r.kind === "read" && r.upToId > (t.ackRead || 0)) {
          t.ackRead = r.upToId; t.ackReadAt = r.at;
        }
        for (const m of t.messages) applyMarks(t, m);
      }
      renderMessages();
    }

    // deliver(): a visitor's chat message arrived.
    //
    // Selection rule (Ron, 2026-08-01): auto-select the thread when the agent
    // is not already mid-conversation, so the common case — one visitor, one
    // chat — needs no clicking. But if the currently open thread has an inbound
    // message the agent has not answered yet, a message from a DIFFERENT
    // visitor only badges as unread. Yanking the view away mid-reply loses what
    // they were typing and, worse, risks sending it to the wrong person.
    async function deliver(data) {
      const cid = data.cid;
      if (!cid || !data.message) return;

      let t = threads.get(cid);
      if (!t) {
        // First message of a conversation this console has not seen. Claim our
        // capability so replies can go back over it.
        const tok = await S.agentConversationToken(cid);
        if (tok.error) return;
        const gs = data.guestSession || tok.guestSession || "";
        t = {
          id: cid, name: data.guestName || tok.guestName || "Visitor",
          picture: "", role: "guest",
          guestSession: gs, online: true,
          messages: [], unread: 0,
          conv: { cid, token: tok.token },
        };
        threads.set(cid, t);
        S.openConversation({ channel: tok.channel, token: tok.token },
          { onMessage: (m) => receive(m), onReceipt: (r) => applyReceipt(r) });
      }

      if (!addMessage(t, data.message, "in")) return; // already have it
      t.awaitingReply = true;

      const active = activePeerId ? threads.get(activePeerId) : null;
      const busyReplying = active && active.id !== cid && active.awaitingReply;

      S.showSection(".im");
      if (!busyReplying) {
        // openThread(), not just activePeerId: the composer stays DISABLED
        // until a thread is opened, which is why the agent had to hunt for the
        // visitor in the list before they could type a reply.
        openThread({ id: cid, name: t.name, role: "guest", picture: t.picture });
      } else {
        t.unread = (t.unread || 0) + 1;
        renderRoster();
        renderDockUnread();
      }
      // One short notice, never a repeating ring.
      S.playNotice();
    }

    // open(): show a conversation thread and its transcript. The agent side of
    // guest-initiated chat — a push-woken console opening to an empty dock
    // while the guest can see everything they typed is exactly the asymmetry
    // the transcript exists to remove.
    function open({ cid, token, name: peerName, picture: peerPicture, guestSession }) {
      activePeerId = cid;
      if (!threads.has(cid)) {
        threads.set(cid, {
          id: cid, name: peerName || "Visitor",
          picture: peerPicture || "",
          role: "guest", guestSession: guestSession || "",
          online: true, messages: [],
        });
      }
      // Per-thread, not a single module-level `conv`: an agent may hold several
      // conversations at once (that is the whole point of the chat governor),
      // and switching threads must not send into the previous one.
      threads.get(cid).conv = { cid, token };
      S.showSection(".im");
      section.classList.remove("im-collapsed");
      renderMessages();
      S.loadTranscript({ ref, cid, token }).then((res) => {
        const t = threads.get(cid);
        if (!t || !res.messages || !res.messages.length) return;
        // Rebuild from the record, then mark every id seen so a live broadcast
        // that overlaps the transcript is not appended a second time.
        t.messages = [];
        t.seen = new Set();
        for (const m of res.messages) {
          addMessage(t, m, m.sender === "agent" ? "out" : "in");
        }
        renderMessages();
      });
    }

    async function send(text) {
      if (!activePeerId || !text) return;
      const t = threads.get(activePeerId);
      if (!t) return;

      // A guest thread opened from the roster has no conversation yet — its id
      // is the visitor's presence session id, not a cid. Create one on demand
      // so the agent can simply start typing.
      //
      // Without this the send fell through to the agent-to-agent inbox path,
      // which the server now refuses, and the message vanished with no error:
      // exactly the silent failure this work exists to remove.
      if (!t.conv && t.role === "guest") {
        const invited = await S.inviteGuest({
          guestSession: t.id,
          guestName: t.name,
          callType: "chat",
          callerName: displayName,
        });
        if (invited.error) {
          console.error("[IM] could not open a conversation with", t.name, invited);
          return;
        }
        // Re-key this thread to the conversation id. The server returns the
        // visitor's EXISTING open conversation when there is one, so this is
        // how a roster-clicked thread merges with the one their messages are
        // already arriving on, instead of becoming a second entry.
        const existing = threads.get(invited.cid);
        if (existing && existing !== t) {
          existing.messages = existing.messages.concat(t.messages);
          existing.guestSession = existing.guestSession || t.id;
          threads.delete(t.id);
          t = existing;
        } else {
          threads.delete(t.id);
          t.guestSession = t.id;
          t.id = invited.cid;
          threads.set(invited.cid, t);
        }
        // Selection must follow the re-key, or the composer points at a thread
        // that no longer exists and silently refuses to send.
        activePeerId = t.id;
        t.conv = { cid: invited.cid, token: invited.token };
        S.openConversation(
          { channel: invited.channel, token: invited.token },
          { onMessage: (m) => receive(m), onReceipt: (r) => applyReceipt(r) }
        );
        if (!t.loadedTranscript) {
          t.loadedTranscript = true;
          S.loadTranscript({ ref, cid: invited.cid, token: invited.token }).then((res) => {
            if (!res.messages || !res.messages.length) return;
            t.messages = [];
            t.seen = new Set();
            for (const m of res.messages) addMessage(t, m, m.sender === "agent" ? "out" : "in");
            renderMessages();
          });
        }
      }

      const pending = { dir: "out", text, ts: Date.now() };
      t.messages.push(pending);
      t.awaitingReply = false; // answered — another visitor may now take focus
      renderMessages();
      // A conversation thread goes over its private channel; anything else is
      // still the agent-to-agent inbox.
      if (t.conv) {
        const saved = await S.sendConversationMessage({
          ref, cid: t.conv.cid, token: t.conv.token, body: text,
        });
        if (saved && saved.message) {
          pending.id = saved.message.id;
          applyMarks(t, pending); // may already have been acknowledged
          renderMessages();
        }
        return;
      }
      // Guests see the public display name; fellow agents see the real name.
      const outName = t.role === "guest" ? displayName : name;
      await S.sendIM(activePeerId, {
        fromId: sessionId,
        fromName: outName,
        fromRole: "auth",
        fromPicture: picture, // lets the recipient render our avatar
        text,
      });
    }

    function receive(data) {
      // A conversation message from the guest side of an open conversation.
      // deliver() handles the FIRST message (it has to mint a capability);
      // this handles the rest, once the channel is already subscribed.
      if (data && data.cid && data.body) {
        if (data.sender === "agent") return; // our own echo
        const ct = threads.get(data.cid);
        if (!ct) return;
        if (!addMessage(ct, data, "in")) return; // same message via another path
        ct.awaitingReply = true;
        const visible =
          data.cid === activePeerId && !section.classList.contains("im-collapsed");
        if (!visible) ct.unread = (ct.unread || 0) + 1;
        renderRoster();
        renderMessages();
        renderDockUnread();
        if (!visible) S.playNotice();
        return;
      }
      if (!data.fromId || !data.text) return;
      const t = thread({ id: data.fromId, name: data.fromName, role: data.fromRole, picture: data.fromPicture });
      t.messages.push({ dir: "in", text: data.text, ts: data.ts });
      // Counts as "read" only if its thread is open AND the dock is expanded.
      const visible =
        data.fromId === activePeerId && !section.classList.contains("im-collapsed");
      if (visible) {
        renderMessages();
      } else {
        t.unread = (t.unread || 0) + 1;
        // Surface the unread chat: pop the dock open (collapsed → expanded) so
        // the agent sees the bolded thread without having to notice the badge.
        section.classList.remove("im-collapsed");
      }
      renderRoster();
      renderDockUnread();
    }

    formEl.addEventListener("submit", (e) => {
      e.preventDefault();
      const text = inputEl.value.trim();
      if (!text) return;
      inputEl.value = "";
      send(text);
    });

    return { updateRoster, receive, open, deliver, restore, refresh, applyReceipt, maybeMarkRead };
  })();

  // Rebuild open conversations now that IM exists. Not awaited: a slow restore
  // must not delay the rest of the console coming up.
  IM.restore().catch((e) => console.error("[IM] restore failed", e));

  // ─── Helpers ──────────────────────────────────────────────────────────
  function appendGreetingMessage(message, type) {
    const p = document.createElement("p");
    p.style.marginTop = "0.5rem";
    p.style.color = type === "warning" ? "darkorange" : "darkred";
    p.style.fontWeight = "bold";
    p.textContent = message;
    document.querySelector(".greeting").appendChild(p);
  }
})();
