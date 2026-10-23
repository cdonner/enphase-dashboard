// Local backend for the Enphase dashboard.
// - Creates / stores / renews the gateway JWT (Enlighten login -> Entrez token)
// - Polls the IQ Gateway meters and records today's power curve + daily energy totals
// - Backfills from the Enphase cloud API what the gateway can't provide (cloud.js)
// - Serves the built React app from dist/
import express from 'express';
import https from 'node:https';
import fs from 'node:fs/promises';
import { existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createCloud } from './cloud.js';

const ROOT = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const DATA = path.join(ROOT, 'data');
const ENVOY = process.env.ENVOY_HOST || 'envoy.local';
const PORT = Number(process.env.PORT || 3001);
const HOST = process.env.HOST || '127.0.0.1';
const POLL_MS = Number(process.env.POLL_MS || 5_000);
const HISTORY_STEP_MS = 60_000; // one stored chart point per minute
const RENEW_BEFORE_MS = 60 * 60_000; // renew when the token has < 1 h left
const PARTIAL_AFTER_MS = 10 * 60_000; // a day's baseline taken after 00:10 is a partial day
const PDM_EVERY_MS = 5 * 60_000; // inverters only report every ~5 minutes

const ENLIGHTEN_LOGIN = 'https://enlighten.enphaseenergy.com/login/login.json';
const ENTREZ_TOKENS = 'https://entrez.enphaseenergy.com/tokens';

// The gateway uses a self-signed certificate.
const gatewayAgent = new https.Agent({ rejectUnauthorized: false, keepAlive: true });

const state = {
  token: null,
  credentials: null, // { email, password } only if the user opted in to auto-renewal
  gateway: { serial: null, firmware: null, reachable: false },
  meters: null, // eid -> measurementType
  pdm: null, // inverter energy totals from /ivp/pdm/energy
  live: null,
  lastError: null,
  renewError: null,
  today: { date: null, points: [] },
  daily: {},
  cloudDaily: {}, // date -> full-day totals from the Enphase cloud
  cloudMeta: {}, // { dailySyncedFor, todayAttempted }
  ev: { at: 0, chargers: [] }, // EV chargers + recent sessions from the cloud
  inverters: { at: 0, data: null },
};

// ---------- helpers ----------

const dayKey = (d = new Date()) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

const startOfDay = (t) => {
  const d = new Date(t);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
};

async function readJson(file, fallback) {
  try {
    return JSON.parse(await fs.readFile(path.join(DATA, file), 'utf8'));
  } catch {
    return fallback;
  }
}

async function writeJson(file, value, mode = 0o644) {
  const target = path.join(DATA, file);
  await fs.mkdir(path.dirname(target), { recursive: true });
  const tmp = `${target}.tmp`;
  await fs.writeFile(tmp, JSON.stringify(value), { mode });
  await fs.rename(tmp, target);
}

function decodeJwt(token) {
  try {
    const payload = token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/');
    return JSON.parse(Buffer.from(payload, 'base64').toString('utf8'));
  } catch {
    return null;
  }
}

function tokenInfo(token) {
  if (!token) return { present: false };
  const claims = decodeJwt(token) || {};
  const exp = claims.exp ? claims.exp * 1000 : null;
  return {
    present: true,
    expiresAt: exp,
    expired: exp ? exp < Date.now() : false,
    role: claims.enphaseUser ?? null,
    serial: claims.aud ?? null,
  };
}

function gatewayGet(p, { token = state.token, timeout = 25_000 } = {}) {
  return new Promise((resolve, reject) => {
    const headers = { Accept: 'application/json' };
    if (token) headers.Authorization = `Bearer ${token}`;
    const req = https.get({ host: ENVOY, path: p, agent: gatewayAgent, headers, timeout }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (c) => (body += c));
      res.on('end', () => {
        if (res.statusCode === 401) {
          const err = new Error('Gateway rejected the token (401)');
          err.status = 401;
          return reject(err);
        }
        if (res.statusCode >= 400) return reject(new Error(`Gateway returned HTTP ${res.statusCode}`));
        resolve(body);
      });
    });
    req.on('timeout', () => req.destroy(new Error('Gateway request timed out')));
    req.on('error', reject);
  });
}

