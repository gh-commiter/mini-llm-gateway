#!/bin/zsh
# End-to-end test of the Compose "admin" profile (deploy/docker, docs/docker.md "Remote settings from a browser") on this
# Mac: certificates from admin-certs.sh, a password through exec, the profile up, then curl --resolve against the external
# name. Called from compose-e2e.sh; also runs alone.
# Rules: everything is a container; the proxy's HTTPS port is published on 127.0.0.1:<random high port> only (never a
# non-loopback listener from this script); no --privileged, no chown/chmod -R, no rm; files stay in the Scratch directory.
emulate -L zsh; set -u
ROOT=${0:A:h:h}
D=${SCRATCH_DIR:-"/Volumes/SAMSUNG990PRO/Mini Gateway Scratch/dev-builds/compose2-0725"}
RUN=$(date +%s)
proj=minigw-admin-e2e-$RUN        # a fresh project per run (fresh volumes); its volumes are left behind, named by this prefix
NAME=settings.minigw.test
PORT=$(( 20000 + RANDOM % 20000 ))
GWPORT=$(( 40000 + RANDOM % 10000 ))
W=$D/admin-e2e/$RUN; CERTS=$W/certs
PW='correct horse battery staple'
FAILED=0
mkdir -p $W $D/gocache $D/bin
setup_fail() { print -u2 "SETUP FAIL $*"; exit 1 }
pass() { print "PASS $*" }
bad() { print "FAIL $*"; FAILED=1 }
check() { # description, then the command that must succeed
  local d=$1; shift
  if "$@"; then pass $d; else bad $d; fi
}
cd $ROOT/deploy/docker || setup_fail cd
export COMPOSE_PROJECT_NAME=$proj MINIGW_VERSION=local MINIGW_LOCAL_DIR=$D/bin MINIGW_PORT=$GWPORT MINIGW_BIND=127.0.0.1 \
  MINIGW_ADMIN_BIND=127.0.0.1 MINIGW_ADMIN_PORT=$PORT MINIGW_ADMIN_NAME=$NAME MINIGW_ADMIN_CERTS=$CERTS
DC=(docker compose -f compose.yaml -f compose.local.yaml -f compose.admin.yaml)
PY=python:3-alpine@sha256:0f9552bb28711333da16915ec146f5e6bb44a7fdac9ced15dc17c2e0206346dd   # pinned: this one gets the config volume
cleanup() { (cd $ROOT/deploy/docker && $DC down >/dev/null 2>&1) }   # containers and networks of this project; volumes stay
trap cleanup EXIT

healthy() {
  local i st
  for i in {1..40}; do
    st=$(docker inspect --format '{{.State.Health.Status}}' $proj-minigw-1 2>/dev/null)
    [[ $st == healthy ]] && return 0
    sleep 3
  done
  return 1
}

setcfg() { # allowedSourceCIDRs as JSON; rewrites remoteSettings in serve.json (a helper container as the gateway's uid), then restarts
  docker run --rm --user 10001:10001 -v ${proj}_config:/c $PY python -c "
import json
p='/c/serve.json'
c=json.load(open(p))
c['remoteSettings']={'enabled':True,'listen':'0.0.0.0:8766','origin':'https://$NAME:$PORT','allowedSourceCIDRs':$1,
 'proxyIdentity':'settings-proxy.internal','proxyCA':'/admin-tls/link-ca.pem','certificate':'/admin-tls/gateway.pem','privateKey':'/admin-tls/gateway.key'}
json.dump(c,open(p,'w'),indent=1)" || return 1
  $DC restart minigw >/dev/null 2>&1 && healthy
}

