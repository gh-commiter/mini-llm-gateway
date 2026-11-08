//go:build windows

package main

// Pladenj: Win32 prek syscall (brez cgo in x/sys). Skrito okno prejema sporočila ikone in časovnika; meni se zgradi
// ob vsakem kliku iz stanja routerja (trayMenu). Navadno skrito okno namesto okna samo za sporočila: tako prejme
// TaskbarCreated (ikona se vrne po ponovnem zagonu Raziskovalca) in meni se pravilno zapre ob kliku drugam.

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

//go:embed tray.ico
var trayIcon []byte

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassEx     = user32.NewProc("RegisterClassExW")
	pCreateWindowEx      = user32.NewProc("CreateWindowExW")
	pDefWindowProc       = user32.NewProc("DefWindowProcW")
	pGetMessage          = user32.NewProc("GetMessageW")
	pTranslateMessage    = user32.NewProc("TranslateMessage")
	pDispatchMessage     = user32.NewProc("DispatchMessageW")
	pRegisterWindowMsg   = user32.NewProc("RegisterWindowMessageW")
	pCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	pAppendMenu          = user32.NewProc("AppendMenuW")
	pTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	pDestroyMenu         = user32.NewProc("DestroyMenu")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pGetCursorPos        = user32.NewProc("GetCursorPos")
	pPostMessage         = user32.NewProc("PostMessageW")
	pCreateIconFromRes   = user32.NewProc("CreateIconFromResourceEx")
	pGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	pMessageBox          = user32.NewProc("MessageBoxW")
	pEnumWindows         = user32.NewProc("EnumWindows")
	pGetWindowText       = user32.NewProc("GetWindowTextW")
	pIsWindowVisible     = user32.NewProc("IsWindowVisible")
	pIsIconic            = user32.NewProc("IsIconic")
	pShowWindow          = user32.NewProc("ShowWindow")
	pSetTimer            = user32.NewProc("SetTimer")
	pOpenClipboard       = user32.NewProc("OpenClipboard")
	pEmptyClipboard      = user32.NewProc("EmptyClipboard")
	pSetClipboardData    = user32.NewProc("SetClipboardData")
	pCloseClipboard      = user32.NewProc("CloseClipboard")
	pShellNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
	pShellExecute        = shell32.NewProc("ShellExecuteW")
	pCreateMutex         = kernel32.NewProc("CreateMutexW")
	pGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
	pGlobalAlloc         = kernel32.NewProc("GlobalAlloc")
	pGlobalLock          = kernel32.NewProc("GlobalLock")
	pGlobalUnlock        = kernel32.NewProc("GlobalUnlock")
	pGlobalFree          = kernel32.NewProc("GlobalFree")
	pRegisterClipFormat  = user32.NewProc("RegisterClipboardFormatW")
	pMoveMemory          = kernel32.NewProc("RtlMoveMemory")
	pGetUILanguage       = kernel32.NewProc("GetUserDefaultUILanguage")
)

const (
	wmNull, wmTimer, wmLButtonUp, wmRButtonUp = 0x0000, 0x0113, 0x0202, 0x0205
	wmTray                                    = 0x8000 + 1 // WM_APP + 1
	nimAdd, nimModify, nimDelete              = 0, 1, 2
	nifMessage, nifIcon, nifTip, nifInfo      = 0x1, 0x2, 0x4, 0x10
	mfGrayed, mfChecked, mfPopup, mfSep       = 0x1, 0x8, 0x10, 0x800
	tpmRightButton, tpmReturnCmd              = 0x2, 0x100
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

type notifyIconData struct {
	Size            uint32
	Hwnd            uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUID            [16]byte
	BalloonIcon     uintptr
}

type tray struct {
	s         *server
	cfgPath   string
	logPath   string
	sl        bool
	hwnd      uintptr
	icon      uintptr
	recreated uint32 // TaskbarCreated
	seen      map[string]bool
	pending   []apiDevice
	picks     chan pickReq // izbirno okno (picker_windows.go)
}

var theTray *tray // okenska procedura je ena za proces

func u16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func messageBox(text string) {
	pMessageBox.Call(0, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16("Mini Gateway"))), 0x10|0x10000|0x40000 /* MB_ICONERROR|MB_SETFOREGROUND|MB_TOPMOST: pred drugimi okni, tudi ob zagonu iz MSI */)
}

