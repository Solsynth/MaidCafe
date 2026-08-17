# Automated deployments with MaidCafe and MaidKit

This guide connects a container image build to a deployment action on a managed
host:

```text
GitHub Actions
    │ build and push ghcr.io/solsynth/capital:<commit>
    ▼
MaidFlow
    │ enqueue deploy-capital through MaidCafe Cloud
    ▼
MaidCafe daemon on the host
    │ poll, authenticate, and run the configured action
    ▼
MaidKit-deployed action (root)
    │ docker compose pull
    │ docker compose up -d
    │ docker image prune -f
    ▼
Updated application
```

The workflow follows the production pattern used by
[Capital](https://github.com/Solsynth/Capital): its Docker image is pushed to
GHCR, then [MaidFlow](https://github.com/Solsynth/MaidFlow) invokes a named
MaidCafe action. The host does not need an inbound port. The daemon polls the
cloud relay over its outbound connection.

## What you need

- A MaidCafe Cloud workspace and a host reachable from [MaidKit](https://github.com/Solsynth/MaidKit) over SSH.
- Docker Engine and the Docker Compose plugin installed on the host.
- The application checkout and production Compose file on the host. This guide uses `/srv/capital`.
- A container registry image that the host can pull. The examples use `ghcr.io/solsynth/capital`.
- A GitHub repository that can publish the image and invoke MaidFlow.
- A MaidCafe credential scoped only to the target daemon and deployment action.

The deployment action is intentionally privileged. It runs as `root` so that
Docker access does not depend on a host-specific `docker` group or a daemon
user configuration. Use a dedicated deployment host or a narrower run-as user
when the host supports that model.

## 1. Prepare the host Compose project

Create the application directory and put the production Compose file there:

```sh
sudo install -d -m 0755 /srv/capital
sudo chown "$USER":"$USER" /srv/capital
```

The image-publishing workflow cannot deploy a Compose service that only has
`build: .`. Change the application service to use the image supplied by the
deployment action. For Capital, keep the existing environment, ports, volumes,
and restart policy from [`docker-compose.yml`](https://github.com/Solsynth/Capital/blob/master/docker-compose.yml), but replace the `build` entry with an image reference:

```yaml
services:
  capital:
    image: ${CAPITAL_IMAGE:?CAPITAL_IMAGE is required}
    ports:
      - "3000:3000"
    environment:
      - NODE_ENV=production
      - NITRO_DATA_DIR=/data/nitro
      - PUBLIC_PB_URL=${PUBLIC_PB_URL:-https://your-pocketbase-instance.com}
      - DATABASE_URL=${DATABASE_URL:-file:/data/nitro/local.db}
      - BETTER_AUTH_SECRET=${BETTER_AUTH_SECRET:-change-me}
      - BETTER_AUTH_URL=${BETTER_AUTH_URL:-http://localhost:3000}
      - SOLIAN_CLIENT_ID=${SOLIAN_CLIENT_ID}
      - SOLIAN_CLIENT_SECRET=${SOLIAN_CLIENT_SECRET}
      - ADMIN_EMAILS=${ADMIN_EMAILS}
    volumes:
      - capital-data:/data
    restart: unless-stopped

volumes:
  capital-data:
```

Put the Compose file and its environment file in `/srv/capital`. Do not put
registry credentials or application secrets in the action script. If the GHCR
package is private, log the host's Docker client in to GHCR once as the account
that will run the action (here, `root`) and grant it read-only package access:

```sh
sudo docker login ghcr.io
```

A public image does not need a registry login. `docker compose pull` must be
able to read the exact image tag sent by CI.

## 2. Register the host with MaidCafe from MaidKit

1. Add the host to MaidKit and establish an SSH connection.
2. Open **MaidCafe Cloud**, sign in, and select the workspace that owns the host.
3. Register the connected host. MaidKit probes the existing installation; it
   installs the published daemon when one is not present, writes the cloud
   credentials, and starts the daemon. The daemon keeps an outbound connection
   to the cloud; MaidKit does not need to remain open after registration.
4. Open the host's **MaidCafe** server tab. Use its action/payload view to add
   and save the deployment action below.

If the host already has a MaidKit-managed daemon, registration patches its cloud
credentials and preserves the other daemon settings. An unmanaged conflicting
installation must be removed or migrated before MaidKit can manage it.

## 3. Create the root deployment action in MaidKit

In the host's MaidCafe action editor, create these values:

| Field | Value |
| --- | --- |
| Action slug | `deploy-capital` |
| Display name | `Deploy Capital` |
| Working directory | `/srv/capital` |
| Run as user | `root` |
| Timeout | `5m` or longer than the normal pull and start time |
| Enabled | On |

The working directory is important: it makes `docker compose` use the
application's Compose file and environment file. MaidKit deploys the script
under `/etc/maidcafe/actions/` and installs the sudo rule needed for the daemon
to run it as `root`.

Use this script body:

```sh
set -eu

# MaidFlow sends {"image":"ghcr.io/solsynth/capital:<commit>"}.
# MaidCafe substitutes {{ image }} before executing the script.
export CAPITAL_IMAGE='{{ image }}'

docker compose pull
docker compose up -d
docker image prune -f
```

The three Docker commands are deliberately separate:

1. `docker compose pull` downloads the image tag selected by CI.
2. `docker compose up -d` recreates or starts services whose image changed and
   leaves the application running in the background.
3. `docker image prune -f` removes dangling images on the Docker host without
   prompting. It does not remove named volumes or the image currently
   referenced by the running Compose project.

`{{ image }}` is substituted verbatim. Only send this action a value generated
by trusted CI; do not expose it to arbitrary webhook callers. The action body
is also available to the daemon over its direct SSH-backed MaidKit session, so
MaidKit can prompt for template values when testing it manually.

Save the action configuration. Run it once from MaidKit with a known image tag
before connecting CI. Review the streamed output and confirm the Compose
project is healthy. The action's audit entry and captured output are available
in the host's MaidCafe audit view.

## 4. Create a least-privilege CI credential

Create a MaidCafe API credential for the deployment action. Scope it to:

- the target host/daemon only; and
- `deploy-capital` only.

MaidKit exposes credential creation in the **MaidCafe Cloud** credentials tab.
The token is shown once, so copy it directly into the GitHub repository secret
named `MAIDCAFE_TOKEN`.

Record the daemon ID shown by the MaidCafe Cloud host card. Add it as the
repository variable `MAIDCAFE_DAEMON_ID`. The daemon ID is used for routing;
the credential token is used for authorization. Do not confuse either value
with the host's stable `host_id` used when scoping credentials.

For an API-based setup, the equivalent credential request is:

```sh
curl -X POST https://mk.solsynth.dev/api/credentials \
  -H 'Authorization: Bearer <solar-token>' \
  -H 'Content-Type: application/json' \
  -d '{"label":"capital-deploy","host_ids":["<host-id>"],"action_names":["deploy-capital"]}'
```

The returned `mk_...` token is a secret. Store it only as
`MAIDCAFE_TOKEN`; never commit it to the application or workflow file.

## 5. Invoke the action from GitHub Actions

Capital's [`docker.yml`](https://github.com/Solsynth/Capital/blob/master/.github/workflows/docker.yml)
uses this sequence:

1. Build and push a short-commit image tag to GHCR.
2. Compute the same short tag for deployment.
3. Invoke `deploy-capital` through MaidFlow.
4. Print the daemon's result code and captured output.

The deployment job is:

```yaml
  deploy:
    needs: build
    if: github.ref == 'refs/heads/master'
    runs-on: ubuntu-latest

    steps:
      - name: Compute pushed image tag
        id: sha
        run: echo "short_sha=${GITHUB_SHA:0:7}" >> "$GITHUB_OUTPUT"

      - name: Trigger deploy on the managed host
        id: maidflow
        uses: Solsynth/MaidFlow@v1
        with:
          daemon_id: ${{ vars.MAIDCAFE_DAEMON_ID }}
          action: deploy-capital
          body: '{"image":"ghcr.io/solsynth/capital:${{ steps.sha.outputs.short_sha }}"}'
          token: ${{ secrets.MAIDCAFE_TOKEN }}
          timeout_minutes: 5

      - name: Print deploy result
        run: |
          echo "deploy exit code: ${{ steps.maidflow.outputs.result_code }}"
          echo "${{ steps.maidflow.outputs.result_body }}" | base64 -d
```

`MaidFlow` uses relay mode by default. It enqueues the request in MaidCafe
Cloud, waits for the daemon to poll and execute it, and exposes the daemon's
HTTP-style result as `result_code` and base64-encoded `result_body`.

Pin the action to a full commit SHA in production instead of the moving `v1`
tag:

```yaml
uses: Solsynth/MaidFlow@<full-commit-sha>
```

The build job must push the exact image referenced by `body`. For example,
Capital's workflow logs in to `ghcr.io`, publishes both a short SHA tag and
`latest`, and passes the short SHA tag to MaidFlow. If your project uses a
different registry or tag, update both the Compose file's expected image and
the JSON body.

## 6. Verify and troubleshoot

After a deployment run:

- Check the MaidFlow step's `result_code`. `200` means the action exited
  successfully; `502` means the script command failed; `504` means the daemon
  timeout was reached.
- Read `result_body` for the action's captured output. Docker errors usually
  identify registry authentication, an invalid image tag, or a Compose
  configuration error.
- In MaidKit, open the host's action result and audit view. The audit record
  identifies the action and invocation source.
- On the host, verify the running image and service state:

  ```sh
  cd /srv/capital
  sudo docker compose ps
  sudo docker compose images
  sudo docker compose logs --tail=100 capital
  ```

Common failure modes:

| Symptom | Check |
| --- | --- |
| `404` or unknown action | `deploy-capital` is saved, enabled, and reported by the daemon; refresh the MaidCafe Cloud host card. |
| `401` | The GitHub secret is the unexpired credential token, not the daemon ID or metrics secret. |
| Pull denied | The host's root Docker client can authenticate to GHCR and has package read access. |
| Compose ignores the CI image | The service still has `build: .`, or `CAPITAL_IMAGE` is not used in the `image:` field. |
| Action times out | Increase the MaidKit action timeout and the MaidFlow `timeout_minutes` value; allow for the daemon's polling interval. |
| `docker: permission denied` | Keep **Run as user** set to `root`, or configure an equivalent Docker-capable run-as account and sudo policy. |

## Security and rollback notes

- The CI credential should name one daemon and one action. A credential that can
  invoke arbitrary actions turns every enabled action on that host into a CI
  privilege boundary.
- `user = "root"` is a deliberate privilege grant. Keep the action script
  short, review changes to it, and do not add arbitrary request-derived shell
  fragments.
- MaidCafe relay delivery is polling-based. A successful GitHub request means
  the cloud accepted the invocation, not that the container is already healthy;
  always inspect the returned result.
- The deployment does not delete volumes and does not run `docker compose down`,
  so persistent application data remains attached to the Compose project.
- To roll back, invoke the same action with a previously published immutable
  image tag in the request body, for example:

  ```json
  {"image":"ghcr.io/solsynth/capital:0123abc"}
  ```

For action request semantics, result codes, relay polling, and template
behavior, see [Invoking actions and webhooks](WEBHOOKS.md).