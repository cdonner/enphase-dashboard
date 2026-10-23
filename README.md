# Enphase Dashboard

A self-hosted dashboard for Enphase solar systems. It reads your **IQ Gateway (Envoy)** directly on your
local network, and can optionally use the **Enphase cloud API** to fill in history and show
**IQ EV Charger** sessions.

- Live production, consumption, and grid import/export (5-second updates)
- Today's power curve and daily energy totals for the last 14 days
- Output for each microinverter
- EV charger sessions: energy, range added, and cost
- Gateway token creation and renewal built in (Enlighten sign-in, or paste a token)
- Light and dark mode, works on phones, and every chart has a table view

Not affiliated with or endorsed by Enphase Energy.

## How it works

```
browser ──► dashboard server (Node) ──► IQ Gateway (https, local network)
                                    └──► Enphase cloud API (optional)
```

Browsers can't call the gateway directly, because it uses a self-signed certificate and has no CORS.
So a small server does that work. It polls `/ivp/meters/readings` every 5 s, records one point per
minute, and works out daily totals from the meters' lifetime counters. On some firmware versions the
`whToday` fields in `production.json` are broken, which is why it doesn't use them. Everything it
stores goes in `data/`.

What you need:
- IQ Gateway firmware 7.x or 8.x with token authentication.
- Consumption CTs, for consumption and import/export figures. Without them you only get production.

## Quick start (Docker / QNAP / Synology)

1. Copy `compose.yaml` and `.env.example` into a folder, rename `.env.example` to `.env`, and set at
   least `ENVOY_HOST` (the gateway's **IP address**; `envoy.local` usually doesn't resolve inside
   Docker) and `TZ`.
2. Run `docker compose up -d`.
3. Open `http://<host>:3001`, then create or paste a gateway token (see [Gateway token](#gateway-token)).

**QNAP Container Station:** go to *Applications → Create*, paste `compose.yaml`, and fill in the
`environment:` values directly. Container Station doesn't read `.env` files. The image is published
for `amd64`, `arm64` and 32-bit `arm/v7` (e.g. ARM-based QNAP models such as the TS-431X series) at `ghcr.io/cdonner/enphase-dashboard`.

Data, tokens, and credentials are kept in the `solar-data` volume, so back it up if you care about
the history.

## Quick start (Node)

Requires Node 22+.

```bash
npm install
cp .env.example .env   # set ENVOY_HOST and TZ
npm start              # builds the UI and serves it on http://127.0.0.1:3001
```

For development, `npm run dev` runs Vite on http://localhost:5173 with hot reload and proxies `/api` to the server.

## Configuration

| Variable | Default | |
|---|---|---|
| `ENVOY_HOST` | `envoy.local` | Gateway IP or hostname |
| `TZ` | system | Time zone; daily totals reset at local midnight |
| `PORT` | `3001` | |
| `HOST` | `127.0.0.1` (`0.0.0.0` in Docker) | Interface to listen on |
| `POLL_MS` | `5000` | Gateway polling interval |
| `DASHBOARD_PASSWORD` | – | If set, the dashboard asks for this password (HTTP basic auth, any username) |
| `ENPHASE_API_KEY`, `ENPHASE_CLIENT_ID`, `ENPHASE_CLIENT_SECRET` | – | Enables the [Enphase cloud](#enphase-cloud-optional) features |

## Gateway token

Gateway firmware 7 and later needs a JWT from Enphase. On first start the dashboard shows a setup
panel with two ways to get one:

- **Sign in with Enlighten.** Enter your Enlighten email and password. The server requests a token
  for your gateway and checks that the gateway accepts it. If you tick *Remember credentials*, it
  renews the token by itself before it expires. Homeowner tokens last about a year; installer and
  self-installer accounts get 12-hour tokens, so tick the box for those.
- **Paste a token.** Sign in at <https://enlighten.enphaseenergy.com>, then open
  `https://enlighten.enphaseenergy.com/entrez-auth-token?serial_num=<gateway serial>` and copy the
  `token` value. The serial number is shown in the setup panel.

## Enphase cloud (optional)

The gateway only knows about the moment the dashboard is running, so it can't fill in history or
report on the EV charger. With a free Enphase developer account the dashboard can also:

- fill in the part of today before the dashboard started (15-minute data),
- show full daily totals for days it wasn't running,
- show IQ EV Charger sessions.

Setup:

1. Sign up at <https://developer-v4.enphase.com> and create an application on the free **Watt** plan.
2. Set `ENPHASE_API_KEY`, `ENPHASE_CLIENT_ID`, and `ENPHASE_CLIENT_SECRET` from the application page,
   then restart.
3. In the dashboard, open **Token → Cloud backfill**, follow the *Approve access* link, sign in with
   the Enlighten account that owns the system, and paste the code you're shown.

The Watt plan allows about 10 calls per minute and 1,000 per month. The dashboard uses about 300 a
month: daily totals once a day, today's backfill at most once a day, and EV sessions every 3 hours.
Access tokens refresh themselves, provided the server runs at least once a month.

## Data and security

Everything is stored in `data/`:

| File | Contents |
|---|---|
| `token.json` | Gateway JWT (mode 600) |
| `credentials.json` | Enlighten login, only if *Remember credentials* was ticked (mode 600) |
| `cloud.json` | Enphase cloud OAuth tokens (mode 600) |
| `daily.json`, `history/`, `cloud-daily.json`, `ev.json` | Recorded data |

Tokens and passwords never reach the browser. The dashboard has no user accounts, though, so
anyone who can reach it can view your data and change the gateway token. Set `DASHBOARD_PASSWORD`
when it listens on anything other than localhost, and don't expose it to the internet.

## API

The server's JSON endpoints, which you can use in your own scripts: `/api/status`, `/api/live`,
`/api/today`, `/api/daily?days=N`, `/api/inverters`, `/api/ev`.

## License

MIT
