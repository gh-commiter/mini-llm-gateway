package main

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procLookup (Windows): starši in imena iz enega posnetka Toolhelp32 (brez OpenProcess, tudi za procese drugih
// uporabnikov); proces, ki ga v posnetku ni (starš se je končal), konča verigo. Ukazna vrstica le za node.exe/bun.exe
// (NtQueryInformationProcess, ProcessCommandLineInformation).
func procLookup() procFunc {
	type entry struct {
		ppid int
		exe  string
	}
	procs := map[int]entry{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err == nil {
		defer windows.CloseHandle(snap)
		e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
		for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
			procs[int(e.ProcessID)] = entry{int(e.ParentProcessID), windows.UTF16ToString(e.ExeFile[:])}
		}
		err = nil
	}
	return func(pid int) (int, string, []string, error) {
		if err != nil {
			return 0, "", nil, err
		}
		e, ok := procs[pid]
		if !ok {
			return 0, "", nil, nil
		}
		if n := strings.ToLower(strings.TrimSuffix(e.exe, filepath.Ext(e.exe))); n != "node" && n != "bun" {
			return e.ppid, e.exe, nil, nil
		}
		args, cerr := commandLine(pid)
		return e.ppid, e.exe, args, cerr
	}
}

// commandLine: argumenti procesa pid (PROCESS_QUERY_LIMITED_INFORMATION, Windows 8.1+).
func commandLine(pid int) ([]string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 64<<10+64) // najdaljša ukazna vrstica (32767 znakov) in UNICODE_STRING
	var n uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), uint32(len(buf)), &n); err != nil {
		return nil, err
	}
	return windows.DecomposeCommandLine((*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String())
}
