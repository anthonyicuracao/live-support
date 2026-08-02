// Hermetic end-to-end harness.
//
// Boots a throwaway live-support on its own port with its own temp data dir and
// its own bootstrap admin, runs the browser tests against it, tears it down.
//
// Hermetic on purpose: the tests must not depend on whatever state a developer
// happens to have in their local appliance, and must never disturb it. A test
// that only passes against one machine's database is not a test.
//
//   node e2e/run.mjs
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL(".", import.meta.url));
const repo = join(here, "..");
const PORT = process.env.E2E_PORT || "8099";
const BASE = `http://127.0.0.1:${PORT}`;
const ADMIN_PW = "e2e-initial-pw";

const dataDir = mkdtempSync(join(tmpdir(), "ls-e2e-"));
let server;

function cleanup() {
  if (server && !server.killed) server.kill("SIGTERM");
  try { rmSync(dataDir, { recursive: true, force: true }); } catch { /* best effort */ }
}
process.on("exit", cleanup);
process.on("SIGINT", () => { cleanup(); process.exit(130); });

console.log("building live-support…");
const build = spawnSync("go", ["build", "-o", join(dataDir, "live-support"), "."], {
  cwd: repo, stdio: "inherit",
});
if (build.status !== 0) { console.error("build failed"); process.exit(1); }

console.log(`starting on ${BASE} (data: ${dataDir})`);
server = spawn(join(dataDir, "live-support"), [], {
  cwd: repo,
  env: {
    ...process.env,
    DATA_DIR: dataDir,
    PORT,
    BIND_ADDR: "127.0.0.1",
    SECURE_COOKIES: "false",
    CONNECT_SECRET: "e2e-connect-secret",
    ADMIN_USERNAME: "admin",
    ADMIN_INITIAL_PASSWORD: ADMIN_PW,
    // Keep the run deterministic: no push, and a short inactivity window is
    // irrelevant here but pinned so a default change cannot alter results.
    VAPID_PUBLIC_KEY: "",
    VAPID_PRIVATE_KEY: "",
    CHAT_INACTIVE_MINUTES: "10",
    MAX_CONCURRENT_CHATS: "3",
  },
  stdio: ["ignore", "pipe", "pipe"],
});
const serverLog = [];
server.stdout.on("data", (d) => serverLog.push(String(d)));
server.stderr.on("data", (d) => serverLog.push(String(d)));

async function waitHealthy(deadlineMs = 20000) {
  const start = Date.now();
  while (Date.now() - start < deadlineMs) {
    try {
      const r = await fetch(`${BASE}/healthz`);
      if (r.ok) return true;
    } catch { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 250));
  }
  return false;
}

if (!(await waitHealthy())) {
  console.error("server never became healthy:\n" + serverLog.join(""));
  process.exit(1);
}

