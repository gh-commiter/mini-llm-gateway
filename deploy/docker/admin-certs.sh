#!/bin/sh
# Certificates for the Compose admin profile (docs/docker.md, "Remote settings from a browser"). POSIX sh + openssl only.
#   admin-certs.sh link DIR [--service NAME] [--identity NAME] [--origin URL]
#       a small private CA for the proxy-gateway link, the gateway's server certificate (SAN = NAME, the gateway's
#       Compose service name, default minigw) and the proxy's client certificate (DNS SAN = NAME, the proxyIdentity,
#       default settings-proxy.internal). Prints the matching remoteSettings block for serve.json; writes no config.
#   admin-certs.sh browser DIR NAME
#       SEPARATE: a private CA (2 years, name-constrained to NAME and its subdomains: it cannot vouch for any other site)
#       and a certificate for the browser-facing HTTPS name NAME (DNS SAN). A certificate from your team's CA or ACME is
#       the better choice: put its chain and key at DIR/proxy/browser.pem and DIR/proxy/browser.key instead and skip
#       this command. With this one, install DIR/ca/browser-ca.pem as trusted in each browser, then MOVE
#       DIR/ca/browser-ca.key OFF this server (offline); it is needed only for renewal.
#   admin-certs.sh renew DIR [--service NAME] [--identity NAME] [--browser NAME]
#       keep the existing CA(s), replace the leaf certificates and keys (each file replaced atomically). Refuses when
#       the CA key it needs is absent (put it back for the renewal, then move it away again). Pass the same names.
# Layout (DIR mode 0700, keys 0600; an existing file is never replaced: the command stops before writing anything):
#   ca/link-ca.{pem,key}  ca/browser-ca.{pem,key}   the CA keys stay here, never mounted into a container
#   gateway/  gateway.{pem,key}  link-ca.pem         mounted read-only into the gateway container (uid 10001)
#   proxy/    proxy-client.{pem,key}  link-ca.pem  browser.{pem,key}   mounted into the proxy container (uid 65532)
# Key type ECDSA P-256. Validity: link CA 10 years, browser CA 2 years, link certificates 1 year, browser certificate
# 397 days (what browsers accept). Renew with the renew command, then restart the services.
set -eu
umask 077
LINK_DAYS=365 CA_DAYS=3650 BROWSER_CA_DAYS=730 BROWSER_DAYS=397

die() { echo "admin-certs.sh: $*" >&2; exit 1; }
usage() { sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }

