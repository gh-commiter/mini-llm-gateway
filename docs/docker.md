# Mini Gateway in Docker Compose

A Linux gateway (`minigw serve`) in a container, for providers only: Claude CLI, Codex, Grok, Copilot and Foundry Local
are off and stay off (the default container has no user namespaces, so the agent sandbox does not work; the gateway
refuses them in the settings, in an imported `serve.json` and when a request runs). Providers (LiteLLM, OpenRouter, xAI,
any OpenAI-compatible endpoint), devices, projects and notes work as on any other gateway. Remote web settings are not
part of the Compose deployment yet: you administer it with `docker compose exec minigw minigw gw …`. The gateway has
an opt-in password listener behind mutual TLS ([configuration](gateway.md#remote-settings-listener)); the supported
Compose reverse-proxy profile will follow separately. Do not publish its internal port.

The files are in [`deploy/docker/`](../deploy/docker): `compose.yaml` and `Dockerfile`. The image is built on your
computer from the GitHub release: no registry, no account. The build checks the signature of `SHA256SUMS` with the
Mini Gateway release key that is written in the Dockerfile (the same key as [install.sh](install.md#check-a-download);
it is never fetched from the download), then the signed version, then the checksum of the binary. A failed check stops
the build.

## Install and first start

```sh
git clone https://github.com/gh-commiter/mini-llm-gateway && cd mini-llm-gateway/deploy/docker
echo MINIGW_VERSION=0.7.6 > .env                     # the release to build: 0.7.6 (a beta) is the first with container mode
MINIGW_BIND=0.0.0.0 docker compose up -d --build     # MINIGW_BIND: the host address to publish port 8765 on
```

`MINIGW_VERSION` must be set: there is no default yet, because no stable release has container mode (0.7.0 would build but
cannot run this way). When a stable release has it, this page will name it.

`MINIGW_BIND` defaults to `127.0.0.1` (this computer only); use `0.0.0.0` or one LAN address so other computers can
connect, and limit who can reach port 8765 with the host firewall. `MINIGW_PORT` changes the host port. Set the
variables in a `.env` file next to `compose.yaml` to keep them.

The first start creates the configuration in the config volume and prints no token. The connect line for clients:

```sh
docker compose exec minigw minigw gw token
```

The traffic is encrypted and each new device must be approved: `docker compose exec minigw minigw gw devices`, then
`… devices allow <id>`. The container sees its own address (`172.x`), not the host's: replace it in the connect line with
the host name or address of the Docker host. `docker compose exec minigw minigw gw status` shows the gateway.

## Add a provider

Type the key inside the container, so it is not in your shell history or a file of yours:

```sh
docker compose exec minigw minigw gw provider add OpenRouter https://openrouter.ai/api   # asks for the key
docker compose exec minigw minigw gw provider models OpenRouter --enable all
```

The key is kept in `gateway-keys.json` (mode 0600) in the config volume: protected by file permissions, not encrypted.

### LiteLLM in another container

Inside the container `localhost` is the container itself. Put both services on one Docker network and use the service
name: the gateway in container mode accepts a name without a dot (a Compose service name) for a provider, with
`--insecure`, as long as it resolves to a private address. It checks this when you add the provider and again at every
connection, so a name that later points to a public address is not followed. Link-local addresses (such as the cloud
metadata address `169.254.169.254`), loopback, unspecified and multicast addresses are refused too. The name is dialled
directly, not through `HTTP(S)_PROXY`, so the key is not sent to a proxy. Any container on that network can claim the
service name and receive the key: put only trusted services on it. Outside a container such a name is refused.

```yaml
# compose.override.yaml next to the gateway's compose.yaml: join the network your LiteLLM service is on
services:
  minigw:
    networks: [default, litellm]
networks:
  litellm:
    external: true
    name: <the network of your LiteLLM compose project>   # docker network ls
```

```sh
docker compose exec minigw minigw gw provider add LiteLLM http://litellm:4000 --insecure   # asks for the key
```

If LiteLLM runs in the same Compose project, no extra network is needed. A name with a dot, `litellm.local` for example,
keeps working as before.

## amd64 and arm64

The same files build for both: the build takes the release binary that matches the Docker platform (`TARGETARCH`).
For the other one, for example on an Apple Silicon Mac that can emulate it: `DOCKER_DEFAULT_PLATFORM=linux/amd64
docker compose up -d --build`. `scripts/compose-e2e.sh` runs the whole deployment test for both platforms and says SKIP
when the machine cannot emulate one.

## Update

Change the version and rebuild (take a backup first, see below):

```sh
MINIGW_VERSION=0.8.0 docker compose up -d --build    # or set it in .env
```

A release before the first one with container mode (it is listed in the release notes) builds and starts, but it has no
container mode: it does not keep the agents off, `docker compose exec … minigw gw` finds no config, and `HEALTHCHECK`
reports `unhealthy`. Use a release whose notes mention Docker Compose.

## Backup

Both volumes together, with the gateway stopped (the database must not change while it is copied):

```sh
docker compose stop
for v in config scratch; do
  docker run --rm -v "$(basename "$PWD" | tr 'A-Z' 'a-z')_$v":/v:ro -v "$PWD":/b debian:trixie-slim tar -C /v -cf /b/minigw-$v.tar .
done
docker compose start
```

(`<project>_config` and `<project>_scratch` are the volume names; `docker volume ls` shows them. The project name is
the folder name, or `COMPOSE_PROJECT_NAME`.) Restore into empty volumes the same way with `tar -x`, owned by uid 10001.
`docker compose down` keeps the volumes; `down -v` deletes them with all data.

## Rollback

A newer version may upgrade the project database schema, and an older program refuses a newer schema ("core database has
a newer schema than this program"): the gateway then still serves models but project routes answer 503. To go back,
restore the backup you took before the update (both volumes), then start the older version. The same goes for the config: a release without service-name providers refuses to start with a provider such as `http://litellm:4000` in `serve.json`.

## Hardening that is on

Non-root user (uid 10001), read-only root file system with a `/tmp` tmpfs, all capabilities dropped,
`no-new-privileges`, an init process, limits of 1 GB memory and 512 processes, `restart: unless-stopped`, a 45 s stop
grace period (SIGTERM stops new requests, running ones get 30 s, then the database is closed). No Docker socket, host
home or host network. Bind-mounted folders instead of the named volumes must belong to uid 10001 (`chown 10001:10001`
on the host, once, not recursively from a container).

## Not supported yet

- Agents (Claude CLI, Codex, Grok, Copilot, Foundry Local) in the container.
- The remote settings reverse-proxy profile, `minigw serve install` and updates from inside the container (`gw` administration works).
- Pre-built images in a registry, automatic updates, Kubernetes or Swarm, several gateways on one database.

## Remote settings from a browser (admin profile)

An optional second service, a [Caddy](https://caddyserver.com) reverse proxy (image pinned by digest), makes the
[remote settings listener](gateway.md#remote-settings-listener) reachable from a browser on your LAN. Without
`--profile admin` nothing of it runs and the gateway's port 8766 is not even configured.

**Prerequisites:** a DNS name for the page that your browsers resolve to the Docker host (`settings.example.net`
below), a LAN address of the host to publish on, `openssl`, and the gateway already running. A port other than 443
works too, but it becomes part of the origin (`https://settings.example.net:8443`).

**1. Certificates.** From `deploy/docker`:

```sh
sh admin-certs.sh link ./admin-certs --origin https://settings.example.net     # prints the serve.json block
sh admin-certs.sh browser ./admin-certs settings.example.net                   # optional, see below
```

`link` creates a private CA for the proxy-gateway link (ECDSA P-256, 10 years), the gateway's server certificate
(SAN `minigw`, the Compose service name) and the proxy's client certificate (SAN `settings-proxy.internal`, the
`proxyIdentity`), both for 1 year. Files are 0600 in a 0700 folder, nothing is overwritten (it stops and names the
file), no secret is printed. The CA keys stay in `ca/` and are never mounted: the gateway sees only `gateway/`, the
proxy only `proxy/`. On a Linux host give each folder to its container user once, as the script prints (`chown
10001:10001` for `gateway/`, `65532:65532` for `proxy/`, not recursive).

`browser` is separate: a private CA and a 397-day certificate for the name browsers use. A certificate from your
team's CA or ACME is the better choice: put its chain and key at `admin-certs/proxy/browser.pem` and `browser.key` and
skip this command. With the private one, install `admin-certs/ca/browser-ca.pem` as a trusted root in each browser.

**2. Password**, typed inside the container (it works while the gateway is stopped):

```sh
docker compose exec minigw minigw gw settings password
```

**3. Configuration.** Add the block that step 1 printed to `serve.json` (in the config volume; edit it with the same
care as any config) and set `origin` and `allowedSourceCIDRs` (the browsers' networks, as the proxy sees them) to your
values; `proxyIdentity`, the three `/admin-tls/…` paths and `0.0.0.0:8766` stay as printed. Then `docker compose restart minigw`.

**4. Start the proxy.** In `.env` (next to `compose.yaml`):

```
MINIGW_ADMIN_NAME=settings.example.net     # the external name, required (no port)
MINIGW_ADMIN_BIND=192.168.10.5             # the host address to publish on; loopback if unset
MINIGW_ADMIN_PORT=443                      # the host port
```

```sh
docker compose --profile admin up -d
```

Open `https://settings.example.net`, sign in. Passwords and sessions follow the gateway's rules (lockout, 30 minutes
idle, audit file `remote-settings.audit.jsonl` in the config volume).

**What is reachable:** only the settings page, login and logout, and the provider, model and device routes that the
gateway allows remotely; everything else, including inference, projects, sync and the other admin routes, answers 404
at the proxy and is refused again by the gateway. Port 8766 is never published; only HTTPS is, no HTTP port exists
(no redirect: a redirect would invite typing a password on an `http://` address; HSTS keeps browsers on HTTPS). The
proxy talks to the gateway over mutual TLS on the dedicated `admin` network, checks the gateway's certificate and
name, and replaces the client-address header with the real peer address of the connection; it deletes
`X-Forwarded-*`, `Forwarded`, `X-Real-IP` and any `X-Minigw-Client-IP` the browser sent. Request bodies are capped at
1 MB. The proxy is a middlebox of the page: it sees the traffic after TLS, so keep it on the host you trust.

**Trust statement:** the gateway cannot see the browser's connection. It trusts the authenticated proxy to attest it:
the client address and that the browser used HTTPS. That is why the HTTPS-only proxy configuration, HSTS and the
header replacement are part of the supported boundary: do not change the Caddyfile to accept plain HTTP or to trust
forwarding headers, and do not give any other service the proxy's key.

**Turn it off:** `docker compose --profile admin down` stops the proxy (the gateway keeps running); to also close the
listener set `remoteSettings.enabled` to `false` and restart the gateway, which discards all remote sessions.

**Renewal:** the link certificates last 1 year, the browser certificate 397 days (an ACME or team certificate has its
own). Move the expired files away (never delete the CA key before its certificates are replaced), run the script again,
and `docker compose --profile admin restart` plus `docker compose restart minigw`. An expired certificate makes the
connection fail closed: the page does not load; there is no HTTP fallback.

**Rollback:** `docker compose --profile admin down`, remove the `remoteSettings` block (or set `enabled` to `false`) and
restart the gateway. Nothing else changed: the volumes and the provider configuration are the same.

The deployment test is `scripts/compose-admin-e2e.sh` (also run by `scripts/compose-e2e.sh`): login over HTTPS, wrong
password, session, out-of-scope routes, spoofed forwarding headers, plain HTTP, port 8766, mutual TLS and the source
network check, all on `127.0.0.1` with a high port.
