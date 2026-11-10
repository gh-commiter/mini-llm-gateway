package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// N4, odločitvi 2 in 3 (0.5.24): vsaka instanca prehoda ima svoj Scratch <osnova>/<instanca>. Datoteka lastnika v njem
// hrani ID instance (serve.json »instance«); mape druge instance prehod ne uporablja (brez agentov, brez čiščenja),
// dokler je lastnik izrecno ne prevzame (Take over). Prehod, ki teče, drži zaklep mape (Windows: ročaj iz
// protectScratch, drugod flock na scratchLockFile). Ista pravila kot ScratchOwner.swift na Macu.

const (
	scratchOwnerFile = ".minigw-owner"
	scratchLockFile  = ".minigw-lock"
	scratchInUse     = "in-use" // lastnik je drug prehod, ki teče (drži zaklep)
	scratchStale     = "stale"  // lastnik je drug prehod, ki ne teče: Take over
)

// scratchNotConversations (odločitev 2): ne štejejo kot pogovori, ko je na poti stara mapa te instance.
var scratchNotConversations = []string{scratchOwnerFile, scratchLockFile, "tmp", "mxc", "gitexec", "prompts", "cache", "caches", "deriveddata"}

func newInstanceID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "gw-" + hex.EncodeToString(b)
}

// ensureInstance: ID instance enkrat ustvarjen in shranjen v serve.json (le za lastnika), pred newServer.
func ensureInstance(cfgPath string, c *serveConfig) error {
	if c.Instance != "" {
		return nil
	}
	file, err := loadServeConfig(cfgPath)
	if err != nil {
		return err
	}
	c.Instance = newInstanceID()
	file.Instance = c.Instance
	return writeServeConfig(cfgPath, file)
}

func scratchOwner(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, scratchOwnerFile))
	return strings.TrimSpace(string(b))
}

// scratchState: "" = mapa te instance (ali še brez lastnika), sicer scratchInUse ali scratchStale.
func scratchState(dir, id string) string {
	if o := scratchOwner(dir); o == "" || o == id {
		return ""
	}
	if scratchLocked(dir) {
		return scratchInUse
	}
	return scratchStale
}

