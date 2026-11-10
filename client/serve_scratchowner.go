package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

// validInstance: oblika newInstanceID (ime mape; uninstall ga sestavi v pot).
var validInstance = regexp.MustCompile(`^gw-[0-9a-f]{12}$`)

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

// scratchOwner: ID iz datoteke lastnika; "" = je ni. Neberljiva, ne navadna datoteka ali povezava: scratchUnknown
// (tuja, nikoli prevzeta samodejno). oNoBlock: podtaknjena cev ne ustavi GET /general.
func scratchOwner(dir string) string {
	f, err := os.OpenFile(filepath.Join(dir, scratchOwnerFile), os.O_RDONLY|oNoBlock, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	} else if err != nil {
		return scratchUnknown
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return scratchUnknown
	}
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return scratchUnknown
	}
	return strings.TrimSpace(string(b))
}

// scratchUnknown: lastnik, ki ga ni mogoče prebrati (odločitev: zaprto, kot tuj).
const scratchUnknown = "?"

// inUseText: drug prehod drži zaklep (Windows: ali program, ki ima mapo odprto, npr. ukazna vrstica v njej).
func inUseText() string {
	if runtime.GOOS == "windows" {
		return "used by another gateway that is running (or a program has the folder open)"
	}
	return "used by another gateway that is running"
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

// claimScratch: prva raba zapiše datoteko lastnika (O_EXCL: od dveh hkratnih zagonov zmaga eden), le v mapi tega
// uporabnika, ki je nihče ne drži (obstoječa mapa 0.5.23 je tako prevzeta enkrat); mapa te instance dobi zaklep.
// Vrne scratchState in opozorilo (zaklepa ni bilo mogoče dobiti, npr. omrežni disk brez flock).
func claimScratch(dir, id string) (string, error) {
	if scratchOwner(dir) == "" {
		releaseScratch() // Windows: naš ročaj bi bil videti kot zaklep
		if scratchLocked(dir) {
			return scratchInUse, nil
		}
		if err := protectScratch(dir); err != nil || !scratchFolderMine(dir) {
			releaseScratch()
			return scratchStale, err
		}
		if f, err := os.OpenFile(filepath.Join(dir, scratchOwnerFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoBlock, 0o600); err == nil {
			_, err = f.WriteString(id + "\n")
			if f.Close() != nil || err != nil {
				os.Remove(f.Name())
			}
		}
	}
	if o := scratchOwner(dir); o != id {
		releaseScratch()
		if o == "" {
			return scratchStale, nil
		}
		return scratchState(dir, id), nil
	}
	if err := lockScratch(dir); errors.Is(err, errScratchHeld) {
		return scratchInUse, nil
	} else if err != nil {
		return "", fmt.Errorf("scratch folder %s: no lock (%v): a Scratch base shared by more than one computer is not supported", dir, err)
	}
	return "", nil
}

var errScratchHeld = errors.New("held by another process")

func (s *server) claimScratch() {
	c := s.conf()
	st, warn := claimScratch(c.Scratch, c.Instance)
	if warn != nil {
		s.log.Print("WARNING: ", warn)
	}
	s.scratchOther.Store(st != "")
	if st != "" {
		s.log.Print("WARNING: " + scratchOtherMsg(c.Scratch, st))
	}
}

func scratchOtherMsg(dir, st string) string {
	who := "belongs to another gateway that is not running (stale owner)"
	if st == scratchInUse {
		who = "is " + inUseText()
	}
	return "the Scratch folder " + dir + " " + who + ": agents are off and nothing in it is cleaned up. " +
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

// takeScratch (odločitev 2: prevzem = preimenovanje, nikoli združevanje): stara mapa druge instance (old, prava mapa
// tega uporabnika) postane <osnova>/<id> z ID-jem te instance in zaščito (protectScratch; napaka: vse nazaj).
// Trenutna mapa te instance (cur, tudi sosednja) in mapa <osnova>/<id>, ki je na poti: brez pogovorov izbrisani, s
// pogovori ob strani kot <id>.old-<datum> (aside). Zaklep drugega prehoda: zavrnitev.
func takeScratch(old, cur, id string, now time.Time) (dst, aside string, err error) {
	releaseScratch()
	if scratchLocked(old) {
		return "", "", errors.New("the folder is " + inUseText() + ": stop it first")
	}
	if !scratchFolderMine(old) {
		return "", "", errors.New("the folder is not a real folder of this user")
	}
	dst = filepath.Join(filepath.Dir(old), id)
	var moved [][2]string // za vrnitev ob napaki
	undo := func(err error) (string, string, error) {
		for _, m := range slices.Backward(moved) {
			err = errors.Join(err, os.Rename(m[1], m[0]))
		}
		return "", "", err
	}
	for _, p := range []string{cur, dst} {
		if p == "" || p == old {
			continue
		}
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return undo(err)
		}
		if !scratchHasConversations(p) {
			if err := os.RemoveAll(p); err != nil {
				return undo(err)
			}
			continue
		}
		a := dst + ".old-" + now.Format("2006-01-02")
		if _, err := os.Lstat(a); err == nil {
			a += now.Format("-150405")
		}
		if err := os.Rename(p, a); err != nil {
			return undo(err)
		}
		moved, aside = append(moved, [2]string{p, a}), a
	}
	if filepath.Base(old) != id {
		if err := os.Rename(old, dst); err != nil {
			return undo(err)
		}
		moved = append(moved, [2]string{old, dst})
	}
	if err := protectScratch(dst); err != nil {
		releaseScratch()
		return undo(fmt.Errorf("%s: %w", dst, err))
	}
	if err := writeFileAtomic(filepath.Join(dst, scratchOwnerFile), []byte(id+"\n")); err != nil {
		releaseScratch()
		return undo(err)
	}
	return dst, aside, nil
}

// scratchSibling: druga mapa instance v osnovi (Scratch row: »used by another gateway«, »stale owner«; Take over).
type scratchSibling struct {
	Name  string `json:"name"`
	State string `json:"state"` // scratchInUse, scratchStale
}

// scratchSiblings: prave mape v osnovi trenutnega Scratch z datoteko lastnika druge instance (brez *.old-*, brez
// trenutne mape), le mape tega uporabnika. Mape brez datoteke lastnika ne štejejo (lastnik: prehod 0.5.23 → 0.5.24 je
// le prevzem nastavljene mape ob prvem zagonu).
func scratchSiblings(cur, id string) []scratchSibling {
	ents, _ := os.ReadDir(filepath.Dir(cur))
	var out []scratchSibling
	for _, e := range ents {
		p := filepath.Join(filepath.Dir(cur), e.Name())
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 || strings.Contains(e.Name(), ".old-") || realPath(p) == realPath(cur) ||
			scratchOwner(p) == "" || !scratchFolderMine(p) {
			continue
		}
		if st := scratchState(p, id); st != "" {
			out = append(out, scratchSibling{e.Name(), st})
		}
	}
	return out
}

// takeScratchAPI: POST /general/scratch/take (Take over; minigw gw scratch take). Zapisano v dnevnik.
const scratchTakeBusy = "scratch folder: minigw sessions or agents are active; take it over when no minigw session runs"

func (s *server) takeScratchAPI(body []byte) (int, any) {
	fail := func(status int, msg string) (int, any) { return status, errorBody(status, msg) }
	var b struct {
		Name string `json:"name"` // sosednja mapa v osnovi; prazno = mapa, ki jo imenuje nastavitev
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
	}
	if !s.scratchGate.TryLock() {
		return fail(409, scratchTakeBusy)
	}
	defer s.scratchGate.Unlock()
	if (s.hub != nil && s.hub.count() > 0) || s.agentsRunning.Load() > 0 {
		return fail(409, scratchTakeBusy)
	}
	foreign := s.scratchOther.Load()
	defer func() { // tudi po napaki: ročaj, lastnik in zaklep na mapi v nastavitvah (kot premik)
		if s.offline {
			return
		}
		if err := protectScratch(s.conf().Scratch); err != nil {
			s.log.Printf("WARNING: scratch folder %s: %v", s.conf().Scratch, err)
		}
		if s.claimScratch(); foreign && !s.scratchOther.Load() {
			sandboxInit(s.conf().Scratch, s.cfgDir, s.log.Printf)
		}
	}()
	c := s.conf()
	cur, old := c.scratchDir(), c.scratchDir()
	if b.Name != "" { // le neposredna podmapa osnove, prava mapa druge instance
		if strings.ContainsAny(b.Name, `/\:`) || b.Name == "." || b.Name == ".." || filepath.Base(b.Name) != b.Name {
			return fail(400, "name: a folder name in the Scratch base, without path separators")
		}
		old = filepath.Join(filepath.Dir(cur), b.Name)
		if i := slices.IndexFunc(scratchSiblings(cur, c.Instance), func(x scratchSibling) bool { return x.Name == b.Name }); i < 0 {
			return fail(400, "name: "+b.Name+" is not another gateway's Scratch folder in "+filepath.Dir(cur))
		}
	}
	switch scratchState(old, c.Instance) {
	case "":
		return fail(409, "the Scratch folder "+old+" is this gateway's own: nothing to take over")
	case scratchInUse:
		return fail(409, "the Scratch folder "+old+" is "+inUseText()+": stop it first, or choose another folder")
	}
	id := c.Instance
	if id == "" { // minigw gw brez prehoda, pred prvim zagonom te izdaje
		id = newInstanceID()
	}
	if old == cur {
		cur = ""
	}
	dst, aside, err := takeScratch(old, cur, id, time.Now())
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
	g := s.apiGeneral(false)
	g.Connect = ""
	if aside != "" {
		g.Warning = "this gateway's previous folder is kept as " + aside
	}
	return 200, g
}