const gatewayJson = async (p, opts) => JSON.parse(await gatewayGet(p, opts));

async function loadGatewayInfo() {
  try {
    const xml = await gatewayGet('/info', { token: null, timeout: 10_000 });
    state.gateway = {
      serial: xml.match(/<sn>([^<]+)<\/sn>/)?.[1] ?? null,
      firmware: xml.match(/<software>([^<]+)<\/software>/)?.[1] ?? null,
      reachable: true,
    };
  } catch (e) {
    state.gateway.reachable = false;
    state.lastError = `Gateway unreachable at ${ENVOY}: ${e.message}`;
  }
}

// ---------- token handling ----------

async function verifyAndSaveToken(token) {
  token = String(token || '').trim();
  if (token.split('.').length !== 3) throw new Error('That does not look like a JWT');
  await gatewayGet('/api/v1/production', { token }); // throws on 401
  state.token = token;
  state.meters = null;
  await writeJson('token.json', { token, savedAt: Date.now() }, 0o600);
  state.lastError = null;
  sample();
}

async function requestToken(email, password, serial = state.gateway.serial) {
  if (!serial) throw new Error('Gateway serial number unknown - is the gateway reachable?');
  const login = await fetch(ENLIGHTEN_LOGIN, {
    method: 'POST',
    body: new URLSearchParams({ 'user[email]': email, 'user[password]': password }),
  });
  const loginBody = await login.json().catch(() => ({}));
  if (!login.ok || !loginBody.session_id) {
    const err = new Error(loginBody.message || `Enlighten login failed (HTTP ${login.status})`);
    err.status = 401;
    throw err;
  }
  const res = await fetch(ENTREZ_TOKENS, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session_id: loginBody.session_id, serial_num: serial, username: email }),
  });
  const token = (await res.text()).trim();
  if (!res.ok) throw new Error(`Entrez refused the token request (HTTP ${res.status}): ${token.slice(0, 200)}`);
  return token;
}

async function renewIfNeeded() {
  if (!state.credentials) return;
  const info = tokenInfo(state.token);
  if (info.present && info.expiresAt && info.expiresAt - Date.now() > RENEW_BEFORE_MS) return;
  try {
    await verifyAndSaveToken(await requestToken(state.credentials.email, state.credentials.password));
    state.renewError = null;
    console.log('Gateway token renewed');
  } catch (e) {
    state.renewError = e.message;
    console.error('Token renewal failed:', e.message);
  }
}

// ---------- sampling ----------

async function loadMeters() {
  const meters = await gatewayJson('/ivp/meters');
  state.meters = Object.fromEntries(meters.filter((m) => m.state === 'enabled').map((m) => [m.eid, m.measurementType]));
}

function summarize(readings) {
  const by = {};
  for (const r of readings) if (state.meters[r.eid]) by[state.meters[r.eid]] = r;
  const prod = by.production;
  const net = by['net-consumption'];
  if (!prod) throw new Error('No production meter reading');

  const productionW = Math.max(0, prod.activePower);
  const netW = net ? net.activePower : null; // + importing from grid, - exporting
  return {
    at: Date.now(),
    productionW,
    netW,
    consumptionW: net ? Math.max(0, productionW + netW) : null,
    // Lifetime counters in Wh. Note /production.json whToday is broken on this firmware
    // (it equals whLifetime), so daily totals are counter deltas from local midnight.
    counters: {
      produced: prod.actEnergyDlvd,
      imported: net?.actEnergyDlvd ?? null,
      exported: net?.actEnergyRcvd ?? null,
    },
  };
}

async function rollDay(date) {
  if (state.today.date === date) return;
  state.today = { date, points: await readJson(`history/${date}.json`, []) };
}

