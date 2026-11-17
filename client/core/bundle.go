package core

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type BundleProject struct {
	Slug    string   `json:"slug"`
	Name    string   `json:"name"`
	Remotes []string `json:"remotes,omitempty"`
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
	TeamPath      string             `json:"team_path,omitempty"`
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
	Source string            `json:"source"`
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

// Limits of bundle reads; variables so that tests can use small ones.
var (
	maxManifestBytes       = 16 << 20
	maxBundleDirBytes      = 8 << 20
	maxBundleDepth         = 32
	maxBundleProjects      = 10000
	maxBundleUsers         = 100000
	maxBundleMemberships   = 200000
	maxBundleGrants        = 200000
	maxBundleNoteFileBytes = int64(maxBody + (64 << 10)) // body plus front matter
)

const maxSourceMeta = 64 << 10

func bundleKey(origin, slug string, id int64) string {
	return "gw:" + origin + ":" + slug + ":" + i64(id)
}

// receiptKey is the receipt namespace: the owner-chosen source name, the bundle's project slug
// and the bundle's note id. The bundle's own origin string cannot establish it.
func receiptKey(source, slug string, id int64) string {
	return "src:" + source + ":" + slug + ":" + i64(id)
}

// foldName is the identity policy for user names and archive names: Unicode NFC and case folding.
func foldName(s string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(s)))
}

// Source names use team-name validation, with at most 64 characters.
func validSourceName(s string) bool {
	return utf8.ValidString(s) && validName(s) && utf8.RuneCountInString(s) <= 64
}

// importProvenance is the trusted local record of a bundle import. Everything the bundle said
// about the note's history is unverified and kept in source_meta instead.
func importProvenance(source, slug string, sourceID, project int64, hash string, now time.Time) Provenance {
	return Provenance{ActorKind: ActorImport, Device: "UNKNOWN", Project: project,
		SourceKind: SourceImport, SourceReference: source + "/" + slug + "/" + i64(sourceID),
		ContentHash: hash, Authored: now, Received: now}
}

