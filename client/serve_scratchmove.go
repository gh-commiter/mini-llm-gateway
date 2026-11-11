package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Premik Scratch (0.5.23): vse za gradnje (zrcala, kloni, odvisnosti, predpomnilniki, stanje agentov) gre skupaj.
// Kot Router.scratchInUse na Macu: ne med sejami minigw ali agenti; med premikom nove zahteve dobijo 503 (scratchGate).

const scratchBusy = "scratch folder: minigw sessions or agents are active; move it when no minigw session runs"

// scratchMovingMsg: odgovor zahtevam odjemalcev med premikom.
const scratchMovingMsg = "The gateway is moving its Scratch folder; try again in a minute"

// protectTarget: DACL le za lastnika (Windows) oz. 0700 na cilju, preden gre vanj karkoli; testi jo zamenjajo.
var protectTarget = protectScratch

// scratchSuggest: predlagana mapa, ko Scratch ni izbran (stara privzeta mapa ali v domači mapi; Windows, škatla MXC
// pokaže imena v domači mapi). "" = ni predloga.
func scratchSuggest(scratch string) string {
	if runtime.GOOS != "windows" || scratchHomeWarning(scratch) == "" {
		return ""
	}
	return newScratchDefault()
}

// scratchCheck: pravila za novo mapo Scratch (PATCH general, premik); explicit = uporabnik jo je vpisal. "" = v redu.
func (s *server) scratchCheck(scratch string, explicit bool) string {
	if !filepath.IsAbs(scratch) {
		return "scratch folder: an absolute path"
	}
	if explicit { // najprej razlog kot na Macu (koren diska, domača mapa …), nato mapa nastavitev
		if why := scratchProblem(scratch, s.conf().AllowedFolders); why != "" { // M1
			return "scratch folder: " + why
		}
	}
	rs, err := realPath(scratch)
	if err != nil { // #83: nerazrešljivo = zavrnjeno (scratchOverlaps, scratchInsideOther)
		return "scratch folder: " + err.Error()
	}
	rc, err := realPath(s.cfgDir)
	if err != nil {
		return "scratch folder: " + err.Error()
	}
	if under(scratch, s.cfgDir) || under(s.cfgDir, scratch) || under(rs, rc) || under(rc, rs) { // #83: tudi skozi stičišče
		return "scratch folder: must not contain or be inside the config folder " + s.cfgDir
	}
	if bad := scratchOverlaps(s.conf().AllowedFolders, scratch); len(bad) > 0 { // L1: tudi privzeta mapa
		return "scratch folder: the allowed folder " + bad[0] + " contains it or is inside it"
	}
	if d := scratchInsideOther(scratch, s.conf().Instance); d != "" { // L1 (0.5.24)
		return "scratch folder: it is or is inside another gateway's Scratch folder " + d
	}
	return ""
}

// moveScratchAPI: POST /general/scratch/move {"to": "…"}; "to" je osnova (odločitev 3: cilj <to>/<instanca>, datoteka
// lastnika gre z mapo); prazno = privzeta osnova.
func (s *server) moveScratchAPI(body []byte) (int, any) {
	fail := func(status int, msg string) (int, any) { return status, errorBody(status, msg) }
	var b struct {
		To string `json:"to"`
	}
	if err := decodeStrict(body, &b); err != nil {
		return fail(400, err.Error())
	}
	to := b.To
	if to == "" {
		if to = newScratchDefault(); to == "" {
			to = defaultScratch()
		}
	}
	if filepath.IsAbs(to) { // L1′: Abs (Windows: GetFullPathName) tudi odreže pike in presledke na koncu imen
		to, _ = filepath.Abs(to)
	}
	if why := s.scratchCheck(to, b.To != ""); why != "" { // pravila za izbrano osnovo
		return fail(400, why)
	}
	to = s.scratchFolder(to)
	if !s.scratchGate.TryLock() { // tekoče zahteve odjemalcev (tudi drug premik) držijo bralno ključavnico
		return fail(409, scratchBusy)
	}
	defer s.scratchGate.Unlock()
	if s.hub.count() > 0 || s.agentsRunning.Load() > 0 {
		return fail(409, scratchBusy)
	}
	if why := s.offlineScratchHeld(); why != "" {
		return fail(409, why)
	}
	old := s.conf().scratchDir()
	ro, err1 := realPath(old)
	rt, err2 := realPath(to)
	if err := errors.Join(err1, err2); err != nil { // #83
		return fail(400, err.Error())
	}
	if samePath(ro, rt) {
		g := s.apiGeneral(false)
		g.Connect = ""
		return 200, g
	}
	foreign := scratchState(old, s.conf().Instance) != ""
	defer func() { // L1′ (Windows): ročaj spet na veljavni mapi, tudi po napaki; N4: lastnik in zaklep
		if err := protectScratch(s.conf().Scratch); err != nil {
			s.log.Printf("WARNING: scratch folder %s: %v", s.conf().Scratch, err)
		}
		if !s.offline {
			if s.claimScratch(); foreign && !s.scratchOther.Load() {
				sandboxInit(s.conf().Scratch, s.cfgDir, s.log.Printf)
			}
		}
	}()
	if foreign { // N4: mapa drugega prehoda ostane, kjer je; ta prehod začne svojo
		if err := protectTarget(to); err != nil {
			return fail(500, "scratch folder "+to+": "+err.Error())
		}
		if err := s.update(func(c *serveConfig) error { c.Scratch = to; return nil }); err != nil {
			return fail(500, err.Error())
		}
		s.log.Printf("scratch folder %s belongs to another gateway and stays; this gateway now uses %s", old, to)
		g := s.apiGeneral(false)
		g.Connect = ""
		return 200, g
	}
	s.log.Printf("moving the scratch folder %s to %s", old, to)
	renamed, err := moveScratchDir(old, to)
	if err != nil {
		s.log.Printf("scratch folder not moved: %v", err)
		return fail(500, "scratch folder not moved (it stays "+old+"): "+err.Error())
	}
	if err := s.update(func(c *serveConfig) error { c.Scratch = to; return nil }); err != nil {
		releaseScratch()
		if renamed {
			err = errors.Join(err, os.Rename(to, old))
		} else {
			err = errors.Join(err, os.RemoveAll(to))
		}
		s.log.Printf("scratch folder not moved: %v", err)
		return fail(500, "scratch folder not moved (it stays "+old+"): "+err.Error())
	}
	sandboxRewarn(to)
	g := s.apiGeneral(false)
	g.Connect = ""
	if !renamed { // kopija preverjena in nastavitve zamenjane: šele zdaj stara mapa
		if err := os.RemoveAll(old); err != nil {
			s.log.Printf("WARNING: scratch folder moved to %s, but the old folder %s was not removed: %v", to, old, err)
			g.Warning = "moved; the old folder " + old + " could not be removed (" + err.Error() + "): remove it yourself"
		}
	}
	s.log.Printf("scratch folder moved to %s", to)
	return 200, g
}

