package main

// Profili (#17): en odjemalec, več prehodov. Vsak profil ima svoj naslov, žeton in ključ prehoda; ključ naprave je en
// za ves odjemalec (v vsakem profilu ista kopija). Prejšnja edina povezava je profil »default« na istem mestu.

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const defaultProfile = "default"

var profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// validProfile: male črke, števke in »-«; ime gre v pripono modela (@ime), datoteko in poverilnico.
func validProfile(n string) bool { return profileRe.MatchString(n) }

// profileFlag: --minigw-profile <ime> velja za cel ukaz (le ta prehod).
var profileFlag string

type profile struct {
	name string
	conn connection
}

func defaultProfileFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "minigw", "default-profile")
}

func profileNames() []string {
	n := storeNames()
	slices.Sort(n)
	return slices.Compact(n)
}

// defaultName: izbrani privzeti profil (minigw profiles default), sicer »default«, sicer prvi.
func defaultName(names []string) string {
	b, _ := os.ReadFile(defaultProfileFile())
	if n := strings.TrimSpace(string(b)); slices.Contains(names, n) {
		return n
	}
	if slices.Contains(names, defaultProfile) || len(names) == 0 {
		return defaultProfile
	}
	return names[0]
}

func loadProfile(name string) *connection {
	data, err := storeLoad(name)
	if err != nil {
		return nil
	}
	var c connection
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

func saveProfile(name string, c connection) {
	data, _ := json.Marshal(c)
	if err := storeSave(name, data); err != nil {
		fail("cannot save the connection to " + storeName + ": " + err.Error())
	}
}

// agentConn: minigw teče v agentu prehoda (MINIGW_AGENT_URL, žeton v datoteki MINIGW_AGENT_AUTH_FILE): le ta povezava,
// brez profilov, proxyja, šifriranja in deljenja mape (prehod sam doda napravo, sejo in mapo klicatelja).
func agentConn() *connection {
	u, f := os.Getenv("MINIGW_AGENT_URL"), os.Getenv("MINIGW_AGENT_AUTH_FILE")
	if u == "" || f == "" {
		return nil
	}
	b, _ := os.ReadFile(f) // M3: žeton ni v okolju (drugi procesi istega uporabnika ga tam lahko preberejo)
	t := strings.TrimSpace(string(b))
	p, err := url.Parse(u)
	if t == "" || len(t) > 256 || err != nil || p.Scheme != "http" || p.Hostname() != "127.0.0.1" || p.Port() == "" || p.User != nil ||
		(p.Path != "" && p.Path != "/") || p.RawQuery != "" { // I3: le vrata izvajanja na tem računalniku
		return nil
	}
	return &connection{URL: u, Token: t}
}

// loadProfiles: shranjeni profili, privzeti prvi; z --minigw-profile le ta. V agentu prehoda le njegova povezava.
func loadProfiles() []profile {
	if c := agentConn(); c != nil {
		return []profile{{"agent", *c}}
	}
	names := profileNames()
	def := defaultName(names)
	if profileFlag != "" {
		if !slices.Contains(names, profileFlag) {
			fail("no profile " + profileFlag + " (minigw profiles lists them)")
		}
		def, names = profileFlag, []string{profileFlag}
	}
	order := []string{def}
	for _, n := range names {
		if n != def {
			order = append(order, n)
		}
	}
	var out []profile
	for _, n := range order {
		if c := loadProfile(n); c != nil {
			out = append(out, profile{n, *c})
		}
	}
	return out
}

// clientDevice: ključ naprave, en za ves odjemalec, shranjen posebej (deviceLoad): minigw disconnect ga ne odstrani, zato
// prehod ob novem connect vidi isto napravo (odobritev, zgodovina). Prvič ga prenese iz profila (starejši minigw ga je
// hranil le tam), sicer nov. Odstrani ga le minigw uninstall --purge.
func clientDevice() *string {
	d := deviceLoad()
	if d == "" {
		for _, n := range profileNames() {
			if c := loadProfile(n); c != nil && c.Device != nil && *c.Device != "" {
				d = *c.Device
				break
			}
		}
		if d == "" {
			d = newDeviceKey()
		}
		if err := deviceSave(d); err != nil {
			fail("cannot save the device key to " + storeName + ": " + err.Error())
		}
	}
	return &d
}

// newToken: vrstica »Copy Token« (<žeton>#<ključ>) ali le žeton; brez ključa ostane shranjeni ključ prehoda.
// false = prazen ali nepopoln žeton (»#«, »#kljuc«, »zeton#«).
func newToken(line string, key *string) (string, *string, bool) {
	tok, k, has := strings.Cut(line, "#")
	if tok == "" || has && k == "" {
		return "", nil, false
	}
	if has {
		key = &k
	}
	return tok, key, true
}

// reconnectCommand: nov žeton za obstoječ profil; naslov, ključ prehoda (če ga vrstica nima) in ključ naprave ostanejo,
// zato prehod vidi isto napravo. Nato kot minigw status; false = profil ni povezan.
func reconnectCommand(args []string) bool {
	name, token := defaultName(profileNames()), ""
	if profileFlag != "" {
		name = profileFlag
	}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			name = args[i+1]
		case args[i] == "--token" && i+1 < len(args):
			token = args[i+1]
		default:
			fail("usage: minigw reconnect [--name <profile>] [--token '<token>' | --token -]   (no --token or - reads it from stdin or the terminal)")
		}
		i++
	}
	if !validProfile(name) {
		fail("invalid --name " + name + " (lowercase letters, digits and -, up to 32)")
	}
	saved := loadProfile(name)
	if saved == nil {
		fail("no profile " + name + "; run: minigw connect <host[:port]> --name " + name + " --token -")
	}
	if token == "" || token == "-" { // F3: s stdin ali s terminala brez odmeva, ne v argv
		t, err := readKey(os.Stdin, "new gateway token")
		if err != nil {
			fail(err.Error())
		}
		token = t
	}
	c := *saved
	var ok bool
	if c.Token, c.Key, ok = newToken(token, saved.Key); !ok {
		fail("the token is empty or incomplete: copy it again (Mini Gateway → Settings → General → Copy Token)")
	}
	c.Device = clientDevice()
	saveProfile(name, c)
	say("Saved the new token for profile " + name)
	return profileStatus(profile{name, c})
}

