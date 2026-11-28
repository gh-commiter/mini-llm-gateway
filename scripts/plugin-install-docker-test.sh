#!/bin/zsh
# Preizkus plugins/minigw/scripts/install-minigw.sh (podpis SHA256SUMS, #8; H1/L4 0.5.17) v Linuxu (Docker), z lažno
# izdajo prek file:// in testnim ključem (pravi ključ izdaj se tu nikoli ne uporabi).
#   1. nameščen minigw 0.5.16 (brez verify-sums) + claude, ki vrne 0, brez ssh-keygen, smetni podpis → stari ostane
#   2. prva namestitev brez ssh-keygen → zavrnjena
#   3. ssh-keygen in podpis testnega ključa → nameščen
#   4. napačen imenski prostor, obstoječi ostane → zavrnjen, z razlogom (L4)
#   5. #25: podpisana starejša izdaja (VERSION 0.5.16) čez novejši nameščeni → zavrnjena, z razlogom
#   6. ista z MINIGW_ALLOW_DOWNGRADE=1 → nameščena
#   7. N1: podpisana izdaja brez VERSION (= 0.5.17) čez nameščeni 0.5.19 → zavrnjena
#   9. Linux: nameščen ~/.local/bin/minigw, ki ni starejši od MIN → povezava (brez prenosa, tudi ob novejši izdaji);
#      starejši od MIN → prenos
#  10. Windows (lažna uname/cygpath, na tem Macu): nameščen minigw.exe → lupini bin/minigw in bin/minigw.cmd kot
#      »minigw plugin install« (client/plugin.go shimSh/shimCmd), kopija vtičnika gre; brez namestitve lupini gresta;
#      M1: novejša podpisana izdaja → brez serve.json (MSI le z odjemalcem) namig na MSI, s serve.json /minigw:gw update
#  11. M1 Linux: nameščen minigw 0.5.19 (≥ MIN), podpisana izdaja 0.99.0 → povezava; brez enote systemd namig za
#      odjemalca (install.sh ali zamenjaj ~/.local/bin/minigw), z enoto /minigw:gw update
#  12. install.sh v Linuxu (kot uporabnik, ne root): privzeto odjemalec + vtičnik v ~/.local/bin, --client, --gateway
#      (serve init + serve install z lažnim systemctl), --uninstall [--purge], napačen podpis → nič nameščeno
#   8. izhod »minigw status« → connected / token rejected / device not approved (ID) / not connected; brez claude: namig (#6)
emulate -L zsh; set -eu
cd ${0:A:h}/..
T=$(mktemp -d); trap 'rm -rf $T' EXIT
fail() { print -u2 "FAIL: $*"; exit 1 }

# 0.5.16 (zadnji commit pred verify-sums) in trenutni minigw za Linux v vsebniku
mkdir -p $T/old $T/rel/good $T/rel/bad $T/rel/evil $T/rel/older $T/rel/newer $T/home/.config/minigw $T/home/bin
git archive 4994f84 client | tar -x -C $T/old
(cd $T/old/client && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $T/minigw-old .)
(cd client && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $T/rel/good/minigw-linux-arm64 .)
print '{"url":"http://127.0.0.1:9","token":"t"}' > $T/home/.config/minigw/connection.json
print '#!/bin/sh\nexit 0' > $T/home/bin/claude; chmod +x $T/home/bin/claude

