import { useCallback, useEffect, useRef, useState } from 'react';

export async function api(path, { method = 'GET', body } = {}) {
  const res = await fetch(`/api${path}`, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(data.error || `HTTP ${res.status}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

// Polls `path` every `ms` while `enabled`. Keeps the previous data during refetch.
export function usePoll(path, ms, enabled = true) {
  const [data, setData] = useState(null);
  const [error, setError] = useState(null);
  const timer = useRef();

  const load = useCallback(async () => {
    try {
      setData(await api(path));
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, [path]);

  useEffect(() => {
    if (!enabled) return;
    load();
    timer.current = setInterval(load, ms);
    return () => clearInterval(timer.current);
  }, [load, ms, enabled]);

  return { data, error, reload: load };
}
