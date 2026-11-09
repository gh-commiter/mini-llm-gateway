package main

// --minigw-work=sync (kot Sync.swift in ShareClient v MiniGW/main.swift): agent dela v kopiji na routerju namesto v
// priklopu. Pred agentom minigw pošlje manifest in datoteke, ki jih router nima; po agentu prevzame spremembe kopije.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

type syncEntry struct {
	Path   string `json:"path"` // relativna, z »/«
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"` // ms; obe strani nastavita enak čas → ni ponovnega zgoščevanja
	SHA256 string `json:"sha256"`
	Exec   bool   `json:"exec"` // izvršljiva (bit x lastnika)
}

type syncChanges struct {
	Changed []syncEntry `json:"changed"`
	Deleted []string    `json:"deleted"`
	Skipped []string    `json:"skipped"` // večje od maxSyncFile
	// Base: sha256 različice, iz katere je agent začel, za vsako spremenjeno in izbrisano pot (ni ključa = datoteke ni
	// bilo). Pišemo ali brišemo le, če je datoteka še ta različica. nil = starejši router → stanje ob prenosu.
	Base map[string]string `json:"base"`
	// Known (#20, ni v JSON): ali je odjemalec to različico poti res poslal routerju; nil = brez preverjanja (testi).
	Known func(path, sha string) bool `json:"-"`
}

// maxSyncFile: cela datoteka gre v eni zahtevi; router sprejme telo do 64 MB.
const maxSyncFile = 64 << 20

// Nikoli se ne sinhronizira (poleg .gitignore): git, izhod gradnje, smeti macOS, spori.
var defaultIgnore = []string{".git/", "target/", ".build/", "node_modules/", "build/", "dist/", "DerivedData/",
	".DS_Store", "._*", "*.minigw-conflict"}

type ignoreRule struct {
	pattern           string
	dirOnly, anchored bool
}

// parseIgnore: .gitignore v korenu (podmnožica): ime, »dir/«, vzorci (»*.ext«), »/« na začetku ali v sredini = od korena.
// ponytail: brez »!«, »**« in .gitignore v podmapah; dodaj, ko bo treba.
func parseIgnore(gitignore string) []ignoreRule {
	var rules []ignoreRule
	for _, line := range append(append([]string{}, defaultIgnore...), strings.Split(gitignore, "\n")...) {
		p := strings.TrimSpace(line)
		if p == "" || strings.HasPrefix(p, "#") || strings.HasPrefix(p, "!") {
			continue
		}
		dirOnly := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		anchored := strings.Contains(p, "/")
		p = strings.TrimPrefix(p, "/")
		if p != "" {
			rules = append(rules, ignoreRule{p, dirOnly, anchored})
		}
	}
	return rules
}

// caseFold: datotečni sistem brez razlike v velikih črkah (APFS, NTFS): pravila veljajo ne glede na črke.
var caseFold = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// normName: ime, kot ga vidi tak datotečni sistem (Windows odreže končne pike in presledke: ».GIT.« = ».git«).
// normName: upper→lower preslika ſ, ı, K (Kelvin) v ASCII; »git~1« je kratko 8.3 ime ».git« na Windows.
func normName(c string) string {
	n := strings.ToLower(strings.ToUpper(strings.TrimRight(c, ". ")))
	if shortGit.MatchString(n) {
		return ".git"
	}
	return n
}

var shortGit = regexp.MustCompile(`^git~[0-9]+$`)

// shortName (N10): kratko ime 8.3 na Windows (CLAUDE~1, VSCODE~1, HUSKY~1 …) je druga pot do iste mape; tam nobena
// pot s takim delom ni dovoljena (windowsNames; v testih tudi drugje).
var shortName = regexp.MustCompile(`^[^~.]{1,6}~[0-9]+(\.[^.]{0,3})?$`)

var windowsNames = runtime.GOOS == "windows"

// claudeRun: mape v .claude, katerih datoteke Claude Code zažene ali jim da dovoljenja (hooki, ukazi, agenti, skilli).
var claudeRun = map[string]bool{"hooks": true, "commands": true, "agents": true, "skills": true}