// moveScratchDir: preimenovanje na istem nosilcu, sicer kopija s preverjanjem (velikost, SHA-256; povezave se ne
// sledijo, ampak prenesejo). Ob napaki ostane stara mapa nedotaknjena, cilj se pobriše. renamed = stare mape ni več.
func moveScratchDir(old, dst string) (renamed bool, err error) {
	if under(dst, old) || under(old, dst) {
		return false, errors.New("the new folder is inside the old one or the other way round")
	}
	if ents, err := os.ReadDir(dst); err == nil && len(ents) > 0 {
		return false, fmt.Errorf("the new folder already contains “%s”", ents[0].Name())
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(old); errors.Is(err, fs.ErrNotExist) { // še ni bilo nič: le nova mapa
		return false, protectTarget(dst)
	}
	if err := protectTarget(dst); err != nil { // L1′: nadrejena mapa preverjena, preden gre vanjo stara mapa
		os.Remove(dst)
		return false, err
	}
	releaseScratch() // L1′: ročaj brez deljenja brisanja bi preprečil odstranitev in preimenovanje
	os.Remove(dst)   // prazna (zgoraj): Rename na Windows ne gre v obstoječo mapo
	if err := os.Rename(old, dst); err == nil {
		if err := protectTarget(dst); err != nil {
			releaseScratch()
			return false, errors.Join(err, os.Rename(dst, old))
		}
		return true, nil
	} else if !crossDevice(err) { // M2: kopija in brisanje le na drug nosilec (zaklenjena mapa ostane cela)
		os.Remove(dst)
		return false, err
	}
	// drug nosilec: kopija
	if err := protectTarget(dst); err != nil {
		return false, err
	}
	if err := scratchCopy(old, dst); err != nil {
		releaseScratch()
		return false, errors.Join(err, os.RemoveAll(dst))
	}
	return false, nil
}

// scratchCopy: mape, datoteke (s časi, ki jih rabi čiščenje starih kopij) in povezave; vsaka datoteka preverjena.
func scratchCopy(src, dst string) error {
	var dirs []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, p)
			if rel == "." {
				return nil
			}
			return os.Mkdir(to, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, to)
		case d.Type().IsRegular():
			if err := copyVerified(p, to, info); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			return os.Chtimes(to, info.ModTime(), info.ModTime())
		}
		return nil // ponytail: vtičnice, cevi … (tekoče, ne gradnje) se ne prenesejo
	})
	if err != nil {
		return err
	}
	for _, d := range slices.Backward(dirs) { // časi map za čiščenje (pruneMirrors, expireCopies) po vsebini
		if info, err := os.Lstat(d); err == nil {
			rel, _ := filepath.Rel(src, d)
			os.Chtimes(filepath.Join(dst, rel), info.ModTime(), info.ModTime())
		}
	}
	return nil
}

func copyVerified(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	sum, err := fileHash(dst)
	switch {
	case err != nil:
		return err
	case n != info.Size():
		return fmt.Errorf("copied %d of %d bytes (the file changed during the move)", n, info.Size())
	case !bytes.Equal(sum, h.Sum(nil)):
		return errors.New("the copy differs from the original")
	}
	return nil
}

func fileHash(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// scratchMoveHint: kako premakniti (opozorilo, CLI).
func scratchMoveHint(suggest string) string {
	if suggest == "" {
		suggest = `D:\minigw-scratch`
	}
	return "minigw gw scratch move " + strings.TrimSpace(suggest) + ", or Settings → General → Scratch folder → Move"
}
