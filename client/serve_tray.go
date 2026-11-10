package main

// Pladenj (minigw-tray.exe, Windows): router v istem procesu in ikona v območju za obvestila. Tu je del brez Win32:
// meni iz stanja (preizkusljiv), dnevnik z menjavo, zagon routerja. Win32 je v tray_windows.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// isTrayExe: minigw-tray.exe (ali minigw-tray-windows-<arch>.exe) se zažene kot pladenj; le natančni imeni (L1).
func isTrayExe(arg0 string) bool {
	n := filepath.Base(arg0)
	return strings.EqualFold(n, "minigw-tray.exe") || strings.EqualFold(n, "minigw-tray-windows-"+runtime.GOARCH+".exe")
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
	if len(args) > 0 && args[0] == "quit" { // MSI: zapre tekoči pladenj, sam brez okna (-H=windowsgui)
		// Nadgradnja/odstranitev ne prekine zahtev agentov: počaka, da končajo, največ 120 s; nato še 3 s, da odjemalec
		// (minigw.exe, ki ga MSI potem ustavi: StopProcesses) prejme in izpiše rezultat.
		waited := false
		for end := time.Now().Add(120 * time.Second); time.Now().Before(end) && gatewayAgents(defaultServeConfigPath()) > 0; {
			time.Sleep(2 * time.Second)
			waited = true
		}
		if waited {
			time.Sleep(3 * time.Second)
		}
		quitRunningTray()
		return
	}
	// MSI (StopProcesses, pred InstallValidate; zažene ga kopija iz tabele Binary, zato mapa kot argument): preostali
	// procesi iz mape namestitve končajo, preden Windows Installer preveri datoteke v rabi (brez ponovnega zagona).
	if len(args) > 2 && args[0] == "msi" && args[1] == "stop" {
		msiStop(args[2], args[3:])
		return
	}
	// MSI (RemoveRun, odložen, po RemoveRegistryValues): vrednost Run, ki kaže na pladenj iz te mape, ob odstranitvi
	// dela Gateway ali celote izgine, tudi če jo je zapisal pladenj (Zaženi ob prijavi) ali minigw gw autostart.
	if len(args) == 2 && args[0] == "msi" && args[1] == "run-off" {
		if exe, err := os.Executable(); err == nil {
			if v, _ := runValueGet(runValueName); runValueIn(v, filepath.Dir(exe)) {
				runValueDel(runValueName)
			}
		}
		return
	}
	// MSI (client/minigw.wxs): minigw.exe plugin …|uninstall … brez okna konzole (msiexec bi ga pokazal), največ 60 s;
	// izpis v %LOCALAPPDATA%\minigw\minigw-msi.log (brez žetonov: le poti in imena profilov).
	if len(args) > 1 && args[0] == "msi" && (args[1] == "plugin" || args[1] == "uninstall") {
		exe, err := os.Executable()
		if err != nil {
			return
		}
		// Izpis najprej v pomnilnik: minigw uninstall med tekom briše %LOCALAPPDATA%\minigw (odprta datoteka bi to ustavila).
		var log bytes.Buffer
		fmt.Fprintln(&log, time.Now().Format(time.RFC3339)+" minigw "+strings.Join(args[1:], " "))
		if slices.Equal(args[1:], []string{"plugin", "install"}) && findClaudeFn() == "" && errFromStat(claudeDir()) != nil {
			fmt.Fprintln(&log, "Claude Code not found (no claude command, no "+claudeDir()+"): plugin not added; later: minigw plugin install")
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := commandContext(ctx, filepath.Join(filepath.Dir(exe), "minigw.exe"), args[1:]...)
			cmd.Stdout, cmd.Stderr = &log, &log
			if err := cmd.Run(); err != nil {
				fmt.Fprintln(&log, "minigw: "+err.Error())
			}
		}
		if slices.Contains(args, "--purge") { // po purge ne ustvarimo mape znova
			return
		}
		if d, err := os.UserCacheDir(); err == nil && os.MkdirAll(filepath.Join(d, "minigw"), 0o700) == nil {
			if f, err := os.OpenFile(filepath.Join(d, "minigw", "minigw-msi.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				f.Write(log.Bytes())
				f.Close()
			}
		}
		return
	}
	if len(args) > 0 && (args[0] == "tray" || args[0] == "serve") { // Run: "minigw-tray.exe" tray --config …
		args = args[1:]
	}
	fs := flag.NewFlagSet("minigw tray", flag.ExitOnError)
	cfgPath := fs.String("config", defaultServeConfigPath(), "config file")
	// MSI (StartTray): če pladenj že teče (ista različica znova), konec brez sporočila; ročni zagon ga pokaže. Sicer ga
	// znova zažene Raziskovalec (trayRun): okolje msiexec nima uporabnikovega trenutnega PATH.
	quiet := fs.Bool("quiet-if-running", false, "installer start: exit silently if running, else restart via Explorer")
	// MSI-nadgradnja: kot quiet-if-running, a le če je quit novega paketa zapisal oznako (pladenj je tekel).
	wasRunning := fs.Bool("if-was-running", false, "installer upgrade: start only if the tray ran before the upgrade")
	fs.Parse(args)
	if *wasRunning && !takeRestartMarker(filepath.Dir(defaultServeConfigPath())) {
		return
	}
	trayRun(*cfgPath, *quiet || *wasRunning)
}

// runValueIn: ukaz Run zažene minigw-tray.exe iz mape dir (Windowsove poti, brez razlike v velikih črkah).
func runValueIn(v, dir string) bool {
	return dir != "" && strings.HasPrefix(strings.ToLower(v), strings.ToLower(`"`+strings.TrimRight(dir, `\/`)+`\minigw-tray.exe"`))
}

// procEntry: proces za msi stop (ID, seja Windows, polna pot programa).
type procEntry struct {
	PID, Session uint32
	Image        string
}

// msiStop: msi stop (testi ga zamenjajo).
var msiStop = msiStopProcesses

// stopTargets: procesi, ki jih msi stop ustavi: program leži neposredno v mapi dir (ne v podmapi, ne drugje), ista
// seja Windows kot msi stop, ne on sam, in je naš (minigw.exe, minigw-tray.exe, minigw-tui.exe; z names le ti; L1:
// INSTALLDIR je javna lastnost). Prazna mapa ali koren pogona (C:\): nič.
func stopTargets(ps []procEntry, dir string, self, session uint32, names []string) []uint32 {
	dir = strings.TrimRight(strings.Trim(dir, `"`), `\/`)
	if !strings.ContainsAny(dir, `\/`) {
		return nil
	}
	if len(names) == 0 {
		names = []string{"minigw.exe", "minigw-tray.exe", "minigw-tui.exe"}
	}
	var out []uint32
	for _, p := range ps {
		i := strings.LastIndexAny(p.Image, `\/`)
		if i < 0 || p.PID == self || p.Session != session || !strings.EqualFold(p.Image[:i], dir) {
			continue
		}
		if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, p.Image[i+1:]) }) {
			out = append(out, p.PID)
		}
	}
	return out
}