// protectedPath: pot, ki je prevzem nikoli ne zapiše ali izbriše: git (tudi ».GIT«, ».git.«), hooki gita (.husky,
// .githooks), .claude/{hooks,commands,agents,skills} in nastavitve, ki zaganjajo ukaze (.claude/settings*.json,
// .vscode/tasks.json, .envrc). Isti seznam velja za WebDAV (M1).
func protectedPath(rel string) bool {
	comps := strings.Split(rel, "/")
	for i, c := range comps {
		if comps[i] = normName(c); comps[i] == ".git" || (windowsNames && shortName.MatchString(c)) || comps[i] == ".husky" || comps[i] == ".githooks" ||
			(i > 0 && comps[i-1] == ".claude" && claudeRun[comps[i]]) {
			return true
		}
	}
	last, parent := comps[len(comps)-1], ""
	if len(comps) > 1 {
		parent = comps[len(comps)-2]
	}
	return last == ".envrc" || (parent == ".vscode" && last == "tasks.json") ||
		(parent == ".claude" && strings.HasPrefix(last, "settings") && strings.HasSuffix(last, ".json"))
}

func ignored(rules []ignoreRule, rel string, isDir bool) bool {
	name := path.Base(rel)
	for _, r := range rules {
		target, pattern := name, r.pattern
		if r.anchored {
			target = rel
		}
		if caseFold {
			target, pattern = strings.ToLower(target), strings.ToLower(pattern)
		}
		if ok, _ := path.Match(pattern, target); ok && (!r.dirOnly || isDir) {
			return true
		}
	}
	return false
}

// ignoredPath: pot ali katera koli njena nadrejena mapa je prezrta.
func ignoredPath(rules []ignoreRule, rel string) bool {
	comps := strings.Split(rel, "/")
	for i := range comps {
		if ignored(rules, strings.Join(comps[:i+1], "/"), i < len(comps)-1) {
			return true
		}
	}
	return false
}

// loadIgnore: le navadna datoteka do 1 MB (ne FIFO ali naprava); sicer kot brez .gitignore. trusted: mapa uporabnika,
// .gitignore je lahko povezava (dotfiles); router (kopija) ne sledi povezavi ven iz root.
func loadIgnore(root string, trusted bool) []ignoreRule {
	gi, _ := readRegular(root, ".gitignore", 1<<20, trusted)
	return parseIgnore(strings.ReplaceAll(string(gi), "\r", ""))
}

