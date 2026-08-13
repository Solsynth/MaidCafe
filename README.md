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
- Account ownership checks for daemon and notification resources.
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
  `sh -c`.
- Static configured arguments only.
- Absolute command paths and controlled working directory.
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
| `POST` | `/api/daemons` | Create a daemon; returns the one-time secret |
| `GET` | `/api/daemons` | List owned daemons |
| `GET` | `/api/daemons/:id` | Read an owned daemon |
| `PATCH` | `/api/daemons/:id` | Change name or enabled state |
| `POST` | `/api/daemons/:id/rotate-secret` | Rotate the one-time secret |
| `DELETE` | `/api/daemons/:id` | Disable daemon and delete its metrics |
| `POST` | `/api/daemons/:id/metrics` | Ingest daemon metrics |
| `POST` | `/api/daemons/:id/notifications` | Create a daemon notification |

Create a daemon:

```sh
curl -X POST http://localhost:8080/api/daemons \
  -H 'Authorization: Bearer <solar-token>' \
  -H 'Content-Type: application/json' \
  -d '{"name":"managed-host-01"}'
```

The response contains `id`, `name`, and `secret`. Store the secret in the
managed host configuration. It is not returned by list or read endpoints.

### Notifications

| Method | Route | Purpose |
| --- | --- | --- |
| `GET` | `/api/notifications` | List owned notifications |
| `POST` | `/api/notifications/:id/read` | Mark an owned notification read |

`GET /api/notifications` supports `unread`, `daemon_id`, `limit` up to `100`,
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

## CI artifacts

GitHub Actions is defined in [`.github/workflows/build.yml`](.github/workflows/build.yml):

- Pull requests run tests and builds and build the cloud image without pushing.
- `master` pushes publish the cloud image to GHCR.
- Version tags publish a versioned cloud image.
- Manual workflow runs are also supported. Run them from a version tag for a
  stable release or from `master` for a rolling release.
- Every verified workflow run uploads a daemon systemd bundle containing the
  daemon binary, unit file, and example configuration.
- Version tags without a leading `v` (for example `1.2.3`) upload that daemon
  bundle to DistributionCenter as the stable `linux`/`amd64` artifact.
- Every commit pushed to `master` also uploads the daemon bundle to the rolling
  channel. Its version is the first six characters of the commit SHA.

Set the repository variable `PACKAGE_OWNER` to the GHCR owner used by the image
name. For daemon artifact publishing, create a DistributionCenter product and
product-scoped upload key, then set:

- `DISTRIBUTION_API_BASE_URL` repository variable
- `DISTRIBUTION_PRODUCT_ID` repository variable
- `DISTRIBUTION_UPLOAD_KEY` repository secret

DistributionCenter can return a daemon artifact's download URL to a server
using the product release API filtered by channel, `platform=linux`, and
`architecture=amd64`. Use `channel=stable` for version-tagged releases or
`channel=rolling` with the six-character commit version for the latest commit
build.

## Security boundary

Webhook request bodies are data delivered to process stdin. They are not shell
syntax. Commands and arguments come only from validated static configuration.
Daemon secrets are sent only in the `Authorization` header, never in query
parameters or cookies.
