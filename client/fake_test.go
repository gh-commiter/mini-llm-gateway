package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Prenosljivi lažni CLI (0.5.25 korak 8): kopija testnega programa z imenom programa (claude, codex …; na Windows
// .exe), vedenje izbere stranska datoteka <pot brez .exe>.fake (ime vedenja in parametri). Okolje ne: prehod ga
// agentu počisti. Vedenja so Go (fake), zato tečejo tudi na Windows.

type fakeParams map[string]string

var fakes = map[string]func(p fakeParams, args []string){}

// fake: vedenje z imenom name (spremenljivka paketa, da obstaja tudi v procesu lažnega programa).
func fake(name string, fn func(p fakeParams, args []string)) string {
	fakes[name] = fn
	return name
}

type fakeSpec struct {
	Fake string
	P    fakeParams
	Self string // testni program (fakeOrphan)
}

func fakeSidecar(exe string) string {
	if runtime.GOOS == "windows" {
		exe = strings.TrimSuffix(strings.ToLower(exe), ".exe")
	}
	return exe + ".fake"
}

// fakeCLI: lažni program name v mapi dir z vedenjem behaviour; vrne njegovo pot. Ponoven klic zamenja le vedenje.
func fakeCLI(t *testing.T, dir, name, behaviour string, p fakeParams) string {
	t.Helper()
	exe := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if _, err := os.Lstat(exe); err != nil {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		// Windows: vedno kopija (trda povezava na tekoči program se ne da izbrisati, TempDir ne bi šel); drugje
		// povezava, ob drugi napravi kopija
		if runtime.GOOS == "windows" || os.Link(self, exe) != nil {
			b, err := os.ReadFile(self)
			if err != nil || os.WriteFile(exe, b, 0o755) != nil {
				t.Fatalf("fake %s: %v", name, err)
			}
		}
	}
	self, _ := os.Executable()
	b, _ := json.Marshal(fakeSpec{behaviour, p, self})
	if err := os.WriteFile(fakeSidecar(exe), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return exe
}

// fakeMain (TestMain): ta proces je lažni program, če ima ob sebi stransko datoteko.
func fakeMain() {
	if v := os.Getenv("MINIGW_TEST_FAKE_SLEEP"); v != "" { // vnuk, ki drži stdout (fakeOrphan)
		d, _ := time.ParseDuration(v)
		time.Sleep(d)
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	b, err := os.ReadFile(fakeSidecar(exe))
	if err != nil {
		return
	}
	var s fakeSpec
	if json.Unmarshal(b, &s) != nil || fakes[s.Fake] == nil {
		fmt.Fprintln(os.Stderr, "fake: unknown behaviour in "+fakeSidecar(exe))
		os.Exit(97)
	}
	fakeSelf = s.Self
	fakes[s.Fake](s.P, os.Args[1:])
	os.Exit(0)
}

// Pomočniki vedenj (kot ukazi lupine v prejšnjih skriptah).

// fakeGuard (N9, prej cliGuard): claude -p prehoda vedno brez »@pot« in ukazov s poševnico; sicer izhod 9.
func fakeGuard(args []string) {
	if os.Getenv("CLAUDE_CODE_DISABLE_FILE_MENTIONS") != "1" || !slices.Contains(args, "--disable-slash-commands") {
		os.Exit(9)
	}
}

// fakeStdin: cat > /dev/null (ali v datoteko) — vrne prebrano.
func fakeStdin() string {
	b, _ := io.ReadAll(os.Stdin)
	return string(b)
}

// fakeArgs: printf '%s\n' "$@" > file.
func fakeArgs(file string, args []string, extra ...string) {
	var b strings.Builder
	for _, a := range append(slices.Clone(args), extra...) {
		b.WriteString(a + "\n")
	}
	os.WriteFile(file, []byte(b.String()), 0o644)
}

// fakeFlag: vrednost za zadnjim flag (for a in "$@"; do [ "$prev" = flag ] && v="$a"; prev="$a"; done).
func fakeFlag(args []string, flag string) string {
	v := ""
	for i := 1; i < len(args); i++ {
		if args[i-1] == flag {
			v = args[i]
		}
	}
	return v
}

// fakeCount: ls <glob> | wc -l.
func fakeCount(glob string) int {
	m, _ := filepath.Glob(glob)
	return len(m)
}

func fakeWrite(name, s string) { os.WriteFile(name, []byte(s), 0o644) }

// fakeAppend: echo … >> name (napaka tiho, kot v lupini); vrne, ali je uspelo.
func fakeAppend(name, text string) bool {
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	_, err = f.WriteString(text)
	return f.Close() == nil && err == nil
}

func fakeTouch(name string) {
	if f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
}

func fakeExists(name string) bool { _, err := os.Stat(name); return err == nil }

// fakeGit: git v mapi dir ("" = cwd) z okoljem agenta; izhod na stderr (ne meša vrstic JSON).
func fakeGit(dir string, args ...string) bool {
	c := exec.Command("git", args...)
	c.Dir, c.Stdout, c.Stderr = dir, os.Stderr, os.Stderr
	return c.Run() == nil
}

var fakeSelf string

// fakeOrphan: (sleep d 2>/dev/null &) — vnuk podeduje stdout in ga drži odprtega. Teče testni program zunaj TempDir
// (Windows: tekoč program ali mapa cwd se ne da izbrisati).
func fakeOrphan(d string) {
	c := exec.Command(fakeSelf)
	c.Dir, c.Env, c.Stdout = filepath.Dir(fakeSelf), append(os.Environ(), "MINIGW_TEST_FAKE_SLEEP="+d), os.Stdout
	c.Start()
}

// Izhod lažnega claude: besedilo »done« in konec (output_tokens kot v prejšnjih skriptah).
func fakeClaudeDone(tokens int) {
	fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}`)
	fmt.Println(`{"type":"result","subtype":"success","usage":{"output_tokens":` + strconv.Itoa(tokens) + `}}`)
}

// fakePrint: splošno vedenje — stdout (besedilo), cat (datoteka na stdout), orphan (vnuk drži stdout), exit.
var fakePrint = fake("print", func(p fakeParams, args []string) {
	if p["args"] != "" {
		fakeArgs(p["args"], args)
	}
	if p["orphan"] != "" {
		fakeOrphan(p["orphan"])
	}
	if p["touch"] != "" {
		fakeTouch(p["touch"])
	}
	os.Stdout.WriteString(p["stdout"])
	if p["cat"] != "" {
		f, err := os.Open(p["cat"])
		if err == nil {
			io.Copy(os.Stdout, f)
			f.Close()
		}
	}
	if p["sleep"] != "" {
		d, _ := time.ParseDuration(p["sleep"])
		time.Sleep(d)
	}
	n, _ := strconv.Atoi(p["exit"])
	os.Exit(n)
})
