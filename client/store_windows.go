//go:build windows

package main

// Povezava (naslov, žeton, ključ routerja, ključ naprave) v Windows Credential Manager (Upravitelj poverilnic),
// šifrirano z uporabnikovo prijavo. Brez zunanjih knjižnic: advapi32 neposredno.

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const credTarget = "minigw:router"

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32      = syscall.NewLazyDLL("advapi32.dll")
	procWrite     = advapi32.NewProc("CredWriteW")
	procRead      = advapi32.NewProc("CredReadW")
	procDelete    = advapi32.NewProc("CredDeleteW")
	procFree      = advapi32.NewProc("CredFree")
	procEnumerate = advapi32.NewProc("CredEnumerateW")
	errNotFound   = errors.New("not found")
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
)

// credTargetFor: profil »default« je prejšnja edina povezava (minigw:router), drugi minigw:router:<ime> (#17).
func credTargetFor(name string) string {
	if name == defaultProfile {
		return credTarget
	}
	return credTarget + ":" + name
}

func storeLoad(name string) ([]byte, error)    { return credLoad(credTargetFor(name)) }
func storeSave(name string, data []byte) error { return credSave(credTargetFor(name), data) }
func storeDelete(name string)                  { credDelete(credTargetFor(name)) }

// storeNames: imena profilov iz Credential Manager (CredEnumerateW s filtrom minigw:router*).
func storeNames() []string {
	filter, _ := syscall.UTF16PtrFromString(credTarget + "*")
	var n uint32
	var list **credential
	if r, _, _ := procEnumerate.Call(uintptr(unsafe.Pointer(filter)), 0, uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&list))); r == 0 {
		return nil
	}
	defer procFree.Call(uintptr(unsafe.Pointer(list)))
	var out []string
	for _, c := range unsafe.Slice(list, n) {
		t := syscall.UTF16ToString(unsafe.Slice(c.TargetName, wcslen(c.TargetName)))
		if t == credTarget {
			out = append(out, defaultProfile)
		} else if name, ok := strings.CutPrefix(t, credTarget+":"); ok && validProfile(name) && name != defaultProfile {
			out = append(out, name)
		}
	}
	return out
}

func credLoad(name string) ([]byte, error) {
	target, _ := syscall.UTF16PtrFromString(name)
	var c *credential
	r, _, err := procRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		if err == syscall.Errno(1168) { // ERROR_NOT_FOUND
			return nil, errNotFound
		}
		return nil, err
	}
	defer procFree.Call(uintptr(unsafe.Pointer(c)))
	return append([]byte{}, unsafe.Slice(c.CredentialBlob, c.CredentialBlobSize)...), nil
}

func credSave(name string, data []byte) error {
	target, _ := syscall.UTF16PtrFromString(name)
	user, _ := syscall.UTF16PtrFromString("minigw")
	c := credential{Type: credTypeGeneric, TargetName: target, UserName: user, Persist: credPersistLocalMachine,
		CredentialBlobSize: uint32(len(data))}
	if len(data) > 0 {
		c.CredentialBlob = &data[0]
	}
	if r, _, err := procWrite.Call(uintptr(unsafe.Pointer(&c)), 0); r == 0 {
		return err
	}
	return nil
}

func credDelete(name string) {
	target, _ := syscall.UTF16PtrFromString(name)
	procDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
}

// Ključi prehodov (minigw serve gateway): generična poverilnica minigw/gateway/<ime>.
func osKeyGet(_, name string) (string, string) {
	b, _ := credLoad("minigw/gateway/" + name)
	return string(b), storeName
}

func osKeySet(_, name, key string) (string, error) {
	return storeName, credSave("minigw/gateway/"+name, []byte(key))
}

func osKeyDel(_, name string) { credDelete("minigw/gateway/" + name) }

// readHidden: vrstica s konzole brez odmeva (ENABLE_ECHO_INPUT izklopljen).
func readHidden(f *os.File) (string, error) {
	k := syscall.NewLazyDLL("kernel32.dll")
	get, set := k.NewProc("GetConsoleMode"), k.NewProc("SetConsoleMode")
	var mode uint32
	if r, _, _ := get.Call(f.Fd(), uintptr(unsafe.Pointer(&mode))); r != 0 {
		set.Call(f.Fd(), uintptr(mode&^0x4))
		defer set.Call(f.Fd(), uintptr(mode))
	}
	return bufio.NewReader(f).ReadString('\n')
}

const storeName = "Windows Credential Manager"

// Vrednost HKCU\Software\Microsoft\Windows\CurrentVersion\Run: zagon ob prijavi brez skrbniških pravic.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

var (
	procRegCreate = advapi32.NewProc("RegCreateKeyExW")
	procRegSet    = advapi32.NewProc("RegSetValueExW")
	procRegQuery  = advapi32.NewProc("RegQueryValueExW")
	procRegDelete = advapi32.NewProc("RegDeleteValueW")
	procRegClose  = advapi32.NewProc("RegCloseKey")
)

