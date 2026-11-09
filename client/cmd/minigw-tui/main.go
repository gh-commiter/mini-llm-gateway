// minigw-tui: the gateway settings in a terminal (minigw gw settings), with the pages of the settings page.
// It never reads the config, the key store or the admin secret: every read and change is a "minigw gw api" call of
// the minigw that started it, so the same access checks and validation apply. Secrets go to it on stdin.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

const usage = `usage: minigw-tui [--minigw PATH] [minigw gw options, e.g. --config FILE]
Started by "minigw gw settings"; it calls back that minigw for every read and change.`

// ui: the backend (the calling minigw) and the state that survives a reload of a page.
type ui struct {
	minigw string
	extra  []string        // passed to every "minigw gw api" (e.g. --config FILE)
	open   map[string]bool // devices with their history shown
	models string          // provider whose model list is shown ("" = the provider list)
	filter string          // that list's filter
	draft  struct {
		name, url string
		insecure  bool
	} // "Add a provider"
	connect bool   // the connect line (with the token) is shown
	session string // session with its details shown
}

// call: one admin API request through "minigw gw api"; the body goes on stdin, never in argv.
func (u *ui) call(method, path string, body any) (map[string]any, error) {
	in := []byte{}
	if body != nil {
		in, _ = json.Marshal(body)
	}
	cmd := exec.Command(u.minigw, append([]string{"gw", "api", method, path}, u.extra...)...)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(in), &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, errors.New(failure(errb.String(), err))
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("%s gw api %s %s: not JSON (an older minigw? it needs 0.5.20)", u.minigw, method, path)
	}
	return m, nil
}

// failure: minigw's last message ("minigw: …" on stderr) in one line, without control characters.
func failure(stderr string, err error) string {
	if strings.Contains(stderr, "unknown command minigw gw api") {
		return "this minigw is older than 0.5.20 (it has no minigw gw api): update minigw"
	}
	if i := strings.LastIndex(stderr, "minigw: "); i >= 0 {
		stderr = stderr[i+len("minigw: "):]
	}
	if s := strings.Join(strings.Fields(clean(strings.ReplaceAll(stderr, "\n", " "))), " "); s != "" {
		return s
	}
	return err.Error()
}

// save: a change; the note says whether it needs a restart.
func (u *ui) save(method, path string, body any) (string, error) {
	r, err := u.call(method, path, body)
	if err != nil {
		return "", err
	}
	if r["restartRequired"] == true {
		return "Saved. Restart the gateway to apply it.", nil
	}
	return "Saved.", nil
}

type kind int

const (
	header kind = iota
	info
	toggle
	edit
	secret
	button
)

// row: one line of a page. act runs on Enter (toggle, button) or with the typed text (edit, secret).
type row struct {
	kind         kind
	label, value string
	on           bool
	confirm      string // asked first (y/n)
	act          func(text string) (string, error)
}

func (r row) selectable() bool { return r.kind >= toggle }

func hdr(title string) row        { return row{kind: header, label: title} }
func inf(label, value string) row { return row{kind: info, label: label, value: value} }
func tog(label string, on bool, f func(bool) (string, error)) row {
	return row{kind: toggle, label: label, on: on, act: func(string) (string, error) { return f(!on) }}
}
func ed(label, value string, f func(string) (string, error)) row {
	return row{kind: edit, label: label, value: value, act: f}
}
func btn(label, confirm string, f func() (string, error)) row {
	return row{kind: button, label: label, confirm: confirm, act: func(string) (string, error) { return f() }}
}

type page struct {
	title string
	load  func(u *ui) ([]row, error)
}

// providersTab: the page with the model list of one provider (esc goes back).
const providersTab = 2

var pages = []page{{"General", general}, {"Devices", devices}, {"Providers", providers}, {"Agents", agents},
	{"Folders", folders}, {"Sessions", sessions}}

// macPage: only when the gateway is the Mac app (its status has no "os").
var macPage = page{"Mac", macOnly}

type loaded struct {
	tab  int // the page it belongs to (an answer for a page left meanwhile is dropped)
	rows []row
	err  error
	note string
	bad  bool
}

type isMac bool

type model struct {
	u          *ui
	mac        bool
	tab        int
	rows       []row
	cur        int
	w, h       int
	note       string
	bad, busy  bool
	editing    bool
	input      []rune
	asking     bool
	loadFailed error
}

func (m model) pages() []page {
	if m.mac {
		return append(slices.Clone(pages), macPage)
	}
	return pages
}

func (m model) reload(note string, bad bool) tea.Cmd {
	u, tab, p := m.u, m.tab, m.pages()[m.tab]
	return func() tea.Msg {
		rows, err := p.load(u)
		return loaded{tab, rows, err, note, bad}
	}
}

func (m model) run(r row, text string) tea.Cmd {
	u, tab, p := m.u, m.tab, m.pages()[m.tab]
	return func() tea.Msg {
		note, err := r.act(text)
		rows, lerr := p.load(u)
		if err != nil {
			return loaded{tab, rows, lerr, err.Error(), true}
		}
		return loaded{tab, rows, lerr, note, false}
	}
}