# --- build, certificates, start ---
arch=$(uname -m); [[ $arch == x86_64 ]] && arch=amd64
(cd $ROOT/client && GOCACHE=$D/gocache GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o $D/bin/minigw-linux-$arch .) || setup_fail "go build"
sh admin-certs.sh link $CERTS --identity settings-proxy.internal --origin https://$NAME:$PORT >$W/link.out 2>&1 || { cat $W/link.out >&2; setup_fail "admin-certs.sh link"; }
sh admin-certs.sh link $W/otherca >/dev/null 2>&1 || setup_fail "second link set"   # another CA, for the wrong-CA probe
sh admin-certs.sh browser $CERTS $NAME >$W/browser.out 2>&1 || { cat $W/browser.out >&2; setup_fail "admin-certs.sh browser"; }
sh admin-certs.sh link $CERTS >/dev/null 2>&1 && bad "admin-certs.sh replaced existing files" || pass "admin-certs.sh refuses to overwrite"
grep -q '"proxyIdentity": "settings-proxy.internal"' $W/link.out && ! grep -qi 'PRIVATE KEY' $W/link.out $W/browser.out && pass "admin-certs.sh prints the block, no secrets" || bad "admin-certs.sh output"
docker compose -f compose.yaml -f compose.local.yaml config 2>/dev/null | grep -qE 'admin-tls|admin-proxy' && bad "plain compose files mention the admin profile" || pass "without compose.admin.yaml the gateway service has no admin mount and no proxy"
$DC up -d --build minigw >/dev/null 2>&1 || setup_fail "up minigw"
healthy || setup_fail "gateway not healthy"
print -r -- $PW | $DC exec -T minigw minigw gw settings password >/dev/null 2>&1 || setup_fail "gw settings password"
setcfg '["0.0.0.0/0"]' || setup_fail "remoteSettings config / restart"
$DC up -d >/dev/null 2>&1 || setup_fail "up with compose.admin.yaml"
CA=$CERTS/ca/browser-ca.pem
URL=https://$NAME:$PORT
H=(-H "Origin: $URL" -H 'Content-Type: application/json' -H 'X-Minigw-Remote-Request: 1')
req() { curl -sS --max-time 15 --cacert $CA --resolve $NAME:$PORT:127.0.0.1 -o $W/body -D $W/head -w '%{http_code}' "$@" 2>$W/err }
for i in {1..30}; do [[ $(req $URL/minigw/settings) == 200 ]] && break; sleep 2; done
audit() { $DC exec -T minigw cat /var/lib/minigw/config/remote-settings.audit.jsonl 2>/dev/null }

# one user, team and project for the read-only list checks below (the CLI changes them; the remote page only reads)
$DC exec -T minigw minigw gw user add e2euser >/dev/null 2>&1 && $DC exec -T minigw minigw gw team add e2eteam >/dev/null 2>&1 && $DC exec -T minigw minigw gw project add e2eproj "E2E Project" >/dev/null 2>&1 || setup_fail "gw user/team/project add"
BINDDEV=$(printf 'A%.0s' {1..43})   # a well-formed device id (32 zero bytes); the binding is what is listed
$DC exec -T minigw minigw gw bind $BINDDEV e2euser >/dev/null 2>&1 || setup_fail "gw bind"
GWTOKEN=$(docker run --rm --user 10001:10001 -v ${proj}_config:/c:ro $PY python -c "import json;print(json.load(open('/c/serve.json'))['token'])" 2>/dev/null)
[[ -n $GWTOKEN ]] || setup_fail "gateway token"

# --- cases ---
c=$(req $URL/minigw/settings)
[[ $c == 200 ]] && grep -qi password $W/body && pass "login page loads over HTTPS" || bad "login page: $c $(cat $W/err)"
grep -qi '^strict-transport-security: max-age=' $W/head && pass "HSTS header" || bad "HSTS header missing"
ca_text=$(openssl x509 -in $CA -noout -text)
if print -r -- $ca_text | grep -q 'Name Constraints: critical' && print -r -- $ca_text | grep -q "DNS:$NAME" && ! openssl x509 -in $CA -noout -checkend $((731*86400)) >/dev/null; then
  pass "browser CA is name-constrained (critical) to $NAME and lives at most 2 years"
