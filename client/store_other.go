//go:build !windows

package main

// Povezava v datoteki uporabnika z dovoljenji 0600 (Linux, in macOS pri preizkusu; macOS minigw v aplikaciji
// uporablja Keychain). ponytail: na Linuxu brez Secret Service; dodaj, če bo potreben namizni keyring.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var errNotFound = errors.New("not found")

func storePath() string {
	dir, err := os.UserConfigDir()
	if err != nil { // L2: nikoli v skupno začasno mapo
		fail("cannot find the settings folder for profiles: " + err.Error())
	}
	return filepath.Join(dir, "minigw", "connection.json")
}

// profilePath: profil »default« je prejšnja edina povezava (connection.json), drugi v connections/<ime>.json (#17).
func profilePath(name string) string {
	if name == defaultProfile {
		return storePath()
	}
	return filepath.Join(filepath.Dir(storePath()), "connections", name+".json")
}

func storeLoad(name string) ([]byte, error) {
	if !validProfile(name) {
		return nil, errNotFound
	}
	data, err := os.ReadFile(profilePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	return data, err
}

func storeSave(name string, data []byte) error {
	if !validProfile(name) {
		return errors.New("invalid profile name")
	}
	return writePrivate(profilePath(name), data)
}

// writePrivate (L2, #49): mapa 0700, datoteka 0600 atomarno (nova datoteka O_EXCL 0600, fsync, preimenovanje): simbolna
// povezava na mestu datoteke je napaka, trda se ne sledi, prekinjen zapis ne pusti prirezane datoteke.
func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errors.New(path + " is a symbolic link: not written")
	}
	return writeFileAtomic(path, data)
}

func storeDelete(name string) {
	if validProfile(name) {
		os.Remove(profilePath(name))
	}
}

// storeNames: imena shranjenih profilov (neveljavna imena datotek prezre).
func storeNames() []string {
	var out []string
	if _, err := os.Stat(storePath()); err == nil {
		out = append(out, defaultProfile)
	}
	ents, _ := os.ReadDir(filepath.Join(filepath.Dir(storePath()), "connections"))
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && validProfile(n) && n != defaultProfile && e.Type().IsRegular() {
			out = append(out, n)
		}
	}
	return out
}

const storeName = "a private file"

// Ključ naprave (clientDevice): svoja datoteka ob profilih (0600); disconnect je ne odstrani.
func deviceFile() string { return filepath.Join(filepath.Dir(storePath()), "device-key") }

// deviceLoad: "" brez napake = ključa ni; druga napaka branja je napaka (L1).
func deviceLoad() (string, error) {
	b, err := os.ReadFile(deviceFile())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

func deviceSave(d string) error { return writePrivate(deviceFile(), []byte(d+"\n")) }

func deviceDelete() { os.Remove(deviceFile()) }

// Ključi prehodov (minigw serve gateway). Linux: secret-tool (libsecret), sicer gateway-keys.json (0600) ob serve.json.
// macOS: vedno ta datoteka, nikoli Keychain (Go minigw serve na Macu je le za razvoj in preizkus; router je aplikacija).

func keyFile(cfgDir string) string { return filepath.Join(cfgDir, "gateway-keys.json") }

// fileKeys: manjkajoča datoteka = prazno; pokvarjena = napaka (sicer bi naslednji add izbrisal vse ključe).
func fileKeys(cfgDir string) (map[string]string, error) {
	m := map[string]string{}
	data, err := os.ReadFile(keyFile(cfgDir))
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %v", keyFile(cfgDir), err)
	}
	return m, nil
}

func secretTool() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	tool := lookPath("secret-tool")
	return tool
}

func osKeyGet(cfgDir, name string) (string, string) {
	if tool := secretTool(); tool != "" {
		if out, _ := command(tool, "lookup", "service", "minigw", "gateway", name).Output(); len(out) > 0 {
			return strings.TrimSpace(string(out)), "the Secret Service"
		}
	}
	m, _ := fileKeys(cfgDir)
	return m[name], keyFile(cfgDir)
}

// osKeySet: ključ nikoli v argv (secret-tool ga bere s stdin).
func osKeySet(cfgDir, name, key string) (string, error) {
	if tool := secretTool(); tool != "" {
		cmd := command(tool, "store", "--label=minigw gateway "+name, "service", "minigw", "gateway", name)
		cmd.Stdin = strings.NewReader(key)
		if cmd.Run() == nil {
			m, _ := fileKeys(cfgDir) // starejša kopija v datoteki ne sme ostati
			if _, ok := m[name]; ok {
				delete(m, name)
				writeKeys(cfgDir, m)
			}
			return "the Secret Service", nil
		}
	}
	m, err := fileKeys(cfgDir)
	if err != nil {
		return "", err
	}
	m[name] = key
	where := keyFile(cfgDir) + " (mode 0600)"
	if runtime.GOOS == "linux" {
		where = "WARNING: no Secret Service (secret-tool), so it is in " + where
	}
	return where, writeKeys(cfgDir, m)
}

