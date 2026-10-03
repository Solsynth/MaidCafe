# MaidCafe

MaidCafe is the backend for MaidKit-managed hosts. It contains two separate
runtime modes:

- **Cloud**: a PostgreSQL-backed control plane for daemon registration,
  credentials, metrics, and notifications.
- **Daemon**: a local named-webhook runner for managed hosts. It can run fully
offline or publish metrics and selected webhook notifications to the cloud over
ordinary HTTP(S) POST requests.

The daemon has no database, queue, NATS connection, Solar authentication client,
or inbound cloud-control channel. Cloud-to-daemon control is intentionally not
implemented.

## Current feature set

### Cloud control plane

- Gin HTTP server with recovery middleware and request validation.
- PostgreSQL persistence through GORM and startup `AutoMigrate`.
- Solar Network bearer-token authentication for user routes.
- Workspace membership checks through the Solar workspace service
  (`DyWorkspaceService`): every daemon belongs to a workspace, and any member
  of that workspace can manage its daemons and notifications.
- Daemon registration with one-time randomly generated credentials.
- bcrypt storage for daemon credentials; plaintext secrets are never persisted.
- Secret rotation that invalidates the previous secret.
- Soft daemon deletion: disables the daemon, removes metrics, and keeps the row
  as an audit record.
- Daemon metrics ingestion with server-side receive timestamps.
- Notification persistence with bounded kind, title, body, and metadata fields.
- Optional NATS/event-bus fan-out using the event type
  `maidcafe.notification.v1`.
- Durable notification listing with unread filtering, daemon filtering, limits,
  cursor pagination, and idempotent read acknowledgement.
- Workspace quota exposure: the effective plan quota (`max_daemons`,
  `polling_interval_seconds`, `metrics_retention_days`) is served to workspace
  members (`GET /api/workspaces/:id/quota`) and to daemons for self-tuning
  (`GET /api/daemons/:id/quota`).
- Unauthenticated health endpoint:

```text
GET /health
{"ok":true,"mode":"cloud"}
```

### Daemon runtime

- Gin HTTP routing with recovery middleware.
- Loopback default listen address: `127.0.0.1:8747`.
- Named, immutable webhook configuration.
- Bearer-secret authentication with constant-time comparison.
- Disabled and unknown webhooks return `404`.
- Request bodies are opaque stdin bytes. They are never parsed, templated, or
  appended to command arguments.
- Commands run directly with `exec.CommandContext`; the daemon never invokes
  `sh -c` (script actions apply their working directory with a `cd` line
  prepended to the rendered body instead).
- Static configured arguments only.
- Absolute command paths and controlled working directory.
- Per-hook `cwd` (absolute working directory), `env` (`KEY=VALUE`
  assignments), `user` (run as another account), and `displayName` (optional
  human-readable label; `name` stays the API slug and is what audit records
  and the `/api/v1/actions/:name` route use).
- `user` runs are delegated to `sudo -H -u <user>`, so the daemon process
  itself stays unprivileged. Environment assignments are passed as
  command-line `VAR=value` entries (sudo applies them on top of its reset
  environment) and the working directory is applied without sudo `-D` (which
  default sudoers rules reject): script bodies carry a `cd` line prepended by
  the executor, and plain commands inherit the daemon-set working directory,
  since sudo does not reset it. The executed command is always the configured
  absolute path, never a shell wrapper, so the sudoers rule matches it. The
  sudoers rule granting the daemon the right to run MaidKit-deployed scripts
  as the configured users is installed by MaidKit; hand-configured entries
  must provide their own rule.
- Script actions that run as another user render their substituted body next
  to the deployed script under a hidden `.run` directory (0755, created on
  demand), so the target account can read and execute them; the daemon user
  needs write access to the scripts directory for that to work.
- Request body size limits.
- Maximum concurrent execution limit with `429` on exhaustion.
- Script timeout handling with `504` responses.
- Non-zero exit handling with `502` responses.
- Bounded stdout/stderr capture in JSON responses.
- Atomic success/failure execution counters.
- Scheduled jobs (`daemon.jobsDir` fragments): cron expressions or `@every`
  descriptors targeting any configured action or native operation, run
  through the same executor (audit, concurrency, timeout) with optional
  `job.failure` notifications.
- Container log tracking: running containers are tailed incrementally
  (`logs --since`) on the logs cadence, appended to per-container JSONL files
  (rotated at 512 KiB, one generation, pruned by `metricsRetentionDays`) and
  pushed to SSE `logs` subscribers; `GET /api/v1/containers/:id/logs` returns
  the tail window, and `?source=runtime` a live tail from the runtime.
- Per-container detail reads: `GET /api/v1/containers/:id/inspect` (the
  runtime's inspect payload) and `GET /api/v1/containers/:id/stats` (a
  normalized resource sample), alongside the container's lifecycle actions.
- Container update checks: on `updateCheckInterval` the daemon compares each
  container's pulled digest with the one its registry publishes for the tag, so
  `GET /api/v1/updates` answers "is this container outdated?" without pulling
  anything. `container.pull` fetches the container's image and
  `container.update` recreates a compose-managed container on it.
- Hot reload: the daemon watches its config file and the action/alarm/job
  fragment directories, `systemctl reload`/SIGHUP re-reads them, and
  `PATCH /api/v1/config` patches a safe subset of `[daemon]` keys — in every
  case the reloadable components (hooks, jobs, alarms, runtimes, watched
  processes, intervals, limits, cloud endpoint) swap without a restart.
  Transport, listen address, storage paths and the metrics secret remain
  restart-required.
