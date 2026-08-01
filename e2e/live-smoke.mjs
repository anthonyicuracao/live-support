// Drives the DEPLOYED appliance, not a hermetic one. "Tagged" and "live" are
// different facts; this asserts the second before anyone is asked to test.
import { chromium, webkit } from "/Users/ronpinkas1/git/insta-rebuild-hub/node_modules/playwright/index.mjs";
const URL = "https://connect.instantaiguru.com/?ref=instantaiguru.com";
let bad = 0;
const check = (n, ok, d) => { console.log(`  ${ok ? "PASS" : "FAIL"}  ${n}${ok ? "" : `\n        ${d}`}`); if (!ok) bad++; };
for (const [name, type] of [["chromium", chromium], ["webkit", webkit]]) {
  const b = await type.launch({ headless: true });
  const p = await (await b.newContext()).newPage();
  const errs = [];
  p.on("pageerror", (e) => errs.push(String(e)));
  await p.goto(URL, { waitUntil: "domcontentloaded" });
  await p.waitForTimeout(2500);
  console.log(name + ":");
  const ver = await p.evaluate(() => document.querySelector('script[src*="shared.js"]')?.src.split("v=")[1]);
  console.log(`  serving ${ver}`);
  check("nothing plays before a gesture", (await p.evaluate(() => window.Shared.playNotice())) === false, "it played");
  check("the alert still lands", ["badge", "notification"].includes(await p.evaluate(() => window.Shared.notify({ title: "t", body: "b" }))), "it vanished");
  await p.mouse.click(10, 10);
  await p.waitForTimeout(600);
  check("one gesture unlocks audio", (await p.evaluate(() => window.Shared.noticeState())) === "running", await p.evaluate(() => window.Shared.noticeState()));
  check("and the blip then sounds", (await p.evaluate(() => window.Shared.playNotice())) === true, "still silent");
  const bell = await p.evaluate(() => !!document.querySelector("[data-mute-toggle]"));
  check("the mute control is present", bell, "no [data-mute-toggle] on the guest dock");
  await p.evaluate(() => window.Shared.setNoticeMuted(true));
  check("muting silences it", (await p.evaluate(() => window.Shared.playNotice())) === false, "played while muted");
  await p.evaluate(() => window.Shared.setNoticeMuted(false));
  check("no page errors", errs.length === 0, errs[0]);
  await b.close();
}
console.log(bad ? `\n${bad} FAILED against the live appliance` : "\nlive appliance OK");
process.exit(bad ? 1 : 0);