ssh-keygen -q -t ed25519 -N "" -C test -f $T/key
(cd $T/rel/good && shasum -a 256 minigw-linux-arm64 > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
cp $T/rel/good/minigw-linux-arm64 $T/rel/good/SHA256SUMS $T/rel/bad/
cp $T/minigw-old $T/rel/older/minigw-linux-arm64
(cd $T/rel/older && { print "VERSION 0.5.16"; shasum -a 256 minigw-linux-arm64; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
ssh-keygen -Y sign -q -f $T/key -n other < $T/rel/bad/SHA256SUMS > $T/rel/bad/SHA256SUMS.sig
cp $T/rel/good/minigw-linux-arm64 $T/rel/newer/
(cd $T/rel/newer && { print "VERSION 0.99.0"; shasum -a 256 minigw-linux-arm64; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
print '#!/bin/sh\necho EVIL' > $T/rel/evil/minigw-linux-arm64
(cd $T/rel/evil && shasum -a 256 minigw-linux-arm64 > SHA256SUMS && print garbage > SHA256SUMS.sig)
# 12: izdaja za install.sh (minigw + lažni minigw-tui, VERSION), podpisana s testnim ključem; ena z napačnim imenskim prostorom
V=$(grep -o 'const version = "[^"]*"' client/main.go | cut -d'"' -f2)
mkdir -p $T/rel/inst $T/rel/instbad $T/fakebin
cp $T/rel/good/minigw-linux-arm64 $T/rel/inst/; print '#!/bin/sh\necho tui' > $T/rel/inst/minigw-tui-linux-arm64
(cd $T/rel/inst && { print "VERSION $V"; shasum -a 256 minigw-linux-arm64 minigw-tui-linux-arm64; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig)
cp $T/rel/inst/* $T/rel/instbad/; ssh-keygen -Y sign -q -f $T/key -n other < $T/rel/instbad/SHA256SUMS > $T/rel/instbad/SHA256SUMS.sig
sed "s|ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINt++OHn+HHmR9mmA2UI+B0osuilO2GP0ddEzNp0GTrH|$(cut -d' ' -f1,2 $T/key.pub)|" install.sh > $T/install-test.sh
grep -q "$(cut -d' ' -f2 $T/key.pub)" $T/install-test.sh || fail "12: test key not pinned in the install.sh copy"
print '#!/bin/sh\n[ "$1" = --version ] && echo 2.1.300; exit 0' > $T/fakebin/claude
print '#!/bin/sh\nexit 0' > $T/fakebin/systemctl; cp $T/fakebin/systemctl $T/fakebin/loginctl; chmod +x $T/fakebin/*

# Kot v pravem vtičniku: MIN_CLIENT leži poleg skripte (brez njega bi skripta uporabila rezervno vrednost in to povedala).
MINV=$(<plugins/minigw/scripts/MIN_CLIENT); print -r -- $MINV > $T/MIN_CLIENT; print -r -- $MINV > $T/minv
[[ $V == $MINV ]] || fail "9: the client built from the source ($V) is not MIN_CLIENT ($MINV): the \"installed current\" client would be too old"
cp ${INSTALL_SCRIPT:-plugins/minigw/scripts/install-minigw.sh} $T/real.sh;  # INSTALL_SCRIPT: preizkus starejše skripte (da se vidi, da test pade)
 cp plugins/minigw/scripts/kb-hook.sh $T/kb-hook.sh
sed "s|^SIGNER=.*|SIGNER='minigw-release namespaces=\"minigw-release\" $(cut -d' ' -f1,2 $T/key.pub)'|" $T/real.sh > $T/testkey.sh

# 10: Windows brez Windows (nova zasnova): skripta NIKOLI ne ustvari, prepiše ali preimenuje bin/minigw in bin/minigw.cmd, nobene ključavnice.
# Lažna uname (MINGW64) in cygpath (-u: ista pot, -m/-w: C: spredaj); pot z ' in %. mv/rm ovojnici zabeležita vsak klic na minigw, minigw.exe, .keep.
W="$T/win/O'Brien 100%"; mkdir -p $T/win/fake "$W/Programs/Mini Gateway" $T/win/data/bin $T/win/home
GB=$T/win/data/bin
print '#!/bin/sh\ncase $1 in -s) echo MINGW64_NT-10.0 ;; *) echo x86_64 ;; esac' > $T/win/fake/uname
print '#!/bin/sh\ncase $1 in -u) p=$2; printf "%s\\n" "${p#C:}" ;; -m) printf "C:%s\\n" "$2" ;; -w) printf "C:%s\\n" "$2" | tr / "\\\\" ;; esac' > $T/win/fake/cygpath
exe="$W/Programs/Mini Gateway/minigw.exe" sq="'" rep="'\\''"
print "#!/bin/sh\n[ \"\$1\" = --version ] && echo minigw $MINV; [ \"\$1\" = status ] && echo \"Status:    connected\"; exit 0" > "$exe"
# OPS: vsak mv/rm na minigw, minigw.exe, .keep (v ustaljenem stanju prazno); VIOL: klic na GOLEM imenu »minigw«, ko ta ne obstaja, a minigw.exe obstaja
for c in mv rm; do print '#!/bin/sh\nif [ -n "${FAILMV:-}" ] && [ '$c' = mv ]; then for last; do :; done; case $last in */bin/minigw.exe) echo busy >\&2; exit 1 ;; esac; fi\nfor a; do case $a in */bin/minigw|*/bin/minigw.exe|*/bin/minigw.exe.keep) echo "'$c' $*" >> "$OPS" ;; esac\ncase $a in */bin/minigw) [ ! -e "$a" ] && [ -e "$a.exe" ] && echo "'$c' $*" >> "$VIOL" ;; esac; done\nexec /bin/'$c' "$@"' > $T/win/fake/$c; done
REALSHA=$(command -v shasum)   # ovojnica: branje minigw.exe v bin ne uspe (odprt izključno), ko je FAILREAD nastavljen; sicer pravi shasum
print '#!/bin/sh\nif [ -n "${FAILREAD:-}" ]; then for a; do case $a in */bin/minigw.exe) echo "sha256sum: $a: Device or resource busy" >\&2; exit 1 ;; esac; done; fi\nexec '$REALSHA' "$@"' > $T/win/fake/shasum
REALSUM=$(command -v sha256sum || true); [[ -n $REALSUM ]] && print '#!/bin/sh\nif [ -n "${FAILREAD:-}" ]; then for a; do case $a in */bin/minigw.exe) echo "sha256sum: $a: Device or resource busy" >\&2; exit 1 ;; esac; done; fi\nexec '$REALSUM' "$@"' > $T/win/fake/sha256sum
chmod +x $T/win/fake/* "$exe"
# izdaja z lažnim minigw-windows-amd64.exe (različica, oznaka vsebine), podpisana s testnim ključem
mkrel() { mkdir -p $T/rel/$1; print "#!/bin/sh\n[ \"\$1\" = --version ] && echo minigw $2; [ \"\$1\" = status ] && echo \"Status:    connected\"; exit 0 # $1" > $T/rel/$1/minigw-windows-amd64.exe; chmod +x $T/rel/$1/minigw-windows-amd64.exe
  (cd $T/rel/$1 && { print "VERSION $2"; print "$(shasum -a 256 minigw-windows-amd64.exe | cut -d' ' -f1)  minigw-windows-amd64.exe"; } > SHA256SUMS && ssh-keygen -Y sign -q -f $T/key -n minigw-release < SHA256SUMS > SHA256SUMS.sig) }
mkrel wdl $MINV; mkrel wnew 0.99.0; mkrel wold 0.5.20
RELEXE=$T/rel/wdl/minigw-windows-amd64.exe
# shrani podpisane vsote ob kopijo (kot po prenosu): wtrust <datoteka> <različica>
wtrust() { { print "VERSION $2"; print "$(shasum -a 256 "$1" | cut -d' ' -f1)  minigw-windows-amd64.exe"; } > $GB/SHA256SUMS; ssh-keygen -Y sign -q -f $T/key -n minigw-release < $GB/SHA256SUMS > $GB/SHA256SUMS.sig }
winrun() { FAILMV=${FAILMV:-} FAILREAD=${FAILREAD:-} OPS=$T/win/ops VIOL=$T/win/viol CLAUDE_PLUGIN_DATA=$T/win/data CLAUDE_ENV_FILE=${WENV:-} LOCALAPPDATA=$W APPDATA=$T/win/roaming MINIGW_RELEASE_BASE=file://$T/rel/${1:-nonexistent} HOME=$T/win/home PATH=$T/win/fake:$PATH sh $T/testkey.sh }
clear10() { rm -f $GB/* $GB/.[a-z]* $T/win/ops $T/win/viol 2>/dev/null || true; rmdir $GB/.aside.lock 2>/dev/null || true }
golauncher() { printf '%s\n' '#!/bin/sh' '# Mini Gateway: the installed minigw' "exec 'C:${1//$sq/$rep}' \"\$@\"" > $GB/minigw; printf '@setlocal DisableDelayedExpansion\r\n@"%s" %%*\r\n' "$1" > $GB/minigw.cmd }
# nobenega ukaza na golem imenu bin/minigw (OPS/VIOL), razen kjer test to izrecno dovoli
nobare() { [[ ! -e $T/win/viol ]] || fail "$1: a call on the bare name with minigw.exe present: $(cat $T/win/viol)"
  if [[ -e $T/win/ops ]]; then local l; while read -r l; do for w in ${(z)l}; do [[ $w == */bin/minigw ]] && fail "$1: the script touched the bare name: $l"; done; done < $T/win/ops; fi }

