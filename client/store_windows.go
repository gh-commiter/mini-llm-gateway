//go:build windows

package main

// Povezava (naslov, žeton, ključ routerja, ključ naprave) v Windows Credential Manager (Upravitelj poverilnic),
// šifrirano z uporabnikovo prijavo. Brez zunanjih knjižnic: advapi32 neposredno.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
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

// Neveljavno ime (L1) nikoli ne doseže poverilnice: ».." ali »x:y« bi zadela drug cilj.
func storeLoad(name string) ([]byte, error) {
	if !validProfile(name) {
		return nil, errNotFound
	}
	return credLoad(credTargetFor(name))
}

func storeSave(name string, data []byte) error {
	if !validProfile(name) {
		return errors.New("invalid profile name")
	}
	return credSave(credTargetFor(name), data)
}

func storeDelete(name string) {
	if validProfile(name) {
		credDelete(credTargetFor(name))
	}
}

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

// Ključ naprave (clientDevice): svoja poverilnica minigw:device (zunaj filtra minigw:router*); disconnect je ne odstrani.
const deviceTarget = "minigw:device"

// deviceLoad: "" brez napake = ključa ni; druga napaka branja je napaka (L1).
func deviceLoad() (string, error) {
	b, err := credLoad(deviceTarget)
	if errors.Is(err, errNotFound) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

func deviceSave(d string) error { return credSave(deviceTarget, []byte(d)) }
func deviceDelete()             { credDelete(deviceTarget) }

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

// msiexecPath: msiexec.exe iz sistemske mape (GetSystemDirectoryW), ne iz PATH (L5).
func msiexecPath() string {
	buf := make([]uint16, 260)
	if n, _, _ := kernel32.NewProc("GetSystemDirectoryW").Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n > 0 && int(n) < len(buf) {
		return filepath.Join(syscall.UTF16ToString(buf[:n]), "msiexec.exe")
	}
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "msiexec.exe")
}

// msiPartInstalled: stanje dela (Feature) nameščenega MSI po Windows Installerju (MsiQueryFeatureStateW za izdelek z
// UpgradeCode iz minigw.wxs), ne po vrednostih v registru (Run ali Gateway lahko ostaneta). MSI 0.5.19 delov ni imel
// (vse, tudi prehod, je bilo vedno nameščeno): neznan del = nameščen. Brez izdelka MSI: false.
func msiPartInstalled(part string) bool {
	msi := syscall.NewLazyDLL("msi.dll")
	code, _ := syscall.UTF16PtrFromString(msiUpgradeCode)
	prod := make([]uint16, 39)
	if r, _, _ := msi.NewProc("MsiEnumRelatedProductsW").Call(uintptr(unsafe.Pointer(code)), 0, 0, uintptr(unsafe.Pointer(&prod[0]))); r != 0 {
		return false
	}
	f, _ := syscall.UTF16PtrFromString(part)
	st, _, _ := msi.NewProc("MsiQueryFeatureStateW").Call(uintptr(unsafe.Pointer(&prod[0])), uintptr(unsafe.Pointer(f)))
	switch int32(st) {
	case 3, 4, 5, -1: // INSTALLSTATE_LOCAL, SOURCE, DEFAULT; UNKNOWN (paket brez tega dela)
		return true
	}
	return false
}

