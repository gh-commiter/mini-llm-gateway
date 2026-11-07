//go:build windows

package main

// Izbira mape ali programa za stran z nastavitvami: IFileOpenDialog prek COM (syscall in vtabela, brez cgo) na niti
// pladnja, v ospredju in v lasti njegovega okna. Za programe isti dialog s filtrom *.exe (namesto GetOpenFileNameW).

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	ole32                  = syscall.NewLazyDLL("ole32.dll")
	pCoInitializeEx        = ole32.NewProc("CoInitializeEx")
	pCoUninitialize        = ole32.NewProc("CoUninitialize")
	pCoCreateInstance      = ole32.NewProc("CoCreateInstance")
	pCoTaskMemFree         = ole32.NewProc("CoTaskMemFree")
	pGetForegroundWindow   = user32.NewProc("GetForegroundWindow")
	pGetWindowThreadProcId = user32.NewProc("GetWindowThreadProcessId")
	pAttachThreadInput     = user32.NewProc("AttachThreadInput")
	pGetCurrentThreadId    = kernel32.NewProc("GetCurrentThreadId")
	clsidFileOpenDialog    = syscall.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidFileOpenDialog      = syscall.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
)

// Vtabela IFileOpenDialog (IUnknown, IModalWindow, IFileDialog) in IShellItem.
const (
	vRelease, vShow, vSetFileTypes, vSetOptions, vGetOptions, vSetTitle, vGetResult = 2, 3, 4, 9, 10, 17, 20
	vGetDisplayName                                                                 = 5
	sigdnFileSysPath                                                                = 0x80058000
	fosPickFolders, fosForceFileSystem, fosPathMustExist, fosFileMustExist          = 0x20, 0x40, 0x800, 0x1000
	errCancelled                                                                    = 0x800704C7 // HRESULT_FROM_WIN32(ERROR_CANCELLED)
	wmPick                                                                          = 0x8000 + 2 // WM_APP + 2
)

// comCall: metoda i objekta COM (prvi kazalec objekta je vtabela). Argumenti, ki so kazalci, morajo biti na kopici
// in živi do konca klica (runtime.KeepAlive pri klicatelju).
func comCall(obj unsafe.Pointer, i int, args ...uintptr) uint32 {
	fn := *(*uintptr)(unsafe.Add(*(*unsafe.Pointer)(obj), i*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return uint32(r)
}

type pickReq struct {
	kind string
	done chan pickRes
}

type pickRes struct {
	path string
	err  error
}

// pick: s poljubne gorutine; okno teče na niti pladnja (trayProc, wmPick). Eno naenkrat zagotovi strežnik (409).
func (t *tray) pick(kind string) (string, error) {
	req := pickReq{kind, make(chan pickRes, 1)}
	select {
	case t.picks <- req:
	default:
		return "", fmt.Errorf("a file dialog is already open")
	}
	pPostMessage.Call(t.hwnd, wmPick, 0, 0)
	r := <-req.done
	return r.path, r.err
}

// onPick: na niti pladnja (iz trayProc).
func (t *tray) onPick() {
	select {
	case req := <-t.picks:
		title := t.tr("Choose a folder the agents may use", "Izberi mapo, ki jo agenti smejo uporabljati")
		if req.kind == "exe" {
			title = t.tr("Choose the program", "Izberi program")
		}
		p, err := fileDialog(t.hwnd, req.kind == "folder", title, t.tr("Programs", "Programi"))
		req.done <- pickRes{p, err}
	default:
	}
}

// toFront: okno pladnja v ospredje, da se dialog ne odpre za brskalnikom (ospredje sme menjati le nit, ki ima vhod
// ospredja: za ta čas se pridružimo njeni vrsti vhoda).
func toFront(hwnd uintptr) {
	fg, _, _ := pGetForegroundWindow.Call()
	ft, _, _ := pGetWindowThreadProcId.Call(fg, 0)
	me, _, _ := pGetCurrentThreadId.Call()
	if ft != 0 && ft != me {
		pAttachThreadInput.Call(ft, me, 1)
		defer pAttachThreadInput.Call(ft, me, 0)
	}
	pSetForegroundWindow.Call(hwnd)
}

// fileDialog: pot izbrane mape ali programa; "" ob preklicu.
func fileDialog(owner uintptr, folder bool, title, exeLabel string) (string, error) {
	if r, _, _ := pCoInitializeEx.Call(0, 2 /* COINIT_APARTMENTTHREADED */); int32(r) >= 0 {
		defer pCoUninitialize.Call()
	}
	d := new(unsafe.Pointer)
	if hr, _, _ := pCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, 1, /* CLSCTX_INPROC_SERVER */
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(d))); int32(hr) < 0 || *d == nil {
		return "", fmt.Errorf("CoCreateInstance(FileOpenDialog): 0x%08x", uint32(hr))
	}
	defer comCall(*d, vRelease)
	opts := new(uint32)
	comCall(*d, vGetOptions, uintptr(unsafe.Pointer(opts)))
	*opts |= fosForceFileSystem | fosPathMustExist
	spec := new([2]*uint16) // COMDLG_FILTERSPEC {pszName, pszSpec}
	if folder {
		*opts |= fosPickFolders
	} else {
		*opts |= fosFileMustExist
		spec[0], spec[1] = u16(exeLabel+" (*.exe)"), u16("*.exe")
		comCall(*d, vSetFileTypes, 1, uintptr(unsafe.Pointer(spec)))
	}
	comCall(*d, vSetOptions, uintptr(*opts))
	tp := u16(title)
	comCall(*d, vSetTitle, uintptr(unsafe.Pointer(tp)))
	toFront(owner)
	hr := comCall(*d, vShow, owner)
	runtime.KeepAlive(opts)
	runtime.KeepAlive(spec)
	runtime.KeepAlive(tp)
	if hr == errCancelled {
		return "", nil
	} else if int32(hr) < 0 {
		return "", fmt.Errorf("Show: 0x%08x", hr)
	}
	item := new(unsafe.Pointer)
	if hr := comCall(*d, vGetResult, uintptr(unsafe.Pointer(item))); int32(hr) < 0 || *item == nil {
		return "", fmt.Errorf("GetResult: 0x%08x", hr)
	}
	defer comCall(*item, vRelease)
	p := new(*uint16)
	if hr := comCall(*item, vGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(p))); int32(hr) < 0 || *p == nil {
		return "", fmt.Errorf("GetDisplayName: 0x%08x", hr)
	}
	defer pCoTaskMemFree.Call(uintptr(unsafe.Pointer(*p)))
	return syscall.UTF16ToString(unsafe.Slice(*p, wcslen(*p))), nil
}
