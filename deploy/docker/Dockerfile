# Mini Gateway in a container (docs/docker.md). Built from the GitHub release: the signature of SHA256SUMS is checked
# with the pinned Mini Gateway release key (the same key as install.sh and client/release_sig.go, never fetched from
# the download), then the signed version and the checksum of the binary.
#   SOURCE=release (default)  download minigw-linux-<arch> of v$VERSION from GitHub
#   SOURCE=local              copy minigw-linux-<arch> from the build context named "local" (compose.local.yaml)
ARG SOURCE=release

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS fetch-release
ARG VERSION
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl openssh-client \
 && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    [ -n "$VERSION" ] || { echo "set MINIGW_VERSION" >&2; exit 1; }; \
    base="https://github.com/gh-commiter/mini-llm-gateway/releases/download/v${VERSION}"; \
    asset="minigw-linux-${TARGETARCH}"; \
    cd /tmp; \
    curl -fsSL -o "$asset" "$base/$asset"; \
    curl -fsSL -o SHA256SUMS "$base/SHA256SUMS"; \
    curl -fsSL -o SHA256SUMS.sig "$base/SHA256SUMS.sig" || { echo "no SHA256SUMS.sig in release v${VERSION}" >&2; exit 1; }; \
    printf '%s\n' 'minigw-release namespaces="minigw-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH' > signers; \
    ssh-keygen -Y verify -f signers -I minigw-release -n minigw-release -s SHA256SUMS.sig < SHA256SUMS >/dev/null 2>&1 \
      || { echo "SHA256SUMS of v${VERSION} is not signed by Mini Gateway" >&2; exit 1; }; \
    signed="$(sed -n 's/^VERSION \([0-9][0-9.]*\)$/\1/p' SHA256SUMS | head -1)"; \
    [ "$signed" = "$VERSION" ] || { echo "signed version '$signed' is not $VERSION" >&2; exit 1; }; \
    want="$(grep " ${asset}\$" SHA256SUMS | cut -d' ' -f1 | head -1)"; \
    [ -n "$want" ] && [ "$(sha256sum "$asset" | cut -d' ' -f1)" = "$want" ] || { echo "checksum mismatch for $asset" >&2; exit 1; }; \
    install -m 0755 "$asset" /minigw; \
    echo "verified: minigw v${VERSION} ${TARGETARCH} (signature, version, checksum)"

FROM scratch AS fetch-local
ARG TARGETARCH
COPY --from=local minigw-linux-${TARGETARCH} /minigw

FROM fetch-${SOURCE} AS binary

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid 10001 minigw \
 && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/minigw --shell /usr/sbin/nologin minigw \
 && mkdir -p /var/lib/minigw/config /var/lib/minigw/scratch \
 && chown minigw:minigw /var/lib/minigw/config /var/lib/minigw/scratch \
 && chmod 700 /var/lib/minigw/config /var/lib/minigw/scratch
COPY --from=binary /minigw /usr/local/bin/minigw
ENV MINIGW_CONTAINER=1 HOME=/home/minigw
USER 10001:10001
WORKDIR /home/minigw
EXPOSE 8765
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 CMD ["minigw", "serve", "healthcheck"]
ENTRYPOINT ["minigw", "serve"]
CMD ["--config", "/var/lib/minigw/config/serve.json"]
