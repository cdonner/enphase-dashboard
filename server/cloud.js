// Enphase cloud (API v4, "Watt" plan) - used only to backfill what the local
// gateway can't provide: full-day totals for past days and the part of today
// before the dashboard started. The plan allows ~10 calls/minute and
// 1,000/month, so calls are serialized and kept to a handful per day.
const API = 'https://api.enphaseenergy.com/api/v4';
const OAUTH = 'https://api.enphaseenergy.com/oauth';
export const REDIRECT_URI = 'https://api.enphaseenergy.com/oauth/redirect_uri';
const MIN_CALL_GAP_MS = 7_000;

export function createCloud({ readJson, writeJson }) {
  let cfg = null; // contents of data/cloud.json
  let lastCall = 0;
  let queue = Promise.resolve();
  const status = { lastSyncAt: null, error: null };

  // App credentials come from data/cloud.json or the environment (env wins);
  // OAuth tokens and the discovered system id are always kept in data/cloud.json.
  async function load() {
    cfg = (await readJson('cloud.json', null)) ?? {};
    const env = {
      apiKey: process.env.ENPHASE_API_KEY,
      clientId: process.env.ENPHASE_CLIENT_ID,
      clientSecret: process.env.ENPHASE_CLIENT_SECRET,
    };
    for (const [k, v] of Object.entries(env)) if (v) cfg[k] = v;
    return cfg;
  }
  const save = () => writeJson('cloud.json', cfg, 0o600);
  const configured = () => !!(cfg?.apiKey && cfg?.clientId && cfg?.clientSecret);
  const connected = () => configured() && !!cfg.refreshToken;

  async function oauthToken(params) {
    const res = await fetch(`${OAUTH}/token?${new URLSearchParams(params)}`, {
      method: 'POST',
      headers: { Authorization: `Basic ${Buffer.from(`${cfg.clientId}:${cfg.clientSecret}`).toString('base64')}` },
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok || !body.access_token) throw new Error(body.error_description || body.error || `OAuth HTTP ${res.status}`);
    cfg.accessToken = body.access_token;
    cfg.refreshToken = body.refresh_token;
    cfg.accessExpiresAt = Date.now() + body.expires_in * 1000;
    cfg.refreshObtainedAt = Date.now();
    await save();
  }

  // One-time: exchange the code shown on the redirect page after the owner approves.
  async function connect(code) {
    if (!configured()) throw new Error('data/cloud.json needs apiKey, clientId and clientSecret');
    await oauthToken({ grant_type: 'authorization_code', redirect_uri: REDIRECT_URI, code: String(code).trim() });
    status.error = null;
  }

  async function accessToken() {
    if (!connected()) throw new Error('Enphase cloud not connected');
    if (!cfg.accessToken || cfg.accessExpiresAt - Date.now() < 10 * 60_000) {
      await oauthToken({ grant_type: 'refresh_token', refresh_token: cfg.refreshToken });
    }
    return cfg.accessToken;
  }

  // Serialized, rate-limited GET against the v4 API.
  function get(path) {
    const run = async () => {
      const wait = lastCall + MIN_CALL_GAP_MS - Date.now();
      if (wait > 0) await new Promise((r) => setTimeout(r, wait));
      lastCall = Date.now();
      const res = await fetch(`${API}${path}`, {
        headers: { Authorization: `Bearer ${await accessToken()}`, key: cfg.apiKey },
      });
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(`Enphase cloud ${path.split('?')[0]}: ${body.details || body.message || `HTTP ${res.status}`}`);
      return body;
    };
    const p = queue.then(run, run);
    queue = p.catch(() => {});
    return p;
  }

  async function systemId() {
    if (!cfg.systemId) {
      const { systems } = await get('/systems');
      if (!systems?.length) throw new Error('No systems on this Enphase account');
      cfg.systemId = systems[0].system_id;
      await save();
    }
    return cfg.systemId;
  }

  // Daily totals (Wh) from startDate (YYYY-MM-DD) through the last complete day.
  // 4 calls. Returns { 'YYYY-MM-DD': { produced, consumed, imported, exported } }.
  async function dailyTotals(startDate) {
    const id = await systemId();
    const q = `?start_date=${startDate}`;
    const prod = await get(`/systems/${id}/energy_lifetime${q}`);
    const cons = await get(`/systems/${id}/consumption_lifetime${q}`);
    const imp = await get(`/systems/${id}/energy_import_lifetime${q}`);
    const exp = await get(`/systems/${id}/energy_export_lifetime${q}`);
    const out = {};
    const d = new Date(`${prod.start_date}T12:00:00`);
    (prod.production || []).forEach((wh, i) => {
      const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
      out[key] = {
        produced: wh,
        consumed: cons.consumption?.[i] ?? null,
        imported: imp.import?.[i] ?? null,
        exported: exp.export?.[i] ?? null,
      };
      d.setDate(d.getDate() + 1);
    });
    status.lastSyncAt = Date.now();
    return out;
  }

  // 15-minute intervals since startAt (unix seconds). 4 calls.
  // Returns [{ end, produced, consumed, imported, exported }] in Wh, oldest first.
  async function intervals(startAt) {
    const id = await systemId();
    const q = `?start_at=${startAt}&granularity=day`;
    const prod = await get(`/systems/${id}/telemetry/production_meter${q}`);
    const cons = await get(`/systems/${id}/telemetry/consumption_meter${q}`);
    const imp = await get(`/systems/${id}/energy_import_telemetry${q}`);
    const exp = await get(`/systems/${id}/energy_export_telemetry${q}`);
    const byEnd = new Map();
    const put = (list, field, key) => {
      for (const i of (list || []).flat()) {
        const row = byEnd.get(i.end_at) ?? { end: i.end_at, produced: null, consumed: null, imported: null, exported: null };
        row[key] = i[field];
        byEnd.set(i.end_at, row);
      }
    };
    put(prod.intervals, 'wh_del', 'produced');
    put(cons.intervals, 'enwh', 'consumed');
    put(imp.intervals, 'wh_imported', 'imported');
    put(exp.intervals, 'wh_exported', 'exported');
    status.lastSyncAt = Date.now();
    return [...byEnd.values()].sort((a, b) => a.end - b.end);
  }

  // EV chargers on the system (IQ EV Charger reports to the cloud only). 1 call.
  async function evChargers() {
    const id = await systemId();
    const body = await get(`/systems/${id}/ev_charger/devices`);
    return (body.devices?.ev_chargers || []).map((c) => ({
      serial: c.serial_number,
      name: c.name,
      model: c.model,
      status: c.status,
      active: c.active,
      lastReportAt: c.last_report_at ? c.last_report_at * 1000 : null,
    }));
  }

  // Recent charging sessions, newest first. 1 call.
  async function evSessions(serial, limit = 20) {
    const id = await systemId();
    const body = await get(`/systems/${id}/ev_charger/${serial}/sessions?limit=${limit}`);
    return (body.sessions || []).map((s) => ({
      start: s.start_time * 1000,
      end: s.end_time ? s.end_time * 1000 : null,
      kwh: s.energy_added,
      miles: s.miles_added,
      cost: s.cost,
    }));
  }

  return {
    load,
    evChargers,
    evSessions,
    connect,
    dailyTotals,
    intervals,
    configured,
    connected,
    status,
    authorizeUrl: () =>
      cfg?.clientId &&
      `${OAUTH}/authorize?response_type=code&client_id=${cfg.clientId}&redirect_uri=${encodeURIComponent(REDIRECT_URI)}`,
  };
}