func writeKeys(cfgDir string, m map[string]string) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return writeFileAtomic(keyFile(cfgDir), data)
}

func osKeyDel(cfgDir, name string) {
	if tool := secretTool(); tool != "" {
		command(tool, "clear", "service", "minigw", "gateway", name).Run()
	}
	if m, err := fileKeys(cfgDir); err == nil && m[name] != "" {
		delete(m, name)
		writeKeys(cfgDir, m)
	}
}

// readHidden: vrstica s terminala brez odmeva (stty).
func readHidden(f *os.File) (string, error) {
	off := command("stty", "-echo")
	off.Stdin = f
	if off.Run() == nil {
		defer func() {
			on := command("stty", "echo")
			on.Stdin = f
			on.Run()
		}()
	}
	return bufio.NewReader(f).ReadString('\n')
}

// Vrednost Run v registru obstaja le na Windows.
func regRunSet(string, string) error    { return errors.New("the registry exists on Windows only") }
func regRunGet(string) (string, error)  { return "", errNotFound }
func regRunDel(string)                  {}
func msiInstallDir() string             { return "" }
func msiPartInstalled(string) bool      { return false }
func msiStopProcesses(string, []string) {}
func msiexecPath() string               { return "msiexec" }

func protectDir(string) error { return nil } // drugod: mapa 0700, datoteke 0600

func releaseScratch() {} // le Windows (L1′: ročaj mape Scratch)

// scratchFolderMine (N4): prava mapa (ne povezava) tega uporabnika.
func scratchFolderMine(dir string) bool { return ownedByMe(dir) }

// scratchLock (N4): flock na scratchLockFile v mapi Scratch te instance, dokler prehod teče.
var scratchLock struct {
	sync.Mutex
	f *os.File
}

// oNoBlock (N4): datoteke lastnika in zaklepa: podtaknjena cev ne blokira, povezava se ne sledi.
const oNoBlock = syscall.O_NONBLOCK | syscall.O_NOFOLLOW

// lockScratch: errScratchHeld = zaklep drži drug proces; druga napaka (ni navadna datoteka, flock ni podprt) = brez zaklepa.
func lockScratch(dir string) error {
	scratchLock.Lock()
	defer scratchLock.Unlock()
	if scratchLock.f != nil { // flock velja za odprto datoteko: tudi naša prejšnja bi zaklep zavrnila
		// LOCK_UN pred Close: otrok fork/exec druge gorutine ima do exec kopijo datoteke (tudi O_CLOEXEC), zato sam
		// Close zaklepa ne sprosti takoj (ponovni lockScratch iste mape bi vrnil errScratchHeld)
		syscall.Flock(int(scratchLock.f.Fd()), syscall.LOCK_UN)
		scratchLock.f.Close()
		scratchLock.f = nil
	}
	f, err := os.OpenFile(filepath.Join(dir, scratchLockFile), os.O_RDWR|os.O_CREATE|oNoBlock, 0o600)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return errors.New(scratchLockFile + " is not a regular file")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return errScratchHeld
	} else if err != nil {
		f.Close()
		return err
	}
	scratchLock.f = f
	return nil
}

// scratchHeldHere: zaklep mape dir drži ta proces (scratchLock), ne drug prehod.
func scratchHeldHere(dir string) bool {
	scratchLock.Lock()
	defer scratchLock.Unlock()
	if scratchLock.f == nil {
		return false
	}
	a, err := scratchLock.f.Stat()
	b, berr := os.Lstat(filepath.Join(dir, scratchLockFile))
	return err == nil && berr == nil && os.SameFile(a, b)
}

// crossDevice (M2): preimenovanje ne gre, ker je cilj na drugem nosilcu.
func crossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// connRefused (H1): na vratih nič ne posluša.
func connRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