// msiInstallDir: mapa namestitve MSI (HKCU\Software\Mini Gateway\InstallDir, zapiše jo minigw.wxs); "" brez MSI.
// Ključa ne ustvari (RegOpenKeyExW, ne RegCreateKeyExW).
func msiInstallDir() string {
	sub, _ := syscall.UTF16PtrFromString(`Software\Mini Gateway`)
	var h uintptr
	if r, _, _ := advapi32.NewProc("RegOpenKeyExW").Call(0x80000001, uintptr(unsafe.Pointer(sub)), 0, 0x0001 /* KEY_QUERY_VALUE */, uintptr(unsafe.Pointer(&h))); r != 0 {
		return ""
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString("InstallDir")
	buf := make([]uint16, 1024)
	size := uint32(len(buf) * 2)
	if r, _, _ := procRegQuery.Call(h, uintptr(unsafe.Pointer(n)), 0, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:size/2])
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
// L1, L1′: skupna nadrejena mapa <pogon>\minigw-scratch (tudi pod imenom 8.3) je prava mapa; naša (ali taka, na
// kateri imamo WRITE_DAC, npr. iz povišanega zagona) dobi DACL scratchParentSDDL (drugi uporabniki smejo v njej le
// ustvariti svojo mapo), tuja širša je zavrnjena (scratchParentOK). Vsaka mapa Scratch (v minigw-scratch katerokoli
// ime) mora biti prava mapa tega uporabnika; preverjena in zaščitena prek ročaja brez FILE_SHARE_DELETE, ki ostane
// odprt, dokler prehod teče (nihče, tudi lastnik nadrejene mape, je ne preimenuje in na njeno mesto podtakne
// junction). Ostanek: lastnik nadrejene mape lahko spremeni njen DACL.
func protectScratch(dir string) error {
	if p := filepath.Dir(dir); strings.EqualFold(filepath.Base(longPath(p)), "minigw-scratch") {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return err
		}
		var mine bool
		var verdict error
		ph, err := openDir(p, false, func(mode os.FileMode, owner, me string, dacl []scratchACE) error {
			if mine, verdict = owner == me, scratchParentOK(mode, owner, me, dacl); errors.Is(verdict, errUnprotected) {
				return nil // popravimo spodaj, če imamo WRITE_DAC
			}
			return verdict
		})
		if err != nil {
			return fmt.Errorf("%s %w", p, err)
		}
		defer windows.CloseHandle(ph)
		if mine || verdict != nil { // tudi selitev mape 0.5.23 (podedovan ACL pogona)
			if err := protectHandle(ph, scratchParentSDDL); verdict != nil && err != nil {
				return fmt.Errorf("%s %w", p, verdict)
			} else if err != nil {
				return fmt.Errorf("%s: %v", p, err)
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	h, err := openDir(dir, true, func(mode os.FileMode, owner, me string, _ []scratchACE) error { return scratchDirOK(mode, owner, me) })
	if err != nil {
		if errors.Is(err, errForeignOwner) { // ni dostopa: mapa drugega uporabnika
			err = errScratchTaken
		}
		return fmt.Errorf("%s %w", dir, err)
	}
	if err := protectDir(dir); err != nil { // po imenu (zaščita se razširi na vsebino); ročaj drži prav to mapo
		windows.CloseHandle(h)
		return err
	}
	scratchHold.Lock()
	if scratchHold.h != 0 {
		windows.CloseHandle(scratchHold.h)
	}
	scratchHold.h = h
	scratchHold.Unlock()
	return nil
}

// scratchParentSDDL: lastnik (ta uporabnik), SYSTEM in Administrators vse; Authenticated Users le prehod, seznam in
// nova podmapa v tej mapi; CREATOR OWNER vse na podmapah (vsaka mapa prehoda ima še svoj zaščiten DACL).
const scratchParentSDDL = "D:P(A;;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x1200ad;;;AU)(A;OICIIO;FA;;;CO)"

var errScratchNoHold = errors.New("cannot be held open (no list access for this user): set \"scratch\" to a folder of your own")

// scratchHold: ročaj trenutne mape Scratch (openDir: s seznamom, brez FILE_SHARE_DELETE), odprt, dokler prehod teče.
var scratchHold struct {
	sync.Mutex
	h windows.Handle
}

// releaseScratch: zapre ročaj (premik Scratch: preimenovanje in brisanje rabita deljenje brisanja).
func releaseScratch() {
	scratchHold.Lock()
	defer scratchHold.Unlock()
	if scratchHold.h != 0 {
		windows.CloseHandle(scratchHold.h)
		scratchHold.h = 0
	}
}

// lockScratch (N4): zaklep mape Scratch je ročaj iz protectScratch (brez FILE_SHARE_DELETE).
func lockScratch(string) error { return nil }

// oNoBlock: na Windows ni cevi v mapi (datoteka lastnika se preveri s Stat).
const oNoBlock = 0

// scratchFolderMine (N4): prava mapa tega uporabnika, preverjena prek ročaja (kot protectScratch).
func scratchFolderMine(dir string) bool {
	h, err := openDir(dir, false, func(mode os.FileMode, owner, me string, _ []scratchACE) error { return scratchDirOK(mode, owner, me) })
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

// scratchLocked: drug prehod (ali program z mapo kot trenutno) drži ročaj mape dir s podatkovnim dostopom brez deljenja
// brisanja: odpiranje z DELETE je kršitev deljenja. Druge napake (npr. ni dostopa) niso zaklep.
func scratchLocked(dir string) bool {
	u, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return false
	}
	h, err := windows.CreateFile(u, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
	}
	windows.CloseHandle(h)
	return false
}

// openDir: ročaj mape p (brez sledenja povezavi, brez FILE_SHARE_DELETE, z WRITE_DAC, če ga dobimo) in odločitev ok iz
// atributov, lastnika in DACL tega ročaja. Brez dostopa: errForeignOwner (mapa drugega uporabnika).
// Ročaj ima tudi FILE_LIST_DIRECTORY|FILE_TRAVERSE: NTFS deljenje uveljavi le proti ročajem s podatkovnim dostopom ali
// DELETE, ročaj samo z READ_CONTROL/WRITE_DAC ne prepreči poznejšega odpiranja z DELETE (preimenovanje, HPX360). Drugo
// odpiranje iste mape (tudi naše, med držanjem) mora zato deliti READ|WRITE in ne sme zahtevati DELETE (kot tu).
// hold: ročaj bo zaklep (mapa Scratch); brez seznama ga ni, zato errScratchNoHold namesto ročaja, ki ne drži.
func openDir(p string, hold bool, ok func(mode os.FileMode, owner, me string, dacl []scratchACE) error) (windows.Handle, error) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	open := func(access uint32) (windows.Handle, error) {
		return windows.CreateFile(u, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	}
	const held = windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.READ_CONTROL
	h, err := open(held | windows.WRITE_DAC)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		h, err = open(held)
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) { // le odločitev (npr. tuja mapa brez seznama); tak ročaj ne drži
		if h, err = open(windows.READ_CONTROL); err == nil && hold {
			windows.CloseHandle(h)
			return 0, errScratchNoHold
		}
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return 0, errForeignOwner
	} else if err != nil {
		return 0, err
	}
	if err := handleVerdict(h, ok); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

func handleVerdict(h windows.Handle, ok func(mode os.FileMode, owner, me string, dacl []scratchACE) error) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	var mode os.FileMode
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		mode = os.ModeDir
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		mode |= os.ModeIrregular
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	var aces []scratchACE
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil { // brez DACL: vsi smejo vse
		aces = []scratchACE{{0, 0, 0x10000000, "S-1-1-0"}}
	} else {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var a *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &a); err != nil {
				return err
			}
			sid := ""
			if a.Header.AceType == 0 || a.Header.AceType == 1 { // dovoli, zavrni: SID takoj za masko
				sid = (*windows.SID)(unsafe.Pointer(&a.SidStart)).String()
			}
			aces = append(aces, scratchACE{a.Header.AceType, a.Header.AceFlags, uint32(a.Mask), sid})
		}
	}
	return ok(mode, owner.String(), tu.User.Sid.String(), aces)
}

