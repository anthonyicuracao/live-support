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
  const perms = await S.checkMediaPermissions();
  if (!perms.hasMic) {
    appendGreetingMessage(
      "Microphone access is required to use this service. Please allow microphone access and refresh the page.",
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
  let sessionId = sessionStorage.getItem("guestSessionId");
  if (!sessionId) {
    sessionId = S.generateId();
    sessionStorage.setItem("guestSessionId", sessionId);
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
    return v === "audio" || v === "video" ? v : null;
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

  // ─── Presence ──────────────────────────────────────────────────────────
  presenceChannel = S.joinPresenceChannel(params.ref, presenceData, (users) => {
    // Only track auth users with same ref who are available and have mic
    authUsers = users.filter(
      (u) => u.role === "auth" && u.status === "available" && u.has_mic
    );
    if (state === "ready") updateCallButtons();
  });

  // ─── Inbox ─────────────────────────────────────────────────────────────
  inboxChannel = S.subscribeToInbox(sessionId, handleInboxMessage);

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

    const anyAvailable = authUsers.length > 0;
    const anyHasCamera = authUsers.some((u) => u.has_camera);

    S.showSection(".call");
    const cancelBtn = document.querySelector(".send-message .cancel-button");
    if (!anyAvailable) {
      if (noAgentsMsg) noAgentsMsg.style.display = "";
      if (audioBtn) audioBtn.style.display = "none";
      if (videoBtn) videoBtn.style.display = "none";
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
      if (audioBtn) audioBtn.style.display = "";
      // Keep Video visible but disabled when no video-capable agent is online;
      // the .no-video class on the card reveals a short explanatory hint.
      if (videoBtn) { videoBtn.style.display = ""; videoBtn.disabled = !anyHasCamera; }
      if (cancelBtn) cancelBtn.style.display = "";
      S.hideSection(".send-message");
    }
    const guestCard = document.querySelector(".guest-user");
    if (guestCard) guestCard.classList.toggle("no-video", anyAvailable && !anyHasCamera);

    // Auto-call (one-shot): only fires while ready, with a matching agent.
    if (autoCallType && state === "ready") {
      const audioBtnEl = document.querySelector(".audio-call-button");
      const videoBtnEl = document.querySelector(".video-call-button");
      if (autoCallType === "audio" && anyAvailable) {
        autoCallType = null;
        audioBtnEl?.click();
      } else if (autoCallType === "video" && anyHasCamera) {
        autoCallType = null;
        videoBtnEl?.click();
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
    initiateCall(target.session_id, target.name, "audio", target.picture);
  });

  // ─── Video call button ─────────────────────────────────────────────────
  document.querySelector(".video-call-button").addEventListener("click", () => {
    const target = getLongestWaitingAuthUserWithCamera();
    if (!target) {
      updateCallButtons();
      return;
    }
    initiateCall(target.session_id, target.name, "video", target.picture);
  });

  // ─── Find longest-waiting available auth user ──────────────────────────
  function getLongestWaitingAuthUser() {
    return authUsers
      .filter((u) => u.has_mic)
      .sort((a, b) => new Date(a.online_since) - new Date(b.online_since))[0] || null;
  }

  function getLongestWaitingAuthUserWithCamera() {
    return authUsers
      .filter((u) => u.has_mic && u.has_camera)
      .sort((a, b) => new Date(a.online_since) - new Date(b.online_since))[0] || null;
  }

  // ─── Initiate call (guest → auth) ──────────────────────────────────────
  async function initiateCall(targetSessionId, targetName, callType, targetPicture) {
    if (state !== "ready") return;

    state = "calling";
    callRole = "caller";
    outgoingCall = { targetSessionId, targetName, callType, targetPicture: targetPicture || "" };
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
      calleeSessionId: targetSessionId,
      calleeName: targetName,
      callType,
    });

    // Subscribe to call channel
    currentCallChannel = S.setupCallChannel(currentCallId, handleCallSignal);

    // Show outgoing call UI
    const outH1 = document.querySelector(".call-outgoing h1");
    if (outH1) outH1.textContent = `Calling...`;
    S.hideSection(".call");
    S.showSection(".call-outgoing");

    // Notify target
    await S.sendToInbox(targetSessionId, {
      type: "incoming-call",
      callId: currentCallId,
      callerId: sessionId,
      callerName: params.name,
      callType,
    });

    // 5-second timeout → show message form
    callTimeoutTimer = setTimeout(async () => {
      if (state !== "calling") return;
      await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
      await S.updateCallRecord(currentCallId, { status: "timeout" });
      S.hideSection(".call-outgoing");
      await resetToReady();
      showMessageForm("We were unable to reach an agent. Please leave a message.");
    }, 10000);
  }

  // ─── Cancel outgoing call ──────────────────────────────────────────────
  document.querySelector(".cancel-call-button").addEventListener("click", async () => {
    if (state !== "calling") return;
    clearTimeout(callTimeoutTimer);
    await S.sendCallSignal(currentCallChannel, { type: "call-cancelled" });
    await S.updateCallRecord(currentCallId, { status: "cancelled" });
    S.hideSection(".call-outgoing");
    await resetToReady();
  });

  // ─── Handle inbox messages (incoming calls from auth) ──────────────────
  function handleInboxMessage(data) {
    if (data.type === "im") {
      IM.receive(data);
      return;
    }
    if (data.type === "incoming-call" && (state === "ready" || state === "message-form")) {
      // If the message form is open, dismiss it so the call can take over
      if (state === "message-form") {
        S.hideSection(".send-message");
      }

      state = "incoming";
      callRole = "callee";
      incomingCall = data;
      currentCallId = data.callId;

      currentCallChannel = S.setupCallChannel(currentCallId, handleCallSignal);
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
        state = "active-call";
        await startAsInitiator();
        break;

      case "call-declined":
        if (state === "calling") {
          clearTimeout(callTimeoutTimer);
          await S.updateCallRecord(currentCallId, { status: "declined" });
          S.hideSection(".call-outgoing");
          await resetToReady();
          showMessageForm("All live agents are currently unavailable. Please leave a message.");
        }
        break;

      case "call-busy":
        if (state === "calling") {
          clearTimeout(callTimeoutTimer);
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
  async function getMediaStream(callType) {
    try {
      return await navigator.mediaDevices.getUserMedia({
        audio: true,
        video: callType === "video" && perms.hasCamera,
      });
    } catch (e) {
      console.error("[Media] getUserMedia error:", e.message);
      return null;
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
    await resetToReady();
  }

  // ─── Reset to ready state ──────────────────────────────────────────────
  async function resetToReady() {
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

    // adminId -> { id, name, messages:[{dir,text,ts}] }
    const threads = new Map();
    let activeAdminId = null;
    let unread = 0;

    if (!section) {
      return { receive() {} };
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
        messagesEl.appendChild(li);
      }
      messagesEl.scrollTop = messagesEl.scrollHeight;
    }

    function receive(data) {
      // Only inbound admin messages create or update a thread. This is the
      // sole way a guest ever learns an admin's session id.
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
      S.showSection(".im");
      if (firstShow) section.classList.remove("im-collapsed");
      if (section.classList.contains("im-collapsed")) {
        unread += 1;
      }
      renderDockUnread();
      renderMessages();
    }

    async function send(text) {
      if (!activeAdminId || !text) return;
      const t = threads.get(activeAdminId);
      if (!t) return; // can't message an admin who never messaged us
      t.messages.push({ dir: "out", text, ts: Date.now() });
      renderMessages();
      await S.sendIM(activeAdminId, {
        fromId: sessionId,
        fromName: params.name,
        fromRole: "guest",
        text,
      });
    }

    formEl.addEventListener("submit", (e) => {
      e.preventDefault();
      const text = inputEl.value.trim();
      if (!text) return;
      inputEl.value = "";
      send(text);
    });

    return { receive };
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