// claimScratch: prva raba zapiše datoteko lastnika (O_EXCL: od dveh hkratnih zagonov zmaga eden); mapa te instance
// dobi zaklep. Vrne scratchState.
func claimScratch(dir, id string) string {
	if f, err := os.OpenFile(filepath.Join(dir, scratchOwnerFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_, err = f.WriteString(id + "\n")
		if f.Close() != nil || err != nil {
			os.Remove(f.Name())
		}
	}
	if o := scratchOwner(dir); o != "" && o != id {
		releaseScratch() // Windows: naš ročaj bi bil videti kot zaklep drugega
		return scratchState(dir, id)
	}
	if !lockScratch(dir) {
		return scratchInUse
	}
	return ""
}

func (s *server) claimScratch() {
	c := s.conf()
	st := claimScratch(c.Scratch, c.Instance)
	s.scratchOther.Store(st != "")
	if st != "" {
		s.log.Print("WARNING: " + scratchOtherMsg(c.Scratch, st))
	}
}

func scratchOtherMsg(dir, st string) string {
	who := "another gateway that is not running (stale owner)"
	if st == scratchInUse {
		who = "another gateway that is running"
	}
	return "the Scratch folder " + dir + " belongs to " + who + ": agents are off and nothing in it is cleaned up. " +
		"Choose a Scratch folder of this gateway (Settings → General → Scratch folder → Move…, or minigw gw scratch move <folder>), " +
		"or, if that gateway is gone, Take over (minigw gw scratch take)"
}

// scratchDir: Scratch iz nastavitev (prazno = defaultScratch, kot newServer).
func (c serveConfig) scratchDir() string {
	if c.Scratch == "" {
		return defaultScratch()
	}
	return c.Scratch
}

// scratchFolder (odločitev 3): nastavitev Scratch imenuje osnovo; Scratch prehoda je njena podmapa <osnova>/<instanca>.
// Osnova, ki je že Scratch tega prehoda ali se imenuje po instanci, ostane.
func (s *server) scratchFolder(base string) string {
	c := s.conf()
	if c.Instance == "" || filepath.Base(base) == c.Instance || realPath(base) == realPath(c.scratchDir()) {
		return base
	}
	return filepath.Join(base, c.Instance)
}

// scratchHasConversations (odločitev 2): kaj drugega kot scratchNotConversations z vsaj eno datoteko (napaka branja
// šteje: raje ob strani kot izbrisano).
func scratchHasConversations(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, e := range ents {
		if slices.Contains(scratchNotConversations, strings.ToLower(e.Name())) {
			continue
		}
		found := false
		filepath.WalkDir(filepath.Join(dir, e.Name()), func(_ string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				found = true
				return fs.SkipAll
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

// takeScratch (odločitev 2: prevzem = preimenovanje, nikoli združevanje): stara mapa druge instance postane
// <osnova>/<id> z ID-jem te instance. Mapa te instance, ki je že tam: brez pogovorov izbrisana, s pogovori ob strani kot
// <id>.old-<datum> (aside). Zaklep drugega prehoda: zavrnitev.
func takeScratch(old, id string, now time.Time) (dst, aside string, err error) {
	releaseScratch()
	if scratchLocked(old) {
		return "", "", errors.New("the folder is used by another gateway that is running: stop it first")
	}
	dst = filepath.Join(filepath.Dir(old), id)
	if filepath.Base(old) != id {
		if _, err := os.Lstat(dst); err == nil {
			if scratchHasConversations(dst) {
				aside = dst + ".old-" + now.Format("2006-01-02")
				if _, err := os.Lstat(aside); err == nil {
					aside += now.Format("-150405")
				}
				if err := os.Rename(dst, aside); err != nil {
					return "", "", err
				}
			} else if err := os.RemoveAll(dst); err != nil {
				return "", "", err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
		if err := os.Rename(old, dst); err != nil {
			if aside != "" {
				err = errors.Join(err, os.Rename(aside, dst))
			}
			return "", "", err
		}
	}
	return dst, aside, writeFileAtomic(filepath.Join(dst, scratchOwnerFile), []byte(id+"\n"))
}

// takeScratchAPI: POST /general/scratch/take (Take over; minigw gw scratch take). Zapisano v dnevnik.
func (s *server) takeScratchAPI() (int, any) {
	fail := func(status int, msg string) (int, any) { return status, errorBody(status, msg) }
	if !s.scratchGate.TryLock() {
		return fail(409, scratchBusy)
	}
	defer s.scratchGate.Unlock()
	if (s.hub != nil && s.hub.count() > 0) || s.agentsRunning.Load() > 0 {
		return fail(409, scratchBusy)
	}
	c := s.conf()
	old := c.scratchDir()
	switch scratchState(old, c.Instance) {
	case "":
		return fail(409, "the Scratch folder "+old+" is this gateway's own: nothing to take over")
	case scratchInUse:
		return fail(409, "the Scratch folder "+old+" is used by another gateway that is running: stop it first, or choose another folder")
	}
	id := c.Instance
	if id == "" { // minigw gw brez prehoda, pred prvim zagonom te izdaje
		id = newInstanceID()
	}
	dst, aside, err := takeScratch(old, id, time.Now())
	if err != nil {
		s.log.Printf("scratch folder %s not taken over: %v", old, err)
		return fail(500, "scratch folder not taken over (it stays "+old+"): "+err.Error())
	}
	if err := s.update(func(c *serveConfig) error { c.Scratch, c.Instance = dst, id; return nil }); err != nil {
		s.log.Printf("scratch folder taken over as %s, settings not saved: %v", dst, err)
		return fail(500, "the Scratch folder is now "+dst+", but the settings were not saved ("+err.Error()+"): set \"scratch\" in "+s.cfgPath+" to it")
	}
	note := ""
	if aside != "" {
		note = "; this gateway's previous folder is kept as " + aside
	}
	s.log.Printf("scratch folder %s taken over from another gateway: now %s%s", old, dst, note)
	if !s.offline {
		if err := protectScratch(dst); err != nil {
			s.log.Printf("WARNING: scratch folder %s: %v", dst, err)
		}
		if s.claimScratch(); !s.scratchOther.Load() {
			sandboxInit(dst, s.cfgDir, s.log.Printf)
		}
	}
	g := s.apiGeneral(false)
	g.Connect = ""
	if aside != "" {
		g.Warning = "this gateway's previous folder is kept as " + aside
	}
	return 200, g
}
