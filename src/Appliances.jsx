import { useEffect, useMemo, useState } from 'react';
import { api, usePoll } from './api.js';
import { fmtTime, fmtW, fmtWh } from './format.js';

// Colors follow the circuit type, which is what the eye needs to separate first.
const TYPES = [
  { key: '240', label: '240 V', color: 'var(--series-1)', test: (l) => l.startsWith('240') || l.includes('phases') },
  { key: 'l1', label: '120 V · leg 1', color: 'var(--series-2)', test: (l) => l.endsWith('leg 1') || l === 'phase 1' },
  { key: 'l2', label: '120 V · leg 2', color: 'var(--series-3)', test: () => true },
];
const typeOf = (legs) => TYPES.find((t) => t.test(legs ?? ''));

// Smallest unnamed load shown; small steps are numerous and rarely worth naming.
const SIZES = [60, 150, 300, 1000];
function loadMinW() {
  try {
    const v = Number(localStorage.getItem('appliances.minW'));
    return SIZES.includes(v) ? v : 150;
  } catch {
    return 150;
  }
}

// A one-time load: an unnamed signature seen only once in two weeks, under 1 kW. Mostly
// two loads switching in the same second, or a stray reading; a real appliance comes back.
const ONCE_MAX_W = 1000;
const isOnce = (runs, sig) => runs <= 1 && sig.powerW < ONCE_MAX_W;

function loadHideOnce() {
  try {
    return localStorage.getItem('appliances.hideOnce') !== 'false';
  } catch {
    return true;
  }
}

// Drops unnamed runs below minW, and one-time loads if hideOnce; activities left without
// runs disappear.
function filterSmall(acts, minW, hideOnce) {
  let hidden = 0;
  const out = [];
  for (const a of acts) {
    if (a.appliance) {
      out.push(a);
      continue;
    }
    if (hideOnce && isOnce(a.seen ?? a.runs, a.signature)) {
      hidden += a.spans.length;
      continue;
    }
    const spans = a.spans.filter((sp) => sp.desc.powerW >= minW);
    hidden += a.spans.length - spans.length;
    if (spans.length) out.push({ ...a, spans });
  }
  return [out, hidden];
}

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

