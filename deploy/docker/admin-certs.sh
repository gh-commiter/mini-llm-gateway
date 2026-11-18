#!/bin/sh
# Certificates for the Compose admin profile (docs/docker.md, "Remote settings from a browser"). POSIX sh + openssl only.
#   admin-certs.sh link DIR [--service NAME] [--identity NAME] [--origin URL]
#       a small private CA for the proxy-gateway link, the gateway's server certificate (SAN = NAME, the gateway's
#       Compose service name, default minigw) and the proxy's client certificate (DNS SAN = NAME, the proxyIdentity,
#       default settings-proxy.internal). Prints the matching remoteSettings block for serve.json; writes no config.
#   admin-certs.sh browser DIR NAME
#       SEPARATE: a private CA and a certificate for the browser-facing HTTPS name NAME (DNS SAN). A certificate from
#       your team's CA or ACME is better: put its chain and key at DIR/proxy/browser.pem and DIR/proxy/browser.key
#       instead, and skip this command. With this one, install DIR/ca/browser-ca.pem as trusted in each browser.
# Layout (DIR mode 0700, keys 0600; an existing file is never replaced: the command stops before writing anything):
#   ca/link-ca.{pem,key}  ca/browser-ca.{pem,key}   the CA keys stay here, never mounted into a container
#   gateway/  gateway.{pem,key}  link-ca.pem         mounted read-only into the gateway container (uid 10001)
#   proxy/    proxy-client.{pem,key}  link-ca.pem  browser.{pem,key}   mounted into the proxy container (uid 65532)
# Key type ECDSA P-256. Validity: CAs 10 years, link certificates 1 year, browser certificate 397 days (what browsers
# accept). Renew by moving the old files away and running the command again, then restart the services.
set -eu
umask 077
LINK_DAYS=365 CA_DAYS=3650 BROWSER_DAYS=397

die() { echo "admin-certs.sh: $*" >&2; exit 1; }
usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