dns_ok() { printf '%s' "$1" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$'; }

refuse_existing() { # files...: stop if any exists
  for f in "$@"; do [ ! -e "$f" ] && [ ! -L "$f" ] || die "$f exists; not replacing it (move it away to renew)"; done
}

mkdir_private() { [ -d "$1" ] || mkdir "$1"; chmod 700 "$1"; }

# key FILE: new EC P-256 key (SEC1 PEM), mode 0600
key() { openssl ecparam -name prime256v1 -genkey -noout -out "$1" 2>/dev/null; chmod 600 "$1"; }

# ca NAME DIR DAYS [PERMITTED_DNS]: self-signed CA key and certificate into DIR/NAME.key and DIR/NAME.pem; with
# PERMITTED_DNS the CA is critically name-constrained to that name (and its subdomains)
ca() {
  key "$2/$1.key"
  openssl req -new -key "$2/$1.key" -subj "/O=Mini Gateway admin profile/CN=$1" -out "$T/ca.csr"
  printf 'basicConstraints=critical,CA:TRUE,pathlen:0\nkeyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\n' > "$T/ca.ext"
  [ -z "${4:-}" ] || printf 'nameConstraints=critical,permitted;DNS:%s\n' "$4" >> "$T/ca.ext"
  openssl x509 -req -in "$T/ca.csr" -signkey "$2/$1.key" -days "$3" -extfile "$T/ca.ext" -set_serial "0x$(openssl rand -hex 8)" -out "$2/$1.pem" 2>/dev/null
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

# browser_leaf NAME OUTBASE: the browser certificate, chain = certificate then its CA
browser_leaf() {
  leaf browser-ca "$2" "$1" serverAuth "$BROWSER_DAYS"
  cat "$DIR/ca/browser-ca.pem" >> "$2.pem"
}

# quoted for a copy-paste shell command
q() { printf "'%s'" "$1"; }

chown_note() { # gateway|proxy|both
  if [ "$(id -u)" = 0 ]; then
    [ "$1" = proxy ] || chown 10001:10001 "$DIR/gateway" "$DIR/gateway"/*
    [ "$1" = gateway ] || chown 65532:65532 "$DIR/proxy" "$DIR/proxy"/*
    echo "owners set: gateway/ to uid 10001, proxy/ to uid 65532"
  else
    echo "On a Linux host each container must be able to read its keys (Docker Desktop on a Mac needs nothing)."
    echo "Once all certificate commands are done, as root, not recursive:"
    [ "$1" = proxy ] || echo "  sudo chown 10001:10001 $(q "$DIR/gateway") $(q "$DIR")/gateway/*"
    [ "$1" = gateway ] || echo "  sudo chown 65532:65532 $(q "$DIR/proxy") $(q "$DIR")/proxy/*"
  fi
}

ca_key_note() {
  echo "Move $(q "$DIR/ca/browser-ca.key") OFF this server now (offline storage): anyone with it can issue certificates for $NAME."
  echo "It is needed only for renewal: put it back, run 'renew --browser $NAME', move it away again."
}

[ $# -ge 2 ] || usage
cmd=$1 DIR=$2; shift 2
command -v openssl >/dev/null || die "openssl not found"
SERVICE=minigw IDENTITY=settings-proxy.internal ORIGIN=https://settings.example.net NAME=
T=$(mktemp -d "${TMPDIR:-/tmp}/admin-certs.XXXXXX")
cleanup() { rm -f "$T"/new/* "$T"/*.* 2>/dev/null || :; rmdir "$T/new" 2>/dev/null || :; rmdir "$T" 2>/dev/null || :; }
trap cleanup EXIT INT TERM

parse() {
  while [ $# -gt 0 ]; do
    case $1 in
      --service) SERVICE=${2:?}; shift 2 ;;
      --identity) IDENTITY=${2:?}; shift 2 ;;
      --origin) ORIGIN=${2:?}; shift 2 ;;
      --browser) NAME=${2:?}; shift 2 ;;
      *) usage ;;
    esac
  done
  dns_ok "$SERVICE" || die "--service must be a lowercase DNS name"
  dns_ok "$IDENTITY" || die "--identity must be a lowercase DNS name"
  [ -z "$NAME" ] || dns_ok "$NAME" || die "the browser name must be a lowercase DNS name (no port)"
}

case $cmd in
link)
  parse "$@"
  refuse_existing "$DIR/ca/link-ca.pem" "$DIR/ca/link-ca.key" "$DIR/gateway/gateway.pem" "$DIR/gateway/gateway.key" \
    "$DIR/gateway/link-ca.pem" "$DIR/proxy/proxy-client.pem" "$DIR/proxy/proxy-client.key" "$DIR/proxy/link-ca.pem"
  mkdir_private "$DIR"; mkdir_private "$DIR/ca"; mkdir_private "$DIR/gateway"; mkdir_private "$DIR/proxy"
  ca link-ca "$DIR/ca" "$CA_DAYS"
  leaf link-ca "$DIR/gateway/gateway" "$SERVICE" serverAuth "$LINK_DAYS"
  leaf link-ca "$DIR/proxy/proxy-client" "$IDENTITY" clientAuth "$LINK_DAYS"
  cp "$DIR/ca/link-ca.pem" "$DIR/gateway/link-ca.pem"; cp "$DIR/ca/link-ca.pem" "$DIR/proxy/link-ca.pem"
  echo "created in $DIR: link CA (10 years), gateway server certificate (SAN $SERVICE) and proxy client certificate (SAN $IDENTITY), 1 year, ECDSA P-256"
  echo "Keep $(q "$DIR/ca/link-ca.key") off the server too once you no longer need to renew (renew needs it back)."
  chown_note both
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
  ca browser-ca "$DIR/ca" "$BROWSER_CA_DAYS" "$NAME"
  browser_leaf "$NAME" "$DIR/proxy/browser"
  echo "created: browser certificate for $NAME (397 days, ECDSA P-256) signed by a private CA (2 years, constrained to $NAME)."
  echo "Install $(q "$DIR/ca/browser-ca.pem") as a trusted root in each browser or computer that opens the page."
  echo "A certificate from your team's CA or ACME is the better choice: put chain and key at $(q "$DIR/proxy/browser.pem") and browser.key."
  ca_key_note
  chown_note proxy
  ;;
renew)
  parse "$@"
  [ -f "$DIR/ca/link-ca.key" ] && [ -f "$DIR/ca/link-ca.pem" ] || die "$DIR/ca/link-ca.key is absent: put the CA key back to renew (or run 'link' into a new folder for a new CA)"
  [ -z "$NAME" ] || { [ -f "$DIR/ca/browser-ca.key" ] && [ -f "$DIR/ca/browser-ca.pem" ] || die "$DIR/ca/browser-ca.key is absent: put it back to renew the browser certificate"; }
  mkdir "$T/new"
  leaf link-ca "$T/new/gateway" "$SERVICE" serverAuth "$LINK_DAYS"
  leaf link-ca "$T/new/proxy-client" "$IDENTITY" clientAuth "$LINK_DAYS"
  [ -z "$NAME" ] || browser_leaf "$NAME" "$T/new/browser"
  mv -f "$T/new/gateway.pem" "$T/new/gateway.key" "$DIR/gateway/"
  mv -f "$T/new/proxy-client.pem" "$T/new/proxy-client.key" "$DIR/proxy/"
  [ -z "$NAME" ] || mv -f "$T/new/browser.pem" "$T/new/browser.key" "$DIR/proxy/"
  echo "renewed (CA kept): gateway and proxy certificates${NAME:+ and the browser certificate for $NAME}. Restart: docker compose restart minigw admin-proxy"
  chown_note both
  ;;
*) usage ;;
esac
