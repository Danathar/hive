// A STANDALONE spoke's terminal assertion must actually verify here.
//
// Run: node terminal_standalone_hive_id.test.js
//
// THE BUG THIS PINS (#7043 — the ttyd "Reconnected" loop).
//
// A terminal assertion is bound to one hive by its `h` claim, and
// verifyTerminalAssertion fails closed when the verifier cannot name its own
// hive (`!expectedHiveID` is the FIRST thing it rejects on). So the minter and
// the verifier have to agree on one string:
//
//   MINTER   — the Go dashboard, setTerminalAssertionCookie, mints with
//              cfg.HiveID. cmd/hive resolves that via loadOrGenerateHiveID:
//              $HIVE_ID if set, ELSE the id persisted at /data/hive-id, ELSE a
//              fresh `hive-<name>` it generates and persists there.
//   VERIFIER — this proxy, which read ONLY process.env.HIVE_ID.
//
// On a hub-provisioned spoke both read the same injected env var and agree. On a
// STANDALONE spoke (bin/hive-podman-setup.sh writes HIVE_DASHBOARD_TOKEN and
// nothing else) nobody injects it. The Go process generates the id at boot and
// publishes it with os.Setenv — into its OWN environment, which the sibling
// proxy process the same entrypoint started never sees. The proxy was therefore
// left with expectedHiveID === '' and rejected EVERY assertion the dashboard on
// the other side of the same container had just minted.
//
// That is not a cosmetic mismatch, because the dashboard's handoff `?code=` is
// SINGLE-USE and only authenticates the request that carries it. ttyd's client
// then issues two more, and only one of them carries the query string:
//
//   GET /terminal/?arg=…&code=X   -> code -> Go dashboard redeems it -> 200,
//                                    and sets hive_terminal_assertion
//   GET /terminal/token           -> NO code -> this gate -> 401
//   GET /terminal/ws?arg=…&code=X -> code -> Go dashboard, which verifies the
//                                    assertion against its OWN cfg.HiveID -> 101
//
// So the socket opens, ttyd immediately closes it (its client never got a token),
// the client reconnects, and the terminal flashes "Reconnected" forever.
//
// WHY THE EXISTING COVERAGE MISSED IT. Every other terminal test in this
// directory provisions HIVE_ID in the proxy's env, because that is what a hosted
// hive looks like. The standalone shape — assertion minted, HIVE_ID absent — had
// no test at all, so the one configuration where the two halves disagree was the
// one configuration nothing exercised. This file spawns the real server.js with
// HIVE_ID DELETED from the environment, which is the whole point: set it and
// every assertion below passes against the broken code too.

import { WebSocket, WebSocketServer } from 'ws';
import { createServer, request as httpRequest } from 'http';
import { spawn } from 'child_process';
import path from 'path';
import os from 'os';
import fs from 'fs';
import crypto from 'crypto';
import { fileURLToPath } from 'url';
import assert from 'assert';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PROXY_PORT = 19131;
const GO_PORT = 19132;
const TTYD_PORT = 19133;

// A standalone spoke is reached by IP/localhost, never at *.hive.kubestellar.io,
// so isHostedHost() is false and the gate runs on the DASHBOARD_TOKEN branch —
// the branch a podman/compose hive actually takes.
const SELF_HOSTED_HOST = '127.0.0.1';
const DASHBOARD_TOKEN = 'standalone-dashboard-token';

// The per-instance terminal key src/deploy/entrypoint.sh auto-provisions on a
// standalone spoke (#6489) and exports as HIVE_TERMINAL_KEY, so BOTH the Go
// minter and this proxy resolve lane 1 with an identical value. #6489 converged
// the KEY; the hive ID stayed divergent, which is what this file covers.
const TERMINAL_KEY = crypto.randomBytes(32).toString('hex');

// The id cmd/hive generated at first boot and persisted to /data/hive-id. It is
// deliberately NOT in the environment anywhere below.
const PERSISTED_HIVE_ID = 'hive-quiet-otter';
const OTHER_HIVE_ID = 'hive-somebody-else';

const TERMINAL_ASSERTION_VERSION = 'hive-terminal-v1';

// mintAssertion mirrors Go terminalassert.Mint EXACTLY: raw-base64url JSON
// {v,u,r,h,iat,exp}, then '.' then base64url HMAC-SHA256 over that body.
function mintAssertion(key, username, role, hiveID, skewSec = 0) {
  const iat = Math.floor(Date.now() / 1000) + skewSec;
  const body = Buffer.from(JSON.stringify({
    v: TERMINAL_ASSERTION_VERSION,
    u: username,
    r: role,
    h: hiveID,
    iat,
    exp: iat + 15 * 60,
  })).toString('base64url');
  const sig = crypto.createHmac('sha256', key).update(body).digest('base64url');
  return `${body}.${sig}`;
}