// scratchLocked: zaklep mape dir drži drug prehod (ki teče).
func scratchLocked(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, scratchLockFile), os.O_RDONLY|oNoBlock, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false
	} else if err != nil {
		return true // zaprto: ni mogoče preveriti
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return true
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// protectScratch (L2): mapa s kopijami map odjemalcev je naša (Lstat: ne povezava) in le naša (0700).
func protectScratch(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return errForeignOwner
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

var listenControl func(network, address string, c syscall.RawConn) error // drugod privzeto (brez SO_REUSEPORT)

// knownConfigDir: domača mapa iz baze uporabnikov (os/user), ne iz HOME/XDG_CONFIG_HOME.
func knownConfigDir() (string, error) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return "", errors.New("no home folder")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(u.HomeDir, "Library", "Application Support"), nil
	}
	return filepath.Join(u.HomeDir, ".config"), nil
}

// Namestitev s programsko mapo, pladnjem in PATH je le na Windows.
func knownUserProgramsDir() string                 { return "" }
func trayMutexExists() bool                        { return false }
func closeRunningTray() error                      { return nil }
func quitRunningTray()                             { trayRun("", false) } // minigw tray quit: isto sporočilo kot minigw tray
func startProcessDetached(string, ...string) error { return errors.New("Windows only") }
func moveFileOnReboot(string) error                { return errors.New("Windows only") }
func regEnvPathGet() (string, error)               { return "", errors.New("Windows only") }
func regEnvPathSet(string) error                   { return errors.New("Windows only") }

func shortPath(p string) string { return p } // le Windows (8.3)

// safeDir (L3): mapa ali datoteka ni zapisljiva za druge, je last tega uporabnika ali root, za skupino pa le, če ji
// uporabnik zaupa (trustedGroup; Homebrew: /opt/homebrew/bin je drwxrwxr-x <user>:admin).
func safeDir(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	perm := fi.Mode().Perm()
	return ok && perm&0o002 == 0 && (int(st.Uid) == os.Getuid() || st.Uid == 0) &&
		(perm&0o020 == 0 || trustedGroup(runtime.GOOS, st.Gid, readGroupFile()))
}

// safePath (#48): safeDir za p, njegovo mapo in vse mape nad njo do /, za dano in za pravo pot (brez povezav); višje
// mape smejo biti zapisljive le z bitom sticky in last roota ali uporabnika (/tmp): tam drugi ne preimenujejo ali
// zamenjajo vnosa, ki je naš.
func safePath(p string) bool {
	real, err := filepath.EvalSymlinks(p)
	return err == nil && safeChain(p) && safeChain(real)
}

func safeChain(p string) bool {
	p, err := filepath.Abs(p)
	if err != nil || !safeDir(p) {
		return false
	}
	for d, strict := filepath.Dir(p), true; ; d, strict = filepath.Dir(d), false {
		fi, err := os.Stat(d)
		if err != nil {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		sticky := !strict && fi.Mode()&os.ModeSticky != 0 && ok && (st.Uid == 0 || int(st.Uid) == os.Getuid())
		if !safeDir(d) && !sticky {
			return false
		}
		if filepath.Dir(d) == d {
			return true
		}
	}
}

// trustedGroup: macOS le admin (80; staff je vsak lokalni uporabnik) (kot Swift PluginSetup.trustedGroup);
// drugod le zasebna skupina uporabnika: primarna, z imenom uporabnika in brez drugih članov v /etc/group.
func trustedGroup(goos string, gid uint32, etcGroup string) bool {
	if goos == "darwin" {
		return gid == 80
	}
	u, err := user.Current()
	if err != nil || int(gid) != os.Getgid() {
		return false
	}
	g, err := user.LookupGroupId(strconv.Itoa(int(gid)))
	if err != nil || g.Name != u.Username {
		return false
	}
	for _, l := range strings.Split(etcGroup, "\n") { // ime:geslo:gid:člani
		if f := strings.Split(l, ":"); len(f) == 4 && f[2] == g.Gid && strings.TrimSpace(f[3]) != "" && strings.TrimSpace(f[3]) != u.Username {
			return false
		}
	}
	return true
}

func readGroupFile() string {
	b, _ := os.ReadFile("/etc/group")
	return string(b)
}

// ownedByMe (L2): mapa (ne povezava) je last tega uporabnika; prva raven pod začasno mapo, ki jo smemo deliti samodejno.
func ownedByMe(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.IsDir() && int(st.Uid) == os.Getuid()
}

// openLockFile (#82): zaklep mape; povezava se ne sledi, le navadna datoteka.
func openLockFile(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|oNoBlock, 0o600)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errors.New(p + " is not a regular file")
	}
	return f, nil
}

// tryLockFile (#82): flock brez čakanja; busy = drži ga druga odprta datoteka (drug proces ali posel).
func tryLockFile(f *os.File, exclusive bool) (busy bool, err error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	err = syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR), err
}

func unlockFile(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
