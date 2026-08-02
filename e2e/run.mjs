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
import { mkdtempSync, rmSync } from "node:fs";
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

// The tenant DB must exist before a guest may use it — /login?ref= bootstraps it
// along with the admin account.
await fetch(`${BASE}/login?ref=e2e.local`).catch(() => {});

// Any spec can be driven by the same hermetic boot — screenshots want an
// identical server to the tests, not a hand-built one that drifts from it.
//   E2E_SPEC=shots.mjs node e2e/run.mjs
const spec = process.env.E2E_SPEC || "chat.spec.mjs";
const test = spawn(process.execPath, [join(here, spec)], {
  stdio: "inherit",
  env: { ...process.env, LS_BASE: BASE, LS_REF: "e2e.local", LS_ADMIN: "admin", LS_ADMIN_PW: ADMIN_PW },
});
test.on("exit", (code) => {
  if (code !== 0) {
    console.log("\n--- server log ---");
    console.log(serverLog.join("").split("\n").slice(-25).join("\n"));
  }
  cleanup();
  process.exit(code ?? 1);
});
