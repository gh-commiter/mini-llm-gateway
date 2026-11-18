package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Step C of the upstream gateway (reading the upstream's KB): the manifest a hub serves, the dry-run pull that shows what a pull would
// reach (nothing stored), and the manual pull into the mirror (upstream_mirror, never `notes`).

const (
	// MaxManifest: notes in one manifest; more and the answer says Truncated (a reader then never infers a deletion from it).
	MaxManifest = 5000
	// manifestSources: the sources a manifest may name (the project, its team segment, the projects and teams it depends on: 16 at most).
	manifestSources = 40
	// pullDryTimeout: the whole of one dry-run request to an upstream (the manifest is one answer of at most maxUpstreamAnswer bytes).
	pullDryTimeout = 30 * time.Second
)

// ManifestSource is a place the caller may read from: the project itself first, then its team's segment and what it depends on.
type ManifestSource struct {
	ID    int64  `json:"id"`
	Label string `json:"label"` // project:<slug> or team:<name>, as a search hit's `from`
}

// ManifestNote is one note the caller could GET: ids, revision, class and kind only (no title, no body).
type ManifestNote struct {
	ID     int64  `json:"id"`
	Source int64  `json:"source"`
	Rev    int64  `json:"rev"`
	Class  Class  `json:"class"`
	Kind   string `json:"kind"`
}

// Manifest is the answer of GET /minigw/v1/projects/{id}/kb/manifest.
type Manifest struct {
	V         int              `json:"v"`
	Sources   []ManifestSource `json:"sources"`
	Notes     []ManifestNote   `json:"notes"`
	Truncated bool             `json:"truncated"`
}

// Manifest lists the published, non-private notes of the project and its sources that the caller may read: exactly the notes that
// GET .../notes/{id} would give this caller (the same readable sets as search and read by id, minus private). Counted as a read.
func (s *Store) Manifest(ctx context.Context, p Principal, project int64) (m Manifest, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, err := memberAccess(tx, p, project)
		if err != nil {
			return err
		}
		if lim != nil {
			return lim
		}
		rs, err := noteSources(tx, p.UserID, project, a)
		if err != nil {
			return err
		}
		rs[0] = a.readable(project, true) // the home project's own private notes are never listed (the other sources never contain them)
		var use []readable
		m = Manifest{V: 1, Sources: []ManifestSource{}, Notes: []ManifestNote{}}
		for _, r := range rs {
			if len(r.all)+len(r.own) == 0 {
				continue
			}
			label, err := sourceLabel(tx, r.project)
			if err != nil {
				return err
			}
			use = append(use, r)
			m.Sources = append(m.Sources, ManifestSource{ID: r.project, Label: label})
		}
		if len(use) == 0 {
			return nil
		}
		q, args := where(p.UserID, use)
		rows, err := tx.Query(`SELECT n.id,n.project_id,n.revision,n.class,n.entry_kind FROM notes n WHERE `+q+` ORDER BY n.id LIMIT ?`, append(args, MaxManifest+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n ManifestNote
			if err = rows.Scan(&n.ID, &n.Source, &n.Rev, &n.Class, &n.Kind); err != nil {
				return err
			}
			m.Notes = append(m.Notes, n)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if len(m.Notes) > MaxManifest {
			m.Notes, m.Truncated = m.Notes[:MaxManifest], true
		}
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	return
}

func sourceLabel(tx *sql.Tx, id int64) (label string, err error) {
	if isSegment(id) {
		err = tx.QueryRow(`SELECT 'team:'||name FROM teams WHERE id=?`, id-segmentBase).Scan(&label)
		return
	}
	err = tx.QueryRow(`SELECT 'project:'||slug FROM projects WHERE id=?`, id).Scan(&label)
	return
}

// UpstreamPull is the report of one link of a pull: a dry run only asks the upstream for its manifest and says what a pull would reach;
// a real pull also fills the mirror (Added, Updated, Removed, Skipped count what it did to it).
type UpstreamPull struct {
	Label    string   `json:"label"`
	Upstream string   `json:"upstream"`
	Remote   string   `json:"remote"`
	Lines    []string `json:"lines"`
	Total    int      `json:"total"`
	// Truncated: the upstream's list was cut at its cap; a pull would never delete anything from it.
	Truncated bool   `json:"truncated"`
	Stopped   string `json:"stopped,omitempty"`
	DryRun    bool   `json:"dryRun"`
	Project   int64  `json:"project"`
	Team      bool   `json:"team"`
	Added     int    `json:"added"`
	Updated   int    `json:"updated"`
	Removed   int    `json:"removed"`
	Skipped   int    `json:"skipped"`
	Mirror    int    `json:"mirror"` // notes in the link's mirror after the run
	// Purged: the upstream withdrew the right to read (or the project is gone there): the mirror of this link was deleted.
	Purged bool `json:"purged,omitempty"`
	// More: the run used its budget (note GETs, time, or the upstream's read limit) and stopped without a fault: pull again (after Wait
	// seconds). Next is the cursor, the id of the last upstream note dealt with, so that the next run goes on above it.
	More bool  `json:"more,omitempty"`
	Wait int   `json:"wait,omitempty"`
	Next int64 `json:"next,omitempty"`
}

// PullUpstream asks the upstream of each named link (project or team > 0: that one; both 0: every link; upstream optional) for the
// manifest of the linked remote project. dry: only reports its sources and the counts per class and kind, nothing is written. A real pull
// (only links with reading on) fetches what the manifest says is new or changed into the mirror, deletes what a complete manifest no longer
// lists, and purges the mirror when the upstream withdraws the right. It is an explicit owner action: nothing calls it on a timer.
func (s *Store) PullUpstream(ctx context.Context, tr UpstreamTransport, project, team int64, upstream string, dry bool, sleep func(time.Duration) bool) ([]UpstreamPull, error) {
	links, err := s.UpstreamLinks(ctx, project, team)
	if err != nil {
		return nil, err
	}
	var out []UpstreamPull
	linked := false
	for _, l := range links {
		if upstream != "" && l.Upstream != upstream {
			continue
		}
		linked = true
		switch {
		case dry:
			out = append(out, s.pullDry(ctx, tr, l.UpstreamLink, l.Label, sleep))
		case l.Read:
			// one budget for all links: a link that comes after it is used up is "more" (the reports of the links done stay in the answer)
			_, isTeam := SegmentTeam(l.Project)
			later := UpstreamPull{Label: l.Label, Upstream: l.Upstream, Remote: l.Remote, Project: l.Project, Team: isTeam, More: true, Wait: 1,
				Stopped: "the time budget of this call is used: pull again"}
			if ctx.Err() != nil {
				out = append(out, later)
				continue
			}
			r, err := s.pullLink(ctx, tr, l.UpstreamLink, l.Label, sleep)
			if err != nil && ctx.Err() != nil && len(out) > 0 {
				out = append(out, later)
				continue
			}
			if err != nil {
				return out, err
			}
			out = append(out, r)
		}
	}
	switch {
	case !linked:
		return nil, ErrRefusal{"no upstream link for this: minigw gw upstream link <project> <upstream>"}
	case len(out) == 0:
		return nil, ErrRefusal{"reading is off for this link: minigw gw upstream link <project> <upstream> --read --accept-reach (look first: pull <project> --dry-run)"}
	}
	return out, nil
}

func (s *Store) pullDry(ctx context.Context, tr UpstreamTransport, l UpstreamLink, label string, sleep func(time.Duration) bool) UpstreamPull {
	_, isTeam := SegmentTeam(l.Project)
	res := UpstreamPull{Label: label, Upstream: l.Upstream, Remote: l.Remote, DryRun: true, Project: l.Project, Team: isTeam}
	ctx, cancel := context.WithTimeout(ctx, pullDryTimeout)
	defer cancel()
	p := &pusher{s: s, tr: tr, up: l.Upstream, sleep: sleep, ctx: ctx}
	remote, why := p.resolve(l)
	if why == "" {
		a := p.call("GET", "/minigw/v1/projects/"+strconv.FormatInt(remote, 10)+"/kb/manifest", nil, nil)
		switch {
		case p.more:
			why = "the upstream's read limit: try again in " + strconv.Itoa(int(p.wait/time.Second)) + " seconds"
		case a.status == 404 || a.status == 501:
			why = "the upstream does not support reading (or the project is gone there)"
		case a.status != 200:
			why = refusal(a)
		default:
			var m *Manifest
			if m, why = parseManifest(a.data); why == "" {
				res.Total, res.Truncated, res.Lines = len(m.Notes), m.Truncated, manifestLines(m)
			}
		}
	}
	res.Stopped = why
	return res
}

// parseManifest reads an upstream's answer as DATA: one JSON object (nothing after it), numbers and strings of the right types, a version
// it knows, ids positive and unique, classes and kinds from our own sets, private never, every note naming a listed source, bounded
// counts. Fields it does not know are ignored. Anything else makes the whole answer unusable.
func parseManifest(data []byte) (*Manifest, string) {
	bad := func(what string) (*Manifest, string) {
		return nil, "the upstream's manifest is not usable (" + what + "): nothing was taken from it"
	}
	d := json.NewDecoder(bytes.NewReader(data))
	var m Manifest
	if d.Decode(&m) != nil {
		return bad("not the expected JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return bad("data after the JSON object")
	}
	if m.V != 1 {
		return nil, "the upstream does not support reading (manifest version " + strconv.Itoa(m.V) + ", expected 1)"
	}
	if len(m.Sources) == 0 || len(m.Sources) > manifestSources || len(m.Notes) > MaxManifest {
		return bad("too many or no sources, or too many notes")
	}
	src := map[int64]bool{}
	for i, x := range m.Sources {
		if x.ID <= 0 || src[x.ID] || x.Label == "" || len(x.Label) > 200 {
			return bad("a source")
		}
		src[x.ID] = true
		m.Sources[i].Label = upText(x.Label, 80)
	}
	seen := make(map[int64]bool, len(m.Notes))
	for _, n := range m.Notes {
		switch {
		case n.ID <= 0 || n.Rev <= 0 || seen[n.ID]:
			return bad("a note id or revision")
		case !src[n.Source]:
			return bad("a note names no listed source")
		case n.Class != Structure && n.Class != Data: // private is refused on both sides
			return bad("a note of a class that is not offered")
		case !validKind(n.Kind):
			return bad("a note kind")
		}
		seen[n.ID] = true
	}
	return &m, ""
}

// manifestLines: per source, the counts per class and kind (what the upstream would let this device read; the default pull takes the
// structure notes only).
func manifestLines(m *Manifest) []string {
	type key struct {
		src         int64
		class, kind string
	}
	count := map[key]int{}
	for _, n := range m.Notes {
		count[key{n.Source, string(n.Class), n.Kind}]++
	}
	var lines []string
	for _, s := range m.Sources {
		var parts []string
		var keys []key
		for k := range count {
			if k.src == s.ID {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].class != keys[j].class {
				return keys[i].class > keys[j].class // structure before data
			}
			return keys[i].kind < keys[j].kind
		})
		total := 0
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %s %d", k.class, k.kind, count[k]))
			total += count[k]
		}
		line := fmt.Sprintf("source %s: %d notes", s.Label, total)
		if len(parts) > 0 {
			line += " (" + strings.Join(parts, ", ") + ")"
		}
		lines = append(lines, line)
	}
	if m.Truncated {
		lines = append(lines, fmt.Sprintf("the upstream's list stops at %d notes: more exist, and a pull would delete nothing", MaxManifest))
	}
	return lines
}