func (m model) Init() tea.Cmd {
	u := m.u
	return tea.Batch(m.reload("", false), func() tea.Msg {
		st, err := u.call("GET", "/status", nil)
		_, hasOS := st["os"]
		return isMac(err == nil && !hasOS)
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case isMac:
		m.mac = bool(msg)
	case loaded:
		if msg.tab != m.tab {
			return m, nil
		}
		m.busy, m.rows, m.loadFailed, m.note, m.bad = false, msg.rows, msg.err, msg.note, msg.bad
		if m.cur >= len(m.rows) {
			m.cur = len(m.rows) - 1
		}
		m.cur = max(m.cur, 0)
		if len(m.rows) > 0 && !m.rows[m.cur].selectable() {
			m.move(1)
		}
	case tea.KeyMsg:
		return m.key(msg)
	case tea.MouseMsg:
		return m.mouse(msg)
	}
	return m, nil
}

func (m *model) move(d int) {
	for i := m.cur + d; i >= 0 && i < len(m.rows); i += d {
		if m.rows[i].selectable() {
			m.cur = i
			return
		}
	}
}

// goTo: another page; the connect line and the model list close.
func (m model) goTo(tab int) (tea.Model, tea.Cmd) {
	m.tab = tab
	m.u.connect, m.u.models = false, ""
	m.rows, m.cur, m.note, m.busy, m.editing, m.asking, m.input = nil, 0, "", true, false, false, nil
	return m, m.reload("", false)
}

// activate: Enter or a click on the current row.
func (m model) activate() (tea.Model, tea.Cmd) {
	if m.cur >= len(m.rows) || !m.rows[m.cur].selectable() {
		return m, nil
	}
	r := m.rows[m.cur]
	switch {
	case r.kind == edit:
		m.editing, m.input = true, []rune(r.value)
	case r.kind == secret:
		m.editing, m.input = true, nil
	case r.confirm != "":
		m.asking, m.note, m.bad = true, r.confirm+" (y/n)", false
	default:
		m.busy = true
		return m, m.run(r, "")
	}
	return m, nil
}

func (m model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.editing {
		switch k.Type {
		case tea.KeyEnter:
			m.editing, m.busy = false, true
			text := strings.TrimSpace(string(m.input))
			m.input = nil
			return m, m.run(m.rows[m.cur], text)
		case tea.KeyEsc:
			m.editing, m.input = false, nil
		case tea.KeyBackspace:
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
		case tea.KeyCtrlU:
			m.input = nil
		case tea.KeyRunes, tea.KeySpace:
			m.input = append(m.input, k.Runes...)
		}
		return m, nil
	}
	if m.asking {
		m.asking = false
		if k.String() == "y" {
			m.busy = true
			return m, m.run(m.rows[m.cur], "")
		}
		m.note = ""
		return m, nil
	}
	n := len(m.pages())
	switch s := k.String(); s {
	case "q":
		return m, tea.Quit
	case "esc":
		if m.tab == providersTab && m.u.models != "" {
			m.u.models = ""
			return m, m.reload("", false)
		}
		return m, nil
	case "tab", "right", "l":
		return m.goTo((m.tab + 1) % n)
	case "shift+tab", "left", "h":
		return m.goTo((m.tab + n - 1) % n)
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if t := int(s[0] - '1'); t < n {
			return m.goTo(t)
		}
		return m, nil
	}
	if m.busy { // a change or load runs: only quit and changing the page
		return m, nil
	}
	switch k.String() {
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		for range 10 {
			m.move(-1)
		}
	case "pgdown":
		for range 10 {
			m.move(1)
		}
	case "r":
		m.busy = true
		return m, m.reload("", false)
	case "enter", " ":
		return m.activate()
	}
	return m, nil
}

// mouse: a click on a page name opens it, a click on a row selects and changes it (as Enter); the wheel moves.
func (m model) mouse(e tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.editing || m.asking {
		return m, nil
	}
	switch {
	case e.Button == tea.MouseButtonWheelUp && !m.busy:
		m.move(-1)
	case e.Button == tea.MouseButtonWheelDown && !m.busy:
		m.move(1)
	case e.Button != tea.MouseButtonLeft || e.Action != tea.MouseActionPress:
	case e.Y == 1:
		for i, x := range m.tabStarts() {
			if e.X >= x && (i+1 == len(m.tabStarts()) || e.X < m.tabStarts()[i+1]) {
				return m.goTo(i)
			}
		}
	case !m.busy:
		_, rowOf, top := m.body()
		if l := top + e.Y - 3; e.Y >= 3 && l < len(rowOf) && e.Y < 3+m.room() && rowOf[l] >= 0 && m.rows[rowOf[l]].selectable() {
			m.cur = rowOf[l]
			return m.activate()
		}
	}
	return m, nil
}

func (m model) size() (int, int) {
	if m.w <= 0 {
		return 80, 24
	}
	return m.w, m.h
}

func (m model) room() int { _, h := m.size(); return max(h-6, 3) }

// tabLine and tabStarts: the page names and the column where each starts.
func (m model) tabLine() string {
	s := " "
	for i, p := range m.pages() {
		if i == m.tab {
			s += fmt.Sprintf("[%d %s]", i+1, p.title)
		} else {
			s += fmt.Sprintf(" %d %s ", i+1, p.title)
		}
	}
	return s
}

func (m model) tabStarts() []int {
	var xs []int
	x := 1
	for _, p := range m.pages() {
		xs = append(xs, x)
		x += utf8.RuneCountInString(fmt.Sprintf(" %d %s ", 1, p.title))
	}
	return xs
}

// body: the page's lines, the row of each line (-1 = none) and the first line shown (the cursor stays visible).
func (m model) body() (lines []string, rowOf []int, top int) {
	cursorLine := 0
	add := func(s string, r int) { lines, rowOf = append(lines, s), append(rowOf, r) }
	if m.loadFailed != nil {
		add("  "+m.loadFailed.Error(), -1)
		add("", -1)
		add("  r: try again", -1)
	}
	for i, r := range m.rows {
		if r.kind == header && len(lines) > 0 {
			add("", -1)
		}
		if i == m.cur {
			cursorLine = len(lines)
		}
		add(m.render(i, r), i)
	}
	if len(m.rows) == 0 && m.loadFailed == nil {
		add("  Loading…", -1)
	}
	if room := m.room(); cursorLine >= room {
		top = cursorLine - room + 1
	}
	if m.cur == firstSelectable(m.rows) { // at the first control: show the headers above it
		top = 0
	}
	return lines, rowOf, top
}

func firstSelectable(rows []row) int {
	for i, r := range rows {
		if r.selectable() {
			return i
		}
	}
	return -1
}

func (m model) View() string {
	w, _ := m.size()
	var b strings.Builder
	line := func(s string) { b.WriteString(cut(s, w) + "\n") }
	line(" Mini Gateway settings")
	line(m.tabLine())
	line(strings.Repeat("─", w))
	body, _, top := m.body()
	for i := top; i < top+m.room(); i++ {
		if i < len(body) {
			line(body[i])
		} else {
			line("")
		}
	}
	line(strings.Repeat("─", w))
	note := m.note
	switch {
	case m.editing:
		shown := string(m.input)
		if m.rows[m.cur].kind == secret {
			shown = strings.Repeat("•", len(m.input))
		}
		note = m.rows[m.cur].label + ": " + shown + "▏"
	case m.busy:
		note = "Working…"
	case m.bad:
		note = "! " + note
	}
	line(" " + note)
	help := " ←/→ page  ↑/↓ move  enter change  r reload  q quit  (mouse: click)"
	switch {
	case m.editing:
		help = " enter save  esc cancel  ctrl+u clear"
	case m.tab == providersTab && m.u.models != "":
		help = " ↑/↓ move  enter change  esc back  r reload  q quit"
	}
	b.WriteString(cut(help, w))
	return b.String()
}
func (m model) render(i int, r row) string {
	mark := "  "
	if i == m.cur && r.selectable() {
		mark = "> "
	}
	pad := func(s string) string { return s + strings.Repeat(" ", max(40-utf8.RuneCountInString(s), 1)) }
	switch r.kind {
	case header:
		return "  ── " + r.label
	case info:
		if r.value == "" {
			return "  " + r.label
		}
		return "  " + pad(r.label) + r.value
	case toggle:
		x := "[ ]"
		if r.on {
			x = "[x]"
		}
		return strings.TrimRight(mark+pad(r.label)+x+"  "+r.value, " ")
	case edit:
		v := r.value
		if v == "" {
			v = "(empty)"
		}
		return mark + pad(r.label) + v
	case secret:
		return mark + pad(r.label) + r.value
	}
	return mark + "[ " + r.label + " ]" + map[bool]string{true: "  " + r.value, false: ""}[r.value != ""]
}

// cut: at most w columns (runes; the pages use no wide characters of their own).
func cut(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	return string([]rune(s)[:max(w-1, 0)]) + "…"
}

func main() {
	u := &ui{open: map[string]bool{}}
	args := os.Args[1:]
	for len(args) > 0 {
		switch {
		case args[0] == "--minigw" && len(args) > 1:
			u.minigw, args = args[1], args[2:]
		case args[0] == "-h" || args[0] == "--help":
			fmt.Println(usage)
			return
		default:
			u.extra, args = append(u.extra, args[0]), args[1:]
		}
	}
	if u.minigw == "" { // started on its own: the minigw next to it, else from PATH
		name := "minigw"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		u.minigw = name
		if self, err := os.Executable(); err == nil {
			if p := filepath.Join(filepath.Dir(self), name); fileExists(p) {
				u.minigw = p
			}
		}
	}
	if _, err := tea.NewProgram(model{u: u, busy: true}, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "minigw-tui:", err)
		os.Exit(1)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
