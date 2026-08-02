// End-to-end tests: a real guest and a real agent, two browser contexts, one
// live appliance.
//
// Why these exist
// ---------------
// Every server rule has a unit test and all of them passed while manual testing
// found four defects in a row. The bugs were never rule violations — they were
// WIRING failures between two clients and a server, and nothing in the suite
// exercised two clients talking to each other.
//
// So each test here asserts something manual testing actually caught:
//   - the visitor appears ONCE, not twice
//   - both sides see both messages, exactly once each
//   - the agent can type without first hunting for the right thread
//   - the transcript survives a reload
//
// Run: node e2e/run.mjs   (boots nothing — expects the appliance already up)
import { chromium } from "/Users/ronpinkas1/git/insta-rebuild-hub/node_modules/playwright/index.mjs";

const BASE = process.env.LS_BASE || "http://127.0.0.1:8001";
const REF = process.env.LS_REF || "e2e.local";
const ADMIN_USER = process.env.LS_ADMIN || "admin";
const ADMIN_PW = process.env.LS_ADMIN_PW || "e2e-initial-pw";
const NEW_PW = "e2e-agent-password-1";

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok, detail });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok || !detail ? "" : "\n      " + detail}`);
}

// The console is a single page with everything on it; these are the bits the
// tests touch.
const SEL = {
  peopleItems: ".im-roster li, .im-roster .im-peer",
  messages: ".im-messages li",
  input: ".im-input",
  send: ".im-send",
  availability: "#availability-toggle",
  chatBtn: ".chat-button",
  guestMessages: ".im-messages li",
  guestInput: ".im-input",
};

async function agentSignIn(page) {
  await page.goto(`${BASE}/login?ref=${REF}`, { waitUntil: "domcontentloaded" });
  await page.fill('input[name="username"]', ADMIN_USER);
  await page.fill('input[name="password"]', ADMIN_PW);
  await page.click('button[type="submit"]');
  await page.waitForLoadState("domcontentloaded");
  // First login forces a password change: current + new + confirm.
  if (page.url().includes("/account/password")) {
    await page.fill('input[name="current"]', ADMIN_PW);
    await page.fill('input[name="new"]', NEW_PW);
    await page.fill('input[name="confirm"]', NEW_PW);
    await page.click('button[type="submit"]');
    await page.waitForLoadState("domcontentloaded");
  }
  if (!page.url().includes("auth.html")) {
    await page.goto(`${BASE}/auth.html?ref=${REF}`, { waitUntil: "domcontentloaded" });
  }
  // The availability control is a visually-hidden checkbox behind a styled
  // slider, so it is attached but never "visible". Wait on attachment and drive
  // it through its label, the way a person does.
  await page.waitForSelector(SEL.availability, { state: "attached", timeout: 20000 });
}

async function goAvailable(page) {
  // Wait for auth.js to enable it — it ships disabled so an early click cannot
  // flip the UI without a real status update.
  await page.waitForFunction(() => {
    const el = document.getElementById("availability-toggle");
    return el && !el.disabled;
  }, { timeout: 20000 });
  const toggle = page.locator(SEL.availability);
  if (!(await toggle.isChecked())) {
    await page.locator(".availability__switch").click();
  }
  await page.waitForFunction(() => {
    const el = document.getElementById("availability-toggle");
    return el && el.checked;
  }, { timeout: 10000 });
  // The console posts availability; give the server a beat to record it.
  await page.waitForTimeout(1500);
}

async function guestOpen(context, name) {
  const page = await context.newPage();
  await page.goto(`${BASE}/index.html?ref=${REF}&name=${encodeURIComponent(name)}`, {
    waitUntil: "domcontentloaded",
  });
  // The Chat button only appears once discovery reports an agent, and the guest
  // polls every 6s — so this waits on the CONDITION, not a fixed delay.
  try {
    await page.waitForSelector(`${SEL.chatBtn}:visible`, { timeout: 30000 });
  } catch (e) {
    const roster = await page.evaluate(async (ref) => {
      const r = await fetch(`/api/agents/available?ref=${encodeURIComponent(ref)}`);
      return r.ok ? await r.text() : `HTTP ${r.status}`;
    }, REF);
    throw new Error(`Chat button never appeared. /api/agents/available said: ${roster}`);
  }
  return page;
}

async function textsOf(page, selector) {
  return (await page.locator(selector).allTextContents()).map((s) => s.trim()).filter(Boolean);
}

(async () => {
  // Fake media devices: going Available acquires the mic, and a headless
  // browser with no devices fails that acquisition — which is a real condition
  // worth its own test, but not the one these are about.
  const browser = await chromium.launch({
    args: [
      "--use-fake-ui-for-media-stream",
      "--use-fake-device-for-media-stream",
      "--autoplay-policy=no-user-gesture-required",
    ],
  });
  // Separate contexts: the agent's session cookie must not leak into the guest,
  // or the guest would be authenticated and the whole capability model untested.
  const perms = { permissions: ["microphone", "camera"], origin: BASE };
  const agentCtx = await browser.newContext(perms);
  const guestCtx = await browser.newContext(perms);

  const agent = await agentCtx.newPage();
  const errors = [];
  const logs = [];
  agent.on("console", (m) => {
    logs.push(`[agent:${m.type()}] ${m.text()}`);
    if (m.type() === "error") errors.push(`[agent] ${m.text()}`);
  });

  try {
    await agentSignIn(agent);
    await goAvailable(agent);
    check("agent can sign in and go available", true);

    // A ref differing only in case is the SAME tenant. Ron hit the opposite:
    // a mis-typed capital resolved to a different database with none of his
    // users in it, so a correct password reported a failed login.
    const mixed = REF.toUpperCase();
    const sameTenant = await agent.evaluate(async ([base, a, b]) => {
      const one = await fetch(`${base}/api/online?ref=${encodeURIComponent(a)}`).then((r) => r.status);
      const two = await fetch(`${base}/api/online?ref=${encodeURIComponent(b)}`).then((r) => r.status);
      return { one, two };
    }, [BASE, REF, mixed]);
    check(
      "a ref differing only in case resolves to the same tenant",
      sameTenant.one === sameTenant.two,
      `${REF} -> ${sameTenant.one}, ${mixed} -> ${sameTenant.two}`
    );

    // And an unknown ref must not BECOME a tenant just by being asked for.
    // Rendering the login form used to create the database and seed an admin
    // into it, with ADMIN_INITIAL_PASSWORD as its password where that is set.
    //
    // Touched here, asserted by run.mjs against the data directory: no HTTP
    // response can answer this, because /api/online deliberately reports the
    // same empty list for an unknown ref as for a quiet one — it must not
    // become a way to enumerate tenants. The only honest evidence is whether a
    // file appeared on disk.
    await agent.evaluate(async ([base, ref]) => {
      // Fetched rather than navigated to: the server renders the same form
      // either way, and the agent's own session must survive the check.
      await fetch(`${base}/login?ref=${encodeURIComponent(ref)}`);
    }, [BASE, "never-provisioned-probe"]);

    // The video modality must be reachable on a console that has never yet
    // acquired a camera — which is every console, the first time. This shipped
    // deadlocked: the control was disabled unless perms.hasCamera, and
    // hasCamera only became true as a result of acquiring a camera, which was
    // only attempted when video was already ticked. Ron hit it on Android and
    // the tick simply did nothing, in both Available states.
    //
    // Chromium runs with fake media here, so a camera is present and permitted
    // and the probe should succeed.
    await agent.evaluate(() => {
      const el = document.getElementById("availability-toggle");
      if (el && el.checked) el.click(); // pause: modes are locked while Available
    });
    await agent.waitForTimeout(500);
    const videoBox = agent.locator("#video-mode-toggle");
    const wasDisabled = await videoBox.isDisabled();
    check(
      "the video modality can be ticked before a camera has ever been acquired",
      wasDisabled === false,
      "#video-mode-toggle is disabled on a console that has never acquired a camera — the deadlock"
    );
    if (!wasDisabled) {
      await agent.evaluate(() => document.getElementById("video-mode-toggle").click());
      await agent.waitForTimeout(2000);
      const stuck = await agent.evaluate(() => document.getElementById("video-mode-toggle").checked);
      check(
        "and it stays ticked once the camera probe succeeds",
        stuck === true,
        "the tick reverted — the camera probe failed where fake media should have satisfied it"
      );
      // Put it back so the rest of the run sees the state it expects.
      await agent.evaluate(() => {
        const v = document.getElementById("video-mode-toggle");
        if (v.checked) v.click();
        const a = document.getElementById("availability-toggle");
        if (a && !a.checked) a.click();
      });
      await agent.waitForTimeout(1200);
    }

    // Diagnostic: what does the SERVER think this agent's state is? Splits
    // "availability never recorded" from "recorded but not discoverable".
    const avail = await agent.evaluate(async () => {
      const r = await fetch("/api/availability");
      return r.ok ? await r.text() : `HTTP ${r.status}`;
    });
    console.log(`      server availability: ${avail}`);

    const guest = await guestOpen(guestCtx, "E2E Visitor");
    guest.on("console", (m) => { if (m.type() === "error") errors.push(`[guest] ${m.text()}`); });

    // ── guest starts a chat and sends the first message ──────────────────
    await guest.click(SEL.chatBtn);
    await guest.waitForSelector(SEL.guestInput, { timeout: 15000 });
    await guest.fill(SEL.guestInput, "Hi from the visitor");
    await guest.press(SEL.guestInput, "Enter");
    await guest.waitForTimeout(1500);

    // ── the console should show ONE entry for this visitor ───────────────
    await agent.waitForTimeout(2000);
    const people = await textsOf(agent, SEL.peopleItems);
    const visitorEntries = people.filter((p) => p.includes("E2E") || p.includes("Visitor"));
    check(
      "visitor appears exactly once in the console",
      visitorEntries.length === 1,
      `saw ${visitorEntries.length}: ${JSON.stringify(people)}`
    );

    // ── the message is visible, once ─────────────────────────────────────
    const agentMsgs = await textsOf(agent, SEL.messages);
    const inbound = agentMsgs.filter((m) => m.includes("Hi from the visitor"));
    check(
      "agent sees the visitor's message exactly once",
      inbound.length === 1,
      `saw ${inbound.length}: ${JSON.stringify(agentMsgs)}`
    );

    // ── the agent can reply WITHOUT first selecting a thread ─────────────
    const composerReady = await agent.locator(SEL.input).isEditable().catch(() => false);
    check("composer is usable without hunting for the thread", composerReady);

    await agent.fill(SEL.input, "Reply from the agent");
    await agent.press(SEL.input, "Enter");
    await agent.waitForTimeout(1800);

    // ── the visitor keeps their own message AND sees the reply ───────────
    const guestMsgs = await textsOf(guest, SEL.guestMessages);
    check(
      "visitor's own first message is still there",
      guestMsgs.some((m) => m.includes("Hi from the visitor")),
      JSON.stringify(guestMsgs)
    );
    check(
      "visitor sees the agent's reply exactly once",
      guestMsgs.filter((m) => m.includes("Reply from the agent")).length === 1,
      JSON.stringify(guestMsgs)
    );

    // ── still one entry after the exchange ───────────────────────────────
    const peopleAfter = await textsOf(agent, SEL.peopleItems);
    const afterEntries = peopleAfter.filter((p) => p.includes("E2E") || p.includes("Visitor"));
    check(
      "still exactly one visitor entry after both sides have spoken",
      afterEntries.length === 1,
      `saw ${afterEntries.length}: ${JSON.stringify(peopleAfter)}`
    );

    // The notice sound is silent unless a user gesture has unlocked audio.
    // WebKit (Safari) starts every AudioContext suspended, so creating it
    // lazily inside a message handler — never a gesture — produced no blip and
    // no error. The agent has clicked by now (sign-in, availability), so it
    // must be running.
    // Sticky activation is the whole mechanism, and both surfaces already have
    // the interaction that grants it: the agent MUST toggle Available, the
    // visitor MUST click Chat. So audio should be unlocked on both without
    // anything extra being asked of anyone.
    const audio = await agent.evaluate(() => window.Shared?.noticeState?.() ?? "no Shared");
    check(
      "toggling Available unlocks audio on the console",
      audio === "running",
      `AudioContext state is ${audio} — a notice would be silent`
    );
    const guestAudio = await guest.evaluate(() => window.Shared?.noticeState?.() ?? "no Shared");
    check(
      "clicking Chat unlocks audio for the visitor",
      guestAudio === "running",
      `AudioContext state is ${guestAudio}`
    );

    // ── delivery ticks ───────────────────────────────────────────────────
    // A receipt that never arrives is invisible: the message still shows, it
    // just never gains its second tick. So assert the STATE, not the presence
    // of a tick element.
    const tickState = async (page) => page.evaluate(() => {
      const els = [...document.querySelectorAll(".im-msg-out .im-tick")];
      const last = els[els.length - 1];
      if (!last) return "none";
      return (last.className.match(/im-tick--(\w+)/) || [])[1] || "unknown";
    });

    await guest.waitForTimeout(2000);
    const guestTick = await tickState(guest);
    // Report the SERVER's view too: a tick stuck at "sent" means either the
    // receipt was never sent, or it was recorded and the sender never heard.
    // Those are different bugs and the tick alone cannot tell them apart.
    const serverView = await guest.evaluate(async () => {
      const t = window.__convForTest;
      if (!t) return "no conv";
      const qs = new URLSearchParams({ ref: t.ref, cid: t.cid, token: t.token });
      const r = await fetch(`/api/conversation/messages?${qs}`);
      if (!r.ok) return `HTTP ${r.status}`;
      const j = await r.json();
      const mine = (j.messages || []).filter((m) => m.sender === "guest");
      const last = mine[mine.length - 1];
      return last ? `delivered_at=${last.delivered_at} read_at=${last.read_at}` : "no messages";
    }).catch((e) => "probe failed: " + e.message);
    check(
      "the visitor's message shows delivered or read, not just sent",
      guestTick === "delivered" || guestTick === "read",
      `tick="${guestTick}"; server says ${serverView}`
    );

    // The AGENT gets the same signal. It is the same question in both
    // directions — did it land, was it read — and the console had no ticks at
    // all while the guest did.
    const agentTick = await tickState(agent);
    check(
      "the agent's own reply also shows a delivery state",
      agentTick === "delivered" || agentTick === "read",
      `agent tick is "${agentTick}"`
    );

    // Spy on the alert BEFORE the follow-up, with the agent's thread open and
    // the page focused — the posture an agent holds all day, and the one that
    // shipped silent. deliver() (first message of a thread) and this follow-up
    // path are separate code, and fixing the announcement in one left the other
    // gated on visibility, so an agent watching the conversation was told
    // nothing. Asserting the ALERT fires, rather than that a sound was heard,
    // is what makes this checkable: audibility is not observable from the page,
    // but whether we tried is.
    await agent.evaluate(() => {
      window.__alerts = [];
      const real = window.Shared.notify;
      window.Shared.notify = (a) => { window.__alerts.push(a); return real(a); };
    });

    // A SECOND visitor message must also go blue. Only the first went through
    // the path that marks a thread read, so follow-ups stayed grey even though
    // the agent had read and answered them.
    await guest.fill(SEL.guestInput, "second message from the visitor");
    await guest.press(SEL.guestInput, "Enter");
    await guest.waitForTimeout(2500);
    const secondTick = await guest.evaluate(() => {
      const els = [...document.querySelectorAll(".im-msg-out .im-tick")];
      const last = els[els.length - 1];
      return last ? (last.className.match(/im-tick--(\w+)/) || [])[1] : "none";
    });
    check(
      "a follow-up visitor message is marked read too, not just the first",
      secondTick === "read",
      `second message tick is "${secondTick}" — read marking only ran for the first`
    );

    const alerts = await agent.evaluate(() => window.__alerts || []);
    check(
      "the console announces a follow-up even with the thread open and focused",
      alerts.length >= 1,
      "no alert fired — the agent watching the conversation was told nothing"
    );
    check(
      "and the alert carries the message that arrived",
      alerts.some((a) => (a.body || "").includes("second message from the visitor")),
      JSON.stringify(alerts)
    );

    // Unfocused is the case that matters most and is the easiest to ship
    // broken, because a headless page is focused by default and never
    // exercises it. Blurring the page makes the console take the away-from-
    // screen branch for real.
    await agent.evaluate(() => window.dispatchEvent(new Event("blur")));
    await agent.evaluate(() => Object.defineProperty(document, "hasFocus", { value: () => false, configurable: true }));
    const awayRoute = await agent.evaluate(() => window.Shared.notify({ title: "t", body: "b" }));
    check(
      "an unfocused console still makes a sound, not just a silent banner",
      awayRoute === "audio" || awayRoute === "audio+notification",
      `notify() resolved to ${awayRoute} — away from the screen is exactly when sound is the signal that works`
    );

    // One message must never produce two chimes. With alerts permission
    // granted the OS banner carries its own sound, so posting it un-silenced
    // alongside our blip double-fires — which is what Ron would have heard,
    // since his console has that permission.
    const doubled = await agent.evaluate(async () => {
      const posted = [];
      const fakeReg = { showNotification: (t, o) => { posted.push(o); } };
      const realGet = navigator.serviceWorker.getRegistration.bind(navigator.serviceWorker);
      navigator.serviceWorker.getRegistration = async () => fakeReg;
      Object.defineProperty(Notification, "permission", { value: "granted", configurable: true });
      const route = await window.Shared.notify({ title: "t", body: "b" });
      navigator.serviceWorker.getRegistration = realGet;
      return { route, posted };
    });
    check(
      "an OS banner posted alongside our blip is silent, so one message is one chime",
      doubled.posted.length === 1 && doubled.posted[0].silent === true,
      JSON.stringify(doubled)
    );

    // ── a message sent while the guest is OFFLINE still arrives ──────────
    //
    // Ron's multi-device case: a phone freezes the tab the moment it goes to
    // the background, the agent replies into a dead socket, and that broadcast
    // is gone for good. config.js documents the hazard; nothing handled it for
    // chat, so the message was silently skipped while later ones arrived.
    await guestCtx.setOffline(true);
    await agent.fill(SEL.input, "Sent while the visitor was offline");
    await agent.press(SEL.input, "Enter");
    await agent.waitForTimeout(1200);

    const whileOffline = await textsOf(guest, SEL.guestMessages);
    check(
      "the offline visitor genuinely missed it (test is meaningful)",
      !whileOffline.some((m) => m.includes("while the visitor was offline")),
      "the guest received it while offline — this test proves nothing"
    );

    await guestCtx.setOffline(true === false); // back online
    // Nudge the page the way returning to a tab does.
    await guest.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
    await guest.waitForTimeout(2500);

    const afterBack = await textsOf(guest, SEL.guestMessages);
    check(
      "the missed message is recovered when the visitor comes back",
      afterBack.filter((m) => m.includes("while the visitor was offline")).length === 1,
      JSON.stringify(afterBack)
    );

    // ── transcript survives a console reload ─────────────────────────────
    await agent.reload({ waitUntil: "domcontentloaded" });
    await agent.waitForSelector(SEL.availability, { state: "attached", timeout: 20000 });
    await agent.waitForTimeout(2500);
    const afterReload = await textsOf(agent, SEL.messages);
    check(
      "transcript survives a console reload",
      afterReload.some((m) => m.includes("Hi from the visitor")),
      JSON.stringify(afterReload)
    );

    // After a reload the page has had no gesture, so audio is locked again.
    // That is browser policy, not something we can code around — it is pinned
    // so nobody later "fixes" the silence by weakening the unlock and wonders
    // why Safari still says nothing. The unread badge carries it until the
    // agent touches the page.
    const audioAfterReload = await agent.evaluate(() => window.Shared?.noticeState?.() ?? "no Shared");
    check(
      "audio is locked again after a reload, until the agent interacts",
      audioAfterReload !== "running",
      `state is ${audioAfterReload} — expected not-running before any gesture`
    );

    // The regression this pins is the one Ron actually hit: a chime for a
    // message that had arrived minutes earlier, firing on his next keystroke.
    //
    // Cause was that resume() is asynchronous — the old code kicked it off and
    // then scheduled the note anyway. Scheduling on a suspended context does not
    // fail, it QUEUES, and the queue drains the moment the gesture resumes it.
    // So the sound was real, correctly generated, and attached to the wrong
    // moment entirely.
    //
    // Asserting playNotice() reports false while suspended is what proves
    // nothing was handed to the audio clock to replay later.
    const queued = await agent.evaluate(() => window.Shared.playNotice());
    check(
      "a locked context is never scheduled onto (no chime replays on the next keystroke)",
      queued === false,
      `playNotice() returned ${queued} while the context was ${audioAfterReload}`
    );

    // And with audio unavailable the alert still has to land somewhere.
    const fallback = await agent.evaluate(() => window.Shared.notify({ title: "t", body: "b" }));
    check(
      "with audio locked the alert falls back rather than vanishing",
      fallback === "notification" || fallback === "badge",
      `notify() resolved to ${fallback}`
    );

    // Mute is the agent-facing control for all of this. It has to survive a
    // reload — an agent who silenced the console before a meeting should not
    // have it shout again because they refreshed a tab.
    await agent.evaluate(() => window.Shared.setNoticeMuted(true));
    await agent.reload({ waitUntil: "domcontentloaded" });
    await agent.waitForTimeout(400);
    const stillMuted = await agent.evaluate(() => window.Shared.noticeMuted());
    check("mute survives a reload", stillMuted === true, `noticeMuted() = ${stillMuted}`);

    const mutedBell = await agent.evaluate(() => {
      const b = document.querySelector("[data-mute-toggle]");
      return b ? b.getAttribute("aria-pressed") : "no button";
    });
    check(
      "the console shows a mute control reflecting the stored state",
      mutedBell === "true",
      `aria-pressed = ${mutedBell}`
    );

    // Muted means SILENT, not unmonitored: no sound even with audio unlocked,
    // but the alert must still land somewhere visible.
    await agent.mouse.click(5, 5);
    await agent.waitForTimeout(300);
    const mutedPlay = await agent.evaluate(() => window.Shared.playNotice());
    check("muted plays nothing even once audio is unlocked", mutedPlay === false, `playNotice() = ${mutedPlay}`);
    const mutedRoute = await agent.evaluate(() => window.Shared.notify({ title: "t", body: "b" }));
    check(
      "a muted console still routes the alert somewhere visible",
      mutedRoute === "notification" || mutedRoute === "badge",
      `notify() resolved to ${mutedRoute}`
    );
    await agent.evaluate(() => window.Shared.setNoticeMuted(false));

    // A deliberately-offline page logs network failures; those are the test
    // doing its job, not a defect. Everything else must still be clean —
    // filtered narrowly rather than relaxing the check, so a real error during
    // the offline window would still fail.
    const realErrors = errors.filter((e) => !/ERR_INTERNET_DISCONNECTED|ERR_NETWORK_CHANGED|Failed to fetch/i.test(e));
    check("no unexpected console errors on either page", realErrors.length === 0,
      realErrors.slice(0, 5).join("\n      "));
  } catch (e) {
    check("test run completed without throwing", false, String(e && e.stack ? e.stack : e));
    // Page console is the only window into a wiring failure — print the tail.
    console.log("\n--- page console (last 30) ---");
    console.log(logs.slice(-30).join("\n"));
  } finally {
    await browser.close();
  }

  const failed = results.filter((r) => !r.ok);
  console.log(`\n${results.length - failed.length}/${results.length} passed`);
  process.exit(failed.length ? 1 : 0);
})();