// readRegular: navadna datoteka rel pod root do max bajtov. Brez follow os.Root ne sledi povezavi ven iz root (tudi ne
// prek podtaknjene starševske mape); O_NONBLOCK ne čaka na FIFO.
func readRegular(root, rel string, max int64, follow bool) ([]byte, bool) {
	var f *os.File
	var err error
	if follow {
		f, err = os.OpenFile(filepath.Join(root, filepath.FromSlash(rel)), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	} else if r, rerr := os.OpenRoot(root); rerr != nil {
		err = rerr
	} else {
		defer r.Close()
		f, err = r.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > max {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	return data, err == nil && int64(len(data)) <= max
}

// syncScan: datoteke pod root brez prezrtih in simbolnih povezav; zgoščuje le, kar ima drugo velikost ali čas kot v prev.
// trusted: mapa uporabnika (odjemalec); sicer kopija routerja.
func syncScan(root string, prev map[string]syncEntry, trusted bool) (map[string]syncEntry, []string) {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rules := loadIgnore(root, trusted)
	files, skipped := map[string]syncEntry{}, []string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if normName(d.Name()) == ".git" { // tudi ».GIT«: nikoli ne gre na router
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if ignored(rules, rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || ignored(rules, rel, false) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > maxSyncFile {
			skipped = append(skipped, rel)
			return nil
		}
		if e, ok := syncEntryOf(p, rel, prev); ok {
			files[rel] = e
		}
		return nil
	})
	return files, skipped
}

// syncEntryOf: ena datoteka; false, če ne obstaja ali ni navadna datoteka.
func syncEntryOf(full, rel string, prev map[string]syncEntry) (syncEntry, bool) {
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return syncEntry{}, false
	}
	e := syncEntry{Path: rel, Size: info.Size(), Mtime: info.ModTime().UnixMilli(), Exec: info.Mode()&0o100 != 0}
	if p, ok := prev[rel]; ok && p.Size == e.Size && p.Mtime == e.Mtime {
		p.Exec = e.Exec
		return p, true
	}
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return syncEntry{}, false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !os.SameFile(info, st) { // vmes zamenjana s povezavo ali FIFO
		return syncEntry{}, false
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return syncEntry{}, false
	}
	e.SHA256 = hex.EncodeToString(h.Sum(nil))
	return e, true
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// protectedCanonical (#11): zaščita velja tudi za kanonično pot: uporabnikova povezava v mapi (hooks → .git/hooks)
// bi sicer pisanje v »hooks/pre-commit« pustila. Kanonična pot najglobljega obstoječega prednika (Windows: dolga imena)
// skupaj z manjkajočim koncem; nečitljiva pot = zaščitena.
func protectedCanonical(full, root string) bool {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return true
	}
	p, rest := full, ""
	for {
		if c, err := filepath.EvalSymlinks(p); err == nil {
			rel, err := filepath.Rel(r, filepath.Join(c, rest))
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
			return rel != "." && protectedPath(filepath.ToSlash(rel))
		}
		parent := filepath.Dir(p)
		if parent == p {
			return true
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
}

// syncResolve: relativna pot druge strani → datoteka pod root; false za »..«, absolutno pot, »\«, prazne dele ali pobeg
// prek povezave (preveri davResolve).
func syncResolve(rel, root string) (string, bool) {
	comps := strings.Split(rel, "/")
	for i, c := range comps {
		if c == "" || c == "." || c == ".." || strings.ContainsAny(c, "\\\x00") {
			return "", false
		}
		comps[i] = url.PathEscape(c)
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	p, _, ok := davResolve("/"+strings.Join(comps, "/"), "", filepath.Clean(r))
	return p, ok
}

// syncWrite: atomarno zapiše datoteko (ustvari mape), nastavi čas spremembe druge strani in ohrani način obstoječe
// (sicer 0755/0644 po exec; CreateTemp sam naredi 0600).
// Biti x sledijo exec, razen pri keepX (uporabnik je bit spremenil sam) in na Windows (bita x ni).
func syncWrite(full string, data []byte, mtime int64, exec, keepX bool) error {
	mode := os.FileMode(0o644)
	if st, err := os.Lstat(full); err == nil && st.Mode().IsRegular() {
		mode = st.Mode().Perm()
	}
	if !keepX && runtime.GOOS != "windows" {
		mode = withExec(mode, exec)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if st, err := os.Lstat(full); err == nil && st.IsDir() {
		os.Remove(full) // prazna mapa na mestu datoteke (mapa → datoteka); polna ostane in pisanje ne uspe
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".minigw-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	readOnlyWin := runtime.GOOS == "windows" && mode&0o200 == 0 // #6: Windows ne preimenuje čez datoteko samo za branje
	if err == nil {
		if readOnlyWin {
			err = tmp.Chmod(0o666)
		} else {
			err = tmp.Chmod(mode)
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && readOnlyWin {
		os.Chmod(full, 0o666) // atribut samo za branje dol; po preimenovanju spet gor
	}
	if err == nil {
		err = os.Rename(tmp.Name(), full)
	}
	if err == nil && readOnlyWin {
		err = os.Chmod(full, mode)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	t := time.UnixMilli(mtime)
	return os.Chtimes(full, t, t)
}

// withExec: biti x tam, kjer je bit r (0644 → 0755); brez exec vsi biti x izklopljeni.
func withExec(mode os.FileMode, exec bool) os.FileMode {
	mode &^= 0o111
	if exec {
		mode |= (mode & 0o444) >> 2
	}
	return mode
}

// syncApply: zapiše in izbriše, kar je agent spremenil. Datoteke, ki ni več različica, iz katere je agent začel
// (ch.Base; spremenil jo je uporabnik ali drug agent), ne prepiše: različico routerja zapiše kot <ime>.minigw-conflict.
// Poti, prezrte pri odjemalcu, preskoči.
// state = manifest ob prenosu. Vrne sporočila za stderr in obdelane poti (potrditev routerju).
func syncApply(ch syncChanges, root string, state map[string]syncEntry, fetch func(string) []byte) (notes, done []string) {
	rules := loadIgnore(root, true) // odjemalec
	before := map[string]string{}
	for p, e := range state {
		before[p] = e.SHA256
	}
	base := func(p string) string {
		if ch.Base == nil {
			return before[p]
		}
		b, ok := ch.Base[p]
		// #20: osnovo pošlje router; velja le različica, ki jo je ta odjemalec poslal (ali je ni bilo). Sicer osnova, ki
		// je nobena datoteka nima: uporabnikova sprememba med izvajanjem ostane, agentova gre v .minigw-conflict.
		if ok && b != before[p] && ch.Known != nil && !ch.Known(p, b) {
			return "\x00unknown base"
		}
		return b
	}
	for _, p := range ch.Deleted { // pred spremembami: datoteka »a« → mapa »a/…«
		if protectedPath(p) {
			notes = append(notes, "not deleted "+p+": protected path")
			done = append(done, p)
			continue
		}
		if ignoredPath(rules, p) {
			done = append(done, p)
			continue
		}
		done = append(done, p)
		full, ok := syncResolve(p, root)
		if !ok {
			continue
		}
		if protectedCanonical(full, root) {
			notes = append(notes, "not deleted "+p+": protected path (through a link)")
			continue
		}
		local, has := syncEntryOf(full, p, state)
		if !has {
			delete(state, p)
			continue
		}
		if local.SHA256 != base(p) {
			notes = append(notes, "conflict: "+p+" was deleted on the gateway but changed here; kept")
			continue
		}
		if runtime.GOOS == "windows" { // #6: datoteke samo za branje Windows ne izbriše
			os.Chmod(full, 0o666)
		}
		os.Remove(full)
		delete(state, p)
	}
	for _, e := range ch.Changed {
		if protectedPath(e.Path) {
			notes = append(notes, "skipped "+e.Path+": protected path")
			done = append(done, e.Path)
			continue
		}
		if ignoredPath(rules, e.Path) {
			notes = append(notes, "skipped "+e.Path+": ignored here")
			done = append(done, e.Path)
			continue
		}
		full, ok := syncResolve(e.Path, root)
		if !ok { // potrjena, sicer bi jo router pošiljal v nedogled
			notes = append(notes, "skipped "+e.Path+": invalid path here")
			done = append(done, e.Path)
			continue
		}
		if protectedCanonical(full, root) {
			notes = append(notes, "skipped "+e.Path+": protected path (through a link)")
			done = append(done, e.Path)
			continue
		}
		if local, has := syncEntryOf(full, e.Path, state); has && local.SHA256 == e.SHA256 {
			// Le bit x, in le če ga uporabnik od prenosa ni spremenil sam (Windows ga nima).
			if local.Exec != e.Exec && local.Exec != state[e.Path].Exec { // stanje ostane: bit ostane uporabnikov
				notes = append(notes, "kept your file mode of "+e.Path+" (the agent changed it too)")
				done = append(done, e.Path)
				continue
			} else if local.Exec != e.Exec && runtime.GOOS != "windows" {
				if st, err := os.Stat(full); err == nil {
					os.Chmod(full, withExec(st.Mode().Perm(), e.Exec))
				}
			}
			state[e.Path] = local
			done = append(done, e.Path)
			continue
		}
		data := fetch(e.Path)
		if data == nil || sha256Hex(data) != e.SHA256 {
			notes = append(notes, "skipped "+e.Path+": download failed")
			continue
		}
		local, _ := syncEntryOf(full, e.Path, state) // znova tik pred pisanjem
		if local.SHA256 != base(e.Path) {            // spremenjena (ali izbrisana) tudi tukaj
			c, ok := syncResolve(e.Path+".minigw-conflict", root)
			if !ok || syncWrite(c, data, e.Mtime, e.Exec, false) != nil {
				notes = append(notes, "conflict: "+e.Path+" changed here and on the gateway; the gateway's version could not be saved")
				continue
			}
			notes = append(notes, "conflict: "+e.Path+" changed here and on the gateway; kept yours, the gateway's version is in "+e.Path+".minigw-conflict")
			done = append(done, e.Path)
			continue
		}
		// Bit x agenta, razen če ga je uporabnik od prenosa spremenil sam.
		_, has := state[e.Path]
		keepX := local.Path != "" && has && local.Exec != state[e.Path].Exec
		if err := syncWrite(full, data, e.Mtime, e.Exec, keepX); err != nil {
			notes = append(notes, "cannot write "+e.Path+": "+err.Error())
			continue
		}
		state[e.Path] = e
		done = append(done, e.Path)
	}
	for _, p := range ch.Skipped {
		notes = append(notes, "not copied back (larger than 64 MB): "+p)
	}
	return notes, done
}

func (s *shareClient) note(msg string) { fmt.Fprintln(os.Stderr, "minigw: "+oneLine(msg)) } // M2: ime datoteke z \n ne ponaredi vrstice

// syncPush: pred agentom manifest k routerju, nato datoteke, ki jih potrebuje.
// caseCollisions (#2): poti, ki se razlikujejo le v velikih črkah (A.txt in a.txt): na routerju z APFS ali NTFS bi
// bile ena datoteka, zato ne gredo nikamor.
func caseCollisions(files map[string]syncEntry) []string {
	byFold := map[string][]string{}
	for p := range files {
		f := strings.ToLower(p)
		byFold[f] = append(byFold[f], p)
	}
	var out []string
	for _, ps := range byFold {
		if len(ps) > 1 {
			out = append(out, ps...)
		}
	}
	sort.Strings(out)
	return out
}

func (s *shareClient) syncPush(k string) bool {
	files, skipped := syncScan(s.root, s.synced, true)
	for _, p := range caseCollisions(files) {
		delete(files, p)
		if !s.reported["case:"+p] {
			s.reported["case:"+p] = true
			s.note("not synced: " + p + " differs from another file only in upper and lower case")
		}
	}
	for _, p := range skipped {
		if !s.reported[p] {
			s.reported[p] = true
			s.note("not synced (larger than 64 MB): " + p)
		}
	}
	list := make([]syncEntry, 0, len(files))
	for _, e := range files {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	body, _ := json.Marshal(list)
	h := map[string]string{"x-share-key": k, "content-type": "application/json"}
	if runtime.GOOS == "windows" {
		h["x-sync-exec"] = "none" // bita x ni: router ohrani način kopije
	}
	st, data := call(s.c, "POST", "/minigw/sync/manifest", h, body, 300*time.Second)
	var r struct {
		Need []string `json:"need"`
		Kept []string `json:"kept"`
	}
	if st != 200 || json.Unmarshal(data, &r) != nil || r.Need == nil {
		var e struct{ Error struct{ Message string } }
		if json.Unmarshal(data, &e); e.Error.Message != "" {
			s.note("sync to the gateway failed: " + e.Error.Message)
		} else {
			s.note(fmt.Sprintf("sync to the gateway failed (HTTP %d)", st))
		}
		return false
	}
	// Neuspešen prenos in »kept« (router je obdržal agentovo različico): stanje poti ostane staro, da prevzem zazna spor.
	keepOld := func(p string) {
		if old, ok := s.synced[p]; ok {
			files[p] = old
		} else {
			delete(files, p)
		}
	}
	for _, p := range r.Kept {
		keepOld(p)
		s.note("the agent's change to " + p + " is not copied back yet; kept on the gateway")
	}
	for _, p := range r.Need {
		full, ok := syncResolve(p, s.root)
		_, listed := files[p]
		b, err := os.ReadFile(full)
		if !listed || !ok || err != nil {
			keepOld(p)
			continue
		}
		if st, _ := call(s.c, "PUT", "/minigw/sync/file", map[string]string{"x-share-key": k, "x-sync-path": url.PathEscape(p)}, b, 300*time.Second); st != 204 {
			keepOld(p)
			if st == 409 {
				s.note("sync: " + p + " changed during sync; it goes with the next request") // N4: nikoli pot na začetku vrstice
			} else {
				s.note(fmt.Sprintf("cannot sync %s (HTTP %d)", p, st))
			}
		}
	}
	s.synced = files
	if s.sent == nil {
		s.sent = map[string]map[string]bool{}
	}
	for p, e := range files { // različice, ki jih ima router od tega odjemalca (#20)
		if s.sent[p] == nil || len(s.sent[p]) > 64 {
			s.sent[p] = map[string]bool{}
		}
		s.sent[p][e.SHA256] = true
	}
	return true
}

// sentVersion: ali je ta odjemalec v tej seji poslal (ali obdržal) to različico poti.
func (s *shareClient) sentVersion(p, sha string) bool { return s.sent[p][sha] }

// syncPull: po agentu spremembe kopije v mapo (spori → <ime>.minigw-conflict).
func (s *shareClient) syncPull(k string) bool {
	st, data := call(s.c, "GET", "/minigw/sync/changes", map[string]string{"x-share-key": k}, nil, 300*time.Second)
	var ch syncChanges
	if st != 200 || json.Unmarshal(data, &ch) != nil {
		s.note(fmt.Sprintf("cannot get the agent's changes from the gateway (HTTP %d)", st))
		s.unpulled = true
		return false
	}
	ch.Known = s.sentVersion
	notes, done := syncApply(ch, s.root, s.synced, func(p string) []byte {
		st, b := call(s.c, "GET", "/minigw/sync/file", map[string]string{"x-share-key": k, "x-sync-path": url.PathEscape(p)}, nil, 300*time.Second)
		if st != 200 {
			return nil
		}
		return b
	})
	for _, n := range notes {
		s.note(n)
	}
	// Potrditev: router šele zdaj šteje te spremembe za prevzete; ostale pošlje znova.
	s.unpulled = len(done) < len(ch.Changed)+len(ch.Deleted)
	if len(done) > 0 {
		body, _ := json.Marshal(done)
		if st, _ := call(s.c, "POST", "/minigw/sync/ack", map[string]string{"x-share-key": k, "content-type": "application/json"}, body, 60*time.Second); st != 204 {
			s.note("the gateway did not confirm the copied-back changes; they come again next time")
			s.unpulled = true
		}
	}
	if len(ch.Changed)+len(ch.Deleted) > 0 {
		s.note(fmt.Sprintf("synced back from the gateway: %d changed, %d deleted", len(ch.Changed), len(ch.Deleted)))
	}
	return true
}
