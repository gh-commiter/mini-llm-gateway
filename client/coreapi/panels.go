package coreapi

// Admin panels: declarative descriptions (format 1) that a renderer (the Mac app) maps to native controls. Design:
// docs/plans/plugin-panels-design.md. This file is the ONE validator, the provider registry and GET /panels. The Swift
// renderer (Sources/RouterCore/Panels.swift) applies the same rules again to what it receives; the shared fixtures in
// client/testdata/panels-*.json keep the two from drifting.

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PanelFormat is the only version of the description format.
const PanelFormat = 1

// Limits of format 1 (the Swift side has the same numbers).
const (
	maxPanelBytes   = 64 << 10
	maxPanelsPerP   = 10
	maxSections     = 12
	maxColumns      = 10
	maxActions      = 12
	maxFields       = 10
	maxLabel        = 120
	maxMessage      = 600
	maxRefs         = 5
	maxRefEntries   = 1000 // enforced by the renderer on the data
	maxOptions      = 20
	maxBodyKeys     = 10
	maxQueryParams  = 8
	maxFieldLength  = 4096
	maxPathBytes    = 200
	maxFilterFields = 5
)

// PanelProvider registers panels and the routes its panels may call, as "METHOD pattern" in the notation of adminDo's
// switch ("PATCH users/{}": a {} is one path segment).
type PanelProvider struct {
	ID     string
	Routes []string
	Panels []json.RawMessage
}

//go:embed panels/*.json
var builtinPanelFiles embed.FS

// BuiltinRoutes: the routes the built-in panels may use. The core's own routes plus the gateway's GET devices.
var BuiltinRoutes = []string{"GET users", "POST users", "PATCH users/{}", "GET bindings", "PUT bindings/{}", "DELETE bindings/{}", "GET devices"}

// panelSymbols: the names a renderer maps to its icons.
var panelSymbols = []string{"person.2", "person.3", "folder", "book", "tray", "gearshape", "doc.text", "key", "list.bullet"}

func builtinProvider() PanelProvider {
	p := PanelProvider{ID: "builtin", Routes: BuiltinRoutes}
	ents, _ := builtinPanelFiles.ReadDir("panels") // sorted by name
	for _, e := range ents {
		b, err := builtinPanelFiles.ReadFile("panels/" + e.Name())
		if err == nil {
			p.Panels = append(p.Panels, b)
		}
	}
	return p
}

// --- the description ---

type pStr struct {
	En string `json:"en"`
	Sl string `json:"sl"`
}

type pPanel struct {
	Format   int             `json:"format"`
	ID       string          `json:"id"`
	Provider string          `json:"provider"`
	Title    pStr            `json:"title"`
	About    pStr            `json:"about"`
	Symbol   string          `json:"symbol"`
	Refs     map[string]pRef `json:"refs"`
	Sections []pSection      `json:"sections"`
}

type pRef struct {
	Path  string `json:"path"`
	Items string `json:"items"`
	Key   string `json:"key"`
	Label string `json:"label"`
}

type pSection struct {
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	Title      *pStr     `json:"title"`
	Body       *pStr     `json:"body"`
	Source     *pSource  `json:"source"`
	Key        string    `json:"key"`
	Columns    []pColumn `json:"columns"`
	Filter     []string  `json:"filter"`
	Hide       *pHide    `json:"hide"`
	Paging     *pPaging  `json:"paging"`
	Empty      *pStr     `json:"empty"`
	Actions    []pAction `json:"actions"`
	RowActions []pAction `json:"rowActions"`
	Detail     *pDetail  `json:"detail"`
}

type pSource struct {
	Path  string `json:"path"`
	Items string `json:"items"`
}

type pColumn struct {
	Field  string          `json:"field"`
	Kind   string          `json:"kind"`
	Label  pStr            `json:"label"`
	Yes    *pStr           `json:"yes"`
	No     *pStr           `json:"no"`
	Values map[string]pStr `json:"values"`
	Ref    string          `json:"ref"`
}

type pHide struct {
	Field  string          `json:"field"`
	Equals json.RawMessage `json:"equals"`
	Toggle pStr            `json:"toggle"`
}

