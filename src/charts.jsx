import { useEffect, useMemo, useRef, useState } from 'react';
import { fmtAxisW, fmtAxisWh, fmtDay, fmtTime, fmtW, fmtWh, niceTicks } from './format.js';

const GAP_MS = 30 * 60_000; // break lines across gaps (backfilled points are 15 min apart)

function useWidth() {
  const ref = useRef(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const ro = new ResizeObserver(([e]) => setWidth(e.contentRect.width));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  return [ref, width];
}

// Column with a 4px rounded data end, square at the baseline.
function barPath(x, y, w, h, r = 4) {
  if (h <= 0) return '';
  r = Math.min(r, w / 2, h);
  return `M${x},${y + h}V${y + r}Q${x},${y} ${x + r},${y}H${x + w - r}Q${x + w},${y} ${x + w},${y + r}V${y + h}Z`;
}

function Tooltip({ x, y, width, title, rows }) {
  if (x == null) return null;
  const flip = x > width - 180;
  return (
    <div className="tooltip" style={{ left: flip ? undefined : x + 12, right: flip ? width - x + 12 : undefined, top: y }}>
      <div className="tooltip-title">{title}</div>
      {rows.map((r) => (
        <div className="tooltip-row" key={r.label}>
          {r.color && <span className="line-key" style={{ background: r.color }} />}
          <strong>{r.value}</strong>
          <span className="muted">{r.label}</span>
        </div>
      ))}
    </div>
  );
}

export function Legend({ items }) {
  return (
    <div className="legend">
      {items.map((i) => (
        <span key={i.label} className="legend-item">
          <span className={i.shape === 'rect' ? 'rect-key' : 'line-key'} style={{ background: i.color }} />
          {i.label}
        </span>
      ))}
    </div>
  );
}

function YAxis({ ticks, y, x0, x1, format = fmtAxisW }) {
  return ticks.map((t) => (
    <g key={t}>
      <line x1={x0} x2={x1} y1={y(t)} y2={y(t)} className={t === 0 ? 'baseline' : 'grid'} />
      <text x={x0 - 8} y={y(t)} className="axis-label" textAnchor="end" dominantBaseline="middle">
        {format(t)}
      </text>
    </g>
  ));
}

// ---------- Today's power (lines) ----------

const POWER_SERIES = [
  { key: 1, label: 'Production', color: 'var(--series-1)' },
  { key: 2, label: 'Consumption', color: 'var(--series-2)' },
];

export function PowerChart({ points = [], date }) {
  const [ref, width] = useWidth();
  const [hover, setHover] = useState(null);
  const H = 280;
  const m = { top: 16, right: width < 600 ? 60 : 76, bottom: 28, left: 44 };
  const iw = Math.max(0, width - m.left - m.right);
  const ih = H - m.top - m.bottom;

  const t0 = useMemo(() => (date ? new Date(`${date}T00:00:00`).getTime() : 0), [date]);
  const t1 = t0 + 86_400_000;
  const { max, ticks } = niceTicks(Math.max(0, ...points.flatMap((p) => [p[1] ?? 0, p[2] ?? 0])));
  const x = (t) => m.left + ((t - t0) / (t1 - t0)) * iw;
  const y = (v) => m.top + ih - (v / max) * ih;

  const paths = useMemo(() => {
    return POWER_SERIES.map((s) => {
      let line = '';
      let area = '';
      let seg = [];
      const flush = () => {
        if (seg.length) {
          line += seg.map((p, i) => `${i ? 'L' : 'M'}${x(p[0])},${y(p[s.key])}`).join('');
          area += `M${x(seg[0][0])},${y(0)}` + seg.map((p) => `L${x(p[0])},${y(p[s.key])}`).join('') + `L${x(seg[seg.length - 1][0])},${y(0)}Z`;
        }
        seg = [];
      };
      points.forEach((p, i) => {
        if (p[s.key] == null || (i && p[0] - points[i - 1][0] > GAP_MS)) flush();
        if (p[s.key] != null) seg.push(p);
      });
      flush();
      return { ...s, line, area };
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [points, width, max, t0]);

  const hourStep = iw < 480 ? 6 : 3;
  const hourTicks = Array.from({ length: 24 / hourStep + 1 }, (_, i) => t0 + i * hourStep * 3_600_000);
  const last = points[points.length - 1];

  function pick(clientX) {
    if (!points.length) return;
    const rect = ref.current.getBoundingClientRect();
    const t = t0 + ((clientX - rect.left - m.left) / iw) * (t1 - t0);
    let lo = 0;
    let hi = points.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (points[mid][0] < t) lo = mid + 1;
      else hi = mid;
    }
    if (lo > 0 && t - points[lo - 1][0] < points[lo][0] - t) lo--;
    setHover(lo);
  }

  function onKey(e) {
    if (!points.length) return;
    if (e.key === 'ArrowLeft') setHover((h) => Math.max(0, (h ?? points.length) - 1));
    else if (e.key === 'ArrowRight') setHover((h) => Math.min(points.length - 1, (h ?? -1) + 1));
    else return;
    e.preventDefault();
  }

  const hp = hover != null ? points[hover] : null;

  // End labels only when the two line ends are far enough apart not to collide.
  const endLabels =
    last && last[1] != null && last[2] != null && Math.abs(y(last[1]) - y(last[2])) >= 16
      ? POWER_SERIES.map((s) => ({ ...s, v: last[s.key] }))
      : [];

  return (
    <div className="chart" ref={ref}>
      {width > 0 && (
        <svg
          width={width}
          height={H}
          role="img"
          aria-label="Production and consumption power today"
          tabIndex={0}
          onKeyDown={onKey}
          onBlur={() => setHover(null)}
        >
          <YAxis ticks={ticks} y={y} x0={m.left} x1={m.left + iw} />
          {hourTicks.map((t, i) => (
            <text key={t} x={x(t)} y={H - 8} className="axis-label" textAnchor={i === 0 ? 'start' : i === hourTicks.length - 1 ? 'end' : 'middle'}>
              {new Date(t).toLocaleTimeString([], { hour: 'numeric' })}
            </text>
          ))}
          {paths.map((p) => (
            <path key={`a${p.key}`} d={p.area} fill={p.color} opacity={0.1} />
          ))}
          {paths.map((p) => (
            <path key={`l${p.key}`} d={p.line} fill="none" stroke={p.color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
          ))}
          {last &&
            POWER_SERIES.map((s) =>
              last[s.key] == null ? null : (
                <circle key={s.key} cx={x(last[0])} cy={y(last[s.key])} r={4} fill={s.color} className="ring" />
              ),
            )}
          {endLabels.map((s) => (
            <text key={s.key} x={x(last[0]) + 10} y={y(s.v)} className="end-label" dominantBaseline="middle">
              {fmtW(s.v)}
            </text>
          ))}
          {hp && (
            <g>
              <line x1={x(hp[0])} x2={x(hp[0])} y1={m.top} y2={m.top + ih} className="crosshair" />
              {POWER_SERIES.map((s) =>
                hp[s.key] == null ? null : <circle key={s.key} cx={x(hp[0])} cy={y(hp[s.key])} r={4} fill={s.color} className="ring" />,
              )}
            </g>
          )}
          <rect
            x={m.left}
            y={m.top}
            width={iw}
            height={ih}
            fill="transparent"
            onPointerMove={(e) => pick(e.clientX)}
            onPointerLeave={() => setHover(null)}
          />
        </svg>
      )}
      {width > 0 && !points.length && <div className="chart-empty">Collecting data. The first point appears within a minute.</div>}
      {hp && (
        <Tooltip
          x={x(hp[0])}
          y={m.top}
          width={width}
          title={fmtTime(hp[0])}
          rows={[
            ...POWER_SERIES.map((s) => ({ label: s.label, color: s.color, value: fmtW(hp[s.key]) })),
            ...(hp[3] == null ? [] : [{ label: hp[3] >= 0 ? 'Grid import' : 'Grid export', value: fmtW(Math.abs(hp[3])) }]),
          ]}
        />
      )}
    </div>
  );
}

export function PowerTable({ points = [] }) {
  const hours = useMemo(() => {
    const acc = new Map();
    for (const p of points) {
      const h = new Date(p[0]).getHours();
      const a = acc.get(h) ?? { n: 0, prod: 0, cons: 0, nc: 0 };
      a.n++;
      a.prod += p[1] ?? 0;
      if (p[2] != null) {
        a.cons += p[2];
        a.nc++;
      }
      acc.set(h, a);
    }
    return [...acc.entries()].sort((a, b) => a[0] - b[0]);
  }, [points]);
  return (
    <table className="data-table">
      <thead>
        <tr>
          <th>Hour</th>
          <th>Avg production</th>
          <th>Avg consumption</th>
        </tr>
      </thead>
      <tbody>
        {hours.map(([h, a]) => (
          <tr key={h}>
            <td>{`${String(h).padStart(2, '0')}:00`}</td>
            <td>{fmtW(a.prod / a.n)}</td>
            <td>{a.nc ? fmtW(a.cons / a.nc) : '–'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// ---------- Daily energy (grouped columns) ----------

const DAILY_SERIES = [
  { key: 'produced', label: 'Produced', color: 'var(--series-1)' },
  { key: 'consumed', label: 'Consumed', color: 'var(--series-2)' },
];

export function DailyChart({ days = [] }) {
  const [ref, width] = useWidth();
  const [hover, setHover] = useState(null);
  const H = 240;
  const m = { top: 12, right: 8, bottom: 28, left: 60 };
  const iw = Math.max(0, width - m.left - m.right);
  const ih = H - m.top - m.bottom;
  const { max, ticks } = niceTicks(Math.max(0, ...days.flatMap((d) => [d.produced ?? 0, d.consumed ?? 0])));
  const y = (v) => m.top + ih - (v / max) * ih;
  const band = days.length ? iw / days.length : 0;
  const barW = Math.max(2, Math.min(24, (band - 10) / 2));
  const labelEvery = band ? Math.ceil(52 / band) : 1;

  return (
    <div className="chart" ref={ref}>
      {width > 0 && (
        <svg width={width} height={H} role="img" aria-label="Daily energy produced and consumed">
          <YAxis ticks={ticks} y={y} x0={m.left} x1={m.left + iw} format={fmtAxisWh} />
          {days.map((d, i) => {
            const cx = m.left + band * i + band / 2;
            return (
              <g
                key={d.date}
                tabIndex={0}
                onPointerEnter={() => setHover(i)}
                onPointerLeave={() => setHover(null)}
                onFocus={() => setHover(i)}
                onBlur={() => setHover(null)}
              >
                <rect x={m.left + band * i} y={m.top} width={band} height={ih} className={hover === i ? 'hover-band' : 'hit'} />
                {DAILY_SERIES.map((s, j) => {
                  const v = d[s.key];
                  if (v == null) return null;
                  const bx = j === 0 ? cx - barW - 1 : cx + 1; // 2px surface gap between the pair
                  return <path key={s.key} d={barPath(bx, y(v), barW, y(0) - y(v))} fill={s.color} opacity={d.partial ? 0.55 : 1} />;
                })}
                {(days.length - 1 - i) % labelEvery === 0 && (
                  <text x={cx} y={H - 8} className="axis-label" textAnchor="middle">
                    {fmtDay(d.date)}
                  </text>
                )}
              </g>
            );
          })}
        </svg>
      )}
      {hover != null && days[hover] && (
        <Tooltip
          x={m.left + band * hover + band / 2}
          y={m.top}
          width={width}
          title={`${fmtDay(days[hover].date)}${days[hover].partial ? ' (partial)' : ''}`}
          rows={
            days[hover].produced == null
              ? [{ label: 'not recorded', value: 'No data' }]
              : [
                  ...DAILY_SERIES.map((s) => ({ label: s.label, color: s.color, value: fmtWh(days[hover][s.key]) })),
                  { label: 'Imported', value: fmtWh(days[hover].imported) },
                  { label: 'Exported', value: fmtWh(days[hover].exported) },
                ]
          }
        />
      )}
    </div>
  );
}

export function DailyTable({ days = [] }) {
  return (
    <table className="data-table">
      <thead>
        <tr>
          <th>Day</th>
          <th>Produced</th>
          <th>Consumed</th>
          <th>Imported</th>
          <th>Exported</th>
        </tr>
      </thead>
      <tbody>
        {days
          .filter((d) => d.produced != null)
          .map((d) => (
            <tr key={d.date}>
              <td>
                {fmtDay(d.date)}
                {d.partial ? ' *' : ''}
              </td>
              <td>{fmtWh(d.produced)}</td>
              <td>{fmtWh(d.consumed)}</td>
              <td>{fmtWh(d.imported)}</td>
              <td>{fmtWh(d.exported)}</td>
            </tr>
          ))}
      </tbody>
    </table>
  );
}

// ---------- Per-inverter output (columns, single series) ----------

export function InverterChart({ inverters = [] }) {
  const [ref, width] = useWidth();
  const [hover, setHover] = useState(null);
  const H = 240;
  const m = { top: 12, right: 8, bottom: 12, left: 44 };
  const iw = Math.max(0, width - m.left - m.right);
  const ih = H - m.top - m.bottom;
  const sorted = useMemo(() => [...inverters].sort((a, b) => b.lastReportWatts - a.lastReportWatts), [inverters]);
  const cap = Math.max(...inverters.map((i) => i.maxReportWatts), ...inverters.map((i) => i.lastReportWatts), 1);
  const { max, ticks } = niceTicks(cap);
  const y = (v) => m.top + ih - (Math.max(0, v) / max) * ih;
  const band = sorted.length ? iw / sorted.length : 0;
  const barW = Math.max(2, Math.min(24, band - 2));

  const inv = hover != null ? sorted[hover] : null;
  return (
    <div className="chart" ref={ref}>
      {width > 0 && (
        <svg width={width} height={H} role="img" aria-label="Current output per microinverter">
          <YAxis ticks={ticks} y={y} x0={m.left} x1={m.left + iw} />
          {sorted.map((d, i) => {
            const bx = m.left + band * i + (band - barW) / 2;
            return (
              <g
                key={d.serialNumber}
                tabIndex={0}
                onPointerEnter={() => setHover(i)}
                onPointerLeave={() => setHover(null)}
                onFocus={() => setHover(i)}
                onBlur={() => setHover(null)}
              >
                <rect x={m.left + band * i} y={m.top} width={band} height={ih} className={hover === i ? 'hover-band' : 'hit'} />
                <path d={barPath(bx, y(d.lastReportWatts), barW, y(0) - y(d.lastReportWatts))} fill="var(--series-1)" />
              </g>
            );
          })}
        </svg>
      )}
      {inv && (
        <Tooltip
          x={m.left + band * hover + band / 2}
          y={m.top}
          width={width}
          title={`Inverter ${inv.serialNumber}`}
          rows={[
            { label: 'last report', value: fmtW(inv.lastReportWatts) },
            { label: 'max', value: fmtW(inv.maxReportWatts) },
            { label: 'reported', value: fmtTime(inv.lastReportDate * 1000) },
          ]}
        />
      )}
    </div>
  );
}

export function InverterTable({ inverters = [] }) {
  return (
    <table className="data-table">
      <thead>
        <tr>
          <th>Serial</th>
          <th>Output</th>
          <th>Max</th>
          <th>Reported</th>
        </tr>
      </thead>
      <tbody>
        {[...inverters]
          .sort((a, b) => a.serialNumber.localeCompare(b.serialNumber))
          .map((i) => (
            <tr key={i.serialNumber}>
              <td>{i.serialNumber}</td>
              <td>{fmtW(i.lastReportWatts)}</td>
              <td>{fmtW(i.maxReportWatts)}</td>
              <td>{fmtTime(i.lastReportDate * 1000)}</td>
            </tr>
          ))}
      </tbody>
    </table>
  );
}