# S1: ni odjemalca: lastna kopija se prenese, .verified imenuje bin/minigw.exe, na golem imenu nič
clear10; out=$(winrun wdl); print -r -- $out
cmp -s $RELEXE $GB/minigw.exe && [[ $out == *"connected to Mini Gateway"* && ! -e $GB/minigw && ! -e $GB/minigw.cmd ]] || fail "10 S1: own exe not installed, or a launcher was written"
[[ "$(head -1 $GB/.verified)" == "$(shasum -a 256 $GB/minigw.exe | cut -d' ' -f1)  $GB/minigw.exe" && "$(wc -l < $GB/.verified)" -eq 1 ]] || fail "10 S1: .verified: $(<$GB/.verified)"
nobare "10 S1"
# ustaljeno stanje: dva zagona, nobenega mv/rm
rm -f $T/win/ops; winrun wdl > /dev/null; winrun > /dev/null
[[ ! -e $T/win/ops ]] || fail "10 S1: steady state touched minigw/minigw.exe: $(cat $T/win/ops)"
# S2: nameščen minigw + zagonska datoteka Go odjemalca, ki velja: uporabi se kot je, zapis ima zagonsko datoteko in nameščeni exe, nič se ne piše
clear10; wtrust "$exe" $MINV; golauncher "$exe"; cp $GB/minigw $T/win/launcher.keep; cp $GB/minigw.cmd $T/win/launcher.cmd.keep
out=$(WENV=$T/win/env2 winrun); print -r -- $out
cmp -s $T/win/launcher.keep $GB/minigw && cmp -s $T/win/launcher.cmd.keep $GB/minigw.cmd && [[ ! -e $GB/minigw.exe && $out == *"connected to Mini Gateway"* ]] || fail "10 S2: the launcher was touched or an own exe appeared"
[[ "$(<$GB/.verified)" == "$(shasum -a 256 $GB/minigw | cut -d' ' -f1)  $GB/minigw"$'\n'"$(shasum -a 256 "$exe" | cut -d' ' -f1)  $exe" ]] || fail "10 S2: .verified: $(<$GB/.verified)"
grep -q '^export PATH=' $T/win/env2 || fail "10 S2: no PATH line"
nobare "10 S2"
# S3: nameščen minigw BREZ zagonske datoteke: lastna kopija se prenese, nič na golem imenu
clear10; out=$(winrun wdl); print -r -- $out
cmp -s $RELEXE $GB/minigw.exe && [[ ! -e $GB/minigw && ! -e $GB/minigw.cmd ]] || fail "10 S3"
nobare "10 S3"
# S4: zagonska datoteka, ki ne velja: odstranjena (edini dovoljen rm na golem imenu), lastna kopija velja
# (i) kaže na neoverjen exe (njegov hash ni v vsotah)
clear10; golauncher "$exe"; out=$(winrun wdl); print -r -- $out
cmp -s $RELEXE $GB/minigw.exe && [[ ! -e $GB/minigw && ! -e $GB/minigw.cmd ]] || fail "10 S4 i: an unverified launcher target was kept or own exe missing"
# (ii) kaže v nič
clear10; golauncher "$W/Programs/Gone/minigw.exe"; out=$(winrun wdl)
cmp -s $RELEXE $GB/minigw.exe && [[ ! -e $GB/minigw && ! -e $GB/minigw.cmd ]] || fail "10 S4 ii: a launcher to nowhere was kept"
# (iii) overjen, a starejši od MIN_CLIENT: lastna kopija, stavek pove, katera
clear10; wtrust "$exe" 0.5.20; golauncher "$exe"; out=$(winrun wdl); print -r -- $out
cmp -s $RELEXE $GB/minigw.exe && [[ ! -e $GB/minigw && $out == *"the installed minigw 0.5.20 is older than $MINV: the plugin uses its own copy $MINV (update the installed one: "* ]] || fail "10 S4 iii: old installed client: $out"
# (iv) tuje besedilo brez znamenja: datoteke se ne dotakne (ni njena, ni zagonska datoteka Go odjemalca)
clear10; print '#!/bin/sh\necho foreign' > $GB/minigw; out=$(winrun wdl) 
[[ "$(<$GB/minigw)" == $'#!/bin/sh\necho foreign' ]] || fail "10 S4 iv: a foreign file named minigw was removed"
# S5: lastna kopija zastarela (druga izdaja): ENA zamenjava mv, brez dotika golega imena
clear10; winrun wdl > /dev/null; rm -f $T/win/ops; winrun wnew > /dev/null
cmp -s $T/rel/wnew/minigw-windows-amd64.exe $GB/minigw.exe || fail "10 S5: not replaced"
[[ "$(wc -l < $T/win/ops)" -eq 1 && "$(<$T/win/ops)" == "mv -f $GB/minigw.exe.new."*" $GB/minigw.exe" ]] || fail "10 S5: not exactly one mv onto minigw.exe: $(cat $T/win/ops)"
nobare "10 S5"
# S6: ubit zagon na vsakem koraku zamenjave (MGTEST_EXIT_AT): naslednji zagon brez omrežja ima overljiv odjemalec, nikoli »cannot verify«
for step in dl prev sums sig exe; do
  clear10; winrun wdl > /dev/null
  MGTEST_EXIT_AT=$step winrun wnew > /dev/null 2>&1 && fail "10 S6 $step: the test hook did not stop the run" || true
  out=$(winrun); print -r -- "$step: $out"
  [[ $out == *"connected to Mini Gateway"* && $out != *"cannot verify"* && -x $GB/minigw.exe ]] || fail "10 S6 $step: no verifiable client after a kill: $out"
  if [[ $step == exe ]]; then cmp -s $T/rel/wnew/minigw-windows-amd64.exe $GB/minigw.exe || fail "10 S6 exe: new exe missing"; else cmp -s $RELEXE $GB/minigw.exe || fail "10 S6 $step: the old exe was lost"; fi
  winrun wnew > /dev/null; cmp -s $T/rel/wnew/minigw-windows-amd64.exe $GB/minigw.exe || fail "10 S6 $step: the next online run did not finish the update"
  [[ -z "$(find $GB \( -name '*.new.*' -o -name '*.chk.*' \) -mmin +10)" ]] || true
done
# S6b: ubit zagon na sig (trenutni par = nova izdaja, .prev = stara, exe stara), nato spet prenos, ubit na vsakem koraku: nikoli »cannot verify«
for step in prev sums sig exe; do
  clear10; winrun wdl > /dev/null
  MGTEST_EXIT_AT=sig winrun wnew > /dev/null 2>&1 || true
  MGTEST_EXIT_AT=$step winrun wnew > /dev/null 2>&1 || true
  out=$(winrun); print -r -- "6b $step: $out"
  [[ $out == *"connected to Mini Gateway"* && $out != *"cannot verify"* && -x $GB/minigw.exe ]] || fail "10 S6b $step: a second killed update left no verifiable client: $out"