type pPaging struct {
	Next  string `json:"next"`
	Param string `json:"param"`
}

type pDetail struct {
	Title    string     `json:"title"` // a row field
	Sections []pSection `json:"sections"`
}

type pAction struct {
	ID          string              `json:"id"`
	Label       pStr                `json:"label"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Query       string              `json:"query"`
	Body        map[string]pBodyVal `json:"body"`
	Fields      []pField            `json:"fields"`
	When        *pWhen              `json:"when"`
	Confirm     *pConfirm           `json:"confirm"`
	Destructive bool                `json:"destructive"`
}

type pBodyVal struct {
	Field *string         `json:"field"`
	Row   *string         `json:"row"`
	Const json.RawMessage `json:"const"`
}

type pWhen struct {
	Field  string          `json:"field"`
	Equals json.RawMessage `json:"equals"`
}

type pConfirm struct {
	Title       pStr `json:"title"`
	Message     pStr `json:"message"`
	Button      pStr `json:"button"`
	Destructive bool `json:"destructive"`
}

type pField struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Label     pStr      `json:"label"`
	Required  bool      `json:"required"`
	MaxLength int       `json:"maxLength"`
	Default   *pDefault `json:"default"`
	Options   []pOption `json:"options"`
	Ref       string    `json:"ref"`
}

type pDefault struct {
	Const json.RawMessage `json:"const"`
	Row   *string         `json:"row"`
}

type pOption struct {
	Value string `json:"value"`
	Label pStr   `json:"label"`
}

// --- the validator ---

var (
	reID       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reName     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	reLiteral  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	rePlace    = regexp.MustCompile(`^\{(row|field|parent)\.([A-Za-z][A-Za-z0-9_]{0,63})\}$`)
	reQueryKV  = regexp.MustCompile(`^([a-z][a-z0-9_]{0,31})=(.*)$`)
	reQueryLit = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,64}$`)
	reMsgPlace = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_]{0,63})\}`)
)

// ValidatePanel: one description against the provider's registered routes. Strict: unknown keys (exact spelling),
// anything above the limits, a route the provider did not register, a path template that is not whole segments, a
// destructive action without a confirmation, a string without en and sl.
func ValidatePanel(raw []byte, p PanelProvider) error {
	if len(raw) > maxPanelBytes {
		return fmt.Errorf("the description is larger than %d bytes", maxPanelBytes)
	}
	var probe struct {
		Format *int `json:"format"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Format == nil {
		return fmt.Errorf("not a JSON object with a format")
	}
	if *probe.Format != PanelFormat {
		return fmt.Errorf("format %d is not supported (this gateway knows format %d)", *probe.Format, PanelFormat)
	}
	var d pPanel
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return err
	}
	var generic any
	_ = json.Unmarshal(raw, &generic)
	if key := exactKeys(generic, reflect.TypeOf(d)); key != "" {
		return fmt.Errorf("unknown key %q", key)
	}
	v := &panelCheck{p: p, d: d}
	return v.panel()
}

// exactKeys returns the first object key that is not the exact (case-sensitive) json tag of the struct it lands in.
func exactKeys(v any, t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch x := v.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			tags := map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				tags[t.Field(i).Tag.Get("json")] = t.Field(i).Type
			}
			for k, e := range x {
				ft, ok := tags[k]
				if !ok {
					return k
				}
				if bad := exactKeys(e, ft); bad != "" {
					return bad
				}
			}
		case reflect.Map:
			for _, e := range x {
				if bad := exactKeys(e, t.Elem()); bad != "" {
					return bad
				}
			}
		}
	case []any:
		if t.Kind() == reflect.Slice {
			for _, e := range x {
				if bad := exactKeys(e, t.Elem()); bad != "" {
					return bad
				}
			}
		}
	}
	return ""
}

type panelCheck struct {
	p PanelProvider
	d pPanel
}