// gatewayAgents: zahteve agentov, ki tečejo v prehodu s to nastavitvijo (skrbniški API /status, kot minigw gw status);
// -1, ko prehod ne teče ali ni dosegljiv.
func gatewayAgents(cfgPath string) int {
	c, err := loadServeConfig(cfgPath)
	if err != nil {
		return -1
	}
	k, err := c.privateKey()
	b, err2 := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), "admin.secret"))
	if err != nil || err2 != nil {
		return -1
	}
	a := &adminClient{base: "http://127.0.0.1:" + strconv.Itoa(c.Port) + adminPrefix, secret: strings.TrimSpace(string(b)), routerKey: k.PublicKey().Bytes()}
	var st apiStatus
	if code, resp, err := a.http("GET", "/status", nil, 3*time.Second); err != nil || code != 200 || json.Unmarshal(resp, &st) != nil {
		return -1
	}
	return st.Agents
}

// trayRestartMarker: quit (MSI CloseTray) ga zapiše, ko naj pladenj po nadgradnji spet teče; tray --if-was-running
// ga pobere. Pozitivna oznaka: quit starega paketa (med RemoveExistingProducts, ko pladenj že ne teče) je zapisal
// negativno »tray-not-running« in s tem preprečil ponovni zagon (poročilo 85334c0, 4.1).
func trayRestartMarker() string {
	return filepath.Join(filepath.Dir(defaultServeConfigPath()), "tray-restart")
}

