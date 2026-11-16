package core

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

type BundleProject struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}
type BundleMembership struct {
	Project string `json:"project"`
	User    string `json:"user"`
	Role    Role   `json:"role"`
}
type BundleGrant struct {
	Source string `json:"source"`
	Reader string `json:"reader"`
	Class  Class  `json:"class"`
}
type BundleNote struct {
	File       string     `json:"file"`
	Project    string     `json:"project"`
	Provenance Provenance `json:"provenance"`
}
type BundleManifest struct {
	Format        int                `json:"format"`
	Exported      string             `json:"exported"`
	OriginGateway string             `json:"origin_gateway"`
	Team          string             `json:"team"`
	Projects      []BundleProject    `json:"projects"`
	Users         []string           `json:"users"`
	Members       []string           `json:"members"`
	Memberships   []BundleMembership `json:"memberships"`
	Assignments   []BundleMembership `json:"assignments"`
	Grants        []BundleGrant      `json:"grants"`
	Notes         []BundleNote       `json:"notes"`
}
type BundleExport struct {
	Team        string   `json:"team"`
	Projects    []string `json:"projects"`
	WithPrivate bool     `json:"with_private"`
	Path        string   `json:"path"`
}
type BundleImport struct {
	Path   string            `json:"path"`
	DryRun bool              `json:"dry_run"`
	Map    map[string]string `json:"map"`
}
type BundleReport struct {
	Lines []string `json:"lines"`
}
type bundle struct {
	manifest BundleManifest
	files    map[string][]byte
}

func bundleKey(origin, slug string, id int64) string {
	return "gw:" + origin + ":" + slug + ":" + i64(id)
}

