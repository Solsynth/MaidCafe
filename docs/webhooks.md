# Invoking actions and webhooks

MaidCafe runs two kinds of named command hooks on a managed host:

- **Webhooks** (`[[daemon.webhooks]]`) are secret-protected. Every invocation
  must carry a per-webhook HMAC signature over the request body.
- **Actions** (`[[daemon.actions]]`) have no secret. They are authenticated by
  the transport instead: the daemon's metrics secret for direct HTTP, the
  cloud-authenticated poll for the relay.

Both are configured the same way (see `config.daemon.example.toml`), share one
namespace — names must be unique across both kinds — and run through the same
executor. A name matches `[A-Za-z0-9._-]+` and the command path is absolute.

There are two ways to invoke a hook:

1. **Direct HTTP** — call the daemon's local API endpoint.
2. **Cloud relay** — enqueue an invocation on the MaidKit cloud; the daemon
   polls it (once a minute) and executes it on the host.

| Channel | Webhook | Action | Who authenticates |
| --- | --- | --- | --- |
| Direct HTTP | `POST /api/v1/webhooks/:name` | `POST /api/v1/actions/:name` | HMAC signature / metrics secret |
| Cloud relay | `POST /api/daemons/:id/webhook-requests` | same route, no signature | Solar/credential token on enqueue, daemon secret on poll |

## 1. Configuration

```toml
[[daemon.webhooks]]
name = "backup"
secret = "replace-with-local-webhook-secret"
command = "/usr/local/bin/maidkit-backup"
args = ["--mode", "incremental"]
enabled = true
notifyOnSuccess = false
notifyOnFailure = true

[[daemon.actions]]
name = "cleanup"
command = "/usr/local/bin/maidkit-cleanup"
args = ["--retention-days", "7"]
enabled = true

[[daemon.actions]]
name = "deploy"
command = "/etc/maidcafe/actions/deploy.sh"
script = true
displayName = "Deploy the web app"
cwd = "/srv/myapp"
user = "deploy"
env = ["NODE_ENV=production"]
enabled = true
```

| Field | Meaning |
| --- | --- |
| `name` | API slug used in the route and audit log; `[A-Za-z0-9._-]+`, unique across webhooks and actions |
| `secret` | webhooks only (required); key for the HMAC signature |
| `command` | absolute command path, or a MaidKit-deployed script with `script = true` |
| `args` | fixed argument list; the request body is **never** appended to args |
| `enabled` | a disabled or unknown name returns `404` |
| `script` | treat `command` as a script body; `{{ NAME }}` placeholders are substituted from a JSON request body before running |
| `cwd`, `user`, `env`, `timeout` | working directory, run-as user (via sudo), environment assignments, per-hook timeout override |

The request body is opaque bytes piped to the command's stdin. It is never
parsed into command arguments. For `script = true` hooks, a JSON body also
fills `{{ NAME }}` template variables; a placeholder missing from the body
fails the run with `400` instead of silently producing a broken command line.

## 2. Direct HTTP invocation

The daemon listens on `daemon.listen` (default `127.0.0.1:8747`). Use the
webhook route for webhooks and the actions route for actions.

### Webhook

```text
POST /api/v1/webhooks/:name
X-MaidCafe-Signature: <hex HMAC-SHA256 of the raw body, keyed by the webhook secret>
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

The signature is the lowercase-hex HMAC-SHA256 of the raw body bytes keyed by
the webhook's own secret (see [Signature computation](#4-signature-computation)).
It proves the caller holds the secret and that the body is untampered.

Optional header `X-MaidCafe-Invoked-By` names the caller; it is recorded in the
audit log.

### Action

```text
POST /api/v1/actions/:name
Authorization: Bearer <daemon metrics secret>
```

Actions carry no per-action secret; the metrics secret authenticates the
caller to the daemon's HTTP API (the same credential used for `/api/v1/metrics`
and `/api/v1/audit`).

```sh
curl -X POST http://127.0.0.1:8747/api/v1/actions/cleanup \
  -H 'Authorization: Bearer replace-with-local-metrics-secret' \
  --data-binary '{"retention_days":14}'
```

### Response

A successful run returns `200` with the execution result; request-level
failures return an error body `{"error": "..."}`:

```json
{"ok": true, "name": "backup", "exit_code": 0, "stdout": "…", "stderr": ""}
```

| Status | Meaning |
| --- | --- |
| `200` | command exited 0 |
| `400` | script template variable missing from body, or unreadable body |
| `401` | bad signature (webhook) or bad metrics secret (action) |
| `404` | unknown or disabled name |
| `413` | body larger than `daemon.maxBodyBytes` (default 64 KiB) |
| `429` | concurrency exhausted (`daemon.maxConcurrentRuns`, default 4) |
| `502` | command exited non-zero |
| `504` | command exceeded the timeout (`daemon.scriptTimeout` or the hook's `timeout`) |

stdout/stderr are captured up to 8 KiB each.

## 3. Cloud relay

The relay lets a workspace member invoke a named webhook or action on a
managed host through the cloud, without the member ever touching the host.
The daemon polls for pending requests (immediately at startup, then once a
minute), verifies the signature against its local configuration, executes the
hook, and reports the result back. Polling is deliberate: the cloud never
holds a connection into the host.

```mermaid
sequenceDiagram
    participant U as Workspace member
    participant C as MaidCafe cloud
    participant D as Daemon
    U->>C: POST /api/daemons/:id/webhook-requests
    C-->>U: 201 {id, status: pending}
    loop every minute
        D->>C: GET /api/daemons/:id/webhook-requests/pending
        C-->>D: requests (leased for 2 min)
    end
    D->>D: verify signature locally, execute
    D->>C: POST /api/daemons/:id/webhook-requests/:request_id/result
    U->>C: GET /api/daemons/:id/webhook-requests/:request_id
    C-->>U: request with result