function updateDaily(s) {
  const date = dayKey(new Date(s.at));
  const d = (state.daily[date] ??= {
    baseline: s.counters,
    baselineAt: s.at,
    partial: s.at - startOfDay(s.at) > PARTIAL_AFTER_MS,
    peakW: 0,
    peakAt: null,
  });
  const delta = (k) => (s.counters[k] == null || d.baseline[k] == null ? null : Math.max(0, s.counters[k] - d.baseline[k]));
  // d.before: energy between midnight and baselineAt, backfilled from the cloud.
  const withBefore = (k) => {
    const v = delta(k);
    return v == null ? null : v + (d.before?.[k] ?? 0);
  };
  d.produced = withBefore('produced');
  d.imported = withBefore('imported');
  d.exported = withBefore('exported');
  d.consumed = d.imported == null ? null : d.produced + d.imported - d.exported;
  // When we missed midnight, the meter deltas only cover part of the day. The inverters'
  // own wattHoursToday (the eim/meter "today" fields are always 0 on this firmware) still
  // gives the full day's production.
  const pcu = state.pdm?.production?.pcu;
  if (d.partial && !d.before && pcu && dayKey(new Date(state.pdm.at)) === date && pcu.wattHoursToday > d.produced) {
    d.produced = pcu.wattHoursToday;
    d.producedFromInverters = true;
  } else {
    d.producedFromInverters = false;
  }
  if (s.productionW > d.peakW) {
    d.peakW = s.productionW;
    d.peakAt = s.at;
  }
}

let lastPersist = 0;

async function record(s) {
  const date = dayKey(new Date(s.at));
  await rollDay(date);
  updateDaily(s);

  const pts = state.today.points;
  const last = pts[pts.length - 1];
  if (!last || s.at - last[0] >= HISTORY_STEP_MS) {
    const r = (v) => (v == null ? null : Math.round(v));
    pts.push([s.at, r(s.productionW), r(s.consumptionW), r(s.netW)]);
  }

  if (s.at - lastPersist >= HISTORY_STEP_MS) {
    lastPersist = s.at;
    await writeJson(`history/${date}.json`, pts);
    await writeJson('daily.json', state.daily);
  }
}

let sampling = false;

async function sample() {
  if (!state.token || sampling) return;
  sampling = true;
  try {
    if (!state.meters) await loadMeters();
    if (!state.pdm || Date.now() - state.pdm.at > PDM_EVERY_MS) {
      state.pdm = { at: Date.now(), ...(await gatewayJson('/ivp/pdm/energy')) };
    }
    const s = summarize(await gatewayJson('/ivp/meters/readings', { timeout: 10_000 }));
    await record(s);
    state.live = s;
    state.lastError = null;
  } catch (e) {
    state.lastError = e.message;
    if (e.status === 401) renewIfNeeded();
  } finally {
    sampling = false;
  }
}

// ---------- cloud backfill ----------

const cloud = createCloud({ readJson, writeJson });
const saveCloudDaily = () => writeJson('cloud-daily.json', { meta: state.cloudMeta, days: state.cloudDaily });

// Full-day totals for the last 30 days: 4 API calls, once a day after 03:00
// (so yesterday is final), or right away the first time.
async function syncCloudDaily() {
  const today = dayKey();
  if (state.cloudMeta.dailySyncedFor === today) return;
  if (state.cloudMeta.dailySyncedFor && new Date().getHours() < 3) return;
  const start = new Date();
  start.setDate(start.getDate() - 30);
  Object.assign(state.cloudDaily, await cloud.dailyTotals(dayKey(start)));
  state.cloudMeta.dailySyncedFor = today;
  await saveCloudDaily();
}

// If the dashboard started after midnight, fill in today's energy and chart
// before that from 15-minute cloud intervals: 4 API calls, at most once a day.
async function backfillToday() {
  const date = dayKey();
  const d = state.daily[date];
  if (!d?.partial || d.before || state.cloudMeta.todayAttempted === date) return;
  state.cloudMeta.todayAttempted = date;
  await saveCloudDaily();

  const rows = await cloud.intervals(Math.floor(startOfDay(Date.now()) / 1000));
  const cut = d.baselineAt / 1000;
  if (!rows.some((r) => r.end >= cut)) throw new Error('Cloud data does not reach the dashboard start yet');
  const before = { produced: 0, imported: 0, exported: 0 };
  const points = [];
  const w = (wh) => (wh == null ? null : wh * 4); // Wh per 15 min -> average W
  for (const r of rows) {
    const frac = Math.max(0, Math.min(1, (cut - (r.end - 900)) / 900)); // share of interval before baseline
    if (!frac) continue;
    for (const k of Object.keys(before)) before[k] += (r[k] ?? 0) * frac;
    if (frac === 1) {
      const net = r.imported != null && r.exported != null ? w(r.imported - r.exported) : null;
      points.push([(r.end - 450) * 1000, w(r.produced), w(r.consumed), net]);
    }
  }
  d.before = before;
  await writeJson('daily.json', state.daily);
  if (state.today.date === date) {
    const first = state.today.points[0]?.[0] ?? Infinity;
    state.today.points = [...points.filter((p) => p[0] < first), ...state.today.points];
    await writeJson(`history/${date}.json`, state.today.points);
  }
}