// ExportBundle is owner-only; the admin adapter authenticates the caller.
func (s *Store) ExportBundle(ctx context.Context, in BundleExport) error {
	b := bundle{files: map[string][]byte{}}
	m := &b.manifest
	m.Format, m.Exported, m.OriginGateway = 1, s.now().UTC().Format(time.RFC3339Nano), s.GatewayID()
	err := s.read(ctx, func(tx *sql.Tx) error {
		var team int64
		var archived bool
		if err := tx.QueryRow(`SELECT id,name,archived FROM teams WHERE name=? COLLATE NOCASE`, in.Team).Scan(&team, &m.Team, &archived); err != nil {
			return notFound(err)
		}
		if archived {
			return ErrInvalid{"archived team"}
		}
		wanted := map[string]bool{}
		for _, slug := range in.Projects {
			wanted[slug] = true
		}
		projects := map[int64]string{}
		rows, err := tx.Query(`SELECT p.id,p.slug,p.name FROM projects p JOIN team_projects t ON t.project_id=p.id WHERE t.team_id=? ORDER BY p.slug`, team)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var p BundleProject
			if err = rows.Scan(&id, &p.Slug, &p.Name); err != nil {
				rows.Close()
				return err
			}
			if len(wanted) == 0 || wanted[p.Slug] {
				projects[id] = p.Slug
				m.Projects = append(m.Projects, p)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for slug := range wanted {
			found := false
			for _, p := range m.Projects {
				if p.Slug == slug {
					found = true
				}
			}
			if !found {
				return ErrInvalid{"project outside team: " + slug}
			}
		}
		users := map[string]bool{}
		rows, err = tx.Query(`SELECT u.name FROM team_members t JOIN users u ON u.id=t.user_id WHERE t.team_id=? ORDER BY u.name`, team)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			users[name] = true
			m.Members = append(m.Members, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, table := range []string{"direct_memberships", "team_assignments"} {
			q := `SELECT a.project_id,u.name,a.role FROM ` + table + ` a JOIN users u ON u.id=a.user_id`
			var args []any
			if table == "team_assignments" {
				q += ` WHERE a.team_id=?`
				args = append(args, team)
			}
			rows, err = tx.Query(q+` ORDER BY a.project_id,u.name`, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var pid int64
				var v BundleMembership
				if err = rows.Scan(&pid, &v.User, &v.Role); err != nil {
					rows.Close()
					return err
				}
				if slug, ok := projects[pid]; ok {
					v.Project = slug
					users[v.User] = true
					if table == "direct_memberships" {
						m.Memberships = append(m.Memberships, v)
					} else {
						m.Assignments = append(m.Assignments, v)
					}
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		rows, err = tx.Query(`SELECT source_id,reader_id,class FROM grants ORDER BY source_id,reader_id,class`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var src, reader int64
			var c Class
			if err = rows.Scan(&src, &reader, &c); err != nil {
				rows.Close()
				return err
			}
			if projects[src] != "" && projects[reader] != "" {
				m.Grants = append(m.Grants, BundleGrant{projects[src], projects[reader], c})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		var index strings.Builder
		index.WriteString("# " + m.Team + "\n\n")
		for _, p := range m.Projects {
			var pid int64
			for id, slug := range projects {
				if slug == p.Slug {
					pid = id
				}
			}
			rows, err = tx.Query(`SELECT `+noteCols+` FROM notes n JOIN users u ON u.id=n.author_id WHERE project_id=? AND (class<>'private' OR ?) ORDER BY n.id`, pid, in.WithPrivate)
			if err != nil {
				return err
			}
			var notes []Note
			for rows.Next() {
				n, e := scanNote(rows, true)
				if e != nil {
					rows.Close()
					return e
				}
				notes = append(notes, n)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, n := range notes {
				if err = noteProvenance(tx, &n); err != nil {
					return err
				}
				file := p.Slug + "/" + i64(n.ID) + ".md"
				b.files[file] = Markdown(n, bundleKey(m.OriginGateway, p.Slug, n.ID))
				m.Notes = append(m.Notes, BundleNote{file, p.Slug, n.Provenance})
				users[n.Author] = true
				fmt.Fprintf(&index, "- [%s](%s)\n", strings.ReplaceAll(n.Title, "\n", " "), file)
			}
		}
		for name := range users {
			m.Users = append(m.Users, name)
		}
		sort.Strings(m.Users)
		b.files["INDEX.md"] = []byte(index.String())
		return nil
	})
	if err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b.files["manifest.json"] = manifest
	// Enforce the same limits on exports and imports, before creating the output.
	if _, err = validateBundle(b); err != nil {
		return err
	}
	if in.Path == "" {
		return ErrInvalid{"bundle path"}
	}
	f, err := os.OpenFile(in.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(in.Path)
		}
	}()
	protect := s.opts.ProtectFile
	if protect == nil {
		protect = protectFile
	}
	if err = protect(in.Path); err != nil {
		return err
	}
	z := zip.NewWriter(f)
	names := make([]string, 0, len(b.files))
	for name := range b.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0600)
		w, e := z.CreateHeader(h)
		if e != nil {
			return e
		}
		if _, e = w.Write(b.files[name]); e != nil {
			return e
		}
	}
	if err = z.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func safeBundlePath(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:") || path.IsAbs(name) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}
func readBundle(file string) (bundle, error) {
	b := bundle{files: map[string][]byte{}}
	fi, err := os.Lstat(file)
	if err != nil {
		return b, err
	}
	if !fi.Mode().IsRegular() {
		return b, ErrInvalid{"bundle is not a regular file"}
	}
	input, err := os.Open(file)
	if err != nil {
		return b, err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil {
		return b, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(fi, opened) {
		return b, ErrInvalid{"bundle file changed"}
	}
	z, err := zip.NewReader(input, opened.Size())
	if err != nil {
		return b, err
	}
	if len(z.File) > maxNotes+2 {
		return b, ErrTooLarge
	}
	var total int64
	for _, f := range z.File {
		if !safeBundlePath(f.Name) || !f.Mode().IsRegular() {
			return b, ErrInvalid{"unsafe zip entry: " + f.Name}
		}
		if _, ok := b.files[f.Name]; ok {
			return b, ErrInvalid{"duplicate zip entry"}
		}
		if f.UncompressedSize64 > uint64(maxBytes-total) {
			return b, ErrTooLarge
		}
		r, e := f.Open()
		if e != nil {
			return b, e
		}
		data, e := io.ReadAll(io.LimitReader(r, maxBytes-total+1))
		closeErr := r.Close()
		if e != nil {
			return b, e
		}
		if closeErr != nil {
			return b, closeErr
		}
		total += int64(len(data))
		if total > maxBytes {
			return b, ErrTooLarge
		}
		b.files[f.Name] = data
	}
	d := json.NewDecoder(bytes.NewReader(b.files["manifest.json"]))
	d.DisallowUnknownFields()
	if err = d.Decode(&b.manifest); err != nil {
		return b, ErrInvalid{"manifest: " + err.Error()}
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return b, ErrInvalid{"manifest trailing data"}
	}
	return b, nil
}

type bundleNoteInput struct {
	meta   BundleNote
	fields map[string]string
	in     NoteInput
	key    string
}

func validateBundle(b bundle) ([]bundleNoteInput, error) {
	m := b.manifest
	if m.Format != 1 {
		return nil, ErrInvalid{"bundle format"}
	}
	if !validName(m.Team) || m.OriginGateway == "" || strings.ContainsAny(m.OriginGateway, ":/\\") || len(m.OriginGateway) > 256 {
		return nil, ErrInvalid{"team or origin gateway"}
	}
	if _, err := time.Parse(time.RFC3339Nano, m.Exported); err != nil {
		return nil, ErrInvalid{"export time"}
	}
	if len(m.Notes) > maxNotes {
		return nil, ErrTooLarge
	}
	var total int64
	for name, data := range b.files {
		if !safeBundlePath(name) {
			return nil, ErrInvalid{"bundle path"}
		}
		total += int64(len(data))
	}
	if total > maxBytes {
		return nil, ErrTooLarge
	}
	projects := map[string]bool{}
	for _, p := range m.Projects {
		if !slugRe.MatchString(p.Slug) || strings.Trim(p.Slug, "0123456789") == "" || !validName(p.Name) || projects[p.Slug] {
			return nil, ErrInvalid{"project"}
		}
		projects[p.Slug] = true
	}
	users := map[string]bool{}
	for _, u := range m.Users {
		k := strings.ToLower(u)
		if !validName(u) || users[k] {
			return nil, ErrInvalid{"user"}
		}
		users[k] = true
	}
	members := map[string]bool{}
	for _, u := range m.Members {
		k := strings.ToLower(u)
		if !users[k] || members[k] {
			return nil, ErrInvalid{"team member"}
		}
		members[k] = true
	}
	for _, list := range [][]BundleMembership{m.Memberships, m.Assignments} {
		seen := map[string]bool{}
		for _, v := range list {
			key := v.Project + ":" + strings.ToLower(v.User)
			if !projects[v.Project] || !users[strings.ToLower(v.User)] || roleRank[v.Role] == 0 || seen[key] {
				return nil, ErrInvalid{"membership"}
			}
			seen[key] = true
		}
	}
	for _, v := range m.Assignments {
		if !members[strings.ToLower(v.User)] {
			return nil, ErrInvalid{"assignment requires team member"}
		}
	}
	for _, g := range m.Grants {
		if !projects[g.Source] || !projects[g.Reader] || g.Source == g.Reader || (g.Class != Structure && g.Class != Data) {
			return nil, ErrInvalid{"grant"}
		}
	}
	seen := map[string]bool{"manifest.json": true, "INDEX.md": true}
	keys := map[string]bool{}
	var notes []bundleNoteInput
	for _, v := range m.Notes {
		data, ok := b.files[v.File]
		if !ok || seen[v.File] || !projects[v.Project] || path.Ext(v.File) != ".md" {
			return nil, ErrInvalid{"note file"}
		}
		seen[v.File] = true
		f, body, err := parseMarkdown(data)
		if err != nil {
			return nil, err
		}
		id, err := markdownID(f)
		if err != nil {
			return nil, err
		}
		in := NoteInput{Class: Class(f["class"]), Title: f["title"], Body: body}
		if err = checkInput(in); err != nil {
			return nil, err
		}
		if err = checkSize(in); err != nil {
			return nil, err
		}
		key := bundleKey(m.OriginGateway, v.Project, id)
		if f["import_key"] != key || keys[key] || !users[strings.ToLower(f["author"])] || f["hash"] != contentHash(in.Title, in.Body) || v.Provenance.ContentHash != f["hash"] || v.Provenance.SourceKind != f["kind"] {
			return nil, ErrInvalid{"note metadata or hash"}
		}
		keys[key] = true
		notes = append(notes, bundleNoteInput{v, f, in, key})
	}
	if len(seen) != len(b.files) {
		return nil, ErrInvalid{"unexpected bundle file"}
	}
	for name := range seen {
		if _, ok := b.files[name]; !ok {
			return nil, ErrInvalid{"missing bundle file"}
		}
	}
	return notes, nil
}

// ImportBundle validates every byte before entering a transaction.
func (s *Store) ImportBundle(ctx context.Context, in BundleImport) (report BundleReport, err error) {
	b, err := readBundle(in.Path)
	if err != nil {
		return BundleReport{[]string{"refuse " + err.Error()}}, err
	}
	notes, err := validateBundle(b)
	if err != nil {
		return BundleReport{[]string{"refuse " + err.Error()}}, err
	}
	mapping := map[string]string{}
	for old, name := range in.Map {
		k := strings.ToLower(old)
		if !validName(old) || !validName(name) || mapping[k] != "" {
			err = ErrInvalid{"user map"}
			return BundleReport{[]string{"refuse " + err.Error()}}, err
		}
		mapping[k] = name
	}
	apply := func(tx *sql.Tx) error { return s.applyBundle(tx, b.manifest, notes, mapping, in.DryRun, &report) }
	if in.DryRun {
		err = s.read(ctx, apply)
	} else {
		err = s.write(ctx, apply)
	}
	if err != nil {
		if !in.DryRun {
			report.Lines = nil
		}
		report.Lines = append(report.Lines, "refuse "+err.Error())
	}
	return
}

func (s *Store) applyBundle(tx *sql.Tx, m BundleManifest, notes []bundleNoteInput, mapping map[string]string, dry bool, report *BundleReport) error {
	changed := false
	line := func(action, subject string) {
		report.Lines = append(report.Lines, action+" "+subject)
		if action != "skip" {
			changed = true
		}
	}
	exec := func(q string, args ...any) error {
		if dry {
			return nil
		}
		_, e := tx.Exec(q, args...)
		return e
	}
	var team int64
	var archived bool
	err := tx.QueryRow(`SELECT id,archived FROM teams WHERE name=? COLLATE NOCASE`, m.Team).Scan(&team, &archived)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	newTeam := err == sql.ErrNoRows
	if archived {
		return ErrInvalid{"target team is archived"}
	}
	// Team ownership conflicts are checked before any write, including user creation.
	for _, p := range m.Projects {
		var existing int64
		e := tx.QueryRow(`SELECT t.team_id FROM team_projects t JOIN projects p ON p.id=t.project_id WHERE p.slug=?`, p.Slug).Scan(&existing)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if e == nil && (newTeam || existing != team) {
			return ErrInvalid{"project already belongs to another team: " + p.Slug}
		}
	}
	if newTeam {
		line("create", "team "+m.Team)
		if dry {
			team = -1
		} else {
			r, e := tx.Exec(`INSERT INTO teams(name) VALUES(?)`, m.Team)
			if e != nil {
				return e
			}
			team, _ = r.LastInsertId()
		}
	} else {
		line("skip", "team "+m.Team)
	}
	users := map[string]int64{}
	targets := map[string]int64{}
	for _, name := range m.Users {
		target := name
		if v := mapping[strings.ToLower(name)]; v != "" {
			target = v
		}
		k := strings.ToLower(target)
		id, ok := targets[k]
		if !ok {
			e := tx.QueryRow(`SELECT id FROM users WHERE name=? COLLATE NOCASE`, target).Scan(&id)
			if e != nil && e != sql.ErrNoRows {
				return e
			}
			if e == sql.ErrNoRows {
				line("create", "inactive user "+target)
				if dry {
					id = -int64(len(targets) + 1)
				} else {
					r, e := tx.Exec(`INSERT INTO users(name,active,owner) VALUES(?,0,0)`, target)
					if e != nil {
						return e
					}
					id, _ = r.LastInsertId()
				}
			} else {
				line("skip", "user "+target)
			}
			targets[k] = id
		}
		users[strings.ToLower(name)] = id
	}
	projects := map[string]int64{}
	high := max(s.st.ProjectHigh, dbInt(tx, "project_high"))
	for _, p := range m.Projects {
		var id int64
		e := tx.QueryRow(`SELECT id FROM projects WHERE slug=?`, p.Slug).Scan(&id)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if e == sql.ErrNoRows {
			high++
			id = high
			line("create", "project "+p.Slug)
			if e = exec(`INSERT INTO projects(id,slug,name) VALUES(?,?,?)`, id, p.Slug, p.Name); e != nil {
				return e
			}
			if e = exec(`INSERT INTO meta(key,value) VALUES('project_high',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, i64(high)); e != nil {
				return e
			}
		} else {
			line("skip", "project "+p.Slug)
		}
		projects[p.Slug] = id
		var n int
		if e = tx.QueryRow(`SELECT count(*) FROM team_projects WHERE team_id=? AND project_id=?`, team, id).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			line("merge", "team project "+p.Slug)
			if e = exec(`INSERT INTO team_projects(team_id,project_id) VALUES(?,?)`, team, id); e != nil {
				return e
			}
		}
	}
	for _, name := range m.Members {
		uid := users[strings.ToLower(name)]
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM team_members WHERE team_id=? AND user_id=?`, team, uid).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			line("merge", "team member "+name)
			if e := exec(`INSERT OR IGNORE INTO team_members(team_id,user_id) VALUES(?,?)`, team, uid); e != nil {
				return e
			}
		}
	}
	// Only raise an origin's role. Rebuilding effective memberships cannot narrow it.
	for i, list := range [][]BundleMembership{m.Memberships, m.Assignments} {
		for _, v := range list {
			pid, uid := projects[v.Project], users[strings.ToLower(v.User)]
			table, where, args := "direct_memberships", "project_id=? AND user_id=?", []any{pid, uid}
			if i == 1 {
				table = "team_assignments"
				where = "team_id=? AND " + where
				args = append([]any{team}, args...)
			}
			var old Role
			e := tx.QueryRow("SELECT role FROM "+table+" WHERE "+where, args...).Scan(&old)
			if e != nil && e != sql.ErrNoRows {
				return e
			}
			if roleRank[old] >= roleRank[v.Role] {
				line("skip", "role "+v.Project+" "+v.User)
				continue
			}
			line("merge", "role "+v.Project+" "+v.User+" "+string(v.Role))
			// Mapped users can collapse several source entries; use SQL rank comparison too.
			q := `INSERT INTO direct_memberships(project_id,user_id,role) VALUES(?,?,?) ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role WHERE CASE direct_memberships.role WHEN 'admin' THEN 3 WHEN 'developer' THEN 2 ELSE 1 END < CASE excluded.role WHEN 'admin' THEN 3 WHEN 'developer' THEN 2 ELSE 1 END`
			args = []any{pid, uid, v.Role}
			if i == 1 {
				q = strings.ReplaceAll(q, "direct_memberships", "team_assignments")
				q = strings.Replace(q, "(project_id,user_id,role) VALUES(?,?,?)", "(team_id,project_id,user_id,role) VALUES(?,?,?,?)", 1)
				q = strings.Replace(q, "ON CONFLICT(project_id,user_id)", "ON CONFLICT(team_id,project_id,user_id)", 1)
				args = append([]any{team}, args...)
			}
			if e = exec(q, args...); e != nil {
				return e
			}
		}
	}
	for _, g := range m.Grants {
		src, reader := projects[g.Source], projects[g.Reader]
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM grants WHERE source_id=? AND reader_id=? AND class=?`, src, reader, g.Class).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			line("merge", "grant "+g.Source+" "+g.Reader+" "+string(g.Class))
			if e := exec(`INSERT OR IGNORE INTO grants(source_id,reader_id,class) VALUES(?,?,?)`, src, reader, g.Class); e != nil {
				return e
			}
		} else {
			line("skip", "grant "+g.Source+" "+g.Reader+" "+string(g.Class))
		}
	}
	counts, sizes := map[int64]int64{}, map[int64]int64{}
	for _, v := range notes {
		pid := projects[v.meta.Project]
		r, e := receipt(tx, pid, v.key)
		if e != nil {
			return e
		}
		hash := contentHash(v.in.Title, v.in.Body)
		var old Note
		if r.submission != 0 {
			line("skip", v.key+": local submission")
			continue
		}
		if r.note != 0 {
			old, e = getNote(tx, r.note)
			if e != nil {
				return e
			}
			if r.hash == hash {
				line("skip", v.key)
				continue
			}
			if contentHash(old.Title, old.Body) != r.hash {
				line("skip", v.key+": local edit")
				continue
			}
		}
		var projectArchived bool
		e = tx.QueryRow(`SELECT archived FROM projects WHERE id=?`, pid).Scan(&projectArchived)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if projectArchived {
			return ErrArchived
		}
		if _, ok := counts[pid]; !ok {
			var countsValue, sizesValue int64
			if e = tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM notes WHERE project_id=?`, pid).Scan(&countsValue, &sizesValue); e != nil {
				return e
			}
			counts[pid] = countsValue
			sizes[pid] = sizesValue
		}
		if r.note == 0 {
			counts[pid]++
		}
		sizes[pid] += int64(len(v.in.Title) + len(v.in.Body) - len(old.Title) - len(old.Body))
		if counts[pid] > int64(maxNotes) || sizes[pid] > maxBytes {
			return ErrTooLarge
		}
		action := "create"
		if r.note != 0 {
			action = "merge"
		}
		line(action, v.key)
		if dry {
			continue
		}
		uid := users[strings.ToLower(v.fields["author"])]
		pr := v.meta.Provenance
		pr.Device = "UNKNOWN"
		pr.OriginGateway, pr.Exported = m.OriginGateway, m.Exported
		if r.note != 0 && old.Class != v.in.Class {
			if e = setMeta(tx, "epoch", i64(max(s.st.Epoch, dbInt(tx, "epoch"))+1)); e != nil {
				return e
			}
		}
		id, e := s.importNote(tx, pid, uid, r.note, v.in, pr)
		if e != nil {
			return e
		}
		r.note, r.hash = id, hash
		if e = saveImport(tx, pid, v.key, r, pr); e != nil {
			return e
		}
	}
	if dry || !changed {
		return nil
	}
	if _, err = rebuildMemberships(tx); err != nil {
		return err
	}
	return Audit(tx, adminActor, "kb.import", m.OriginGateway, map[string]any{"team": m.Team, "exported": m.Exported})
}
