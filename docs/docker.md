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
- `minigw serve install` and updates from inside the container (`gw` administration works).
- Pre-built images in a registry, automatic updates, Kubernetes or Swarm, several gateways on one database.

## Remote settings from a browser (admin profile)

An optional second service, a [Caddy](https://caddyserver.com) reverse proxy (image pinned by digest), makes the
[remote settings listener](gateway.md#remote-settings-listener) reachable from a browser on your LAN. It is a separate
file, `compose.admin.yaml`: without it nothing of this exists, and a plain `docker compose up -d` does not touch your
gateway container (the admin file is what adds the certificate mount and the `admin` network to it; adding it
recreates the gateway container once, the volumes stay).

**Prerequisites:** a DNS name for the page that your browsers resolve to the Docker host (`settings.example.net`
below), a LAN address of the host to publish on, `openssl`, and the gateway already running. A port other than 443
works too, but it becomes part of the origin (`https://settings.example.net:8443`).

**Where it works as a real filter.** The proxy attests the TCP peer address of the browser connection and the gateway
applies `allowedSourceCIDRs` and its per-source lockout to it. That is only meaningful on **rootful Docker on Linux**,
where the browser's LAN address reaches the proxy. On Docker Desktop (Mac, Windows) and on rootless Docker the proxy sees
the Docker gateway address (`172.x`) for every browser: then `allowedSourceCIDRs` cannot tell browsers apart, one
person's wrong passwords lock out everybody, and a good sign-in by one marks the shared source as trusted for the others.
Docker hides this; the gateway cannot fix it. Check it after your first sign-in:

```sh
docker compose exec minigw tail -n 3 /var/lib/minigw/config/remote-settings.audit.jsonl   # the IP field is the source the gateway saw
```

If that is a Docker address, restrict who can reach the page with `MINIGW_ADMIN_BIND` and the network firewall instead,
and do not copy that address into `allowedSourceCIDRs` (it would admit everyone who can reach the port). Docker also
publishes ports ahead of `ufw` and `firewalld` rules: filter in the `DOCKER-USER` chain
(`iptables -I DOCKER-USER -i <lan interface> ! -s 192.168.10.0/24 -p tcp --dport 443 -j DROP`) or at the network firewall.

**1. Certificates.** From `deploy/docker`:

```sh
sh admin-certs.sh link ./admin-certs --origin https://settings.example.net     # prints the serve.json block
sh admin-certs.sh browser ./admin-certs settings.example.net                   # optional, see below
```

`link` creates a private CA for the proxy-gateway link (ECDSA P-256, 10 years), the gateway's server certificate
(SAN `minigw`, the Compose service name) and the proxy's client certificate (SAN `settings-proxy.internal`, the
`proxyIdentity`), both for 1 year. Files are 0600 in a 0700 folder, nothing is overwritten (it stops and names the
file), no secret is printed. The CA keys stay in `ca/` and are never mounted: the gateway sees only `gateway/`, the
proxy only `proxy/`. Run both commands first, then (on a Linux host) give each folder to its container user once, as the
script prints (`chown 10001:10001` for `gateway/`, `65532:65532` for `proxy/`, not recursive).

**Use your team's CA or an ACME certificate for the browser name if you have one** (recommended): put its chain and key at
`admin-certs/proxy/browser.pem` and `browser.key` and skip `browser`. `browser` is the fallback: a private CA that is
name-constrained to that one name (it cannot vouch for any other site), valid 2 years, and a 397-day certificate. Install
`admin-certs/ca/browser-ca.pem` as a trusted root in each browser, then **move `ca/browser-ca.key` off the server**: it
is needed only for renewal. If you lose control of that key, remove the root from the browsers.

**2. Password**, typed inside the container (it works while the gateway is stopped):

```sh
docker compose exec minigw minigw gw settings password
```

**3. Configuration.** Add the block that step 1 printed to `serve.json` (in the config volume; edit it with the same
care as any config) and set `origin` and `allowedSourceCIDRs` (see "Where it works" above) to your values;
`proxyIdentity`, the three `/admin-tls/…` paths and `0.0.0.0:8766` stay as printed.

**4. Start the proxy.** In `.env` (next to `compose.yaml`):

```
COMPOSE_FILE=compose.yaml:compose.admin.yaml
MINIGW_ADMIN_NAME=settings.example.net     # the external name, required (no port)
MINIGW_ADMIN_BIND=192.168.10.5             # the host address to publish on; loopback if unset
MINIGW_ADMIN_PORT=443                      # the host port
```

```sh
docker compose up -d        # recreates the gateway with the mount, starts the proxy
```

Open `https://settings.example.net`, sign in. Passwords and sessions follow the gateway's rules (lockout, 30 minutes
idle, audit file `remote-settings.audit.jsonl` in the config volume). The proxy's log holds no passwords and no session
values (the session header is filtered out of Caddy's error log; the e2e test checks it).

**What is reachable:** only the settings page, login and logout, and the provider, model and device routes that the
gateway allows remotely; everything else, including inference, projects, sync and the other admin routes, answers 404
at the proxy and is refused again by the gateway. The proxy also refuses any path with dot segments, doubled slashes or
percent-encoding, so it never forwards what it did not check. Port 8766 is never published; only HTTPS is, no HTTP port
exists (no redirect: a redirect would invite typing a password on an `http://` address; HSTS keeps browsers on HTTPS).
The gateway listens on 8766 on every network it joins, so a container on a network shared with it (`default`, a
`litellm` network) can connect, and mutual TLS refuses all but the proxy. The proxy checks the gateway's certificate
and name, and replaces the client-address header with the real peer address; it deletes `X-Forwarded-*`, `Forwarded`,
`X-Real-IP` and any `X-Minigw-Client-IP` the browser sent. Request bodies are capped at 1 MB. The proxy sees the traffic
after TLS, so keep it on the host you trust, and guard `admin-certs/proxy/proxy-client.key`: whoever holds it can claim
any client address to the gateway.

**Trust statement:** the gateway cannot see the browser's connection. It trusts the authenticated proxy to attest it:
the client address and that the browser used HTTPS. That is why the HTTPS-only proxy configuration, HSTS and the
header replacement are part of the supported boundary: do not change the Caddyfile to accept plain HTTP or to trust
forwarding headers, and do not give any other service the proxy's key.

**Turn it off:** remove the `COMPOSE_FILE` line, then `docker compose up -d --remove-orphans` (stops the proxy, takes the
mount off the gateway; the gateway is recreated, the volumes stay). To also close the listener set
`remoteSettings.enabled` to `false` and restart the gateway, which discards all remote sessions.

**Renewal:** link certificates last 1 year, the browser certificate 397 days (an ACME or team certificate has its own).
With the CA keys present (put `ca/browser-ca.key` back for the time it takes):

```sh
sh admin-certs.sh renew ./admin-certs --browser settings.example.net      # same --service/--identity as before if you set them
docker compose restart minigw admin-proxy
```

`renew` keeps the CAs, replaces the leaf certificates and keys (each file atomically) and refuses when a CA key it needs
is absent. When a CA itself is near its end (browser CA: 2 years) or a key leaked (the proxy's key: anyone who holds it
can set the client address), start over: move `admin-certs` away, run `link` and `browser` again, restart both services;
browsers need the new `browser-ca.pem`. An expired certificate makes the connection fail closed: the page does not load;
there is no HTTP fallback.

**Rollback:** turn it off as above, and remove the `remoteSettings` block (or set `enabled` to `false`) and restart the
gateway. Nothing else changed: the volumes and the provider configuration are the same.

The deployment test is `scripts/compose-admin-e2e.sh` (also run by `scripts/compose-e2e.sh`): login over HTTPS, wrong
password, session, out-of-scope and unclean routes, spoofed forwarding headers, plain HTTP, port 8766, mutual TLS, the
source network check, the proxy log, the CA constraint and renewal, all on `127.0.0.1` with a high port.