- Opt-in WebSocket terminal (`GET /api/v1/terminal`) serving an allowlisted
  login shell on a PTY, so browser-based MaidKit builds can attach a terminal
  without SSH. Off by default, audited as `source: "terminal"`. A daemon behind
  NAT can serve the same terminal through the cloud instead
  (`daemon.terminal.relay.enabled`); see
  [WebSocket terminal](#websocket-terminal).
- Opt-in file API (`/api/v1/files/...`) serving the SFTP operations a browser
  cannot make — list, stat, read (windowed), write, mkdir, move, copy, delete —
  confined to an allowlisted set of roots by `daemon.files.roots` and by
  `os.Root`, with mutations gated separately by `allowWrite`. Off by default and
  available over the stdio transport as `files.*` actions; see
  [File API](#file-api).
- Opt-in privileged roots: a root declared `privileged = true` is written as
  root through the `maidkit-priv` helper, so a browser client can edit an nginx
  or Caddy configuration the daemon account cannot touch. The helper resolves a
  *profile name* — never a path — from a root-owned profile file, so a
  compromised daemon can only reach directories an operator declared in both
  files. See [Privileged operations](#privileged-operations).
- Public health endpoint that exposes only daemon mode and ID:

```text
GET /health
{"ok":true,"mode":"daemon","id":"..."}
```

### SSH stdio daemon mode

Set `daemon.transport = "stdio"` to run without a listening HTTP port. MaidKit
starts `/usr/local/bin/maidcafe-daemon` over SSH and exchanges newline-delimited
JSON on stdin/stdout:

```json
{"type":"request","id":"1","action":"health"}
{"type":"request","id":"2","action":"metrics"}
{"type":"request","id":"3","action":"action","name":"backup","body":{"job":"incremental"}}
```

The daemon emits a `ready` event, periodic `metrics` events, and one response
per request. SSH authentication is the transport boundary for actions, so
`daemon.actions` entries use fixed absolute commands and fixed argument lists
without webhook secrets. MaidKit's server detail page installs this mode and
lets the user define those action presets.

### Optional cloud publishing from the daemon

- Publishes metrics on every `metricsInterval` tick.
- Scores host health locally (CPU, memory, swap, disk, load, process memory,
  webhook failures) and embeds the score in every metric it publishes as
  `health_score`/`health_status`, so the cloud stores health history without a
  second request.
- Publishes configured action metadata on every metrics tick.
- When `logsUploadEnabled = true`, batches captured container log lines to
  `POST /api/daemons/:id/logs`; failed uploads remain in a bounded daemon-local
  buffer and workspace `metrics_retention_days` prunes cloud rows.
- Publishes daemon-side log alert notifications through the same notification
  pipeline as metric alarms.
- Uses request deadlines and disables cross-host redirect following.
- Drops failed publishing attempts without failing local execution.
- Does not retain an unbounded retry queue.

Cloud publishing is disabled when either setting is empty. Log upload is
separately opt-in even when cloud publishing is configured. HTTPS is required,
except for HTTP development URLs using `localhost` or `127.0.0.1`.
- Container status upload: when `statusUploadEnabled = true`, the daemon
  publishes the managed container set's status (state, image, compose project,
  runtime) to the cloud on the metrics tick, so managed hosts can be inspected
  centrally from one place. An optional managed allowlist (`managedContainers`,
  `managedComposes`) scopes both log upload and status upload to a curated set;
  an empty allowlist uploads every container. Status carries no secrets.
- Each publish is a **snapshot**, not an append: the daemon names the runtimes
  it managed to enumerate in that tick, and the cloud drops the containers of
  those runtimes that the snapshot leaves out. A container that has left the
  host therefore stops being listed on the next tick instead of answering
  queries in its final state — which is what an update needs, because a
  recreate mints a new container id for the same name and the old row would
  otherwise read as a stopped container long after its replacement is up. A
  runtime whose listing failed is not named, so a hiccup holds its containers
  at their last known state rather than deleting them, and the cloud's
  retention window remains the backstop for a host that stops reporting.

## Cloud API

User routes require a Solar bearer token. Daemon ingestion routes require the
registered daemon secret instead; a daemon secret cannot access user routes.

### Daemons

| Method | Route | Purpose |
| --- | --- | --- |
| `POST` | `/api/daemons` | Create a daemon in a workspace; returns the one-time secret |
| `GET` | `/api/daemons?workspace_id=` | List daemons in a workspace |
| `GET` | `/api/daemons/:id` | Read a workspace daemon |
| `PATCH` | `/api/daemons/:id` | Change name or enabled state |
| `POST` | `/api/daemons/:id/rotate-secret` | Rotate the one-time secret |
| `DELETE` | `/api/daemons/:id` | Disable daemon and delete its metrics |
| `POST` | `/api/daemons/:id/metrics` | Ingest daemon metrics |
| `GET` | `/api/daemons/:id/health` | Read the daemon's latest host health score |
| `POST` | `/api/daemons/:id/logs` | Ingest a bounded batch of container log lines |
| `POST` | `/api/daemons/:id/notifications` | Create a daemon notification |

Create a daemon inside a workspace you belong to:

```sh
curl -X POST http://localhost:8080/api/daemons \
  -H 'Authorization: Bearer <solar-token>' \
  -H 'Content-Type: application/json' \
  -d '{"workspace_id":"<workspace-id>","name":"managed-host-01"}'
```

The response contains `id`, `workspace_id`, `name`, and `secret`. Store the
secret in the managed host configuration. It is not returned by list or read
endpoints.

### Uploaded logs

| Method | Route | Purpose |
| --- | --- | --- |
| `GET` | `/api/daemons/:id/logs?container_id=&limit=` | List uploaded logs for workspace members |

Uploaded rows follow the workspace's `metrics_retention_days` quota and are
bounded to 500 lines per request, with each line capped at 4096 bytes.

### Notifications
| `GET` | `/api/notifications?workspace_id=` | List workspace notifications |
| `POST` | `/api/notifications/:id/read` | Mark a workspace notification read |

`GET /api/notifications` requires `workspace_id` and additionally supports
and an RFC3339 `before` cursor.

## Daemon API

Full walkthrough for configuring and invoking actions and webhooks, including
the cloud relay, lives in [`docs/webhooks.md`](docs/webhooks.md).

Webhook requests carry an HMAC signature over the body, keyed by the webhook's
own secret:

```text
POST /api/v1/webhooks/:name
X-MaidCafe-Signature: <hex HMAC-SHA256 of the raw body keyed by the webhook secret>
```

Example:

```sh
SECRET='replace-with-local-webhook-secret'
BODY='{"job":"incremental"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}')

curl -X POST http://127.0.0.1:8747/api/v1/webhooks/backup \
  -H "X-MaidCafe-Signature: $SIG" \
  --data-binary "$BODY"
```

Successful execution returns `200`; non-zero exit returns `502`; timeout returns
`504`; oversized bodies return `413`; exhausted concurrency returns `429`.
Metrics and configured actions use the daemon metrics secret:

```text
GET /health
GET /api/v1/metrics
GET /api/v1/health
POST /api/v1/actions/:name
GET /api/v1/audit?limit=N
Authorization: Bearer <metrics-secret>
```

Every execution — HTTP webhooks, actions, cloud-relayed webhooks and scheduled
jobs — is appended to `daemon.auditPath` (default `/var/lib/maidcafe/audit.jsonl`)
as one JSON line per run: timestamp, `name` (API slug), optional
`display_name`, `source` (`http` | `stdio` | `relay` | `job` | `terminal`),
`ok`, `exit_code`, `duration_ms`, and a truncated failure reason. WebSocket
terminal sessions land in the same file with `name` and `source` `terminal`,
one line per session and no transcript. File API calls land there too, with
the action slug as `name`, `source` `http` or `stdio`, the resolved path in
`target` (`from -> to` for a move or copy), and `privileged: true` when the
operation went through the `maidkit-priv` helper as root. The file rotates at 1 MiB
keeping one generation (`audit.jsonl.1`). Logging is best-effort: an
unwritable path disables it with a warning and never affects execution.
`GET /api/v1/audit?limit=N` returns the newest entries (default 50, max 500)
newest first, authenticated with the metrics secret.

### Overview health

`GET /api/v1/health` evaluates the daemon's current metric sample and returns a
scored overview of host health, authenticated with the metrics secret:

```json
{
  "score": 84,
  "status": "degraded",
  "evaluated_at": "2026-08-15T12:00:00Z",
  "host_id": "9e4b...",
  "checks": [
    {"name": "cpu", "status": "ok", "score": 100, "value": 12.3, "unit": "percent", "warn": 75, "crit": 95},
    {"name": "memory", "status": "warning", "score": 33.3, "value": 90, "unit": "percent", "warn": 80, "crit": 95,
     "message": "Memory at 90.0% (warn 80.0%, crit 95.0%)"}
  ],
  "issues": ["Memory at 90.0% (warn 80.0%, crit 95.0%)"]
}
```

Every dimension scores 0..100: 100 while at or below `warn`, decaying linearly
to 0 at `crit`. The report `score` is the weight-renormalized average of the
applicable dimensions — CPU (0.22), memory (0.24), disk (0.20, worst mount),
load per core (0.14), swap (0.10), daemon process memory (0.05) and webhook
failure ratio (0.05). A dimension that does not apply (no swap, no webhook runs,
no disk data) is marked `skipped` and excluded from the average. `status` bands
the score: `healthy` at 90 or above, `degraded` at 70 or above, otherwise
`critical`.

The same evaluation is embedded in every metric sample as `health_score` and
`health_status`, so the SSE `metric` frames, the stdio `metrics` action, the
local history files and the cloud all carry one score computed in one place.
The thresholds are built in, not configurable: per-condition paging stays with
`[[daemon.alarms]]`; this endpoint is the overview.

### Realtime event stream

`GET /api/v1/stream` serves a Server-Sent Events stream of realtime daemon
state over HTTP, authenticated with the same metrics secret:

```text
GET /api/v1/stream?events=metric,containers,images,processes,systemd
Authorization: Bearer <metrics-secret>
```

- `events` is a comma-separated whitelist of event types; omitted means all.
  Unknown names return `400`.
- `processesLimit` overrides how many processes `processes` frames carry for
  this subscriber: `0` requests the complete process table (the default for
  this parameter is the daemon's configured `processesLimit`). Valid values
  are `0` or `1..10000`; anything else returns `400`. Each subscriber is
  capped independently, so one client can ask for everything while another
  keeps the default slice.
- Every frame is standard SSE (`event:`/`data:` lines terminated by a blank
  line); a `: ping` comment is emitted every 15s while idle.
- The first frame is always `hello`, carrying the stream version, daemon
  version, and the configured collection intervals in seconds so clients can
  detect staleness:

```json
{"stream":"v1","version":"0.1.0","intervals":{"metric":1,"containers":5,"images":60,"processes":10,"systemd":30}}
```

- `metric` frames are the same payload as `GET /api/v1/metrics`, delivered
  every `streamInterval` (default `1s`) while at least one client subscribes.
- `containers` frames (every `containersInterval`, default `5s`) report the
  podman/docker container list, including the compose project extracted from
  container labels.
- `images` frames (every `imagesInterval`, default `60s`) report the
  podman/docker image list (`id`, `tags`, `size`, `created`, `digest`).
- `processes` frames (every `processesInterval`, default `10s`) report the top
  `processesLimit` (default `50`, valid `1..500`) CPU consumers.
- `systemd` frames (every `systemdInterval`, default `30s`) report the merged
  systemd unit list, including enabled-but-inactive units.
- Container and image listing retries through `sudo -n` when the direct query
  fails or returns nothing and the daemon is not root, so root-owned
  containers stay visible to a non-root daemon (e.g. the systemd `maidcafe`
  user) when passwordless sudo is available. The retry is never interactive.
  A failed direct query keeps its own error; an empty direct listing whose
  elevated retry also fails is reported as an error rather than a misleading
  empty list, so invisible root-owned containers never look like "no
  containers".
- That retry is the *read* rule, and it is a fallback in one direction only:
  the daemon user's own store answers whenever it has containers, and root's
  fills in when it has none. A store that does not answer is skipped, unless
  `sudo -n` may run that runtime — the grant the shipped sudoers rule sets up —
  in which case a store that is reachable by policy but does not answer is an
  error rather than an absence, because a project that might be there must not
  be written a second time.
- Container and compose **actions** follow the same distinction, because one
  runtime binary reaches two stores: `podman` run by the daemon user keeps its
  containers in that user's own store, and the same binary through `sudo -n`
  keeps root's. A step runs in the store that already holds its target and
  never in the other one as a fallback — `sudo podman` is not "the same
  container with more privilege", it is a different container store, so
  recreating into it (or from it, unprivileged) would leave two copies of the
  same project fighting for its published ports. The steps that need a store
  this daemon cannot reach say so and name the sudoers grant that would let it,
  instead of building the step somewhere else.
- The [container list](#snapshot-endpoints) reports what each runtime says
  about itself — `rootless`, `cgroups`, `compose_tool` — which is what tells an
  operator whose store the daemon is addressing, and why a podman without a
  systemd session repeats its cgroup fallback on every invocation.
- Setting a collector interval to `0` disables that collector. Collection is
  gated on active subscribers and never persists or writes to disk; metrics
  persistence and cloud publishing stay on `metricsInterval`.

### WebSocket terminal

`GET /api/v1/terminal` (WebSocket) serves an interactive login shell on the
host, so browser-based MaidKit builds — which cannot open the raw sockets
`dartssh2` needs — can attach a terminal without SSH. It is opt-in, off by
default, and unrelated to the cloud relay: nothing about a session, its output
or its audit entry ever leaves the host.

```text
GET /api/v1/terminal?shell=/bin/bash&user=deploy&cols=120&rows=40
Sec-WebSocket-Protocol: maidcafe.terminal.<base64url-unpadded secret>
```

The credential is the dedicated `daemon.terminal.secret` when set, and the
metrics secret otherwise. Browsers cannot set an `Authorization` header on a
WebSocket handshake, so they carry the secret as a subprotocol token
(`maidcafe.terminal.` + the secret encoded with unpadded base64url, e.g.
`metrics-secret` → `maidcafe.terminal.bWV0cmljcy1zZWNyZXQ`). Native clients
and tunnels may use `Authorization: Bearer <secret>` instead, and the server
echoes the offered subprotocol token back on the handshake response. Secrets
are never accepted in the query string or a cookie.

Query parameters:

- `shell` — an absolute path from `daemon.terminal.shells`; omitted uses the
  first entry. A path outside the allowlist returns `403`.
- `user` — a name from `daemon.terminal.users`; omitted (or an empty list) runs
  the shell as the daemon's own account. Anything else returns `403`.
- `cols`/`rows` — initial PTY size, `1..1000` and `1..500` (defaults `80x24`).
  A non-numeric or out-of-range value returns `400`.
- An unauthenticated handshake returns `401`, the endpoint being disabled
  returns `403 terminal disabled`, exceeding `maxSessions` returns `429`, and a
  platform without a PTY (Windows) returns `501`.

Frames:

| Direction | Frame | Meaning |
| --- | --- | --- |
| server → client | text `{"type":"hello","version":"v1","session":"<uuid>","shell":"/bin/bash","user":"maidcafe","cols":120,"rows":40}` | session opened; `user` is the account the shell runs as |
| server → client | binary | raw PTY output, in order, never coalesced or dropped |
| server → client | text `{"type":"exit","code":0,"reason":""}` then close `1000` | shell exited (or the session was closed); `reason` is empty for a normal shell exit and otherwise `idle timeout`, `lifetime exceeded`, `client closed`, `client unreachable`, `daemon shutdown` or `protocol error` |
| server → client | text `{"type":"error","message":"..."}`, then the `exit` frame, then close `1008` | protocol violation |
| server → client | text `{"type":"error","message":"..."}` then close `1011` | the shell failed to start (no session was created) |
| client → server | binary | keystrokes written to the PTY |
| client → server | text `{"type":"resize","cols":100,"rows":30}` | PTY resize, `1..1000` and `1..500` |

The server pings every 20s and closes a client that stops answering. Sessions
are bounded by `idleTimeout` (no traffic in either direction) and
`maxLifetime` (absolute age), and are recorded in `daemon.auditPath` as one
line per session with `source: "terminal"` — the summary only, never the
transcript. A PTY that fills up blocks the shell rather than dropping output,
so a slow client slows its own session and nobody else's.

Operator notes:

- `daemon.terminal.enabled` is off by default: enabling it grants an
  interactive shell on the host to whoever holds the credential, which is why
  `daemon.terminal.secret` is worth setting to keep a leaked metrics secret
  from reaching a shell.
- Run-as-user sessions use `sudo -H -u <user>`, exactly like actions with a
  `user`. They rely on the same NOPASSWD sudoers rule; without it, sudo's
  password prompt appears on the PTY (a terminal is the one place that is
  acceptable).
- Browser origins must be listed in `daemon.terminal.allowedOrigins`
  (`path.Match` against the `Origin` host, or `scheme://host` when the pattern
  contains `://`); an origin equal to the request host is always allowed, and a
  request without an `Origin` header (native client, tunnel) always is.
- Sessions are accepted from loopback, RFC1918, link-local and Tailscale
  addresses (`100.64.0.0/10`). `daemon.terminal.allowRemote = true` lifts that
  restriction, and only the TCP peer address is ever inspected —
  `X-Forwarded-For` is ignored.
- The daemon serves plain HTTP: a page delivered over HTTPS needs `wss://` and
  therefore the operator's own TLS front (`nginx`, `Caddy`, `tailscale serve`).
- Terminal sessions do not consume a `maxConcurrentRuns` slot and are not
  bounded by `scriptTimeout`: they are interactive and long-lived, so they are
  bounded by `maxSessions`, `idleTimeout` and `maxLifetime` instead.
- The endpoint only exists in the `http` transport; `stdio` has no listener.

### Cloud-relayed terminal

A daemon behind NAT cannot be dialed from a browser, so the same terminal can
instead reach it through the MaidCafe cloud. With `daemon.terminal.relay.enabled`
the daemon long-polls the cloud for handovers and, per accepted session, dials
one outbound `wss://<cloudUrl>/api/daemons/<id>/terminal/agent?session=<id>`
socket authenticated with the daemon cloud secret (`cloudUrl` and `cloudSecret`
are required). The browser attaches to
`wss://<cloud-host>/api/daemons/<id>/terminal` with a cloud-minted session
ticket; the cloud pairs the two sockets and forwards frames verbatim, so the
frames, and therefore the client, are exactly the ones above. The daemon only
ever dials out — it never listens for a relayed session — so this works in the
`stdio` transport too.

Relayed sessions are the same sessions: they share `maxSessions`, `idleTimeout`
and `maxLifetime`, obey the `daemon.terminal.shells` and `daemon.terminal.users`
allowlists, and land in `daemon.auditPath` with `source: "terminal"` and
`invoked_by` set to the cloud identity prefixed with `cloud:` (e.g.
`cloud:alice@solsynth.dev`). `daemon.terminal.allowedOrigins` does not apply:
the browser talks to the cloud, and the daemon's own socket sends no `Origin`.

- `daemon.terminal.relay.enabled` (default `false`) — serve relayed sessions.
- `daemon.terminal.relay.users` — optional allowlist of cloud identities; empty
  accepts any identity the cloud authorizes, which is still the daemon's own
  gate: the cloud only authorizes members of the daemon's workspace, and a
  workspace is not the same trust boundary as a shell.
- `daemon.terminal.relay.pollWait` (default `20s`, `3s`–`25s`) — how long one
  pickup long-poll is held, which bounds start latency and idle cloud traffic.

The cloud terminates TLS and pumps the frames, so a relayed session passes
through it in the clear: it can read every keystroke and every byte of output,
including a password typed at a `sudo` prompt. Neither side persists a
transcript — the audit entry is the session summary only.

### File API

The file manager and the editor reach a host over SFTP, which a browser build
cannot open. With `daemon.files.enabled` the daemon serves the same operations
over plain HTTP (and over the SSH stdio pipe), so a MaidKit web build can
browse, read, edit, upload and delete files on a managed host without an SSH
session.

```text
GET    /api/v1/files/roots
GET    /api/v1/files/list?path=<abs>
GET    /api/v1/files/stat?path=<abs>&follow=<bool>
GET    /api/v1/files/content?path=<abs>&offset=<n>&limit=<n>   # raw bytes
POST   /api/v1/files/read                                      # JSON, base64
POST   /api/v1/files/write                                     # JSON, base64
PUT    /api/v1/files/content?path=<abs>                        # raw body
POST   /api/v1/files/mkdir    {"path","parents"}
POST   /api/v1/files/move     {"from","to"}
POST   /api/v1/files/copy     {"from","to","overwrite"}
POST   /api/v1/files/delete   {"path","recursive"}
```

Each mount also accepts an alternate verb (the read routes `POST`, the write
routes `POST`/`PUT`, delete `POST`/`DELETE`), so a client whose HTTP helper
issues only one method is not locked out of an operation it is allowed to make.

Authentication is `Authorization: Bearer <secret>`, where the secret is
`daemon.files.secret` when set and `daemon.metricsSecret` otherwise; a
`Sec-WebSocket-Protocol: maidcafe.files.<base64url>` token is accepted too, the
same accommodation the terminal handshake makes. `GET /api/v1/files/content`
additionally accepts `?token=<secret>`, because a download or an image load
cannot carry a header at all — and that is the only route that does: a URL
which leaks into a proxy log or a `Referer` can read a file, never change one.
Every mutation presents the credential in a header, and a JSON mutation body
must additionally carry the usual `X-MaidCafe-Signature` HMAC, so a credential
lifted in transit cannot be replayed against a body it was never signed for.

```sh
API=https://host/api/v1/files
AUTH='Authorization: Bearer <files-secret>'

curl -sS "$API/roots" -H "$AUTH"
curl -sS "$API/list?path=/srv/app" -H "$AUTH"
curl -sS "$API/read?path=/srv/app/index.html" -H "$AUTH"
curl -sS "$API/content?path=/srv/app/logo.png&offset=0&limit=2048" -H "$AUTH" -o window.bin

# Mutations present the credential in a header; a JSON body is signed too.
BODY='{"path":"/srv/app/notes.txt","content":"aGVsbG8K"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac '<metrics-secret>' -hex | awk '{print $NF}')
curl -sS -X POST "$API/write" -H "$AUTH" -H "X-MaidCafe-Signature: $SIG" \
  -H 'Content-Type: application/json' --data-binary "$BODY"

curl -sS -X PUT "$API/content?path=/srv/app/upload.bin" -H "$AUTH" --data-binary @upload.bin
curl -sS -X POST "$API/delete?path=/srv/app/upload.bin" -H "$AUTH"
```

Containment is layered:

- Every path must be absolute and lexically inside one of `daemon.files.roots`;
  anything else is `403` before the filesystem is touched, and a configured
  root itself can never be written, moved, copied or deleted through the API.
- The I/O then runs through `os.Root`, which refuses any access that resolves
  outside the root — including through a symbolic link planted inside it — so
  the kernel closes the race the lexical check cannot. `stat` reports a link
  as `type: "symlink"` with its raw `link_target`, and `target_type` only when
  the destination still resolves inside the root.
- Operations run as the daemon's own account. The API never elevates, so it
  can touch exactly what that account can touch.
- `daemon.files.allowWrite` (default `false`) gates every mutation, so a root
  can be published read-only.

Reads are windowed rather than whole-file: `offset`/`limit` return a slice,
`size` reports the file's real length, and a whole-file read of something
larger than `maxReadBytes` is refused with `413` instead of silently
truncated. `GET /api/v1/files/content` carries the total size in
`X-MaidCafe-File-Size`. A listing reports `truncated: true` when a directory
holds more entries than `maxListEntries`; a whole-file copy (move/copy) is not
bounded by `maxReadBytes`, which caps one request, not one operation.

Every call is appended to `daemon.auditPath` with the action slug
(`files.list`, `files.write`, …), the resolved path in `target`
(`from -> to` for a move or copy), `source` `http` or `stdio`, and the usual
`ok`/`duration_ms`/`error` fields.

- `daemon.files.enabled` (default `false`) — serve the API at all.
- `daemon.files.roots` — required when enabled; a list of
  `{ path, privileged, profile }`. `path` must be an absolute, existing
  directory; a root the daemon account cannot read is a root the API cannot
  serve. `privileged` routes writes through the helper (see
  [Privileged operations](#privileged-operations)) and `profile` names the
  helper profile that authorizes them.
- `daemon.files.secret` — optional dedicated credential.
- `daemon.files.allowWrite` (default `false`) — allow mutating operations.
- `daemon.files.maxReadBytes` / `maxWriteBytes` (default 8 MiB) — per-request
  caps.
- `daemon.files.maxListEntries` (default 5000) — per-listing cap.

Over the stdio transport the same operations are actions named `files.list`,
`files.read`, `files.write`, `files.mkdir`, `files.move`, `files.copy`,
`files.delete` and `files.roots`, carrying the same parameters in the request
body. The pipe already required the account's SSH credentials, so those
actions carry no signature — but the `[daemon.files]` policy still applies, so
the pipe cannot reach outside the configured roots either.

Not served, deliberately: permission/ownership changes (`chmod`, `chown`) and
archive create/extract. The file manager performs those over SSH today, and a
browser build without SSH reports them unavailable rather than silently
downgrading.

The API is reachable where the daemon is reachable: directly over HTTP(S), and
over the stdio pipe for a client that already has SSH. There is no cloud-relayed
file transport (the terminal has one; files do not), so a browser client needs
an endpoint it can dial — the same TLS-fronted address the rest of the daemon
API is served at.

### Privileged operations

Some of what the file manager edits is not writable by the daemon account: an
nginx or Caddy configuration, a systemd unit. Declaring such a root
`privileged` routes its writes through `maidkit-priv`, the one component that
runs as root:

```toml
[daemon.files]
enabled = true
allowWrite = true

[[daemon.files.roots]]
path = "/etc/nginx"
privileged = true
profile = "nginx"
```

```sh
sudo install -o root -g root -m 0755 maidkit-priv /usr/local/libexec/maidkit-priv
sudo install -o root -g root -m 0644 config.priv.example.toml /etc/maidkit/priv.toml
sudo install -o root -g root -m 0440 /dev/stdin /etc/sudoers.d/maidkit-priv \
  < <(maidkit-priv sudoers maidcafe)
```

```toml
# /etc/maidkit/priv.toml
[[profiles]]
name = "nginx"
path = "/etc/nginx"
modes = ["0644", "0640"]
```

The helper takes a profile **name**, never a path: it resolves that name
against its own root-owned profile file, which the daemon cannot write. A
sudoers wildcard is therefore safe, because the helper — not the pattern — is
the boundary. Writes land through a temporary file renamed into place, confined
by `os.Root`, and every attempt is journalled with sudo's own record of who
invoked it. Reads stay unprivileged; `allowWrite = false` still refuses
privileged writes; `move`, `copy` and recursive delete answer `501` inside a
privileged root rather than falling back to the daemon account.

[`docs/PRIVILEGED.md`](docs/PRIVILEGED.md) has the full model: why a stored root
password or root SSH key is worse than this (a password the daemon can read is
a larger grant than the write it enables), the profile and mode rules, the exit
codes, and what the helper deliberately refuses.

### Snapshot endpoints

The same state the stream pushes is also available as one-shot responses, so
clients can paint first data from the daemon instead of an SSH fallback. They
reuse the stream collectors' probe cache and rate limits:

- `GET /api/v1/containers` — same payload as the `containers` event: a
  `runtimes` list covering every runtime found on the host (podman first),
  each with `runtime`, `available`, `error`, `containers`, and what the
  runtime says about itself: `rootless` (absent when it did not answer),
  `cgroups` (the manager it will use — `systemd` or `cgroupfs`) and
  `compose_tool` (the tool a stack action for that runtime runs first, e.g.
  `podman-compose` or `podman compose`). A podman that reports `cgroupfs`
  because the user has no systemd session repeats that fallback, with the
  remedy it suggests, on every invocation it makes — the daemon's own task
  logs no longer add anything to it beyond the runtime's output.
- `GET /api/v1/images` — same payload as the `images` event: a `runtimes`
  list, each with `runtime`, `available`, `error` and `images`.
- `GET /api/v1/processes` — same payload as the `processes` event. `?limit=N`
  overrides the daemon's configured `processesLimit`: `0` returns the complete
  process table (no cap), a positive value keeps the top N CPU consumers.
  Valid values are `0` or `1..10000`; anything else returns `400`.
- `GET /api/v1/systemd` — same payload as the `systemd` event.

All four are authenticated with the same metrics secret and cost one
collection on demand; repeated calls are rate-limited by the shared probe
cache.

### Container detail

Beyond the list, the daemon answers the per-container questions a panel asks,
one container at a time. Every one of them resolves its container the same way
— by full id, id prefix or name, through the daemon's own container snapshot —
so an unknown container is `404`, a runtime that refuses is `502` with the
runtime's own message, and a request never has to guess which runtime holds
what:

- `GET /api/v1/containers/:id/inspect` — the runtime's own inspect payload
  (`inspect`), passed through unmodified as `{container, name, runtime,
  inspect}`. It includes the container's environment, so it is served only to a
  caller holding the daemon credentials.
- `GET /api/v1/containers/:id/stats` — a one-shot resource sample (`stats
  --no-stream`), normalized across the two runtimes: `cpu_percent`,
  `memory_usage_bytes`, `memory_limit_bytes`, `memory_percent`,
  `network_input_bytes`, `network_output_bytes`, `block_input_bytes`,
  `block_output_bytes`, `pids`. A measurement the runtime cannot report (a
  rootless runtime prints `--`) is `null`, never `0`.
- `GET /api/v1/containers/:id/logs?lines=N` — the tail window, oldest first.
  `source=captured` (the default) is the daemon's own disk-backed history and
  survives a container restart; `source=runtime` asks the runtime directly
  (`logs --tail N --timestamps`), which also answers for a container the daemon
  never tailed — one whose `logsInterval` is `0`, or one that started a moment
  ago. Both what an application writes to stdout and what it writes to stderr
  are included; `lines` is `1..1000`, default `200`.
- `GET /api/v1/containers/:id/update-check` — whether the image's registry
  publishes something newer than this container runs, checking now when the
  cached answer is older than a minute.
- `GET /api/v1/updates` — every cached update answer in one request, with
  `interval_seconds` (the cadence they are refreshed on) and no registry
  traffic at all.

Like the snapshots, all of these are authenticated with the metrics secret and
each costs one runtime command (the update check costs a registry request too).

#### Update checks

A container's update status is the comparison of two digests: the one the
runtime recorded when it pulled the image, and the one the image's registry
publishes for that tag right now.

```json
{
  "container": "abcdef…", "name": "web", "runtime": "podman",
  "image": "docker.io/library/nginx:1.25",
  "checked_at": "2024-05-01T10:00:00Z",
  "outdated": true,
  "pinned": false,
  "restart_required": false,
  "local_digest": "sha256:aaaa…", "remote_digest": "sha256:bbbb…"
}
```

- `outdated` is `null` when the question cannot be answered, and `error` says
  why (an unreachable registry, a private repository read anonymously, an image
  that was built or imported on the host and so has no registry digest). A
  blank badge is honest; a wrong "up to date" is not.
- `pinned` marks a container created from a digest-pinned reference, which is
  never outdated — the digest is what the operator asked for.
- `restart_required` marks a container that is already behind the image the
  local store holds, so recreating it applies an image that is already there.
- A local platform digest that is still a member of the tag's multi-platform
  index counts as current: the tag gained another platform, not a new image for
  this host.
- Checks read manifest metadata over HTTPS from the Docker Registry HTTP API
  v2 (`/v2/<repository>/manifests/<tag>`), following the registry's bearer
  challenge, and use the daemon account's own `docker`/`containers` auth file
  when it holds a credential for that registry. Nothing is pulled, nothing is
  written to the image store, and no credential is ever sent in the clear: a
  plain-HTTP registry is reported as an error rather than dialled, and the
  token endpoint the registry names has to use the same scheme as the registry
  itself.
- The comparison runs on `daemon.updateCheckInterval` (default `6h`, `0`
  disables it) for every container the daemon can see, one at a time, so a host
  restarting often does not re-check every registry on every boot (the first
  round waits 30s). On-demand checks are floored at one minute per container, so
  a button cannot drive one registry request per click.

Webhook secrets are separate from the metrics secret. The daemon does not
provide an HTTP configuration API; MaidKit updates the managed TOML
configuration over SSH and restarts the service when needed.

### Native host operations

The daemon also executes typed mutations directly — container lifecycle and
image updates, process kill, systemd unit actions, compose project actions,
package operations and firewall rules — mirroring what MaidKit's SSH layer can
do, so a managed host can be operated through the daemon (locally over HTTP,
over the SSH stdio pipe, or remotely through the cloud relay) without a
workstation SSH session. Unlike script actions, native ops never interpolate
caller input into a shell: targets are validated against the same patterns
MaidKit enforces client-side, and commands run directly with
`exec.CommandContext` (no `sh -c`). Root-owned resources are reached with a
`sudo -n` retry mirroring the collectors; under the shipped systemd unit's
`NoNewPrivileges` that retry is inert, so such ops fail with a clear error and
MaidKit falls back to SSH. A host that installs the privileged helper can route
a family through it instead — see
[Privileged operations](docs/PRIVILEGED.md).

```text
POST   /api/v1/containers/:id/:action   action = start|stop|restart|pause|unpause|kill|remove|pull|update
POST   /api/v1/processes/:pid/kill
POST   /api/v1/systemd/:unit/:action    action = start|stop|restart|reload|enable|disable
POST   /api/v1/compose/:project/:action action = up|stop|restart|pull|recreate|update
POST   /api/v1/packages/:action         action = refresh|upgrade|install|remove
POST   /api/v1/firewall/:action         action = enable|disable|allow|deny|delete
GET    /api/v1/compose/stacks           the managed stack registry, with what each stack runs
POST   /api/v1/compose/stacks/scan      assign stacks: {"path": "…"} | {"roots": ["…"]} | neither (configured roots)
DELETE /api/v1/compose/stacks/:project  unassign one stack
GET    /api/v1/tasks                    the recent operations, newest first
GET    /api/v1/tasks/:id                one operation, with its output since ?since=<bytes>
POST   /api/v1/tasks/:id/cancel         stop a running operation
```

All are authenticated with the metrics secret and a body signature, like the
actions route. `POST /api/v1/containers/:id/remove` accepts `{"force":
true}` (mapped to `rm -f`); `POST /api/v1/compose/:project/:action` takes an
optional `{"directory": "<absolute path>"}` — compose resolves its file from the
working directory, so a caller that sends one makes the path the daemon's
working directory, and a caller that sends none runs the project where the
managed stack registry says it lives. Container ops
resolve the runtime with the shared probe (podman first) and fall back to the
other runtime when the container is not found there. Every run is appended to
the audit log under its slug (`container.restart`, `process.kill`, …).

#### Updating a container

`container.pull` and `container.update` are the quick actions behind the update
badge. Both read the container's own configuration first — which runtime holds
it, and the image reference it was created from — instead of taking an image
name from the caller, and a pull is passed to the runtime exactly as the
container records it, so a short name resolves through the host's registry
configuration the same way the container's creation did.

- `container.pull` runs `pull <reference>` on the runtime that holds the
  container, with the usual `sudo -n` variant. That is the whole operation: it
  makes the newest image available locally without touching what is running.
- `container.update` is a two-step operation: pull the service's image, then
  recreate the container so it runs that image. Both steps report their output.
  It only handles **compose-managed** containers, whose identity comes from
  their labels (`com.docker.compose.*`, or podman compose's
  `io.podman.compose.*`): the step is
  `compose -p <project> [-f <file>…] pull <service>` followed by
  `compose … up -d --force-recreate <service>`, run in the project directory,
  with every label value validated before it reaches an argv.
  `--force-recreate` is deliberate: the caller asked for the update, so the
  container must end up on the pulled image even when compose cannot tell the
  image changed.
- The project directory comes from the container's own labels when they record
  one, and from the [managed stack registry](#managed-compose-stacks)
  otherwise. The config files may be recorded either way: the standalone
  `podman-compose` (and docker compose before 2.x) records them exactly as the
  operator typed them (`-f docker-compose.yml` stays `docker-compose.yml`), so
  a relative entry is resolved against the recorded directory instead of
  failing the update. A recorded path may not contain `..`: it is the
  operator's own file list, but the directory it is resolved in is the
  daemon's working directory. When the labels record no file list at all, the
  step runs `compose` in the directory and lets compose read the files it would
  read by itself.
- The step runs in the **store that owns the project**, which is not always the
  store the named container was read from. The daemon user's podman and root's
  podman are two stores with their own containers, and a project belongs to
  exactly one: running `up -d --force-recreate` in the other one does not
  retry it with less privilege, it creates the whole project a second time —
  same project name, same container names — where it cannot bind the ports the
  original holds. So the ladder is built for one store and never offers the
  other as a fallback, and a project that exists in *two* stores is refused
  (`400`) with both named: choosing silently is how the second copy appeared in
  the first place.
- Which tool runs depends on the store, and it stays the tool the project is
  made with. In the daemon user's own store the tools are tried in the order
  above, unprivileged. In root's store every attempt is `sudo -n`-wrapped and
  the order is a *requirement* rather than a fallback: the tool that would run
  first must be one `sudo -n` may run, and when it is not, the operation is
  refused (`400`) with that tool's sudoers line instead of being handed to the
  next tool. Substituting the runtime's own `compose` subcommand for a stack the
  operator's `podman-compose` created would give one project two compose
  implementations — a different version, a different argument parser, a
  different idea of the project's name — and that is how a stack comes apart. A
  host that has no standalone tool has only the runtime's subcommand to run, and
  then there is nothing to mix.
- The compose command is chosen from the runtime's own name, so a docker host is
  never sent to `podman-compose`: the two write to different image stores, and
  pulling into the wrong one would leave the container exactly where it was.
  **Podman** runs `podman-compose` when the host has it, with `podman compose`
  only as the fallback — podman does not implement compose, so its subcommand is
  a wrapper that execs whichever provider is installed, normally that same
  `podman-compose`. Calling the tool directly is one layer less, and that layer
  is where an argument it evaluates and forwards can kill the command. That
  order is what runs in root's store as well, where a tool the daemon may not
  run through `sudo -n` is a refusal rather than a reason to reorder — see the
  store rule above.
  **Docker** runs `docker compose` first, because there the plugin *is* the
  implementation, with `docker-compose` as the fallback for a host that has only
  it.
- Neither form is given an ANSI flag. The providers spell it differently
  (`--ansi never` for the compose plugin, `--no-ansi` for the standalone tools),
  so the spelling one accepts ends the command on the other before it starts:
  `podman-compose: error: argument command: invalid choice: 'never'`. Nothing
  needs it — the daemon never gives compose a terminal, and compose's own
  `ansi: auto` disables colors when it writes to a pipe, which is the only place
  this output ever goes.
- A container that is not compose-managed is refused (`400`) with the reason.
  Neither runtime can recreate a plain `docker run` container from its own
  configuration, and replaying `inspect` into a `run` argv silently drops
  whatever the daemon does not model — a container that comes back missing a
  device, a sysctl or a network alias is worse than one that was not touched.
  `container.pull` still works for those: pull, then recreate it where its
  lifecycle is declared.
- `compose.update` is the same two steps for a whole project — every service,
  no container named. It is the one call behind "upgrade this stack", and it
  takes its directory from the registry unless the caller sends one.

Both run under the executor's concurrency slot and audit trail like every other
native op, with a 5 minute bound (a pull is slow by nature), so they are also
available as scheduled jobs and through the cloud relay:
`name: container.update` with `{"id": "web"}` in the body.

#### Following a long operation

The operations that pull an image — `container.pull`, `container.update`,
`compose.pull`, `compose.update`, `compose.recreate` — are measured in minutes,
which no HTTP client waits for: MaidKit's own client gives up after ten seconds
of silence (its read timeout), and a browser or a proxy in between may give up
sooner. Those five answer `202` with a **task** instead of holding the request:

```json
{
  "ok": true,
  "task": {
    "id": "9f2c…", "name": "compose.update", "display_name": "Update compose stack",
    "target": "myapp", "source": "http", "status": "running",
    "started_at": "2026-10-03T11:20:31Z",
    "stages": [
      {"label": "pull", "status": "running", "started_at": "2026-10-03T11:20:31Z"},
      {"label": "recreate", "status": "pending"}
    ],
    "ok": false, "exit_code": 0, "stdout": "", "stderr": "", "output_bytes": 412
  },
  "output": "Pulling web …", "output_from": 0
}
```

- The run belongs to the **daemon**, not to the connection that asked for it: a
  client that disconnects mid-pull no longer aborts the operation halfway
  through. What stops it is `POST /api/v1/tasks/:id/cancel`, the 5 minute bound
  it has always had, or the daemon shutting down (which cancels everything still
  running, since a task is the daemon's work and not the client's).
- `GET /api/v1/tasks/:id` returns the task with the output written since
  `?since=<bytes>`: a client tracks the byte count it has shown and asks for
  what came after it. Output is retained as a 32 KiB tail, so a client that has
  fallen behind gets the tail with `"output_truncated": true` and replaces what
  it holds instead of appending. Without `since`, the response carries the tail.
- `stages` is the plan the run is following, not a log of what has happened:
  a stage is `pending`, `running`, `succeeded` or `failed`, and the stages a
  failed or cancelled run never reached stay `pending`. `pull` and `recreate`
  are the labels used so far; a one-stage operation names its single stage after
  its verb.
- On completion `status` is `succeeded`, `failed` or `canceled`, with the same
  `ok`/`exit_code`/`stdout`/`stderr` a request-scoped run returns, and `error`
  saying why when it did not succeed.
- Tasks are in memory: a restart loses them along with the runs themselves, and
  the store keeps the 32 most recent finished ones (`GET /api/v1/tasks` is how a
  client that lost an id finds a run again).
- The transports that *can* hold a request open keep the old behaviour: the
  same operation over stdio (SSH) or through the cloud relay still returns its
  result in the response, because nothing on those paths times out. Tasks exist
  for the HTTP clients that do.

#### Managed compose stacks

A daemon can be told which compose projects it manages. That assignment is what
makes a container whose own labels do not point at its project updatable at all,
and it is the directory `compose.update` runs in.

```text
POST   /api/v1/compose/stacks/scan   {"path": "/opt/stacks/web"} | {"roots": ["/opt", "/srv"]} | {}
GET    /api/v1/compose/stacks
DELETE /api/v1/compose/stacks/:project
```

- A scan walks the starting points it was given — one `path`, a list of `roots`,
  or `daemon.compose.scanRoots` when the request names neither — and reads every
  `*.yml`/`*.yaml` below them that declares a `services:` section. A project is
  one directory: its files are recorded in the order compose merges them (base
  files first, overrides last), its services are the union of what those files
  declare, and its name is the one a file declares or, failing that, the
  directory's name — the same fallback compose itself applies. A declared name
  outranks the directory, because compose refuses `-p <directory>` against a
  file that declares another name.
- The walk is bounded and says so: `daemon.compose.scanDepth` (default 3) below
  each root, at most 400 candidate files read per scan, and a 30 second budget.
  A depth sent by a request is clamped to 12, so a client cannot ask for an
  unbounded walk by accident.
- What a scan finds is assigned and persisted:
  `daemon.compose.stacksPath` (default
  `/var/lib/maidcafe/compose-stacks.json`) is rewritten atomically on every
  change and read at start. A managed stack whose directory has since
  disappeared is dropped by the next scan; a stack outside that scan's roots is
  left alone, because scanning one starting point says nothing about the others.
  `DELETE` unassigns one stack and touches nothing on the host.
- `GET /api/v1/compose/stacks` is the registry joined to the daemon's own
  container snapshot: per stack, the containers carrying its project label, how
  many of them are running, and the files and services the scan recorded. That
  is the health view a client paints without a second request, and it is served
  even when a runtime cannot be listed.
- Nothing is guessed at request time. A container whose project no scan assigned
  is refused with that reason rather than updated in a directory the daemon
  picked for it, and an ambiguous scan is not a thing this daemon has: one
  project is one directory, and the registry holds what the operator's own scan
  found.

The package and firewall routes carry their operands in the body:

```json
{"name": "nginx"}                                   // install and remove only
{"port": "443", "protocol": "tcp", "source": "any"} // allow, deny and delete
{"rule_action": "allow"}                            // delete only
```

A package name must be the distribution's own name for a package — a `.deb`
path, a URL, a version pin (`nginx=1.2.3`), an architecture qualifier
(`nginx:amd64`) and a leading-dash argument are all refused, because each names
a *source* rather than a package. A firewall rule is a port (a number, a
`start:end` range or a ufw service name), a protocol (`tcp`, `udp` or `any`)
and a source (`any`, an address or a CIDR block); `delete` also needs the
action it is deleting, because ufw identifies a rule by its full text. The
manager and the backend are not caller-selectable: without the helper they are
detected from what the host has installed, and with it they come from the
helper's own grant file.

The slugs are also valid through the cloud relay (`POST
/api/daemons/:id/webhook-requests` with `name: container.restart` and the
identity in the body: `{"id": "…"}`, `{"pid": 123}`, `{"unit": "…"}`,
`{"project": "…", "directory": "…"}`, `{"name": "nginx"}`, `{"port": "443",
"protocol": "tcp", "source": "any"}`) and over the SSH stdio pipe (request
`action` = the slug, same body). They are reported to the cloud on every
metrics tick, so the cloud page lists them as invocable, and the slugs are
reserved — a webhook or action may not use one, keeping the relay name space
unambiguous.

### Scheduled jobs

Jobs run a configured action or native operation on a schedule. Each job is a
`<slug>.toml` fragment under `daemon.jobsDir` (default
`/etc/maidcafe/jobs`), merged at load like action and alarm fragments, or an
inline `[[daemon.jobs]]` entry:

```toml
name = "nightly-backup"
schedule = "0 3 * * *"     # five-field cron, or "@every 30s", "@hourly"...
action = "backup"          # a configured action name or native op slug
body = { job = "incremental" }
enabled = true
notifyOnFailure = true
```

- `schedule` accepts standard five-field cron expressions and robfig/cron
  descriptors (`@every 30s`, `@hourly`, `@daily`).
- `action` targets a configured action or a native operation slug
  (`container.restart`, `process.kill`, …); `body` is the JSON object passed
  to it (script template variables or native-op identity parameters).
- Runs go through the same executor as manual invocations: audit log entries
  with `source: "job"` and `invoked_by: "job:<name>"`, the shared concurrency
  slot and per-run timeout. Overlapping runs are skipped, never stacked.
- `notifyOnFailure` publishes a `job.failure` notification when a run fails.

### Container logs

- The daemon tails every running container on `daemon.logsInterval` (default
  `30s`, `0` disables) with `<runtime> logs --since <cursor> --timestamps <id>`
  (initial run backfills the last 200 lines), reusing the runtime probe and
  `sudo -n` retry. Both streams are captured: a runtime's `logs` writes what an
  application logged to stderr to its own stderr, so a tail that read only
  stdout would drop every line such an application wrote. New lines are:

- appended to per-container JSONL files under `daemon.logsDir` (default
  `/var/lib/maidcafe/logs`), rotated at 512 KiB with one generation and
  pruned by `metricsRetentionDays`;
- pushed to SSE `logs` subscribers as `{"container": "<id>", "lines":
  [{"ts": ..., "line": ...}]}` frames;
- served by `GET /api/v1/containers/:id/logs?lines=N` (1–1000, default 200)
  as the tail window, oldest first. `?source=runtime` answers with a live
  `logs --tail N` read from the runtime instead — the same shape, with
  `source` naming which one answered;
- uploaded to the cloud only when `logsUploadEnabled = true`, batched every
  `logsUploadInterval` (default `30s`) up to `logsUploadBatchLines` (default
  100), through the daemon-secret endpoint `POST
  /api/daemons/:id/logs`. Failed uploads remain queued in a bounded local
  buffer and retry; old cloud rows follow the workspace
  `metrics_retention_days` quota.

Log alerts are daemon-side regex fragments under `daemon.logAlertsDir`
(default `/etc/maidcafe/log-alerts`):

```toml
name = "errors"
pattern = "(?i)\\berror\\b|\\bfail(ed|ure)?\\b"
container = "web"       # omit to match every container
title = "Web container error"
cooldownSeconds = 300
enabled = true
```

Each matching line publishes `daemon.log_alert` through the existing cloud
notification pipeline. Alerts are local and regex-based; the raw line is
included as the bounded notification body, with alert/container/timestamp
metadata. Cooldowns are tracked independently per alert and container.

### Hot reload and the config API

Three triggers re-read the configuration: file changes (the config file and
the action/alarm/job fragment directories are watched, debounced), SIGHUP
(`systemctl reload maidcafe-daemon` — the shipped unit carries
`ExecReload=/bin/kill -HUP $MAINPID`), and the config API. A reload validates
the result and swaps the reloadable components atomically: webhook/action
tables, jobs, alarms, runtime groups, watched processes, collector intervals,
timeouts, body limits, concurrency, and the cloud endpoint. A failed load or
validation keeps the previous state; the daemon never restarts itself.

`GET /api/v1/config` returns the merged configuration with secrets redacted
(metrics/cloud secrets and webhook secrets are never exposed) plus the
fragment contents. `PATCH /api/v1/config` patches a safe subset of `[daemon]`
keys — intervals, `processesLimit`, `scriptTimeout`, `maxBodyBytes`,
`maxConcurrentRuns`, `runtimes`, `watchedProcesses`, `cloudUrl`,
`cloudSecret` — into `config.toml` (preserving every other line verbatim),
then hot-reloads; a failed reload restores the previous file. Unsupported or
invalid keys are rejected with `400` before any write. Every interval is
patchable, `updateCheckInterval` included.

Restart-required settings — `transport`, `listen`, `metricsSecret`, storage
paths, audit path — are read-only over the API, and patching them is
rejected. The daemon writes its own config file, so the systemd unit grants
it `ReadWritePaths=/etc/maidcafe`; the config API needs the daemon user to be
able to write that directory (a hand-rolled install must allow it too).

Example response:

```json
{
  "ok": true,
  "name": "backup",
  "exit_code": 0,
  "stdout": "...",
  "stderr": ""
}
```

## Configuration

The daemon's configuration is documented by
[`config.daemon.example.toml`](config.daemon.example.toml); the privileged
helper's profile file by [`config.priv.example.toml`](config.priv.example.toml)
and [`docs/PRIVILEGED.md`](docs/PRIVILEGED.md).

Use [`config.cloud.example.toml`](config.cloud.example.toml) for cloud mode and
[`config.daemon.example.toml`](config.daemon.example.toml) for daemon mode.
Configuration is typed TOML loaded through Viper and can also be selected with
`CONFIG_PATH`.

Cloud requires:

- `database.dsn`
- `auth.target`
- `workspace.target`
- `http.port` (default `8080`)

Daemon requires:

- `daemon.id`
- Valid positive durations and resource limits.
- Webhook names matching `[A-Za-z0-9._-]+`.
- Absolute webhook command paths.
- Unique webhook names.

Daemon cloud publishing is optional. An empty cloud URL and secret are valid.

`daemon.terminal.*` is optional and disabled by default; enabling it requires
`daemon.terminal.shells`. The keys are `enabled`, `secret`, `shells`, `users`,
`cwd`, `env`, `allowedOrigins`, `allowRemote`, `maxSessions` (1–8, default 2),
`idleTimeout` (default `15m`) and `maxLifetime` (default `8h`). The
cloud-relayed part adds `relay.enabled` (default `false`, requires
`daemon.cloudUrl` and `daemon.cloudSecret`), `relay.users` and `relay.pollWait`
(default `20s`); see [WebSocket terminal](#websocket-terminal) for what they
gate.

## Running locally

```sh
go run ./cmd/cloud --config config.cloud.example.toml
go run ./cmd/daemon --config config.daemon.example.toml
```

Useful Make targets:

```sh
make build
make build-cloud
make build-daemon
make test
make tidy
```

## Cloud deployment with Docker

The Docker image builds **cloud mode only** and uses a non-root distroless
runtime image:

```sh
docker build -t maidcafe-cloud .
docker run --rm \
  -p 8080:8080 \
  -v "$PWD/config.cloud.toml:/etc/maidcafe/config.toml:ro" \
  -e CONFIG_PATH=/etc/maidcafe/config.toml \
  maidcafe-cloud
```

For PostgreSQL plus the cloud service, copy
[`docker-compose.example.yml`](docker-compose.example.yml), then replace the
example database credentials and Solar auth target.

## Daemon deployment with systemd

The daemon is intended to run directly on managed hosts under systemd, not in
the cloud Docker image.

Create a service account:

```sh
sudo useradd --system --home /var/lib/maidcafe --create-home maidcafe
```

Build and install:

```sh
make build-daemon
sudo install -o root -g root -m 0755 bin/maidcafe-daemon /usr/local/bin/maidcafe-daemon
sudo install -d -o root -g maidcafe -m 0750 /etc/maidcafe
sudo install -o root -g maidcafe -m 0640 config.daemon.toml /etc/maidcafe/config.toml
sudo install -o root -g root -m 0644 deploy/maidcafe-daemon.service \
  /etc/systemd/system/maidcafe-daemon.service
sudo systemctl daemon-reload
sudo systemctl enable --now maidcafe-daemon
```

The unit runs as `maidcafe`, restarts after failure, and applies systemd
hardening. Webhook commands must be readable and executable by that account.

Actions that run as another account (`user = "..."` in `[[daemon.actions]]`
or `[[daemon.webhooks]]`) are executed through sudo and render their
substituted scripts under `/etc/maidcafe/actions/run`. For those, the unit
needs `NoNewPrivileges=false` (sudo's setuid bit) and `ReadWritePaths=/etc/maidcafe/actions`,
and the daemon account needs a sudoers rule such as:

```sh
sudo install -o root -g root -m 0440 /dev/stdin /etc/sudoers.d/maidcafe-actions <<'EOF'
maidcafe ALL=(deploy) NOPASSWD: /etc/maidcafe/actions/run/*, /etc/maidcafe/actions/*
EOF
```

The two specs matter: user-mode script actions render their substituted body
under `/etc/maidcafe/actions/run/`, and sudoers wildcards do not cross `/`.

The same rule must exist for the SSH user that runs the daemon in `stdio`
transport mode. MaidKit deploys all of this automatically when an action
selects a run-as user.

Container and image reads, and every container or compose step, can also reach
**root's** container store through sudo. That is how a daemon running as
`maidcafe` lists and manages stacks an operator started as root:

```sh
sudo install -o root -g root -m 0440 /dev/stdin /etc/sudoers.d/maidcafe-containers <<'EOF'
maidcafe ALL=(root) NOPASSWD: /usr/bin/podman, /usr/bin/docker
EOF
```

Root's store is where the daemon *reads* when its own store has nothing to
show, and it is the only store a project that lives there may be written to.
Two grants are in play and they do different things: the runtime is what lets
the daemon see root's containers at all, and the **compose tool the stacks were
deployed with** is what lets it update them — in that store, through that tool,
never by substituting another one:

```sh
sudo install -o root -g root -m 0440 /dev/stdin /etc/sudoers.d/maidcafe-containers <<'EOF'
maidcafe ALL=(root) NOPASSWD: /usr/bin/podman, /usr/bin/docker
maidcafe ALL=(root) NOPASSWD: /usr/local/bin/podman-compose
EOF
```

The second line is the one that matters for a stack built with the standalone
tool. Without it the daemon does *not* fall back to `podman compose`: it refuses
the step and prints the line to add, because a project that one tool created
must not be recreated by another. Adjust the path to where the host keeps its
tool (`command -v podman-compose`); a host that has no standalone tool needs no
such line — the runtime's subcommand is then the only compose tool there is.

The tradeoff is worth stating plainly: `sudo podman` is root-equivalent for
that binary — a container can mount the host filesystem and run as root inside
it — so this rule gives the daemon the same reach as running it as root. A
host that should not extend that reach has two coherent options: run the
daemon as root itself (one store, and the rule is unnecessary), or keep its
stacks in the daemon user's own rootless store and grant nothing, in which
case the daemon can only ever see the containers that account runs.

With the rule in place, a podman that reports `rootless` is still the daemon
user's own store for everything the daemon *writes*; the elevated form is the
other store, and the daemon picks between them per operation rather than
trying both.

The file API runs as the daemon account under the unit's sandbox, so a
`daemon.files.roots` entry must be somewhere that account can actually read and
write. `ProtectSystem=full` leaves `/srv` and `/var` writable while `/etc` is
writable only through the existing `ReadWritePaths=/etc/maidcafe`, and
`ProtectHome=true` hides `/home` and `/root` entirely — a root under either is
rejected at startup, which is deliberate: the alternative is a root that
validates and then fails every request.

A `privileged` root interacts with that sandbox, and the interaction is worth
understanding before enabling one. `maidkit-priv` is a child of the daemon, so
it inherits the unit's mount namespace: `ProtectSystem=full` makes `/etc`
read-only for the helper exactly as it does for the daemon, and a privileged
root under such a path therefore needs it added to `ReadWritePaths` (plus
`NoNewPrivileges=false`, since sudo needs its setuid bit — the same relaxation a
run-as action already requires, and what the installer does when one is
configured). That grant is namespace-wide, so the daemon's own process gains
write access to the path too; the helper's guarantees in that configuration are
the audited, validated, profile-scoped *authorization* — the only way a client
can reach the path is through it — rather than containment of a compromised
daemon. Closing that gap means giving the helper its own unit (a privileged
broker outside this namespace), which is a larger change than this feature;
until then, prefer privileged roots on paths the unit does not need to sandbox.

## CI artifacts

GitHub Actions is defined in [`.github/workflows/build.yml`](.github/workflows/build.yml):

- Pull requests run tests and builds and build the cloud image without pushing.
- `master` pushes publish the cloud image to GHCR.
- Version tags publish a versioned cloud image.
- Manual workflow runs are also supported. Run them from a version tag for a
  stable release or from `master` for a rolling release.
- Pull requests validate the daemon build matrix without publishing daemon artifacts.
- Version tags without a leading `v` (for example `1.2.3`) publish compressed
  daemon-binary-only archives for Linux, macOS, and Windows on `amd64` and
  `arm64` to the stable channel.
- Every commit pushed to `master` publishes only Linux `amd64` and `arm64`
  daemon archives to the rolling channel. Its version is the first six
  characters of the commit SHA.

Set the repository variable `PACKAGE_OWNER` to the GHCR owner used by the image
name. For daemon artifact publishing, create a DistributionCenter product and
product-scoped upload key, then set:

- `DISTRIBUTION_API_BASE_URL` repository variable
- `DISTRIBUTION_PRODUCT_ID` repository variable
- `DISTRIBUTION_UPLOAD_KEY` repository secret

DistributionCenter returns a daemon artifact's download URL by matching the
release channel, platform, and architecture. The archive contains only the
compressed daemon binary; MaidKit supplies configuration and service-manager
integration on the target host.

## Security boundary

Webhook request bodies are data delivered to process stdin. They are not shell
syntax. Commands and arguments come only from validated static configuration.
Daemon secrets are sent only in the `Authorization` header — or, for a browser
WebSocket handshake, the `Sec-WebSocket-Protocol` token — never in query
parameters or cookies. The WebSocket terminal is only as strong as that
credential and rides the same transport as the rest of the daemon API: without
an operator TLS front it is plain `ws://`, so a dedicated
`daemon.terminal.secret` is what keeps a leaked metrics secret from becoming a
shell. A cloud-relayed session additionally passes through the cloud, which
terminates TLS and can therefore read the session; the direct endpoint does not.

Update checks are the one daemon feature that talks to a third party on its own
initiative: they request a manifest from the registry an image names, over
HTTPS, on the check cadence. They read the daemon account's own runtime auth
file for that registry when one exists — the same credential a `podman pull` or
`docker pull` run by that account would use — and never log it, never send it
anywhere but the registry's own token endpoint, and never send it over plain
HTTP: a registry dialled over TLS can only name a token endpoint over TLS.
`updateCheckInterval = 0` turns the cadence off entirely, and each container can
still be checked on demand.

A privileged file root does not widen that boundary; it narrows the daemon's own
reach. The daemon never holds root: it runs `maidkit-priv` through a
passwordless sudoers rule, and the helper resolves a *profile name* against a
root-owned profile file the daemon cannot write, so a compromised daemon can
only reach directories an operator declared in both places. A sudoers rule over
a general tool would not be a boundary — `sudo tee` writes any path and `sudo
systemctl` loads any unit file — which is why the grant is over one helper with
a fixed, validated command surface. See
[`docs/PRIVILEGED.md`](docs/PRIVILEGED.md).

The file API is the same trade in a different shape, with two differences. It
accepts the credential in a `token` query parameter — but only on the
raw-content read route, where a browser can carry nothing else: a leak of that
URL therefore reads a file and cannot change one, since every mutation needs a
header credential and a JSON body additionally needs a signature it cannot
produce. That URL is still as sensitive as the credential, which is why the API
has its own switch, its own optional secret, an explicit root allowlist and a
separate write gate. And it is confined twice: lexically to
`daemon.files.roots` and by `os.Root` against symlinks, so a link planted
inside a root cannot be used to read or write outside it. The API runs as the
daemon account and never elevates.
