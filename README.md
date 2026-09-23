# botchecker

Detects which of your VLESS server addresses have been blocked from inside Iran.

A server that is filtered in Iran still answers perfectly from everywhere else,
so nothing in your own monitoring will show it. This service fetches the live
config list, probes every unique `address:port` from eight Iranian networks
through [check-host.net](https://check-host.net), and tells you which addresses
are unreachable — and, importantly, which are merely switched off.

## What it distinguishes

| Verdict | Meaning |
|---|---|
| `HEALTHY` | Every Iranian probe completed the TCP handshake. |
| `BLOCKED_IR` | Control nodes connect, Iran gets nothing back. **These are the addresses to replace.** |
| `PARTIAL_BLOCK` | Some Iranian carriers reach it, others do not. |
| `PORT_CLOSED` | Iranian packets arrive and are refused — reachable, but nothing is listening. Not filtering. |
| `SERVER_DOWN` | The control nodes cannot connect either, so nothing can be said about Iran. |

The distinction between a refusal and a timeout is what makes this reliable. A
refusal proves the packet arrived; a timeout on a port that is known to be
closed proves it did not. Without the control nodes outside Iran, a server that
has simply stopped listening looks identical to one that has been filtered.

## Running it

```bash
cp .env.example .env
# set DASHBOARD_PASS — the service refuses to start without one
docker compose up --build
```

Then open <http://localhost:8081/> and sign in with `DASHBOARD_USER` /
`DASHBOARD_PASS`.

## Dashboard

| Page | Shows |
|---|---|
| `/` | Every endpoint with its current verdict and how many Iranian probes answered. |
| `/timeline` | The daily mix over the last 30 days, plus every state change in that window. |
| `/lifetime` | How long each address survived before it was blocked, with average and median. |
| `/asn` | Failure rate per Iranian operator — nationwide block, or only some carriers. |
| `/target/{addr}/{port}` | Full history for one address, where its packets stop, and the notify-only switch. |
| `/providers` | Which addresses are in your Hetzner project and which are not. |
| `/provisions` | Every reaction to a block, and every alert sent. |
| `/settings` | Tokens and thresholds, editable without a restart. |
| `/bot` | What the Telegram bot has been asked to do, including refused attempts. |

Pages are rendered server side with no JavaScript and no external assets, so
the dashboard works even when opened from a network that blocks CDNs.

## API

All endpoints require Basic Auth except `/healthz`.

```bash
curl -u admin:$DASHBOARD_PASS -X POST localhost:8081/api/v1/scan   # → {"scan_id": N}
curl -u admin:$DASHBOARD_PASS localhost:8081/api/v1/scan/N         # status and progress
curl -u admin:$DASHBOARD_PASS localhost:8081/api/v1/blocked        # the actionable list
```

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/scan` | Start a scan (`409` if one is running). |
| `GET` | `/api/v1/scan/:id` | Status, progress and current results. |
| `GET` | `/api/v1/scan/latest` | The most recent completed scan. |
| `GET` | `/api/v1/scans` | Scan history. |
| `GET` | `/api/v1/blocked` | Blocked endpoints with their config ids and traceroute summary. |
| `GET` | `/api/v1/targets` | Every endpoint with its latest verdict. |
| `GET` | `/api/v1/targets/:addr/:port` | Full history for one endpoint. |
| `GET` | `/api/v1/stats/timeline?days=30` | Daily verdict mix. |
| `GET` | `/api/v1/stats/lifetime` | Lifetime statistics. |
| `GET` | `/api/v1/stats/asn?days=30` | Per-operator failure rates. |
| `GET` | `/healthz` | Liveness, unauthenticated. |
| `GET` | `/api/v1/watchlist` | Addresses being re-probed, with their outage counts. |
| `GET` | `/api/v1/outages?days=7` | Recorded blocked readings. |
| `GET` | `/api/v1/provisions` | What happened each time the threshold was crossed. |
| `POST` | `/api/v1/provision/:addr/:port` | Trigger the reaction by hand; every guard still applies. |
| `POST` | `/api/v1/targets/:addr/:port/notify-only` | Mark an endpoint hands-off. |
| `GET` | `/api/v1/providers` | Ownership of every address. |
| `GET` | `/api/v1/panel/nodes` | 3x-ui nodes with their online counts. |
| `GET`/`POST` | `/api/v1/settings` | Read or change settings; secrets are never returned. |
| `POST` | `/api/v1/settings/test/:target` | Verify `telegram`, `xui` or `hetzner` credentials. |
| `GET` | `/api/v1/bot/actions` | Telegram audit trail and loop state. |

## Guards against false alarms

Replacing a server costs money and downtime, so a block is not declared lightly:

- **Control nodes.** Every check also probes nodes outside Iran. If they fail
  too, the verdict is `SERVER_DOWN`, not a block.
- **Consensus threshold.** `MIN_IR_FAIL` (default 6 of 8) Iranian probes must
  fail. The Iranian nodes are flaky on their own.
- **Confirmation round.** A failure verdict is probed again after
  `CONFIRM_DELAY` before it is recorded. If the two rounds disagree, the newer
  reading wins and is flagged unconfirmed.
- **Deduplication.** The config list repeats the same address under several
  ids; each `address:port` is probed once.

## Reacting to a block

Detection alone still leaves the work manual, so a blocked address is followed
through automatically:

1. Every known address is re-probed on a **watch loop**, every
   `WATCH_INTERVAL` (default 8 minutes), so an address that gets filtered is
   caught within minutes rather than at the next full scan. Set
   `WATCH_SCOPE=blocked` to watch only the already-blocked ones. The splash
   API only hands out configs marked active, so a server whose configs have
   been switched off would otherwise vanish from monitoring at exactly the
   wrong moment; list it in `EXTRA_TARGETS` (`address:port:CC`, comma
   separated) to keep probing it regardless.
2. Once it has been seen blocked `OUTAGE_THRESHOLD` times inside
   `OUTAGE_WINDOW` (default 3 times in 30 minutes), an alert goes to Telegram —
   including **how many users are online** on that server, read from 3x-ui.
3. If the address is in your Hetzner project, a replacement server can be
   created from a snapshot, and the new address is then probed from Iran with
   the same engine.
4. When it checks out, a **handoff hook** fires. It is deliberately empty: the
   new address is reported and a human wires it in.

### Not every address is yours

Most blocked addresses in a real config are not on Hetzner at all. The token is
used to list the project's servers, primary IPs and floating IPs, so each
address is classified as `hetzner`, `external`, or `unknown` when no token is
set — never guessed from IP ranges. An `external` address gets its own alert
saying plainly that it cannot be replaced automatically, rather than silently
doing nothing. The `/providers` page shows the split.

Hetzner's own anti-abuse block is reported separately: a new server does not
fix an abuse notice.

A token covers **one project**. If your servers are spread across several
projects, addresses in the others show as `external`. After changing the token,
the *Re-read the Hetzner project* button on `/providers` re-tags every address
without waiting for the next full scan.

### Notify only

Any endpoint can be marked **notify only** on its page. It keeps being probed
and keeps alerting, but no replacement is ever created for it — for servers you
want to hear about but handle yourself.

### Guards

Creating servers spends money, so it is fenced in:

- `PROVISION_ENABLED` is **off** by default and `PROVISION_DRY_RUN` is **on**:
  the full flow runs and alerts, but no server is created.
- One alert per address per `PROVISION_COOLDOWN` (default 6h), so a block you
  already know about does not fill the chat.
- `PROVISION_MAX_PER_DAY` caps creations, and only one run happens at a time.
- If the *new* address turns out to be blocked too — Hetzner recycles
  addresses — it is reported and the run stops. Retrying automatically would
  loop through paid servers.

## The Telegram bot

By default the bot only sends alerts. Setting `TELEGRAM_BOT_ENABLED=true` — or
the switch under `/settings` — lets it take commands too, turning it into a
control surface for a phone.

```
/start     the menu          /servers   every endpoint
/blocked   only the blocked  /status    how the service is configured
/scan      start a scan      /help      this list
```

The menu leads to a paginated endpoint list; tapping one opens its detail
screen — verdict, Iranian and control probe counts, connected users, where its
packets stop — with the actions available for it.

**Buttons reflect what is actually possible.** An address outside your Hetzner
project is not offered a replacement it cannot create; the 3x-ui controls only
appear once a panel is configured. A control that would always fail is not
shown.

### Anything destructive asks first

Disabling, deleting or replacing shows a second screen stating exactly what
will happen and how many users are connected. Confirmations are single-use,
expire after 60 seconds, and only the person who started one can complete it.

Destroying a Hetzner server is guarded harder: it is **refused outright** while
anyone is still connected, and the check runs again at confirmation time in
case the situation changed between taps.

### Who can use it

`TELEGRAM_ALLOWED_IDS` lists the numeric Telegram user ids permitted to command
the bot; an empty value falls back to `TELEGRAM_CHAT_ID`. Anyone else is
**ignored without a reply** — answering would confirm the bot exists to someone
who should not be talking to it — and the attempt is recorded. With commands
enabled but no allow list, the loop refuses to start at all.

Every update the bot receives is recorded, authorized or not. The `/bot` page
shows the trail and counts refused attempts over the last week.

⚠️ Telegram allows only one process to poll a bot. Running a second botchecker
with commands enabled produces a clear error rather than two bots fighting.

## Replacing a blocked server

With the Zex VPN panel configured, a confirmed block runs the whole cycle
without anyone touching it:

1. **Quiet** — every config on the blocked address is switched off, so the app
   stops handing it out.
2. **Choose** — a country is picked where *every* address is fully reachable
   from all eight Iranian probes. One filtered address disqualifies a country:
   buying into a range that is already being blocked wastes the server.
3. **Build** — a server is created from the snapshot in that country, trying
   each of its Hetzner locations in turn.
4. **Verify** — the new address is probed from Iran with the same engine and
   the same verdict rules as the main scan. Anything short of healthy stops it.
5. **Swap** — every config moves onto the new address and is switched back on,
   renamed for its new country.

`PROVISION_QUIET_WINDOW` (default 10 minutes) is the minimum time the configs
stay off. It is a floor, not an added delay — building and verifying normally
take longer, and the swap happens as soon as both are done.

**Every failure leaves the configs switched off, on purpose.** Handing users a
config that is known not to work is worse than handing them none, so the alert
says exactly what is still broken and what was left off. The created server is
not deleted either; it has been paid for and its id is in the alert.

### Why the raw link matters

The panel serves each config's `RawLink` — `vless://uuid@1.2.3.4:443?…` — and
that string carries the address inside it. Changing only the address column
would leave the panel looking correct while every client kept dialling the dead
IP. The replacement rewrites the link too, for `vless://`, `trojan://`,
`vmess://` and raw xray JSON, preserving the uuid, query and fragment.

### Panel changes this needs

The panel could previously only create, edit and delete configs: `is_active`
existed and the splash query already filtered on it, but nothing could ever
turn it off. Three endpoints were added under the existing admin auth:

| Endpoint | Purpose |
|---|---|
| `POST /admin/v2ray/:id/active` | Show or hide one config |
| `POST /admin/v2ray/bulk-active` | Show or hide every config on one address |
| `POST /admin/v2ray/replace-address` | Move every config onto a new address |

They live in the `vpn-pannel` repository, not this one.

## Settings in the panel

Tokens can be entered under `/settings` instead of the environment. A value
saved there overrides the matching variable and takes effect immediately, with
no restart, because the clients read their credentials per request.

Secrets are encrypted with AES-256-GCM before they are stored. The key comes
from `ENCRYPTION_KEY`, or is generated on first run and written next to the
database as `secret.key` (mode 0600). **Back that file up with the database** —
without it the stored tokens have to be entered again.

Secret values are never returned, by the API or the page: you see only whether
one is set and its last four characters. Clearing a value is an explicit
checkbox, so submitting a form cannot wipe a token you could not see.

Because the settings page introduces forms behind Basic Auth, form submissions
carry a CSRF token. It applies only to form content types — a JSON or bodyless
`POST` from `curl` is unaffected, so `POST /api/v1/scan` keeps working.

## Configuration

Every setting is an environment variable — see [.env.example](.env.example).
The ones worth knowing:

| Variable | Default | Notes |
|---|---|---|
| `DASHBOARD_PASS` | — | Required. The service will not start without it. |
| `CONTROL_NODES` | `de1,nl1` | Set empty to disable, at the cost of telling a block from an outage. |
| `MIN_IR_FAIL` | `6` | Iranian probes that must fail before a block is declared. |
| `CONFIRM_ROUNDS` | `2` | Probe rounds before a failure is recorded. |
| `SCAN_INTERVAL` | `30m` | `0` leaves scans manual only. |
| `RETENTION_DAYS` | `90` | Raw probe data is pruned after this; the transition log is kept. |
| `TRACEROUTE_ENABLED` | `true` | Traceroutes run only on blocked endpoints, and take 90 s or more. |
| `TELEGRAM_BOT_ENABLED` | `false` | Off means alerts only. On means the bot accepts commands. |
| `TELEGRAM_ALLOWED_IDS` | — | Who may command the bot. Empty falls back to the chat id. |

`CHECKHOST_RPS_DELAY` is deliberately conservative: check-host.net does not
document its rate limits.

## Storage

SQLite via the pure-Go `modernc.org/sqlite` driver, so the image needs no build
toolchain. Raw probe readings are pruned after `RETENTION_DAYS`; the `events`
table, which records only verdict *changes*, is never pruned. That is what makes
"which addresses were blocked this month" a small indexed lookup rather than a
scan of every probe ever taken, and what the lifetime figures are built from.

## Development

```bash
go test ./...          # unit and integration tests, no network required
go vet ./...
```

The tests replay real captured check-host responses, including the traceroute
from a blocked address whose packets die on `10.233.65.174` — inside the
operator's own network, never reaching an international hop.

## Deliberately unimplemented

`provision.Handoff` is still an empty extension point, used only when the Zex
panel is not configured: the service then creates a server, verifies it, and
reports the address for someone to wire in by hand.

Registering a new server as a node in 3x-ui is also out of scope. Every server
built from the snapshot carries the panel's database, and therefore the same
`panelGuid` — 3x-ui calls this "the cloned-server footgun" and warns that
per-node online attribution breaks. Resetting that GUID after creation is the
missing piece.