```

### Enqueue

```text
POST /api/daemons/:id/webhook-requests
Authorization: Bearer <solar-token or credential token>
Content-Type: application/json
```

```json
{"name": "backup", "body": "<base64 of raw body>", "signature": "<hex HMAC>"}
```

| Field | Constraints |
| --- | --- |
| `name` | required; must match a webhook or action configured on the daemon |
| `body` | required base64; decoded size `<= 256 KiB` |
| `signature` | required for webhooks: lowercase-hex HMAC-SHA256 of the **raw** body keyed by the webhook's secret. Omitted for actions — the daemon runs them because the request arrived through its own cloud-authenticated poll |

`201` on success:

```json
{"id": "3f9a…", "status": "pending", "created_at": "2026-08-15T10:00:00Z"}
```

The caller is recorded as `invoked_by` (`@<handle>` for Solarpass users, the
credential label for API credentials) and lands in the daemon's audit log.

Example (webhook):

```sh
DAEMON_ID='d0f2f0c2-…'
TOKEN='<solar-token>'
SECRET='<webhook secret>'
BODY='{"job":"incremental"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}')

curl -X POST "http://localhost:8080/api/daemons/$DAEMON_ID/webhook-requests" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"backup\",\"body\":\"$(printf '%s' "$BODY" | base64 -w0)\",\"signature\":\"$SIG\"}"
```

Invoking an action through the relay is the same request with the `signature`
field omitted:

```sh
curl -X POST "http://localhost:8080/api/daemons/$DAEMON_ID/webhook-requests" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"cleanup","body":"e30="}'
```

### Daemon side (for reference)

The daemon authenticates to the cloud with its daemon secret:

- `GET /api/daemons/:id/webhook-requests/pending?limit=N` — pulls pending
  requests (`limit` 1–50, default 50). Returned requests are leased for 2
  minutes so a concurrent poll does not double-execute; expired leases are
  reclaimed.
- `POST /api/daemons/:id/webhook-requests/:request_id/result` — reports
  `{"code": <http status>, "body": "<base64 stdout>", "error": "…"}`; `204` on
  success.

Pending requests are capped at 50 per daemon.

### Fetching the result

```text
GET /api/daemons/:id/webhook-requests/:request_id
Authorization: Bearer <solar-token or credential token>
```

`200` returns the request with its result once the daemon completed it;
`404` if unknown. `status` is `pending` while queued, `leased` while the
daemon holds it, `done` once reported.

```json
{
  "id": "3f9a…",
  "daemon_id": "d0f2f0c2-…",
  "name": "backup",
  "body": "eyJqb2IiOiJpbmNyZW1lbnRhbCJ9",
  "signature": "7f9a…",
  "invoked_by": "@alice",
  "status": "done",
  "result_code": 200,
  "result_body": "…",      // base64 stdout, present on success
  "result_error": "",      // non-empty when the daemon rejected or failed the run
  "created_at": "2026-08-15T10:00:00Z",
  "updated_at": "2026-08-15T10:01:00Z"
}
```

Because the daemon polls once a minute, a relayed invocation takes up to one
minute plus the hook's own runtime before the result appears.

## 4. Signature computation

The signature is the lowercase-hex HMAC-SHA256 of the **raw body bytes** keyed
by the webhook secret — identical for direct HTTP (`X-MaidCafe-Signature`) and
the relay (`signature` field; computed over the raw body before base64
encoding).

```sh
printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}'
```

```python
import hashlib, hmac
hmac.new(b"<secret>", b"<raw body>", hashlib.sha256).hexdigest()
```

The secret itself is never sent anywhere: it stays in the daemon's local
config, and the transport (loopback, SSH tunnel, Tailscale, or the cloud
relay) provides confidentiality. The signature proves possession of the
secret and body integrity only.

## 5. Execution semantics

- The body is piped to the command's stdin, untouched. Fixed `args` and the
  command path are configured, never taken from the request.
- Every run — HTTP webhooks, actions, and relayed invocations — is appended to
  `daemon.auditPath` (default `/var/lib/maidcafe/audit.jsonl`) as one JSON line
  per run: timestamp, `name`, `display_name`, `source` (`http` | `stdio` |
  `relay`), `invoked_by`, `ok`, `exit_code`, `duration_ms`, and a truncated
  failure reason. Rotated at 1 MiB, one generation kept.
- `notifyOnSuccess` / `notifyOnFailure` publish `webhook.success` /
  `webhook.failure` notifications to the cloud for that hook. The cloud
  localizes the generated title using the account language; command output in
  the body remains unchanged.
- The daemon also reports its configured actions to the cloud
  (`POST /api/daemons/:id/actions` on every metrics tick), so
  `GET /api/daemons/:id/actions` lists what a workspace member may invoke.
  Script bodies and secrets never leave the host.