// ExportBundle is owner-only; the admin adapter authenticates the caller.
func (s *Store) ExportBundle(ctx context.Context, in BundleExport) error {
	b := bundle{files: map[string][]byte{}}
	m := &b.manifest
	m.Format, m.Exported, m.OriginGateway = 1, s.now().UTC().Format(time.RFC3339Nano), s.GatewayID()
	err := s.read(ctx, func(tx *sql.Tx) error {
		var team int64
		var archived bool
		if err := tx.QueryRow(`SELECT id,name,archived,path FROM teams WHERE name=? COLLATE NOCASE`, in.Team).Scan(&team, &m.Team, &archived, &m.TeamPath); err != nil {
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
		for i, p := range m.Projects {
			for id, slug := range projects {
				if slug == p.Slug {
					if m.Projects[i].Remotes, err = remoteList(tx, id); err != nil {
						return err
					}
					if len(m.Projects[i].Remotes) == 0 {
						m.Projects[i].Remotes = nil
					}
				}
			}
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

// checkBundleDirectory bounds the ZIP central directory before zip.NewReader parses it.
func checkBundleDirectory(f *os.File, size int64) error {
	const eocd = 22
	if size < eocd {
		return ErrInvalid{"bundle is not a zip"}
	}
	n := int64(eocd + 0xffff)
	if n > size {
		n = size
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-n); err != nil {
		return err
	}
	for i := len(buf) - eocd; i >= 0; i-- {
		if buf[i] != 'P' || buf[i+1] != 'K' || buf[i+2] != 5 || buf[i+3] != 6 {
			continue
		}
		comment := int(binary.LittleEndian.Uint16(buf[i+20 : i+22]))
		if i+eocd+comment != len(buf) {
			continue
		}
		entries := binary.LittleEndian.Uint16(buf[i+10 : i+12])
		dirSize := binary.LittleEndian.Uint32(buf[i+12 : i+16])
		dirOffset := int64(binary.LittleEndian.Uint32(buf[i+16 : i+20]))
		// These limits never need ZIP64; its markers mean a reader would allocate far more.
		if entries == 0xffff || dirSize == 0xffffffff {
			return ErrTooLarge
		}
		if int(entries) > maxNotes+2 || int64(dirSize) > int64(maxBundleDirBytes) || int64(dirSize) > size {
			return ErrTooLarge
		}
		end := size - n + int64(i)
		if binary.LittleEndian.Uint32(buf[i+4:i+8]) != 0 || binary.LittleEndian.Uint16(buf[i+8:i+10]) != entries || dirOffset+int64(dirSize) != end {
			return ErrInvalid{"bundle directory layout"}
		}
		// Count the actual records. ZIP readers need not honor the footer's count or size.
		var header [46]byte
		pos, count := dirOffset, 0
		for pos < end {
			if end-pos < int64(len(header)) {
				return ErrInvalid{"bundle directory record"}
			}
			if _, err := f.ReadAt(header[:], pos); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(header[:4]) != 0x02014b50 {
				return ErrInvalid{"bundle directory record"}
			}
			pos += 46 + int64(binary.LittleEndian.Uint16(header[28:30])) + int64(binary.LittleEndian.Uint16(header[30:32])) + int64(binary.LittleEndian.Uint16(header[32:34]))
			count++
			if count > maxNotes+2 {
				return ErrTooLarge
			}
		}
		if pos != end || count != int(entries) {
			return ErrInvalid{"bundle directory count"}
		}
		return nil
	}
	return ErrInvalid{"bundle is not a zip"}
}

// jsonDepth is the maximum nesting of objects and arrays outside strings.
func jsonDepth(b []byte) int {
	depth, max := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > max {
				max = depth
			}
		case '}', ']':
			depth--
		}
	}
	return max
}

func checkManifestLimits(m BundleManifest) error {
	if len(m.Projects) > maxBundleProjects || len(m.Users) > maxBundleUsers || len(m.Members) > maxBundleUsers ||
		len(m.Memberships) > maxBundleMemberships || len(m.Assignments) > maxBundleMemberships ||
		len(m.Grants) > maxBundleGrants || len(m.Notes) > maxNotes {
		return ErrTooLarge
	}
	return nil
}

// manifestFields are the exact top-level names the manifest decoder accepts. The decoder
// folds Unicode case, so it would also read "uſers" as users; the collection limit must
// not be dodged that way.
var manifestFields = func() map[string]bool {
	fields := map[string]bool{}
	t := reflect.TypeOf(BundleManifest{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			fields[name] = true
		}
	}
	return fields
}()

// manifestLimits is the limit of each bounded top-level collection; read at call time so
// tests can use small values.
func manifestLimits() map[string]int {
	return map[string]int{"projects": maxBundleProjects, "users": maxBundleUsers, "members": maxBundleUsers, "memberships": maxBundleMemberships, "assignments": maxBundleMemberships, "grants": maxBundleGrants, "notes": maxNotes}
}

// Count collection elements before decoding them into retained Go objects.
func preflightManifest(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	limits := manifestLimits()
	var value func(int, int) error
	value = func(depth, limit int) error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		switch t {
		case json.Delim('{'):
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name := key.(string)
				n := -1
				if depth == 0 {
					if !manifestFields[name] {
						return ErrInvalid{"bundle manifest: unknown field " + strconv.Quote(name)}
					}
					if v, ok := limits[name]; ok {
						n = v
					}
				}
				if err := value(depth+1, n); err != nil {
					return err
				}
			}
			_, err = d.Token()
		case json.Delim('['):
			count := 0
			for d.More() {
				count++
				if limit >= 0 && count > limit {
					return ErrTooLarge
				}
				if err := value(depth+1, -1); err != nil {
					return err
				}
			}
			_, err = d.Token()
		}
		return err
	}
	return value(0, -1)
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	closeErr := r.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
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
	if opened.Size() > maxBytes+(16<<20) {
		return b, ErrTooLarge
	}
	if err = checkBundleDirectory(input, opened.Size()); err != nil {
		return b, err
	}
	z, err := zip.NewReader(input, opened.Size())
	if err != nil {
		return b, err
	}
	if len(z.File) > maxNotes+2 {
		return b, ErrTooLarge
	}
	// Preflight every entry's metadata before any body is decompressed.
	names := map[string]bool{}
	var declared int64
	manifest := -1
	for i, f := range z.File {
		if !utf8.ValidString(f.Name) || !safeBundlePath(f.Name) || !f.Mode().IsRegular() {
			return b, ErrInvalid{"unsafe zip entry: " + f.Name}
		}
		k := foldName(f.Name)
		if names[k] {
			return b, ErrInvalid{"ambiguous zip entry name: " + f.Name}
		}
		names[k] = true
		if f.UncompressedSize64 > uint64(maxBytes) {
			return b, ErrTooLarge
		}
		declared += int64(f.UncompressedSize64)
		if declared > maxBytes {
			return b, ErrTooLarge
		}
		if f.Name == "manifest.json" {
			manifest = i
			if f.UncompressedSize64 > uint64(maxManifestBytes) {
				return b, ErrTooLarge
			}
		}
	}
	if manifest < 0 {
		return b, ErrInvalid{"missing manifest"}
	}
	// The manifest is decoded (bounded) before any other entry body is read.
	data, err := readEntry(z.File[manifest], int64(maxManifestBytes))
	if err != nil {
		return b, err
	}
	if jsonDepth(data) > maxBundleDepth {
		return b, ErrTooLarge
	}
	if err = preflightManifest(data); err != nil {
		return b, err
	}
	b.files["manifest.json"] = data
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&b.manifest); err != nil {
		return b, ErrInvalid{"manifest: " + err.Error()}
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return b, ErrInvalid{"manifest trailing data"}
	}
	if err = checkManifestLimits(b.manifest); err != nil {
		return b, err
	}
	// Refuse unlisted entries and oversized note files from metadata alone, before any body
	// is decompressed. validateBundle repeats these checks as a second line.
	allowed := map[string]bool{"manifest.json": true, "INDEX.md": true}
	notes := map[string]bool{}
	for _, v := range b.manifest.Notes {
		allowed[v.File] = true
		notes[v.File] = true
	}
	for _, f := range z.File {
		if f.Name == "manifest.json" {
			continue
		}
		if !allowed[f.Name] {
			return b, ErrInvalid{"unexpected bundle file"}
		}
		if notes[f.Name] && f.UncompressedSize64 > uint64(maxBundleNoteFileBytes) {
			return b, ErrTooLarge
		}
	}
	total := int64(len(data))
	for i, f := range z.File {
		if i == manifest {
			continue
		}
		left := maxBytes - total
		if left < 0 {
			return b, ErrTooLarge
		}
		limit := left
		if notes[f.Name] && maxBundleNoteFileBytes < limit {
			limit = maxBundleNoteFileBytes
		}
		data, err := readEntry(f, limit)
		if err != nil {
			return b, err
		}
		total += int64(len(data))
		b.files[f.Name] = data
	}
	if total > maxBytes {
		return b, ErrTooLarge
	}
	return b, nil
}