else bad "browser CA constraint or lifetime"; fi
c=$(req -X POST $H -d '{"password":"wrong password here"}' $URL/minigw/settings/login)
[[ $c == 401 ]] && pass "wrong password refused ($c)" || bad "wrong password answered $c"
c=$(req -X POST $H -d "{\"password\":\"$PW\"}" $URL/minigw/settings/login)
S=$(sed -n 's/.*"session": *"\([0-9a-f]*\)".*/\1/p' $W/body)
[[ $c == 200 && -n $S ]] && pass "right password gives a session" || bad "login answered $c"
c=$(req -H "X-Minigw-Remote-Session: $S" $URL/minigw/admin/v1/providers)
[[ $c == 200 ]] && pass "allowed read route with the session (providers)" || bad "providers answered $c"
c=$(req $URL/minigw/admin/v1/providers)
[[ $c == 401 ]] && pass "same route without a session -> 401" || bad "no-session providers answered $c"
# read-only lists: users, bindings, teams, projects
SH=(-H "X-Minigw-Remote-Session: $S")
for pair in users:e2euser bindings:$BINDDEV teams:e2eteam projects:e2eproj; do
  p=${pair%%:*}; want=${pair#*:}
  c=$(req $SH $URL/minigw/admin/v1/$p)
  [[ $c == 200 ]] && grep -qF -- "$want" $W/body && pass "logged in: GET $p -> 200 with the data" || bad "GET $p answered $c"
  c=$(req $URL/minigw/admin/v1/$p)
  [[ $c == 401 ]] && pass "GET $p without a session -> 401" || bad "GET $p without a session answered $c"
  for m in POST PUT DELETE; do
    c=$(req -X $m $H $SH -d '{}' $URL/minigw/admin/v1/$p)
    [[ $c == 404 ]] && pass "$m $p -> 404 at the proxy" || bad "$m $p answered $c"
  done
  c=$(req -I $SH $URL/minigw/admin/v1/$p)
  [[ $c == 404 ]] && pass "HEAD $p -> 404 at the proxy" || bad "HEAD $p answered $c"
done
for p in projects/1/members projects/1/grants grants; do
  c=$(req $SH $URL/minigw/admin/v1/$p)
  [[ $c == 404 ]] && pass "GET $p -> 404 at the proxy" || bad "GET $p answered $c"
done
for p in /minigw/admin/v1/status /minigw/admin/v1/core/projects /minigw/settings/link /v1/models /minigw/settings/; do
  c=$(req -H "X-Minigw-Remote-Session: $S" $URL$p)
  [[ $c == 404 ]] && grep -q 'Not found' $W/body && pass "outside the remote scope: $p -> 404 at the proxy" || bad "$p answered $c"
done
c=$(req -X DELETE -H "X-Minigw-Remote-Session: $S" $URL/minigw/settings)
[[ $c == 404 ]] && pass "wrong method on the page path -> 404" || bad "DELETE page answered $c"
for p in /v1/models/../../minigw/settings /minigw/settings/./ /minigw//settings /minigw/admin/v1/providers%2Fx /minigw%2Fsettings '/minigw/admin/v1/..%2fstatus'; do
  c=$(req --path-as-is -H "X-Minigw-Remote-Session: $S" "$URL$p")
  [[ $c == 404 ]] && grep -q 'Not found' $W/body && pass "unclean path $p -> 404 at the proxy" || bad "unclean path $p answered $c"
done
# spoofed source: the audit must show the real peer, never the spoofed address
real=$(audit | sed -n 's/.*"IP":"\([^"]*\)".*"Action":"login","Result":"ok".*/\1/p' | tail -1)
c=$(req -X POST $H -H 'X-Minigw-Client-IP: 198.51.100.7' -H 'X-Forwarded-For: 198.51.100.8' -H 'Forwarded: for=198.51.100.9' -H 'X-Real-IP: 198.51.100.10' \
    -d '{"password":"wrong password again"}' $URL/minigw/settings/login)
last=$(audit | grep '"Action":"login","Result":"denied"' | tail -1 | sed -n 's/.*"IP":"\([^"]*\)".*/\1/p')
[[ $c == 401 && -n $real && $last == $real ]] && ! audit | grep -q '198\.51\.100\.' && pass "spoofed X-Minigw-Client-IP / X-Forwarded-For ignored (audit shows $real, not the spoofed address)" \
  || bad "spoof: answer $c, real '$real', audited '$last'"
# plain HTTP to the proxy's port: no content
c=$(curl -sS --max-time 10 -o $W/body -w '%{http_code}' http://127.0.0.1:$PORT/minigw/settings 2>/dev/null)
! grep -qi password $W/body && [[ $c != 200 ]] && pass "plain HTTP gives no content ($c)" || bad "plain HTTP answered $c"
# 8766 never published; one published port, on loopback
pub=$(docker port $proj-minigw-1 | grep -c '^8766/'); ppub=$(docker port $proj-admin-proxy-1)
[[ $pub == 0 ]] && pass "gateway port 8766 is not published" || bad "8766 published"
[[ $(print -r -- $ppub | wc -l | tr -d ' ') == 1 && $ppub == "8443/tcp -> 127.0.0.1:$PORT" ]] && pass "proxy publishes only HTTPS on 127.0.0.1:$PORT" || bad "proxy ports: $ppub"
c=$(curl -sk --max-time 5 -o /dev/null -w '%{http_code}' https://127.0.0.1:8766/ 2>/dev/null)
[[ $c == 000 ]] && pass "direct connection to 8766 from the host impossible" || bad "host reached 8766: $c"
# another container on the default network: TCP reachable, mutual TLS refuses it
tlsprobe() { # network, certificate dir, certificate name or '' -> prints REFUSED or ANSWERED
  docker run --rm --network ${proj}_$1 -v $2:/c:ro $PY python -c "
import socket,ssl,sys
x=ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT);x.check_hostname=False;x.verify_mode=ssl.CERT_NONE;x.minimum_version=ssl.TLSVersion.TLSv1_3
if '$3': x.load_cert_chain('/c/$3.pem','/c/$3.key')
try:
    s=x.wrap_socket(socket.create_connection(('minigw',8766),5),server_hostname='minigw');s.sendall(b'GET /minigw/settings HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n');d=s.recv(100)
    print('ANSWERED' if d.startswith(b'HTTP/') else 'REFUSED')
except Exception: print('REFUSED')" 2>/dev/null
}
[[ $(tlsprobe default $CERTS/proxy '') == REFUSED ]] && pass "other container (default network), no client certificate: handshake refused" || bad "no-certificate client got an answer"
[[ $(tlsprobe default $W/otherca/proxy proxy-client) == REFUSED ]] && pass "other container, client certificate with the right name and EKU but from another CA: refused" || bad "wrong-CA client got an answer"
[[ $(tlsprobe admin $CERTS/proxy proxy-client) == ANSWERED ]] && pass "control: the proxy's own certificate is answered (the probe can see answers)" || bad "control probe got no answer"
# upstream error with a session header: the proxy log must not contain the session (Caddy logs request headers on errors)
$DC stop minigw >/dev/null 2>&1
c=$(req -H "X-Minigw-Remote-Session: $S" $URL/minigw/admin/v1/providers)
[[ $c == 502 || $c == 503 ]] && pass "gateway down: the proxy answers $c" || bad "gateway down answered $c"
sleep 1
plog=$($DC logs admin-proxy 2>&1)
print -r -- $plog | grep -q 'http.log.error' && pass "the proxy logged the upstream error" || bad "no upstream error in the proxy log (the next check would prove nothing)"
[[ $plog == *"$S"* ]] && bad "the session token is in the proxy log" || pass "no session token in the proxy log after an upstream error"
$DC start minigw >/dev/null 2>&1; healthy || setup_fail "gateway did not come back"
# where did the gateway see the browser? (the documented check; informational)
seen=$(audit | grep '"IP":"[0-9a-f]' | tail -1 | sed -n 's/.*"IP":"\([^"]*\)".*/\1/p')
print "INFO the gateway audited the source $seen for the last login (a 172.x/192.168.65.x address here is the Docker gateway: Docker Desktop and rootless Docker hide the browser address, so allowedSourceCIDRs cannot tell browsers apart there)"
# renewal with the existing CAs: new leaf certificates, restart, the page loads; refused without the CA key
sh admin-certs.sh renew $CERTS --browser $NAME >/dev/null 2>&1 && pass "admin-certs.sh renew replaced the leaf certificates (CAs kept)" || bad "renew"
$DC restart minigw admin-proxy >/dev/null 2>&1; healthy
for i in {1..20}; do [[ $(req $URL/minigw/settings) == 200 ]] && break; sleep 2; done
[[ $(req $URL/minigw/settings) == 200 ]] && pass "after renewal the page loads over the same CA" || bad "page after renewal"
sh admin-certs.sh link $W/norenew >/dev/null 2>&1; mv $W/norenew/ca/link-ca.key $W/link-ca.key.away
sh admin-certs.sh renew $W/norenew >/dev/null 2>&1 && bad "renew without the CA key succeeded" || pass "renew refuses when the CA key is absent"
# no password or session value anywhere in the proxy log after the whole run
plog=$($DC logs admin-proxy 2>&1)
leak=0; for v in $S $PW $GWTOKEN "wrong password here" "wrong password again"; do [[ $plog == *"$v"* ]] && leak=1; done
(( leak )) && bad "a password, session or token value is in the proxy log" || pass "no password, session or gateway token value in the proxy log after the run"
glog=$($DC logs minigw 2>&1)
leak=0; for v in $S $PW $GWTOKEN "wrong password here" "wrong password again"; do [[ $glog == *"$v"* ]] && leak=1; done
(( leak )) && bad "a password, session or token value is in the gateway log" || pass "no password, session or gateway token value in the gateway log after the run"
# client IP outside allowedSourceCIDRs
setcfg '["203.0.113.0/24"]' || setup_fail "second config / restart"
for i in {1..15}; do c=$(req $URL/minigw/settings); [[ $c == 403 ]] && break; sleep 2; done
[[ $c == 403 ]] && pass "client IP outside allowedSourceCIDRs refused (403)" || bad "outside-CIDR answered $c"
(( FAILED )) && { print "FAILED: some admin profile cases failed (files in $W)"; exit 1 }
print "DONE: admin profile cases passed"