done
# S6c: exe zaseden (mv ne uspe) dvakrat zapored: stari overjeni exe ostane, brez povezave je vse še »connected«, kasnejši prenos dokonča
clear10; winrun wdl > /dev/null
for i in 1 2; do out=$(FAILMV=1 winrun wnew); print -r -- "6c $i: $out"; [[ $out == *"could not replace"* ]] || fail "10 S6c $i: no line about the busy exe: $out"; done
out=$(winrun); print -r -- "6c offline: $out"
[[ $out == *"connected to Mini Gateway"* && $out != *"cannot verify"* ]] && cmp -s $RELEXE $GB/minigw.exe || fail "10 S6c: the old verified exe was lost or unverifiable: $out"
winrun wnew > /dev/null; cmp -s $T/rel/wnew/minigw-windows-amd64.exe $GB/minigw.exe || fail "10 S6c: the later online run did not finish the update"
# S3b: nameščen minigw, ki ga podpisane vsote overijo, BREZ zagonske datoteke in brez omrežja: KOPIRA se kot lastna kopija (ena zamenjava mv),
# .verified imenuje bin/minigw.exe, nič na golem imenu; ko MSI posodobi nameščeni exe, naslednji zagon kopira znova; neoverjene datoteke ne kopira
clear10; wtrust "$exe" $MINV; rm -f $T/win/ops
out=$(winrun); print -r -- $out
cmp -s "$exe" $GB/minigw.exe && [[ $out == *"connected to Mini Gateway"* && ! -e $GB/minigw && "$(head -1 $GB/.verified)" == "$(shasum -a 256 $GB/minigw.exe | cut -d' ' -f1)  $GB/minigw.exe" ]] || fail "10 S3b: the verified installed client was not copied into place: $out"
[[ "$(wc -l < $T/win/ops)" -eq 1 ]] || fail "10 S3b: not exactly one mv onto minigw.exe: $(cat $T/win/ops)"
nobare "10 S3b"
[[ "$(<$GB/.asset.sha256)" == "$(shasum -a 256 $GB/minigw.exe | cut -d' ' -f1)" ]] || fail "10 S3b: .asset.sha256 does not match the copy in place"
cp "$exe" $T/win/exe.keep; print "#!/bin/sh\n[ \"\$1\" = --version ] && echo minigw $MINV; [ \"\$1\" = status ] && echo \"Status:    connected\"; exit 0 # updated by the MSI" > "$exe"; wtrust "$exe" $MINV
winrun > /dev/null; cmp -s "$exe" $GB/minigw.exe || fail "10 S3b: an updated installed client was not copied again"
cp $T/win/exe.keep "$exe"
print '#!/bin/sh\ntouch '$T'/win/ran3b' > "$exe"; clear10; wtrust $RELEXE $MINV   # sums vouch for ANOTHER file: the installed one is unverified
winrun > /dev/null; [[ ! -e $T/win/ran3b && ! -e $GB/minigw.exe ]] || fail "10 S3b: an unverified installed client was copied or run"
cp $T/win/exe.keep "$exe"
# S9: exe, ki ga ni mogoče PREBRATI (odprt izključno): NI »neoverjen«. Nobenega rm/mv, ena vrstica, izhod 0, PATH izvožen, zapis nespremenjen; naslednji zagon običajen
clear10; winrun wdl > /dev/null; cp $GB/.verified $T/win/verified.keep; cp $GB/minigw.exe $T/win/exe.keep9; rm -f $T/win/ops; : > $T/win/env9
out=$(FAILREAD=1 WENV=$T/win/env9 winrun wnew 2> $T/win/err9); print -r -- $out
[[ $out == *"the client in bin is in use and could not be checked this time; trying again at the next session start"* && $out != *"cannot verify"* && $out != *"unavailable"* && ! -s $T/win/err9 ]] || fail "10 S9: the unreadable exe was not treated as no verdict: $out $(cat $T/win/err9)"
[[ ! -e $T/win/ops ]] && cmp -s $T/win/exe.keep9 $GB/minigw.exe && cmp -s $T/win/verified.keep $GB/.verified && ! grep -q '^export PATH=' $T/win/env9 || fail "10 S9: the exe or the record was touched, or PATH exported for a record that cannot be matched"
winrun wnew > /dev/null; cmp -s $T/rel/wnew/minigw-windows-amd64.exe $GB/minigw.exe || fail "10 S9: the next normal run did not update"
# S10: stavek o starejšem nameščenem: klavzula le, ko je nameščeni STROGO starejši od lastne kopije
clear10; cp $T/rel/wold/minigw-windows-amd64.exe "$exe"; out=$(winrun wold); print -r -- $out   # nameščeni = lastna kopija (0.5.20): enako
[[ $out == *"using the plugin's own copy of minigw 0.5.20 (the latest release), older than"* && $out != *"is older still"* ]] || fail "10 S10 equal: a clause about an installed client that is not older: $out"
clear10; wtrust "$T/rel/wold/minigw-windows-amd64.exe" 0.5.0   # shranjene vsote overijo nameščeni exe kot 0.5.0
cp $T/rel/wold/minigw-windows-amd64.exe "$exe"; out=$(winrun wold); print -r -- $out
[[ $out == *"using the plugin's own copy of minigw 0.5.20 (the latest release), older than"* && $out == *"the installed minigw 0.5.0 is older still: "* ]] || fail "10 S10 lower: no clause for an older installed client: $out"
cp $T/win/exe.keep "$exe" 2>/dev/null || true
# S11: SVEŽA mapa, brez omrežja, VELJAVNA zagonska datoteka (besedilo Go odjemalca): brez sodbe. Zagonska datoteka in minigw.cmd ostaneta, brez zapisa
# .verified, ena vrstica, izhod 0, nič na golem imenu; šele prava sodba (vsote so, hash cilja ni v njih) jo odstrani
clear10; golauncher "$exe"; cp $GB/minigw $T/win/l11.keep; cp $GB/minigw.cmd $T/win/l11.cmd.keep; rm -f $T/win/ops
out=$(winrun); print -r -- $out
[[ $out == *"the installed minigw cannot be verified yet (no signed checksums could be fetched or verified); trying again at the next session start"* ]] || fail "10 S11: no no-verdict line: $out"
cmp -s $T/win/l11.keep $GB/minigw && cmp -s $T/win/l11.cmd.keep $GB/minigw.cmd && [[ ! -e $GB/.verified && ! -e $T/win/ops ]] || fail "10 S11: the launcher was removed or a record written offline"
nobare "10 S11"
[[ $out != *"nothing verified before"* ]] || fail "10 S11: the no-verdict text still claims nothing was verified before"
# S11b (#168): PATH only when the existing .verified record still matches the files; a record that does not match: no PATH, the same one line
clear10; golauncher "$exe"; : > $T/win/env11b
cp "$exe" $GB/minigw.exe 2>/dev/null || true; printf '%s  %s\n' "$(shasum -a 256 $GB/minigw.exe | cut -d' ' -f1)" "$GB/minigw.exe" > $GB/.verified
out=$(WENV=$T/win/env11b winrun); print -r -- $out
[[ $out == *"cannot be verified yet"* ]] && grep -q '^export PATH=' $T/win/env11b || fail "10 S11b: a matching record did not export PATH: $out"
clear10; golauncher "$exe"; cp "$exe" $GB/minigw.exe 2>/dev/null || true; printf '%s  %s\n' "$(shasum -a 256 $GB/minigw.exe | cut -d' ' -f1)" "$GB/minigw.exe" > $GB/.verified; print x >> $GB/minigw.exe; : > $T/win/env11c
out=$(WENV=$T/win/env11c winrun); print -r -- $out
[[ $out == *"cannot be verified yet"* ]] && ! grep -q '^export PATH=' $T/win/env11c || fail "10 S11c: a record that does not match still exported PATH: $out"
clear10; golauncher "$exe"
winrun wdl > /dev/null   # prava sodba: vsote so, hash cilja (MSI exe) ni v njih: zagonska datoteka gre, lastna kopija velja
[[ ! -e $GB/minigw && ! -e $GB/minigw.cmd ]] && cmp -s $RELEXE $GB/minigw.exe || fail "10 S11: a real verdict did not remove the launcher"
# S12: ista sveža mapa, brez omrežja, nameščen MSI minigw BREZ zagonske datoteke: ne trdi »is not the current release«, pove brez sodbe in ohrani sporočilo o omrežju
clear10; out=$(winrun); print -r -- $out
[[ $out == *"cannot verify the installed minigw yet (no signed checksums could be fetched or verified) and cannot download a verified one"* && $(print -r -- $out | grep -c "^minigw plugin:") = 1 && $out != *"is not the current release"* && $out == *"gateway skills are unavailable until the network is back"* ]] || fail "10 S12: wrong sentence for an unverifiable installed client without any sums: $out"
# S14: brez omrežja, SHRANJENE vsote starejše izdaje (lastna kopija 0.5.20), nameščeni MSI minigw, ki ga vsote ne vsebujejo: brez svežega pogleda ne vemo, kaj je
# zadnja izdaja: stavek »is not the current release … update it« se NE izpiše; velja lastna overjena kopija (»connected«)
clear10; winrun wold > /dev/null; out=$(winrun); print -r -- $out
[[ $out != *"is not the current release"* && $out == *"connected to Mini Gateway"* && $out == *"(the latest release this plugin has seen), older than"* ]] || fail "10 S14: advice about the current release without a fresh signed view: $out"
# in ko ni uporabnega odjemalca: vrstica brez sodbe, ne tega stavka
rm -f $GB/minigw.exe; out=$(winrun); print -r -- $out
[[ $out != *"is not the current release"* && $out == *"cannot verify the installed minigw yet (no signed checksums could be fetched or verified) and cannot download a verified one"* ]] || fail "10 S14b: no-verdict line expected when no client is usable: $out"
# S13: exe zaseden pri kopiranju nameščenega (mv ne uspe): ENA vrstica, brez surove napake orodja, lastna kopija ostane (overja jo .prev)
clear10; winrun wdl > /dev/null; cp $GB/SHA256SUMS $GB/SHA256SUMS.prev; cp $GB/SHA256SUMS.sig $GB/SHA256SUMS.prev.sig
cp "$exe" $T/win/exe.keep13; print "#!/bin/sh\n[ \"\$1\" = --version ] && echo minigw $MINV; [ \"\$1\" = status ] && echo \"Status:    connected\"; exit 0 # newer installed" > "$exe"; wtrust "$exe" $MINV
out=$(FAILMV=1 winrun 2> $T/win/err13); print -r -- $out
[[ "$(print -r -- $out | grep -c 'could not replace')" -eq 1 && ! -s $T/win/err13 && $out == *"connected to Mini Gateway"* ]] || fail "10 S13: not exactly one line, or a raw tool error: $out $(cat $T/win/err13)"
cmp -s $RELEXE $GB/minigw.exe || fail "10 S13: the verified own copy was lost"
cp $T/win/exe.keep13 "$exe"
# S7: ostanek 0.8.1-0.8.9: .keep brez minigw.exe in .aside.lock: .keep gre nazaj, ključavnica izgine, nič drugega
clear10; winrun wdl > /dev/null; mv $GB/minigw.exe $GB/minigw.exe.keep; mkdir $GB/.aside.lock; print 1 > $GB/.aside.lock/pid
winrun > /dev/null
cmp -s $RELEXE $GB/minigw.exe && [[ ! -e $GB/minigw.exe.keep && ! -e $GB/.aside.lock ]] || fail "10 S7: leftover .keep/.aside.lock not cleaned: $(ls -A $GB)"
# oba obstajata: .keep se ne dotakne (eden mv, ko cilja ni)
print other > $GB/minigw.exe.keep; winrun > /dev/null
[[ "$(<$GB/minigw.exe.keep)" == other ]] && cmp -s $RELEXE $GB/minigw.exe || fail "10 S7: a .keep next to an exe was touched"
rm -f $GB/minigw.exe.keep
# S8: dva zagona hkrati (10 parov), oba prenašata: zadnji mv zmaga, ista overjena izdaja, brez ostankov in golih imen
for i in 1 2 3 4 5 6 7 8 9 10; do
  clear10
  winrun wdl > $T/win/p1.out 2> $T/win/p1.err & p1=$!
  winrun wdl > $T/win/p2.out 2> $T/win/p2.err & p2=$!
  wait $p1 && wait $p2 || fail "10 S8 $i: exit status"
  [[ ! -s $T/win/p1.err && ! -s $T/win/p2.err ]] || fail "10 S8 $i: stderr: $(cat $T/win/p1.err $T/win/p2.err)"
  cmp -s $RELEXE $GB/minigw.exe && [[ -s $GB/.verified && -z "$(find $GB \( -name '*.new.*' -o -name '*.chk.*' -o -name '.aside.lock' -o -name '*.keep' \))" ]] || fail "10 S8 $i: leftovers: $(ls -A $GB)"
  nobare "10 S8 $i"