type bundleNoteInput struct {
	meta       BundleNote
	fields     map[string]string
	in         NoteInput
	key        string
	id         int64
	sourceMeta string
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
	if err := checkManifestLimits(m); err != nil {
		return nil, err
	}
	var total int64
	files := map[string]bool{}
	for name, data := range b.files {
		if !safeBundlePath(name) {
			return nil, ErrInvalid{"bundle path"}
		}
		k := foldName(name)
		if files[k] {
			return nil, ErrInvalid{"ambiguous bundle file"}
		}
		files[k] = true
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
		if len(p.Remotes) > maxRemotes {
			return nil, ErrInvalid{"project remotes: at most 8"}
		}
		for i, r := range p.Remotes {
			id, err := NormalizeRemote("https://" + r) // the manifest carries identities (host/path), not URLs
			if err != nil {
				return nil, ErrInvalid{"project remote"}
			}
			p.Remotes[i] = id // the list is shared with the manifest: import sees the identities
		}
	}
	if _, err := normalizeTeamPath(m.TeamPath); err != nil {
		return nil, err
	}
	users := map[string]bool{}
	for _, u := range m.Users {
		k := foldName(u)
		if !validName(u) || users[k] {
			return nil, ErrInvalid{"user"}
		}
		users[k] = true
	}
	members := map[string]bool{}
	for _, u := range m.Members {
		k := foldName(u)
		if !users[k] || members[k] {
			return nil, ErrInvalid{"team member"}
		}
		members[k] = true
	}
	for _, list := range [][]BundleMembership{m.Memberships, m.Assignments} {
		seen := map[string]bool{}
		for _, v := range list {
			key := v.Project + ":" + foldName(v.User)
			if !projects[v.Project] || !users[foldName(v.User)] || roleRank[v.Role] == 0 || seen[key] {
				return nil, ErrInvalid{"membership"}
			}
			seen[key] = true
		}
	}
	for _, v := range m.Assignments {
		if !members[foldName(v.User)] {
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
		in := NoteInput{Class: Class(f["class"]), Title: f["title"], Body: body, EntryKind: f["entry_kind"]}
		if err = checkInput(in); err != nil {
			return nil, err
		}
		if err = checkSize(in); err != nil {
			return nil, err
		}
		key := bundleKey(m.OriginGateway, v.Project, id)
		if f["import_key"] != key || keys[key] || !users[foldName(f["author"])] || f["hash"] != contentHash(in.Title, in.Body) || v.Provenance.ContentHash != f["hash"] || v.Provenance.SourceKind != f["kind"] {
			return nil, ErrInvalid{"note metadata or hash"}
		}
		keys[key] = true
		// Everything the bundle says about the note's history becomes one opaque text.
		meta, err := json.Marshal(map[string]any{"markdown": f, "provenance": v.Provenance})
		if err != nil {
			return nil, err
		}
		if len(meta) > maxSourceMeta {
			return nil, ErrTooLarge
		}
		notes = append(notes, bundleNoteInput{v, f, in, key, id, string(meta)})
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
	if !validSourceName(in.Source) {
		err = ErrInvalid{"source name: 1-64 characters, using team-name validation"}
		return BundleReport{[]string{"refuse " + err.Error()}}, err
	}
	b, err := readBundle(in.Path)
	if err != nil {
		return BundleReport{[]string{"refuse " + err.Error()}}, err
	}
	notes, err := validateBundle(b)
	if err != nil {
		return BundleReport{[]string{"refuse " + err.Error()}}, err
	}
	// A bundle of this gateway's own export could claim its notes back.
	if b.manifest.OriginGateway == s.GatewayID() {
		err = ErrInvalid{"bundle was exported by this gateway"}
		return BundleReport{[]string{"refuse " + err.Error()}}, err
	}
	mapping := map[string]string{}
	for old, name := range in.Map {
		k := foldName(old)
		if !validName(old) || !validName(name) || mapping[k] != "" {
			err = ErrInvalid{"user map"}
			return BundleReport{[]string{"refuse " + err.Error()}}, err
		}
		mapping[k] = name
	}
	apply := func(tx *sql.Tx) error {
		return s.applyBundle(tx, b.manifest, notes, mapping, in.Source, in.DryRun, &report)
	}
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

func (s *Store) applyBundle(tx *sql.Tx, m BundleManifest, notes []bundleNoteInput, mapping map[string]string, source string, dry bool, report *BundleReport) error {
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
	// The source name is the owner's namespace of receipts. The bundle's own origin is only
	// recorded the first time; a later import under this source must agree with it.
	var sourceOrigin string
	e := tx.QueryRow(`SELECT origin_gateway FROM import_sources WHERE source=?`, source).Scan(&sourceOrigin)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if e == nil && sourceOrigin != m.OriginGateway {
		return ErrInvalid{"this source was first imported from another gateway"}
	}
	if e == sql.ErrNoRows {
		line("create", "source "+source+", origin "+m.OriginGateway)
		if e = exec(`INSERT INTO import_sources(source,origin_gateway) VALUES(?,?)`, source, m.OriginGateway); e != nil {
			return e
		}
	} else {
		line("skip", "source "+source+", origin "+sourceOrigin)
	}
	var team int64
	var archived bool
	var teamPath string
	err := tx.QueryRow(`SELECT id,archived,path FROM teams WHERE name=? COLLATE NOCASE`, m.Team).Scan(&team, &archived, &teamPath)
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
	if label, _ := normalizeTeamPath(m.TeamPath); label != "" {
		if teamPath == "" {
			line("merge", "team path "+label)
			if e = exec(`UPDATE teams SET path=? WHERE id=?`, label, team); e != nil {
				return e
			}
		} else {
			line("skip", "team path "+label)
		}
	}
	// Target users are matched after Unicode NFC and case folding. Two accounts that fold to one
	// name, and two bundle users that land on one account, cannot be told apart: refuse.
	type account struct {
		name string
		id   int64
	}
	accounts := map[string][]account{}
	rows, e := tx.Query(`SELECT id,name FROM users`)
	if e != nil {
		return e
	}
	for rows.Next() {
		var a account
		if e = rows.Scan(&a.id, &a.name); e != nil {
			rows.Close()
			return e
		}
		k := foldName(a.name)
		accounts[k] = append(accounts[k], a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	users := map[string]int64{}
	used := map[string]string{}
	neg := int64(0)
	for _, name := range m.Users {
		target := name
		if v := mapping[foldName(name)]; v != "" {
			target = v
		}
		k := foldName(target)
		if other, ok := used[k]; ok {
			return ErrInvalid{"two bundle users map to one target user: " + other + ", " + name}
		}
		used[k] = name
		matches := accounts[k]
		if len(matches) > 1 {
			return ErrInvalid{"ambiguous target users " + matches[0].name + ", " + matches[1].name + " for " + name}
		}
		var a account
		if len(matches) == 0 {
			line("create", "inactive user "+target)
			if dry {
				neg--
				a = account{target, neg}
			} else {
				r, e := tx.Exec(`INSERT INTO users(name,active,owner) VALUES(?,0,0)`, target)
				if e != nil {
					return e
				}
				a.id, _ = r.LastInsertId()
			}
			accounts[k] = []account{a}
		} else {
			a = matches[0]
			line("skip", "user "+a.name)
		}
		users[foldName(name)] = a.id
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
		if e = s.bundleRemotes(tx, id, p, line, exec); e != nil {
			return e
		}
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
		uid := users[foldName(name)]
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
			pid, uid := projects[v.Project], users[foldName(v.User)]
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
			// Keep the higher existing role.
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
		key := receiptKey(source, v.meta.Project, v.id)
		r, e := receipt(tx, pid, key)
		if e != nil {
			return e
		}
		hash := contentHash(v.in.Title, v.in.Body)
		var old Note
		if r.submission != 0 {
			line("skip", key+": local submission")
			continue
		}
		// A pending local proposal for the same source entry is not superseded by an import.
		var pending int
		if e = tx.QueryRow(`SELECT count(*) FROM kb_submissions WHERE state='pending' AND project_id=? AND (key=? OR (replaces<>0 AND replaces=?))`, pid, v.key, r.note).Scan(&pending); e != nil {
			return e
		}
		if pending > 0 {
			line("skip", key+": local submission")
			continue
		}
		if r.note != 0 {
			old, e = getNote(tx, r.note)
			if e != nil {
				return e
			}
			// Any local change, including a class change, bumps the revision and is never overwritten.
			if old.Revision != r.revision {
				line("skip", key+": local edit")
				continue
			}
			if r.hash == hash && r.class == string(v.in.Class) {
				line("skip", key)
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
		if r.note != 0 { // the kind of a published note never changes through an import; its limits apply
			chk := v.in
			chk.EntryKind = old.EntryKind
			if e = checkInput(chk); e != nil {
				return e
			}
		}
		action := "create"
		if r.note != 0 {
			action = "merge"
		}
		line(action, key)
		if dry {
			continue
		}
		uid := users[foldName(v.fields["author"])]
		if r.note != 0 && old.Class != v.in.Class {
			if e = setMeta(tx, "epoch", i64(max(s.st.Epoch, dbInt(tx, "epoch"))+1)); e != nil {
				return e
			}
		}
		// The note's local provenance is the owner's import; the bundle's claims are kept apart.
		pr := importProvenance(source, v.meta.Project, v.id, pid, hash, s.now())
		id, e := s.importNote(tx, pid, uid, r.note, v.in, pr)
		if e != nil {
			return e
		}
		if e = tx.QueryRow(`SELECT revision FROM notes WHERE id=?`, id).Scan(&r.revision); e != nil {
			return e
		}
		r.note, r.hash, r.class = id, hash, string(v.in.Class)
		if e = saveImport(tx, pid, key, r, m.OriginGateway, m.Exported); e != nil {
			return e
		}
		if e = setSourceMeta(tx, id, v.sourceMeta); e != nil {
			return e
		}
	}
	if dry || !changed {
		return nil
	}
	if _, err = rebuildMemberships(tx); err != nil {
		return err
	}
	return Audit(tx, adminActor, "kb.import", source, map[string]any{"team": m.Team, "origin": m.OriginGateway, "exported": m.Exported})
}

// bundleRemotes adds the bundle's identities to a project by union (they were normalized by validateBundle).
func (s *Store) bundleRemotes(tx *sql.Tx, id int64, p BundleProject, line func(action, subject string), exec func(q string, args ...any) error) error {
	have, err := remoteList(tx, id)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, r := range have {
		known[r] = true
	}
	added := 0
	for _, r := range p.Remotes {
		if known[r] {
			line("skip", "remote "+p.Slug+" "+r)
			continue
		}
		known[r] = true
		added++
		if len(known) > maxRemotes {
			return ErrInvalid{"project remotes: at most 8: " + p.Slug}
		}
		line("create", "remote "+p.Slug+" "+r)
		if err = exec(`INSERT OR IGNORE INTO project_remotes(project_id,identity) VALUES(?,?)`, id, r); err != nil {
			return err
		}
	}
	if added > 0 {
		var archived bool
		if err = tx.QueryRow(`SELECT archived FROM projects WHERE id=?`, id).Scan(&archived); err != nil && err != sql.ErrNoRows {
			return err
		}
		if archived {
			return ErrArchived
		}
	}
	return nil
}
