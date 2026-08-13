# MaidCafe

MaidCafe provides a cloud control plane and a dependency-light MaidKit daemon.
Build them separately:

```sh
go run ./cmd/cloud --config config.toml
go run ./cmd/daemon --config config.toml
```

## Cloud deployment with Docker

The Docker image is cloud-only and expects a read-only TOML configuration mount:

```sh
docker build -t maidcafe-cloud .
docker run --rm \
  -p 8080:8080 \
  -v "$PWD/config.toml:/etc/maidcafe/config.toml:ro" \
  -e CONFIG_PATH=/etc/maidcafe/config.toml \
  maidcafe-cloud
```

For PostgreSQL plus the cloud service, copy `docker-compose.example.yml` and
replace the example credentials and auth target before starting it.

GitHub Actions builds and publishes the cloud image to GHCR on `master` and
version tags. Pull requests build it without pushing. The same workflow uploads
`maidcafe-daemon-systemd-<commit>` as an artifact containing the daemon binary,
systemd unit, and example configuration. Set the repository `PACKAGE_OWNER`
variable to the GHCR owner used by the workflow.

## Daemon deployment with systemd

Build only the daemon binary and install it with the unit template:

```sh
make build-daemon
sudo install -o root -g root -m 0755 bin/maidcafe-daemon /usr/local/bin/maidcafe-daemon
sudo install -d -o root -g maidcafe -m 0750 /etc/maidcafe
sudo install -o root -g maidcafe -m 0640 config.toml /etc/maidcafe/config.toml
sudo install -o root -g root -m 0644 deploy/maidcafe-daemon.service \
  /etc/systemd/system/maidcafe-daemon.service
sudo systemctl daemon-reload
sudo systemctl enable --now maidcafe-daemon
```

Create the service account before installation:

```sh
sudo useradd --system --home /var/lib/maidcafe --create-home maidcafe
```

The unit binds the configured daemon address, runs as `maidcafe`, restarts after
failure, and allows outbound cloud HTTPS publishing. Webhook commands must be
absolute paths readable and executable by that account.

Cloud mode requires PostgreSQL and a Solar Network auth target. `POST /api/daemons`
returns a daemon ID and a credential once. Store that credential in the daemon
configuration; `POST /api/daemons/:id/rotate-secret` invalidates the old credential
and returns a replacement once.

Daemon mode can run with no cloud URL or secret. Configure named absolute-path
webhooks and call one with:

```sh
curl -X POST http://127.0.0.1:8747/api/v1/webhooks/backup \
  -H 'Authorization: Bearer replace-with-local-webhook-secret' \
  --data-binary '{"job":"incremental"}'
```

The request body is opaque stdin data. It is never parsed as shell syntax and is
never appended to the configured command's static argument list. The daemon invokes
the absolute command directly, without `sh -c`. When both `cloudUrl` and
`cloudSecret` are configured, it periodically publishes metrics and optionally
publishes webhook success/failure notifications over HTTPS POST. Publishing
failures are dropped and do not affect local execution.
