// js/guest.js
// Guest user page logic. Runs on index.html.

(async function () {
  await window.configReady;
  if (!window.Realtime || !window.DB) return; // Config failed to load

  const S = window.Shared;
  if (await S.ensureNameParam()) return; // Prompt for name and reload
  const params = S.getUrlParams();

  // ─── Greeting ──────────────────────────────────────────────────────────
  const greetingH1 = document.querySelector(".greeting h1");
  if (greetingH1) greetingH1.textContent = `Hello, ${params.name}!`;

  S.hideAllSections();
  // Marks the card as holding a conversation. Desktop uses it to give the card
  // the full window height and sit the chat at the bottom of it; without a
  // signal the card would either be tall and empty before a chat starts, or
  // stay short and leave the chat floating mid-screen.
  function showChat() {
    S.showSection(".im");
    document.body.classList.add("guest-chat-open");
  }
  S.showSection(".greeting");

  // ─── Guard: no ref ─────────────────────────────────────────────────────
  if (!params.ref) {
    appendGreetingMessage(
      "No valid link detected. Please use a link with a ref parameter to access this service.",
      "warning"
    );
    return;
  }

  // ─── Guard: no microphone ──────────────────────────────────────────────
  // Detect device EXISTENCE only (enumerateDevices needs no permission) — a
  // visitor who merely opened the page must not get a scary mic prompt.
  // Permission is requested from the call click itself (a user gesture), and
  // that stream is REUSED by the call, so the guest is prompted exactly once,
  // at the moment it makes sense.
  const perms = { hasMic: false, hasCamera: false };
  try {
    const devices = await navigator.mediaDevices.enumerateDevices();
    perms.hasMic = devices.some((d) => d.kind === "audioinput");
    perms.hasCamera = devices.some((d) => d.kind === "videoinput");
  } catch (e) {
    console.warn("[Media] enumerateDevices failed:", e.message);
  }
  if (!perms.hasMic) {
    appendGreetingMessage(
      "A microphone is required to use this service. Please connect one and refresh the page.",
      "alert"
    );
    return;
  }

  // ─── Session setup ─────────────────────────────────────────────────────
  // Reuse the session ID across page refreshes (sessionStorage persists
  // through refreshes but clears when the tab closes). This prevents the
  // same guest from appearing twice in the auth user's list when the page
  // is refreshed — the new presence track replaces the old one with the
  // same key instead of adding a second entry.
  // The id and the capability for this visitor's private inbox are minted
  // together by the server. Client-chosen ids would not do: an agent can see a
  // visitor's session id in presence, so if the client picked it, seeing it
  // would be enough to request its token and read that visitor's invitations.
  let sessionId = sessionStorage.getItem("guestSessionId");
  let guestToken = sessionStorage.getItem("guestSessionToken");
  if (!sessionId || !guestToken) {
    const minted = await S.guestSession(params.ref);
    if (!minted) {
      appendGreetingMessage(
        "We could not start a session. Please refresh and try again.",
        "alert"
      );
      return;
    }
    sessionId = minted.sessionId;
    guestToken = minted.token;
    sessionStorage.setItem("guestSessionId", sessionId);
    sessionStorage.setItem("guestSessionToken", guestToken);
  }
  const presenceData = {
    session_id: sessionId,
    name: params.name,
    role: "guest",
    status: "available",
    has_camera: perms.hasCamera,
    has_mic: true,
    online_since: new Date().toISOString(),
  };

  await S.createSession({
    sessionId,
    ref: params.ref,
    email: params.email,
    name: params.name,
    role: "guest",
    hasCamera: perms.hasCamera,
    hasMic: true,
  });

  const heartbeatTimer = S.setupHeartbeat(sessionId);

  // ─── Auto-call from URL param ──────────────────────────────────────────
  // ?auto=audio or ?auto=video → auto-click the matching button once an
  // eligible agent is available. Consumed after firing.
  let autoCallType = (() => {
    const v = new URLSearchParams(window.location.search).get("auto");
    // `chat` joins audio/video as a first-class entry. Additive: an existing
    // ?auto=audio or ?auto=video invite link keeps working unchanged.
    return v === "audio" || v === "video" || v === "chat" ? v : null;
  })();

  // ─── Show main sections ────────────────────────────────────────────────
  // Call section starts hidden; updateCallButtons will show it when auth users come online

  // ─── App State ─────────────────────────────────────────────────────────
  let state = "ready"; // 'ready' | 'calling' | 'incoming' | 'active-call' | 'message-form'
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
  let presenceChannel = null;
  let inboxChannel = null;
  let authUsers = []; // Available auth users with same ref

  // Picked mic/camera (deviceIds), persisted per-browser. Used as getUserMedia
  // constraints and hot-swapped into an active call via replaceTrack.
  const STORAGE_MIC = "guest-mic";
  const STORAGE_CAM = "guest-cam";
  let selectedMicId = "";
  let selectedCamId = "";
  try { selectedMicId = localStorage.getItem(STORAGE_MIC) || ""; } catch (e) {}
  try { selectedCamId = localStorage.getItem(STORAGE_CAM) || ""; } catch (e) {}

  // ─── Durable-agent discovery ───────────────────────────────────────────
  // Callable agents still come from two sources — consoles open right now, and
  // agents who went Available then closed the tab (reachable via Web Push until
  // they Pause or log out) — but the merge that used to happen HERE now happens
  // on the server.
  //
  // A guest used to subscribe to presence:<ref> and union it with the REST
  // roster itself. It cannot any more, and should never have been able to: ref
  // is the tenant's own domain, so that channel handed anyone who could guess it
  // the full agent roster — and the ability to broadcast into it. The server
  // folds live consoles into /api/agents/available instead (each row carries
  // `live`), so the guest sees exactly the same set with none of the reach.
  // Announce this visitor so the console's waiting list shows them. Write-only:
  // no subscribe, so the roster cannot be read back.
  presenceChannel = S.announcePresence(params.ref, presenceData);

  // This visitor's private inbox, which is how an AGENT opens contact. Gated by
  // the capability minted with the session id above, so an agent who can see
  // the id in presence still cannot subscribe to it.
  inboxChannel = S.openConversation(
    { channel: `guest:${sessionId}`, token: guestToken },
    { onMessage: handleInboxMessage }
  );

  let restAgents = [];
  function mergeAgents() {
    authUsers = restAgents;
    if (state === "ready") updateCallButtons();
  }
  async function refreshRestAgents() {
    try {
      const r = await fetch(
        `/api/agents/available?ref=${encodeURIComponent(params.ref)}`
      );
      if (r.ok) restAgents = (await r.json()).agents || [];
    } catch (e) {
      /* keep the last list rather than blanking the UI on one bad fetch */
    }
    mergeAgents();
  }
  refreshRestAgents();
  // Polled rather than pushed. Tightened from 15s because this is now the ONLY
  // source of liveness for the guest, where live presence used to make an agent
  // appear instantly.
  setInterval(refreshRestAgents, 6000);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) refreshRestAgents();
  });

  // ─── Device pickers (mic / camera), like Zoom/Meet ─────────────────────
  // The guest is granted mic/camera at load (checkMediaPermissions), so device
  // names are available immediately; we still retry + listen for devicechange.
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
  function populateDevicesSoon() {
    populateDevices();
    setTimeout(populateDevices, 400);
    setTimeout(populateDevices, 1200);
  }
  // If a call is live, hot-swap the picked device into the peer connection.
  async function hotSwapDevice(kind) {
    if (!peerConnection) return;
    try {
      const constraints =
        kind === "mic"
          ? { audio: selectedMicId ? { deviceId: { exact: selectedMicId } } : true }
          : { video: selectedCamId ? { deviceId: { exact: selectedCamId } } : true };
      const fresh = await navigator.mediaDevices.getUserMedia(constraints);
      const track = kind === "mic" ? fresh.getAudioTracks()[0] : fresh.getVideoTracks()[0];
      if (!track) return;
      const sender = peerConnection.getSenders().find((s) => s.track && s.track.kind === (kind === "mic" ? "audio" : "video"));
      if (sender) {
        const old = sender.track;
        await sender.replaceTrack(track);
        if (old) old.stop();
        if (localStream) {
          if (old) localStream.removeTrack(old);
          localStream.addTrack(track);
        }
        if (kind === "cam") {
          const lv = document.querySelector(".call-active .local-video");
          if (lv) lv.srcObject = localStream;
        }
      } else {
        track.stop();
      }
    } catch (e) {
      console.warn("[Media] device hot-swap failed:", e.message);
    }
  }
  if (micSelect) {
    micSelect.addEventListener("change", async () => {
      selectedMicId = micSelect.value;
      try { localStorage.setItem(STORAGE_MIC, selectedMicId); } catch (e) {}
      await hotSwapDevice("mic");
    });
  }
  if (camSelect) {
    camSelect.addEventListener("change", async () => {
      selectedCamId = camSelect.value;
      try { localStorage.setItem(STORAGE_CAM, selectedCamId); } catch (e) {}
      await hotSwapDevice("cam");
    });
  }
  populateDevicesSoon();
  if (navigator.mediaDevices && navigator.mediaDevices.addEventListener) {
    navigator.mediaDevices.addEventListener("devicechange", populateDevices);
  }

  // Inbox is set up earlier, alongside the session mint, because it is now
  // capability-gated: the token arrives with the session id and the channel
  // cannot be opened without it. The old inbox:<sessionId> subscription that
  // stood here needed no token at all, which is what let a passer-by listen.

  // ─── Cleanup on page unload ────────────────────────────────────────────
  window.addEventListener("beforeunload", () => {
    clearInterval(heartbeatTimer);
    S.updateSessionStatus(sessionId, "offline");
    if (presenceChannel) presenceChannel.unsubscribe();
    if (inboxChannel) inboxChannel.unsubscribe();
    if (currentCallChannel) currentCallChannel.unsubscribe();
  });

  // ─── Call buttons visibility ───────────────────────────────────────────
  function updateCallButtons() {
    const audioBtn = document.querySelector(".audio-call-button");
    const videoBtn = document.querySelector(".video-call-button");
    const noAgentsMsg = document.querySelector(".no-agents-message");

    const chatBtn = document.querySelector(".chat-button");
    const anyAvailable = authUsers.length > 0;
    const anyHasCamera = authUsers.some((u) => u.modes && u.modes.video);
    const anyChat = authUsers.some((u) => u.modes && u.modes.chat);
    const anyAudio = authUsers.some((u) => u.modes && u.modes.audio);

    S.showSection(".call");
    const cancelBtn = document.querySelector(".send-message .cancel-button");
    if (!anyAvailable) {
      if (noAgentsMsg) noAgentsMsg.style.display = "";
      if (audioBtn) audioBtn.style.display = "none";
      if (videoBtn) videoBtn.style.display = "none";
      if (chatBtn) chatBtn.style.display = "none";
      // Show the message form inline; no cancel since there's nothing to return to.
      const nameInput = document.querySelector(".send-message-form-name");
      const contactInput = document.querySelector(".send-message-form-contact");
      if (nameInput && !nameInput.value)
        nameInput.value = params.name !== "Unknown" ? params.name : "";
      if (contactInput && !contactInput.value)
        contactInput.value = params.email !== "Unknown" ? params.email : "";
      const reasonEl = document.querySelector(".send-message-reason");
      if (reasonEl) reasonEl.textContent = "";
      if (cancelBtn) cancelBtn.style.display = "none";
      S.showSection(".send-message");
    } else {
      if (noAgentsMsg) noAgentsMsg.style.display = "none";
      // Each modality is shown but disabled when nobody is offering it, rather
      // than hidden: a control that vanishes reads as "broken", one that is
      // greyed out reads as "not right now".
      if (audioBtn) { audioBtn.style.display = ""; audioBtn.disabled = !anyAudio; }
      // Keep Video visible but disabled when no video-capable agent is online;
      // the .no-video class on the card reveals a short explanatory hint.
      if (videoBtn) { videoBtn.style.display = ""; videoBtn.disabled = !anyHasCamera; }
      if (chatBtn) { chatBtn.style.display = ""; chatBtn.disabled = !anyChat; }
      if (cancelBtn) cancelBtn.style.display = "";
      S.hideSection(".send-message");
    }
    const guestCard = document.querySelector(".guest-user");
    if (guestCard) guestCard.classList.toggle("no-video", anyAvailable && !anyHasCamera);

    // Auto-call (one-shot): only fires while ready, with a matching agent.
    if (autoCallType && state === "ready") {
      const audioBtnEl = document.querySelector(".audio-call-button");
      const videoBtnEl = document.querySelector(".video-call-button");
      const chatBtnEl = document.querySelector(".chat-button");
      if (autoCallType === "audio" && anyAudio) {
        autoCallType = null;
        audioBtnEl?.click();
      } else if (autoCallType === "video" && anyHasCamera) {
        autoCallType = null;
        videoBtnEl?.click();
      } else if (autoCallType === "chat" && anyChat) {
        autoCallType = null;
        chatBtnEl?.click();
      }
    }
  }

  // ─── Audio call button ─────────────────────────────────────────────────
  document.querySelector(".audio-call-button").addEventListener("click", () => {
    const target = getLongestWaitingAuthUser();
    if (!target) {
      updateCallButtons();
      return;
    }
    initiateCall(target, "audio");
  });

  // ─── Chat button ───────────────────────────────────────────────────────
  // Chat is now a peer of audio and video, not something only an agent could
  // start. The guest picks a target from the same roster and the server mints
  // the conversation, so there is no longer any need to have been messaged
  // first in order to reply.
  document.querySelector(".chat-button")?.addEventListener("click", async () => {
    const target = pickForModality("chat");
    if (!target) {
      updateCallButtons();
      return;
    }
    await startChat(target);
  });

  // ─── Video call button ─────────────────────────────────────────────────
  document.querySelector(".video-call-button").addEventListener("click", () => {
    const target = getLongestWaitingAuthUserWithCamera();
    if (!target) {
      updateCallButtons();
      return;
    }
    initiateCall(target, "video");
  });

  // ─── Find longest-waiting available auth user ──────────────────────────
  // Prefer a LIVE (connected) agent over a push-only (Offline·Reachable) one,
  // so a call rings an agent who's actually at the console rather than a ghost.
  // Only fall back to push-only when no live agent qualifies.
  function pickLongestWaiting(pool) {
    // `live` is now reported by the server (it used to be derived from the
    // presence channel the guest subscribed to).
    const live = pool.filter((u) => u.live);
    const pick = live.length ? live : pool;
    return pick.sort((a, b) => new Date(a.online_since) - new Date(b.online_since))[0] || null;
  }
  // Selection is per MODALITY: an agent taking chat but not video must not be
  // picked for a video call. modes is the agent's stated intent; has_mic /
  // has_camera are kept as the compatibility view of the same thing.
  function pickForModality(modality) {
    return pickLongestWaiting(authUsers.filter((u) => u.modes && u.modes[modality]));
  }
  function getLongestWaitingAuthUser() {
    return pickForModality("audio");
  }

  function getLongestWaitingAuthUserWithCamera() {
    return pickForModality("video");
  }

  // ─── Conversation establishment ────────────────────────────────────────
  // Every contact with an agent — chat, audio or video — now runs over a
  // private conversation channel. The guest holds a capability token for that
  // one conversation and nothing else; knowing another conversation's id would
  // not help, and no id is published anyway.
  //
  // The server decides whether the conversation may exist at all (agent
  // available, takes this modality, has chat capacity), so a stale roster
  // fails here with a clear reason instead of ringing a void.
  let currentConv = null; // { cid, token, channel }

  async function openConversationWith(target, callType) {
    const started = await S.startConversation({
      ref: params.ref,
      callType,
      guestSession: sessionId,
      guestName: params.name,
    });
    if (started.error) {
      // 409 now means only one thing: nobody offers this channel right now.
      // Capacity never refuses — it sorts — so a busy team still gets you a
      // conversation, flagged `waiting`.
      refreshRestAgents();
      return { error: started.error, status: started.status };
    }
    currentConv = {
      cid: started.cid,
      token: started.token,
      channel: started.channel,
      agentName: started.agentName || "",
      waiting: !!started.waiting,
    };
    return currentConv;
  }

  // ─── Start a chat (guest-initiated) ────────────────────────────────────
  async function startChat(target) {
    if (state !== "ready") return;
    const conv = await openConversationWith(target, "chat");
    if (conv.error) {
      updateCallButtons();
      return;
    }
    // Subscribe BEFORE ringing, so a fast agent's first message cannot arrive
    // before we are listening.
    currentCallChannel = S.openConversation(conv, {
      onMessage: (m) => IM.receive(m),
      onReceipt: (r) => IM.applyReceipt(r),
    });
    IM.open({
      cid: conv.cid,
      token: conv.token,
      name: conv.agentName || target.name,
      picture: conv.agentName && conv.agentName !== target.name ? "" : target.picture,
    });
    // Deliberately NO ring. Starting a chat is not placing a call: the visitor
    // types immediately, and the agent is alerted by the MESSAGE — the server
    // delivers it to their console, and pushes to a closed one, when it is
    // sent. Ringing here made a chat present as an accept/decline call with a
    // 30-second deadline, which is not what either side is doing.
  }

  // ─── Initiate call (guest → auth) ──────────────────────────────────────
  async function initiateCall(target, callType) {
    if (state !== "ready") return;
    let targetName = target.name;
    let targetPicture = target.picture || "";

    // Acquire the mic (and camera for video) NOW, from this click's gesture —
    // the only permission prompt the guest ever sees, at the moment it makes
    // sense — with a friendly explainer first (permission priming). The
    // stream is held and reused when the agent answers, so there is no second
    // prompt mid-connection.
    if (!(await primeMicPermission(callType))) return; // guest chose Cancel
    const stream = await getMediaStream(callType);
    if (!stream) {
      showAlert("Microphone access is required to place a call. Please allow it and try again.");
      return;
    }

    // Establish the private conversation FIRST. It is both the permission check
    // (the server refuses a modality the agent has not enabled) and the channel
    // the whole call will signal over, so there is nothing to tear down if the
    // agent turns out to be unavailable.
    const conv = await openConversationWith(target, callType);
    if (conv.error) {
      updateCallButtons();
      showMessageForm("Nobody is taking calls right now. Please leave a message.");
      return;
    }
    if (conv.agentName && conv.agentName !== targetName) {
      targetName = conv.agentName;
      targetPicture = "";
    }
    // A call is exclusive: an agent already on one cannot pick up, so ringing
    // would just burn 30 seconds and end in "no answer". Chat is the honest
    // alternative — it is available right now, and it is a real channel rather
    // than a consolation prize. The conversation already exists, so switching
    // costs the visitor nothing.
    if (conv.waiting) {
      updateCallButtons();
      const anyChat = authUsers.some((u) => u.modes && u.modes.chat);
      if (anyChat) {
        currentCallChannel = S.openConversation(conv, { onMessage: (m) => IM.receive(m) });
        IM.open({ cid: conv.cid, token: conv.token, name: "Live Support" });
        showAlert("Everyone is on a call right now — you can chat instead and we'll reply here.");
      } else {
        showMessageForm("Everyone is on a call right now. Please leave a message.");
      }
      return;
    }

    state = "calling";
    callRole = "caller";
    outgoingCall = { targetName, callType, targetPicture };
    currentCallId = S.generateId();

    // Update presence + session status
    presenceData.status = "in-call";
    await S.updatePresence(presenceChannel, presenceData);
    await S.updateSessionStatus(sessionId, "in-call");

    // Create call record
    await S.createCallRecord({
      callId: currentCallId,
      ref: params.ref,
      callerSessionId: sessionId,
      callerName: params.name,
      calleeSessionId: "",
      calleeName: targetName,
      callType,
    });

    // Signal over the CONVERSATION channel. Previously this was call:<callId>,
    // whose name was guessable from a call id and which the hub let anyone
    // subscribe to — so a third party could have followed, or forged, the
    // WebRTC negotiation. The conversation channel is random-keyed and
    // token-gated, which is what makes caller/callee privacy actually hold.
    currentCallChannel = S.openConversation(conv, {
      onSignal: handleCallSignal,
      onMessage: (m) => IM.receive(m),
    });

    // Show outgoing call UI
    const outH1 = document.querySelector(".call-outgoing h1");
    if (outH1) outH1.textContent = `Calling...`;
    S.hideSection(".call");
    S.showSection(".call-outgoing");

    // One ring path for every surface. The server resolves the agent from the
    // conversation, fans out to their live consoles over WS, queues an invite
    // for a console that opens later, and sends Web Push to wake a closed one —
    // so the guest no longer needs to know the agent's session id, which is the
    // identifier we stopped publishing.
    const ring = await ringPush(conv.cid, callType);

    // Fail fast if the agent is genuinely unreachable, rather than making the
    // guest sit through a 30s dead ring. `live` comes from the server-side
    // discovery merge now that the guest cannot read presence itself.
    const liveTarget = !!target.live;
    if (!ring.queued && !ring.pushed && !liveTarget) {
      await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
      await S.updateCallRecord(currentCallId, { status: "timeout" });
      S.hideSection(".call-outgoing");
      await resetToReady();
      refreshRestAgents(); // drop the stale entry from discovery
      showMessageForm("We were unable to reach an agent. Please leave a message.");
      return;
    }

    // Ring for 30s so a push-woken agent has time to open and answer, then fall
    // back to the leave-a-message form.
    callTimeoutTimer = setTimeout(async () => {
      if (state !== "calling") return;
      await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
      await S.updateCallRecord(currentCallId, { status: "timeout" });
      clearRing(currentCallId);
      S.hideSection(".call-outgoing");
      await resetToReady();
      showMessageForm("We were unable to reach an agent. Please leave a message.");
    }, 30000);
  }

  // ─── Web Push ring helpers (Phase 2) ───────────────────────────────────
  // Returns how many push subscriptions took the ring (0 = closed-tab agent is
  // unreachable; the caller uses this to fail fast instead of ringing a void).
  // Returns the server's ring verdict: queued = the ring reached an available
  // agent's invite queue + live consoles (WS fan-out), pushed = how many push
  // subscriptions were also alerted. Both false/0 = nobody can answer.
  async function ringPush(cid, callType) {
    try {
      const r = await fetch("/api/call/ring", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          ref: params.ref,
          // The conversation identifies the agent, so the guest no longer sends
          // (or knows) their session id. The server resolves the target from
          // the conversation record it created.
          cid,
          token: currentConv ? currentConv.token : "",
          callId: currentCallId,
          callType,
          callerName: params.name,
        }),
      });
      if (r.ok) {
        const v = await r.json();
        return { pushed: v.pushed || 0, queued: !!v.queued };
      }
    } catch (e) {
      /* treated as not-queued — live presence may still deliver over WS */
    }
    return { pushed: 0, queued: false };
  }
  function clearRing(callId) {
    fetch("/api/call/ring/clear", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ callId }),
    }).catch(() => {});
  }

  // ─── Cancel outgoing call ──────────────────────────────────────────────
  document.querySelector(".cancel-call-button").addEventListener("click", async () => {
    if (state !== "calling") return;
    clearTimeout(callTimeoutTimer);
    await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
    await S.updateCallRecord(currentCallId, { status: "cancelled" });
    clearRing(currentCallId);
    S.hideSection(".call-outgoing");
    await resetToReady();
  });

  // Unlock ring audio at the first interaction of any kind, so an agent
  // calling a guest who hasn't clicked anything yet still rings audibly
  // (browsers block audio that wasn't unlocked by a user gesture).
  const primeOnFirstGesture = () => S.primeRingtone();
  window.addEventListener("pointerdown", primeOnFirstGesture, { once: true });
  window.addEventListener("keydown", primeOnFirstGesture, { once: true });

  // ─── Handle inbox messages (incoming calls from auth) ──────────────────
  function handleInboxMessage(data) {
    if (data.type === "incoming-call" && (state === "ready" || state === "message-form")) {
      // If the message form is open, dismiss it so the call can take over
      if (state === "message-form") {
        S.hideSection(".send-message");
      }

      // The invitation carries THIS visitor's capability for the conversation,
      // delivered by the server to this private inbox. Without it there is
      // nothing to join — which is the point: an invitation is a grant, not an
      // announcement anyone could overhear.
      if (!data.cid || !data.token) return;

      state = "incoming";
      callRole = "callee";
      incomingCall = data;
      currentCallId = data.callId;
      currentConv = { cid: data.cid, token: data.token, channel: `conv:${data.cid}` };

      currentCallChannel = S.openConversation(currentConv, {
        onSignal: handleCallSignal,
        onMessage: (m) => IM.receive(m),
        onReceipt: (r) => IM.applyReceipt(r),
      });

      // An agent opening a CHAT shows the thread rather than ringing.
      if (data.callType === "chat") {
        state = "ready";
        IM.open({ cid: data.cid, token: data.token, name: data.callerName });
        return;
      }

      S.playRingtone();

      const callType = data.callType === "video" ? "video" : "audio";
      const incH1 = document.querySelector(".call-incoming h1");
      if (incH1)
        incH1.textContent = `Incoming ${callType} call from ${data.callerName}...`;

      S.hideSection(".call");
      S.showSection(".call-incoming");
    }
  }

  // ─── Accept incoming call ──────────────────────────────────────────────
  document.querySelector(".accept-call-button").addEventListener("click", async () => {
    if (state !== "incoming") return;
    S.stopRingtone();
    S.hideSection(".call-incoming");

    presenceData.status = "in-call";
    await S.updatePresence(presenceChannel, presenceData);
    await S.updateSessionStatus(sessionId, "in-call");

    // Signal accepted — caller (auth) will send the offer
    await S.sendCallSignal(currentCallChannel, { type: "call-accepted" });
    state = "active-call";
    notifyParent("call-started");
  });

  // ─── Decline incoming call ─────────────────────────────────────────────
  document.querySelector(".decline-call-button").addEventListener("click", async () => {
    if (state !== "incoming") return;
    S.stopRingtone();
    S.hideSection(".call-incoming");
    await S.sendCallSignal(currentCallChannel, { type: "call-declined" });
    await S.updateCallRecord(currentCallId, { status: "declined" });
    await resetToReady();
  });

  // ─── Call signal handler ───────────────────────────────────────────────
  async function handleCallSignal(data) {
    switch (data.type) {
      case "call-accepted":
        // We (guest) are the caller; auth accepted → create offer
        if (state !== "calling") return;
        clearTimeout(callTimeoutTimer);
        clearRing(currentCallId);
        state = "active-call";
        notifyParent("call-started");
        await startAsInitiator();
        break;

      case "call-declined":
        if (state === "calling") {
          clearTimeout(callTimeoutTimer);
          clearRing(currentCallId);
          await S.updateCallRecord(currentCallId, { status: "declined" });
          S.hideSection(".call-outgoing");
          await resetToReady();
          showMessageForm("All live agents are currently unavailable. Please leave a message.");
        }
        break;

      case "call-busy":
        if (state === "calling") {
          clearTimeout(callTimeoutTimer);
          clearRing(currentCallId);
          await S.updateCallRecord(currentCallId, { status: "busy" });
          S.hideSection(".call-outgoing");
          await resetToReady();
          showMessageForm("All agents are currently on a call. Please leave a message.");
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
        // We (guest) are the callee; auth (caller) sent offer after we accepted
        if (callRole !== "callee") return;
        await handleOffer(data.sdp);
        break;

      case "answer":
        // We (guest) are the caller; auth (callee) replied with answer
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

  // ─── Start as initiator (guest is caller, auth accepted) ───────────────
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
        showActiveCallUI(outgoingCall.targetName, remoteStream, callType, outgoingCall.targetPicture);
      },
      onConnectionStateChange: async (connState) => {
        if (connState === "disconnected" || connState === "failed") await endCall(false);
      },
    });

    localStream.getTracks().forEach((track) => peerConnection.addTrack(track, localStream));

    const offer = await peerConnection.createOffer();
    await peerConnection.setLocalDescription(offer);
    await S.sendCallSignal(currentCallChannel, { type: "offer", sdp: offer.sdp });
  }

  // ─── Handle offer (guest is callee, auth sent offer after guest accepted) ─
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
        showActiveCallUI(incomingCall.callerName, remoteStream, callType, incomingCall.callerPicture);
      },
      onConnectionStateChange: async (connState) => {
        if (connState === "disconnected" || connState === "failed") await endCall(false);
      },
    });

    localStream.getTracks().forEach((track) => peerConnection.addTrack(track, localStream));

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
  // The stream acquired at the call click is held here and reused when the
  // call connects — the guest is never re-prompted mid-connection.
  let heldStream = null;

  // Permission priming: before the browser's cold "use your microphone?"
  // dialog, show our own explainer with an OK — it raises grant rates and, if
  // the guest cancels OUR modal, the browser permission is never burned (it
  // stays askable next time). Skipped when permission is already granted.
  async function primeMicPermission(callType) {
    if (!window.commandBox) return true; // lib missing — straight to the prompt
    try {
      const q = await navigator.permissions.query({ name: "microphone" });
      if (q.state === "granted") return true;
    } catch (e) {
      /* query unsupported (older Safari) — show the primer anyway */
    }
    const what = callType === "video" ? "microphone and camera" : "microphone";
    const ans = await commandBox(
      `To connect your call, your browser will ask permission to use your ${what}. Please choose Allow when asked.`,
      ["&Ok", "&Cancel"], 1, "INFO");
    return ans === "O";
  }

  async function getMediaStream(callType) {
    if (heldStream && (callType !== "video" || heldStream.getVideoTracks().length > 0)) {
      return heldStream;
    }
    // Try the saved device picks first, then fall back to defaults — a stale
    // pick (device unplugged, or its id rotated by the browser) must never
    // kill the call.
    const audioPick = selectedMicId ? { deviceId: { exact: selectedMicId } } : true;
    const wantVideo = callType === "video" && perms.hasCamera;
    const videoPick = wantVideo ? (selectedCamId ? { deviceId: { exact: selectedCamId } } : true) : false;
    const attempts = [{ audio: audioPick, video: videoPick }];
    if (selectedMicId || (wantVideo && selectedCamId)) {
      attempts.push({ audio: true, video: wantVideo });
    }
    if (wantVideo) {
      attempts.push({ audio: audioPick, video: false });
      if (selectedMicId) attempts.push({ audio: true, video: false });
    }
    for (const c of attempts) {
      try {
        heldStream = await navigator.mediaDevices.getUserMedia(c);
        // First grant exposes device labels — refresh the pickers.
        populateDevicesSoon();
        return heldStream;
      } catch (e) {
        /* try the next, less specific constraint set */
      }
    }
    console.error("[Media] getUserMedia failed for every constraint set");
    return null;
  }

  // Release the held stream (call over or never connected) so the browser's
  // recording indicator turns off.
  function releaseHeldStream() {
    if (heldStream) {
      heldStream.getTracks().forEach((t) => t.stop());
      heldStream = null;
    }
  }

  // ─── Show active call UI ───────────────────────────────────────────────
  function showActiveCallUI(peerName, remoteStream, callType, peerPicture) {
    S.hideSection(".call-incoming");
    S.hideSection(".call-outgoing");

    const callActiveEl = document.querySelector(".call-active");
    callActiveEl.classList.toggle("call-active--video", callType === "video");

    const nameEl = document.querySelector(".call-active .auth-user-name");
    if (nameEl) nameEl.textContent = peerName;

    // Agent avatar overlay (shown especially during audio calls where there is
    // no video to look at). Falls back to the initial-letter placeholder.
    const avatarEl = document.querySelector(".call-active .call-active-avatar");
    if (avatarEl) {
      avatarEl.innerHTML = S.avatarHtml(peerName, peerPicture, "lg");
    }

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

  // ─── Embed notifications ───────────────────────────────────────────────
  // Notify an embedding page (e.g. the in-store kiosk iframe) of call
  // lifecycle events. No-op when not embedded.
  function notifyParent(msg) {
    if (window.parent !== window) window.parent.postMessage(msg, "*");
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
    if (localStream) {
      localStream.getTracks().forEach((t) => t.stop());
      localStream = null;
    }
    releaseHeldStream();

    const remoteVideo = document.querySelector(".call-active .remote-video");
    if (remoteVideo) remoteVideo.srcObject = null;
    const localVideo = document.querySelector(".call-active .local-video");
    if (localVideo) { localVideo.srcObject = null; localVideo.style.display = "none"; }

    if (currentCallId) {
      const updates = { status: "answered" };
      if (callStartTime) {
        updates.duration = Math.floor((Date.now() - callStartTime) / 1000);
      }
      await S.updateCallRecord(currentCallId, updates);
    }

    S.hideSection(".call-active");

    // Notify an embedding page (e.g. the in-store kiosk iframe) that the call
    // is over so it can close the overlay. No-op when not embedded.
    if (window.parent !== window) {
      window.parent.postMessage("call-ended", "*");
    }

    await resetToReady();
  }

  // ─── Reset to ready state ──────────────────────────────────────────────
  async function resetToReady() {
    state = "ready";
    // A call that never connected (cancelled, declined, timed out) must not
    // keep the mic open — release the held stream so the recording indicator
    // turns off.
    releaseHeldStream();
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

    presenceData.status = "available";
    await S.updatePresence(presenceChannel, presenceData);
    await S.updateSessionStatus(sessionId, "available");

    S.hideSection(".call-incoming");
    S.hideSection(".call-outgoing");
    S.hideSection(".call-active");
    S.hideSection(".send-message");
    S.hideSection(".alert");
    S.showSection(".call");
    updateCallButtons();
  }

  // ─── Message form ──────────────────────────────────────────────────────
  function showMessageForm(reason) {
    // A kiosk (?kiosk=1, framed, no keyboard) can't use a leave-a-message
    // form — tell the parent the session is over so it can close the overlay
    // and return to the landing screen. Other embeds (e.g. the planned chat
    // widget iframe) keep the form.
    const isKiosk = new URLSearchParams(window.location.search).get("kiosk") === "1";
    if (isKiosk && window.parent !== window) {
      window.parent.postMessage("call-ended", "*");
      return;
    }

    state = "message-form";

    // Pre-fill from URL params
    const nameInput = document.querySelector(".send-message-form-name");
    const contactInput = document.querySelector(".send-message-form-contact");
    if (nameInput && !nameInput.value) nameInput.value = params.name !== "Unknown" ? params.name : "";
    if (contactInput && !contactInput.value) contactInput.value = params.email !== "Unknown" ? params.email : "";

    const reasonEl = document.querySelector(".send-message-reason");
    if (reasonEl) reasonEl.textContent = reason || "";

    S.hideSection(".call");
    S.showSection(".send-message");
  }

  document.querySelector(".send-message-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = document.querySelector(".send-message-form-name").value.trim();
    const contact = document.querySelector(".send-message-form-contact").value.trim();
    const message = document.querySelector(".send-message-form-message").value.trim();

    if (!name || !contact || !message) {
      showAlert("Please fill in all fields.");
      return;
    }

    const ok = await S.createMessage({ ref: params.ref, name, contact, message });
    if (ok) {
      document.querySelector(".send-message-form").reset();
      showAlert("Your message has been sent. We will get back to you soon.");
      state = "ready";
      S.hideSection(".send-message");
      S.showSection(".call");
      updateCallButtons();
    } else {
      showAlert("Failed to send message. Please try again.");
    }
  });

  document.querySelector(".send-message .cancel-button").addEventListener("click", async () => {
    S.hideSection(".send-message");
    await resetToReady();
  });

  // ─── Alert ─────────────────────────────────────────────────────────────
  function showAlert(message) {
    const el = document.querySelector(".alert h1");
    if (el) el.textContent = message;
    S.showSection(".alert");
  }

  document.querySelector(".alert .close-btn").addEventListener("click", () => {
    S.hideSection(".alert");
  });

  // ─── Instant Messaging (guest view) ────────────────────────────────────
  // A guest can ONLY reply to admins who have messaged them first. There is no
  // roster: the conversation map is populated exclusively from inbound `im`
  // messages, so a guest can never see or cold-message the list of admins.
  // The panel stays hidden until the first admin message arrives. Ephemeral.
  const IM = (function () {
    const section    = document.querySelector(".im");
    const messagesEl = section?.querySelector(".im-messages");
    const nameEl     = section?.querySelector(".im-thread-name");
    const formEl     = section?.querySelector(".im-form");
    const inputEl    = section?.querySelector(".im-input");
    const dockHeader = section?.querySelector(".im-dock-header");
    const dockUnread = section?.querySelector(".im-dock-unread");

    // Keyed by conversation id now, not by an agent's session id — the whole
    // point of the change: a conversation is the unit of privacy.
    const threads = new Map();
    let activeAdminId = null;
    let conv = null; // { cid, token }
    let unread = 0;

    if (!section) {
      return { receive() {}, open() {}, refresh() {}, applyReceipt() {} };
    }

    // Expand / restore, desktop only (the button is display:none below the
    // breakpoint). Kept separate from collapse: collapsing hides the
    // conversation, expanding gives it more room — opposite intents that the
    // same control would muddle.
    const expandBtn = section?.querySelector(".im-expand");
    if (expandBtn) {
      expandBtn.addEventListener("click", (e) => {
        e.stopPropagation(); // must not also toggle the dock
        const on = section.classList.toggle("im-expanded");
        // The chat flows inside the card on desktop rather than floating over
        // it, so widening the chat means widening the card. Marked on the card
        // itself instead of relying on :has(), which would silently do nothing
        // on an older browser and leave the chat overflowing its container.
        document.querySelector(".guest-user")?.classList.toggle("guest-expanded", on);
        expandBtn.setAttribute("aria-pressed", String(on));
        expandBtn.setAttribute("aria-label", on ? "Restore chat size" : "Expand chat");
        // Expanding reveals more of the thread, so scroll to the newest.
        messagesEl.scrollTop = messagesEl.scrollHeight;
      });
    }

    // Click the header bar to minimize / expand, like Facebook chat.
    if (dockHeader) {
      dockHeader.addEventListener("click", () => {
        section.classList.toggle("im-collapsed");
        if (!section.classList.contains("im-collapsed")) {
          unread = 0;
          renderDockUnread();
          inputEl?.focus();
        }
      });
    }

    function renderDockUnread() {
      if (!dockUnread) return;
      if (unread > 0) {
        dockUnread.textContent = unread > 99 ? "99+" : String(unread);
        dockUnread.style.display = "";
      } else {
        dockUnread.style.display = "none";
      }
    }

    function renderMessages() {
      messagesEl.innerHTML = "";
      const t = activeAdminId ? threads.get(activeAdminId) : null;
      if (!t) return;
      // Render the agent's avatar (if any) beside their name in the thread header.
      nameEl.innerHTML = `${S.avatarHtml(t.name, t.picture)}<span class="im-thread-name-text">${S.escapeHtml(t.name || "Agent")}</span>`;
      for (const m of t.messages) {
        const li = document.createElement("li");
        li.className = "im-msg " + (m.dir === "out" ? "im-msg-out" : "im-msg-in");
        li.textContent = m.text;
        // Ticks only on OUR messages: a receipt describes what the other side
        // did, so showing one against their own message would be meaningless.
        if (m.dir === "out") {
          const tick = document.createElement("span");
          const state = m.readAt ? "read" : m.deliveredAt ? "delivered" : m.id ? "sent" : "pending";
          tick.className = "im-tick im-tick--" + state;
          tick.setAttribute("aria-label", {
            pending: "sending", sent: "sent", delivered: "delivered", read: "read",
          }[state]);
          tick.textContent = state === "pending" ? "🕘" : state === "sent" ? "✓" : "✓✓";
          li.appendChild(tick);
        }
        messagesEl.appendChild(li);
      }
      messagesEl.scrollTop = messagesEl.scrollHeight;
    }

    function receive(data) {
      // A conversation message: { id, cid, sender, body, created_at }. This is
      // the shape everything now travels in; the legacy {fromId,text} branch
      // below is only the agent-to-agent inbox format.
      if (data && data.cid && data.body) {
        if (data.sender === "guest") return; // our own echo
        const ct = threads.get(data.cid);
        if (!ct) return;
        ct.seen = ct.seen || new Set();
        if (data.id != null) {
          if (ct.seen.has(data.id)) return; // already rendered
          ct.seen.add(data.id);
        }
        ct.messages.push({ dir: "in", text: data.body, ts: (data.created_at || 0) * 1000, id: data.id });
        // It is on this device now — that is precisely what "delivered" means.
        S.sendReceipt({ ref: params.ref, cid: data.cid, token: conv && conv.token,
                        upToId: data.id, kind: "delivered" });
        maybeMarkRead(ct);
        activeAdminId = data.cid;
        showChat();
        const collapsed = section.classList.contains("im-collapsed");
        if (collapsed) {
          unread += 1;
          renderDockUnread();
        }
        renderMessages();
        // Same chain as the console, and announced on every arrival for the
        // same reason.
        S.notify({ title: (ct && ct.name) || "Live Support", body: data.body || "", tag: "conv-" + data.cid });
        return;
      }
      if (!data.fromId || !data.text) return;
      let t = threads.get(data.fromId);
      if (!t) {
        t = { id: data.fromId, name: data.fromName || "Agent", picture: data.fromPicture || "", messages: [] };
        threads.set(data.fromId, t);
      } else {
        if (data.fromName) t.name = data.fromName;
        if (data.fromPicture !== undefined) t.picture = data.fromPicture;
      }
      t.messages.push({ dir: "in", text: data.text, ts: data.ts });
      // Focus the conversation with whoever just messaged.
      activeAdminId = data.fromId;
      // Reveal the dock. On first appearance, open it; if the guest had
      // minimized it, leave it collapsed and just badge the unread count.
      const firstShow = section.style.display === "none";
      showChat();
      if (firstShow) section.classList.remove("im-collapsed");
      if (section.classList.contains("im-collapsed")) {
        unread += 1;
      }
      renderDockUnread();
      renderMessages();
    }

    // open() is what makes chat guest-initiated. The dock used to be openable
    // only by an inbound agent message — "can't message an admin who never
    // messaged us" — because a guest had no way to address an agent that was
    // not also a way for anyone else to. A conversation capability replaces
    // that restriction with an actual permission.
    function open({ cid, token, name, picture }) {
      conv = { cid, token };
      // Test hook: lets e2e ask the SERVER what it thinks the receipt state is,
      // which is the only way to tell "never sent" from "never heard back".
      window.__convForTest = { cid, token, ref: params.ref };
      activeAdminId = cid;
      if (!threads.has(cid)) {
        threads.set(cid, { id: cid, name: name || "Agent", picture: picture || "", messages: [] });
      }
      showChat();
      section.classList.remove("im-collapsed");
      renderMessages();
      inputEl?.focus();
      // Show anything already said — an agent may have replied before this tab
      // subscribed, and a transcript that starts blank is a lie about history.
      S.loadTranscript({ ref: params.ref, cid, token }).then((res) => {
        const t = threads.get(cid);
        if (!t || !res.messages || !res.messages.length) return;
        const carried = t.messages || [];
        t.seen = new Set();
        t.messages = res.messages.map((m) => {
          if (m.id != null) t.seen.add(m.id);
          return {
            dir: m.sender === "guest" ? "out" : "in",
            text: m.body,
            ts: m.created_at * 1000,
            id: m.id,
            deliveredAt: m.delivered_at || 0,
            readAt: m.read_at || 0,
          };
        });
        for (const m of carried) {
          if (m.id != null && t.seen.has(m.id)) continue;
          if (m.id != null) t.seen.add(m.id);
          t.messages.push(m);
        }
        renderMessages();
      });
    }

    // "Read" is a claim about a human, so it needs BOTH: the tab is actually
    // visible, and the dock is open. An unattended tab left on screen must not
    // report that someone read anything.
    function maybeMarkRead(t) {
      if (!conv || !t || document.visibilityState !== "visible") return;
      if (section.classList.contains("im-collapsed")) return;
      let top = 0;
      for (const m of t.messages) if (m.dir === "in" && m.id > top) top = m.id;
      if (top) S.sendReceipt({ ref: params.ref, cid: conv.cid, token: conv.token, upToId: top, kind: "read" });
    }

    // The other side acknowledged us: move our ticks.
    // Receipt high-water marks, kept per thread.
    //
    // A receipt can arrive BEFORE the sender learns its own message id: the
    // recipient acknowledges the moment they render, which can beat our own
    // POST response. Applying receipts only to messages that already have ids
    // silently drops those, and nothing ever re-applies them — the tick then
    // sits on "sent" forever even though the server has both timestamps.
    //
    // So the marks are remembered and re-applied when an id lands.
    function applyMarks(t, m) {
      if (!m || m.dir !== "out" || !m.id) return;
      if (t.ackDelivered && m.id <= t.ackDelivered) m.deliveredAt = m.deliveredAt || t.ackDeliveredAt;
      if (t.ackRead && m.id <= t.ackRead) {
        m.readAt = m.readAt || t.ackReadAt;
        m.deliveredAt = m.deliveredAt || t.ackReadAt;
      }
    }

    function applyReceipt(r) {
      if (!conv || !r || r.by === "guest") return; // our own acks are not news
      const t = threads.get(conv.cid);
      if (!t) return;
      if (r.kind === "delivered" && r.upToId > (t.ackDelivered || 0)) {
        t.ackDelivered = r.upToId; t.ackDeliveredAt = r.at;
      }
      if (r.kind === "read" && r.upToId > (t.ackRead || 0)) {
        t.ackRead = r.upToId; t.ackReadAt = r.at;
      }
      for (const m of t.messages) applyMarks(t, m);
      renderMessages();
    }

    // Rebuild this conversation from the SERVER record.
    //
    // A WebSocket broadcast sent while the socket is down is gone — config.js
    // says so explicitly, and on mobile the tab is frozen the moment it goes to
    // the background, which is exactly when an agent replies. The transcript is
    // the record, so re-reading it is the catch-up; the message ids make it
    // idempotent, so this can run as often as we like.
    async function refresh() {
      if (!conv) return;
      const res = await S.loadTranscript({ ref: params.ref, cid: conv.cid, token: conv.token });
      const t = threads.get(conv.cid);
      if (!t || !res.messages || !res.messages.length) return;
      const carried = t.messages || [];
      t.seen = new Set();
      t.messages = res.messages.map((m) => {
        if (m.id != null) t.seen.add(m.id);
        return {
          dir: m.sender === "guest" ? "out" : "in",
          text: m.body,
          ts: m.created_at * 1000,
          id: m.id,
          deliveredAt: m.delivered_at || 0,
          readAt: m.read_at || 0,
        };
      });
      for (const m of carried) {
        if (m.id != null && t.seen.has(m.id)) continue;
        if (m.id != null) t.seen.add(m.id);
        t.messages.push(m);
      }
      renderMessages();
      maybeMarkRead(t); // a catch-up can deliver something now on screen
    }

    // Catch up on BOTH signals. Reconnect covers a dropped socket; visibility
    // covers a phone that froze the tab without ever closing it, which is the
    // case that lost the message.
    if (window.Realtime.onReconnect) window.Realtime.onReconnect(() => refresh());
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState !== "visible") return;
      refresh().then(() => { if (conv) maybeMarkRead(threads.get(conv.cid)); });
    });

    async function send(text) {
      if (!conv || !text) return;
      const t = threads.get(conv.cid);
      if (!t) return;
      // Optimistic, with no id yet — that is what "pending" means, and it is
      // why the tick has a fourth state rather than starting at "sent".
      const pending = { dir: "out", text, ts: Date.now() };
      t.messages.push(pending);
      renderMessages();
      const saved = await S.sendConversationMessage({
        ref: params.ref,
        cid: conv.cid,
        token: conv.token,
        body: text,
      });
      if (saved && saved.message) {
        pending.id = saved.message.id; // now "sent"
        applyMarks(t, pending);        // ...and possibly already acknowledged
        renderMessages();
      }
    }

    formEl.addEventListener("submit", (e) => {
      e.preventDefault();
      const text = inputEl.value.trim();
      if (!text) return;
      inputEl.value = "";
      send(text);
    });

    return { receive, open, refresh, applyReceipt };
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