function mockBackend(port, body) {
  return new Promise(resolve => {
    const s = createServer((req, res) => { res.writeHead(200); res.end(body); });
    s.listen(port, () => resolve(s));
  });
}

// The ttyd mock speaks WebSocket as well as HTTP: /terminal/token is a plain GET
// but the shell itself is an upgrade, and both gates must be exercised.
function mockTtydWS(port) {
  return new Promise(resolve => {
    const s = createServer((req, res) => { res.writeHead(200); res.end('ttyd'); });
    const wss = new WebSocketServer({ server: s });
    wss.on('connection', ws => ws.send('shell'));
    s.listen(port, () => resolve(s));
  });
}

// startProxy spawns the real server.js. `hiveIdFile` is the path the Go binary
// would have persisted its id to; `envHiveID` is the hub-injected env var, and
// passing '' means genuinely ABSENT (deleted), not empty-string-present.
function startProxy({ hiveIdFile, envHiveID }) {
  return new Promise((resolve, reject) => {
    const env = {
      ...process.env,
      HIVE_PROXY_PORT: String(PROXY_PORT),
      HIVE_API_PORT: String(GO_PORT),
      HIVE_TTYD_PORT: String(TTYD_PORT),
      HIVE_DASHBOARD_TOKEN: DASHBOARD_TOKEN,
      HIVE_STATIC_DIR: __dirname,
      HIVE_TERMINAL_KEY: TERMINAL_KEY,
      HIVE_ID_FILE: hiveIdFile,
      NODE_ENV: 'test',
    };
    // A standalone spoke has NO hub master and NO hub-injected identity. Deleted
    // rather than set to '' so this matches the real process environment — and
    // so an inherited HIVE_ID from the developer's own shell cannot make this
    // test pass against the broken code.
    delete env.HIVE_ID;
    delete env.HIVE_HUB_SECRET;
    delete env.HIVE_AUTHORIZED_USERS;
    if (envHiveID) env.HIVE_ID = envHiveID;

    const proc = spawn('node', ['server.js'], {
      cwd: __dirname, env, stdio: ['ignore', 'pipe', 'pipe'],
    });
    let started = false;
    proc.stdout.on('data', d => {
      if (!started && d.toString().includes('hive-proxy')) { started = true; resolve(proc); }
    });
    proc.on('error', reject);
    setTimeout(() => { if (!started) reject(new Error('proxy start timeout')); }, 10000);
  });
}

// NOTE: http.request, not fetch — fetch() silently drops a custom Host header,
// which would send the proxy down a different branch than the one under test.
function terminalHTTP(pathname, assertion) {
  return new Promise((resolve, reject) => {
    const headers = { Host: SELF_HOSTED_HOST };
    if (assertion !== null) headers.Cookie = `hive_terminal_assertion=${assertion}`;
    const req = httpRequest(
      { host: '127.0.0.1', port: PROXY_PORT, path: pathname, method: 'GET', headers },
      res => { res.resume(); resolve(res.statusCode); },
    );
    req.on('error', reject);
    req.end();
  });
}

function terminalWS(assertion) {
  return new Promise(resolve => {
    const headers = { Host: SELF_HOSTED_HOST };
    if (assertion !== null) headers.Cookie = `hive_terminal_assertion=${assertion}`;
    const ws = new WebSocket(`ws://127.0.0.1:${PROXY_PORT}/terminal/ws?arg=hive-scanner`, { headers });
    const done = opened => { try { ws.close(); } catch {} resolve(opened); };
    ws.on('open', () => done(true));
    ws.on('error', () => done(false));
    ws.on('close', () => done(false));
    setTimeout(() => done(false), 4000);
  });
}

const work = fs.mkdtempSync(path.join(os.tmpdir(), 'hive-standalone-hive-id-'));
const HIVE_ID_FILE = path.join(work, 'hive-id');
const MISSING_HIVE_ID_FILE = path.join(work, 'no-such-hive-id');
// Trailing newline on purpose: cmd/hive writes the id as `id + "\n"`, and a
// resolver that forgets to trim produces a hive id that matches nothing.
fs.writeFileSync(HIVE_ID_FILE, `${PERSISTED_HIVE_ID}\n`);

