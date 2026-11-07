package main

// Profili (#17): en odjemalec, več prehodov. Vsak profil ima svoj naslov, žeton in ključ prehoda; ključ naprave je en
// za ves odjemalec (v vsakem profilu ista kopija). Prejšnja edina povezava je profil »default« na istem mestu.

import (
	"encoding/json"
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

// agentConn: minigw teče v agentu prehoda (MINIGW_AGENT_URL in MINIGW_AGENT_TOKEN): le ta povezava, brez profilov,
// proxyja, šifriranja in deljenja mape (prehod sam doda napravo, sejo in mapo klicatelja).
func agentConn() *connection {
	u, t := os.Getenv("MINIGW_AGENT_URL"), os.Getenv("MINIGW_AGENT_TOKEN")
	if u == "" || t == "" {
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

// anyDevice: ključ naprave iz kateregakoli profila (en ključ na odjemalca).
func anyDevice() *string {
	for _, n := range profileNames() {
		if c := loadProfile(n); c != nil && c.Device != nil {
			return c.Device
		}
	}
	return nil
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
