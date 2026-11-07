#!/bin/zsh
# E2E za način worktree na minigw serve (Go) na tem Macu, kot worktree-e2e.sh za aplikacijo: lokalni serve na prostih
# vratih, odjemalec Go z lastnim HOME, šifrirano prek »localhost.«. »claude« prehoda je lažen agent, ki v klonu izvede
# $T/agent.sh (brez žetonov); »claude« odjemalca pošlje zahteve z orodji.
# Preveri: veja z delom agenta (commit in necommitano) in neshranjenimi spremembami uporabnika; datoteke, indeks, HEAD in
# .git/config uporabnika nespremenjeni; druga zahteva iste seje premakne isto vejo naprej; zlonamerni agent (.gitattributes
# s filtrom, .gitmodules file://, nastavitve in hooki v .git klona) ne zažene ničesar; drevo z ».git« odjemalec zavrne;
# --minigw-work=auto izbere worktree za repozitorij git.
#   serve-worktree-e2e.sh
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=${$(mktemp -d):A}; SPID=""
trap '[[ -n $SPID ]] && kill $SPID 2>/dev/null; [[ -n ${KEEP:-} ]] && print -u2 "kept $T" || rm -rf $T' EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }
(cd client && go build -o $T/minigw .)
M=$T/minigw; H=$T/home; MARKS=$T/marks
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
mkdir -p $H/bin $T/gwbin $MARKS
gw() { HOME=$H "$M" "$@" }

# Prehod: lažen agent v klonu (cwd), odgovor v stream-json kot claude -p.
cat > $T/gwbin/claude <<EOF
#!/bin/sh
[ "\$1" = "--version" ] && { echo "2.1.99 (Claude Code)"; exit 0; }
cat > /dev/null
pwd > $T/clone
zsh $T/agent.sh > $T/agent.log 2>&1
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
EOF
chmod +x $T/gwbin/claude
"$M" serve init --config $T/serve/serve.json --port $PORT > $T/init.txt
TOKEN=$(grep -o "\-\-token '[^']*'" $T/init.txt | cut -d"'" -f2)
python3 -c 'import json,sys; p=sys.argv[1]; c=json.load(open(p)); c["scratch"]=sys.argv[2]; json.dump(c,open(p,"w"))' $T/serve/serve.json $T/scratch
"$M" gw claude --path $T/gwbin/claude --tools Read,Edit,Write,Bash --config $T/serve/serve.json >/dev/null
"$M" serve --config $T/serve/serve.json > $T/serve.log 2>&1 &
SPID=$!
for i in {1..50}; do curl -s -o /dev/null http://127.0.0.1:$PORT/health/live && break; sleep 0.2; done
kill -0 $SPID 2>/dev/null || fail "serve did not start: $(<$T/serve.log)"
print -r -- $TOKEN | gw connect localhost.:$PORT --token - >/dev/null || fail "connect"