// protectHandle: zaščiten DACL iz sddl (%s = ta uporabnik) na odprtem ročaju.
func protectHandle(h windows.Handle, sddl string) error {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf(sddl, tu.User.Sid.String()))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// longPath: dolga oblika obstoječe poti (ime 8.3, npr. MINIGW~1, bi sicer obšlo primerjavo imena); sicer p.
func longPath(p string) string {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, 1024)
	if n, err := windows.GetLongPathName(u, &buf[0], uint32(len(buf))); err == nil && n > 0 && int(n) < len(buf) {
		return windows.UTF16ToString(buf[:n])
	}
	return p
}

func protectDir(dir string) error {
	var (
		pConvert = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
		pGetDacl = advapi32.NewProc("GetSecurityDescriptorDacl")
		pSetSec  = advapi32.NewProc("SetNamedSecurityInfoW")
		pFree    = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
	)
	ownerSID, me, err := ownerAndMe(dir)
	if err != nil {
		return err
	}
	if ownerSID != me {
		return errForeignOwner
	}
	name, _ := syscall.UTF16PtrFromString(dir)
	const seFileObject, daclInfo, protectedDacl = 1, 0x4, 0x80000000
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
func closeRunningTray() error { return closeTray(50) }

// quitRunningTray: minigw-tray.exe quit (MSI pred odstranitvijo, brez konzole): WM_CLOSE, 5 s, nato konec
// drugih procesov z natančnim imenom minigw-tray.exe v tej seji Windows (L1: ne drugih programov ali uporabnikov).
func quitRunningTray() {
	// Oznaka za tray --if-was-running (nadgradnja, popravilo): pladenj teče, ali dela Gateway še ni (le odjemalec: novi
	// Gateway ga zažene); sicer je ni.
	if !trayMutexExists() {
		if msiPartInstalled("Gateway") {
			os.Remove(trayRestartMarker())
		} else {
			os.WriteFile(trayRestartMarker(), nil, 0o600)
		}
		return
	}
	os.WriteFile(trayRestartMarker(), nil, 0o600)
	if closeTray(25) == nil {
		return
	}
	sessionOf := processSession
	own, ok := sessionOf(uint32(os.Getpid()))
	if !ok {
		return
	}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(snap)
	e := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err := syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if int(e.ProcessID) == os.Getpid() || !strings.EqualFold(syscall.UTF16ToString(e.ExeFile[:]), "minigw-tray.exe") {
			continue
		}
		if s, ok := sessionOf(e.ProcessID); !ok || s != own {
			continue
		}
		if h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, e.ProcessID); err == nil {
			syscall.TerminateProcess(h, 1)
			syscall.CloseHandle(h)
		}
	}
}

