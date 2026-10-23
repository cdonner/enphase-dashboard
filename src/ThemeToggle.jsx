import { useEffect, useState } from 'react';

// Auto follows the device setting; Light/Dark override it (styles.css reads data-theme).
const MODES = ['auto', 'light', 'dark'];
const LABELS = { auto: 'Theme: auto', light: 'Theme: light', dark: 'Theme: dark' };

function load() {
  try {
    return MODES.includes(localStorage.getItem('theme')) ? localStorage.getItem('theme') : 'auto';
  } catch {
    return 'auto';
  }
}

export default function ThemeToggle() {
  const [mode, setMode] = useState(load);
  useEffect(() => {
    const root = document.documentElement;
    if (mode === 'auto') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', mode);
    try {
      localStorage.setItem('theme', mode);
    } catch {
      // private mode etc. - the choice just won't persist
    }
  }, [mode]);
  return (
    <button className="ghost" onClick={() => setMode((m) => MODES[(MODES.indexOf(m) + 1) % MODES.length])}>
      {LABELS[mode]}
    </button>
  );
}