func shellOpen(target, params string) {
	p := uintptr(0)
	if params != "" {
		p = uintptr(unsafe.Pointer(u16(params)))
	}
	pShellExecute.Call(0, uintptr(unsafe.Pointer(u16("open"))), uintptr(unsafe.Pointer(u16(target))), p, 0, 1 /* SW_SHOWNORMAL */)
}

// regValue: privzeta vrednost (name "") ali imenovana vrednost niza; "" če je ni.
func regValue(root syscall.Handle, path, name string) string {
	var k syscall.Handle
	if syscall.RegOpenKeyEx(root, u16(path), 0, syscall.KEY_READ, &k) != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	var typ, n uint32
	var np *uint16
	if name != "" {
		np = u16(name)
	}
	if syscall.RegQueryValueEx(k, np, nil, &typ, nil, &n) != nil || n < 2 {
		return ""
	}
	buf := make([]uint16, n/2+1)
	n = uint32(len(buf) * 2)
	if syscall.RegQueryValueEx(k, np, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return ""
	}
	return syscall.UTF16ToString(buf)
}

// findWindowByTitle: prvo vidno okno najvišje ravni, katerega naslov se začne s prefix (zavihek brskalnika ima pripono).
// Povratni klic je eden za proces (syscall.NewCallback jih ima omejeno število); kliče se le z niti pladnja.
var (
	findPrefix string
	findFound  syscall.Handle
	findCB     = syscall.NewCallback(func(h, _ uintptr) uintptr {
		if v, _, _ := pIsWindowVisible.Call(h); v == 0 {
			return 1
		}
		buf := make([]uint16, 256)
		n, _, _ := pGetWindowText.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if strings.HasPrefix(syscall.UTF16ToString(buf[:n]), findPrefix) {
			findFound = syscall.Handle(h)
			return 0
		}
		return 1
	})
)

func findWindowByTitle(prefix string) syscall.Handle {
	findPrefix, findFound = prefix, 0
	pEnumWindows.Call(findCB, 0)
	return findFound
}