func (c *panelCheck) panel() error {
	d := c.d
	if !reID.MatchString(d.Provider) || d.Provider != c.p.ID {
		return fmt.Errorf("provider %q is not %q", d.Provider, c.p.ID)
	}
	if name, ok := strings.CutPrefix(d.ID, d.Provider+"."); !ok || !reID.MatchString(name) {
		return fmt.Errorf("id %q must be %s.<name>", d.ID, d.Provider)
	}
	if err := firstErr(str("title", d.Title, maxLabel), str("about", d.About, maxLabel)); err != nil {
		return err
	}
	if !slices.Contains(panelSymbols, d.Symbol) {
		return fmt.Errorf("symbol %q is not in the list", d.Symbol)
	}
	if len(d.Refs) > maxRefs {
		return fmt.Errorf("more than %d refs", maxRefs)
	}
	for name, r := range d.Refs {
		if !reID.MatchString(name) || !reName.MatchString(r.Items) || !reName.MatchString(r.Key) || !reName.MatchString(r.Label) {
			return fmt.Errorf("ref %q: bad name, items, key or label", name)
		}
		if err := c.path("ref "+name, "GET", r.Path, tpl{}); err != nil {
			return err
		}
	}
	return c.sections("", d.Sections, false)
}

// tpl says which placeholders a template may use: row (the list's row), parent (the row a detail page was opened for),
// fields (the ids of the action's form).
type tpl struct {
	row, parent bool
	fields      map[string]bool
}

func (c *panelCheck) sections(where string, ss []pSection, nested bool) error {
	if len(ss) == 0 || len(ss) > maxSections {
		return fmt.Errorf("%ssections: 1 to %d", where, maxSections)
	}
	seen := map[string]bool{}
	for i, s := range ss {
		w := fmt.Sprintf("%ssection %d: ", where, i)
		if !reID.MatchString(s.ID) || seen[s.ID] {
			return fmt.Errorf("%sbad or repeated id %q", w, s.ID)
		}
		seen[s.ID] = true
		switch s.Kind {
		case "text":
			if s.Body == nil || s.Title != nil || s.Source != nil || s.Key != "" || s.Columns != nil || s.Filter != nil || s.Hide != nil ||
				s.Paging != nil || s.Empty != nil || s.Actions != nil || s.RowActions != nil || s.Detail != nil {
				return fmt.Errorf("%sa text section has only id and body", w)
			}
			if err := str("body", *s.Body, maxMessage); err != nil {
				return fmt.Errorf("%s%w", w, err)
			}
		case "list":
			if err := c.list(w, s, nested); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%sunknown kind %q", w, s.Kind)
		}
	}
	return nil
}

func (c *panelCheck) list(w string, s pSection, nested bool) error {
	if s.Body != nil || s.Source == nil || s.Title == nil {
		return fmt.Errorf("%sa list needs title and source, and has no body", w)
	}
	if err := firstErr(str("title", *s.Title, maxLabel), optStr("empty", s.Empty, maxLabel)); err != nil {
		return fmt.Errorf("%s%w", w, err)
	}
	if !reName.MatchString(s.Source.Items) || !reName.MatchString(s.Key) {
		return fmt.Errorf("%sbad items or key", w)
	}
	if err := c.path(w+"source", "GET", s.Source.Path, tpl{parent: nested}); err != nil {
		return err
	}
	if len(s.Columns) == 0 || len(s.Columns) > maxColumns {
		return fmt.Errorf("%scolumns: 1 to %d", w, maxColumns)
	}
	for _, col := range s.Columns {
		if err := c.column(col); err != nil {
			return fmt.Errorf("%scolumn %q: %w", w, col.Field, err)
		}
	}
	if len(s.Filter) > maxFilterFields {
		return fmt.Errorf("%sfilter: at most %d fields", w, maxFilterFields)
	}
	for _, f := range s.Filter {
		if !reName.MatchString(f) {
			return fmt.Errorf("%sfilter: bad field %q", w, f)
		}
	}
	if h := s.Hide; h != nil {
		if !reName.MatchString(h.Field) || !scalar(h.Equals) {
			return fmt.Errorf("%shide: bad field or equals", w)
		}
		if err := str("hide.toggle", h.Toggle, maxLabel); err != nil {
			return fmt.Errorf("%s%w", w, err)
		}
	}
	if pg := s.Paging; pg != nil && (!reName.MatchString(pg.Next) || !reName.MatchString(pg.Param)) {
		return fmt.Errorf("%spaging: bad next or param", w)
	}
	if len(s.Actions) > maxActions || len(s.RowActions) > maxActions {
		return fmt.Errorf("%sat most %d actions", w, maxActions)
	}
	for _, a := range s.Actions {
		if err := c.action(a, tpl{parent: nested}); err != nil {
			return fmt.Errorf("%saction %q: %w", w, a.ID, err)
		}
	}
	for _, a := range s.RowActions {
		if err := c.action(a, tpl{row: true, parent: nested}); err != nil {
			return fmt.Errorf("%srow action %q: %w", w, a.ID, err)
		}
	}
	if s.Detail != nil {
		if nested {
			return fmt.Errorf("%sdetail pages nest only one level", w)
		}
		if !reName.MatchString(s.Detail.Title) {
			return fmt.Errorf("%sdetail title: bad row field", w)
		}
		return c.sections(w+"detail ", s.Detail.Sections, true)
	}
	return nil
}

