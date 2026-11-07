package main

// Pladenj (minigw-tray.exe, Windows): router v istem procesu in ikona v območju za obvestila. Tu je del brez Win32:
// meni iz stanja (preizkusljiv), dnevnik z menjavo, zagon routerja. Win32 je v tray_windows.go.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// isTrayExe: minigw-tray.exe (ali minigw-tray-windows-amd64.exe) se zažene kot pladenj.
func isTrayExe(arg0 string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(arg0)), "minigw-tray")
}

// trayBeside: pladenj ob exe (za serve install), le z natančnim imenom; exe sam, če je že pladenj.
func trayBeside(exe string) string {
	if isTrayExe(exe) {
		return exe
	}
	for _, n := range []string{"minigw-tray.exe", "minigw-tray-windows-" + runtime.GOARCH + ".exe"} {
		if st, err := os.Stat(filepath.Join(filepath.Dir(exe), n)); err == nil && st.Mode().IsRegular() {
			return filepath.Join(filepath.Dir(exe), n)
		}
	}
	return ""
}

func trayCommand(args []string) {
	if len(args) > 0 && (args[0] == "tray" || args[0] == "serve") { // Run: "minigw-tray.exe" tray --config …
		args = args[1:]
	}
	fs := flag.NewFlagSet("minigw tray", flag.ExitOnError)
	cfgPath := fs.String("config", defaultServeConfigPath(), "config file")
	fs.Parse(args)
	trayRun(*cfgPath)
}

type trayItem struct {
	Text     string
	Action   string // "" = le besedilo (onemogočeno)
	Checked  bool
	Sep      bool
	Sub      []trayItem
	Disabled bool
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// trayMenu: meni pladnja iz stanja routerja (vse prek skrbniškega API).
func trayMenu(st apiStatus, pending []apiDevice, sl bool) []trayItem {
	tr := func(en, s string) string {
		if sl {
			return s
		}
		return en
	}
	status := fmt.Sprintf("Running on :%d · %s · %s", st.Port, plural(st.Devices, "device", "devices"), plural(len(st.Gateways), "gateway", "gateways"))
	if sl {
		status = fmt.Sprintf("Teče na :%d · naprav: %d · prehodov: %d", st.Port, st.Devices, len(st.Gateways))
	}
	items := []trayItem{{Text: status, Disabled: true}}
	if len(pending) > 0 {
		var sub []trayItem
		for _, d := range pending {
			name := d.Name
			if name == "" {
				name = "?"
			}
			sub = append(sub, trayItem{Text: clean(name) + " (" + d.ID + ")", Sub: []trayItem{
				{Text: tr("Allow", "Dovoli"), Action: "allow:" + d.ID},
				{Text: tr("Deny", "Zavrni"), Action: "deny:" + d.ID},
			}})
		}
		items = append(items, trayItem{Text: fmt.Sprintf("%s (%d)", tr("Waiting for approval", "Čaka na odobritev"), len(pending)), Sub: sub})
	}
	return append(items,
		trayItem{Sep: true},
		trayItem{Text: tr("Settings…", "Nastavitve …"), Action: "settings"},
		trayItem{Text: tr("Copy connect line", "Kopiraj vrstico za povezavo"), Action: "copy"},
		trayItem{Sep: true},
		trayItem{Text: tr("Start at sign-in", "Zaženi ob prijavi"), Action: "startup", Checked: st.Installed},
		trayItem{Text: tr("Open log", "Odpri dnevnik"), Action: "log"},
		trayItem{Sep: true},
		trayItem{Text: tr("Quit", "Končaj"), Action: "quit"},
	)
}

// trayState: stanje in čakajoče naprave iz skrbniškega API v istem procesu.
func (s *server) trayState() (apiStatus, []apiDevice) {
	var st apiStatus
	var d struct{ Pending []apiDevice }
	_, obj := s.adminDo("GET", "/status", nil)
	json.Unmarshal(must(json.Marshal(obj)), &st)
	_, obj = s.adminDo("GET", "/devices", nil)
	json.Unmarshal(must(json.Marshal(obj)), &d)
	return st, d.Pending
}

// trayAction: dejanja menija, ki ne potrebujejo Win32 (naprave). Vrne napako za sporočilo.
func (s *server) trayDevice(action string, pending []apiDevice) error {
	verb, id, _ := strings.Cut(action, ":")
	status, obj := 0, any(nil)
	switch verb {
	case "allow":
		name := ""
		for _, d := range pending {
			if d.ID == id {
				name = d.Name
			}
		}
		status, obj = s.adminDo("POST", "/devices", must(json.Marshal(apiDevice{ID: id, Name: name})))
	case "deny":
		status, obj = s.adminDo("DELETE", "/devices/"+url.PathEscape(id), nil)
	}
	if status >= 300 {
		b, _ := json.Marshal(obj)
		return errors.New(string(b))
	}
	return nil
}

// rotatingLog: dnevnik pladnja v mapi nastavitev (brez konzole); pri ~5 MB postane .1.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	n    int64
}

func openRotatingLog(path string) (*rotatingLog, error) {
	l := &rotatingLog{path: path}
	return l, l.open()
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	l.f, l.n = f, st.Size()
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n+int64(len(p)) > 5<<20 {
		l.f.Close()
		os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.n += int64(n)
	return n, err
}

// startTrayRouter: nastavitve (ob prvem zagonu nove), router z dnevnikom v datoteki in poslušanje.
func startTrayRouter(cfgPath string) (*server, net.Listener, string, bool, error) {
	c, err := loadServeConfig(cfgPath)
	fresh := errors.Is(err, os.ErrNotExist)
	if fresh {
		c = newServeConfig()
		if err = writeServeConfig(cfgPath, c); err == nil {
			c, err = loadServeConfig(cfgPath)
		}
	}
	if err != nil {
		return nil, nil, "", false, err
	}
	logPath := filepath.Join(filepath.Dir(cfgPath), "minigw-serve.log")
	if c.Log != "" {
		logPath = c.Log
	}
	rl, err := openRotatingLog(logPath)
	if err != nil {
		return nil, nil, "", false, err
	}
	c.Log = "" // newServer naj ne odpre še enkrat
	s, err := newServer(c, cfgPath)
	if err != nil {
		return nil, nil, "", false, err
	}
	s.log.SetOutput(rl)
	ln, err := listenRouter(c)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("port %d: %v (is minigw serve already running?)", c.Port, err)
	}
	s.announce(ln)
	return s, ln, logPath, fresh, nil
}