done
# 10j: lastna kopija je (zadnja izdaja) starejša od MIN, nameščenega ni: stavek pove, da je to lastna kopija, ne nameščeni minigw
clear10; out=$(winrun wold); print -r -- $out
[[ $out == *"using the plugin's own copy of minigw 0.5.20 (the latest release), older than $MINV which this plugin's commands need"* && $out != *"using the installed minigw"* ]] || fail "10j: the sentence does not say it is the plugin's own copy: $out"
print "ok 10: Windows: the script never writes bin/minigw; own exe or the Go launcher; one mv; kills at every step stay verifiable"

docker run --rm -v $T:/t:ro debian:stable-slim@sha256:fbfb4059d2127120aa05b6e6064523d2102552f0cf0026ddf0701c12f1f8f3e0 sh -c '
set -u
apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq curl >/dev/null 2>&1
fail() { echo "FAIL: $*"; exit 1; }
run() { CLAUDE_PLUGIN_DATA=$1 CLAUDE_ENV_FILE= MINIGW_RELEASE_BASE=file:///t/rel/$2 HOME=/tmp/home PATH=/tmp/home/bin:$PATH sh /t/$3.sh; }
cp -r /t/home /tmp/home
# trust DIR FILE: SHA256SUMS ob kopiji v DIR/bin s hashem FILE, podpisan s testnim ključem (potem ko je nameščen ssh-keygen)
trust() { mkdir -p $1/bin; { echo "VERSION ${3:-0.5.19}"; echo "$(sha256sum $2 | cut -d" " -f1)  minigw-linux-arm64"; } > $1/bin/SHA256SUMS
  ssh-keygen -Y sign -q -f /t/key -n minigw-release < $1/bin/SHA256SUMS > $1/bin/SHA256SUMS.sig; }
MINV=$(cat /t/minv)

mkdir -p /tmp/d1/bin; cp /t/minigw-old /tmp/d1/bin/minigw
out=$(run /tmp/d1 evil real); echo "$out"
[ "$(/tmp/d1/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "1: old minigw replaced"
echo "$out" | grep -q "not updated: cannot check the release signature" || fail "1: no reason printed"
echo "ok 1: 0.5.16 minigw does not verify a garbage signature"
# 1b (#153): brez ssh-keygen se obstoječa lastna kopija obdrži (izjema), a BREZ zapisa .verified; kljuka je tiha (marker odsoten)
mkdir -p /tmp/d1b/bin /tmp/home/.config/minigw; printf "#!/bin/sh\ntouch /tmp/marker1b\necho minigw 0.5.16\n" > /tmp/d1b/bin/minigw; chmod +x /tmp/d1b/bin/minigw
echo stale > /tmp/d1b/bin/.verified
out=$(run /tmp/d1b evil real); echo "$out"
[ -x /tmp/d1b/bin/minigw ] && [ ! -e /tmp/d1b/bin/.verified ] || fail "1b: the unverified own copy got a record, or was removed"
echo "{\"version\":1,\"enabled\":[{\"gateway\":\"g\",\"project\":3}]}" > /tmp/home/.config/minigw/kb-hooks.json; rm -f /tmp/marker1b
echo "{}" | CLAUDE_PLUGIN_DATA=/tmp/d1b HOME=/tmp/home sh /t/kb-hook.sh stop; [ ! -e /tmp/marker1b ] || fail "1b: the hook ran an unverified copy"
rm -f /tmp/home/.config/minigw/kb-hooks.json
echo "ok 1b: no record for an unverified own copy, the hook stays silent"

mkdir -p /tmp/d2/bin; echo stale > /tmp/d2/bin/.verified  # #153: zavrnitev zbriše zapis
out=$(run /tmp/d2 good real); echo "$out"
[ ! -e /tmp/d2/bin/minigw ] || fail "2: installed without a signature check"
[ ! -e /tmp/d2/bin/.verified ] || fail "2: the record survived a refused install (#153)"
echo "ok 2: first install without ssh-keygen refused"

apt-get install -y -qq openssh-client >/dev/null 2>&1
out=$(run /tmp/d3 good testkey); echo "$out"
[ -x /tmp/d3/bin/minigw ] || fail "3: not installed with a good signature"
[ "$(cat /tmp/d3/bin/.verified)" = "$(sha256sum /tmp/d3/bin/minigw | cut -d" " -f1)  /tmp/d3/bin/minigw" ] || fail "3: no record of the accepted download (#153)"
[ "$(stat -c %a /tmp/d3/bin/.verified)" = 600 ] || fail "3: the record is not mode 600"
echo "ok 3: good signature installs"
# 3b (#153): dve seji hkrati v svežih mapah: obe končata 0, zapis ustreza datoteki, brez ostankov in napak
for i in 1 2 3 4 5 6 7 8 9 10; do
  d=/tmp/dpar$i
  run $d good testkey > /tmp/p1.out 2> /tmp/p1.err & p1=$!
  run $d good testkey > /tmp/p2.out 2> /tmp/p2.err & p2=$!
  wait $p1 && wait $p2 || fail "3b $i: exit status"
  [ ! -s /tmp/p1.err ] && [ ! -s /tmp/p2.err ] || fail "3b $i: stderr: $(cat /tmp/p1.err /tmp/p2.err)"
  [ "$(cut -d" " -f1 $d/bin/.verified)" = "$(sha256sum $d/bin/minigw | cut -d" " -f1)" ] || fail "3b $i: no record, or it does not match"
  [ -z "$(find $d/bin -name "*.new.*" -o -name "*.chk.*" -o -name .aside.lock)" ] || fail "3b $i: leftovers: $(ls -A $d/bin)"
done
echo "ok 3b: two session starts at once keep the record"

mkdir -p /tmp/d4/bin; cp /t/minigw-old /tmp/d4/bin/minigw; cp /t/rel/older/SHA256SUMS /t/rel/older/SHA256SUMS.sig /tmp/d4/bin/  # overjena stara kopija
out=$(run /tmp/d4 bad testkey); echo "$out"
[ "$(/tmp/d4/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "4: replaced despite a wrong signature"
echo "$out" | grep -q "not updated: the release.s SHA256SUMS is not signed" || fail "4: no reason printed"
echo "ok 4: wrong namespace refused, reason printed"

new=$(/tmp/d3/bin/minigw --version | head -1)
mkdir -p /tmp/d5/bin; cp /tmp/d3/bin/minigw /tmp/d3/bin/SHA256SUMS /tmp/d3/bin/SHA256SUMS.sig /tmp/d5/bin/
out=$(run /tmp/d5 older testkey); echo "$out"
[ "$(/tmp/d5/bin/minigw --version | head -1)" = "$new" ] || fail "5: rolled back to an older signed release"
echo "$out" | grep -q "not updated: the latest release (0.5.16) is older than the installed" || fail "5: no reason printed"
echo "ok 5: older signed release refused (#25)"

out=$(MINIGW_ALLOW_DOWNGRADE=1 run /tmp/d5 older testkey); echo "$out"
[ "$(/tmp/d5/bin/minigw --version | head -1)" = "minigw 0.5.16" ] || fail "6: MINIGW_ALLOW_DOWNGRADE=1 did not install"
echo "ok 6: downgrade with MINIGW_ALLOW_DOWNGRADE=1"

mkdir -p /tmp/d7/bin; printf "#!/bin/sh\necho minigw 0.5.19\n" > /tmp/d7/bin/minigw; chmod +x /tmp/d7/bin/minigw; trust /tmp/d7 /tmp/d7/bin/minigw
out=$(run /tmp/d7 good testkey); echo "$out"
[ "$(/tmp/d7/bin/minigw --version | head -1)" = "minigw 0.5.19" ] || fail "7: a signed release without VERSION replaced 0.5.19"
echo "$out" | grep -q "not updated: the latest release (0.5.17) is older than the installed minigw 0.5.19" || fail "7: no reason printed"
echo "ok 7: signed release without VERSION counts as 0.5.17 (N1)"

# 8: kljuka prebere izhod »minigw status« (lažni minigw); v kontekst le fiksni nizi in ID naprave
mkdir -p /tmp/d8/bin; cat > /tmp/d8/bin/minigw <<EOS
#!/bin/sh
[ "\$1" = --version ] && { echo minigw 0.5.19; exit 0; }
cat /tmp/status.out; exit \$(cat /tmp/status.rc)
EOS
chmod +x /tmp/d8/bin/minigw; trust /tmp/d8 /tmp/d8/bin/minigw
ID=AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdE
st() { printf "%s\n" "$1" > /tmp/status.out; echo $2 > /tmp/status.rc; out=$(run /tmp/d8 nonexistent testkey); echo "$out"; }
st "Status:    connected" 0
echo "$out" | grep -q "connected to Mini Gateway" || fail "8: connected"
st "Gateway: x
Status:    token rejected: EVIL text" 1
echo "$out" | grep -q "gateway token was rejected" && ! echo "$out" | grep -q EVIL || fail "8: token rejected"
st "Device:    $ID (waiting for approval)
Status:    device not approved: EVIL text" 1
echo "$out" | grep -q "device not approved (ID $ID)" && ! echo "$out" | grep -q EVIL || fail "8: device not approved"
st "Status:    device not approved: x" 1
echo "$out" | grep -q "installed but not connected" && ! echo "$out" | grep -q "(ID" || fail "8: device not approved without a Device line"
st "Status:    gateway not reachable" 1
echo "$out" | grep -q "installed but not connected" || fail "8: not connected"
! echo "$out" | grep -q "standalone claude command" || fail "8: claude hint with claude present"
rm /tmp/home/bin/claude; out=$(PATH=/tmp/home/bin:/usr/bin:/bin run /tmp/d8 nonexistent testkey); echo "$out"
echo "$out" | grep -q "standalone claude command on PATH" || fail "8: no claude hint"
echo "ok 8: hook reports status reasons and a missing claude"

mkdir -p /tmp/home/.local/bin; cp /t/rel/good/minigw-linux-arm64 /tmp/home/.local/bin/minigw
mkdir -p /tmp/d9/bin; cp /t/minigw-old /tmp/d9/bin/minigw; cp /t/rel/good/SHA256SUMS /t/rel/good/SHA256SUMS.sig /tmp/d9/bin/  # prenesena izdaja: overjen je tudi nameščeni iz iste izdaje
for rel in older good newer; do
  out=$(run /tmp/d9 $rel testkey); echo "$out"
  [ "$(readlink /tmp/d9/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "9: plugin bin not linked to the installed minigw ($rel)"
  [ "$(cat /tmp/d9/bin/.verified)" = "$(sha256sum /tmp/home/.local/bin/minigw | cut -d" " -f1)  /tmp/d9/bin/minigw" ] || fail "9: no record of the linked installed minigw ($rel, #153)"
  ! echo "$out" | grep -q "not updated" || fail "9: downgrade note although the installed minigw is used"
done
rm -f /tmp/home/.local/bin/minigw; cp /t/minigw-old /tmp/home/.local/bin/minigw  # rm: the client of the last run may still be running
out=$(run /tmp/d9 good testkey); echo "$out"
[ ! -L /tmp/d9/bin/minigw ] && cmp -s /t/rel/good/minigw-linux-arm64 /tmp/d9/bin/minigw || fail "9b: minigw older than MIN linked instead of the release"
# 9c: nameščen minigw, ki ni v preverjenih vsotah (zamenjan): se ne zažene, ne poveže; velja prenesena overjena kopija
rm -f /tmp/home/.local/bin/minigw /tmp/ran9c; printf "#!/bin/sh\ntouch /tmp/ran9c\necho minigw 9.9.9\n" > /tmp/home/.local/bin/minigw; chmod 755 /tmp/home/.local/bin/minigw
rm -rf /tmp/d9/bin; mkdir -p /tmp/d9/bin
out=$(run /tmp/d9 good testkey); echo "$out"
[ ! -e /tmp/ran9c ] && [ ! -L /tmp/d9/bin/minigw ] && cmp -s /t/rel/good/minigw-linux-arm64 /tmp/d9/bin/minigw || fail "9c: a substituted installed minigw was run or linked"
[ "$(cut -d" " -f1 /tmp/d9/bin/.verified)" = "$(sha256sum /tmp/d9/bin/minigw | cut -d" " -f1)" ] || fail "9c: the record is not of the downloaded copy (#153)"
[ -f /tmp/d9/bin/SHA256SUMS.sig ] || fail "9c: the verified sums were not kept next to the downloaded copy"
rm -rf /tmp/home/.local
echo "ok 9: installed minigw (verified, not older than MIN) linked, never downloaded; 9c: a substituted one never run"

mkdir -p /tmp/home/.local/bin /tmp/d11/bin; printf "#!/bin/sh\necho minigw $MINV\n" > /tmp/home/.local/bin/minigw; chmod +x /tmp/home/.local/bin/minigw; trust /tmp/d11 /tmp/home/.local/bin/minigw $MINV
out=$(run /tmp/d11 newer testkey); echo "$out"
[ "$(readlink /tmp/d11/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "11: not linked to the installed minigw"
echo "$out" | grep -q "minigw $MINV is installed, 0.99.0 is available: run curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh (or replace /tmp/home/.local/bin/minigw with minigw-linux-arm64 from the latest release" || fail "11: no client-only update hint (M1)"
mkdir -p /tmp/home/.config/systemd/user; : > /tmp/home/.config/systemd/user/minigw-serve.service
out=$(run /tmp/d11 newer testkey); echo "$out"
echo "$out" | grep -q "Mini Gateway $MINV is installed, 0.99.0 is available: /minigw:gw update" || fail "11: no gateway update hint"
rm -rf /tmp/home/.config/systemd
# 11b: BREZ vnaprej vpisanih vsot: nameščen 0.7.0-podoben minigw (neznan hash) se ne zažene, vrstica ponudi posodobitev, plugin prenese svojega
printf "#!/bin/sh\ntouch /tmp/ran11b\necho minigw 0.7.0\n" > /tmp/home/.local/bin/minigw; rm -f /tmp/ran11b
out=$(run /tmp/d11b newer testkey); echo "$out"
echo "$out" | grep -q "the minigw installed at /tmp/home/.local/bin/minigw is not the current release 0.99.0 (or could not be verified); update it: curl -fsSL https://github.com/gh-commiter/mini-llm-gateway/releases/latest/download/install.sh | sh" || fail "11b: no update line for an untrusted installed client"
[ ! -e /tmp/ran11b ] && [ ! -L /tmp/d11b/bin/minigw ] && cmp -s /t/rel/newer/minigw-linux-arm64 /tmp/d11b/bin/minigw || fail "11b: the untrusted client was run or linked, or no own copy"
# 11c: nameščen je prav ta izdaja (hash v vsotah, preverjenih v tem zagonu): overjen že v prvi seji, brez vrstice
rm -f /tmp/home/.local/bin/minigw; cp /t/rel/good/minigw-linux-arm64 /tmp/home/.local/bin/minigw
out=$(run /tmp/d11c good testkey); echo "$out"
[ "$(readlink /tmp/d11c/bin/minigw)" = /tmp/home/.local/bin/minigw ] && ! echo "$out" | grep -q "not the current release" || fail "11c: the current installed client is not trusted in the first session"
[ -f /tmp/d11c/bin/SHA256SUMS ] || fail "11c: the verified sums were not kept"
rm -rf /tmp/home/.local
echo "ok 11: newer signed release than the installed minigw: hint, no download"

# 13 (#48): ~/.local/bin/minigw, ki ni last uporabnika ali je (z mapo ali mapo nad njo) zapisljiv za skupino ali druge, se ne
# zažene in ne poveže; višja mapa s sticky, last roota, sme
for bad in file-gw dir-ow owner up-ow up-foreign-sticky; do
  mkdir -p /tmp/home/.local/bin /tmp/d13/bin; printf "#!/bin/sh\ntouch /tmp/ran13; echo minigw $MINV\n" > /tmp/home/.local/bin/minigw
  chmod 755 /tmp/home/.local/bin/minigw; rm -f /tmp/ran13 /tmp/d13/bin/minigw
  case $bad in file-gw) chmod 775 /tmp/home/.local/bin/minigw ;; dir-ow) chmod 757 /tmp/home/.local/bin ;; owner) chown 1234 /tmp/home/.local/bin/minigw ;;
    up-ow) chmod 777 /tmp/home/.local ;; up-foreign-sticky) chmod 1777 /tmp/home/.local; chown 1234 /tmp/home/.local ;; esac
  out=$(run /tmp/d13 nonexistent testkey); echo "$out"
  [ ! -e /tmp/ran13 ] && [ ! -L /tmp/d13/bin/minigw ] && echo "$out" | grep -q "not using /tmp/home/.local/bin/minigw" || fail "13: unsafe installed minigw used ($bad)"
  rm -rf /tmp/home/.local