// EV charger sessions: 1 call per charger every 3 hours (+1 to list chargers once a day).
const EV_EVERY_MS = 3 * 3_600_000;

async function syncEv() {
  if (Date.now() - state.ev.at < EV_EVERY_MS) return;
  const known = Date.now() - (state.ev.listedAt ?? 0) < 86_400_000 ? state.ev.chargers : await cloud.evChargers();
  const chargers = [];
  for (const c of known) chargers.push({ ...c, sessions: await cloud.evSessions(c.serial) });
  state.ev = { at: Date.now(), listedAt: known === state.ev.chargers ? state.ev.listedAt : Date.now(), chargers };
  await writeJson('ev.json', state.ev);
}

async function cloudTick() {
  if (!cloud.connected()) return;
  try {
    await syncCloudDaily();
    await backfillToday();
    await syncEv();
    cloud.status.error = null;
  } catch (e) {
    cloud.status.error = e.message;
    console.error('Cloud sync:', e.message);
  }
}

// ---------- API ----------

const app = express();

// Optional HTTP basic auth for the whole dashboard (any username).
if (process.env.DASHBOARD_PASSWORD) {
  app.use((req, res, next) => {
    const [, b64 = ''] = (req.headers.authorization || '').split(' ');
    const pass = Buffer.from(b64, 'base64').toString().split(':').slice(1).join(':');
    if (pass === process.env.DASHBOARD_PASSWORD) return next();
    res.set('WWW-Authenticate', 'Basic realm="Solar dashboard"').status(401).send('Authentication required');
  });
}

app.use(express.json());

app.get('/api/status', (_req, res) => {
  res.json({
    gatewayHost: ENVOY,
    gateway: state.gateway,
    token: tokenInfo(state.token),
    autoRenew: !!state.credentials,
    renewError: state.renewError,
    lastSampleAt: state.live?.at ?? null,
    lastError: state.lastError,
    pollMs: POLL_MS,
    cloud: {
      configured: cloud.configured(),
      connected: cloud.connected(),
      lastSyncAt: cloud.status.lastSyncAt,
      error: cloud.status.error,
      authorizeUrl: cloud.authorizeUrl() || null,
    },
  });
});

// Paste the code shown after approving access at cloud.authorizeUrl.
app.post('/api/cloud/connect', async (req, res) => {
  try {
    await cloud.connect(req.body?.code);
    state.cloudMeta = {};
    cloudTick();
    res.json({ ok: true });
  } catch (e) {
    res.status(400).json({ error: e.message });
  }
});

// Sign in to Enlighten and request a token for this gateway. Credentials are
// only kept (data/credentials.json, mode 600) when `remember` is set.
app.post('/api/token/create', async (req, res) => {
  const { email, password, remember } = req.body || {};
  if (!email || !password) return res.status(400).json({ error: 'Email and password are required' });
  try {
    await verifyAndSaveToken(await requestToken(email, password));
    if (remember) {
      state.credentials = { email, password };
      await writeJson('credentials.json', state.credentials, 0o600);
    }
    res.json({ ok: true, token: tokenInfo(state.token) });
  } catch (e) {
    res.status(e.status === 401 ? 401 : 502).json({ error: e.message });
  }
});

// Save a token generated elsewhere (Entrez web UI, entrez-auth-token URL).
app.post('/api/token', async (req, res) => {
  try {
    await verifyAndSaveToken(req.body?.token);
    res.json({ ok: true, token: tokenInfo(state.token) });
  } catch (e) {
    res.status(e.status === 401 ? 401 : 400).json({ error: e.message });
  }
});

