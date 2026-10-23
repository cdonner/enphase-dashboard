import { useEffect, useState } from 'react';
import { usePoll } from './api.js';
import AuthPanel from './AuthPanel.jsx';
import EvCard from './EvCard.jsx';
import { DailyChart, DailyTable, InverterChart, InverterTable, Legend, PowerChart, PowerTable } from './charts.jsx';
import { fmtAgo, fmtExpiry, fmtTime, fmtW, fmtWh } from './format.js';

function Tile({ label, value, sub }) {
  return (
    <div className="tile">
      <div className="tile-label">{label}</div>
      <div className="tile-value">{value}</div>
      {sub && <div className="tile-sub">{sub}</div>}
    </div>
  );
}

function Card({ title, subtitle, legend, table, children, className = '' }) {
  return (
    <section className={`card ${className}`}>
      <header className="card-head">
        <div>
          <h2>{title}</h2>
          {subtitle && <p className="muted small">{subtitle}</p>}
        </div>
        {legend}
      </header>
      {children}
      {table && (
        <details className="table-view">
          <summary>Table</summary>
          {table}
        </details>
      )}
    </section>
  );
}

const PRODUCTION = { label: 'Production', color: 'var(--series-1)' };
const CONSUMPTION = { label: 'Consumption', color: 'var(--series-2)' };

export default function App() {
  const status = usePoll('/status', 15_000);
  const [showAuth, setShowAuth] = useState(false);
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  const st = status.data;
  const authed = !!st?.token?.present && !st.token.expired;
  const live = usePoll('/live', 5_000, authed);
  const today = usePoll('/today', 60_000, authed);
  const daily = usePoll('/daily?days=14', 300_000, authed);
  const inverters = usePoll('/inverters', 60_000, authed);
  const ev = usePoll('/ev', 300_000, authed);

  // A 401 from the data endpoints means the token went away: refresh status.
  const reloadStatus = status.reload;
  useEffect(() => {
    if (live.error?.status === 401) reloadStatus();
  }, [live.error, reloadStatus]);

  function onAuthDone() {
    setShowAuth(false);
    status.reload();
    setTimeout(() => {
      live.reload();
      today.reload();
      daily.reload();
      inverters.reload();
    }, 1500);
  }

  if (!st) return <main className="page"><p className="muted">Connecting to the dashboard server…</p></main>;

  const l = live.data?.live;
  const d = live.data?.today;
  const stale = l && now - l.at > 30_000;
  const exporting = l?.netW != null && l.netW < 0;
  const invList = Array.isArray(inverters.data) ? inverters.data : [];
  const reporting = invList.filter((i) => i.lastReportWatts > 0).length;

  return (
    <main className="page">
      <header className="top">
        <div>
          <h1>Solar</h1>
          <p className="muted small">
            Gateway {st.gateway?.serial ?? '?'} · {st.gatewayHost}
            {st.gateway?.firmware ? ` · ${st.gateway.firmware}` : ''}
          </p>
        </div>
        <div className="top-right">
          <span className={`pill ${st.lastError || stale ? 'pill-warn' : ''}`}>
            <span aria-hidden="true">{st.lastError || stale ? '⚠' : '●'}</span>
            {st.lastError ? st.lastError : l ? `Updated ${fmtAgo(l.at, now)}` : authed ? 'Waiting for data' : 'Not connected'}
          </span>
          {st.token?.present && (
            <button className="ghost" onClick={() => setShowAuth((v) => !v)}>
              Token · {fmtExpiry(st.token.expiresAt, now)}
            </button>
          )}
        </div>
      </header>

      {(!authed || showAuth) && <AuthPanel status={st} onDone={onAuthDone} onClose={authed ? () => setShowAuth(false) : null} />}

      {authed && (
        <>
          <section className="overview">
            <div className="card hero">
              <div className="tile-label">Producing now</div>
              <div className="hero-value">{fmtW(l?.productionW)}</div>
              <div className="tile-sub">
                {d?.peakW ? `Peak today ${fmtW(d.peakW)} at ${fmtTime(d.peakAt)}` : ' '}
              </div>
              {live.data?.producedSevenDays != null && (
                <div className="tile-sub">{fmtWh(live.data.producedSevenDays)} produced in the last 7 days</div>
              )}
            </div>
            <div className="tiles">
              <Tile label="Consuming now" value={fmtW(l?.consumptionW)} />
              <Tile
                label={exporting ? 'Exporting to grid' : 'Importing from grid'}
                value={fmtW(l?.netW == null ? null : Math.abs(l.netW))}
                sub={l?.netW == null ? null : exporting ? '↑ to grid' : '↓ from grid'}
              />
              <Tile
                label="Produced today"
                value={fmtWh(d?.produced)}
                sub={d?.producedFromInverters ? 'full day, from inverters' : d?.partial ? 'since dashboard started' : null}
              />
              <Tile label="Consumed today" value={fmtWh(d?.consumed)} sub={d?.partial ? 'since dashboard started' : null} />
              <Tile label="Imported today" value={fmtWh(d?.imported)} />
              <Tile label="Exported today" value={fmtWh(d?.exported)} />            </div>
          </section>

          <Card
            title="Power today"
            subtitle="1-minute gateway readings · 15-minute Enphase cloud data where the dashboard wasn’t running"
            legend={<Legend items={[PRODUCTION, CONSUMPTION]} />}
            table={<PowerTable points={today.data?.points} />}
            className={today.error ? 'dim' : ''}
          >
            <PowerChart points={today.data?.points ?? []} date={today.data?.date} />
          </Card>

          <div className="two-col">
            <Card
              title="Daily energy"
              subtitle="Last 14 days · gateway data, cloud for days it missed · lighter = incomplete"
              legend={<Legend items={[{ ...PRODUCTION, label: 'Produced', shape: 'rect' }, { ...CONSUMPTION, label: 'Consumed', shape: 'rect' }]} />}
              table={<DailyTable days={daily.data ?? []} />}
            >
              <DailyChart days={daily.data ?? []} />
            </Card>
            <Card
              title="Microinverters"
              subtitle={invList.length ? `Latest report per inverter · ${reporting} of ${invList.length} producing` : 'Loading…'}
              table={<InverterTable inverters={invList} />}
            >
              <InverterChart inverters={invList} />
            </Card>
          </div>

          <EvCard ev={ev.data} now={now} />
        </>
      )}
    </main>
  );
}
