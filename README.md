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
  environment) and the working directory is applied with sudo `-D` for plain
  commands (sudo 1.9.9+) or a `cd` line prepended to script bodies — the
  executed command is always the configured absolute path, never a shell
  wrapper, so the sudoers rule matches it. The sudoers rule granting the
  daemon the right to run MaidKit-deployed scripts as the configured users is
  installed by MaidKit; hand-configured entries must provide their own rule.
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

When both `daemon.cloudUrl` and `daemon.cloudSecret` are configured, the daemon:

- Publishes metrics on every `metricsInterval` tick.
- Publishes webhook success notifications when `notifyOnSuccess = true`.
- Publishes webhook failure notifications when `notifyOnFailure = true`.
- Uses `Authorization: Bearer <daemon-secret>`.
- Uses request deadlines and disables cross-host redirect following.
- Drops failed publishing attempts without failing local webhook execution.
- Does not retain an unbounded retry queue.

Cloud publishing is disabled when either setting is empty. HTTPS is required,
except for HTTP development URLs using `localhost` or `127.0.0.1`.

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

### Notifications

| Method | Route | Purpose |
| --- | --- | --- |
| `GET` | `/api/notifications?workspace_id=` | List workspace notifications |
| `POST` | `/api/notifications/:id/read` | Mark a workspace notification read |

`GET /api/notifications` requires `workspace_id` and additionally supports
and an RFC3339 `before` cursor.

## Daemon API

Webhook requests use:

```text
POST /api/v1/webhooks/:name
Authorization: Bearer <webhook-secret>
```

Example:

```sh
curl -X POST http://127.0.0.1:8747/api/v1/webhooks/backup \
  -H 'Authorization: Bearer replace-with-local-webhook-secret' \
  --data-binary '{"job":"incremental"}'
```

Successful execution returns `200`; non-zero exit returns `502`; timeout returns
`504`; oversized bodies return `413`; exhausted concurrency returns `429`.
Metrics and configured actions use the daemon metrics secret:

```text
GET /health
GET /api/v1/metrics
POST /api/v1/actions/:name
GET /api/v1/audit?limit=N
Authorization: Bearer <metrics-secret>
```

Every execution — HTTP webhooks, actions, and cloud-relayed webhooks — is
appended to `daemon.auditPath` (default `/var/lib/maidcafe/audit.jsonl`) as
one JSON line per run: timestamp, `name` (API slug), optional `display_name`,
`source` (`http` | `stdio` | `relay`), `ok`, `exit_code`, `duration_ms`, and a
truncated failure reason. The file rotates at 1 MiB keeping one generation
(`audit.jsonl.1`). Logging is best-effort: an unwritable path disables it with
a warning and never affects execution. `GET /api/v1/audit?limit=N` returns the
newest entries (default 50, max 500) newest first, authenticated with the
metrics secret.

### Realtime event stream

`GET /api/v1/stream` serves a Server-Sent Events stream of realtime daemon
state over HTTP, authenticated with the same metrics secret:

```text
GET /api/v1/stream?events=metric,containers,images,processes,systemd
Authorization: Bearer <metrics-secret>
```

- `events` is a comma-separated whitelist of event types; omitted means all.
  Unknown names return `400`.
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
  user) when passwordless sudo is available. The retry is never interactive
  and the direct result stands otherwise.
- Setting a collector interval to `0` disables that collector. Collection is
  gated on active subscribers and never persists or writes to disk; metrics
  persistence and cloud publishing stay on `metricsInterval`.

### Snapshot endpoints

The same state the stream pushes is also available as one-shot responses, so
clients can paint first data from the daemon instead of an SSH fallback. They
reuse the stream collectors' probe cache and rate limits:

- `GET /api/v1/containers` — same payload as the `containers` event: a
  `runtimes` list covering every runtime found on the host (podman first),
  each with `runtime`, `available`, `error` and `containers`.
- `GET /api/v1/images` — same payload as the `images` event: a `runtimes`
  list, each with `runtime`, `available`, `error` and `images`.
- `GET /api/v1/processes` — same payload as the `processes` event.
- `GET /api/v1/systemd` — same payload as the `systemd` event.

All four are authenticated with the same metrics secret and cost one
collection on demand; repeated calls are rate-limited by the shared probe
cache.

Webhook secrets are separate from the metrics secret. The daemon does not
provide an HTTP configuration API; MaidKit updates the managed TOML
configuration over SSH and restarts the service when needed.


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
substituted scripts under `/etc/maidcafe/actions/.run`. For those, the unit
needs `NoNewPrivileges=false` (sudo's setuid bit) and `ReadWritePaths=/etc/maidcafe/actions`,
and the daemon account needs a sudoers rule such as:

```sh
sudo install -o root -g root -m 0440 /dev/stdin /etc/sudoers.d/maidcafe-actions <<'EOF'
maidcafe ALL=(deploy) NOPASSWD: /etc/maidcafe/actions/.run/*, /etc/maidcafe/actions/*
EOF
```

The two specs matter: user-mode script actions render their substituted body
under `/etc/maidcafe/actions/.run/`, and sudoers wildcards do not cross `/`.

The same rule must exist for the SSH user that runs the daemon in `stdio`
transport mode. MaidKit deploys all of this automatically when an action
selects a run-as user.

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
Daemon secrets are sent only in the `Authorization` header, never in query
parameters or cookies.
