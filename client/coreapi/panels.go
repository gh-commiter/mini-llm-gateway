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
	// SecretRoutes: the registered routes that may receive the value of a secret field (in the body). A secret field in an action whose
	// route is not listed here is refused.
	SecretRoutes []string
	// ConfirmRoutes: the registered routes on which every action must carry a confirm (a send that changes the other side): an action
	// on such a route without one is refused, whatever its method.
	ConfirmRoutes []string
	Strings       map[string]map[string]string // locale -> {key: text}; "C" is the base and holds every key
}

//go:embed panels/*.json
var builtinPanelFiles embed.FS

// BuiltinRoutes: the routes the built-in panels may use. The core's own routes plus the gateway's GET devices.
// BuiltinSecretRoutes: the built-in routes that may receive a secret (the upstream's token, in the body of its add).
var BuiltinSecretRoutes = []string{"PUT upstreams/{}"}

// BuiltinConfirmRoutes: the built-in routes whose actions must carry a confirmation: the sends to an upstream, the links that decide what is sent, the settling of a conflict and the removal.
var BuiltinConfirmRoutes = []string{"POST upstream-push", "POST upstream-pull", "PUT upstreams/{}", "DELETE upstreams/{}", "DELETE upstreams/{}/links/{}", "PUT upstreams/{}/links/{}", "POST upstream-resolve"}

var BuiltinRoutes = []string{"GET users", "POST users", "PATCH users/{}", "GET bindings", "PUT bindings/{}", "DELETE bindings/{}", "GET devices", "GET kb/submissions", "GET kb/submissions/{}", "GET kb/submissions/{}/versions/{}", "POST kb/submissions/{}/accept", "POST kb/submissions/{}/reject", "GET kb/notes", "GET kb/notes/{}", "GET kb/faq", "GET projects", "GET teams", "POST teams", "PATCH teams/{}", "GET teams/{}/members", "PUT teams/{}/members/{}", "DELETE teams/{}/members/{}", "GET teams/{}/projects", "PUT teams/{}/projects/{}", "DELETE teams/{}/projects/{}", "GET teams/{}/assignments", "PUT teams/{}/assignments", "DELETE teams/{}/assignments", "POST projects", "PATCH projects/{}", "GET projects/{}/members", "PUT projects/{}/members/{}", "DELETE projects/{}/members/{}", "GET projects/{}/remotes", "POST projects/{}/remotes", "GET projects/{}/depends", "PUT projects/{}/depends/{}", "DELETE projects/{}/depends/{}", "PUT projects/{}/depends/teams/{}", "DELETE projects/{}/depends/teams/{}", "GET grants", "POST grants", "DELETE grants", "GET upstreams", "PUT upstreams/{}", "DELETE upstreams/{}", "GET upstreams/{}/probe", "PUT upstreams/{}/links/{}", "DELETE upstreams/{}/links/{}", "GET upstream-links/{}", "POST upstream-push", "POST upstream-pull", "POST upstream-resolve", "GET usage/labels/settings", "PUT usage/labels/settings", "GET usage/labels/agreement"}

// panelSymbols: the names a renderer maps to its icons.
var panelSymbols = []string{"person.2", "person.3", "folder", "book", "tray", "gearshape", "doc.text", "key", "list.bullet"}

//go:embed panels/strings/*.json
var builtinStringFiles embed.FS

// builtinPanelOrder: the order of the built-in panels in GET /panels and so in the sidebar.
var builtinPanelOrder = []string{"builtin.users.json", "builtin.teams.json", "builtin.kb.json", "builtin.upstreams.json", "builtin.labels.json"}

func builtinProvider() PanelProvider {
	p := PanelProvider{ID: "builtin", Routes: BuiltinRoutes, SecretRoutes: BuiltinSecretRoutes, ConfirmRoutes: BuiltinConfirmRoutes, Strings: map[string]map[string]string{}}
	sents, _ := builtinStringFiles.ReadDir("panels/strings")
	for _, e := range sents {
		var t map[string]string
		if b, err := builtinStringFiles.ReadFile("panels/strings/" + e.Name()); err == nil && json.Unmarshal(b, &t) == nil {
			p.Strings[strings.TrimSuffix(e.Name(), ".json")] = t
		}
	}
	ents, _ := builtinPanelFiles.ReadDir("panels")
	files := map[string][]byte{}
	for _, e := range ents {
		if b, err := builtinPanelFiles.ReadFile("panels/" + e.Name()); err == nil {
			files[e.Name()] = b
		}
	}
	for _, name := range builtinPanelOrder { // the order of the sidebar is given here, never by file names
		if b, ok := files[name]; ok {
			p.Panels = append(p.Panels, b)
			delete(files, name)
		}
	}
	for _, e := range ents { // a file that is not listed comes last (a test fails for it)
		if b, ok := files[e.Name()]; ok {
			p.Panels = append(p.Panels, b)
		}
	}
	return p
}