func runKeyOpen() (uintptr, error) { return regOpen(runKey) }

func regOpen(key string) (uintptr, error) {
	const hkcu, access = 0x80000001, 0x0002 | 0x0001 // KEY_SET_VALUE | KEY_QUERY_VALUE
	sub, _ := syscall.UTF16PtrFromString(key)
	var h uintptr
	if r, _, _ := procRegCreate.Call(hkcu, uintptr(unsafe.Pointer(sub)), 0, 0, 0, access, 0, uintptr(unsafe.Pointer(&h)), 0); r != 0 {
		return 0, syscall.Errno(r)
	}
	return h, nil
}

func regRunSet(name, value string) error {
	h, err := runKeyOpen()
	if err != nil {
		return err
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString(name)
	v, _ := syscall.UTF16FromString(value)
	if r, _, _ := procRegSet.Call(h, uintptr(unsafe.Pointer(n)), 0, 1 /* REG_SZ */, uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2)); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func regRunGet(name string) (string, error) {
	h, err := runKeyOpen()
	if err != nil {
		return "", err
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString(name)
	buf := make([]uint16, 2048)
	size := uint32(len(buf) * 2)
	if r, _, _ := procRegQuery.Call(h, uintptr(unsafe.Pointer(n)), 0, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return "", syscall.Errno(r)
	}
	return syscall.UTF16ToString(buf[:size/2]), nil
}

func regRunDel(name string) {
	h, err := runKeyOpen()
	if err != nil {
		return
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString(name)
	procRegDelete.Call(h, uintptr(unsafe.Pointer(n)))
}

// protectDir: zaščiten DACL samo za lastnika in SYSTEM (mapa nastavitev zunaj %AppData%, kjer ni privzeto zasebna).
// protectScratch (L2): kot mapa nastavitev (DACL le za uporabnika; tuj lastnik = zavrnitev).
func protectScratch(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return protectDir(dir)
}

func protectDir(dir string) error {
	var (
		pConvert = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
		pGetDacl = advapi32.NewProc("GetSecurityDescriptorDacl")
		pSetSec  = advapi32.NewProc("SetNamedSecurityInfoW")
		pGetSec  = advapi32.NewProc("GetNamedSecurityInfoW")
		pFree    = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
	)
	// SID trenutnega uporabnika iz žetona procesa (ne OW: lastnika lahko spremeni drug).
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return err
	}
	me, err := tu.User.Sid.String()
	if err != nil {
		return err
	}
	name, _ := syscall.UTF16PtrFromString(dir)
	const seFileObject, ownerInfo, daclInfo, protectedDacl = 1, 0x1, 0x4, 0x80000000
	var owner *syscall.SID
	var osd uintptr
	if r, _, _ := pGetSec.Call(uintptr(unsafe.Pointer(name)), seFileObject, ownerInfo, uintptr(unsafe.Pointer(&owner)), 0, 0, 0,
		uintptr(unsafe.Pointer(&osd))); r != 0 {
		return syscall.Errno(r)
	}
	ownerSID, _ := owner.String()
	pFree.Call(osd)
	if ownerSID != me {
		return errForeignOwner
	}
	var sd uintptr
	sddl, _ := syscall.UTF16PtrFromString("D:P(A;OICI;FA;;;" + me + ")(A;OICI;FA;;;SY)")
	if r, _, err := pConvert.Call(uintptr(unsafe.Pointer(sddl)), 1 /* SDDL_REVISION_1 */, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return err
	}
	defer pFree.Call(sd)
	var present, defaulted int32
	var dacl uintptr
	if r, _, err := pGetDacl.Call(sd, uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted))); r == 0 {
		return err
	}
	if r, _, _ := pSetSec.Call(uintptr(unsafe.Pointer(name)), seFileObject, daclInfo|protectedDacl, 0, 0, dacl, 0); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// listenControl: SO_EXCLUSIVEADDRUSE (= ^SO_REUSEADDR): drug proces ne more poslušati na istih vratih.
func listenControl(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, ^syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}

// knownConfigDir: RoamingAppData iz SHGetKnownFolderPath, ne iz %APPDATA% (ta je spremenljivka okolja).
func knownConfigDir() (string, error) {
	folderRoamingAppData := syscall.GUID{Data1: 0x3EB685DB, Data2: 0x65F9, Data3: 0x4CF6, Data4: [8]byte{0xA0, 0x3A, 0xE3, 0xEF, 0x65, 0x72, 0x9F, 0x3D}}
	var p *uint16
	r, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath").Call(uintptr(unsafe.Pointer(&folderRoamingAppData)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if r != 0 || p == nil {
		return "", syscall.Errno(r)
	}
	defer syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(p)))
	return syscall.UTF16ToString(unsafe.Slice(p, 32768)[:wcslen(p)]), nil
}