// Provision the tenant exactly the way a self-hosted operator does, because
// nothing else can create one: sign in with the shared secret as the password,
// then redeem the admin invite that comes back. Exercising it here is also the
// only coverage the path gets.
const REF = "e2e.local";
const csrfOf = (html) => (/name="csrf"[^>]*value="([^"]*)"/.exec(html) || [])[1] || "";
// ALL of them. `headers.get("set-cookie")` joins multiple cookies with ", ",
// so splitting on ";" keeps only the first - and when the CSRF cookie was not
// first, every POST failed the CSRF check and re-rendered, which reads exactly
// like a wrong password.
const cookiesOf = (res) =>
  (res.headers.getSetCookie?.() || [res.headers.get("set-cookie") || ""])
    .map((c) => c.split(";")[0]).filter(Boolean).join("; ");
{
  const form = await fetch(`${BASE}/login?ref=${REF}`);
  const cookie = cookiesOf(form);
  const res = await fetch(`${BASE}/login?ref=${REF}`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", cookie },
    body: new URLSearchParams({
      ref: REF, username: "admin", password: "e2e-connect-secret",
      csrf: csrfOf(await form.text()),
    }),
  });
  const html = await res.text();
  // Unescaped: the link is read out of rendered HTML, where `&t=` arrives as
  // `&amp;t=`. Parsed as-is the token parameter is named "amp;t", so the
  // invite is presented without a token and comes back Invalid - which renders
  // a page with no password field, so a naive "did the form re-render" check
  // calls it a success.
  const link = ((/\/invite\?ref=[^"<\s]*/.exec(html) || [])[0] || "").replace(/&amp;/g, "&");
  if (!link) {
    console.error("provisioning did not return an admin invite:\n" + html.slice(0, 400));
    process.exit(1);
  }
  // Redeem it: the operator chooses the username and password themselves, so
  // the tenant never holds a credential anyone else could already know.
  const invForm = await fetch(`${BASE}${link}`);
  const invCookie = cookiesOf(invForm);
  const token = new URL(BASE + link).searchParams.get("t");
  const redeem = await fetch(`${BASE}/invite`, {
    method: "POST", redirect: "follow",
    headers: { "Content-Type": "application/x-www-form-urlencoded", cookie: invCookie },
    body: new URLSearchParams({
      ref: REF, t: token, username: "admin",
      new: ADMIN_PW, confirm: ADMIN_PW, csrf: csrfOf(await invForm.text()),
    }),
  });
  const redeemBody = await redeem.text();

  // A re-rendered form means it did NOT redeem: the handler answers 200 either
  // way, so status alone cannot tell success from a rejected invite.
  if (!redeem.ok || /name="new"/.test(redeemBody)) {
    const why = (/class="notice err">([^<]*)/.exec(redeemBody) || [])[1] || redeemBody.slice(0, 300);
    console.error(`invite redemption failed (${redeem.status}): ${why}`);
    process.exit(1);
  }
  // Prove the credential the browser tests are about to use actually works.
  // Redemption reporting success is not the same as an account that can sign
  // in, and a failure here is far easier to read than a missing selector.
  const check = await fetch(`${BASE}/login?ref=${REF}`);
  const checkCookie = cookiesOf(check);
  // Manual, and asserted on the redirect itself. Following it looks like a
  // failed login: Node's fetch has no cookie jar, so the session set by the
  // 303 is not sent to /users, which bounces straight back to /login with no
  // error message at all. In the browser spec the opposite holds - there,
  // manual yields an opaque status 0 - so the two suites must read the outcome
  // differently.
  const signIn = await fetch(`${BASE}/login?ref=${REF}`, {
    method: "POST", redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded", cookie: checkCookie },
    body: new URLSearchParams({
      ref: REF, username: "admin", password: ADMIN_PW, csrf: csrfOf(await check.text()),
    }),
  });
  const landed = signIn.headers.get("location") || "(no redirect)";
  if (signIn.status !== 303) {
    console.error(`the provisioned admin cannot sign in (${signIn.status}): ` +
      ((/class="notice err">([^<]*)/.exec(await signIn.text()) || [])[1] || "").trim());
    process.exit(1);
  }
  console.log(`provisioned ${REF} via secret + invite; first admin signs in to ${landed}`);
}

const spec = process.env.E2E_SPEC || "chat.spec.mjs";
const test = spawn(process.execPath, [join(here, spec)], {
  stdio: "inherit",
  env: { ...process.env, LS_BASE: BASE, LS_REF: "e2e.local", LS_ADMIN: "admin", LS_ADMIN_PW: ADMIN_PW },
});
test.on("exit", (code) => {
  // The spec asks the server for a tenant nobody provisioned; the evidence is
  // here, not in any response. A stray file means an anonymous GET can still
  // mint a tenant — and with ADMIN_INITIAL_PASSWORD set, seed a usable admin
  // into it.
  const files = readdirSync(dataDir);
  const strays = files.filter((f) => f.startsWith("never-provisioned-probe"));
  if (strays.length) {
    console.log(`\nFAIL  an unknown ref became a tenant: ${strays.join(", ")}`);
    code = 1;
  } else {
    console.log("PASS  an unknown ref did not become a tenant");
  }
  // The spec provisioned "MixedCase.Probe". It must have landed in the
  // normalised file, or provisioning would itself recreate the case split.
  const mixed = files.filter((f) => f.toLowerCase().startsWith("mixedcase.probe"));
  const badCase = mixed.filter((f) => f !== f.toLowerCase());
  if (!mixed.length) {
    console.log("FAIL  provisioning a mixed-case ref created nothing");
    code = 1;
  } else if (badCase.length) {
    console.log(`FAIL  provisioning kept the case: ${badCase.join(", ")}`);
    code = 1;
  } else {
    console.log("PASS  a mixed-case ref provisioned into the normalised database");
  }
  if (code !== 0) {
    console.log("\n--- server log ---");
    console.log(serverLog.join("").split("\n").slice(-25).join("\n"));
  }
  cleanup();
  process.exit(code ?? 1);
});
