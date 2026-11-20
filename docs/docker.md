# Mini Gateway in Docker Compose

A Linux gateway (`minigw serve`) in a container, for providers only: Claude CLI, Codex, Grok, Copilot and Foundry Local
are off and stay off (the default container has no user namespaces, so the agent sandbox does not work; the gateway
refuses them in the settings, in an imported `serve.json` and when a request runs). Providers (LiteLLM, OpenRouter, xAI,
any OpenAI-compatible endpoint), devices, projects and notes work as on any other gateway. Remote web settings cover
providers and devices, plus read-only lists of users, bindings, teams and projects and who is in a team and who has access to a project (changes stay with
`docker compose exec minigw minigw gw …`), through the optional `compose.admin.yaml` proxy below.
Administer users, teams, projects, KB review and upstream with `docker compose exec minigw minigw gw …`.
The image has no `minigw-tui`: `minigw gw settings` does not work in the container.

The files are in [`deploy/docker/`](../deploy/docker): `compose.yaml` and `Dockerfile`. The image is built on your
computer from the GitHub release: no registry, no account. The build checks the signature of `SHA256SUMS` with the
Mini Gateway release key that is written in the Dockerfile (the same key as [install.sh](install.md#check-a-download);
it is never fetched from the download), then the signed version, then the checksum of the binary. A failed check stops
the build.

## Install and first start

### First setup on a team server

Run these commands in order (choose the host bind address and protect it with the host firewall):

1. Clone and enter the deployment directory:
   ```sh
   git clone https://github.com/gh-commiter/mini-llm-gateway
   cd mini-llm-gateway/deploy/docker
   ```
2. Save the release and bind address in `.env`:
   ```sh
   printf "MINIGW_VERSION=0.8.0\nMINIGW_BIND=127.0.0.1\n" > .env
   ```
   Replace loopback with your server address to admit other computers. `MINIGW_PORT` defaults to 8765.
3. Build and start:
   ```sh
   docker compose up -d --build
   ```
4. Check health:
   ```sh
   docker compose exec minigw minigw serve healthcheck
   ```
5. Add the first provider in an interactive terminal (keep the default TTY; the key prompt disables echo):
   ```sh
   docker compose exec minigw minigw gw provider add OpenRouter https://openrouter.ai/api
   docker compose exec minigw minigw gw provider models OpenRouter --enable all
   ```
6. Follow [team onboarding](team-onboarding.md) for users, teams and projects, prefixing its commands
   with `docker compose exec minigw`.

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

## Backup

`backup.sh` needs Python 3 on the server (it reads `docker compose config --format json`). Run it next to
`compose.yaml`:

```sh
sh backup.sh --stop ./backups
# Or stop first, then: sh backup.sh ./backups
sh backup.sh --restore ./backups/minigw-backup-<UTC time>
# Restore requires stopped services and empty or absent target volumes.
docker compose up -d
```

**Use the same Compose environment as the deployment.** The script cannot know how the deployment was started,
only what Compose resolves in the current folder and environment. Run it from the same folder with the same `.env`,
or set the same `COMPOSE_PROJECT_NAME` and `COMPOSE_FILE`, including the admin overlay
(`COMPOSE_FILE=compose.yaml:compose.admin.yaml`) if you use it. Its first line is
`Project: <name>; volumes: <config>, <scratch>`: check that these are the volumes you mean. A wrong project
usually has the volumes of another deployment or none, and never a container of this deployment, so a backup
refuses when the project has no container at all. If you took the deployment down on purpose
(`docker compose down` keeps the volumes but removes the containers), add `--offline`. A restore refuses a
backup made in another project unless you give `--into-project <current project>`: that is how you restore
into a separate project, for example as a recovery drill.

The script resolves real volume names from Compose, refuses missing backup sources, and checks checksums before
restoring. With `--stop` it always runs `docker compose stop`, then refuses if any container (of any project)
that mounts either volume is anything other than stopped (exited or created), and afterwards starts only the services
that were running before, also when archiving fails. A bind mount of a volume's backing directory by some other
container is not detected. Restore makes the same check. When the project has a gateway container, the script also
checks that its real mounts are the two volumes Compose resolves, and refuses if not (a missing `-f` override can
rename them). Restore prints `Restoring backup of <project> (<volumes>) into <project> (<volumes>)` and needs
`--into-project` whenever the project or the volume names differ from the manifest. With `--offline` nothing can
confirm the project: the printed `Project:` line is what you must check. The checksums guard against a damaged or
mixed-up archive; a backup directory must be protected like the volume itself (it holds the keys), and restore
does not inspect archive content.
The script streams the archives through a helper container and bind-mounts no host folder. Where to put backups:
not on the Docker data disk if that is small (a failed backup removes its own incomplete directory, but a full disk
stops the gateway too); a root_squash NFS target must be writable by the user who runs the script. A helper that
cannot start (image missing and not pullable, a remote Docker context) is reported with Docker's own message.
A failed backup is
incomplete and must not be used. Both volumes belong together. The config volume holds provider keys in protected
files: a backup is a secret. Keep it private and copy it to protected storage.
`docker compose down` keeps volumes; `down -v` deletes all their data.
A restore leaves a "restore pending" file (`core/core.restored`) in the config volume. The gateway reads it at its next start, marks every upstream link "restored: reconcile before pushing" in one database
transaction (a link marked this way sends nothing until the owner runs `minigw gw upstream reconcile <project> --upstream <name>`; `gw upstream push <project> --upstream <name> --after-restore` is the override that skips it and can create second copies) and only then removes the file. Only a gateway 0.9.2 or newer reads the file, so `backup.sh --restore` writes it only when the image to run is that new (or a local build) and says so when it does not; a backup never contains it. A volume
copied back by some other means leaves no such file and is not detected: create it by hand before starting (command in the upstream chapter of `usage.md`).
If a restore fails during extraction, its targets may be partially populated. Keep the backup,
inspect the failure and remove only those failed restore volumes before retrying into empty targets.

Manual equivalent (default project; substitute the exact names shown by Compose for an override):

```sh
docker compose config --format json      # volumes.config.name and volumes.scratch.name
docker volume ls                        # verify minigw_config and minigw_scratch exist
docker volume inspect minigw_config minigw_scratch || exit 1
docker compose stop
umask 077
mkdir -m 700 manual-backup
for v in config scratch; do
  docker run --rm --network none --mount type=volume,src=minigw_$v,dst=/v,readonly \
    debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 \
    tar -C /v -cf - . > manual-backup/$v.tar
done
docker compose start
```

The default project is `minigw`, independent of the folder name; `COMPOSE_PROJECT_NAME` overrides it.
Prefer the script for validation, checksums and restart handling.

## Updating and going back

Always run `backup.sh --stop` **before changing `MINIGW_VERSION`**, using the same Compose environment as the
deployment. Keep that backup off the Docker data disk. Going back means the old version **and both volumes from
the backup made before the update**. Restoring a backup made after the update does not undo a database migration.

From `deploy/docker`, with the old version still selected:

```sh
sh backup.sh --stop ./backups
# Save the printed Backup: path and the old MINIGW_VERSION before continuing.
export MINIGW_VERSION=<new-release>
docker compose up -d --build
docker compose exec minigw minigw serve healthcheck
docker compose exec minigw minigw gw status
docker compose exec minigw minigw gw project list
```

`deploy/docker/Caddyfile` and the compose files (`compose.yaml`, `compose.local.yaml`, `compose.admin.yaml`) belong to
the version. If you use the admin profile, copy those files of the new release into your deployment folder too and run
`docker compose restart admin-proxy`: Caddy keeps the old `Caddyfile` until then, and a proxy that is older than the
gateway answers 404 to the new read-only lists on the "Team (read-only)" page.

Wait for health and check `projects: on`, then check your users, memberships, notes and pending review. Liveness alone
does not prove the project database opened. `gw status` does not print its schema number when the core is available.
The rehearsal below checks the schema in an archived copy of the stopped database too.

To go back, first preserve any new work separately: the pre-update backup contains none of it. Resolve the exact
volume names before taking down the deployment:

```sh
docker compose config --format json   # read volumes.config.name and volumes.scratch.name
docker compose down
# Destructive: substitute ONLY those two exact volume names after checking them.
docker volume rm <config-volume> <scratch-volume>
export MINIGW_VERSION=<old-release>
sh backup.sh --restore ./backups/minigw-backup-<pre-update-UTC-time>
docker compose up -d --build
docker compose exec minigw minigw serve healthcheck
docker compose exec minigw minigw gw status
docker compose exec minigw minigw gw project list
```

Keep the chosen version in `.env` for future commands too. Do not run `down -v` on an unrelated deployment.
For recovery on a clean host, copy the deployment files and the protected backup directory, select the version,
and restore into absent or empty volumes. A separate project needs explicit consent in the restore command:

```sh
export COMPOSE_PROJECT_NAME=minigw-recovered
export MINIGW_VERSION=<new-release>
sh backup.sh --restore --into-project minigw-recovered ./backups/minigw-backup-<pre-update-UTC-time>
docker compose up -d --build
```

The new version can restore the pre-update backup and migrate it again. An older version refuses a newer schema;
models and liveness can still work while projects and notes answer 503 `core_unavailable`. A release without
service-name providers also refuses a configuration with a provider such as `http://litellm:4000`.

The rehearsal captured this exact line from 0.7.6's `gw status` after restoring a schema-25 backup:

```text
projects: off (core database has a newer schema than this program (schema 25, program 12))
```

Its project listing failed, and after stopping the refused gateway `core.db` was byte-identical to the restored file.

Before migrating, the gateway writes `core-<old-schema>-<UTC-time>.db` next to `core.db` in the config volume's
`core` folder. It is a consistent database snapshot for an offline `minigw gw core restore`, not a deployment backup:
it lacks the configuration, provider keys and scratch volume, and is lost with that volume. The newest three snapshots
are kept, plus the oldest snapshot of the lowest schema for 30 days (and protection during failed migrations).

`zsh scripts/compose-recovery-e2e.sh` rehearses the signed 0.7.6 release (schema 12, the first with container mode),
this branch's Linux binary, going back, and recovery into a separate project. It needs zsh on Linux too, Docker
Compose v2 with BuildKit, Go with cached modules, Python 3, curl, ssh-keygen and shasum. Its provider stub listens only
on container loopback; published ports bind to `127.0.0.1`. `SCRATCH_DIR` selects where it retains private evidence
and backup directories; otherwise it uses the platform cache convention described at the top of the script.
It checks the refused-start database bytes and captures the old gateway's status, project error and logs.
A loud SKIP returns nonzero unless `ALLOW_SKIP=1`; release gates should leave that unset. No Linux server run is
claimed by this rehearsal on a development machine.

## Rehearsal before inviting the team

Run the whole team workflow once in an isolated Compose project on the real server, before anyone is invited. From a
clone of the repository, as a user who may use Docker (POSIX `sh`; needs Docker Compose v2 and Python 3, `openssl` and `curl` for
`--admin`, and beyond POSIX a `find` with `-maxdepth`, `grep -o`, `tail -c`, `date +%s` and `test -O`, which GNU, BusyBox and BSD tools all have;
the script checks this first):

```sh
sh scripts/compose-team-rehearsal.sh --admin        # --version X.Y.Z if deploy/docker/.env does not name the release
```

It builds the release the way this page says (the build checks the signature, the signed version and the checksum),
starts the project `minigw-rehearsal` on its own free ports (on `127.0.0.1`; `--bind` for another address) with its own
volumes, and runs: health and `gw status`; a team, two projects and three users (reader, developer, admin) through the
owner CLI; device approval and binding; an access matrix with random sentinel notes (a reader sees structure notes only, a
private note is seen only by its author and the project admins, team membership alone opens no project; every refusal must be
the product's own message); submit, review, accept, replacement, `note history` and `note restore`; with `--admin` the remote
page (certificates, HTTPS login, forged `Origin`, wrong-password lockout, routes outside the list, the Team and Review routes);
`backup.sh --stop`, a restore into a second isolated project and a comparison of users, bindings, roles, notes with their
history, search, provider configuration and the gateway identity; at the end a scan of the container logs for every credential the run created.
`--provider-url` adds a provider (the key is asked without echo on a terminal, or read from `--provider-key-file`, a file of yours with mode 600).
The devices are simulated: clients run from a second container of the same image on the Compose network. A step that cannot
run is a FAIL with the reason. These steps are not run and print `TODO(operator)` with the exact commands: remote settings
without `--admin`, the provider without `--provider-url`, one model request (it needs Claude Code), a real Mac and a real
Windows client, a browser on another machine and the upstream link.

It refuses a project name that already has containers, networks or volumes, or whose existence Docker cannot tell (never `minigw`,
the default project). The image is built into its own tag, `minigw-rehearsal:<run id>`, never the shared `minigw:<version>`
a production project may use. The working directory (`--workdir`, default `${TMPDIR:-/tmp}/<project>-work`) must not exist: the script
makes it (mode 700, with a marker file naming the project and the run id; its parent must not be writable by others), and every
volume, network and container it creates in Docker, the restore project's included, carries that run id as a label. A name never
authorizes a removal, and a Docker error is never read as "nothing there". The caller's own account is trusted; the marker proves
what this run made, it does not defend against the account that owns it. `--reuse` only continues the leftover of an interrupted run,
proven by that marker file and the marker volume, with the settings (release, addresses, ports, admin name) that run saved:
a flag that differs is refused. After an interruption (Ctrl-C) the script prints the exact recovery command. At the end it asks
on a terminal whether to remove what it made, or does so with `--cleanup`; later `--cleanup-only --project <name> --workdir <dir>`
removes exactly the containers, networks, volumes and image tag carrying that run id, checks that nothing is left and
says what remains or could not be checked (the marker and working directory stay then). Secrets never appear in
a command line: the generated remote page password goes to a mode-600 file in the working directory (its path is printed), the
session header goes to curl through a private config file.
The report `rehearsal-report-<run id>.md` in the current directory lists versions, the image, the source filter that was
used, one row per step (`ok`, `FAIL`, `TODO`) with seconds, the access matrix and the operator's commands; the last lines say
how the cleanup ended and give the verdict. Only the detail text is sanitized (host and user names, paths, addresses, the admin name,
the provider URL and every credential of the run are replaced), never the verdict columns, and the report is not written if a
credential value would be in it. The exit code is 0 only when no step FAILED and the cleanup, if requested, succeeded; every
`TODO(operator)` row is still yours before you invite the team. The remote page's source filter `allowedSourceCIDRs` is set to the
Compose admin network's subnet when that works with a browser on the host, otherwise it is opened to all IPv4 for the rehearsal; the report
says which, and in neither case is the documented LAN filter proven: only the second-machine browser item does that.

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

The page manages providers and devices and shows users, device-user bindings, teams and projects read-only (menu item
"Team (read-only)"). Change those with `docker compose exec minigw minigw gw …` (the one exception: the "Review" page accepts or rejects pending knowledge-base submissions, the exact version you read; a replacement is shown with the current text of the note and the new text and can be accepted there while that note is unchanged and both texts are shown in full; long ones, ones with hidden characters and replacements whose note changed or no longer exists can only be rejected there (the rest is reviewed with `minigw gw kb review`); notes, the FAQ and paging beyond the first 100 submissions stay with the CLI; a rejected author sees the reason only with `minigw kb submissions --json` today); the proxy and the listener allow only
the four `GET` lists plus three `GET` views with one numeric id (who is in a team, what a team assigns, who has access to
a project and why), nothing deeper (no grants, dependencies or member changes). Rows of teams and projects open on a
click or Enter. After an update copy the release's
`Caddyfile` into your deployment folder and run `docker compose restart admin-proxy`, otherwise those lists answer 404.

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

**What is reachable.** The page and its sign-in, and these routes below `/minigw/admin/v1/` (nothing else: inference, sync,
notes, history, the other admin routes and every other method answer 404 at the proxy and are refused again by the gateway).
`GET /minigw/settings` is the page; `POST /minigw/settings/login` and `POST /minigw/settings/logout` sign in and out.

<!-- remote-routes:begin (docs/docker.md; the test TestRemoteRoutesTableMatchesTheAllowList compares this table with remoteAdminAllowed) -->
| Methods | Path (below `/minigw/admin/v1/`) | What it does |
|---|---|---|
| GET, POST | `providers`, `devices` | list; add (a provider's key is entered here) |
| PATCH, DELETE | `providers/{name}`, `devices/{name}` | change or remove |
| GET, POST | `providers/{name}/models` | list and enable models |
| GET | `devices/{name}/history` | a device's connections |
| GET | `users`, `bindings`, `teams`, `projects` | read-only lists |
| GET | `teams/{id}`, `teams/{id}/assignments`, `projects/{id}/members` | read-only: a team, who is assigned, who has access to a project and why |
| GET | `kb/submissions`, `kb/submissions/{id}` | pending knowledge-base submissions and their text (the first 100) |
| POST | `kb/submissions/{id}/accept`, `kb/submissions/{id}/reject` | decide a submission |
<!-- remote-routes:end -->

**A remote accept publishes to the knowledge base**, for everyone who can read that project or team. A stolen session can
therefore read every pending text (for a replacement also the current text of the note it replaces, private notes
included) and publish it. What cannot be accepted remotely: a text with hidden characters, a text too long to review on the
page, and a replacement whose note changed or no longer exists since it was written or whose current text is too long (reject
works). Two limits apply: at most 30 decisions that reach the core per 10 minutes (the 429 carries `Retry-After`), and a higher
limit of 300 refused decisions per 10 minutes; the exact rules are in [gateway.md](gateway.md#remote-settings-listener). Notes,
note history and restore, sync and every change of projects, teams or users stay with the CLI. To make the remote review
reject-only, remove `accept` from the proxy's `@post` matcher (and from `remoteAdminAllowed`). The proxy also refuses any path with dot segments, doubled slashes or
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