func wcslen(p *uint16) int {
	n := 0
	for s := unsafe.Slice(p, 32768); n < len(s) && s[n] != 0; n++ {
	}
	return n
}

// --- minigw serve install ---

const (
	trayMutexName = `Local\MiniGatewayRouter`
	trayClassName = "MiniGatewayTray"
)

var (
	user32dll  = syscall.NewLazyDLL("user32.dll")
	kernel32dl = syscall.NewLazyDLL("kernel32.dll")
)

func knownFolder(g syscall.GUID) string {
	var p *uint16
	r, _, _ := syscall.NewLazyDLL("shell32.dll").NewProc("SHGetKnownFolderPath").Call(uintptr(unsafe.Pointer(&g)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if r != 0 || p == nil {
		return ""
	}
	defer syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree").Call(uintptr(unsafe.Pointer(p)))
	return syscall.UTF16ToString(unsafe.Slice(p, 32768)[:wcslen(p)])
}

// knownUserProgramsDir: FOLDERID_UserProgramFiles (%LocalAppData%\Programs), sicer LocalAppData\Programs.
func knownUserProgramsDir() string {
	if d := knownFolder(syscall.GUID{Data1: 0x5CD7AEE2, Data2: 0x2219, Data3: 0x4A67, Data4: [8]byte{0xB8, 0x5D, 0x6C, 0x9C, 0xE1, 0x56, 0x60, 0xCB}}); d != "" {
		return d
	}
	if d := knownFolder(syscall.GUID{Data1: 0xF1B32785, Data2: 0x6FBA, Data3: 0x4FCF, Data4: [8]byte{0x9D, 0x55, 0x7B, 0x8E, 0x7F, 0x15, 0x70, 0x91}}); d != "" {
		return filepath.Join(d, "Programs")
	}
	return ""
}

func trayMutexExists() bool {
	name, _ := syscall.UTF16PtrFromString(trayMutexName)
	h, _, _ := kernel32dl.NewProc("OpenMutexW").Call(0x00100000 /* SYNCHRONIZE */, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false
	}
	syscall.CloseHandle(syscall.Handle(h))
	return true
}

// closeRunningTray: WM_CLOSE oknu pladnja, nato do 10 s čakanja, da mutex izgine.
func closeRunningTray() error {
	if !trayMutexExists() {
		return nil
	}
	class, _ := syscall.UTF16PtrFromString(trayClassName)
	if h, _, _ := user32dll.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(class)), 0); h != 0 {
		user32dll.NewProc("PostMessageW").Call(h, 0x0010 /* WM_CLOSE */, 0, 0)
	}
	for range 50 {
		if !trayMutexExists() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("the running Mini Gateway tray did not quit: quit it from its menu, then run minigw serve install again")
}

func startProcessDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func moveFileOnReboot(path string) error {
	p, _ := syscall.UTF16PtrFromString(path)
	if r, _, err := kernel32dl.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(p)), 0, 0x4 /* MOVEFILE_DELAY_UNTIL_REBOOT */); r == 0 {
		return err
	}
	return nil
}

// regEnvPathGet/Set: uporabniški PATH (HKCU\Environment\Path, REG_EXPAND_SZ); po spremembi WM_SETTINGCHANGE.
func regEnvPathGet() (string, error) {
	h, err := regOpen("Environment")
	if err != nil {
		return "", err
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString("Path")
	size := uint32(32768 * 2)
	for range 3 {
		buf := make([]uint16, size/2+1)
		r, _, _ := procRegQuery.Call(h, uintptr(unsafe.Pointer(n)), 0, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
		switch r {
		case 0:
			return syscall.UTF16ToString(buf[:size/2]), nil
		case 2: // ERROR_FILE_NOT_FOUND: še brez uporabniškega PATH
			return "", nil
		case 234: // ERROR_MORE_DATA: size je zdaj potrebna velikost
			continue
		default:
			return "", syscall.Errno(r)
		}
	}
	return "", errors.New("the user PATH keeps growing while it is read")
}

func regEnvPathSet(value string) error {
	h, err := regOpen("Environment")
	if err != nil {
		return err
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString("Path")
	v, _ := syscall.UTF16FromString(value)
	if r, _, _ := procRegSet.Call(h, uintptr(unsafe.Pointer(n)), 0, 2 /* REG_EXPAND_SZ */, uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2)); r != 0 {
		return syscall.Errno(r)
	}
	env, _ := syscall.UTF16PtrFromString("Environment")
	var res uintptr
	user32dll.NewProc("SendMessageTimeoutW").Call(0xffff /* HWND_BROADCAST */, 0x001A /* WM_SETTINGCHANGE */, 0,
		uintptr(unsafe.Pointer(env)), 0x0002 /* SMTO_ABORTIFHUNG */, 5000, uintptr(unsafe.Pointer(&res)))
	return nil
}