# Odjemalec: lažen claude pošlje $E2E_N zahtev z orodji v isti seji.
cat > $H/bin/claude <<'EOF'
#!/bin/zsh
sid=$2; hdr=(); for l in ${(f)ANTHROPIC_CUSTOM_HEADERS}; hdr+=(-H $l)
for i in {1..${E2E_N:-1}}; do
  [[ -f $E2E/agent$i.sh ]] && cp $E2E/agent$i.sh $E2E/agent.sh
  code=$(curl -s -o $E2E/resp -w '%{http_code}' $ANTHROPIC_BASE_URL/v1/messages -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" $hdr \
    -H 'content-type: application/json' -d '{"model":"claude-cli-haiku","max_tokens":50,"tools":[{"name":"x","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go '$i'"}]}')
  [[ $code == 200 ]] || { print -u2 "messages $i: HTTP $code $(head -c 400 $E2E/resp)"; exit 1 }
done
EOF
chmod +x $H/bin/claude
C=$H/client; mkdir -p $C/sub
g() { git -C $C -c user.name=t -c user.email=t@x "$@" }
g init -q -b main
print one > $C/a.txt; print s > $C/sub/s.txt; g add -A; g commit -qm first
print -l one uncommitted > $C/a.txt; print untracked > $C/new.txt
g config filter.evil.smudge "touch $MARKS/client-smudge; cat"
for h in reference-transaction post-checkout post-merge; do print -l '#!/bin/sh' "touch $MARKS/hook-$h" > $C/.git/hooks/$h; chmod +x $C/.git/hooks/$h; done
st0=$(g status --porcelain); head=$(g rev-parse HEAD); cfg=$(shasum $C/.git/config); idx=$(shasum $C/.git/index)
same() { [[ $(g status --porcelain) == $st0 && $(g rev-parse HEAD) == $head && $(shasum $C/.git/config) == $cfg && $(shasum $C/.git/index) == $idx ]] }
run() { (cd ${DIR:-$C} && E2E=$T PATH=$H/bin:$PATH MINIGW_WORK=${WORK:-worktree} gw -p go) 2> $T/err || fail "minigw: $(<$T/err) $(tail -5 $T/serve.log)" }
newest() { g for-each-ref --sort=-committerdate --format='%(refname:short)' 'refs/heads/minigw/claude-*' | head -1 }

# a + b: commit in necommitano delo agenta; druga zahteva iste seje nadaljuje na isti veji
print 'print agent > agent.txt; git add agent.txt; git -c user.name=a -c user.email=a@x commit -qm "agent work"; print left > left.txt' > $T/agent1.sh
print 'print more > more.txt' > $T/agent2.sh
DIR=$C/sub E2E_N=2 run
grep -q "(worktree)" $T/err || fail "a: not worktree mode: $(<$T/err)"
br=$(newest); [[ -n $br ]] || fail "a: no branch: $(<$T/err)"
[[ $(g show ${br}:sub/agent.txt) == agent && $(g show ${br}:sub/left.txt) == left && $(g show ${br}:sub/more.txt) == more \
   && $(g show ${br}:new.txt) == untracked && $(g show ${br}:a.txt) == $'one\nuncommitted' ]] || fail "a/b: branch content ($br)"
[[ $(g for-each-ref 'refs/heads/minigw/' | wc -l | tr -d ' ') == 1 ]] || fail "b: second request made another branch"
[[ ! -e $C/sub/agent.txt ]] || fail "a: agent file in the user's folder"
same || fail "a: the user's files, index, HEAD or .git/config changed"
print "a b ok ($br)"

# c: zlonamerni agent ne zažene ničesar (odjemalec ali prehod)
cat > $T/agent1.sh <<EOF
print "* filter=evil" > .gitattributes; printf '[submodule "x"]\n\tpath = x\n\turl = file://$MARKS/sub\n' > .gitmodules
git add -A; git -c user.name=a -c user.email=a@x commit -qm evil
printf '[core]\n\tfsmonitor = touch $MARKS/gw-fsmonitor\n[filter "evil"]\n\tclean = touch $MARKS/gw-clean; cat\n' >> "\$GIT_DIR/config"
mkdir -p "\$GIT_DIR/hooks"; print -l '#!/bin/sh' 'touch $MARKS/gw-hook' > "\$GIT_DIR/hooks/post-commit"; chmod +x "\$GIT_DIR/hooks/post-commit"
print x > left2.txt
EOF
sed -i '' 's/^print /echo /' $T/agent1.sh
E2E_N=1 run
[[ -z $(ls $MARKS) ]] || fail "c: something ran: $(ls $MARKS)"
same || fail "c: the user's repository changed"
brc=$(newest); [[ $(g show ${brc}:.gitattributes) == "* filter=evil" ]] || fail "c: evil branch not fetched as content ($brc)"
print "c ok"

# d: drevo z ».git« (agent ga sestavi z mktree): odjemalec veje ne posodobi
cat > $T/agent1.sh <<'EOF'
b=$(git hash-object -w --stdin </dev/null); e=$(printf '100644 blob %s\thooks-post-checkout\n' $b | git mktree)
t=$(printf '040000 tree %s\t.git\n' $e | git mktree); c=$(git -c user.name=a -c user.email=a@x commit-tree $t -p HEAD -m dotgit)
git update-ref HEAD $c
EOF
E2E_N=1 run
[[ -z $(ls $MARKS) ]] || fail "d: something ran: $(ls $MARKS)"
for b in $(g for-each-ref --format='%(refname:short)' 'refs/heads/minigw/'); do
  g ls-tree -r --name-only $b | grep -q '^\.git/' && fail "d: a branch has .git/ ($b)"
done
same || fail "d: the user's repository changed"
print "d ok"

# e: auto izbere worktree za repozitorij git
print 'print auto > auto.txt' > $T/agent1.sh
WORK=auto E2E_N=1 run
grep -q "(worktree)" $T/err || fail "e: auto did not pick worktree: $(<$T/err)"
print "e ok"
print "serve worktree e2e ok"