// takeRestartMarker: pobere oznako (true = zaženi) in staro negativno oznako starejših paketov.
func takeRestartMarker(dir string) bool {
	os.Remove(filepath.Join(dir, "tray-not-running"))
	return os.Remove(filepath.Join(dir, "tray-restart")) == nil
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

// trayStartup: »Zaženi ob prijavi« v meniju pladnja: le vrednost Run za ta exe (brez kopiranja in zapiranja pladnja).
func trayStartup(on bool, cfgPath string) error {
	if !on {
		runValueDel(runValueName)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfgPath, _ = filepath.Abs(cfgPath)
	if trayBeside(exe) == "" { // brez pladnja ne minigw.exe ob prijavi (okno konzole); kot installWindows
		return errors.New("minigw-tray.exe is not next to " + filepath.Base(exe) + ": put minigw-tray.exe next to minigw.exe and run minigw serve install again")
	}
	v, err := serviceFile("windows", exe, cfgPath)
	if err != nil {
		return err
	}
	return runValueSet(runValueName, v)
}

// trayTip: opis ikone; omeni čakajoče naprave.
func trayTip(st apiStatus, waiting int, sl bool) string {
	tip := "Mini Gateway · " + trayMenu(st, nil, sl)[0].Text
	switch {
	case waiting == 0:
	case sl:
		tip += fmt.Sprintf(" · na odobritev čaka naprav: %d", waiting)
	default:
		tip += " · " + plural(waiting, "device", "devices") + " waiting for approval"
	}
	return tip
}

// trayBadge: ikona (PNG) z oranžno piko spodaj desno in prozornim robom okoli nje (kot značka na Macu).
func trayBadge(b []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	r := src.Bounds()
	img := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	w := float64(r.Dx())
	rad := w * 0.22                      // polmer pike
	cx, cy := w-rad-w*0.02, w-rad-w*0.02 // središče
	ring := max(1, w/16)
	const ss = 4 // glajenje robov: 4×4 vzorcev na piko
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			c := color.NRGBAModel.Convert(src.At(r.Min.X+x, r.Min.Y+y)).(color.NRGBA)
			dot, cut := 0.0, 0.0
			for i := 0; i < ss*ss; i++ {
				dx, dy := float64(x)+(float64(i%ss)+0.5)/ss-cx, float64(y)+(float64(i/ss)+0.5)/ss-cy
				d := dx*dx + dy*dy
				if d <= rad*rad {
					dot++
				} else if d <= (rad+ring)*(rad+ring) {
					cut++
				}
			}
			dot, cut = dot/(ss*ss), cut/(ss*ss)
			a := float64(c.A) * (1 - dot - cut)
			o := color.NRGBA{R: 0xFF, G: 0x95, B: 0x00} // oranžna (systemOrange)
			if na := a + 255*dot; na > 0 {
				mix := func(v, ov uint8) uint8 { return uint8((float64(v)*a + float64(ov)*255*dot) / na) }
				o = color.NRGBA{R: mix(c.R, o.R), G: mix(c.G, o.G), B: mix(c.B, o.B), A: uint8(na + 0.5)}
			} else {
				o.A = 0
			}
			img.SetNRGBA(x, y, o)
		}
	}
	var out bytes.Buffer
	err = png.Encode(&out, img)
	return out.Bytes(), err
}