let proxy, go, ttyd;
try {
  go = await mockBackend(GO_PORT, 'go');
  ttyd = await mockTtydWS(TTYD_PORT);

  console.log('=== standalone spoke: HIVE_ID absent, id persisted at /data/hive-id ===');
  proxy = await startProxy({ hiveIdFile: HIVE_ID_FILE, envHiveID: '' });
  await new Promise(r => setTimeout(r, 400));

  // --- THE REGRESSION -------------------------------------------------------
  // The exact request ttyd's client makes right after the handoff code is burnt.
  // It carries no ?code=, so the assertion cookie is the only credential left —
  // and this 401 is the head of the reconnect loop.
  const good = mintAssertion(TERMINAL_KEY, 'danathar', 'owner', PERSISTED_HIVE_ID);
  assert.equal(await terminalHTTP('/terminal/token', good), 200,
    'GET /terminal/token with a valid assertion for THIS hive must reach ttyd. A 401 here is the ' +
    'ttyd reconnect loop: the client never gets its auth token, so every socket it opens is ' +
    'immediately closed and reopened, forever.');
  console.log('  ✓ /terminal/token accepts an assertion minted against the persisted hive id');

  assert.equal(await terminalHTTP('/terminal/', good), 200,
    'the terminal document must also be reachable with the assertion alone (the handoff code is single-use)');
  console.log('  ✓ /terminal/ accepts it too — the code is single-use, the cookie carries the session');

  assert.ok(await terminalWS(good),
    'the WS upgrade gate must accept the same assertion — it is the same credential and the same ttyd');
  console.log('  ✓ the WebSocket gate accepts it');

  // --- FAIL-CLOSED: reading the file must not weaken the hive binding --------
  // An assertion minted for a DIFFERENT hive is exactly the cross-tenant forgery
  // the `h` claim exists to stop. Resolving the id from a file must not turn the
  // check into a no-op.
  const wrongHive = mintAssertion(TERMINAL_KEY, 'danathar', 'owner', OTHER_HIVE_ID);
  assert.equal(await terminalHTTP('/terminal/token', wrongHive), 401,
    'an assertion minted for another hive must still be rejected — the h claim is the tenant boundary');
  assert.ok(!(await terminalWS(wrongHive)),
    'and rejected on the WS gate too');
  console.log('  ✓ an assertion for another hive is still refused (HTTP + WS)');

  const badSig = `${good.slice(0, good.indexOf('.'))}.${crypto.randomBytes(32).toString('base64url')}`;
  assert.equal(await terminalHTTP('/terminal/token', badSig), 401,
    'a tampered signature must still be rejected');
  assert.equal(await terminalHTTP('/terminal/token', null), 401,
    'no credential at all must still be rejected');
  console.log('  ✓ tampered and absent assertions still refused');

  // A read-only grant authenticates but must not open a shell (N4).
  const readOnly = mintAssertion(TERMINAL_KEY, 'danathar', 'read', PERSISTED_HIVE_ID);
  assert.equal(await terminalHTTP('/terminal/token', readOnly), 403,
    'a valid read-only assertion must be refused a shell, not upgraded by the allowlist fallback');
  console.log('  ✓ a read-only assertion is still refused a shell');

  // An expired assertion is not resurrected by the new lane.
  const expired = mintAssertion(TERMINAL_KEY, 'danathar', 'owner', PERSISTED_HIVE_ID, -3600);
  assert.equal(await terminalHTTP('/terminal/token', expired), 401,
    'an expired assertion must still be rejected');
  console.log('  ✓ an expired assertion is still refused');

  proxy.kill();
  proxy = null;

  // --- The hub-provisioned shape is byte-for-byte unchanged -----------------
  // env HIVE_ID must WIN over the file, so a hosted spoke cannot be steered onto
  // a stale or attacker-planted id sitting in the data directory.
  console.log('=== hub-provisioned spoke: env HIVE_ID wins over the file ===');
  proxy = await startProxy({ hiveIdFile: HIVE_ID_FILE, envHiveID: OTHER_HIVE_ID });
  await new Promise(r => setTimeout(r, 400));

  const envMatched = mintAssertion(TERMINAL_KEY, 'danathar', 'owner', OTHER_HIVE_ID);
  assert.equal(await terminalHTTP('/terminal/token', envMatched), 200,
    'an assertion for the env-injected hive id must be accepted');
  assert.equal(await terminalHTTP('/terminal/token', good), 401,
    'an assertion for the id in the FILE must be rejected when the env names a different hive — ' +
    'the file is a fallback, never an override');
  console.log('  ✓ env HIVE_ID takes precedence; the file is ignored entirely');

  proxy.kill();
  proxy = null;

  // --- Neither source available: unchanged fail-closed behaviour ------------
  console.log('=== neither env nor file: still fails closed ===');
  proxy = await startProxy({ hiveIdFile: MISSING_HIVE_ID_FILE, envHiveID: '' });
  await new Promise(r => setTimeout(r, 400));

  assert.equal(await terminalHTTP('/terminal/token', good), 401,
    'with no hive id from either source the gate must deny, exactly as before');
  assert.ok(!(await terminalWS(good)),
    'and the WS gate must deny too');
  console.log('  ✓ an unresolvable hive id denies, as it always did');

  console.log('\n=== terminal_standalone_hive_id: all assertions passed ===');
} finally {
  if (proxy) proxy.kill();
  if (go) go.close();
  if (ttyd) ttyd.close();
  fs.rmSync(work, { recursive: true, force: true });
}