func (c *panelCheck) column(col pColumn) error {
	if !reName.MatchString(col.Field) {
		return fmt.Errorf("bad field")
	}
	if err := str("label", col.Label, maxLabel); err != nil {
		return err
	}
	extra := func(yesNo, values, ref bool) error {
		if (col.Yes != nil || col.No != nil) != yesNo || (col.Values != nil) != values || (col.Ref != "") != ref {
			return fmt.Errorf("kind %s: wrong extra keys", col.Kind)
		}
		return nil
	}
	switch col.Kind {
	case "text", "multiline", "time":
		return extra(false, false, false)
	case "bool":
		if err := extra(true, false, false); err != nil {
			return err
		}
		if col.Yes == nil || col.No == nil {
			return fmt.Errorf("bool needs yes and no")
		}
		return firstErr(str("yes", *col.Yes, maxLabel), str("no", *col.No, maxLabel))
	case "enum":
		if err := extra(false, true, false); err != nil {
			return err
		}
		if len(col.Values) == 0 || len(col.Values) > maxOptions {
			return fmt.Errorf("enum: 1 to %d values", maxOptions)
		}
		for k, l := range col.Values {
			if k == "" || len(k) > 64 {
				return fmt.Errorf("enum: bad value key")
			}
			if err := str("value "+k, l, maxLabel); err != nil {
				return err
			}
		}
		return nil
	case "ref":
		if err := extra(false, false, true); err != nil {
			return err
		}
		if _, ok := c.d.Refs[col.Ref]; !ok {
			return fmt.Errorf("ref %q is not declared", col.Ref)
		}
		return nil
	}
	return fmt.Errorf("unknown column kind %q", col.Kind)
}

func (c *panelCheck) action(a pAction, t tpl) error {
	if !reID.MatchString(a.ID) {
		return fmt.Errorf("bad id")
	}
	if err := str("label", a.Label, maxLabel); err != nil {
		return err
	}
	switch a.Method {
	case "POST", "PUT", "PATCH", "DELETE":
	default:
		return fmt.Errorf("method %q", a.Method)
	}
	if len(a.Fields) > maxFields {
		return fmt.Errorf("at most %d fields", maxFields)
	}
	t.fields = map[string]bool{}
	for _, f := range a.Fields {
		if err := c.field(f, t); err != nil {
			return fmt.Errorf("field %q: %w", f.ID, err)
		}
		if t.fields[f.ID] {
			return fmt.Errorf("field %q repeated", f.ID)
		}
		t.fields[f.ID] = true
	}
	if err := c.path("path", a.Method, a.Path, t); err != nil {
		return err
	}
	if err := query(a.Query, t); err != nil {
		return err
	}
	if len(a.Body) > maxBodyKeys || (a.Method == "DELETE" && len(a.Body) > 0) {
		return fmt.Errorf("body: at most %d keys, none on DELETE", maxBodyKeys)
	}
	for k, b := range a.Body {
		n := 0
		if b.Field != nil {
			n++
			if !t.fields[*b.Field] {
				return fmt.Errorf("body %q: no form field %q", k, *b.Field)
			}
		}
		if b.Row != nil {
			n++
			if !t.row || !reName.MatchString(*b.Row) {
				return fmt.Errorf("body %q: row is for row actions with a plain field name", k)
			}
		}
		if b.Const != nil {
			n++
			if !scalar(b.Const) {
				return fmt.Errorf("body %q: const must be a string, number or boolean", k)
			}
		}
		if !reName.MatchString(k) || n != 1 {
			return fmt.Errorf("body %q: a name and exactly one of field, row, const", k)
		}
	}
	if w := a.When; w != nil && (!t.row || !reName.MatchString(w.Field) || !scalar(w.Equals)) {
		return fmt.Errorf("when: for row actions, a field and a scalar")
	}
	if a.Method == "DELETE" || a.Destructive {
		if a.Confirm == nil {
			return fmt.Errorf("a destructive action needs a confirmation")
		}
	}
	if cf := a.Confirm; cf != nil {
		for _, s := range []struct {
			n string
			v pStr
			m int
		}{{"confirm.title", cf.Title, maxLabel}, {"confirm.message", cf.Message, maxMessage}, {"confirm.button", cf.Button, maxLabel}} {
			if err := str(s.n, s.v, s.m); err != nil {
				return err
			}
			for _, txt := range []string{s.v.En, s.v.Sl} {
				if err := messagePlaceholders(txt, t.row); err != nil {
					return fmt.Errorf("%s: %w", s.n, err)
				}
			}
		}
	}
	return nil
}

