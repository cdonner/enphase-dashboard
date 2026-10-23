export function fmtW(w) {
  if (w == null || Number.isNaN(w)) return '–';
  const a = Math.abs(w);
  if (a >= 1000) return `${(w / 1000).toFixed(a >= 10000 ? 1 : 2)} kW`;
  return `${Math.round(w)} W`;
}

export function fmtWh(wh) {
  if (wh == null || Number.isNaN(wh)) return '–';
  const a = Math.abs(wh);
  if (a >= 1_000_000) return `${(wh / 1_000_000).toFixed(2)} MWh`;
  if (a >= 1000) return `${(wh / 1000).toFixed(a >= 100_000 ? 0 : 1)} kWh`;
  return `${Math.round(wh)} Wh`;
}

// Axis ticks: 0 / 500 / 1K / 1.5K ...
export function fmtAxisW(w) {
  if (w === 0) return '0';
  return Math.abs(w) >= 1000 ? `${+(w / 1000).toFixed(1)}K` : String(w);
}

export function fmtAxisWh(wh) {
  if (wh === 0) return '0';
  return Math.abs(wh) >= 1000 ? `${+(wh / 1000).toFixed(1)} kWh` : `${wh} Wh`;
}

export const fmtTime = (t) => new Date(t).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });

export const fmtDay = (key) =>
  new Date(`${key}T12:00:00`).toLocaleDateString([], { month: 'short', day: 'numeric' });

export function fmtAgo(t, now = Date.now()) {
  if (!t) return 'never';
  const s = Math.round((now - t) / 1000);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  return `${Math.round(s / 3600)} h ago`;
}

export function fmtExpiry(t, now = Date.now()) {
  if (!t) return 'no expiry';
  const ms = t - now;
  if (ms <= 0) return 'expired';
  const h = ms / 3_600_000;
  if (h < 1) return `expires in ${Math.round(ms / 60000)} min`;
  if (h < 48) return `expires in ${Math.round(h)} h`;
  return `expires ${new Date(t).toLocaleDateString()}`;
}

// "Nice" upper bound and ticks for a 0-based axis.
export function niceTicks(max, count = 4) {
  if (!(max > 0)) return { max: 1000, ticks: [0, 250, 500, 750, 1000] };
  const raw = max / count;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw);
  const top = Math.ceil(max / step) * step;
  const ticks = [];
  for (let v = 0; v <= top + step / 2; v += step) ticks.push(+v.toFixed(6));
  return { max: top, ticks };
}