app.delete('/api/token', async (_req, res) => {
  state.token = null;
  state.credentials = null;
  state.live = null;
  await fs.rm(path.join(DATA, 'token.json'), { force: true });
  await fs.rm(path.join(DATA, 'credentials.json'), { force: true });
  res.json({ ok: true });
});

app.delete('/api/credentials', async (_req, res) => {
  state.credentials = null;
  await fs.rm(path.join(DATA, 'credentials.json'), { force: true });
  res.json({ ok: true });
});

const requireToken = (_req, res, next) =>
  state.token ? next() : res.status(401).json({ error: 'No gateway token' });

const round = (v) => (v == null ? null : Math.round(v));

// Local measurements win when they cover the whole day (or were backfilled);
// otherwise use the cloud's full-day totals if we have them.
function publicDaily(date) {
  const d = state.daily[date];
  const c = state.cloudDaily[date];
  const complete = !!d && (!d.partial || !!d.before);
  const src = complete || !c ? d : c;
  return {
    date,
    produced: round(src?.produced),
    consumed: round(src?.consumed),
    imported: round(src?.imported),
    exported: round(src?.exported),
    peakW: d?.peakW ?? null,
    peakAt: d?.peakAt ?? null,
    partial: complete || c ? false : d?.partial ?? null,
    source: !src ? null : src === c ? 'cloud' : 'gateway',
    producedFromInverters: src === d && !!d?.producedFromInverters,
  };
}

app.get('/api/live', requireToken, (_req, res) => {
  const date = dayKey();
  const { counters, ...live } = state.live ?? {};
  res.json({
    live: state.live ? live : null,
    today: publicDaily(date),
    producedSevenDays: state.pdm?.production?.pcu?.wattHoursSevenDays ?? null,
  });
});

app.get('/api/today', requireToken, async (_req, res) => {
  await rollDay(dayKey());
  res.json(state.today);
});

app.get('/api/daily', requireToken, (req, res) => {
  const days = Math.min(Number(req.query.days) || 14, 90);
  const out = [];
  const d = new Date();
  for (let i = 0; i < days; i++) {
    const key = dayKey(d);
    out.unshift(publicDaily(key));
    d.setDate(d.getDate() - 1);
  }
  res.json(out);
});

app.get('/api/ev', requireToken, (_req, res) => res.json(state.ev));

app.get('/api/inverters', requireToken, async (_req, res) => {
  try {
    if (!state.inverters.data || Date.now() - state.inverters.at > 60_000) {
      state.inverters = { at: Date.now(), data: await gatewayJson('/api/v1/production/inverters') };
    }
    res.json(state.inverters.data);
  } catch (e) {
    res.status(502).json({ error: e.message });
  }
});

const dist = path.join(ROOT, 'dist');
if (existsSync(dist)) {
  app.use(express.static(dist));
  app.get(/^(?!\/api\/).*/, (_req, res) => res.sendFile(path.join(dist, 'index.html')));
}

// ---------- start ----------

state.token = (await readJson('token.json', null))?.token ?? null;
if (!state.token && existsSync(path.join(ROOT, 'token.txt'))) {
  // One-time import of a hand-made token.
  state.token = (await fs.readFile(path.join(ROOT, 'token.txt'), 'utf8')).trim();
  await writeJson('token.json', { token: state.token, savedAt: Date.now() }, 0o600);
}
state.credentials = await readJson('credentials.json', null);
state.daily = await readJson('daily.json', {});
({ meta: state.cloudMeta = {}, days: state.cloudDaily = {} } = await readJson('cloud-daily.json', {}));
await cloud.load();
state.ev = await readJson('ev.json', state.ev);
await rollDay(dayKey());
await loadGatewayInfo();
await renewIfNeeded();
sample();
setInterval(sample, POLL_MS);
setInterval(renewIfNeeded, 10 * 60_000);
setInterval(loadGatewayInfo, 10 * 60_000);
setTimeout(cloudTick, 15_000); // after the first samples have set today's baseline
setInterval(cloudTick, 10 * 60_000);

app.listen(PORT, HOST, () => {
  console.log(`Enphase dashboard API on http://${HOST}:${PORT} (gateway ${ENVOY})`);
});
