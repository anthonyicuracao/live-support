// Layout screenshots against the hermetic server:  E2E_SPEC=shots.mjs node e2e/run.mjs
import { chromium } from "/Users/ronpinkas1/git/insta-rebuild-hub/node_modules/playwright/index.mjs";
import { mkdirSync } from "node:fs";
const BASE = process.env.LS_BASE, REF = process.env.LS_REF;
const OUT = "/tmp/ls-shots";
mkdirSync(OUT, { recursive: true });
const b = await chromium.launch({ args: ["--use-fake-ui-for-media-stream", "--use-fake-device-for-media-stream"] });
const shot = async (p, n) => { await p.screenshot({ path: `${OUT}/${n}.png` }); console.log(`  ${n}.png`); };
const overflow = (p, tag) => p.evaluate(() => ({
  docW: document.documentElement.scrollWidth, winW: window.innerWidth,
  culprits: [...document.querySelectorAll("*")]
    .filter((el) => el.getBoundingClientRect().right > window.innerWidth + 1)
    .slice(0, 6).map((el) => `${el.tagName.toLowerCase()}.${(el.className && el.className.baseVal !== undefined ? el.className.baseVal : el.className) || "-"} → ${Math.round(el.getBoundingClientRect().right)}px`),
})).then((o) => console.log(`  ${tag}:`, JSON.stringify(o)));

const perms = { permissions: ["microphone", "camera"], origin: BASE };
const ctx = await b.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, ...perms });
const p = await ctx.newPage();
await p.goto(`${BASE}/login?ref=${REF}`, { waitUntil: "domcontentloaded" });
await p.fill('input[name="username"]', process.env.LS_ADMIN);
await p.fill('input[name="password"]', process.env.LS_ADMIN_PW);
await p.click('button[type="submit"]');
await p.waitForLoadState("domcontentloaded");
if (p.url().includes("/account/password")) {
  const NP = "e2e-changed-pw-1";
  await p.fill('input[name="current"]', process.env.LS_ADMIN_PW);
  await p.fill('input[name="new"]', NP); await p.fill('input[name="confirm"]', NP);
  await p.click('button[type="submit"]'); await p.waitForLoadState("domcontentloaded");
}
if (!p.url().includes("auth.html")) await p.goto(`${BASE}/auth.html?ref=${REF}`, { waitUntil: "domcontentloaded" });
await p.waitForTimeout(1500);
await p.locator("#availability-toggle").waitFor({ state: "attached" });
await p.evaluate(() => { const el = document.getElementById("availability-toggle"); if (!el.checked) el.click(); });
await p.waitForTimeout(2000);

// Guest starts a chat so the console has a roster + thread to lay out.
const gctx = await b.newContext({ viewport: { width: 1440, height: 900 }, ...perms });
const g = await gctx.newPage();
await g.goto(`${BASE}/?ref=${REF}&name=Jane+Visitor&email=jane@example.com`, { waitUntil: "domcontentloaded" });
await g.waitForTimeout(2500);
console.log("guest desktop:");
await shot(g, "guest-desktop-landing");
await g.locator(".chat-button").click();
await g.waitForTimeout(1500);
await g.fill(".im-input", "Hello from the desktop visitor");
await g.press(".im-input", "Enter");
await g.waitForTimeout(2500);
await shot(g, "guest-desktop-chat");
console.log("  geometry:", JSON.stringify(await g.evaluate(() => {
  const r = document.querySelector(".im").getBoundingClientRect();
  const msgs = document.querySelector(".im-messages")?.getBoundingClientRect();
  return { top: Math.round(r.top), height: Math.round(r.height), width: Math.round(r.width),
           msgsHeight: msgs ? Math.round(msgs.height) : null, winH: window.innerHeight };
})));

console.log("agent console (mobile), chat active:");
await p.waitForTimeout(1500);
await shot(p, "console-mobile-chat");
await overflow(p, "390px");
console.log("  thread geometry:", JSON.stringify(await p.evaluate(() => {
  const t = document.querySelector(".auth-user > .im .im-thread");
  const m = document.querySelector(".auth-user > .im .im-messages");
  const s2 = document.querySelector(".auth-user > .im .im-sidebar");
  return { threadH: t ? Math.round(t.getBoundingClientRect().height) : null,
           threadCSS: t ? getComputedStyle(t).height : null,
           msgsH: m ? Math.round(m.getBoundingClientRect().height) : null,
           sidebarH: s2 ? Math.round(s2.getBoundingClientRect().height) : null,
           winH: window.innerHeight };
})));
await p.evaluate(() => document.getElementById("settings-toggle")?.click());
await p.waitForTimeout(600);
await shot(p, "console-mobile-settings");
await overflow(p, "390px settings open");
for (const w of [360, 320]) {
  await p.setViewportSize({ width: w, height: 800 });
  await p.waitForTimeout(500);
  await overflow(p, `${w}px settings open`);
  await shot(p, `console-mobile-${w}`);
}
await b.close();