func (c *panelCheck) field(f pField, t tpl) error {
	if !reName.MatchString(f.ID) {
		return fmt.Errorf("bad id")
	}
	if err := str("label", f.Label, maxLabel); err != nil {
		return err
	}
	if f.MaxLength < 0 || f.MaxLength > maxFieldLength {
		return fmt.Errorf("maxLength 0 to %d", maxFieldLength)
	}
	if f.Default != nil {
		if (f.Default.Const != nil) == (f.Default.Row != nil) {
			return fmt.Errorf("default: exactly one of const, row")
		}
		if f.Default.Row != nil && (!t.row || !reName.MatchString(*f.Default.Row)) {
			return fmt.Errorf("default: row is for row actions with a plain field name")
		}
		if f.Default.Const != nil && !scalar(f.Default.Const) {
			return fmt.Errorf("default: const must be a scalar")
		}
	}
	other := func(maxLen, opts, ref bool) error {
		if (f.MaxLength != 0) && !maxLen || (f.Options != nil) != opts || (f.Ref != "") != ref {
			return fmt.Errorf("type %s: wrong extra keys", f.Type)
		}
		return nil
	}
	switch f.Type {
	case "text", "toggle":
		return other(f.Type == "text", false, false)
	case "choice":
		if err := other(false, true, false); err != nil {
			return err
		}
		if len(f.Options) == 0 || len(f.Options) > maxOptions {
			return fmt.Errorf("choice: 1 to %d options", maxOptions)
		}
		for _, o := range f.Options {
			if o.Value == "" || len(o.Value) > 64 {
				return fmt.Errorf("choice: bad option value")
			}
			if err := str("option "+o.Value, o.Label, maxLabel); err != nil {
				return err
			}
		}
		return nil
	case "ref":
		if err := other(false, false, true); err != nil {
			return err
		}
		if _, ok := c.d.Refs[f.Ref]; !ok {
			return fmt.Errorf("ref %q is not declared", f.Ref)
		}
		return nil
	}
	return fmt.Errorf("unknown field type %q", f.Type)
}

// path checks a path template and that it resolves to a route the provider registered.
func (c *panelCheck) path(where, method, path string, t tpl) error {
	if len(path) > maxPathBytes || !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return fmt.Errorf("%s: a path starts with / and does not end with one", where)
	}
	segs := strings.Split(path[1:], "/")
	pat := make([]string, len(segs))
	for i, s := range segs {
		if m := rePlace.FindStringSubmatch(s); m != nil {
			ok := (m[1] == "row" && t.row) || (m[1] == "parent" && t.parent) || (m[1] == "field" && t.fields[m[2]])
			if !ok {
				return fmt.Errorf("%s: placeholder %s is not available here", where, s)
			}
			pat[i] = "{}"
		} else if reLiteral.MatchString(s) {
			pat[i] = s
		} else {
			return fmt.Errorf("%s: segment %q is neither a plain name nor exactly one placeholder", where, s)
		}
	}
	if c.p.ID != "builtin" && !strings.HasPrefix(path, "/plugins/"+c.p.ID+"/") {
		return fmt.Errorf("%s: the routes of provider %s live under /plugins/%s/", where, c.p.ID, c.p.ID)
	}
	if route := method + " " + strings.Join(pat, "/"); !slices.Contains(c.p.Routes, route) {
		return fmt.Errorf("%s: route %q is not registered by provider %s", where, route, c.p.ID)
	}
	return nil
}