// splitModel: »claude-cli-opus@win« → (claude-cli-opus, win), če je win profil; sicer model ostane cel (tudi
// imena ponudnikov z »@«, npr. claude-opus-4@20250514).
func splitModel(model string, names []string) (string, string) {
	if i := strings.LastIndex(model, "@"); i > 0 && slices.Contains(names, model[i+1:]) {
		return model[:i], model[i+1:]
	}
	return model, ""
}

func profilesCommand(args []string) {
	names := profileNames()
	switch {
	case len(args) == 0 || args[0] == "list":
		if len(names) == 0 {
			fail("no profiles; run: minigw connect <host[:port]> --name <name> --token -")
		}
		def := defaultName(names)
		for _, n := range names {
			mark, url := "  ", "?"
			if n == def {
				mark = "* "
			}
			if c := loadProfile(n); c != nil {
				url = c.URL
			}
			say(mark + n + "  " + url)
		}
	case args[0] == "default" && len(args) == 2:
		if !slices.Contains(names, args[1]) {
			fail("no profile " + args[1])
		}
		f := defaultProfileFile()
		os.MkdirAll(filepath.Dir(f), 0o700)
		if err := os.WriteFile(f, []byte(args[1]+"\n"), 0o600); err != nil {
			fail("cannot save " + f + ": " + err.Error())
		}
		say("Default profile: " + args[1] + " (its models also appear without @" + args[1] + ")")
	default:
		fail("usage: minigw profiles [list] | minigw profiles default <name>")
	}
}
