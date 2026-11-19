package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Source kinds recorded in revision provenance.
const (
	ActorMember            = "member"
	ActorGatewayService    = "gateway service"
	ActorImport            = "import"
	SourcePushedNote       = "pushed note"
	SourceImport           = "import"
	SourceSessionSummary   = "session summary"
	SourceGatewaySynthesis = "gateway synthesis"
)

// Provenance records the authenticated writer of a revision. UNKNOWN means not recorded.
type Provenance struct {
	OriginGateway   string     `json:"originGateway,omitempty"`
	Exported        string     `json:"exported,omitempty"`
	ActorKind       string     `json:"actorKind"`
	User            int64      `json:"user"`
	Device          string     `json:"device"`
	Project         int64      `json:"project"`
	SourceKind      string     `json:"sourceKind"`
	SourceReference string     `json:"sourceReference"`
	ContentHash     string     `json:"contentHash"`
	Authored        time.Time  `json:"authored"`
	Received        time.Time  `json:"received"`
	Reviewer        string     `json:"reviewer"`
	Reviewed        *time.Time `json:"reviewed"`
}

func migrate11(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS note_revisions(note_id INTEGER NOT NULL, revision INTEGER NOT NULL, provenance TEXT NOT NULL, PRIMARY KEY(note_id,revision));
 CREATE TABLE IF NOT EXISTS kb_submissions(id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER NOT NULL REFERENCES projects(id), user_id INTEGER NOT NULL REFERENCES users(id), device_id TEXT NOT NULL, class TEXT NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL, bytes INTEGER NOT NULL, replaces INTEGER NOT NULL, revision INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'pending', provenance TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', note_id INTEGER NOT NULL DEFAULT 0, key TEXT NOT NULL, hash TEXT NOT NULL, received INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS kb_submissions_author ON kb_submissions(user_id,project_id,id);
 CREATE INDEX IF NOT EXISTS kb_submissions_pending ON kb_submissions(state,project_id,id);`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id,revision,author_id,project_id,title,body,created,updated FROM notes`)
	if err != nil {
		return err
	}
	type legacy struct {
		id, rev int64
		p       Provenance
	}
	var entries []legacy
	for rows.Next() {
		var v legacy
		var title, body string
		var created, updated int64
		if err = rows.Scan(&v.id, &v.rev, &v.p.User, &v.p.Project, &title, &body, &created, &updated); err != nil {
			rows.Close()
			return err
		}
		v.p.ActorKind, v.p.Device, v.p.SourceKind = "member", "UNKNOWN", "UNKNOWN"
		v.p.SourceReference = "UNKNOWN"
		v.p.Authored, v.p.Received = fromMs(created), fromMs(updated)
		v.p.ContentHash = contentHash(title, body)
		entries = append(entries, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range entries {
		if err = saveProvenance(tx, v.id, v.rev, v.p); err != nil {
			return err
		}
	}
	// Old write receipts can contain an earlier revision. Preserve their known metadata too.
	rows, err = tx.Query(`SELECT user_id,project_id,key,result FROM idempotency WHERE result<>''`)
	if err != nil {
		return err
	}
	type receipt struct {
		user, project int64
		key, result   string
		note          Note
	}
	var receipts []receipt
	for rows.Next() {
		var r receipt
		if err = rows.Scan(&r.user, &r.project, &r.key, &r.result); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(r.result), &r.note); err != nil {
			break
		}
		if r.note.Provenance.ActorKind == "" {
			r.note.Provenance = Provenance{ActorKind: ActorMember, User: r.note.AuthorID, Device: "UNKNOWN", Project: r.note.ProjectID, SourceKind: "UNKNOWN", SourceReference: "UNKNOWN", ContentHash: contentHash(r.note.Title, r.note.Body), Authored: r.note.Created, Received: r.note.Updated}
			receipts = append(receipts, r)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range receipts {
		if err = saveProvenance(tx, r.note.ID, r.note.Revision, r.note.Provenance); err != nil {
			return err
		}
		b, e := json.Marshal(r.note)
		if e != nil {
			return e
		}
		if _, err = tx.Exec(`UPDATE idempotency SET result=? WHERE user_id=? AND project_id=? AND key=?`, string(b), r.user, r.project, r.key); err != nil {
			return err
		}
	}
	return nil
}

func contentHash(title, body string) string {
	b, _ := json.Marshal([]string{title, body})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func writerProvenance(p Principal, project int64, in NoteInput, now time.Time) Provenance {
	return Provenance{ActorKind: "member", User: p.UserID, Device: p.DeviceID, Project: project, SourceKind: "pushed note", ContentHash: contentHash(in.Title, in.Body), Authored: now, Received: now}
}
func saveProvenance(tx *sql.Tx, id, revision int64, p Provenance) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO note_revisions(note_id,revision,provenance) VALUES(?,?,?)`, id, revision, string(b))
	return err
}
func noteProvenance(tx *sql.Tx, n *Note) error {
	var b string
	if err := tx.QueryRow(`SELECT provenance FROM note_revisions WHERE note_id=? AND revision=?`, n.ID, n.Revision).Scan(&b); err != nil {
		return err
	}
	return json.Unmarshal([]byte(b), &n.Provenance)
}

const maxReason = 1024

// newKind is the SQL for a new submission's kind: the replaced note's (a published kind never changes), else the given one,
// else a finding. Arguments: the replaced note id, then the kind.
const newKind = `COALESCE((SELECT entry_kind FROM notes WHERE id=?),NULLIF(?,''),'finding')`

var maxPendingUser = 100
var maxPendingProject = 500
var maxPendingUserBytes int64 = 5 << 20
var maxPendingProjectBytes int64 = 20 << 20
var ErrPendingLimit = errors.New("pending submission limit reached (user: 100 / 5 MiB; project: 500 / 20 MiB); ask the owner to review pending submissions")

type SubmissionInput struct {
	NoteInput
	Replaces int64 `json:"replaces"`
	Revision int64 `json:"revision"`
}
type Submission struct {
	ID         int64      `json:"id"`
	Project    int64      `json:"project"`
	User       int64      `json:"user"`
	Class      Class      `json:"class"`
	Title      string     `json:"title"`
	Body       *string    `json:"body,omitempty"`
	Replaces   int64      `json:"replaces"`
	Revision   int64      `json:"revision"`
	State      string     `json:"state"`
	Provenance Provenance `json:"provenance"`
	Reason     string     `json:"reason"`
	NoteID     int64      `json:"note"`
	EntryKind  string     `json:"entryKind"`
	// ReviewHash names the content a reviewer read (reviewHash); only the review reads carry it, and accept requires it.
	ReviewHash string `json:"reviewHash,omitempty"`
	// The review reads (with ReviewHash) show Title and Body without what a reviewer cannot see (invisibleRune, the mirror's rule):
	// Hidden counts what was left out of the text shown (an accept is refused while the stored text has any); ReviewCheck is "ok",
	// "hidden", or "long" (the body is longer than the owner's panel shows: review and accept it with gw kb review).
	Hidden      int    `json:"hidden,omitempty"`
	ReviewCheck string `json:"reviewCheck,omitempty"`
}

// reviewPanelLimit: the characters of a body the owner's panel shows (PanelLimits.multiline, PL.multiline); a rune count is never
// lower than either renderer's count, so "long" covers every body a panel would cut.
const reviewPanelLimit = 20000

// hiddenIn: the stored title and body as a reviewer is shown them, and how many characters that leaves out.
func hiddenIn(title, body string) (t, b string, n int) {
	t = dropInvisible(title, -1, false)
	lf := strings.ReplaceAll(body, "\r\n", "\n") // CRLF is a line ending (rows stored before lineEndings); a lone CR stays hidden
	b = mirrorBody(lf)
	return t, b, utf8.RuneCountInString(title) - utf8.RuneCountInString(t) + utf8.RuneCountInString(lf) - utf8.RuneCountInString(b)
}

var hiddenNames = map[rune]string{0x09: "CHARACTER TABULATION", 0x0A: "LINE FEED", 0x200B: "ZERO WIDTH SPACE", 0x200C: "ZERO WIDTH NON-JOINER", 0x200D: "ZERO WIDTH JOINER", 0x00AD: "SOFT HYPHEN",
	0xFEFF: "ZERO WIDTH NO-BREAK SPACE", 0xFE0F: "VARIATION SELECTOR-16", 0x202E: "RIGHT-TO-LEFT OVERRIDE", 0x202D: "LEFT-TO-RIGHT OVERRIDE",
	0x202A: "LEFT-TO-RIGHT EMBEDDING", 0x202B: "RIGHT-TO-LEFT EMBEDDING", 0x202C: "POP DIRECTIONAL FORMATTING", 0x2060: "WORD JOINER"}

// hiddenRefusal: a submission is refused at once when its text or title has characters a reviewer could not see (it could never be accepted),
// with the place of the first three so that the writer can remove them. what is "title" or "text"; ws: tab and newline are allowed (a body), not in a title.
func hiddenRefusal(what, s string, ws bool) error {
	rs := []rune(s)
	var where []string
	n, line, col := 0, 1, 0
	for i, r := range rs {
		col++
		if ws && r == '\n' {
			line, col = line+1, 0
			continue
		}
		if ws && r == '\t' || !invisibleAt(rs, i) && r != '\n' && r != '\t' {
			continue
		}
		if n++; n <= 3 {
			name := hiddenNames[r]
			if name == "" {
				name = "invisible"
			}
			where = append(where, fmt.Sprintf("line %d column %d: U+%04X %s", line, col, r, name))
		}
	}
	if n == 0 {
		return nil
	}
	more := ""
	if n > 3 {
		more = "; ..."
	}
	return ErrRefusal{fmt.Sprintf("the %s has %d hidden characters (%s%s): remove them and submit again", what, n, strings.Join(where, "; "), more)}
}

// reviewRead: a review read of v (stored text from the table, whatever v carries): the text shown, Hidden and ReviewCheck.
func reviewRead(tx *sql.Tx, v *Submission) error {
	var title, body string
	if err := tx.QueryRow(`SELECT title, body FROM kb_submissions WHERE id=?`, v.ID).Scan(&title, &body); err != nil {
		return err
	}
	t, b, n := hiddenIn(title, body)
	v.Title, v.Hidden, v.ReviewCheck = t, n, "ok"
	if v.Body != nil {
		v.Body = &b
	}
	switch {
	case n > 0:
		v.ReviewCheck = "hidden"
	case utf8.RuneCountInString(body) > reviewPanelLimit:
		v.ReviewCheck = "long"
	}
	return nil
}

// ErrHiddenCharacters: an accept of a submission whose stored text has characters a reviewer cannot see.
func ErrHiddenCharacters(n int) error {
	return ErrRefusal{fmt.Sprintf("the submission contains %d hidden characters (invisible or direction-changing ones, or a carriage return inside a line) that a reviewer cannot see: it cannot be accepted; reject it and ask the writer to submit the text without them", n)}
}

const submissionCols = `id,project_id,user_id,class,title,replaces,revision,state,provenance,reason,note_id,entry_kind`

func scanSubmission(row interface{ Scan(...any) error }) (v Submission, err error) {
	var b string
	err = row.Scan(&v.ID, &v.Project, &v.User, &v.Class, &v.Title, &v.Replaces, &v.Revision, &v.State, &b, &v.Reason, &v.NoteID, &v.EntryKind)
	if err == nil {
		err = json.Unmarshal([]byte(b), &v.Provenance)
	}
	return v, notFound(err)
}
func submissionAccess(tx *sql.Tx, p Principal, project int64, in SubmissionInput, checkRevision bool) error {
	a, err := memberAccess(tx, p, project)
	if err != nil {
		return err
	}
	if !a.can(pCreate, in.Class, true) {
		return ErrForbidden
	}
	if in.Replaces != 0 {
		_, n, e := writeAccess(tx, p, project, in.Replaces)
		if e != nil {
			return e
		}
		if !a.canMove(n.Class, in.Class, n.AuthorID == p.UserID) {
			return ErrForbidden
		}
		if checkRevision && n.Revision != in.Revision {
			return ErrRevision{Current: n.Revision}
		}
	} else if in.Revision != 0 {
		return ErrInvalid{"revision requires replaces"}
	}
	return archivedErr(tx, project)
}

func (s *Store) Submit(ctx context.Context, p Principal, project int64, key string, in SubmissionInput) (v Submission, replay bool, err error) {
	lim := s.rate(p.UserID, true)
	in.Body = lineEndings(in.Body) // before the idempotency hash: a retry of the same text is the same submission
	err = s.write(ctx, func(tx *sql.Tx) error {
		if err := submissionAccess(tx, p, project, SubmissionInput{NoteInput: in.NoteInput}, false); err != nil {
			return err
		}
		chk := in.NoteInput
		if in.Replaces > 0 { // a replacement keeps the kind of the replaced note: its limits apply
			// Only of a note the caller may change: any other id answers as a missing one (no kind oracle across projects).
			_, n, e := writeAccess(tx, p, project, in.Replaces)
			if e != nil {
				return e
			}
			chk.EntryKind = n.EntryKind
		}
		if err := checkInput(chk); err != nil {
			return err
		}
		if err := checkSize(in.NoteInput); err != nil {
			return err
		}
		if e := hiddenRefusal("title", in.Title, false); e != nil {
			return e
		}
		if e := hiddenRefusal("text", in.Body, true); e != nil {
			return e
		}
		if key == "" || len(key) > 64 {
			return ErrInvalid{"idempotency key"}
		}
		if in.Replaces < 0 || in.Revision < 0 {
			return ErrInvalid{"replacement"}
		}
		if lim != nil {
			return lim
		}
		b, _ := json.Marshal(in)
		h := sha256.Sum256(b)
		hash := hex.EncodeToString(h[:])
		var oldHash string
		var id int64
		e := tx.QueryRow(`SELECT id,hash FROM kb_submissions WHERE user_id=? AND project_id=? AND key=? AND received>=? ORDER BY id DESC LIMIT 1`, p.UserID, project, key, ms(s.now().Add(-idempotencyTTL))).Scan(&id, &oldHash)
		if e == nil {
			if hash != oldHash {
				return ErrIdempotency
			}
			if e := submissionAccess(tx, p, project, in, false); e != nil {
				return e
			}
			var e error
			v, e = getSubmission(tx, p, project, id, false, true)
			replay = e == nil
			return e
		}
		if e != sql.ErrNoRows {
			return e
		}
		if e := submissionAccess(tx, p, project, in, true); e != nil {
			return e
		}
		for _, r := range in.Title {
			if unicode.IsControl(r) {
				return ErrInvalid{"title: control characters"}
			}
		}
		size := int64(len(in.Title) + len(in.Body))
		if e = pendingLimit(tx, p.UserID, project, size, 0); e != nil {
			return e
		}
		pr := writerProvenance(p, project, in.NoteInput, fromMs(ms(s.now())))
		enc, _ := json.Marshal(pr)
		r, e := tx.Exec(`INSERT INTO kb_submissions(project_id,user_id,device_id,class,title,body,bytes,replaces,revision,provenance,key,hash,received,entry_kind) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,`+newKind+`)`, project, p.UserID, p.DeviceID, in.Class, in.Title, in.Body, size, in.Replaces, in.Revision, string(enc), key, hash, ms(s.now()), in.Replaces, in.EntryKind)
		if e != nil {
			return e
		}
		id, _ = r.LastInsertId()
		v, e = getSubmission(tx, p, project, id, false, true)
		if e != nil {
			return e
		}
		return Audit(tx, actor(p), "kb.submit", i64(id), map[string]any{"project": project, "replaces": in.Replaces})
	})
	if err != nil {
		return Submission{}, false, err
	}
	return
}

// Metadata authorization precedes selecting the body, including replay.
func getSubmission(tx *sql.Tx, p Principal, project, id int64, owner, body bool) (v Submission, err error) {
	if !owner {
		if err = checkPrincipal(tx, p); err != nil {
			return
		}
	}
	v, err = scanSubmission(tx.QueryRow(`SELECT `+submissionCols+` FROM kb_submissions WHERE id=?`, id))
	if err != nil {
		return Submission{}, err
	}
	if !owner && (v.User != p.UserID || v.Project != project) {
		return Submission{}, ErrNotFound
	}
	if !owner {
		if _, err = memberAccess(tx, p, project); err != nil {
			return Submission{}, err
		}
	}
	if body {
		var b string
		err = tx.QueryRow(`SELECT body FROM kb_submissions WHERE id=?`, id).Scan(&b)
		v.Body = &b
	}
	return
}
func (s *Store) GetSubmission(ctx context.Context, p Principal, project, id int64, owner bool) (v Submission, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		var e error
		v, e = getSubmission(tx, p, project, id, owner, false)
		if e != nil {
			return e
		}
		if !owner && lim != nil {
			return lim
		}
		if v, e = getSubmission(tx, p, project, id, owner, true); e != nil || !owner {
			return e
		}
		if v.ReviewHash, e = reviewHash(tx, id); e != nil { // the owner's read is a review read
			return e
		}
		return reviewRead(tx, &v)
	})
	if err != nil {
		return Submission{}, err
	}
	return
}
func (s *Store) ListSubmissions(ctx context.Context, p Principal, project, after int64, owner bool) (out []Submission, err error) {
	out = []Submission{}
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		q := `SELECT ` + submissionCols + ` FROM kb_submissions WHERE id>?`
		args := []any{after}
		if !owner {
			if e := checkPrincipal(tx, p); e != nil {
				return e
			}
			if _, e := memberAccess(tx, p, project); e != nil {
				return e
			}
			if lim != nil {
				return lim
			}
			q += ` AND user_id=?`
			args = append(args, p.UserID)
		} else {
			q += ` AND state='pending'`
		}
		if project != 0 {
			q += ` AND project_id=?`
			args = append(args, project)
		}
		rows, e := tx.Query(q+` ORDER BY id LIMIT 100`, args...)
		if e != nil {
			return e
		}
		for rows.Next() {
			v, e := scanSubmission(rows)
			if e != nil {
				rows.Close()
				return e
			}
			out = append(out, v)
		}
		e = rows.Err()
		rows.Close()
		for i := range out { // the owner's queue rows are review reads: the panel accepts with this hash
			if e == nil && owner {
				if out[i].ReviewHash, e = reviewHash(tx, out[i].ID); e == nil {
					e = reviewRead(tx, &out[i])
				}
			}
		}
		return e
	})
	return
}

// ReviewSubmission is the gateway owner's review: any submission, audited as the owner. seen is the ReviewHash of the
// read the decision rests on: an accept requires it to name the current content (reject ignores it).
func (s *Store) ReviewSubmission(ctx context.Context, id int64, accept bool, reason, seen string) (v Submission, err error) {
	return s.review(ctx, nil, 0, id, accept, reason, seen, nil)
}

// reviewHash is the version of a submission a reviewer read: everything an accept publishes or attributes (the id, class,
// title, kind, body, what it replaces and at which revision, and the stored provenance, which the note keeps as its
// revision's provenance: device, source, authored and received time). Any rewrite of a pending submission (kb import
// merges into it, with a new received time) changes it. It is compared WHOLE (256 bits): the author writes both texts,
// so a short prefix could be made to collide; the full SHA-256 cannot, and needs no secret nonce or extra column.
func reviewHash(tx *sql.Tx, id int64) (string, error) {
	var class, title, kind, body, prov string
	var replaces, revision int64
	if err := tx.QueryRow(`SELECT class,title,entry_kind,body,replaces,revision,provenance FROM kb_submissions WHERE id=?`, id).Scan(&class, &title, &kind, &body, &replaces, &revision, &prov); err != nil {
		return "", notFound(err)
	}
	b, _ := json.Marshal([]any{id, class, title, kind, body, replaces, revision, prov})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// The refusals of an accept that does not name what is there now.
var (
	ErrReviewHashMissing = ErrRefusal{"name the version you read: read the submission first (minigw kb review), or update minigw"}
	ErrSubmissionChanged = ErrRefusal{"the submission changed since you read it: review it again"}
)

// ErrWriterLost: at accept, the WRITER may no longer write this (deactivated, device unbound, role or note changed). Its
// own answer, so that a reviewer never reads it as their own lost access; Unwrap keeps the cause for errors.Is.
type ErrWriterLost struct{ Cause error }

func (e ErrWriterLost) Error() string {
	return "the writer of this submission may no longer write it here: reject it or ask them to submit again"
}
func (e ErrWriterLost) Unwrap() error { return e.Cause }

// GetSubmissionVersion is the owner's read of submission id as of the version hash (the panel's detail of a list row):
// the submission only while it still is that version, else ErrSubmissionChanged, so the detail never shows a text the
// row's Accept would not publish.
func (s *Store) GetSubmissionVersion(ctx context.Context, id int64, hash string) (Submission, error) {
	v, err := s.GetSubmission(ctx, Principal{}, 0, id, true)
	if err == nil && v.ReviewHash != hash {
		return Submission{}, ErrSubmissionChanged
	}
	return v, err
}

// reviewAccess: a member reviews the submissions of a project (or a team's own notes) only with the role admin there,
// as loadAccess computes it (direct or through a team assignment; a grant never; for a team segment: a project admin of
// the team on its roster). Anyone else gets the answer for a project they cannot see (404). Archived: 403.
func reviewAccess(tx *sql.Tx, p Principal, project int64) error {
	a, err := memberAccess(tx, p, project)
	if err != nil {
		return err
	}
	if a.Role != Admin {
		return ErrNotFound
	}
	return archivedErr(tx, project)
}

// ErrOwnSubmission: four eyes. A project admin never reviews their own submission; another admin or the owner does.
var ErrOwnSubmission = ErrRefusal{"your own submission: another admin of the project or the gateway owner reviews it"}

// reviewable loads the metadata of submission id for reviewer p: principal valid, the submission exists, lies in project
// (0 = whichever it lies in) and p may review that project (reviewAccess). Every miss is the same 404.
func reviewable(tx *sql.Tx, p Principal, project, id int64) (Submission, error) {
	if err := checkPrincipal(tx, p); err != nil {
		return Submission{}, err
	}
	v, err := getSubmission(tx, Principal{}, 0, id, true, false)
	if err == nil && project != 0 && v.Project != project {
		err = ErrNotFound
	}
	if err == nil {
		err = reviewAccess(tx, p, v.Project)
	}
	if err != nil {
		return Submission{}, err
	}
	return v, nil
}

// ListReview lists the pending submissions p may review (never p's own): of project, or with project 0 of every
// project and team that p reviews now (the others, archived ones too, are left out without a word).
func (s *Store) ListReview(ctx context.Context, p Principal, project, after int64) (out []Submission, err error) {
	out = []Submission{}
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		projects := []int64{project}
		if project != 0 {
			if e := reviewAccess(tx, p, project); e != nil {
				return e
			}
		} else {
			if e := checkPrincipal(tx, p); e != nil {
				return e
			}
			var e error
			if projects, e = reviewedProjects(tx, p); e != nil {
				return e
			}
		}
		if lim != nil {
			return lim
		}
		if len(projects) == 0 {
			return nil
		}
		args := []any{p.UserID, after}
		for _, id := range projects {
			args = append(args, id)
		}
		rows, e := tx.Query(`SELECT `+submissionCols+` FROM kb_submissions WHERE state='pending' AND user_id<>? AND id>? AND project_id IN (`+marks(len(projects))+`) ORDER BY id LIMIT 100`, args...)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			v, e := scanSubmission(rows)
			if e != nil {
				return e
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return
}

// reviewedProjects: the projects and team segments p reviews now. The candidates come from p's own admin roles (bounded
// by p's memberships, never a scan of the gateway's submissions); reviewAccess, the one rule, confirms each.
func reviewedProjects(tx *sql.Tx, p Principal) (out []int64, err error) {
	rows, err := tx.Query(`SELECT project_id FROM memberships WHERE user_id=?1 AND role='admin'
 UNION SELECT ?2+tp.team_id FROM team_projects tp JOIN memberships m ON m.project_id=tp.project_id AND m.user_id=?1 AND m.role='admin'
 JOIN team_members tm ON tm.team_id=tp.team_id AND tm.user_id=?1 ORDER BY 1`, p.UserID, segmentBase)
	if err != nil {
		return nil, err
	}
	var cands []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, id := range cands {
		e := reviewAccess(tx, p, id)
		if e == nil {
			out = append(out, id)
		} else if !errors.Is(e, ErrNotFound) && !errors.Is(e, ErrArchived) {
			return nil, e
		}
	}
	return out, nil
}

// GetReview reads one submission, with its body and the writer's name, for a reviewer of its project (project 0 = any).
func (s *Store) GetReview(ctx context.Context, p Principal, project, id int64) (v Submission, writer string, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if _, e := reviewable(tx, p, project, id); e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		var e error
		if v, e = getSubmission(tx, Principal{}, 0, id, true, true); e != nil {
			return e
		}
		if v.ReviewHash, e = reviewHash(tx, id); e != nil {
			return e
		}
		if e = reviewRead(tx, &v); e != nil {
			return e
		}
		return tx.QueryRow(`SELECT name FROM users WHERE id=?`, v.User).Scan(&writer)
	})
	if err != nil {
		return Submission{}, "", err
	}
	return
}

// MemberReview is a project admin's accept or reject of a submission of project (0 = its own project): reviewable, never their own, and an
// accept only of what the reviewer could write themselves (class, the replaced note). Audited as the reviewing user.
func (s *Store) MemberReview(ctx context.Context, p Principal, project, id int64, accept bool, reason, seen string) (Submission, error) {
	return s.review(ctx, &p, project, id, accept, reason, seen, s.rate(p.UserID, true))
}

// review: rv nil = the owner (any project); else the reviewing member, limited to project (lim after the access checks).
func (s *Store) review(ctx context.Context, rv *Principal, project, id int64, accept bool, reason, seen string, lim error) (v Submission, err error) {
	if len(reason) > maxReason {
		return v, ErrInvalid{"reason: at most 1024 bytes"}
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return v, ErrInvalid{"reason: control characters"}
		}
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var e error
		reviewer := adminActor
		if rv != nil {
			v, e = reviewable(tx, *rv, project, id)
			reviewer = actor(*rv)
		} else {
			v, e = getSubmission(tx, Principal{}, 0, id, true, false)
		}
		if e != nil {
			return e
		}
		if rv != nil && v.User == rv.UserID {
			return ErrOwnSubmission
		}
		if lim != nil {
			return lim
		}
		state := "rejected"
		if accept {
			state = "accepted"
		}
		if v.State == state { // a repeat publishes nothing (the stored provenance now names the reviewer)
			return nil
		}
		if v.State != "pending" {
			return ErrInvalid{"submission already reviewed"}
		}
		if accept { // four eyes on the text too: publish only what the reviewer read
			cur, e := reviewHash(tx, id)
			if e != nil {
				return e
			}
			if seen == "" {
				return ErrReviewHashMissing
			}
			if seen != cur {
				return ErrSubmissionChanged
			}
			var title, body string
			if e = tx.QueryRow(`SELECT title, body FROM kb_submissions WHERE id=?`, id).Scan(&title, &body); e != nil {
				return e
			}
			if _, _, n := hiddenIn(title, body); n > 0 { // publish only text the reviewer could see whole
				return ErrHiddenCharacters(n)
			}
		}
		now := fromMs(ms(s.now()))
		v.Provenance.Reviewer = reviewer
		v.Provenance.Reviewed = &now
		if accept {
			p := Principal{UserID: v.User, DeviceID: v.Provenance.Device}
			in := SubmissionInput{NoteInput: NoteInput{Class: v.Class, Title: v.Title}, Replaces: v.Replaces, Revision: v.Revision}
			if rv != nil { // first: the reviewer could write this note themselves (class, and the note it replaces)
				if e = submissionAccess(tx, *rv, v.Project, in, true); e != nil {
					return e
				}
			}
			if e = submissionAccess(tx, p, v.Project, in, true); e != nil { // then the writer, as of now
				if errors.Is(e, ErrNoProjectAccess) || errors.Is(e, ErrNotFound) || errors.Is(e, ErrForbidden) {
					return ErrWriterLost{e}
				}
				return e
			}
			var body string
			if e = tx.QueryRow(`SELECT body FROM kb_submissions WHERE id=?`, id).Scan(&body); e != nil {
				return e
			}
			body = lineEndings(body) // a submission stored before line endings were normalised is published with LF
			if e = checkInput(NoteInput{Class: v.Class, Title: v.Title, Body: body, EntryKind: v.EntryKind}); e != nil {
				return e // backstop: the stored kind's limits
			}
			size := int64(len(v.Title) + len(body))
			var oldSize int64
			if v.Replaces != 0 {
				if e = tx.QueryRow(`SELECT bytes FROM notes WHERE id=?`, v.Replaces).Scan(&oldSize); e != nil {
					return e
				}
			}
			if e = checkProjectLimits(tx, v.Project, v.Replaces == 0, size-oldSize); e != nil {
				return e
			}
			rev := int64(1)
			if v.Replaces == 0 {
				r, e := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated,entry_kind) VALUES(?,?,?,?,?,?,?,?,?)`, v.Project, v.Class, v.Title, body, size, v.User, ms(now), ms(now), v.EntryKind)
				if e != nil {
					return e
				}
				v.NoteID, _ = r.LastInsertId()
			} else {
				old, e := noteMetadata(tx, v.Replaces)
				if e != nil {
					return e
				}
				if e = saveNoteHistory(tx, v.Replaces, NoteInput{Class: v.Class, Title: v.Title, Body: body, EntryKind: v.EntryKind}); e != nil {
					return e
				}
				if _, e = tx.Exec(`UPDATE notes SET class=?,title=?,body=?,bytes=?,updated=?,revision=revision+1,entry_kind=? WHERE id=?`, v.Class, v.Title, body, size, ms(now), v.EntryKind, v.Replaces); e != nil {
					return e
				}
				if old.Class != v.Class {
					if _, e = tx.Exec(`UPDATE idempotency SET result='' WHERE note_id=?`, v.Replaces); e != nil {
						return e
					}
					if _, e = s.BumpEpochTx(tx, "reviewed note class changed"); e != nil {
						return e
					}
				}
				v.NoteID = v.Replaces
				rev = old.Revision + 1
			}
			if e = s.markUpstreamDirty(tx, v.Project); e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE note_imports SET note_id=?,submission_id=0,revision=?,class=? WHERE submission_id=?`, v.NoteID, rev, string(v.Class), id); e != nil {
				return e
			}
			if e = syncSearch(tx, v.NoteID); e != nil {
				return e
			}
			if e = saveProvenance(tx, v.NoteID, rev, v.Provenance); e != nil {
				return e
			}
		}
		v.State, v.Reason = state, reason
		b, _ := json.Marshal(v.Provenance)
		if _, e = tx.Exec(`UPDATE kb_submissions SET state=?,reason=?,provenance=?,note_id=? WHERE id=?`, state, reason, string(b), v.NoteID, id); e != nil {
			return e
		}
		return Audit(tx, reviewer, "kb."+state, i64(id), map[string]any{"project": v.Project, "note": v.NoteID})
	})
	if err != nil {
		return Submission{}, err
	}
	return
}
