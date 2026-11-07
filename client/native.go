package main

// --minigw-work=native in »minigw run« (kot Native v Sources/RouterCore/Native.swift): mapa je prek SMB mapa routerja.
// minigw pošlje ime sheme in relativno pot, router v svojo mapo zapiše označbo z naključno vsebino, minigw jo prebere
// skozi priklop in jo vrne. Le kdor vidi prav to mapo, pozna vsebino.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// smbPath: priklop SMB (»//uporabnik@strežnik/Shema[/podmapa]«, odstotki dekodirani) na mountPoint → shema in pot
// delovne mape pod njo; ok=false, če cwd ni pod priklopom.
func smbPath(source, mountPoint, cwd string) (share, rel string, ok bool) {
	source = strings.ReplaceAll(source, `\`, "/") // UNC na Windows
	if !strings.HasPrefix(source, "//") {
		return "", "", false
	}
	var parts []string
	for _, p := range strings.Split(source[2:], "/") {
		if p == "" {
			continue
		}
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		parts = append(parts, p)
	}
	if len(parts) < 2 {
		return "", "", false
	}
	r, err := filepath.Rel(mountPoint, cwd)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return "", "", false
	}
	tail := parts[2:]
	if r != "." {
		tail = append(tail, strings.Split(filepath.ToSlash(r), "/")...)
	}
	return parts[1], strings.Join(tail, "/"), true
}

// nativeDetect: shema in pot za cwd; MINIGW_NATIVE_MOUNT=<shema>=<mapa> velja pred zaznavanjem (router vseeno preveri označbo).
func nativeDetect(cwd string) (share, rel string, ok bool) {
	if o := os.Getenv("MINIGW_NATIVE_MOUNT"); o != "" {
		if i := strings.Index(o, "="); i > 0 {
			return smbPath("//minigw/"+o[:i], o[i+1:], cwd)
		}
	}
	if src, mp, found := smbMount(cwd); found {
		return smbPath(src, mp, cwd)
	}
	return "", "", false
}

// smbFromMountinfo: priklop cifs/smb3 z najdaljšo potjo, pod katero je cwd (vrstice /proc/self/mountinfo).
func smbFromMountinfo(info, cwd string) (source, mountPoint string, ok bool) {
	for _, l := range strings.Split(info, "\n") {
		f := strings.Fields(l)
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || len(f) < sep+3 {
			continue
		}
		mp := unescapeMount(f[4])
		if (f[sep+1] != "cifs" && f[sep+1] != "smb3") || !under(cwd, mp) || len(mp) <= len(mountPoint) {
			continue
		}
		source, mountPoint, ok = unescapeMount(f[sep+2]), mp, true
		// Polje 4: podmapa sheme pri priklopu bind (pri cifs prefixpath je že v viru).
		if root := unescapeMount(f[3]); root != "/" && !strings.HasSuffix(strings.TrimSuffix(source, "/"), root) {
			source = strings.TrimSuffix(source, "/") + root
		}
	}
	return
}

// unescapeMount: \040 (presledek) in druge osmiške ubežne kode v mountinfo.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var markerName = regexp.MustCompile(`^\.minigw-native-[0-9a-f]{32}$`)

var markerTries = 25 // po 200 ms

// nativeProof: glave z dokazom ali razlog, zakaj native ni mogoč.
func nativeProof(c connection, cwd string) (map[string]string, string) {
	share, rel, ok := nativeDetect(cwd)
	if !ok {
		return nil, cwd + " is not on an SMB share (set MINIGW_NATIVE_MOUNT=<share>=<mount folder> if it is)"
	}
	body, _ := json.Marshal(map[string]string{"share": share, "path": rel})
	st, data := call(c, "POST", "/minigw/native", map[string]string{"content-type": "application/json"}, body, 10*time.Second)
	var r struct {
		ID, File string
		Error    struct{ Message string }
	}
	json.Unmarshal(data, &r)
	if st != 200 || r.File != ".minigw-native-"+r.ID || !markerName.MatchString(r.File) { // router ni zaupanja vreden
		switch {
		case st == 404:
			return nil, "the gateway does not support native mode"
		case r.Error.Message != "":
			return nil, clean(r.Error.Message)
		}
		return nil, fmt.Sprintf("the gateway refused native mode (HTTP %d)", st)
	}
	for i := 0; i < markerTries; i++ { // SMB: nova datoteka je lahko vidna z zamikom
		if v, ok := readMarker(filepath.Join(cwd, r.File)); ok {
			return map[string]string{"x-native-id": r.ID, "x-native-proof": v}, ""
		}
		time.Sleep(200 * time.Millisecond)
	}
	call(c, "DELETE", "/minigw/native", map[string]string{"x-native-id": r.ID}, nil, 5*time.Second) // označba ne ostane do TTL
	return nil, cwd + " is not the gateway's folder " + share + "/" + rel + " (its check file is not visible here)"
}

// readMarker: le navadna datoteka (odprta brez sledenja povezavi, glej openMarker), največ 128 bajtov.
func readMarker(p string) (string, bool) {
	f, err := openMarker(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil || len(b) > 128 {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// terminalSafe: izhod minigw run za terminal (kot Native.terminalSafe): le barve SGR (ESC[…m) in \n \r \t \b;
// brez OSC, DCS, APC, PM, drugih CSI, kontrolnih znakov in C1 v UTF-8 (C2 80–C2 9F). Brez stanja med kosi.
func terminalSafe(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c == 0xc2 && i+1 == len(b) { // osamljen na koncu (glej safeWriter)
			break
		}
		if c == 0xc2 && b[i+1] >= 0x80 && b[i+1] <= 0x9f { // C1 v UTF-8 (U+0080–U+009F)
			i += 2
			continue
		}
		if c != 0x1b {
			if c >= 0x20 && c != 0x7f || c == '\n' || c == '\r' || c == '\t' || c == '\b' {
				out = append(out, c)
			}
			i++
			continue
		}
		if i+1 >= len(b) {
			break
		}
		switch k := b[i+1]; k {
		case '[':
			j := i + 2
			for j < len(b) && b[j] >= 0x30 && b[j] <= 0x3f {
				j++
			}
			params, inter := b[i+2:j], false
			for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2f {
				j, inter = j+1, true
			}
			if j >= len(b) || b[j] < 0x40 || b[j] > 0x7e {
				return out // nedokončano
			}
			if b[j] == 'm' && !inter && strings.Trim(string(params), "0123456789;") == "" {
				out = append(out, b[i:j+1]...)
			}
			i = j + 1
		case ']', 'P', 'X', '^', '_':
			j := i + 2 // niz do BEL ali ST (ESC \\)
			for j < len(b) && b[j] != 0x07 && !(b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\') {
				j++
			}
			if j < len(b) && b[j] == 0x1b {
				i = j + 2
			} else {
				i = j + 1
			}
		default:
			i += 2
		}
	}
	return out
}

// runCommand: minigw run [--] <ukaz> … v tej mapi na routerju; izhodna koda kot ukaz (255 = povezava prekinjena).
func runCommand(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fail("usage: minigw run [--] <command> [args …]")
	}
	c := route(requireProfile())
	cwd, _ := os.Getwd()
	proof, why := nativeProof(c, cwd)
	if proof == nil {
		fail("cannot run on the gateway: " + why)
	}
	body, _ := json.Marshal(map[string][]string{"argv": args})
	req, _ := http.NewRequest("POST", c.URL+"/minigw/run", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+c.Token)
	req.Header.Set("content-type", "application/json")
	for k, v := range proof {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{CheckRedirect: noRedirect}).Do(req) // brez časovne omejitve: gradnja traja; router pošlje »p« vsakih 15 s
	if err != nil {
		fail("cannot reach the gateway: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var r struct{ Error struct{ Message string } }
		json.NewDecoder(resp.Body).Decode(&r)
		if r.Error.Message == "" {
			r.Error.Message = fmt.Sprintf("the gateway answered HTTP %d", resp.StatusCode)
		}
		fail(clean(r.Error.Message))
	}
	return runOutput(resp.Body, ttySafe(os.Stdout), ttySafe(os.Stderr))
}

// ttySafe: terminal dobi le varna zaporedja (terminalSafe); datoteka ali cev izhod nespremenjen.
func ttySafe(f *os.File) io.Writer {
	if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		return &safeWriter{w: f}
	}
	return f
}

// safeWriter: končni 0xC2 počaka na naslednji kos, zato C1 ne zdrsne čez mejo kosa; drugje razdeljen znak UTF-8 ostane cel.
type safeWriter struct {
	w    io.Writer
	held []byte
}

func (s *safeWriter) Write(p []byte) (int, error) {
	all := append(s.held, p...)
	s.held = nil
	if len(all) > 0 && all[len(all)-1] == 0xc2 {
		s.held, all = []byte{0xc2}, all[:len(all)-1]
	}
	_, err := s.w.Write(terminalSafe(all))
	return len(p), err
}

// runOutput: vrstice »o|e <base64>«, »p«, »x <koda>« → izhod ukaza (nespremenjen, barve gradnje) in koda.
func runOutput(body io.Reader, stdout, stderr io.Writer) int {
	r := bufio.NewReaderSize(body, 1<<20)
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")
		if len(line) > 2 {
			switch line[:2] {
			case "o ", "e ":
				if d, e := base64.StdEncoding.DecodeString(line[2:]); e == nil {
					if line[0] == 'o' {
						stdout.Write(d)
					} else {
						stderr.Write(d)
					}
				}
			case "x ":
				if n, e := strconv.Atoi(line[2:]); e == nil {
					return n
				}
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "minigw: lost the connection to the gateway")
			return 255
		}
	}
}
