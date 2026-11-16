# Mini Gateway in Docker Compose

A Linux gateway (`minigw serve`) in a container, for providers only: Claude CLI, Codex, Grok, Copilot and Foundry Local
are off and stay off (the default container has no user namespaces, so the agent sandbox does not work; the gateway
refuses them in the settings, in an imported `serve.json` and when a request runs). Providers (LiteLLM, OpenRouter, xAI,
any OpenAI-compatible endpoint), devices, projects and notes work as on any other gateway. Remote web settings are not
part of this deployment yet: you administer it with `docker compose exec minigw minigw gw …`.

The files are in [`deploy/docker/`](../deploy/docker): `compose.yaml` and `Dockerfile`. The image is built on your
computer from the GitHub release: no registry, no account. The build checks the signature of `SHA256SUMS` with the
Mini Gateway release key that is written in the Dockerfile (the same key as [install.sh](install.md#check-a-download);
it is never fetched from the download), then the signed version, then the checksum of the binary. A failed check stops
the build.

## Install and first start

```sh
git clone https://github.com/gh-commiter/mini-llm-gateway && cd mini-llm-gateway/deploy/docker
MINIGW_BIND=0.0.0.0 docker compose up -d --build     # MINIGW_BIND: the host address to publish port 8765 on
```

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

Inside the container `localhost` is the container itself. Put both services on one Docker network and give LiteLLM
the network alias `litellm.local` (the gateway accepts plain `http` only for localhost, a private IP or a `.local`
name, and only with `--insecure`):

```yaml
# in your LiteLLM compose file
services:
  litellm:
    networks: { default: { aliases: [litellm.local] } }
# the gateway joins its network: docker network connect <litellm-network> minigw-minigw-1
```

```sh
docker compose exec minigw minigw gw provider add LiteLLM http://litellm.local:4000 --insecure
```

(To keep the connection over a reconnect, add the network to the `minigw` service in a `compose.override.yaml`.)

## Update

Change the version and rebuild. The default is the latest stable release; a beta only by its explicit version:

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
restore the backup you took before the update (both volumes), then start the older version.

## Hardening that is on

Non-root user (uid 10001), read-only root file system with a `/tmp` tmpfs, all capabilities dropped,
`no-new-privileges`, an init process, limits of 1 GB memory and 512 processes, `restart: unless-stopped`, a 45 s stop
grace period (SIGTERM stops new requests, running ones get 30 s, then the database is closed). No Docker socket, host
home or host network. Bind-mounted folders instead of the named volumes must belong to uid 10001 (`chown 10001:10001`
on the host, once, not recursively from a container).

## Not supported yet

- Agents (Claude CLI, Codex, Grok, Copilot, Foundry Local) in the container.
- Remote web settings, `minigw serve install` and updates from inside the container (`gw` administration works).
- Pre-built images in a registry, automatic updates, Kubernetes or Swarm, several gateways on one database.
