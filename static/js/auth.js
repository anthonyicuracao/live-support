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
    identity = { ref: me.ref, name: me.name, email: me.email, isAdmin: me.isAdmin };
  }

  const ref     = identity.ref;
  const email   = identity.email || "";
  const name    = identity.name  || "Agent";
  const isAdmin = !!identity.isAdmin;

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
  (function renderAccountBar() {
    if (!me) return;
    const greeting = document.querySelector(".greeting");
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

  // ─── Availability toggle (persisted per-admin) ────────────────────────
  // When the admin flips this off they appear "offline" to guests and to
  // the /api/online endpoint, so no one can call them.
  const STORAGE_AVAILABLE = "auth-user-available";
  function loadAvailability() {
    try {
      const v = localStorage.getItem(STORAGE_AVAILABLE);
      // Default to PAUSED on first load — the agent explicitly goes Available,
      // which is the moment camera/mic permission is requested.
      return v === null ? false : v === "true";
    } catch (e) {
      return false; // localStorage unavailable (private mode, etc.)
    }
  }
  function saveAvailability(on) {
    try {
      localStorage.setItem(STORAGE_AVAILABLE, String(on));
    } catch (e) {
      // Best-effort — toggle still works for the current session.
    }
  }
  let isAvailable = loadAvailability();
  function availabilityStatus() {
    return isAvailable ? "available" : "offline";
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
    const audio = selectedMicId ? { deviceId: { exact: selectedMicId } } : true;
    const video = wantVideo ? (selectedCamId ? { deviceId: { exact: selectedCamId } } : true) : false;
    try {
      liveStream = await navigator.mediaDevices.getUserMedia({ audio, video });
    } catch (e) {
      if (wantVideo) {
        try { liveStream = await navigator.mediaDevices.getUserMedia({ audio }); } catch (e2) { liveStream = null; }
      } else {
        liveStream = null;
      }
    }
    if (liveStream) {
      result.hasMic = liveStream.getAudioTracks().length > 0;
      result.hasCamera = liveStream.getVideoTracks().length > 0;
    }
    return result;
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

  const presenceData = {
    session_id: sessionId,
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
  // createSession always writes status=available; reconcile with toggle.
  if (!isAvailable) {
    await S.updateSessionStatus(sessionId, "offline");
  }

  // Reflect persisted state in the UI and wire up the change handler.
  const availabilityInput = document.getElementById("availability-toggle");
  const availabilityStateEl = document.getElementById("availability-state");
  const videoModeInput = document.getElementById("video-mode-toggle");
  function renderAvailabilityUI() {
    if (availabilityInput) availabilityInput.checked = isAvailable;
    if (availabilityStateEl) {
      availabilityStateEl.textContent = isAvailable ? "✅" : "🛑";
    }
    if (videoModeInput) {
      videoModeInput.checked = wantsVideo;
      videoModeInput.disabled = isAvailable; // lock the mode choice while available
    }
  }
  renderAvailabilityUI();

  // Video-mode picker: editable only while Paused; saved per-browser.
  if (videoModeInput) {
    videoModeInput.addEventListener("change", () => {
      wantsVideo = videoModeInput.checked;
      saveVideoPref(wantsVideo);
    });
  }

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
        // later arrives asynchronously, and ask for notification permission so a
        // backgrounded tab can still alert on an incoming call.
        S.primeRingtone();
        S.requestNotifyPermission();
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
        // Subscribe to Web Push so calls reach us even when the tab is hidden
        // or closed (best-effort; degrades to the inbox path where unsupported).
        if (window.Push) Push.enablePush(sessionId);
      } else if (liveStream) {
        // Going Paused: release the camera/mic so the device indicator clears,
        // and stop server-side push (we're not taking calls).
        liveStream.getTracks().forEach((t) => t.stop());
        liveStream = null;
        if (window.Push) Push.disablePush();
      }
      isAvailable = goingAvailable;
      saveAvailability(isAvailable);
      renderAvailabilityUI();
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

  // ─── Pagination state ──────────────────────────────────────────────────
  // Must be declared before the log-load calls below (let/const are not
  // hoisted like function declarations, so they'd be in the TDZ otherwise).
  const LOG_PAGE_SIZE = 5;
  let callsPage = 0;
  let messagesPage = 0;
  let sessionsPage = 0;
  let adminSessionsPage = 0;

  // ─── Show main sections ────────────────────────────────────────────────
  S.showSection(".call");

  // Logs: all auth users see calls + messages; admin also sees sessions
  if (!isAdmin) {
    S.showSection(".logs");
    document.querySelector(".log-sessions").style.display = "none";
    const adminSessionsEl = document.querySelector(".log-admin-sessions");
    if (adminSessionsEl) adminSessionsEl.style.display = "none";
    loadCallsLog(ref);
    loadMessagesLog(ref);
  } else {
    S.showSection(".logs");
    loadLogs(ref);
    subscribeToLogChanges(ref);

    // ─── Admin: Delete All buttons ────────────────────────────────────────
    const deleteAllCalls = document.getElementById("delete-all-calls");
    const deleteAllMessages = document.getElementById("delete-all-messages");
    const deleteAllSessions = document.getElementById("delete-all-sessions");

    if (deleteAllCalls) {
      deleteAllCalls.style.display = "";
      deleteAllCalls.addEventListener("click", async () => {
        if (!confirm("Delete all call records for this ref? This cannot be undone.")) return;
        const { error } = await window.DB.deleteCalls({ ref });
        if (error) { console.error("[Delete All] Calls:", error.message); return; }
        loadCallsLog(ref, 0);
      });
    }

    if (deleteAllMessages) {
      deleteAllMessages.style.display = "";
      deleteAllMessages.addEventListener("click", async () => {
        if (!confirm("Delete all messages for this ref? This cannot be undone.")) return;
        const { error } = await window.DB.deleteMessages({ ref });
        if (error) { console.error("[Delete All] Messages:", error.message); return; }
        loadMessagesLog(ref, 0);
      });
    }

    if (deleteAllSessions) {
      deleteAllSessions.style.display = "";
      deleteAllSessions.addEventListener("click", async () => {
        if (!confirm("Delete all session records for this ref? This cannot be undone.")) return;
        // Scope to guest sessions only — the Admin Sessions table has its
        // own delete controls and admin rows should survive a guest purge.
        const { error } = await window.DB.deleteSessions({ ref, notRole: "auth" });
        if (error) { console.error("[Delete All] Sessions:", error.message); return; }
        loadSessionsLog(ref, 0);
      });
    }

    const deleteAllAdminSessions = document.getElementById("delete-all-admin-sessions");
    if (deleteAllAdminSessions) {
      deleteAllAdminSessions.style.display = "";
      deleteAllAdminSessions.addEventListener("click", async () => {
        if (!confirm("Delete all admin session records for this ref? This cannot be undone.")) return;
        // Scope to admin (auth) sessions only — guest rows are untouched.
        const { error } = await window.DB.deleteSessions({ ref, role: "auth" });
        if (error) { console.error("[Delete All] Admin sessions:", error.message); return; }
        loadAdminSessionsLog(ref, 0);
      });
    }
  }

  // ─── App State ─────────────────────────────────────────────────────────
  let state = "ready"; // 'ready' | 'calling' | 'incoming' | 'active-call'
  let callRole = null; // 'caller' | 'callee'
  let currentCallId = null;
  let currentCallChannel = null;
  let outgoingCall = null; // { targetSessionId, targetName, callType }
  let incomingCall = null; // { callId, callerId, callerName, callType }
  let peerConnection = null;
  let localStream = null;
  let callStartTime = null;
  let callTimeoutTimer = null;
  let iceCandidateBuffer = [];
  let waitTimeInterval = null;
  let presenceChannel = null;
  let inboxChannel = null;
  let guestUsers = [];

  // ─── Presence ──────────────────────────────────────────────────────────
  presenceChannel = S.joinPresenceChannel(ref, presenceData, (users) => {
    // Only track guests with same ref who are available
    guestUsers = users.filter(
      (u) => u.role === "guest" && u.status === "available"
    );
    if (state === "ready") renderGuestList();
    // Admins see the full roster (guests + other admins, same ref) for IM.
    // Guests never receive this list — that asymmetry is what keeps the admin
    // roster hidden from guests.
    IM.updateRoster(users);
  });

  // ─── Inbox ─────────────────────────────────────────────────────────────
  inboxChannel = S.subscribeToInbox(sessionId, handleInboxMessage);

  // ─── Web Push (Phase 2) ────────────────────────────────────────────────
  // Register the service worker so a backgrounded/closed tab can be woken and
  // rung, and re-hydrate any call still ringing for us when the page (re)gains
  // focus — e.g. right after the agent clicks a push notification.
  if (window.Push) Push.registerServiceWorker();
  checkPendingInvites();
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) checkPendingInvites();
  });

  // ─── Cleanup on page unload ────────────────────────────────────────────
  window.addEventListener("beforeunload", () => {
    clearInterval(heartbeatTimer);
    S.updateSessionStatus(sessionId, "offline");
    if (presenceChannel) presenceChannel.unsubscribe();
    if (inboxChannel) inboxChannel.unsubscribe();
    if (currentCallChannel) currentCallChannel.unsubscribe();
  });

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

    // Subscribe to call channel
    currentCallChannel = S.setupCallChannel(currentCallId, handleCallSignal);

    // Show outgoing call UI
    const outH1 = document.querySelector(".call-outgoing h1");
    if (outH1) outH1.textContent = `Calling ${targetName}...`;
    S.hideSection(".call");
    S.hideSection(".logs");
    S.showSection(".call-outgoing");

    // Notify target (the popup the guest sees uses the public display name)
    await S.sendToInbox(targetSessionId, {
      type: "incoming-call",
      callId: currentCallId,
      callerId: sessionId,
      callerName: displayName,
      callerPicture: picture, // guest renders this on the call overlay
      callType,
    });

    // 5-second timeout
    callTimeoutTimer = setTimeout(async () => {
      if (state !== "calling") return;
      await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
      await S.updateCallRecord(currentCallId, { status: "timeout" });
      showAlert("User did not answer.");
      await resetToReady();
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
    if (data.type !== "incoming-call") return;

    // If we're not free, immediately tell the caller we're busy
    if (state !== "ready") {
      S.sendToInbox(data.callerId, { type: "call-busy" });
      return;
    }

    presentIncomingCall(data);
  }

  // Render the incoming-call UI and start ringing. Shared by the live inbox
  // path and the pending-invite re-hydration (after a push wakes the agent).
  // `data` is { callId, callType, callerName, callerId? }.
  function presentIncomingCall(data) {
    state = "incoming";
    callRole = "callee";
    incomingCall = data;
    currentCallId = data.callId;

    currentCallChannel = S.setupCallChannel(currentCallId, handleCallSignal);
    S.playRingtone();

    const callType = data.callType === "video" ? "video" : "audio";
    // Backgrounded tabs throttle audio — also raise an OS notification so the
    // agent doesn't miss the call while the tab is hidden.
    S.notifyIncomingCall("Incoming call", `${callType} call from ${data.callerName}`);
    const incH1 = document.querySelector(".call-incoming h1");
    if (incH1)
      incH1.textContent = `Incoming ${callType} call from ${data.callerName}...`;

    S.hideSection(".call");
    S.hideSection(".logs");
    S.showSection(".call-incoming");
  }

  // After a push notification wakes/focuses the console, re-hydrate any call
  // that's still ringing for this agent (the guest waits ~30s).
  async function checkPendingInvites() {
    if (state !== "ready") return;
    try {
      const r = await fetch("/api/call/pending");
      if (!r.ok) return;
      const { invites } = await r.json();
      if (invites && invites.length) presentIncomingCall(invites[0]);
    } catch (e) {
      /* best effort */
    }
  }

  // ─── Accept incoming call ──────────────────────────────────────────────
  document.querySelector(".accept-call-button").addEventListener("click", async () => {
    if (state !== "incoming") return;
    S.stopRingtone();
    S.clearIncomingNotification();
    S.hideSection(".call-incoming");

    presenceData.status = "in-call";
    await S.updatePresence(presenceChannel, presenceData);
    await S.updateSessionStatus(sessionId, "in-call");

    // Signal accepted — caller (guest) will send the offer
    await S.sendCallSignal(currentCallChannel, { type: "call-accepted" });
    state = "active-call"; // Will get offer next; offer handler sets up the PC
  });

  // ─── Decline incoming call ─────────────────────────────────────────────
  document.querySelector(".decline-call-button").addEventListener("click", async () => {
    if (state !== "incoming") return;
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
        if (state !== "calling") return;
        clearTimeout(callTimeoutTimer);
        state = "active-call";
        await startAsInitiator();
        break;

      case "call-declined":
        if (state === "calling") {
          clearTimeout(callTimeoutTimer);
          await S.updateCallRecord(currentCallId, { status: "declined" });
          showAlert("User declined the call.");
          await resetToReady();
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
    try {
      const audio = selectedMicId ? { deviceId: { exact: selectedMicId } } : true;
      const video = callType === "video" && perms.hasCamera
        ? (selectedCamId ? { deviceId: { exact: selectedCamId } } : true) : false;
      return await navigator.mediaDevices.getUserMedia({ audio, video });
    } catch (e) {
      console.error("[Media] getUserMedia error:", e.message);
      showAlert("Could not access media devices.");
      return null;
    }
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
    if (!isAdmin) {
      document.querySelector(".log-sessions").style.display = "none";
      const adminSessionsEl = document.querySelector(".log-admin-sessions");
      if (adminSessionsEl) adminSessionsEl.style.display = "none";
    }
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
    sessionsPage = 0;
    adminSessionsPage = 0;
    await loadCallsLog(ref, 0);
    await loadMessagesLog(ref, 0);
    await loadSessionsLog(ref, 0);
    await loadAdminSessionsLog(ref, 0);
  }

  function subscribeToLogChanges(ref) {
    // Writers publish a "refresh" broadcast on dashboard:<ref>; refetch the
    // matching log (debounced) when it arrives so the dashboard stays live.
    const debounce = (fn, ms = 150) => {
      let t;
      return () => { clearTimeout(t); t = setTimeout(fn, ms); };
    };
    const reloadSessions = debounce(() => {
      loadSessionsLog(ref, sessionsPage);
      loadAdminSessionsLog(ref, adminSessionsPage);
    });
    const reloaders = {
      calls:    debounce(() => loadCallsLog(ref, callsPage)),
      messages: debounce(() => loadMessagesLog(ref, messagesPage)),
      sessions: reloadSessions,
    };
    window.Realtime
      .channel(`dashboard:${ref}`)
      .on("broadcast", { event: "refresh" }, ({ payload }) => {
        const fn = reloaders[payload && payload.table];
        if (fn) fn();
      })
      .subscribe();
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
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td>${S.escapeHtml(msg.message_id || "")}</td>
        <td>${S.escapeHtml(msg.ref || "")}</td>
        <td>${S.escapeHtml(msg.name || "")}</td>
        <td>${S.escapeHtml(msg.contact || "")}</td>
        <td>${S.escapeHtml(msg.message || "")}</td>
        <td>${msg.created_at ? new Date(msg.created_at).toLocaleString() : ""}</td>
        <td>${isAdmin ? `<button data-id="${msg.id}" class="delete-msg-btn">Delete</button>` : ""}</td>
      `;
      tbody.appendChild(tr);
    });
    if (isAdmin) {
      tbody.querySelectorAll(".delete-msg-btn").forEach((btn) => {
        btn.addEventListener("click", async () => {
          await window.DB.deleteMessageById(btn.dataset.id);
          btn.closest("tr").remove();
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
  }

  async function loadSessionsLog(ref, page = 0) {
    sessionsPage = page;
    // Exclude admin (auth) rows — they get their own Admin Sessions table.
    const { data, error, count } = await window.DB.listSessions({
      ref, page, pageSize: LOG_PAGE_SIZE, notRole: "auth",
    });

    if (error) { console.error("[Logs] Sessions:", error.message); return; }

    const tbody = document.querySelector(".log-sessions-table tbody");
    if (!tbody) return;
    tbody.innerHTML = "";
    (data || []).forEach((sess) => {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td>${S.escapeHtml(sess.session_id || "")}</td>
        <td>${S.escapeHtml(sess.ref || "")}</td>
        <td>${S.escapeHtml(sess.email || "")}</td>
        <td>${S.escapeHtml(sess.name || "")}</td>
        <td>${S.escapeHtml(sess.status || "")}</td>
        <td>${sess.logged_in_at ? new Date(sess.logged_in_at).toLocaleString() : ""}</td>
        <td>${sess.last_seen_at ? new Date(sess.last_seen_at).toLocaleString() : ""}</td>
        <td>${isAdmin ? `<button data-id="${sess.id}" class="delete-sess-btn">Delete</button>` : ""}</td>
      `;
      tbody.appendChild(tr);
    });
    if (isAdmin) {
      tbody.querySelectorAll(".delete-sess-btn").forEach((btn) => {
        btn.addEventListener("click", async () => {
          await window.DB.deleteSessionById(btn.dataset.id);
          btn.closest("tr").remove();
          if (tbody.querySelectorAll("tr").length === 0 && sessionsPage > 0) {
            loadSessionsLog(ref, sessionsPage - 1);
            return;
          }
          renderPagination("sessions-pagination", sessionsPage, (count || 1) - 1,
            (p) => loadSessionsLog(ref, p));
        });
      });
    }

    renderPagination("sessions-pagination", page, count || 0,
      (p) => loadSessionsLog(ref, p));
    if (isAdmin) {
      const btn = document.getElementById("delete-all-sessions");
      if (btn) btn.style.display = (count || 0) > 1 ? "" : "none";
    }
  }

  async function loadAdminSessionsLog(ref, page = 0) {
    adminSessionsPage = page;
    const { data, error, count } = await window.DB.listSessions({
      ref, page, pageSize: LOG_PAGE_SIZE, role: "auth",
    });

    if (error) { console.error("[Logs] Admin sessions:", error.message); return; }

    const tbody = document.querySelector(".log-admin-sessions-table tbody");
    if (!tbody) return;
    tbody.innerHTML = "";
    (data || []).forEach((sess) => {
      const tr = document.createElement("tr");
      tr.innerHTML = `
        <td>${S.escapeHtml(sess.session_id || "")}</td>
        <td>${S.escapeHtml(sess.ref || "")}</td>
        <td>${S.escapeHtml(sess.email || "")}</td>
        <td>${S.escapeHtml(sess.name || "")}</td>
        <td>${S.escapeHtml(sess.status || "")}</td>
        <td>${sess.logged_in_at ? new Date(sess.logged_in_at).toLocaleString() : ""}</td>
        <td>${sess.last_seen_at ? new Date(sess.last_seen_at).toLocaleString() : ""}</td>
        <td>${isAdmin ? `<button data-id="${sess.id}" class="delete-admin-sess-btn">Delete</button>` : ""}</td>
      `;
      tbody.appendChild(tr);
    });
    if (isAdmin) {
      tbody.querySelectorAll(".delete-admin-sess-btn").forEach((btn) => {
        btn.addEventListener("click", async () => {
          await window.DB.deleteSessionById(btn.dataset.id);
          btn.closest("tr").remove();
          if (tbody.querySelectorAll("tr").length === 0 && adminSessionsPage > 0) {
            loadAdminSessionsLog(ref, adminSessionsPage - 1);
            return;
          }
          renderPagination("admin-sessions-pagination", adminSessionsPage, (count || 1) - 1,
            (p) => loadAdminSessionsLog(ref, p));
        });
      });
    }

    renderPagination("admin-sessions-pagination", page, count || 0,
      (p) => loadAdminSessionsLog(ref, p));
    if (isAdmin) {
      const btn = document.getElementById("delete-all-admin-sessions");
      if (btn) btn.style.display = (count || 0) > 1 ? "" : "none";
    }
  }

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
      return { updateRoster() {}, receive() {} };
    }
    // The IM section is always available to admins; show the dock (collapsed,
    // tucked into the bottom-right corner until the agent opens it).
    section.style.display = "";
    section.classList.add("im-collapsed");

    // No conversation selected yet — hide the thread pane until the agent
    // picks someone from the roster.
    if (threadEl) threadEl.style.display = "none";

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
      // Mark existing threads online/offline so we can still show history for
      // someone who just went offline.
      for (const [id, t] of threads) t.online = onlineIds.has(id);
      // Dock title shows the live count of online guests available to chat.
      if (dockTitle) {
        const guestCount = roster.filter((u) => u.role === "guest").length;
        dockTitle.textContent = `Online (${guestCount})`;
      }
      renderRoster();
    }

    function renderRoster() {
      rosterEl.innerHTML = "";
      // Union of currently-online people and anyone we have an open thread with.
      const byId = new Map();
      roster.forEach((u) => byId.set(u.id, { ...u, online: true }));
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
        const li = document.createElement("li");
        li.className = "im-roster-item" + (u.id === activePeerId ? " im-active" : "");
        li.dataset.peerId = u.id;
        const unread = t && t.unread > 0
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

    async function send(text) {
      if (!activePeerId || !text) return;
      const t = threads.get(activePeerId);
      if (!t) return;
      t.messages.push({ dir: "out", text, ts: Date.now() });
      renderMessages();
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

    return { updateRoster, receive };
  })();

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