// trayMenu: meni pladnja iz stanja routerja (vse prek skrbniškega API).
func trayMenu(st apiStatus, pending []apiDevice, sl bool) []trayItem {
	tr := func(en, s string) string {
		if sl {
			return s
		}
		return en
	}
	status := fmt.Sprintf("Running on :%d · %s · %s", st.Port, plural(st.Devices, "device", "devices"), plural(len(st.Gateways), "provider", "providers"))
	if sl {
		status = fmt.Sprintf("Teče na :%d · naprav: %d · ponudnikov: %d", st.Port, st.Devices, len(st.Gateways))
	}
	items := []trayItem{{Text: status, Disabled: true}}
	if len(pending) > 0 {
		var sub []trayItem
		for _, d := range pending {
			name := d.Name
			if name == "" {
				name = "?"
			}
			text := clean(name) + " (" + d.ID + ")"
			if d.Client != "" {
				text += " · " + clean(d.Client)
			}
			sub = append(sub, trayItem{Text: text, Sub: []trayItem{
				{Text: tr("Allow", "Dovoli"), Action: "allow:" + d.ID},
				{Text: tr("Reject", "Zavrni"), Action: "deny:" + d.ID},
			}})
		}
		items = append(items, trayItem{Text: fmt.Sprintf("%s (%d)", tr("Waiting for approval", "Čaka na odobritev"), len(pending)), Sub: sub})
	}
	return append(items,
		trayItem{Sep: true},
		trayItem{Text: tr("Settings…", "Nastavitve …"), Action: "settings"},
		trayItem{Text: tr("Copy Token", "Kopiraj žeton"), Action: "copy"},
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
	var d struct{ Devices, Pending []apiDevice }
	_, obj := s.adminDo("GET", "/status", nil)
	json.Unmarshal(must(json.Marshal(obj)), &st)
	_, obj = s.adminDo("GET", "/devices", nil)
	json.Unmarshal(must(json.Marshal(obj)), &d)
	for i, p := range d.Pending { // isto ime kot dovoljena naprava: opozorilo (ime je le oznaka, primerjaj ID)
		if p.Name != "" && slices.ContainsFunc(d.Devices, func(x apiDevice) bool { return strings.EqualFold(x.Name, p.Name) }) {
			d.Pending[i].Name = "⚠ " + p.Name
		}
	}
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

// startTrayRouter: nastavitve (ob prvem zagonu nove), router z dnevnikom v datoteki in poslušanje. Napaka gre tudi v
// dnevnik (npr. neveljaven serve.json: pladenj nima konzole).
func startTrayRouter(cfgPath string) (s *server, ln net.Listener, logPath string, fresh bool, err error) {
	c, err := loadServeConfig(cfgPath)
	fresh = errors.Is(err, os.ErrNotExist)
	if fresh {
		c = newServeConfig()
		if err = writeServeConfig(cfgPath, c); err == nil {
			c, err = loadServeConfig(cfgPath)
		}
	}
	logPath = filepath.Join(filepath.Dir(cfgPath), "minigw-serve.log")
	if err == nil && c.Log != "" {
		logPath = c.Log
	}
	rl, lerr := openRotatingLog(logPath)
	if lerr != nil {
		return nil, nil, "", false, errors.Join(err, lerr)
	}
	logOut = rl
	defer func() {
		if err != nil {
			fmt.Fprintf(rl, "%s cannot start: %v\n", time.Now().Format("2006/01/02 15:04:05"), err)
		}
	}()
	if err != nil {
		return nil, nil, logPath, false, err
	}
	c.Log = "" // newServer naj ne odpre še enkrat
	if err = ensureInstance(cfgPath, &c); err != nil {
		return nil, nil, logPath, false, err
	}
	if s, err = newServer(c, cfgPath); err != nil {
		return nil, nil, logPath, false, err
	}
	if ln, err = listenRouter(c); err != nil {
		return nil, nil, logPath, false, fmt.Errorf("port %d: %v (is minigw serve already running?)", c.Port, err)
	}
	s.announce(ln)
	if !s.scratchOther.Load() { // N4: v mapi drugega prehoda nič
		sandboxInit(s.conf().Scratch, s.cfgDir, s.log.Printf) // #68: Windows, preizkus škatle MXC pred strežbo
	}
	return s, ln, logPath, fresh, nil
}
