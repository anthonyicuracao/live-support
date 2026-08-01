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
    const audio = await agent.evaluate(() => window.Shared?.noticeState?.() ?? "no Shared");
    check(
      "notice audio is unlocked on the console",
      audio === "running",
      `AudioContext state is ${audio} — a notice would be silent`
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

    check("no console errors on either page", errors.length === 0, errors.slice(0, 5).join("\n      "));
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
