# Enphase Dashboard

A self-hosted dashboard for Enphase solar systems. It reads your **IQ Gateway (Envoy)** directly on your
local network, and can optionally use the **Enphase cloud API** to fill in history and show
**IQ EV Charger** sessions.

- Live production, consumption, and grid import/export (5-second updates)
- Today's power curve and daily energy totals for the last 14 days
- Output for each microinverter
- EV charger sessions: energy, range added, how much came from solar, and the value at your electricity rates
- Gateway token creation and renewal built in (Enlighten sign-in, or paste a token)
- Appliance detection: finds larger appliances switching on and off in the second-by-second data, with suggestions for naming them
- AI help for naming detected appliances: a ready-made prompt for any AI chat, or optionally the Claude or Gemini API
- Home Assistant integration over MQTT, with automatic discovery
- Light and dark mode (automatic, or chosen with the theme button), works on phones, and every chart has a table view

Not affiliated with or endorsed by Enphase Energy.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshot-dark.png">
  <img alt="Dashboard: live production and consumption, today's power curve, daily energy, microinverters, and EV charging sessions" src="docs/screenshot.png">
</picture>

## How it works

```
browser ──► dashboard server ──► IQ Gateway (https, local network)
                              └──► Enphase cloud API (optional)
```

Browsers can't call the gateway directly, because it uses a self-signed certificate and has no CORS.
So a small server does that work. It's a single self-contained Go binary with the React UI built in, and it polls `/ivp/meters/readings` every 5 s, records one point per
minute, and works out daily totals from the meters' lifetime counters. On some firmware versions the
`whToday` fields in `production.json` are broken, which is why it doesn't use them. Everything it
stores goes in `data/`.

What you need:
- IQ Gateway firmware 7.x or 8.x with token authentication.
- Consumption CTs, for consumption and import/export figures. Without them you only get production.

## Quick start (Docker)

1. Copy `compose.yaml` and `.env.example` into a folder, rename `.env.example` to `.env`, and set at
   least `ENVOY_HOST` (the gateway's **IP address**; `envoy.local` usually doesn't resolve inside
   Docker) and `TZ`.