done
mkdir -p /tmp/home/.local/bin /tmp/d13/bin; printf "#!/bin/sh\necho minigw $MINV\n" > /tmp/home/.local/bin/minigw; chmod 755 /tmp/home/.local/bin/minigw; trust /tmp/d13 /tmp/home/.local/bin/minigw $MINV
chmod 1777 /tmp/home/.local; rm -f /tmp/d13/bin/minigw
out=$(run /tmp/d13 nonexistent testkey); echo "$out"
[ "$(readlink /tmp/d13/bin/minigw)" = /tmp/home/.local/bin/minigw ] || fail "13: root-owned sticky folder above refused"
rm -rf /tmp/home/.local
echo "ok 13: unsafe ~/.local/bin/minigw refused (#48)"

for u in a b c d; do useradd -m u$u; done
inst() { u=u$1; shift; setpriv --reuid=$u --regid=$u --clear-groups env -i HOME=/home/$u PATH=/t/fakebin:/usr/bin:/bin MINIGW_BASE=file:///t/rel/${REL:-inst} sh /t/install-test.sh "$@"; }
out=$(inst a 2>&1); echo "$out"
cmp -s /t/rel/inst/minigw-linux-arm64 /home/ua/.local/bin/minigw && [ -x /home/ua/.local/bin/minigw-tui ] || fail "12: minigw or minigw-tui not in ~/.local/bin"
grep -q "\"minigw@mini-gateway\": true" /home/ua/.claude/settings.json || fail "12: plugin not added by default"
[ "$(readlink /home/ua/.claude/plugins/data/minigw-mini-gateway/bin/minigw)" = /home/ua/.local/bin/minigw ] || fail "12: plugin not linked to ~/.local/bin/minigw"
inst b --client >/dev/null 2>&1 || fail "12: --client failed"
[ -x /home/ub/.local/bin/minigw ] && [ ! -e /home/ub/.claude/settings.json ] || fail "12: --client added the plugin or no minigw"
inst c --gateway >/dev/null 2>&1 || fail "12: --gateway failed"
[ -f /home/uc/.config/minigw/serve.json ] && [ -f /home/uc/.config/systemd/user/minigw-serve.service ] && [ ! -e /home/uc/.claude/settings.json ] ||
  fail "12: --gateway: no serve.json or systemd unit, or the plugin added"
inst a --purge >/dev/null 2>&1 && fail "12: --purge without --uninstall accepted"
out=$(inst a --uninstall 2>&1); echo "$out"
[ ! -e /home/ua/.local/bin/minigw ] && [ ! -e /home/ua/.local/bin/minigw-tui ] && ! grep -q minigw@mini-gateway /home/ua/.claude/settings.json || fail "12: --uninstall left minigw or the plugin"
inst c --uninstall --purge >/dev/null 2>&1 || fail "12: --uninstall --purge failed"
[ ! -e /home/uc/.config/minigw ] && [ ! -e /home/uc/.config/systemd/user/minigw-serve.service ] && [ ! -e /home/uc/.local/bin/minigw ] || fail "12: --purge left settings, unit or minigw"
out=$(REL=instbad inst d 2>&1); echo "$out"
[ ! -e /home/ud/.local/bin/minigw ] && echo "$out" | grep -q "not signed by Mini Gateway" || fail "12: wrong signature installed"
out=$(env -i PATH=/usr/bin:/bin sh /t/install-test.sh 2>&1); echo "$out" | grep -q "not as root" || fail "12: runs as root"
echo "ok 12: install.sh on Linux: client + plugin, --client, --gateway, --uninstall [--purge], signature"
' || fail "docker cases"
print "plugin install test ok"
