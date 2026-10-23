import { useState } from 'react';
import { api } from './api.js';
import { fmtAgo, fmtExpiry } from './format.js';

function CloudSection({ cloud, onDone }) {
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  if (!cloud?.configured) {
    return (
      <p className="muted small">
        Enphase cloud backfill is off. Set ENPHASE_API_KEY, ENPHASE_CLIENT_ID and ENPHASE_CLIENT_SECRET (see the README) to enable it.
      </p>
    );
  }
  async function connect(e) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api('/cloud/connect', { method: 'POST', body: { code } });
      setCode('');
      onDone();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="token-info">
      <div>
        Enphase cloud: <strong>{cloud.connected ? 'connected' : 'not connected'}</strong>
        {cloud.lastSyncAt ? ` · last sync ${fmtAgo(cloud.lastSyncAt)}` : ''}
      </div>
      <div className="muted small">Used to backfill past days and the part of today before the dashboard started.</div>
      {cloud.error && <p className="error">{cloud.error}</p>}
      {!cloud.connected && (
        <form onSubmit={connect} className="auth-form">
          <p className="muted small">
            <a href={cloud.authorizeUrl} target="_blank" rel="noreferrer">
              Approve access
            </a>{' '}
            with your Enlighten account, then paste the code shown.
          </p>
          <div className="row">
            <input value={code} onChange={(e) => setCode(e.target.value)} placeholder="Code" required />
            <button type="submit" disabled={busy}>
              Connect
            </button>
          </div>
          {error && <p className="error">{error}</p>}
        </form>
      )}
    </div>
  );
}

export default function AuthPanel({ status, onDone, onClose }) {
  const [mode, setMode] = useState('signin');
  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [remember, setRemember] = useState(false);
  const [token, setToken] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);

  const serial = status?.gateway?.serial;
  const current = status?.token;

  async function submit(e) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      if (mode === 'signin') await api('/token/create', { method: 'POST', body: { email, password, remember } });
      else await api('/token', { method: 'POST', body: { token } });
      setPassword('');
      setToken('');
      onDone();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  }

  async function remove(path) {
    setBusy(true);
    try {
      await api(path, { method: 'DELETE' });
      onDone();
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="card auth">
      <div className="auth-head">
        <h2>Gateway access</h2>
        {onClose && (
          <button className="ghost" onClick={onClose}>
            Close
          </button>
        )}
      </div>
      <p className="muted">
        Gateway {serial ?? 'unknown'} at {status?.gatewayHost}
        {status?.gateway?.firmware ? ` · firmware ${status.gateway.firmware}` : ''}
        {status?.gateway?.reachable === false ? ' · unreachable' : ''}
      </p>

      {current?.present && (
        <div className="token-info">
          <div>
            Current token: <strong>{current.role ?? 'unknown'}</strong> · {fmtExpiry(current.expiresAt)}
            {current.serial && serial && current.serial !== serial ? ' · issued for a different gateway' : ''}
          </div>
          <div className="muted">
            {status.autoRenew ? 'Auto-renewal is on (credentials saved locally).' : 'Auto-renewal is off.'}
            {status.renewError ? ` Last renewal failed: ${status.renewError}` : ''}
          </div>
          <div className="row">
            {status.autoRenew && (
              <button className="ghost" disabled={busy} onClick={() => remove('/credentials')}>
                Forget saved credentials
              </button>
            )}
            <button className="ghost danger" disabled={busy} onClick={() => remove('/token')}>
              Remove token
            </button>
          </div>
        </div>
      )}

      <div className="tabs" role="tablist">
        <button role="tab" aria-selected={mode === 'signin'} onClick={() => setMode('signin')}>
          Sign in with Enlighten
        </button>
        <button role="tab" aria-selected={mode === 'paste'} onClick={() => setMode('paste')}>
          Paste a token
        </button>
      </div>

      <form onSubmit={submit} className="auth-form">
        {mode === 'signin' ? (
          <>
            <label>
              Enlighten email
              <input type="email" autoComplete="username" value={email} onChange={(e) => setEmail(e.target.value)} required />
            </label>
            <label>
              Password
              <input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
            </label>
            <label className="check">
              <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
              Remember credentials on this server to renew the token automatically
            </label>
            <p className="muted small">
              The server signs in to Enlighten, requests a token for gateway {serial ?? '…'} from Entrez, and checks it
              against the gateway. Homeowner tokens last about a year; installer tokens last 12 hours, so turn on
              auto-renewal for those.
            </p>
          </>
        ) : (
          <>
            <label>
              Token (JWT)
              <textarea rows={4} value={token} onChange={(e) => setToken(e.target.value)} placeholder="eyJ…" required />
            </label>
            <p className="muted small">
              Get one at entrez.enphaseenergy.com, or sign in to Enlighten and open
              /entrez-auth-token?serial_num={serial ?? 'SERIAL'}.
            </p>
          </>
        )}
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <button type="submit" disabled={busy}>
          {busy ? 'Working…' : mode === 'signin' ? 'Create token' : 'Save token'}
        </button>
      </form>

      <h2 className="section-gap">Cloud backfill</h2>
      <CloudSection cloud={status?.cloud} onDone={onDone} />

      <h2 className="section-gap">Home Assistant (MQTT)</h2>
      {status?.mqtt?.enabled ? (
        <p className="small">
          {status.mqtt.connected ? '● Connected' : '⚠ Not connected'} to {status.mqtt.broker}
          {status.mqtt.error ? ` · ${status.mqtt.error}` : ''}
        </p>
      ) : (
        <p className="muted small">Off. Set MQTT_URL to publish sensors to Home Assistant (see the README).</p>
      )}
    </section>
  );
}