2. Run `docker compose up -d`.
3. Open `http://<host>:3001`, then create or paste a gateway token (see [Gateway token](#gateway-token)).

The image `ghcr.io/cdonner/enphase-dashboard` is published for `amd64`, `arm64`, and 32-bit `arm/v7` and `arm/v6`.
Docker picks the right one automatically. Data, tokens, and credentials are kept in the `solar-data`
volume, so back it up if you care about the history.

## Installing on a NAS

A NAS is a good home for the dashboard: it's always on and on the same network as the gateway. The
dashboard needs very little (under 100 MB of RAM), so even older ARM models run it fine.

### 1. Create a data folder

Make a folder on a share for the dashboard's data, for example `Container/solar-dashboard/data`.
On QNAP this folder is `/share/Container/solar-dashboard/data`; on Synology it's
`/volume1/docker/solar-dashboard/data`. Store the data here rather than in a Docker volume, so you
can back it up and copy files in and out over SMB.

### 2. Create the container

**QNAP (Container Station):** go to *Applications → Create*, enter a name, and paste the YAML below.
**Synology (Container Manager):** go to *Project → Create*, choose the folder from step 1's parent as
the path, and paste the same YAML.

```yaml
services:
  solar-dashboard:
    image: ghcr.io/cdonner/enphase-dashboard:latest
    container_name: solar-dashboard
    restart: unless-stopped
    ports:
      - "3001:3001"
    environment:
      ENVOY_HOST: 192.168.1.50   # your gateway's IP address
      TZ: America/New_York       # your time zone
      DASHBOARD_PASSWORD: ""     # recommended: anyone on your network can reach the NAS
      # Optional Enphase cloud features (see below):
      # ENPHASE_API_KEY: ""
      # ENPHASE_CLIENT_ID: ""
      # ENPHASE_CLIENT_SECRET: ""
      # ANTHROPIC_API_KEY: ""     # AI suggestions for appliances
      # GEMINI_API_KEY: ""        # or Gemini (free tier)
      # MQTT_URL: mqtt://192.168.1.20:1883   # Home Assistant
    volumes:
      - /share/Container/solar-dashboard/data:/app/data   # Synology: /volume1/docker/solar-dashboard/data
```

Put the values directly in the YAML, because the NAS apps don't read a `.env` file. Then open
`http://<nas-ip>:3001`.

### 3. Moving from another machine (optional)

If the dashboard has already been running somewhere else, stop it there, then copy the contents of
its `data/` folder into the NAS data folder before starting the container. That brings over the
gateway token, the cloud connection, and the recorded history.

Never run two copies against the same Enphase cloud connection at once. Each refresh issues a new
token and invalidates the previous one, so one of the copies will lose access. A copy without cloud
credentials is fine, because it only polls the gateway.

### 4. Updating

Every push to `main` publishes a new `latest` image. NAS container apps usually keep using the copy
they already downloaded, even if you delete and recreate the application, so the image has to be
pulled explicitly. The update script does that and recreates the container from its compose file,
keeping your settings and data. Install it once over SSH, e.g. next to the data folder:

```bash
cd /share/Container/solar-dashboard      # Synology: /volume1/docker/solar-dashboard
curl -fsSL -o update.sh https://raw.githubusercontent.com/cdonner/enphase-dashboard/main/scripts/update-docker.sh
```

(If `curl` is missing, use `wget -O update.sh <same URL>`.) From then on, updating is:

```bash
sh /share/Container/solar-dashboard/update.sh
```

It reports *Already up to date* or recreates the container. If your container isn't called
`solar-dashboard`, pass its name as an argument. To update automatically, run the same command
from cron, e.g. nightly. Old image layers pile up over time; `docker image prune -f` removes them.

### NAS notes

- **Gateway address:** use the IP address. `envoy.local` usually doesn't resolve inside containers.
  Giving the gateway a fixed address (a DHCP reservation in your router) stops it from moving.
- **ARM models and "page size" warnings:** some 32-bit ARM NAS units, such as the QNAP TS-431X
  series, run kernels with 32 KB memory pages. Ordinary Linux images (Debian, Alpine, Node) can't
  start on them, and QNAP shows a page-size warning when you install any container. This image is
  just one static Go binary with no system libraries, so it isn't affected and the warning can be
  ignored.
- **No Container Station?** Run the binary directly instead (see below).
- **Port in use:** change the left side of the mapping, for example `"8085:3001"`.

## Installing on a Raspberry Pi

Any Raspberry Pi on the same network as the gateway works, from a Pi Zero W to a Pi 5. The dashboard
runs as a single binary under systemd and needs no Docker. Raspberry Pi OS Lite is enough.

| Pi | OS | Binary |
|---|---|---|
| Pi 3, 4, 5, Zero 2 W | 64-bit Raspberry Pi OS | `linux-arm64` |
| Pi 2, 3, 4, Zero 2 W | 32-bit Raspberry Pi OS | `linux-armv7` |
| Pi 1, Zero, Zero W | 32-bit Raspberry Pi OS | `linux-armv6` |

The install script picks the right one for you.

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/cdonner/enphase-dashboard/main/scripts/install.sh | sudo ENVOY_HOST=192.168.1.50 sh
```

Use your gateway's IP address. You may want to [read the script](scripts/install.sh) before running it.
It:

- downloads the latest release binary for your Pi to `/usr/local/bin/enphase-dashboard` and checks
  its checksum,
- creates a system user `enphase` and the data directory `/var/lib/enphase-dashboard`,
- writes the settings to `/etc/enphase-dashboard.env` (first install only), taking the time zone from
  the Pi,
- installs and starts the `enphase-dashboard` systemd service, which starts at boot.

Then open `http://<pi-ip>:3001` and set up the [gateway token](#gateway-token).

### Managing it

```bash
sudo nano /etc/enphase-dashboard.env           # change settings (cloud keys, password, ...)
sudo systemctl restart enphase-dashboard       # apply them
systemctl status enphase-dashboard             # is it running?
journalctl -u enphase-dashboard -f             # logs
```

**Updating:** run the install command again. It replaces the binary, keeps your settings and data,
and restarts the service. Add `VERSION=v1.2.0` (for example) after `sudo` to install a specific release.

**Moving from another machine:** stop the dashboard there and on the Pi
(`sudo systemctl stop enphase-dashboard`), copy the contents of the old `data/` folder to
`/var/lib/enphase-dashboard/`, run `sudo chown -R enphase:enphase /var/lib/enphase-dashboard`, then
start the service again. Don't run two copies with the same Enphase cloud connection (see
[step 3 of the NAS guide](#3-moving-from-another-machine-optional)).

**Uninstalling:**

```bash
sudo systemctl disable --now enphase-dashboard
sudo rm /etc/systemd/system/enphase-dashboard.service /usr/local/bin/enphase-dashboard /etc/enphase-dashboard.env
sudo rm -r /var/lib/enphase-dashboard   # your data and tokens
sudo userdel enphase
```

Prefer Docker? The [Docker quick start](#quick-start-docker) also works on a Pi. The image is
published for `arm64`, `arm/v7` and `arm/v6`.

## Running without Docker

Each [release](https://github.com/cdonner/enphase-dashboard/releases) includes standalone binaries
for Linux (`amd64`, `arm64`, `armv7`, `armv6`), macOS, and Windows, plus a `SHA256SUMS` file. A binary has the UI built in, so there's
nothing else to install:

```bash
chmod +x enphase-dashboard-linux-armv7
ENVOY_HOST=192.168.1.50 TZ=America/New_York HOST=0.0.0.0 ./enphase-dashboard-linux-armv7
```

It stores its data in `./data` (change this with `DATA_DIR`) and also reads settings from a `.env`
file in the current directory. To start it at boot, use a systemd unit on Linux, or the NAS's
autorun mechanism (QNAP: `autorun.sh`).

## Building from source

Requires Node 22+ and Go 1.26+.

```bash
npm install
cp .env.example .env   # set ENVOY_HOST and TZ
npm start              # builds the UI and the server binary, then serves on http://127.0.0.1:3001
```

For development, `npm run dev` runs Vite on http://localhost:5173 with hot reload, and proxies `/api`
to the Go server.

## Configuration

| Variable | Default | |
|---|---|---|
| `ENVOY_HOST` | `envoy.local` | Gateway IP or hostname |
| `TZ` | system | Time zone; daily totals reset at local midnight |
| `PORT` | `3001` | |
| `HOST` | `127.0.0.1` (`0.0.0.0` in Docker) | Interface to listen on |
| `DATA_DIR` | `./data` (`/app/data` in Docker) | Where tokens and history are stored |
| `POLL_MS` | `1000` | Gateway polling interval; appliance detection needs 1-second readings |
| `DASHBOARD_PASSWORD` | – | If set, the dashboard asks for this password (HTTP basic auth, any username) |
| `ENPHASE_API_KEY`, `ENPHASE_CLIENT_ID`, `ENPHASE_CLIENT_SECRET` | – | Enables the [Enphase cloud](#enphase-cloud-optional) features |
| `ANTHROPIC_API_KEY` | – | Enables the Claude API button for [AI suggestions](#ai-suggestions) |
| `AI_MODEL` | `claude-haiku-5-5` | Claude model for AI suggestions |
| `GEMINI_API_KEY`, `GEMINI_MODEL` | –, newest Flash | Enables the Gemini button for [AI suggestions](#ai-suggestions) |
| `MQTT_URL`, `MQTT_USERNAME`, `MQTT_PASSWORD` | – | Enables [Home Assistant](#home-assistant) publishing |
| `MQTT_TOPIC`, `HA_DISCOVERY_PREFIX` | `enphase_dashboard`, `homeassistant` | MQTT topics |

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
- show IQ EV Charger sessions, with the share of each session that came from solar.

See [EV charging value](#ev-charging-value) for pricing the sessions.

Setup:

1. Sign up at <https://developer-v4.enphase.com> and create an application on the free **Watt** plan.
2. Set `ENPHASE_API_KEY`, `ENPHASE_CLIENT_ID`, and `ENPHASE_CLIENT_SECRET` from the application page,
   then restart.
3. In the dashboard, open **Token → Cloud backfill**, follow the *Approve access* link, sign in with
   the Enlighten account that owns the system, and paste the code you're shown.

The Watt plan allows about 10 calls per minute and 1,000 per month. The dashboard uses about 400 a
month: daily totals once a day, today's backfill at most once a day, EV sessions every 3 hours, and
3 calls for each day with charging, to work out the solar share.
Access tokens refresh themselves, provided the server runs at least once a month.

## EV charging value

The EV card values each charging session at **what it would have cost at your normal electricity
rate**: session kWh × rate. It also shows how much of each session your panels supplied (the *Solar*
column), but solar doesn't change the value. Both need the
[Enphase cloud](#enphase-cloud-optional) connection, because the charger only reports to the cloud.

Click **Electricity rates** in the EV card and choose one of two options.

**One blended rate (recommended).** A single $/kWh for all sessions, so there's nothing to keep up to
date. Either enter it, for example a year of bills divided by the kWh billed, or leave the field
empty to use the average of your last 12 months of monthly rates (see below).

**Rate per month.** A rate for each month, for example from each utility bill. A session uses the rate
for the month it started in. A month without an entry uses the most recent earlier month, so one
entry is enough until your rate changes. Months before your first entry use the earliest rate.

Until you set any rate, the card shows Enphase's own cost figure, marked with `*`. That figure is
based on the tariff in the Enphase app and counts solar energy as free.

### Rate file

Instead of typing monthly rates into the page, you can keep them in a file, `rates.csv` in the data
directory, e.g. exported from the spreadsheet where you track your electricity bills. The dashboard
re-reads it whenever it changes. Each line is either a rate or a bill:

```csv
# month,rate
2026-09,0.2305

# month,kWh,amount   (the rate is amount / kWh)
Aug-26,1767,407.42
Jul-26,1901,$437.55
```

- Months can be written as `2026-09`, `Sep-26` or `Sep-2026`.
- Tab-separated lines (pasted from a spreadsheet) work too, and a header row is skipped.
- Lines starting with `#` are ignored.

Months in the file override the same months entered on the page. With bill lines, the blended
average is weighted by kWh (total amount / total kWh over the 12 most recent months), so it matches
what you actually paid per kWh. The page shows how many months the file provides, along with any
line it couldn't read.

### Setting rates with the API

Rates are stored in `settings.json` in the data directory, and can be set from a script:

```bash
curl -X PUT http://<host>:3001/api/settings -H 'Content-Type: application/json' \
  -d '{"mode": "blended", "blendedRate": 0.248, "rates": {}}'
```

`mode` is `blended` or `monthly`. Leave out `blendedRate` (or set it to `null`) to use the 12-month
average. `rates` holds monthly rates, e.g. `{"2026-09": 0.2305}`. The request replaces all settings,
so include everything you want to keep. If you set `DASHBOARD_PASSWORD`, add `-u any:<password>`.

## Appliances tab

The **Appliances** tab shows when larger appliances ran, without a meter on each circuit. Every
second, the dashboard reads the real and reactive power on each leg (phase) from the gateway's
consumption meter, after subtracting solar so clouds don't count. It then looks for **steps**: a
jump to a new steady level of at least about 60 W. Each step is described by:

- **size** in watts,
- **circuit**: both legs together for a 240 V appliance, one leg for 120 V, or 240 V plus extra on one
  leg (e.g. a dryer: heating element plus motor),
- **character**: *resistive* (heating elements: water heater, kettle, sauna, oven) or *motor*
  (compressors, pumps, blowers), from the ratio of reactive to real power.

Steps up and down are paired into runs. Run times show on a timeline for each day, and repeated
runs of the same appliance are grouped. If an appliance switches off in stages, such as a sauna heater
turning off one element at a time, each stage is split off its run.

**Naming appliances.** *Needs a name* lists signatures that repeated over the last 7 days, most
energy first, so you can name them in batches. Each shows how often it ran, for how long, example
times, and up to three suggestions from a built-in catalog of common appliances (water heater,
dryer, oven, AC/heat pump, well pump, microwave, kettle, laser printer, furnace blower, ...). Click a
suggestion or type a name. The name then applies to all past and future runs with that signature.
To identify one, switch a suspect on or off and watch for the bar on today's timeline.

**Limitations.** This works best for appliances that switch on and off at a fixed power, roughly
60 W and up. It can't separate:

- loads that vary gradually (computers, TVs, variable-speed heat pumps and pumps while ramping),
- two similar appliances on the same leg (a kettle and a toaster look the same),
- appliances that switch within the same second as another.

Steady loads like servers and network gear simply form the base load. The gateway is polled every
second for this (`POLL_MS`); only detected runs are stored, about 100 bytes each.

### AI suggestions

The built-in catalog only looks at power and circuit. An AI assistant can weigh more clues together:
how regularly a load repeats, the hours it runs, and your own description of the house. Write that
description under **About your home**, e.g. which appliances run on gas, what's off for the season,
the size of a sauna heater. There are two ways to ask:

- **Copy prompt for AI chat** (free, nothing to set up). Copies a ready-made prompt to the clipboard.
  Paste it into a chat with Claude (the free claude.ai works) or another assistant. The answer refers
  to each signature by the ID shown on its card, so you can name the right ones. If the browser blocks
  the clipboard (it does on plain `http://` addresses other than localhost), the prompt appears in a
  box to copy by hand.
- **Ask Claude** / **Ask Gemini** (optional). These buttons appear once an API key is configured. They
  show up to three guesses per signature right on its card, each with a confidence level and a
  one-line reason. Click a guess to use it as the name.

Either way, the prompt holds only a summary per signature (step size, circuit, character, typical
run times, repeat interval, starts per hour of day, energy), your household text, and the names of
appliances you've already identified. It contains no raw readings, gateway details or location.

To enable the API button, create an API key in the Claude Developer Platform
(<https://console.anthropic.com>), which is billed separately from a claude.ai subscription, and set
`ANTHROPIC_API_KEY`. A dedicated workspace with a small monthly spend limit keeps the cost capped.
Requests are sent only when you click, and use Claude Haiku 5.5 by default: about 2-3K input and
2-4K output tokens, a fraction of a cent per request. Set `AI_MODEL=claude-opus-5-5` for the most
capable model, at roughly 5-10 cents per request.

**Gemini** has a free tier. To get a key:

1. Open <https://aistudio.google.com>, sign in with a Google account, and go to **Get API key → Create
   API key**.
2. Pick the Google Cloud project for the key. AI Studio creates one for you called **Default Gemini
   Project** and lists it under *imported projects*, even if you never imported anything. That one
   works. For a separate project, choose **Create project**, or create it at
   <https://console.cloud.google.com> and then pick it under **Import project**.
3. Make sure the project has **no billing account** attached, so the key stays on the free tier and
   can't cost anything. The worst case is a "rate limit reached" message. The *API keys* page in AI
   Studio shows each key's project and plan; it should say *Free*. With billing enabled, requests
   are charged at paid rates.
4. Set the key as `GEMINI_API_KEY` in the dashboard's environment and restart it. An **Ask Gemini**
   button then appears.

On the free tier Google may use requests to improve its products. The request contains only the
summary described above and your household text. The dashboard picks the newest *Flash* model
available to the key, or set `GEMINI_MODEL`. When the free-tier rate limit is reached, wait a minute
and try again.

Workload identity federation for Claude also works: set `ANTHROPIC_FEDERATION_RULE_ID`,
`ANTHROPIC_ORGANIZATION_ID`, `ANTHROPIC_SERVICE_ACCOUNT_ID` and `ANTHROPIC_IDENTITY_TOKEN_FILE`
instead of a key. This needs an identity provider that issues tokens for the machine the dashboard
runs on (Kubernetes, a cloud VM, CI), so on a home NAS or Pi an API key is the practical choice.

## Home Assistant

With `MQTT_URL` set, the dashboard publishes to an MQTT broker using
[Home Assistant MQTT discovery](https://www.home-assistant.io/integrations/mqtt/#mqtt-discovery), so
the sensors appear automatically under a device called **Enphase dashboard**:

| Entity | Unit |
|---|---|
| Solar production, House consumption, Grid power (import +, export -) | W |
| Solar produced / Consumed / Grid import / Grid export today | Wh (reset daily, usable in the Energy dashboard) |
| For each named appliance: running (on/off), power, energy today | – / W / Wh |

Named appliances are added and removed in Home Assistant as you name or forget them. With these you
can build automations like "notify me when the dryer stops running".

Setup:

1. In Home Assistant, install the **Mosquitto broker** add-on (or use any MQTT broker) and the
   **MQTT** integration.
2. Set `MQTT_URL` (e.g. `mqtt://192.168.1.20:1883`, or `mqtts://` for TLS), and `MQTT_USERNAME` /
   `MQTT_PASSWORD` if the broker needs them, then restart the dashboard.
3. The settings panel (**Token** button) shows whether the dashboard is connected.

State is published every 10 seconds as retained JSON on `enphase_dashboard/state` and
`enphase_dashboard/appliance/<id>`, with availability on `enphase_dashboard/availability`. Change the
base topic with `MQTT_TOPIC`, and the discovery prefix with `HA_DISCOVERY_PREFIX` (default
`homeassistant`).

## Data and security

Everything is stored in the data directory (`DATA_DIR`):

| File | Contents |
|---|---|
| `token.json` | Gateway JWT (mode 600) |
| `credentials.json` | Enlighten login, only if *Remember credentials* was ticked (mode 600) |
| `cloud.json` | Enphase cloud OAuth tokens (mode 600) |
| `settings.json`, `rates.csv` | Electricity rates (`rates.csv` is optional, see [Rate file](#rate-file)) |
| `appliances.json`, `ai-suggestions.json` | Appliance names and signatures, last AI suggestions |
| `daily.json`, `history/`, `cloud-daily.json`, `ev.json`, `ev-days/`, `events/` | Recorded data |

Tokens and passwords never reach the browser. The dashboard has no user accounts, though, so
anyone who can reach it can view your data and change the gateway token. Set `DASHBOARD_PASSWORD`
when it listens on anything other than localhost, and don't expose it to the internet.

## API

The server's JSON endpoints, which you can use in your own scripts: `/api/status`, `/api/live`,
`/api/today`, `/api/daily?days=N`, `/api/inverters`, `/api/ev`, `/api/appliances` and
`/api/appliances/day?date=YYYY-MM-DD`, and `/api/settings` (GET and PUT).

## License

MIT
