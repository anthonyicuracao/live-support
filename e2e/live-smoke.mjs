import { chromium, webkit } from "/Users/ronpinkas1/git/insta-rebuild-hub/node_modules/playwright/index.mjs";
const URL = "https://connect.instantaiguru.com/?ref=instantaiguru.com";
for (const [name, type] of [["chromium", chromium], ["webkit", webkit]]) {
  const b = await type.launch({ headless: true });
  const p = await (await b.newContext()).newPage();
  await p.goto(URL, { waitUntil: "domcontentloaded" });
  await p.waitForTimeout(2500);
  const locked = await p.evaluate(() => window.Shared.playNotice());
  const fellBack = await p.evaluate(() => window.Shared.notify({ title: "t", body: "b" }));
  const titled = await p.evaluate(() => document.title);
  // any gesture at all — this is the sticky activation Ron described
  await p.mouse.click(10, 10);
  await p.waitForTimeout(600);
  const state = await p.evaluate(() => window.Shared.noticeState());
  const played = await p.evaluate(() => window.Shared.playNotice());
  const avail = await p.evaluate(async () => {
    const r = await fetch("/api/availability?ref=instantaiguru.com");
    return JSON.stringify(await r.json());
  }).catch((e) => "err " + e.message);
  console.log(`${name}:\n  locked: play=${locked} notify=${fellBack} title="${titled}"\n  after one click: state=${state} play=${played}\n  availability: ${avail}`);
  await b.close();
}