func query(q string, t tpl) error {
	if q == "" {
		return nil
	}
	parts := strings.Split(q, "&")
	if len(parts) > maxQueryParams {
		return fmt.Errorf("query: at most %d parameters", maxQueryParams)
	}
	for _, p := range parts {
		m := reQueryKV.FindStringSubmatch(p)
		if m == nil {
			return fmt.Errorf("query: %q is not name=value", p)
		}
		if pm := rePlace.FindStringSubmatch(m[2]); pm != nil {
			if !((pm[1] == "row" && t.row) || (pm[1] == "parent" && t.parent) || (pm[1] == "field" && t.fields[pm[2]])) {
				return fmt.Errorf("query: placeholder %s is not available here", m[2])
			}
		} else if !reQueryLit.MatchString(m[2]) {
			return fmt.Errorf("query: value %q is neither plain nor exactly one placeholder", m[2])
		}
	}
	return nil
}

// messagePlaceholders: {field} in a confirmation takes a row value; no other braces.
func messagePlaceholders(s string, row bool) error {
	rest := reMsgPlace.ReplaceAllString(s, "")
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("a stray brace")
	}
	if !row && reMsgPlace.MatchString(s) {
		return fmt.Errorf("placeholders need a row")
	}
	return nil
}

// str: both languages, non-empty, at most max characters, no control or format characters (bidi tricks included).
func str(name string, s pStr, max int) error {
	for lang, v := range map[string]string{"en": s.En, "sl": s.Sl} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s.%s is empty (en and sl are required)", name, lang)
		}
		if utf8.RuneCountInString(v) > max || !utf8.ValidString(v) {
			return fmt.Errorf("%s.%s is longer than %d characters", name, lang, max)
		}
		for _, r := range v {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
				return fmt.Errorf("%s.%s has a control character", name, lang)
			}
		}
	}
	return nil
}

func optStr(name string, s *pStr, max int) error {
	if s == nil {
		return nil
	}
	return str(name, *s, max)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// scalar: a JSON string (at most maxLabel), number or boolean.
func scalar(raw json.RawMessage) bool {
	if raw == nil {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x) <= maxLabel
	case float64, bool:
		return true
	}
	return false
}

// --- the registry and GET /panels ---

// panelProviders: the registered providers (only the built-in one in this step).
func panelProviders() []PanelProvider { return []PanelProvider{builtinProvider()} }

// panelsAnswer: the validated descriptions of all providers. A panel that fails validation is left out and logged; a
// provider has at most maxPanelsPerP panels.
func (a *API) panelsAnswer(ps []PanelProvider) []json.RawMessage {
	out := []json.RawMessage{}
	for _, p := range ps {
		for i, raw := range p.Panels {
			if i >= maxPanelsPerP {
				a.o.Log("panels: provider %s has more than %d panels; the rest is left out", p.ID, maxPanelsPerP)
				break
			}
			if err := ValidatePanel(raw, p); err != nil {
				a.o.Log("panels: a panel of provider %s is left out: %v", p.ID, err)
				continue
			}
			var buf bytes.Buffer
			if json.Compact(&buf, raw) == nil {
				out = append(out, json.RawMessage(buf.Bytes()))
			}
		}
	}
	return out
}

// adminPanels serves GET /panels. It needs no store, so it answers while the project database is down.
func (a *API) adminPanels(method string) Response {
	if method != http.MethodGet {
		return resp(405, errorBody(405, "Use GET /panels"))
	}
	return resp(200, map[string]any{"format": PanelFormat, "panels": a.panelsAnswer(panelProviders())})
}
