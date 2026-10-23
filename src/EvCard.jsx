import { useState } from 'react';
import { api } from './api.js';
import { fmtAgo } from './format.js';

const DAY = 86_400_000;
const fmtKwh = (v) => (v == null ? '–' : `${v.toFixed(1)} kWh`);
const fmtCost = (v) => (v == null ? '–' : `$${v.toFixed(2)}`);
const fmtWhen = (t) =>
  new Date(t).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
const thisMonth = () => new Date().toISOString().slice(0, 7);

function fmtDuration(ms) {
  const m = Math.round(ms / 60000);
  return m < 60 ? `${m}m` : `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}

function totals(sessions, sinceMs) {
  const s = sessions.filter((x) => x.start >= sinceMs);
  const sum = (f) => s.reduce((a, x) => a + (f(x) ?? 0), 0);
  return {
    n: s.length,
    kwh: sum((x) => x.kwh),
    solarKwh: sum((x) => x.solarKwh),
    miles: sum((x) => x.miles),
    cost: sum((x) => x.cost),
    fromRates: s.length > 0 && s.every((x) => x.costSource === 'rates'),
  };
}

const solarPct = (s) => (s.solarKwh == null || !s.kwh ? null : Math.round((s.solarKwh / s.kwh) * 100));

function RatesEditor({ settings, onSaved, onClose }) {
  const [rows, setRows] = useState(() =>
    Object.entries(settings?.rates ?? {})
      .sort(([a], [b]) => b.localeCompare(a))
      .map(([month, rate]) => ({ month, rate: String(rate) })),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  const update = (i, k, v) => setRows((r) => r.map((row, j) => (j === i ? { ...row, [k]: v } : row)));

  async function save(e) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const rates = {};
      for (const r of rows) {
        if (!r.month && !r.rate) continue;
        const rate = Number(r.rate);
        if (!/^\d{4}-\d{2}$/.test(r.month) || !(rate >= 0))
          throw new Error(`Check the row for ${r.month || 'a month'}`);
        rates[r.month] = rate;
      }
      await api('/settings', { method: 'PUT', body: { rates } });
      onSaved();
      onClose();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="rates" onSubmit={save}>
      <p className="muted small">
        Your electricity rate per month, from your utility bill. Each session's value is its kWh × the rate for its
        month; months without an entry use the most recent earlier rate.
      </p>
      <div className="rates-rows">
        {rows.map((r, i) => (
          <div className="row" key={i}>
            <input
              type="month"
              value={r.month}
              onChange={(e) => update(i, 'month', e.target.value)}
              aria-label="Month"
            />
            <label className="rate-input">
              $
              <input
                type="number"
                step="0.0001"
                min="0"
                value={r.rate}
                onChange={(e) => update(i, 'rate', e.target.value)}
                aria-label="Rate per kWh"
              />
              /kWh
            </label>
            <button type="button" className="ghost" onClick={() => setRows((rs) => rs.filter((_, j) => j !== i))}>
              Remove
            </button>
          </div>
        ))}
      </div>
      <div className="row">
        <button
          type="button"
          className="ghost"
          onClick={() => setRows((rs) => [{ month: thisMonth(), rate: '' }, ...rs])}
        >
          Add month
        </button>
      </div>
      {error && <p className="error">{error}</p>}
      <div className="row">
        <button type="submit" disabled={busy}>
          Save rates
        </button>
        <button type="button" className="ghost" onClick={onClose}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function Charger({ c, now }) {
  const sessions = c.sessions ?? [];
  const week = totals(sessions, now - 7 * DAY);
  const charging = sessions.some((s) => !s.end);
  return (
    <>
      <p className="muted small">
        {c.model} · {c.status}
        {charging ? ' · charging now' : ''} · last reported {fmtAgo(c.lastReportAt, now)}
      </p>
      <div className="tiles ev-tiles">
        <div className="tile">
          <div className="tile-label">Last 7 days</div>
          <div className="tile-value">{fmtKwh(week.kwh)}</div>
          <div className="tile-sub">
            {week.n} sessions
            {week.kwh > 0 && week.solarKwh > 0 ? ` · ${Math.round((week.solarKwh / week.kwh) * 100)}% solar` : ''}
          </div>
        </div>
        <div className="tile">
          <div className="tile-label">Range added, 7 days</div>
          <div className="tile-value">{Math.round(week.miles)} mi</div>
        </div>
        <div className="tile">
          <div className="tile-label">Value, 7 days</div>
          <div className="tile-value">{fmtCost(week.cost)}</div>
          <div className="tile-sub">
            {week.fromRates ? 'at your electricity rates' : 'Enphase’s figure; set your rates'}
          </div>
        </div>
      </div>
      <div className="table-scroll">
        <table className="data-table ev-table">
          <thead>
            <tr>
              <th>Started</th>
              <th className="wide-only">Duration</th>
              <th>kWh</th>
              <th>Solar</th>
              <th className="wide-only">Range</th>
              <th>Value</th>
            </tr>
          </thead>
          <tbody>
            {sessions.slice(0, 10).map((s) => (
              <tr key={s.start}>
                <td>{fmtWhen(s.start)}</td>
                <td className="wide-only">{s.end ? fmtDuration(s.end - s.start) : 'charging'}</td>
                <td>{s.kwh == null ? '–' : s.kwh.toFixed(1)}</td>
                <td>{solarPct(s) == null ? '–' : `${solarPct(s)}%`}</td>
                <td className="wide-only">{s.miles == null ? '–' : `${Math.round(s.miles)} mi`}</td>
                <td
                  title={s.costSource === 'rates' ? `Enphase: ${fmtCost(s.enphaseCost)}` : 'As calculated by Enphase'}
                >
                  {fmtCost(s.cost)}
                  {s.costSource !== 'rates' && s.cost != null ? ' *' : ''}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {sessions.slice(0, 10).some((s) => s.costSource !== 'rates' && s.cost != null) && (
        <p className="muted small">* Enphase’s own figure. Set your electricity rates to see each session’s value.</p>
      )}
    </>
  );
}

export default function EvCard({ ev, now, onSettingsSaved }) {
  const [editing, setEditing] = useState(false);
  if (!ev?.chargers?.length) return null;
  return ev.chargers.map((c) => (
    <section className="card" key={c.serial}>
      <header className="card-head">
        <div>
          <h2>{c.name || 'EV charger'}</h2>
        </div>
        <div className="row">
          <span className="muted small">Enphase cloud · updated {fmtAgo(ev.at, now)}</span>
          <button className="ghost" onClick={() => setEditing((v) => !v)}>
            Electricity rates
          </button>
        </div>
      </header>
      {editing && <RatesEditor settings={ev.settings} onSaved={onSettingsSaved} onClose={() => setEditing(false)} />}
      <Charger c={c} now={now} />
    </section>
  ));
}