// Details of a clicked run: name it, or for a named one label its signature, move it to
// another appliance, or take it off its appliance.
function Selected({ sel, onName, onChanged, onClose }) {
  const { a, sp, s } = sel;
  const [labelText, setLabelText] = useState(sp.label ?? '');
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose();
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);
  const sigUrl = `/appliances/${a.applianceId}/signatures/${sp.sig}`;
  const done = () => {
    onChanged();
    onClose();
  };
  // A popup over the page: with many rows, a panel below the timeline is out of sight.
  return (
    <div className="modal-backdrop" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal tl-selected" role="dialog" aria-modal="true" aria-label="Run details">
        <div className="tl-selected-head">
          <strong>
            {label(a)}
            {sp.label ? ` · ${sp.label}` : ''}
          </strong>{' '}
          <span className="muted small">
            {fmtTime(s[0])} · {fmtDur((s[1] - s[0]) / 1000)}
            {a.appliance ? ` · ${sigText(sp.desc)}` : ''}
          </span>
          <button className="ghost" onClick={onClose}>
            Close
          </button>
        </div>
        {!a.appliance ? (
          <div className="row name-row">
            <NameForm
              onSave={async (name) => {
                await onName(name, sp.desc);
                onClose();
              }}
            />
            <button
              className="ghost"
              title="Hide this signature from the timeline and the naming list"
              onClick={async () => {
                await api('/appliances', { method: 'POST', body: { ignore: true, dp: sp.desc.dp, dq: sp.desc.dq } });
                onChanged();
                onClose();
              }}
            >
              Ignore
            </button>
          </div>
        ) : (
          <div className="sel-actions">
            <form
              className="row name-form"
              onSubmit={async (e) => {
                e.preventDefault();
                await api(sigUrl, { method: 'PUT', body: { label: labelText } });
                done();
              }}
            >
              <input
                value={labelText}
                onChange={(e) => setLabelText(e.target.value)}
                placeholder="Label for this signature, e.g. warm-up"
                aria-label="Signature label"
              />
              <button type="submit" className="ghost">
                Save label
              </button>
            </form>
            <NameForm
              placeholder="Move to another appliance (or a new name)"
              button="Move"
              onSave={async (name) => {
                await api(`${sigUrl}/move`, { method: 'POST', body: { name } });
                done();
              }}
            />
            <div className="row">
              <button
                className="ghost danger"
                onClick={async () => {
                  await api(sigUrl, { method: 'DELETE' });
                  done();
                }}
              >
                Remove from {a.appliance}
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

function Timeline({ date, activities, onName, onChanged }) {
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
        sessions: [],
        energy: 0,
      };
      row.spans.push(...a.spans.map((sp) => ({ s: [sp.s, sp.e], sp, a })));
      // A session: repeated runs close together (a heater cycling on its thermostat).
      if (a.runs > 1) row.sessions.push([a.start, a.end ?? Date.now()]);
      row.energy += a.energyWh;
      byRow.set(key, row);
    }
    return [...byRow.values()].sort((x, y) => (x.named === y.named ? y.energy - x.energy : x.named ? -1 : 1));
  }, [activities]);

  const t0 = new Date(`${date}T00:00:00`).getTime();
  const pct = (t) => `${Math.min(100, Math.max(0, ((t - t0) / 86_400_000) * 100))}%`;
  // The run in row r closest in time to the pointer.
  const nearest = (r, e) => {
    const box = e.currentTarget.getBoundingClientRect();
    const t = t0 + ((e.clientX - box.left) / box.width) * 86_400_000;
    let best = null;
    let bestDist = Infinity;
    for (const sp of r.spans) {
      const dist = t < sp.s[0] ? sp.s[0] - t : t > sp.s[1] ? t - sp.s[1] : 0;
      if (dist < bestDist) [best, bestDist] = [sp, dist];
    }
    return best && { r, ...best };
  };
  const latest = (r) => {
    const sp = r.spans.reduce((x, y) => (y.s[0] > x.s[0] ? y : x));
    return { r, ...sp };
  };
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
          <button
            className="tl-label"
            title={`${r.label} - click to select its latest run`}
            onClick={() => setSelected(latest(r))}
          >
            <span className="rect-key" style={{ background: r.type.color }} />
            <span className={r.named ? '' : 'muted'}>{r.label}</span>
          </button>
          {/* The whole track is the hit target: runs are often only a few pixels wide. */}
          <div
            className="tl-track"
            onPointerMove={(e) => setHover(nearest(r, e))}
            onPointerLeave={() => setHover(null)}
            onClick={(e) => setSelected(nearest(r, e))}
          >
            {r.sessions.map(([s0, s1], i) => (
              <span
                key={`s${i}`}
                className="tl-session"
                style={{
                  left: pct(s0),
                  width: `max(2px, calc(${pct(s1)} - ${pct(s0)}))`,
                  background: r.type.color,
                }}
              />
            ))}
            {r.spans.map(({ s, sp, a }, i) => (
              <span
                key={i}
                className="tl-bar"
                tabIndex={0}
                style={{
                  left: pct(s[0]),
                  width: `max(2px, calc(${pct(s[1])} - ${pct(s[0])}))`,
                  background: r.type.color,
                }}
                onFocus={() => setHover({ r, s, sp, a })}
                onBlur={() => setHover(null)}
                onKeyDown={(e) => e.key === 'Enter' && setSelected({ r, s, sp, a })}
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
              {hover.sp.label ? ` · ${hover.sp.label}` : ''} · {sigText(hover.sp.desc)}
              {hover.sp.startedBefore ? ' · already on when monitoring started' : ''}
            </span>
          </>
        ) : (
          <span className="muted">Hover a bar for details, click it to name it.</span>
        )}
      </div>
      {selected && (
        <Selected
          key={`${selected.r.key}-${selected.s[0]}`}
          sel={selected}
          onName={onName}
          onChanged={onChanged}
          onClose={() => setSelected(null)}
        />
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

const describeCluster = (c) =>
  `${fmtW(c.signature.powerW)} ${c.signature.legs.replace('120 V · ', '')} ${c.signature.character}`;

// Unnamed signatures that keep appearing together: probably one appliance.
function Groups({ groups, byKey, onChanged }) {
  const usable = (groups ?? []).map((g) => g.keys.map((k) => byKey[k]).filter(Boolean)).filter((m) => m.length >= 2);
  if (!usable.length) return null;
  const post = (body) => api('/appliances', { method: 'POST', body });
  return (
    <div className="groups">
      <h3>Probably one appliance</h3>
      <p className="muted small">These signatures keep appearing together. Name them in one go.</p>
      {usable.map((members) => (
        <div className="cluster group" key={members.map((m) => m.key).join('+')}>
          <ul className="group-members">
            {members.map((m) => (
              <li key={m.key}>
                <span className="rect-key" style={{ background: typeOf(m.signature.legs).color }} />
                <strong>{fmtW(m.signature.powerW)}</strong>{' '}
                <span className="muted small">
                  {m.signature.legs} · {m.signature.character} · {m.runs} {m.runs === 1 ? 'run' : 'runs'} · ID {m.key}
                </span>
              </li>
            ))}
          </ul>
          <div className="row name-row">
            <NameForm
              button="Name all"
              onSave={async (name) => {
                for (const m of members) await post({ name, dp: m.signature.dp, dq: m.signature.dq });
                onChanged();
              }}
            />
            <button
              className="ghost"
              onClick={async () => {
                for (const m of members) await post({ ignore: true, dp: m.signature.dp, dq: m.signature.dq });
                onChanged();
              }}
            >
              Ignore all
            </button>
          </div>
        </div>
      ))}
    </div>
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
              <span className="muted small">
                {c.ai.source === 'gemini' ? 'Gemini' : 'Claude'}
                {c.ai.model ? ` (${c.ai.model})` : ''} suggests:
              </span>
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
          {c.pattern?.text && (
            <div className="small pattern">
              {c.pattern.kind && <span className={`kind ${c.pattern.kind}`}>{c.pattern.kind}</span>} {c.pattern.text}
            </div>
          )}
          {c.related?.length > 0 && (
            <ul className="small co-start">
              {c.related.slice(0, 4).map((r) => (
                <li key={r.to}>
                  {r.how === 'runs during' ? 'Runs during' : 'Starts with'}{' '}
                  {r.named ? (
                    <strong>{r.to}</strong>
                  ) : (
                    <>
                      the <strong>{r.label}</strong> (ID {r.to})
                    </>
                  )}{' '}
                  in {r.runs} of {r.of} runs
                </li>
              ))}
              {c.related.some((r) => r.named) && <li className="muted">Probably part of it: give it the same name.</li>}
            </ul>
          )}
          <div className="muted small">
            e.g.{' '}
            {c.examples
              .map((t) => new Date(t).toLocaleString([], { weekday: 'short', hour: 'numeric', minute: '2-digit' }))
              .join(', ')}
          </div>
          <div className="row name-row">
            <NameForm
              onSave={async (name) => {
                await api('/appliances', { method: 'POST', body: { name, dp: c.signature.dp, dq: c.signature.dq } });
                onChanged();
              }}
            />
            <button
              className="ghost"
              title="Hide this signature from the timeline and this list"
              onClick={async () => {
                await api('/appliances', {
                  method: 'POST',
                  body: { ignore: true, dp: c.signature.dp, dq: c.signature.dq },
                });
                onChanged();
              }}
            >
              Ignore
            </button>
          </div>
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
  const [open, setOpen] = useState(!household);
  const [justSaved, setJustSaved] = useState(false);

  async function saveHousehold() {
    await api('/household', { method: 'PUT', body: { household: text } });
    setSaved(text);
    setJustSaved(true);
    setTimeout(() => setJustSaved(false), 3000);
  }
  async function onSave() {
    setError(null);
    try {
      await saveHousehold();
      setOpen(false);
      onChanged();
    } catch (e) {
      setError(e.message);
    }
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
  const [elapsed, setElapsed] = useState(0);
  const askApi = (p) =>
    run(async () => {
      setAsking(p.id);
      setElapsed(0);
      const started = Date.now();
      const timer = setInterval(() => setElapsed(Math.round((Date.now() - started) / 1000)), 1000);
      try {
        const res = await api(`/appliances/ask-ai?provider=${p.id}`, { method: 'POST' });
        // The response maps signature IDs to suggestions; count the ones from this request.
        const fresh = Object.values(res).filter((x) => x.at >= started - 1000);
        const model = fresh[0]?.model;
        setNote(
          fresh.length
            ? `${p.name}${model ? ` (${model})` : ''} added suggestions to ${fresh.length} ${
                fresh.length === 1 ? 'signature' : 'signatures'
              }. They're shown on each card below.`
            : `${p.name} answered, but had no suggestions.`,
        );
        onChanged();
      } finally {
        clearInterval(timer);
        setAsking(null);
      }
    });

  return (
    <div className="ask-ai">
      <details open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
        <summary>
          About your home{' '}
          {justSaved ? (
            <span className="small saved">✓ Saved</span>
          ) : saved ? (
            <span className="muted small">(saved)</span>
          ) : null}
        </summary>
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
          <button className="ghost" disabled={text === saved} onClick={onSave}>
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
            {asking === p.id
              ? `Asking ${p.name}… ${Math.floor(elapsed / 60)}:${String(elapsed % 60).padStart(2, '0')}`
              : `Ask ${p.name}`}
          </button>
        ))}
      </div>
      <p className="muted small">
        The prompt holds only summary numbers per signature and the text above, no readings or location.
        {providers.length ? ' The Ask buttons show the answers right here.' : ''}
      </p>
      {asking && (
        <p className="muted small">
          Free models are often busy and can take 1–2 minutes; the dashboard tries the next model when one doesn't
          answer.
        </p>
      )}
      {note && <p className="small ai-note">{note}</p>}
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

const CIRCUITS = { 120: '120 V', 240: '240 V', '240+': '240 V + motor' };
const normInventory = (inv) => ({ catalog: inv?.catalog ?? {}, custom: inv?.custom ?? [] });

// The owner's appliance list. "Yes" ranks an appliance first in the suggestions, "No" hides
// it; unmarked ones are still suggested, so loads missing from the list can show up.
function Inventory({ catalog = [], inventory, onChanged }) {
  const initial = normInventory(inventory);
  const [inv, setInv] = useState(initial);
  const [saved, setSaved] = useState(JSON.stringify(initial));
  const [error, setError] = useState(null);
  const [justSaved, setJustSaved] = useState(false);
  const dirty = JSON.stringify(inv) !== saved;
  const marked = Object.keys(inv.catalog ?? {}).length + (inv.custom?.length ?? 0);

  const mark = (name, v) => {
    const next = { ...inv.catalog };
    if (v) next[name] = v;
    else delete next[name];
    setInv({ ...inv, catalog: next });
  };
  const setCustom = (i, patch) =>
    setInv({ ...inv, custom: inv.custom.map((c, j) => (j === i ? { ...c, ...patch } : c)) });
  async function save() {
    setError(null);
    try {
      const custom = inv.custom.map((c) => ({ ...c, minW: Number(c.minW), maxW: Number(c.maxW) }));
      const res = await api('/inventory', { method: 'PUT', body: { ...inv, custom } });
      setInv(normInventory(res));
      setSaved(JSON.stringify(normInventory(res)));
      setJustSaved(true);
      setTimeout(() => setJustSaved(false), 3000);
      onChanged();
    } catch (e) {
      setError(e.message);
    }
  }

  return (
    <details className="inventory">
      <summary>
        Your appliances{' '}
        {justSaved ? (
          <span className="small saved">✓ Saved</span>
        ) : marked ? (
          <span className="muted small">({marked} listed)</span>
        ) : null}
      </summary>
      <p className="muted small">
        Mark what your home has. Suggestions offer your appliances first and never offer the ones marked No. Unmarked
        ones are still suggested, so a load you forgot or only plug in now and then can still be named.
      </p>
      <ul className="inv-list">
        {catalog.map((c) => (
          <li key={c.name}>
            <span>
              {c.name}{' '}
              <span className="muted small">
                {c.circuits.map((k) => CIRCUITS[k]).join(' or ')} · {fmtW(c.minW)}–{fmtW(c.maxW)}
              </span>
            </span>
            <select
              aria-label={`Do you have: ${c.name}?`}
              value={inv.catalog?.[c.name] ?? ''}
              onChange={(e) => mark(c.name, e.target.value)}
            >
              <option value="">Not sure</option>
              <option value="yes">Yes</option>
              <option value="no">No</option>
            </select>
          </li>
        ))}
      </ul>
      <p className="small">Not in the list (e.g. a pressure washer):</p>
      {inv.custom.map((c, i) => (
        <div className="row inv-custom" key={i}>
          <input
            aria-label="Name"
            placeholder="Name"
            value={c.name}
            onChange={(e) => setCustom(i, { name: e.target.value })}
          />
          <select aria-label="Circuit" value={c.circuit} onChange={(e) => setCustom(i, { circuit: e.target.value })}>
            {Object.entries(CIRCUITS).map(([k, l]) => (
              <option key={k} value={k}>
                {l}
              </option>
            ))}
          </select>
          <input
            aria-label="Minimum watts"
            type="number"
            min="1"
            value={c.minW}
            onChange={(e) => setCustom(i, { minW: e.target.value })}
          />
          –
          <input
            aria-label="Maximum watts"
            type="number"
            min="1"
            value={c.maxW}
            onChange={(e) => setCustom(i, { maxW: e.target.value })}
          />
          W
          <button
            className="ghost link"
            title="Remove"
            onClick={() => setInv({ ...inv, custom: inv.custom.filter((_, j) => j !== i) })}
          >
            ×
          </button>
        </div>
      ))}
      <div className="row">
        <button
          className="ghost"
          onClick={() => setInv({ ...inv, custom: [...inv.custom, { name: '', circuit: '120', minW: '', maxW: '' }] })}
        >
          + Add appliance
        </button>
        <button disabled={!dirty} onClick={save}>
          Save
        </button>
      </div>
      {error && <p className="error">{error}</p>}
    </details>
  );
}

function SigLabel({ appliance, n, onChanged }) {
  const current = appliance.signatures?.[n]?.label ?? '';
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState(current);
  if (!editing)
    return (
      <button className="ghost link sig-label" title="Label this signature" onClick={() => setEditing(true)}>
        {current || '+ label'}
      </button>
    );
  return (
    <form
      className="row"
      onSubmit={async (e) => {
        e.preventDefault();
        await api(`/appliances/${appliance.id}/signatures/${n}`, { method: 'PUT', body: { label: text } });
        setEditing(false);
        onChanged();
      }}
    >
      <input className="sig-label-input" autoFocus value={text} onChange={(e) => setText(e.target.value)} />
      <button type="submit" className="ghost">
        Save
      </button>
    </form>
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
              ) : a.ignored ? (
                <>
                  <strong>Ignored</strong>
                  <div className="muted small">Hidden from the timeline and the naming list</div>
                </>
              ) : (
                <strong>{a.name}</strong>
              )}
            </td>
            <td>
              <ul className="sig-list">
                {(a.described ?? []).map((d, n) => (
                  <li key={n}>
                    <span className="rect-key" style={{ background: typeOf(d.legs).color }} />
                    {!a.ignored && <SigLabel appliance={a} n={n} onChanged={onChanged} />}
                    <span className="muted small">{sigText(d)}</span>
                    {(a.described.length > 1 || a.ignored) && (
                      <button
                        className="ghost link"
                        title={a.ignored ? 'Show this signature again' : 'Remove this signature from the appliance'}
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
                {!a.ignored && (
                  <button className="ghost" onClick={() => setEditing(editing === a.id ? null : a.id)}>
                    {editing === a.id ? 'Cancel' : 'Rename'}
                  </button>
                )}
                <button
                  className="ghost danger"
                  onClick={async () => {
                    await api(`/appliances/${a.id}`, { method: 'DELETE' });
                    onChanged();
                  }}
                >
                  {a.ignored ? 'Show all again' : 'Forget'}
                </button>
              </div>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// Run the detector again over the day's recorded raw readings (kept for 7 days).
function Redetect({ date, onDone }) {
  const [state, setState] = useState(null);
  useEffect(() => setState(null), [date]);
  return (
    <p className="muted small redetect">
      <button
        className="ghost"
        disabled={state === 'busy'}
        onClick={async () => {
          setState('busy');
          try {
            const r = await api(`/appliances/redetect?date=${date}`, { method: 'POST' });
            setState(`Found ${r.runs} runs.`);
            onDone();
          } catch (e) {
            setState(e.message);
          }
        }}
      >
        {state === 'busy' ? 'Detecting…' : 'Re-detect this day'}
      </button>
      {state && state !== 'busy' ? state : 'Runs the detector again over the recorded readings (last 7 days).'}
    </p>
  );
}

// Per-leg power over the teach window, with the switch times of a candidate marked.
function LegChart({ samples, from, to, marks = [] }) {
  if (!samples?.length) return <div className="teach-chart muted small">Waiting for readings…</div>;
  const legs = (samples[0].length - 1) / 2;
  let max = 100;
  for (const row of samples) for (let i = 1; i <= legs; i++) max = Math.max(max, row[i]);
  const x = (t) => ((t - from) / Math.max(1, to - from)) * 600;
  const y = (w) => 116 - (Math.max(0, w) / max) * 110;
  const colors = ['var(--series-2)', 'var(--series-3)', 'var(--series-1)'];
  return (
    <div className="teach-chart">
      <svg viewBox="0 0 600 120" preserveAspectRatio="none" role="img" aria-label="Power per leg">
        {marks.map((t) => (
          <line key={t} x1={x(t)} x2={x(t)} y1="0" y2="120" className="teach-mark" />
        ))}
        {Array.from({ length: legs }, (_, i) => (
          <polyline
            key={i}
            fill="none"
            stroke={colors[i % colors.length]}
            strokeWidth="1.5"
            vectorEffect="non-scaling-stroke"
            points={samples.map((r) => `${x(r[0]).toFixed(1)},${y(r[1 + i]).toFixed(1)}`).join(' ')}
          />
        ))}
      </svg>
      <div className="row small muted teach-legend">
        {Array.from({ length: legs }, (_, i) => (
          <span key={i} className="legend-item">
            <span className="rect-key" style={{ background: colors[i % colors.length] }} />
            Leg {i + 1}: {fmtW(samples[samples.length - 1][1 + i])}
          </span>
        ))}
        <span>scale to {fmtW(max)}</span>
      </div>
    </div>
  );
}

const TEACH_MAX_MS = 30 * 60_000;

// Teach mode: the owner switches one appliance on and off a few times; the signature that
// switched most often in that window is offered as the appliance's.
function Teach({ onChanged }) {
  const [name, setName] = useState('');
  const [phase, setPhase] = useState('idle'); // idle | recording | analyzing | results
  const [win, setWin] = useState(null); // { from, to }
  const [samples, setSamples] = useState(null);
  const [found, setFound] = useState([]);
  const [pick, setPick] = useState(0);
  const [saved, setSaved] = useState([]);
  const [error, setError] = useState(null);
  const [now, setNow] = useState(Date.now());

  const loadSamples = async (from, to) => {
    const r = await api(`/debug/raw?from=${from}&to=${to}`);
    setSamples(r.samples);
  };
  const stop = async (from) => {
    const to = Date.now();
    setWin({ from, to });
    setPhase('analyzing');
    try {
      await loadSamples(from, to);
      const r = await api('/appliances/teach', { method: 'POST', body: { from, to } });
      setFound(r.candidates);
      setPick(0);
      setSaved([]);
      setPhase('results');
    } catch (e) {
      setError(e.message);
      setPhase('idle');
    }
  };

  useEffect(() => {
    if (phase !== 'recording') return;
    const tick = async () => {
      const t = Date.now();
      setNow(t);
      if (t - win.from >= TEACH_MAX_MS) return stop(win.from);
      try {
        await loadSamples(win.from - 30_000, t);
      } catch {
        // keep the last chart
      }
    };
    tick();
    const id = setInterval(tick, 2000);
    return () => clearInterval(id);
  }, [phase, win]);

  const start = () => {
    setError(null);
    setSamples(null);
    setWin({ from: Date.now() });
    setPhase('recording');
  };
  const save = async (c) => {
    try {
      await api('/appliances', { method: 'POST', body: { name: name.trim(), dp: c.signature.dp, dq: c.signature.dq } });
      setSaved((s) => [...s, c]);
      onChanged();
    } catch (e) {
      setError(e.message);
    }
  };
  const reset = () => {
    setPhase('idle');
    setFound([]);
    setSamples(null);
  };

  return (
    <section className="card teach">
      <h2>Teach an appliance</h2>
      {phase === 'idle' && (
        <>
          <p className="muted small">
            Name the appliance and press Start. Then switch it on, wait about 10 seconds, switch it off, and do that
            two or three times. Press Stop when done. Its signature is the step that switches in time with you; loads
            that cycle on their own don’t. Works best while the house is quiet, and for a washer or dryer you can record a
            whole program (up to 30 minutes).
          </p>
          <form
            className="row name-form"
            onSubmit={(e) => {
              e.preventDefault();
              if (name.trim()) start();
            }}
          >
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Appliance, e.g. Microwave"
              aria-label="Appliance to teach"
              list="appliance-names"
            />
            <button type="submit" disabled={!name.trim()}>
              Start
            </button>
          </form>
        </>
      )}
      {phase === 'recording' && (
        <>
          <p>
            <strong>Recording {name.trim()}</strong> <span className="muted">· {fmtDur((now - win.from) / 1000)}</span>
          </p>
          <p className="muted small">Switch it on, wait, switch it off. Repeat two or three times, then press Stop.</p>
          <LegChart samples={samples} from={win.from - 30_000} to={now} marks={[win.from]} />
          <div className="row">
            <button onClick={() => stop(win.from)}>Stop</button>
            <button className="ghost" onClick={reset}>
              Cancel
            </button>
          </div>
        </>
      )}
      {phase === 'analyzing' && <p className="muted">Looking for the steps…</p>}
      {phase === 'results' && (
        <>
          <LegChart samples={samples} from={win.from} to={win.to} marks={found[pick]?.times ?? []} />
          {found.length === 0 ? (
            <p className="muted">
              No steps of 60 W or more in that time. Was the appliance on a different circuit, or too small to see?
            </p>
          ) : (
            <>
              <p className="muted small">
                Most switches first. Select one to see when it switched (lines on the chart), then add it to{' '}
                <strong>{name.trim()}</strong>. Add several for an appliance with stages, e.g. heater and motor.
              </p>
              <ul className="teach-list">
                {found.map((c, i) => {
                  const done = saved.includes(c);
                  return (
                    <li key={i} className={i === pick ? 'picked' : ''}>
                      <button className="ghost teach-pick" onClick={() => setPick(i)} aria-pressed={i === pick}>
                        <span className="rect-key" style={{ background: typeOf(c.signature.legs).color }} />
                        {sigText(c.signature)}
                        <span className="muted small">
                          {' '}
                          · switched on {c.ons}×, off {c.offs}×
                          {c.appliance ? ` · already named ${c.appliance}` : ''}
                        </span>
                      </button>
                      <button disabled={done} className={i === 0 ? '' : 'ghost'} onClick={() => save(c)}>
                        {done ? 'Added' : `Add to ${name.trim()}`}
                      </button>
                    </li>
                  );
                })}
              </ul>
            </>
          )}
          <div className="row">
            <button className="ghost" onClick={start}>
              Record again
            </button>
            <button className="ghost" onClick={reset}>
              Done
            </button>
          </div>
        </>
      )}
      {error && <p className="error">{error}</p>}
    </section>
  );
}

function CleanSlate({ onDone }) {
  const [asking, setAsking] = useState(false);
  const [state, setState] = useState(null);
  if (!asking)
    return (
      <p className="muted small clean-slate">
        <button className="ghost danger" onClick={() => setAsking(true)}>
          Clean slate…
        </button>
        {state ?? 'Forget all names and recorded runs, and start learning from scratch.'}
      </p>
    );
  return (
    <div className="clean-slate-confirm">
      <p>
        <strong>Start over?</strong> This removes every named appliance, signature label and ignored signature, the AI
        suggestions, and all recorded runs. Your appliance list, household notes and the last 7 days of per-second
        readings stay, so you can re-detect recent days afterwards. The old files are moved to a backup folder in the
        data directory.
      </p>
      <div className="row">
        <button
          className="danger"
          disabled={state === 'busy'}
          onClick={async () => {
            setState('busy');
            try {
              const r = await api('/appliances/clean-slate', { method: 'POST', body: { confirm: true } });
              setState(`Started over. Backup: ${r.backup}`);
              setAsking(false);
              onDone();
            } catch (e) {
              setState(e.message);
            }
          }}
        >
          {state === 'busy' ? 'Erasing…' : 'Erase names and runs'}
        </button>
        <button className="ghost" onClick={() => setAsking(false)}>
          Cancel
        </button>
      </div>
      {state && state !== 'busy' && <p className="error">{state}</p>}
    </div>
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

  const [minW, setMinW] = useState(loadMinW);
  const chooseMinW = (v) => {
    setMinW(v);
    try {
      localStorage.setItem('appliances.minW', String(v));
    } catch {
      // not persisted
    }
  };
  const [hideOnce, setHideOnce] = useState(loadHideOnce);
  const chooseHideOnce = (v) => {
    setHideOnce(v);
    try {
      localStorage.setItem('appliances.hideOnce', String(v));
    } catch {
      // not persisted
    }
  };
  const [acts, hiddenRuns] = filterSmall((day.data?.date === date && day.data.activities) || [], minW, hideOnce);
  const allClusters = lists.data?.unnamed ?? [];
  const clusters = allClusters.filter(
    (c) => c.signature.powerW >= minW && !(hideOnce && isOnce(c.runs, c.signature)),
  );
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
        <div className="row size-filter">
          <label className="small">
            Show unnamed loads from{' '}
            <select value={minW} onChange={(e) => chooseMinW(Number(e.target.value))}>
              {SIZES.map((v) => (
                <option key={v} value={v}>
                  {fmtW(v)}
                </option>
              ))}
            </select>
          </label>
          <label className="small">
            <input type="checkbox" checked={hideOnce} onChange={(e) => chooseHideOnce(e.target.checked)} /> Hide
            one-time loads under {fmtW(ONCE_MAX_W)}
          </label>
          {hiddenRuns > 0 && (
            <span className="muted small">
              {hiddenRuns} {hiddenRuns === 1 ? 'run' : 'runs'} hidden
            </span>
          )}
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
        <Timeline date={date} activities={acts} onName={nameSignature} onChanged={reload} />
        <Redetect date={date} onDone={reload} />
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

      <Teach onChanged={reload} />

      <div className="two-col">
        <section className="card">
          <h2>Needs a name</h2>
          <p className="muted small">
            Signatures from the last 14 days, most energy first. Naming one labels all of its runs, past and future.
            Give several signatures the same name to group them into one appliance.
          </p>
          {lists.data && (
            <>
              <AskAI providers={lists.data.aiProviders} household={lists.data.household} onChanged={reload} />
              <Inventory catalog={lists.data.catalog} inventory={lists.data.inventory} onChanged={reload} />
            </>
          )}
          <Groups
            groups={lists.data?.groups}
            byKey={Object.fromEntries(allClusters.map((c) => [c.key, c]))}
            onChanged={reload}
          />
          <Unnamed clusters={clusters} onChanged={reload} />
          {allClusters.length > clusters.length && (
            <p className="muted small">
              {allClusters.length - clusters.length}{' '}
              {allClusters.length - clusters.length === 1 ? 'signature' : 'signatures'} hidden (under {fmtW(minW)}
              {hideOnce ? ', or seen only once' : ''}).
            </p>
          )}
        </section>
        <section className="card">
          <h2>Known appliances</h2>
          <Known appliances={lists.data?.appliances} onChanged={reload} />
          <CleanSlate onDone={reload} />
        </section>
      </div>
    </>
  );
}
