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
			return nil, "the router does not support native mode"
		case r.Error.Message != "":
			return nil, clean(r.Error.Message)
		}
		return nil, fmt.Sprintf("the router refused native mode (HTTP %d)", st)
	}
	for i := 0; i < markerTries; i++ { // SMB: nova datoteka je lahko vidna z zamikom
		if v, ok := readMarker(filepath.Join(cwd, r.File)); ok {
			return map[string]string{"x-native-id": r.ID, "x-native-proof": v}, ""
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, cwd + " is not the router's folder " + share + "/" + rel + " (its check file is not visible here)"
}

// readMarker: le navadna datoteka do 128 bajtov.
func readMarker(p string) (string, bool) {
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 128 {
		return "", false
	}
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err == nil
}

// runCommand: minigw run [--] <ukaz> … v tej mapi na routerju; izhodna koda kot ukaz (255 = povezava prekinjena).
func runCommand(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fail("usage: minigw run [--] <command> [args …]")
	}
	c := route(requireConnection())
	cwd, _ := os.Getwd()
	proof, why := nativeProof(c, cwd)
	if proof == nil {
		fail("cannot run on the router: " + why)
	}
	body, _ := json.Marshal(map[string][]string{"argv": args})
	req, _ := http.NewRequest("POST", c.URL+"/minigw/run", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+c.Token)
	req.Header.Set("content-type", "application/json")
	for k, v := range proof {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req) // brez časovne omejitve: gradnja traja; router pošlje »p« vsakih 15 s
	if err != nil {
		fail("cannot reach the router: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var r struct{ Error struct{ Message string } }
		json.NewDecoder(resp.Body).Decode(&r)
		if r.Error.Message == "" {
			r.Error.Message = fmt.Sprintf("the router answered HTTP %d", resp.StatusCode)
		}
		fail(clean(r.Error.Message))
	}
	return runOutput(resp.Body, os.Stdout, os.Stderr)
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
			fmt.Fprintln(os.Stderr, "minigw: lost the connection to the router")
			return 255
		}
	}
}