dns_ok() { printf '%s' "$1" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$'; }

refuse_existing() { # files...: stop if any exists
  for f in "$@"; do [ ! -e "$f" ] && [ ! -L "$f" ] || die "$f exists; not replacing it (move it away to renew)"; done
}

mkdir_private() { [ -d "$1" ] || mkdir "$1"; chmod 700 "$1"; }

# key FILE: new EC P-256 key (SEC1 PEM), mode 0600
key() { openssl ecparam -name prime256v1 -genkey -noout -out "$1" 2>/dev/null; chmod 600 "$1"; }

# ca NAME DIR: self-signed CA key and certificate into DIR/NAME.key and DIR/NAME.pem
ca() {
  key "$2/$1.key"
  openssl req -new -key "$2/$1.key" -subj "/O=Mini Gateway admin profile/CN=$1" -out "$T/ca.csr"
  printf 'basicConstraints=critical,CA:TRUE,pathlen:0\nkeyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\n' > "$T/ca.ext"
  openssl x509 -req -in "$T/ca.csr" -signkey "$2/$1.key" -days "$CA_DAYS" -extfile "$T/ca.ext" -set_serial "0x$(openssl rand -hex 8)" -out "$2/$1.pem" 2>/dev/null
  chmod 644 "$2/$1.pem"
}

# leaf CANAME OUTBASE SAN EKU DAYS: certificate (OUTBASE.pem) and key (OUTBASE.key) signed by ca/CANAME
leaf() {
  key "$2.key"
  openssl req -new -key "$2.key" -subj "/O=Mini Gateway admin profile/CN=$3" -out "$T/leaf.csr"
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=%s\nsubjectAltName=DNS:%s\nsubjectKeyIdentifier=hash\n' "$4" "$3" > "$T/leaf.ext"
  openssl x509 -req -in "$T/leaf.csr" -CA "$DIR/ca/$1.pem" -CAkey "$DIR/ca/$1.key" -days "$5" -extfile "$T/leaf.ext" -set_serial "0x$(openssl rand -hex 8)" -out "$2.pem" 2>/dev/null
  chmod 644 "$2.pem"
}

owner_note() {
  if [ "$(id -u)" = 0 ]; then
    chown 10001:10001 "$DIR/gateway" "$DIR/gateway"/*
    chown 65532:65532 "$DIR/proxy" "$DIR/proxy"/*
    echo "owners set: gateway/ to uid 10001, proxy/ to uid 65532"
  else
    echo "On a Linux host the containers must be able to read their keys (Docker Desktop on a Mac needs nothing). Once, as root, not recursive:"
    echo "  sudo chown 10001:10001 $DIR/gateway $DIR/gateway/*"
    echo "  sudo chown 65532:65532 $DIR/proxy $DIR/proxy/*"
  fi
}

[ $# -ge 2 ] || usage
cmd=$1 DIR=$2; shift 2
command -v openssl >/dev/null || die "openssl not found"
SERVICE=minigw IDENTITY=settings-proxy.internal ORIGIN=https://settings.example.net
T=$(mktemp -d "${TMPDIR:-/tmp}/admin-certs.XXXXXX")
trap 'rm -f "$T"/*; rmdir "$T"' EXIT INT TERM

case $cmd in
link)
  while [ $# -gt 0 ]; do
    case $1 in
      --service) SERVICE=${2:?}; shift 2 ;;
      --identity) IDENTITY=${2:?}; shift 2 ;;
      --origin) ORIGIN=${2:?}; shift 2 ;;
      *) usage ;;
    esac
  done
  dns_ok "$SERVICE" || die "--service must be a lowercase DNS name"
  dns_ok "$IDENTITY" || die "--identity must be a lowercase DNS name"
  refuse_existing "$DIR/ca/link-ca.pem" "$DIR/ca/link-ca.key" "$DIR/gateway/gateway.pem" "$DIR/gateway/gateway.key" \
    "$DIR/gateway/link-ca.pem" "$DIR/proxy/proxy-client.pem" "$DIR/proxy/proxy-client.key" "$DIR/proxy/link-ca.pem"
  mkdir_private "$DIR"; mkdir_private "$DIR/ca"; mkdir_private "$DIR/gateway"; mkdir_private "$DIR/proxy"
  ca link-ca "$DIR/ca"
  leaf link-ca "$DIR/gateway/gateway" "$SERVICE" serverAuth "$LINK_DAYS"
  leaf link-ca "$DIR/proxy/proxy-client" "$IDENTITY" clientAuth "$LINK_DAYS"
  cp "$DIR/ca/link-ca.pem" "$DIR/gateway/link-ca.pem"; cp "$DIR/ca/link-ca.pem" "$DIR/proxy/link-ca.pem"
  echo "created in $DIR: link CA (10 years), gateway server certificate (SAN $SERVICE) and proxy client certificate (SAN $IDENTITY), 1 year, ECDSA P-256"
  owner_note
  echo
  echo "remoteSettings block for serve.json (paths as mounted in the gateway container; adjust origin and allowedSourceCIDRs):"
  cat <<EOF
"remoteSettings": {
  "enabled": true,
  "listen": "0.0.0.0:8766",
  "origin": "$ORIGIN",
  "allowedSourceCIDRs": ["192.168.10.0/24"],
  "proxyIdentity": "$IDENTITY",
  "proxyCA": "/admin-tls/link-ca.pem",
  "certificate": "/admin-tls/gateway.pem",
  "privateKey": "/admin-tls/gateway.key"
}
EOF
  ;;
browser)
  [ $# -eq 1 ] || usage
  NAME=$1
  dns_ok "$NAME" || die "NAME must be a lowercase DNS name (no port)"
  [ -d "$DIR/proxy" ] || die "$DIR/proxy not found: run 'link' first"
  refuse_existing "$DIR/ca/browser-ca.pem" "$DIR/ca/browser-ca.key" "$DIR/proxy/browser.pem" "$DIR/proxy/browser.key"
  ca browser-ca "$DIR/ca"
  leaf browser-ca "$DIR/proxy/browser" "$NAME" serverAuth "$BROWSER_DAYS"
  cat "$DIR/ca/browser-ca.pem" >> "$DIR/proxy/browser.pem"   # chain: certificate, then its CA
  [ "$(id -u)" != 0 ] || chown 65532:65532 "$DIR/proxy/browser.pem" "$DIR/proxy/browser.key"
  echo "created: browser certificate for $NAME (397 days, ECDSA P-256) signed by a private CA."
  echo "Install $DIR/ca/browser-ca.pem as a trusted root in each browser or computer that opens the page."
  echo "A certificate from your team's CA or ACME is better: put chain and key at $DIR/proxy/browser.pem and browser.key."
  ;;
*) usage ;;
esac