// msiStopProcesses (MSI StopProcesses, pred InstallValidate): ustavi preostale procese iz mape namestitve v tej seji
// (odjemalci minigw.exe, minigw-tui.exe), da nobena datoteka ni v rabi (brez FilesInUse in ponovnega zagona): WM_CLOSE
// njihovim oknom, do 5 s, nato TerminateProcess in čakanje na konec. Izbor je stopTargets (serve_tray.go).
func msiStopProcesses(dir string, names []string) {
	own, ok := processSession(uint32(os.Getpid()))
	if !ok {
		return
	}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	// L2: vsak proces le enkrat odprt (poizvedba, konec, čakanje); pot programa iz tega ročaja, ne iz PID (ponovna raba).
	var ps []procEntry
	hs := map[uint32]syscall.Handle{}
	q := kernel32dl.NewProc("QueryFullProcessImageNameW")
	e := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err := syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		s, ok := processSession(e.ProcessID)
		if !ok || s != own {
			continue
		}
		h, err := syscall.OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */ |syscall.PROCESS_TERMINATE|syscall.SYNCHRONIZE, false, e.ProcessID)
		if err != nil {
			continue
		}
		buf := make([]uint16, 1024)
		n := uint32(len(buf))
		if r, _, _ := q.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
			syscall.CloseHandle(h)
			continue
		}
		ps = append(ps, procEntry{PID: e.ProcessID, Session: s, Image: syscall.UTF16ToString(buf[:n])})
		hs[e.ProcessID] = h
	}
	syscall.CloseHandle(snap)
	want := map[uint32]bool{}
	for _, p := range stopTargets(ps, dir, uint32(os.Getpid()), own, names) {
		want[p] = true
	}
	for pid, h := range hs {
		if !want[pid] {
			syscall.CloseHandle(h)
			delete(hs, pid)
		}
	}
	if len(hs) == 0 {
		return
	}
	post := user32dll.NewProc("PostMessageW")
	owner := user32dll.NewProc("GetWindowThreadProcessId")
	user32dll.NewProc("EnumWindows").Call(syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		var pid uint32
		owner.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if want[pid] {
			post.Call(hwnd, 0x0010 /* WM_CLOSE */, 0, 0)
		}
		return 1
	}), 0)
	end := time.Now().Add(5 * time.Second)
	for _, h := range hs {
		left := max(time.Until(end), 0)
		if ev, _ := syscall.WaitForSingleObject(h, uint32(left/time.Millisecond)); ev != syscall.WAIT_OBJECT_0 {
			syscall.TerminateProcess(h, 1)
			syscall.WaitForSingleObject(h, 5000) // datoteka je prosta šele, ko proces res konča
		}
		syscall.CloseHandle(h)
	}
}