// --- the description ---

type pPanel struct {
	Format   int             `json:"format"`
	ID       string          `json:"id"`
	Provider string          `json:"provider"`
	Title    string          `json:"title"`
	About    string          `json:"about"`
	Symbol   string          `json:"symbol"`
	Refs     map[string]pRef `json:"refs"`
	Sections []pSection      `json:"sections"`
}

type pRef struct {
	Path  string `json:"path"`
	Items string `json:"items"`
	Key   string `json:"key"`
	Label string `json:"label"`
	// Skip: entries whose field equals the value are not offered in pickers (an archived project, an inactive user); they still
	// give the labels of values that refer to them.
	Skip *pWhen `json:"skip"`
}

type pSection struct {
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	Title      *string   `json:"title"`
	Body       *string   `json:"body"`
	Source     *pSource  `json:"source"`
	Key        string    `json:"key"`
	Columns    []pColumn `json:"columns"`
	Filter     []string  `json:"filter"`
	Hide       *pHide    `json:"hide"`
	Paging     *pPaging  `json:"paging"`
	Empty      *string   `json:"empty"`
	Actions    []pAction `json:"actions"`
	RowActions []pAction `json:"rowActions"`
	Detail     *pDetail  `json:"detail"`
	Scope      *pScope   `json:"scope"` // list: a selector whose value is sent as a query parameter
	When       *pWhen    `json:"when"`  // record in a detail page: shown only when the opened row's field equals
}

// pScope: a picker above a list ("which project", "which team"). Its options are the entries of the ref; the chosen key is
// sent as `param=<key>` to the list's source, and the list loads once an option is chosen (the first one at the start).
type pScope struct {
	Ref   string `json:"ref"`
	Param string `json:"param"`
	Label string `json:"label"`
}

type pSource struct {
	Path  string `json:"path"`
	Items string `json:"items"`
}

type pColumn struct {
	Field  string            `json:"field"`
	Kind   string            `json:"kind"`
	Label  string            `json:"label"`
	Yes    *string           `json:"yes"`
	No     *string           `json:"no"`
	Values map[string]string `json:"values"`
	Ref    string            `json:"ref"`
}

type pHide struct {
	Field  string          `json:"field"`
	Equals json.RawMessage `json:"equals"`
	Toggle string          `json:"toggle"`
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
	Label       string              `json:"label"`
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Query       string              `json:"query"`
	Body        map[string]pBodyVal `json:"body"`
	Fields      []pField            `json:"fields"`
	When        *pWhen              `json:"when"`
	Confirm     *pConfirm           `json:"confirm"`
	Destructive bool                `json:"destructive"`
	// Result: what the action's answer is shown as after it succeeded: a list of rows (Items names the array in the answer) or, without
	// Items, the answer itself as one row. Read-only; the values are plain text like every other.
	Result *pResult `json:"result"`
}

type pResult struct {
	Items   string    `json:"items"`
	Title   string    `json:"title"`
	Columns []pColumn `json:"columns"`
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
	Title       string `json:"title"`
	Message     string `json:"message"`
	Button      string `json:"button"`
	Destructive bool   `json:"destructive"`
}

type pField struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Label     string    `json:"label"`
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
	Label string `json:"label"`
}

// --- the validator ---

var (
	reID        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reName      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	reLiteral   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	rePlace     = regexp.MustCompile(`^\{(row|field|parent)\.([A-Za-z][A-Za-z0-9_]{0,63})\}$`)
	reQueryKV   = regexp.MustCompile(`^([a-z][a-z0-9_]{0,31})=(.*)$`)
	reQueryLit  = regexp.MustCompile(`^[A-Za-z0-9_.-]{0,64}$`)
	reQueryName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reKey       = regexp.MustCompile(`^[a-z0-9._-]{1,120}$`)
)

