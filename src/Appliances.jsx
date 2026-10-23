import { useMemo, useState } from 'react';
import { api, usePoll } from './api.js';
import { fmtTime, fmtW, fmtWh } from './format.js';

// Colors follow the circuit type, which is what the eye needs to separate first.
const TYPES = [
  { key: '240', label: '240 V', color: 'var(--series-1)', test: (l) => l.startsWith('240') || l.includes('phases') },
  { key: 'l1', label: '120 V · leg 1', color: 'var(--series-2)', test: (l) => l.endsWith('leg 1') || l === 'phase 1' },
  { key: 'l2', label: '120 V · leg 2', color: 'var(--series-3)', test: () => true },
];
const typeOf = (legs) => TYPES.find((t) => t.test(legs ?? ''));

const fmtDur = (s) => {
  if (s < 60) return `${Math.round(s)}s`;
  const m = Math.round(s / 60);
  return m < 60 ? `${m}m` : `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
};
const sigText = (sig) => `${fmtW(sig.powerW)} · ${sig.legs} · ${sig.character}`;
const label = (a) => a.appliance ?? sigText(a.signature);
// Compact form for the timeline's narrow label column; the full signature is in the hover text.
const shortLabel = (a) => a.appliance ?? `${fmtW(a.signature.powerW)} · ${a.signature.legs.replace('120 V · ', '')}`;

function dateKey(d) {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}
function shiftDate(key, days) {
  const d = new Date(`${key}T12:00:00`);
  d.setDate(d.getDate() + days);
  return dateKey(d);
}

function Timeline({ date, activities, onName }) {
  const [hover, setHover] = useState(null);
  const [selected, setSelected] = useState(null);
  const rows = useMemo(() => {
    const byRow = new Map();
    for (const a of activities) {
      const key = a.appliance ?? a.clusterKey;
      const row = byRow.get(key) ?? {
        key,
        label: shortLabel(a),
        named: !!a.appliance,
        type: typeOf(a.signature.legs),
        spans: [],
        energy: 0,
      };
      row.spans.push(...a.spans.map((s) => ({ s, a })));
      row.energy += a.energyWh;
      byRow.set(key, row);
    }
    return [...byRow.values()].sort((x, y) => (x.named === y.named ? y.energy - x.energy : x.named ? -1 : 1));
  }, [activities]);

  const t0 = new Date(`${date}T00:00:00`).getTime();
  const pct = (t) => `${Math.min(100, Math.max(0, ((t - t0) / 86_400_000) * 100))}%`;
  if (!rows.length) return <p className="muted">No appliance activity detected for this day yet.</p>;

  return (
    <div className="timeline">
      <div className="tl-axis">
        {[0, 6, 12, 18, 24].map((h) => (
          <span key={h} style={{ left: `${(h / 24) * 100}%` }}>
            {h === 24 ? '' : new Date(t0 + h * 3_600_000).toLocaleTimeString([], { hour: 'numeric' })}
          </span>
        ))}
      </div>
      {rows.map((r) => (
        <div className="tl-row" key={r.key}>
          <div className="tl-label" title={r.label}>
            <span className="rect-key" style={{ background: r.type.color }} />
            <span className={r.named ? '' : 'muted'}>{r.label}</span>
          </div>
          <div className="tl-track">
            {r.spans.map(({ s, a }, i) => (
              <span
                key={i}
                className="tl-bar"
                tabIndex={0}
                style={{
                  left: pct(s[0]),
                  width: `max(2px, calc(${pct(s[1])} - ${pct(s[0])}))`,
                  background: r.type.color,
                }}
                onPointerEnter={() => setHover({ r, s, a })}
                onPointerLeave={() => setHover(null)}
                onFocus={() => setHover({ r, s, a })}
                onBlur={() => setHover(null)}
                onClick={() => setSelected({ r, s, a })}
                onKeyDown={(e) => e.key === 'Enter' && setSelected({ r, s, a })}
              />
            ))}
          </div>
        </div>
      ))}
      <div className="tl-hover" aria-live="polite">
        {hover ? (
          <>
            <strong>
              {fmtTime(hover.s[0])} – {fmtTime(hover.s[1])}
            </strong>{' '}
            <span className="muted">
              {fmtDur((hover.s[1] - hover.s[0]) / 1000)} · {label(hover.a)}
              {hover.a.appliance ? ` · ${sigText(hover.a.signature)}` : ` · ${hover.a.signature.character}`}
            </span>
          </>
        ) : (
          <span className="muted">Hover a bar for details, click it to name it.</span>
        )}
      </div>
      {selected && (
        <div className="tl-selected">
          <div>
            <strong>{label(selected.a)}</strong>{' '}
            <span className="muted small">
              {fmtTime(selected.s[0])} · {fmtDur((selected.s[1] - selected.s[0]) / 1000)} ·{' '}
              {sigText(selected.a.signature)}
            </span>
          </div>
          {selected.a.appliance ? (
            <span className="muted small">Already named. Manage it under Known appliances.</span>
          ) : (
            <NameForm
              onSave={async (name) => {
                await onName(name, selected.a.signature);
                setSelected(null);
              }}
            />
          )}
          <button className="ghost" onClick={() => setSelected(null)}>
            Close
          </button>
        </div>
      )}
    </div>
  );
}

function NameForm({ placeholder = 'Name, or pick an existing appliance', initial = '', onSave, button = 'Save' }) {
  const [name, setName] = useState(initial);
  const [busy, setBusy] = useState(false);
  return (
    <form
      className="row name-form"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!name.trim()) return;
        setBusy(true);
        try {
          await onSave(name.trim());
          setName('');
        } finally {
          setBusy(false);
        }
      }}
    >
      <input
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder={placeholder}
        aria-label="Appliance name"
        list="appliance-names"
      />
      <button type="submit" disabled={busy || !name.trim()}>
        {button}
      </button>
    </form>
  );
}

function Unnamed({ clusters, onChanged }) {
  if (!clusters?.length)
    return (
      <p className="muted small">Nothing to name right now. New signatures show up here as soon as they're detected.</p>
    );
  return (
    <div className="clusters">
      {clusters.map((c) => (
        <div className="cluster" key={c.key}>
          <div className="cluster-head">
            <span className="rect-key" style={{ background: typeOf(c.signature.legs).color }} />
            <strong>{fmtW(c.signature.powerW)}</strong>
            <span className="muted">
              {c.signature.legs} · {c.signature.character}
            </span>
            <span className="muted small cluster-id" title="ID used in the AI prompt">
              ID {c.key}
            </span>
          </div>
          <div className="muted small">
            {c.runs} {c.runs === 1 ? 'run' : 'runs'} on {c.days} {c.days === 1 ? 'day' : 'days'} · typically on{' '}
            {fmtDur(c.medianOnSec)} · {fmtWh(c.energyWh)} in total
          </div>
          {c.ai?.guesses?.length > 0 && (
            <div className="ai-guesses">
              <span className="muted small">{c.ai.source === 'gemini' ? 'Gemini' : 'Claude'} suggests:</span>
              {c.ai.guesses.map((g) => (
                <div key={g.name} className="ai-guess">
                  <button
                    className="ghost chip"
                    title={`Name it "${g.name}"`}
                    onClick={async () => {
                      await api('/appliances', {
                        method: 'POST',
                        body: { name: g.name, dp: c.signature.dp, dq: c.signature.dq },
                      });
                      onChanged();
                    }}
                  >
                    {g.name}
                  </button>
                  <span className={`confidence ${g.confidence}`}>{g.confidence}</span>
                  <span className="muted small">{g.reason}</span>
                </div>
              ))}
            </div>
          )}
          {c.suggestions?.length > 0 && (
            <div className="row suggestions">
              <span className="muted small">{c.ai?.guesses?.length ? 'Catalog:' : 'Possibly:'}</span>
              {c.suggestions.map((name) => (
                <button
                  key={name}
                  className="ghost chip"
                  title={`Name it "${name}"`}
                  onClick={async () => {
                    await api('/appliances', {
                      method: 'POST',
                      body: { name, dp: c.signature.dp, dq: c.signature.dq },
                    });
                    onChanged();
                  }}
                >
                  {name}
                </button>
              ))}
            </div>
          )}
          <div className="muted small">
            e.g.{' '}
            {c.examples
              .map((t) => new Date(t).toLocaleString([], { weekday: 'short', hour: 'numeric', minute: '2-digit' }))
              .join(', ')}
          </div>
          <NameForm
            onSave={async (name) => {
              await api('/appliances', { method: 'POST', body: { name, dp: c.signature.dp, dq: c.signature.dq } });
              onChanged();
            }}
          />
        </div>
      ))}
    </div>
  );
}

function AskAI({ providers = [], household, onChanged }) {
  const [text, setText] = useState(household ?? '');
  const [saved, setSaved] = useState(household ?? '');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [note, setNote] = useState(null);
  const [manual, setManual] = useState(null); // prompt to copy by hand when the clipboard isn't available

  async function saveHousehold() {
    await api('/household', { method: 'PUT', body: { household: text } });
    setSaved(text);
  }
  async function run(fn) {
    setBusy(true);
    setError(null);
    setNote(null);
    try {
      if (text !== saved) await saveHousehold();
      await fn();
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy(false);
    }
  }
  const copyPrompt = () =>
    run(async () => {
      const { prompt } = await api('/appliances/ai-prompt');
      try {
        await navigator.clipboard.writeText(prompt); // needs https or localhost
        setManual(null);
        setNote(
          'Copied. Paste it into a chat with Claude (or another assistant), then name the signatures it identifies.',
        );
      } catch {
        setManual(prompt);
      }
    });
  const [asking, setAsking] = useState(null);
  const askApi = (p) =>
    run(async () => {
      setAsking(p.id);
      try {
        await api(`/appliances/ask-ai?provider=${p.id}`, { method: 'POST' });
        onChanged();
      } finally {
        setAsking(null);
      }
    });

  return (
    <div className="ask-ai">
      <details open={!household}>
        <summary>About your home</summary>
        <p className="muted small">
          List your larger appliances and anything unusual: what runs on gas, what's off for the season, sizes you know.
          It's included in the AI prompt as context.
        </p>
        <textarea
          rows={5}
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="e.g. Gas water heater and 2 gas furnaces (blowers are electric). 6 kW sauna heater with 3 stages. Drip coffee maker. Pool pump off for winter."
        />
        <div className="row">
          <button className="ghost" disabled={text === saved} onClick={() => run(saveHousehold)}>
            Save
          </button>
        </div>
      </details>
      <div className="row ai-row">
        <button disabled={busy} onClick={copyPrompt}>
          Copy prompt for AI chat
        </button>
        <a className="small" href="https://claude.ai/new" target="_blank" rel="noreferrer">
          Open claude.ai
        </a>
        {providers.map((p) => (
          <button key={p.id} className="ghost" disabled={busy} onClick={() => askApi(p)} title={`Model: ${p.model}`}>
            {asking === p.id ? `Asking ${p.name}…` : `Ask ${p.name}`}
          </button>
        ))}
      </div>
      <p className="muted small">
        The prompt holds only summary numbers per signature and the text above, no readings or location.
        {providers.length ? ' The Ask buttons show the answers right here.' : ''}
      </p>
      {note && <p className="small">{note}</p>}
      {manual && (
        <div className="manual-copy">
          <p className="small">Your browser blocked the clipboard on this address. Select all and copy:</p>
          <textarea rows={8} readOnly value={manual} onFocus={(e) => e.target.select()} />
        </div>
      )}
      {error && <p className="error">{error}</p>}
    </div>
  );
}

function Known({ appliances, onChanged }) {
  const [editing, setEditing] = useState(null);
  if (!appliances?.length) return <p className="muted small">None yet.</p>;
  return (
    <table className="data-table known">
      <tbody>
        {appliances.map((a) => (
          <tr key={a.id}>
            <td>
              {editing === a.id ? (
                <NameForm
                  initial={a.name}
                  button="Rename"
                  onSave={async (name) => {
                    await api(`/appliances/${a.id}`, { method: 'PUT', body: { name } });
                    setEditing(null);
                    onChanged();
                  }}
                />
              ) : (
                <strong>{a.name}</strong>
              )}
            </td>
            <td>
              <ul className="sig-list">
                {(a.described ?? []).map((d, n) => (
                  <li key={n}>
                    <span className="rect-key" style={{ background: typeOf(d.legs).color }} />
                    <span className="muted small">{sigText(d)}</span>
                    {a.described.length > 1 && (
                      <button
                        className="ghost link"
                        title="Remove this signature from the appliance"
                        aria-label={`Remove signature ${sigText(d)}`}
                        onClick={async () => {
                          await api(`/appliances/${a.id}/signatures/${n}`, { method: 'DELETE' });
                          onChanged();
                        }}
                      >
                        ×
                      </button>
                    )}
                  </li>
                ))}
              </ul>
            </td>
            <td>
              <div className="row">
                <button className="ghost" onClick={() => setEditing(editing === a.id ? null : a.id)}>
                  {editing === a.id ? 'Cancel' : 'Rename'}
                </button>
                <button
                  className="ghost danger"
                  onClick={async () => {
                    await api(`/appliances/${a.id}`, { method: 'DELETE' });
                    onChanged();
                  }}
                >
                  Forget
                </button>
              </div>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export default function Appliances() {
  const today = dateKey(new Date());
  const [date, setDate] = useState(today);
  const day = usePoll(`/appliances/day?date=${date}`, date === today ? 15_000 : 3_600_000);
  const lists = usePoll('/appliances', 60_000);
  const reload = () => {
    day.reload();
    lists.reload();
  };
  const nameSignature = async (name, signature) => {
    await api('/appliances', { method: 'POST', body: { name, dp: signature.dp, dq: signature.dq } });
    reload();
  };
  const knownNames = (lists.data?.appliances ?? []).map((a) => a.name);

  const acts = (day.data?.date === date && day.data.activities) || [];
  const running = acts.filter((a) => a.end == null);
  const named = acts.filter((a) => a.appliance);
  const sum = (xs) => xs.reduce((s, a) => s + a.energyWh, 0);

  return (
    <>
      <section className="card">
        <header className="card-head">
          <div>
            <h2>Appliance activity</h2>
            <p className="muted small">
              Detected from second-by-second changes on each leg. Larger appliances (about 60 W and up) that switch on
              and off are found; slowly varying loads aren’t.
            </p>
          </div>
          <div className="row day-nav">
            <button className="ghost" onClick={() => setDate((d) => shiftDate(d, -1))} aria-label="Previous day">
              ‹
            </button>
            <span>
              {date === today
                ? 'Today'
                : new Date(`${date}T12:00:00`).toLocaleDateString([], {
                    weekday: 'short',
                    month: 'short',
                    day: 'numeric',
                  })}
            </span>
            <button
              className="ghost"
              disabled={date >= today}
              onClick={() => setDate((d) => shiftDate(d, 1))}
              aria-label="Next day"
            >
              ›
            </button>
          </div>
        </header>
        <div className="tiles ev-tiles">
          <div className="tile">
            <div className="tile-label">Running now</div>
            <div className="tile-value">{date === today ? running.length : '–'}</div>
            <div className="tile-sub">
              {date === today && running.length ? running.map(label).slice(0, 3).join(', ') : ' '}
            </div>
          </div>
          <div className="tile">
            <div className="tile-label">Identified</div>
            <div className="tile-value">{fmtWh(sum(named))}</div>
            <div className="tile-sub">{named.length} activities</div>
          </div>
          <div className="tile">
            <div className="tile-label">Not yet named</div>
            <div className="tile-value">{fmtWh(sum(acts) - sum(named))}</div>
            <div className="tile-sub">{acts.length - named.length} activities</div>
          </div>
        </div>
        <div className="legend">
          {TYPES.map((t) => (
            <span key={t.key} className="legend-item">
              <span className="rect-key" style={{ background: t.color }} />
              {t.label}
            </span>
          ))}
        </div>
        <datalist id="appliance-names">
          {knownNames.map((n) => (
            <option key={n} value={n} />
          ))}
        </datalist>
        <Timeline date={date} activities={acts} onName={nameSignature} />
        <details className="table-view">
          <summary>Table</summary>
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>Start</th>
                  <th>Duration</th>
                  <th>Appliance</th>
                  <th>Power</th>
                  <th>Runs</th>
                  <th>Energy</th>
                </tr>
              </thead>
              <tbody>
                {acts.map((a, i) => (
                  <tr key={i}>
                    <td>{fmtTime(a.start)}</td>
                    <td>{a.end ? fmtDur((a.end - a.start) / 1000) : 'running'}</td>
                    <td>{label(a)}</td>
                    <td>{fmtW(a.signature.powerW)}</td>
                    <td>{a.runs}</td>
                    <td>{fmtWh(a.energyWh)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      </section>

      <div className="two-col">
        <section className="card">
          <h2>Needs a name</h2>
          <p className="muted small">
            Signatures that repeated over the last 7 days, most energy first. Naming one labels all of its runs, past
            and future.
          </p>
          {lists.data && (
            <AskAI providers={lists.data.aiProviders} household={lists.data.household} onChanged={reload} />
          )}
          <Unnamed clusters={lists.data?.unnamed} onChanged={reload} />
        </section>
        <section className="card">
          <h2>Known appliances</h2>
          <Known appliances={lists.data?.appliances} onChanged={reload} />
        </section>
      </div>
    </>
  );
}