func processSession(pid uint32) (uint32, bool) {
	var s uint32
	r, _, _ := kernel32dl.NewProc("ProcessIdToSessionId").Call(uintptr(pid), uintptr(unsafe.Pointer(&s)))
	return s, r != 0
}

func closeTray(waits int) error {
	if !trayMutexExists() {
		return nil
	}
	class, _ := syscall.UTF16PtrFromString(trayClassName)
	if h, _, _ := user32dll.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(class)), 0); h != 0 {
		user32dll.NewProc("PostMessageW").Call(h, 0x0010 /* WM_CLOSE */, 0, 0)
	}
	for range waits {
		if !trayMutexExists() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("the running Mini Gateway tray did not quit: quit it from its menu, then run minigw serve install again")
}

func startProcessDetached(exe string, args ...string) error {
	cmd := command(exe, args...)
	cmd.SysProcAttr.CreationFlags |= 0x00000008 | 0x00000200 // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
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

// ownerAndMe: SID lastnika path in SID trenutnega uporabnika iz žetona procesa (ne OW: lastnika lahko spremeni drug).
func ownerAndMe(path string) (owner, me string, err error) {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", "", err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", "", err
	}
	if me, err = tu.User.Sid.String(); err != nil {
		return "", "", err
	}
	name, _ := syscall.UTF16PtrFromString(path)
	var sid *syscall.SID
	var osd uintptr
	if r, _, _ := advapi32.NewProc("GetNamedSecurityInfoW").Call(uintptr(unsafe.Pointer(name)), 1 /* SE_FILE_OBJECT */, 0x1, /* OWNER */
		uintptr(unsafe.Pointer(&sid)), 0, 0, 0, uintptr(unsafe.Pointer(&osd))); r != 0 {
		return "", "", syscall.Errno(r)
	}
	defer syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree").Call(osd)
	owner, err = sid.String()
	return owner, me, err
}

// ownedByMe (L2, M1): prava mapa (lstat; ne junction, symlink ali druga točka ponovne razčlenitve), katere lastnik je
// uporabnik žetona tega procesa (isti SID kot protectDir).
func ownedByMe(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return false
	}
	owner, me, err := ownerAndMe(dir)
	return err == nil && owner == me
}

// shortPath: kratka pot 8.3 (minigw.cmd: ne-ASCII pot v kodni strani cmd); brez nje dolga.
func shortPath(p string) string {
	long, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, 1024)
	if n, err := syscall.GetShortPathName(long, &buf[0], uint32(len(buf))); err == nil && n > 0 && int(n) < len(buf) {
		return syscall.UTF16ToString(buf[:n])
	}
	return p
}

func safeDir(string) bool  { return true } // Windows: pravice ACL, ne bitov načina
func safePath(string) bool { return true }