// ValidatePanel: one description against the provider's registered routes. Strict: unknown keys (exact spelling),
// anything above the limits, a route the provider did not register, a path template that is not whole segments, a
// destructive action without a confirmation, a text that is not a key of the panel's namespace.
func ValidatePanel(raw []byte, p PanelProvider) error {
	_, err := PanelKeys(raw, p)
	return err
}

// PanelKeys validates like ValidatePanel and returns the string keys the description uses, each with the longest text its
// position allows.
func PanelKeys(raw []byte, p PanelProvider) (map[string]int, error) {
	if len(raw) > maxPanelBytes {
		return nil, fmt.Errorf("the description is larger than %d bytes", maxPanelBytes)
	}
	var probe struct {
		Format *int `json:"format"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Format == nil {
		return nil, fmt.Errorf("not a JSON object with a format")
	}
	if *probe.Format != PanelFormat {
		return nil, fmt.Errorf("format %d is not supported (this gateway knows format %d)", *probe.Format, PanelFormat)
	}
	var d pPanel
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	var generic any
	_ = json.Unmarshal(raw, &generic)
	if key := exactKeys(generic, reflect.TypeOf(d)); key != "" {
		return nil, fmt.Errorf("unknown key %q", key)
	}
	v := &panelCheck{p: p, d: d, keys: map[string]int{}}
	if err := v.panel(); err != nil {
		return nil, err
	}
	return v.keys, nil
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
	p    PanelProvider
	d    pPanel
	keys map[string]int
}

func (c *panelCheck) panel() error {
	d := c.d
	if !reID.MatchString(d.Provider) || d.Provider != c.p.ID {
		return fmt.Errorf("provider %q is not %q", d.Provider, c.p.ID)
	}
	if name, ok := strings.CutPrefix(d.ID, d.Provider+"."); !ok || !reID.MatchString(name) {
		return fmt.Errorf("id %q must be %s.<name>", d.ID, d.Provider)
	}
	if err := firstErr(c.key("title", d.Title, maxLabel), c.key("about", d.About, maxLabel)); err != nil {
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
		if r.Skip != nil && (!reName.MatchString(r.Skip.Field) || !scalar(r.Skip.Equals)) {
			return fmt.Errorf("ref %q: skip needs a plain field name and a scalar", name)
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
	secret      map[string]bool // fields of type secret: their value goes into the body only, never into a path or a query
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
				s.Paging != nil || s.Empty != nil || s.Actions != nil || s.RowActions != nil || s.Detail != nil || s.Scope != nil || s.When != nil {
				return fmt.Errorf("%sa text section has only id and body", w)
			}
			if err := c.key("body", *s.Body, maxMessage); err != nil {
				return fmt.Errorf("%s%w", w, err)
			}
		case "list":
			if err := c.list(w, s, nested); err != nil {
				return err
			}
		case "record":
			if err := c.record(w, s, nested); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%sunknown kind %q", w, s.Kind)
		}
	}
	return nil
}

func (c *panelCheck) list(w string, s pSection, nested bool) error {
	if s.Body != nil || s.Source == nil || s.Title == nil || s.When != nil {
		return fmt.Errorf("%sa list needs title and source, and has no body or when", w)
	}
	if sc := s.Scope; sc != nil {
		if _, ok := c.d.Refs[sc.Ref]; !ok || !reQueryName.MatchString(sc.Param) {
			return fmt.Errorf("%sscope: an undeclared ref or a bad parameter name", w)
		}
		if s.Paging != nil && s.Paging.Param == sc.Param { // the paging cursor would overwrite the choice (a source path has no query of its own)
			return fmt.Errorf("%sscope and paging use the same parameter %q", w, sc.Param)
		}
		if err := c.key("scope.label", sc.Label, maxLabel); err != nil {
			return fmt.Errorf("%s%w", w, err)
		}
	}
	if err := firstErr(c.key("title", *s.Title, maxLabel), c.optKey("empty", s.Empty, maxLabel)); err != nil {
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
		if err := c.key("hide.toggle", h.Toggle, maxLabel); err != nil {
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

// record: one object read from the source's answer, shown as label and value pairs (the list's column kinds). In a detail page
// it may carry `when`: it is shown only when the opened row's field equals the value (nothing is requested otherwise).
func (c *panelCheck) record(w string, s pSection, nested bool) error {
	if s.Body != nil || s.Source == nil || s.Source.Items != "" || s.Key != "" || s.Filter != nil || s.Hide != nil || s.Paging != nil || s.Empty != nil ||
		s.Actions != nil || s.RowActions != nil || s.Detail != nil || s.Scope != nil {
		return fmt.Errorf("%sa record has id, title, source (a path), columns and perhaps when", w)
	}
	if err := c.optKey("title", s.Title, maxLabel); err != nil {
		return fmt.Errorf("%s%w", w, err)
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
	if wh := s.When; wh != nil && (!nested || !reName.MatchString(wh.Field) || !scalar(wh.Equals)) {
		return fmt.Errorf("%swhen: only in a detail page, with a field and a scalar", w)
	}
	return nil
}

func (c *panelCheck) column(col pColumn) error {
	if !reName.MatchString(col.Field) {
		return fmt.Errorf("bad field")
	}
	if err := c.key("label", col.Label, maxLabel); err != nil {
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
		return firstErr(c.key("yes", *col.Yes, maxLabel), c.key("no", *col.No, maxLabel))
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
			if err := c.key("value "+k, l, maxLabel); err != nil {
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
	if err := c.key("label", a.Label, maxLabel); err != nil {
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
	t.fields, t.secret = map[string]bool{}, map[string]bool{}
	for _, f := range a.Fields {
		if err := c.field(f, t); err != nil {
			return fmt.Errorf("field %q: %w", f.ID, err)
		}
		if t.fields[f.ID] {
			return fmt.Errorf("field %q repeated", f.ID)
		}
		t.fields[f.ID] = true
		if f.Type == "secret" {
			t.secret[f.ID] = true
		}
	}
	if err := c.path("path", a.Method, a.Path, t); err != nil {
		return err
	}
	if err := query(a.Query, t); err != nil {
		return err
	}
	if len(t.secret) > 0 {
		// A secret goes only to a route its provider registered as taking one, and only in the body.
		if route := c.routeKey(a.Method, a.Path); !slices.Contains(c.p.SecretRoutes, route) {
			return fmt.Errorf("a secret field needs a route that the provider registered as taking a secret: %s is not", route)
		}
		used := map[string]bool{}
		for _, b := range a.Body {
			if b.Field != nil && t.secret[*b.Field] {
				used[*b.Field] = true
			}
		}
		for id := range t.secret {
			if !used[id] {
				return fmt.Errorf("secret field %q is not sent in the body", id)
			}
		}
	}
	if r := a.Result; r != nil {
		if r.Items != "" && !reName.MatchString(r.Items) {
			return fmt.Errorf("result: bad items name")
		}
		if r.Title != "" {
			if err := c.key("result.title", r.Title, maxLabel); err != nil {
				return err
			}
		}
		if len(r.Columns) == 0 || len(r.Columns) > maxColumns {
			return fmt.Errorf("result: columns 1 to %d", maxColumns)
		}
		for _, col := range r.Columns {
			if err := c.column(col); err != nil {
				return fmt.Errorf("result column %q: %w", col.Field, err)
			}
		}
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
	if route := c.routeKey(a.Method, a.Path); a.Confirm == nil && slices.Contains(c.p.ConfirmRoutes, route) {
		return fmt.Errorf("an action on %s needs a confirmation", route)
	}
	if cf := a.Confirm; cf != nil {
		if err := firstErr(c.key("confirm.title", cf.Title, maxLabel), c.key("confirm.message", cf.Message, maxMessage), c.key("confirm.button", cf.Button, maxLabel)); err != nil {
			return err
		}
	}
	return nil
}

func (c *panelCheck) field(f pField, t tpl) error {
	if !reName.MatchString(f.ID) {
		return fmt.Errorf("bad id")
	}
	if err := c.key("label", f.Label, maxLabel); err != nil {
		return err
	}
	if f.MaxLength < 0 || f.MaxLength > maxFieldLength {
		return fmt.Errorf("maxLength 0 to %d", maxFieldLength)
	}
	if f.Type == "secret" && f.Default != nil {
		return fmt.Errorf("a secret has no default")
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
	case "text", "toggle", "secret":
		return other(f.Type != "toggle", false, false)
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
			if err := c.key("option "+o.Value, o.Label, maxLabel); err != nil {
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

// routeKey: the registered pattern a path template resolves to, "METHOD users/{}" (placeholders as {}).
func (c *panelCheck) routeKey(method, path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, s := range segs {
		if rePlace.MatchString(s) {
			segs[i] = "{}"
		}
	}
	return method + " " + strings.Join(segs, "/")
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
			ok := (m[1] == "row" && t.row) || (m[1] == "parent" && t.parent) || (m[1] == "field" && t.fields[m[2]] && !t.secret[m[2]])
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
			if !((pm[1] == "row" && t.row) || (pm[1] == "parent" && t.parent) || (pm[1] == "field" && t.fields[pm[2]] && !t.secret[pm[2]])) {
				return fmt.Errorf("query: placeholder %s is not available here", m[2])
			}
		} else if !reQueryLit.MatchString(m[2]) {
			return fmt.Errorf("query: value %q is neither plain nor exactly one placeholder", m[2])
		}
	}
	return nil
}

// key: a text of the description is a key of the provider's string tables, never the text itself: [a-z0-9._-], at most
// 120 characters, inside the namespace of the panel's id. `max` is the longest text the table may give it.
func (c *panelCheck) key(name, k string, max int) error {
	if !reKey.MatchString(k) || !strings.HasPrefix(k, c.d.ID+".") || strings.Contains(k, "..") || strings.HasSuffix(k, ".") {
		return fmt.Errorf("%s: %q is not a string key of panel %s (a key is %s.<name>, characters a-z 0-9 . _ -)", name, k, c.d.ID, c.d.ID)
	}
	if c.keys[k] < max {
		c.keys[k] = max
	}
	return nil
}

func (c *panelCheck) optKey(name string, s *string, max int) error {
	if s == nil {
		return nil
	}
	return c.key(name, *s, max)
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

// builtinProviderID is reserved: only the code of this package registers it; a provider that is added through Options may
// not use it, so a plugin can never get the built-in routes.
const builtinProviderID = "builtin"

// providers: the built-in provider, then those added through Options (a reserved or repeated id, or one that is not a
// valid id, is left out and logged).
func (a *API) providers() []PanelProvider {
	ps := []PanelProvider{builtinProvider()}
	seen := map[string]bool{builtinProviderID: true}
	for _, p := range a.o.PanelProviders {
		if !reID.MatchString(p.ID) || seen[p.ID] {
			a.o.Log("panels: a provider with the id %q is refused (reserved, repeated or not a valid id)", p.ID)
			continue
		}
		seen[p.ID] = true
		ps = append(ps, p)
	}
	return ps
}

// panelEntry is a description together with the id of the provider that registered it, as the registry knows it. A renderer
// takes the routes from THIS id, never from the provider field inside the description.
type panelEntry struct {
	Provider    string          `json:"provider"`
	Description json.RawMessage `json:"description"`
}

// panelsAnswer: the validated descriptions of all providers. A panel that fails validation is left out and logged; a
// provider has at most maxPanelsPerP panels; a provider whose string tables do not fit its panels is left out as a whole.
func (a *API) panelsAnswer(ps []PanelProvider) []panelEntry {
	out := []panelEntry{}
	for _, p := range ps {
		if err := checkProviderStrings(p); err != nil {
			a.o.Log("panels: provider %s is left out, its texts are broken: %v", p.ID, err)
			continue
		}
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
				out = append(out, panelEntry{Provider: p.ID, Description: json.RawMessage(buf.Bytes())})
			}
		}
	}
	return out
}

// adminPanels serves GET /panels. It needs no store, so it answers while the project database is down.
func (a *API) adminPanels(method string, seg []string, rawQuery string) Response {
	if method != http.MethodGet {
		return resp(405, errorBody(405, "Use GET /panels or GET /panels/strings?lang=<locale>"))
	}
	if len(seg) == 2 {
		return a.adminPanelStrings(rawQuery)
	}
	return resp(200, map[string]any{"format": PanelFormat, "panels": a.panelsAnswer(a.providers())})
}
