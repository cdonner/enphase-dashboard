import { fmtAgo } from './format.js';

const DAY = 86_400_000;
const fmtKwh = (v) => (v == null ? '–' : `${v.toFixed(1)} kWh`);
const fmtCost = (v) => (v == null ? '–' : `$${v.toFixed(2)}`);
const fmtWhen = (t) =>
  new Date(t).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });

function fmtDuration(ms) {
  const m = Math.round(ms / 60000);
  return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${String(m % 60).padStart(2, '0')} min`;
}

function totals(sessions, sinceMs) {
  const s = sessions.filter((x) => x.start >= sinceMs);
  return {
    n: s.length,
    kwh: s.reduce((a, x) => a + (x.kwh ?? 0), 0),
    miles: s.reduce((a, x) => a + (x.miles ?? 0), 0),
    cost: s.reduce((a, x) => a + (x.cost ?? 0), 0),
  };
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
          <div className="tile-sub">{week.n} sessions</div>
        </div>
        <div className="tile">
          <div className="tile-label">Range added, 7 days</div>
          <div className="tile-value">{Math.round(week.miles)} mi</div>
        </div>
        <div className="tile">
          <div className="tile-label">Cost, 7 days</div>
          <div className="tile-value">{fmtCost(week.cost)}</div>
          <div className="tile-sub">as calculated by Enphase</div>
        </div>
      </div>
      <table className="data-table">
        <thead>
          <tr>
            <th>Started</th>
            <th>Duration</th>
            <th>Energy</th>
            <th>Range</th>
            <th>Cost</th>
          </tr>
        </thead>
        <tbody>
          {sessions.slice(0, 10).map((s) => (
            <tr key={s.start}>
              <td>{fmtWhen(s.start)}</td>
              <td>{s.end ? fmtDuration(s.end - s.start) : 'charging'}</td>
              <td>{fmtKwh(s.kwh)}</td>
              <td>{s.miles == null ? '–' : `${Math.round(s.miles)} mi`}</td>
              <td>{fmtCost(s.cost)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

export default function EvCard({ ev, now }) {
  if (!ev?.chargers?.length) return null;
  return ev.chargers.map((c) => (
    <section className="card" key={c.serial}>
      <header className="card-head">
        <div>
          <h2>{c.name || 'EV charger'}</h2>
        </div>
        <span className="muted small">Enphase cloud · updated {fmtAgo(ev.at, now)}</span>
      </header>
      <Charger c={c} now={now} />
    </section>
  ));
}