// openSettings: obstoječe okno nastavitev pride naprej; sicer lastno okno brskalnika (--app), nazadnje navadno odpiranje.
// URL nosi enkratni prijavni žeton, zato ni v dnevniku.
func openSettings(url string) {
	if h := findWindowByTitle("Mini Gateway Settings"); h != 0 {
		if v, _, _ := pIsIconic.Call(uintptr(h)); v != 0 {
			pShowWindow.Call(uintptr(h), 9 /* SW_RESTORE */)
		}
		pSetForegroundWindow.Call(uintptr(h))
		return
	}
	exe := ""
	if prog := regValue(syscall.HKEY_CURRENT_USER, `Software\Microsoft\Windows\Shell\Associations\UrlAssociations\http\UserChoice`, "ProgId"); prog != "" {
		exe = shellCommandExe(regValue(syscall.HKEY_CLASSES_ROOT, prog+`\shell\open\command`, ""))
	}
	if !supportsAppFlag(exe) {
		exe = regValue(syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`, "")
	}
	if supportsAppFlag(exe) && command(exe, "--app="+url).Start() == nil {
		return
	}
	shellOpen(url, "")
}

func trayRun(cfgPath string, quiet bool) {
	runtime.LockOSThread() // okno in zanka sporočil na eni niti
	if _, _, err := pCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(u16(trayMutexName)))); err == syscall.Errno(183) {
		if !quiet {
			messageBox("Mini Gateway is already running: see its icon in the notification area.")
		}
		return
	}
	s, ln, logPath, fresh, err := startTrayRouter(cfgPath)
	if err != nil {
		msg := "Mini Gateway cannot start: " + err.Error()
		if logPath != "" {
			msg += "\n\nAlso in the log " + logPath + ". Fix the problem (or delete " + cfgPath + " for new settings) and start Mini Gateway again."
		}
		messageBox(msg)
		os.Exit(1)
	}
	go func() { s.log.Printf("gateway stopped: %v", s.serve(ln)) }()
	lang, _, _ := pGetUILanguage.Call()
	t := &tray{s: s, cfgPath: cfgPath, logPath: logPath, sl: lang&0x3ff == 0x24, seen: map[string]bool{}, picks: make(chan pickReq, 1)}
	theTray = t
	if err := t.create(); err != nil {
		messageBox("Mini Gateway: " + err.Error())
		os.Exit(1)
	}
	pf := pickFunc(t.pick)
	s.pick.Store(&pf) // stran z nastavitvami: Izberi… in Prebrskaj…
	if fresh {
		t.balloon(t.tr("Mini Gateway is ready", "Mini Gateway je pripravljen"),
			t.tr("Copy the token from this icon's menu; on each client run /minigw:connect <this computer> <token>.", "V meniju te ikone kopiraj žeton; na vsakem odjemalcu zaženi /minigw:connect <ta računalnik> <žeton>."))
	}
	var m winMsg
	for {
		if r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (t *tray) tr(en, sl string) string {
	if t.sl {
		return sl
	}
	return en
}

func (t *tray) create() error {
	inst, _, _ := pGetModuleHandle.Call(0)
	wc := wndClassEx{WndProc: syscall.NewCallback(trayProc), Instance: inst, ClassName: u16(trayClassName)}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassEx: %v", err)
	}
	hwnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(wc.ClassName)), uintptr(unsafe.Pointer(u16("Mini Gateway"))), 0,
		0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx: %v", err)
	}
	t.hwnd = hwnd
	r, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	t.recreated = uint32(r)
	t.icon = loadTrayIcon()
	t.addIcon()
	pSetTimer.Call(hwnd, 1, 3000, 0) // čakajoče naprave in opis ikone
	t.poll()
	return nil
}

// loadTrayIcon: vnos .ico v velikosti male ikone (PNG v vnosu podpira CreateIconFromResourceEx).
func loadTrayIcon() uintptr {
	cx, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
	n := int(binary.LittleEndian.Uint16(trayIcon[4:]))
	best, bw := 0, 0 // najmanjša ≥ cx, sicer največja
	for i := 0; i < n; i++ {
		w := int(trayIcon[6+16*i])
		if w == 0 {
			w = 256
		}
		if (w >= int(cx) && (bw < int(cx) || w < bw)) || (bw < int(cx) && w > bw) {
			best, bw = i, w
		}
	}
	e := trayIcon[6+16*best:]
	size, off := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
	h, _, _ := pCreateIconFromRes.Call(uintptr(unsafe.Pointer(&trayIcon[off])), uintptr(size), 1, 0x00030000, cx, cx, 0)
	return h
}

func (t *tray) data(flags uint32) *notifyIconData {
	d := &notifyIconData{Hwnd: t.hwnd, ID: 1, Flags: flags, CallbackMessage: wmTray, Icon: t.icon}
	d.Size = uint32(unsafe.Sizeof(*d))
	return d
}

func (t *tray) addIcon() {
	d := t.data(nifMessage | nifIcon | nifTip)
	copy(d.Tip[:127], syscall.StringToUTF16("Mini Gateway"))
	pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(d)))
}

func (t *tray) tip(text string) {
	d := t.data(nifTip)
	u := syscall.StringToUTF16(text)
	copy(d.Tip[:127], u[:min(len(u), 127)])
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(d)))
}

func (t *tray) balloon(title, text string) {
	d := t.data(nifInfo)
	tu, xu := syscall.StringToUTF16(title), syscall.StringToUTF16(text)
	copy(d.InfoTitle[:63], tu[:min(len(tu), 63)])
	copy(d.Info[:255], xu[:min(len(xu), 255)])
	d.InfoFlags = 1 // NIIF_INFO
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(d)))
}

func trayProc(hwnd, m, wp, lp uintptr) uintptr {
	t := theTray
	switch {
	case t == nil:
	case m == wmTray && (lp == wmRButtonUp || lp == wmLButtonUp):
		t.menu()
		return 0
	case m == wmPick:
		t.onPick()
		return 0
	case m == wmTimer:
		t.poll()
		return 0
	case m == 0x0010: // WM_CLOSE: minigw serve install (posodobitev) ali uninstall zapre pladenj
		pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(t.data(0))))
		t.s.state.save()
		os.Exit(0)
	case uint32(m) == t.recreated && t.recreated != 0:
		t.addIcon()
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, m, wp, lp)
	return r
}

// poll: opis ikone in obvestilo za vsako novo čakajočo napravo.
func (t *tray) poll() {
	st, pending := t.s.trayState()
	t.pending = pending
	t.tip("Mini Gateway · " + trayMenu(st, nil, t.sl)[0].Text)
	for _, d := range pending {
		if !t.seen[d.ID] {
			t.seen[d.ID] = true
			t.balloon(t.tr("A device is waiting for approval", "Naprava čaka na odobritev"),
				clean(d.Name)+" ("+d.Address+"): "+t.tr("compare the ID with the client, then Allow it in this icon's menu.",
					"primerjaj ID z odjemalcem in jo nato dovoli v meniju te ikone."))
		}
	}
}

func (t *tray) menu() {
	st, pending := t.s.trayState()
	t.pending = pending
	actions := map[uintptr]string{}
	var build func(items []trayItem) uintptr
	build = func(items []trayItem) uintptr {
		h, _, _ := pCreatePopupMenu.Call()
		for _, it := range items {
			switch {
			case it.Sep:
				pAppendMenu.Call(h, mfSep, 0, 0)
			case it.Sub != nil:
				pAppendMenu.Call(h, mfPopup, build(it.Sub), uintptr(unsafe.Pointer(u16(it.Text))))
			default:
				id := uintptr(len(actions) + 1)
				actions[id] = it.Action
				flags := uintptr(0)
				if it.Disabled || it.Action == "" {
					flags |= mfGrayed
				}
				if it.Checked {
					flags |= mfChecked
				}
				pAppendMenu.Call(h, flags, id, uintptr(unsafe.Pointer(u16(it.Text))))
			}
		}
		return h
	}
	h := build(trayMenu(st, pending, t.sl))
	var pt struct{ X, Y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(h, tpmRightButton|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	pPostMessage.Call(t.hwnd, wmNull, 0, 0)
	pDestroyMenu.Call(h) // uniči tudi podmenije
	t.do(actions[cmd], st)
}

func (t *tray) do(action string, st apiStatus) {
	switch action {
	case "":
	case "settings":
		openSettings(t.s.settingsLoginURL())
	case "copy":
		if err := setClipboard(t.hwnd, connectToken(t.s.conf())); err != nil {
			messageBox(err.Error())
		}
	case "startup": // le vrednost Run za ta pladenj (celotna namestitev bi zaprla ta pladenj in čakala nase)
		if err := trayStartup(!st.Installed, t.cfgPath); err != nil {
			messageBox(err.Error())
		}
	case "log":
		shellOpen("notepad.exe", `"`+t.logPath+`"`) // dnevnik vedno kot besedilo, ne s privzetim programom za .log
	case "quit":
		pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(t.data(0))))
		t.s.state.save()
		os.Exit(0)
	default:
		if err := t.s.trayDevice(action, t.pending); err != nil {
			messageBox(err.Error())
		}
		t.poll()
	}
}

func setClipboard(hwnd uintptr, text string) error {
	u, _ := syscall.UTF16FromString(text)
	if r, _, err := pOpenClipboard.Call(hwnd); r == 0 {
		return fmt.Errorf("clipboard: %v", err)
	}
	defer pCloseClipboard.Call()
	pEmptyClipboard.Call()
	if err := clipboardData(13 /* CF_UNICODETEXT */, unsafe.Pointer(&u[0]), len(u)*2); err != nil {
		return err
	}
	// Vrstica vsebuje žeton: ne v zgodovino odložišča in ne v sinhronizacijo v oblak.
	var zero uint32
	for _, name := range []string{"ExcludeClipboardContentFromMonitorProcessing", "CanIncludeInClipboardHistory", "CanUploadToCloudClipboard"} {
		if f, _, _ := pRegisterClipFormat.Call(uintptr(unsafe.Pointer(u16(name)))); f != 0 {
			clipboardData(f, unsafe.Pointer(&zero), 4)
		}
	}
	return nil
}

// clipboardData: kopija v GlobalAlloc; ob napaki pomnilnik sprosti (po uspehu je last odložišča).
func clipboardData(format uintptr, data unsafe.Pointer, size int) error {
	h, _, _ := pGlobalAlloc.Call(0x0002 /* GMEM_MOVEABLE */, uintptr(size))
	if h == 0 {
		return fmt.Errorf("clipboard: no memory")
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		pGlobalFree.Call(h)
		return fmt.Errorf("clipboard: GlobalLock failed")
	}
	pMoveMemory.Call(p, uintptr(data), uintptr(size))
	pGlobalUnlock.Call(h)
	if r, _, err := pSetClipboardData.Call(format, h); r == 0 {
		pGlobalFree.Call(h)
		return fmt.Errorf("clipboard: %v", err)
	}
	return nil
}
