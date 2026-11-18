package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Labels of a run for usage reports: the phase (plan, do, check, act), the activity (feature, bugfix, ...) and a task
// reference. They are data, never permissions: a missing label is empty, an unknown value is kept as given when it is
// a safe token, and a value that is not one is dropped (the event is never refused for a label). Each label has a
// source: "given" (the caller said it), "branch" (the client read the git branch; task only), "model" (a model chose it
// from a closed list, see SetModelLabels; phase and activity only), "internal" (the gateway's own classification events;
// only the gateway sets it), "" (none). A client can only ever claim "given" and "branch" (normalizeLabels); an explicit
// label is never overwritten by a model label.
var (
	labelWordRe = regexp.MustCompile(`^[a-z0-9._-]{1,32}$`)
	labelTaskRe = regexp.MustCompile(`^[A-Za-z0-9._#/-]{1,64}$`)
)

// SafeWordLabel lowercases a phase or activity and returns it when it is a safe token, else "".
func SafeWordLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if labelWordRe.MatchString(s) {
		return s
	}
	return ""
}

// SafeTaskLabel returns the task when it is a safe token, else "".
func SafeTaskLabel(s string) string {
	s = strings.TrimSpace(s)
	if labelTaskRe.MatchString(s) {
		return s
	}
	return ""
}

func labelSource(label, src string) string {
	switch {
	case label == "":
		return ""
	case src == "branch":
		return "branch"
	}
	return "given"
}

// normalizeLabels applies the gateway's own limits to the labels of an event (a modified client cannot store more).
func normalizeLabels(e *UsageEvent) {
	e.Phase, e.Activity, e.Task = SafeWordLabel(e.Phase), SafeWordLabel(e.Activity), SafeTaskLabel(e.Task)
	e.PhaseSource, e.ActivitySource, e.TaskSource = labelSource(e.Phase, "given"), labelSource(e.Activity, "given"), labelSource(e.Task, e.TaskSource)
}

// migrate17: labels of an event. Old rows have none (empty). Folded daily totals carry no labels.
func migrate17(tx *sql.Tx) error {
	for _, c := range []string{"phase", "activity", "task", "phase_source", "activity_source", "task_source"} {
		if _, err := tx.Exec(`ALTER TABLE usage_events ADD COLUMN ` + c + ` TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// normalizeLabelFilters applies to filter values the normalization stored values got (phase and activity lower case): an
// empty value is no filter; a value that is not a safe token can match nothing (impossible), it is not an error.
func normalizeLabelFilters(phase, activity, task string) (p, a, t string, impossible bool) {
	norm := func(v string, safe func(string) string) string {
		if v == "" {
			return ""
		}
		if s := safe(v); s != "" {
			return s
		}
		impossible = true
		return ""
	}
	return norm(phase, SafeWordLabel), norm(activity, SafeWordLabel), norm(task, SafeTaskLabel), impossible
}

// Sources of a label.
const (
	LabelGiven    = "given"
	LabelBranch   = "branch"
	LabelModel    = "model"
	LabelInternal = "internal"
)

// ValidLabelSource: the values of the --label-source filter.
func ValidLabelSource(s string) bool {
	return s == "" || s == LabelGiven || s == LabelBranch || s == LabelModel
}

// PhaseWords: the phases a model may choose (plan, do, check, act).
var PhaseWords = []string{"plan", "do", "check", "act"}

// DefaultActivities: the activities a model may choose until the owner changes the list.
var DefaultActivities = []string{"feature", "bugfix", "security", "refactor", "docs", "test", "release", "research", "ops"}

// FinishLabelDecision is for the gateway's own direct backend calls, never client-supplied events.
func (s *Store) FinishLabelDecision(ctx context.Context, id, provider, model string, start, end time.Time, in, out int64, failed bool) error {
	e := UsageEvent{ID: id, Start: start, End: end, DeviceID: "gateway", Provider: provider, Model: model, BillingMode: "local", TokensIn: in, TokensOut: out, TokenSource: "estimated", Activity: "ops", Task: "usage-labels", Outcome: OutcomeOK}
	if failed {
		e.Outcome = OutcomeError
		e.ErrorKind = "other"
	}
	e.DurationMs = new(end.Sub(start).Milliseconds())
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := finishEvent(tx, e); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE usage_events SET phase_source='internal', activity_source='internal', task_source='internal' WHERE id=? AND user_id IS NULL AND device_id='gateway'`, id)
		return err
	})
}

// A root finishing after the decision also covers children admitted later.
func propagateModelLabels(tx *sql.Tx, id string) error {
	for _, col := range []string{"phase", "activity"} {
		_, err := tx.Exec(`UPDATE usage_events SET `+col+`=(SELECT `+col+` FROM usage_events WHERE id=?), `+col+`_source='model', `+col+`_score=(SELECT `+col+`_score FROM usage_events WHERE id=?) WHERE parent_id=? AND `+col+`='' AND `+col+`_source='' AND 'internal' NOT IN(phase_source,activity_source,task_source) AND EXISTS(SELECT 1 FROM usage_events WHERE id=? AND parent_id IS NULL AND `+col+`_source='model')`, id, id, id, id)
		if err != nil {
			return err
		}
	}
	return nil
}

const maxActivities = 16

// migrate21: model-filled labels: a score (per mille of agreement, NULL = none) beside phase and activity, and the counters
// of the shadow comparison (model against explicit labels; only numbers, never any text).
func migrate21(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE usage_events ADD COLUMN phase_score INTEGER;
ALTER TABLE usage_events ADD COLUMN activity_score INTEGER;
CREATE TABLE label_agreement(day TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN('phase','activity')), bucket TEXT NOT NULL CHECK(bucket IN('667','1000','unknown')),
  agree INTEGER NOT NULL CHECK(agree IN(0,1)), n INTEGER NOT NULL, PRIMARY KEY(day,kind,bucket,agree)) WITHOUT ROWID;`)
	return err
}

// LabelSettings: the owner's settings of the model-filled labels, one JSON value in meta. Off by default.
type LabelSettings struct {
	Enabled        bool     `json:"enabled"`
	Backend        string   `json:"backend"`           // "" (none chosen), "apple-fm" or "foundry:<model>"
	Threshold      string   `json:"threshold"`         // "1.0" (all three samples agree) or "0.67" (two of three)
	MaxPerHour     int      `json:"maxPerHour,string"` // a string on the wire: the panel's text and choice fields hold strings
	Shadow         bool     `json:"shadow"`
	ShadowRate     int      `json:"shadowRate,string"` // one in N explicitly labelled calls is also classified
	Activities     []string `json:"activities"`
	ActivitiesText string   `json:"activitiesText"` // the same list as one line (the panel's text column and field)
	LastError      string   `json:"lastError"`
	Revision       uint64   `json:"revision,omitempty"`
}

const labelSettingsKey = "labels_settings"

var backendRe = regexp.MustCompile(`^(apple-fm|foundry:[A-Za-z0-9][A-Za-z0-9._:/-]{0,99})$`)

func defaultLabelSettings() LabelSettings {
	return LabelSettings{Threshold: "1.0", MaxPerHour: 120, Shadow: true, ShadowRate: 4, Activities: slices.Clone(DefaultActivities)}
}

func (l LabelSettings) finish() LabelSettings {
	l.ActivitiesText = strings.Join(l.Activities, ", ")
	return l
}

func loadLabelSettings(q interface{ QueryRow(string, ...any) *sql.Row }) LabelSettings {
	l := defaultLabelSettings()
	var v string
	if q.QueryRow(`SELECT value FROM meta WHERE key=?`, labelSettingsKey).Scan(&v) == nil {
		_ = json.Unmarshal([]byte(v), &l) // a damaged value keeps the defaults it did not overwrite
	}
	return l.finish()
}

// LabelSettings reads the settings (defaults when never saved).
func (s *Store) LabelSettings(ctx context.Context) (l LabelSettings, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error { l = loadLabelSettings(tx); return nil })
	return
}

// LabelSettingsPatch: the fields of a change; a nil field stays as it is.
type LabelSettingsPatch struct {
	Enabled    *bool           `json:"enabled"`
	Backend    *string         `json:"backend"`
	Threshold  json.RawMessage `json:"threshold"`  // "1.0", "0.67" or the numbers
	MaxPerHour json.RawMessage `json:"maxPerHour"` // 1 to 1000, a number or a string
	Shadow     *bool           `json:"shadow"`
	ShadowRate json.RawMessage `json:"shadowRate"` // 1 to 100, a number or a string
	Activities json.RawMessage `json:"activities"` // a list of words or one comma separated string; empty = the default list
}

// wireInt reads a whole number from a JSON number or a string, within lo..hi.
func wireInt(raw json.RawMessage, lo, hi int) (int, bool) {
	var str string
	if json.Unmarshal(raw, &str) != nil {
		str = strings.TrimSpace(string(raw))
	}
	n, err := strconv.Atoi(strings.TrimSpace(str))
	return n, err == nil && n >= lo && n <= hi
}

// ParseActivities: safe words, no repeats, at most 16; an empty list is the default list.
func ParseActivities(raw json.RawMessage) ([]string, error) {
	var list []string
	var one string
	if json.Unmarshal(raw, &one) == nil {
		for _, w := range strings.Split(one, ",") {
			if strings.TrimSpace(w) != "" {
				list = append(list, w)
			}
		}
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return nil, ErrRefusal{"activities: a list of words or one comma separated line"}
	}
	var out []string
	for _, w := range list {
		a := SafeWordLabel(w)
		if a == "" {
			return nil, ErrRefusal{"activities: a word is letters, digits . _ - (up to 32)"}
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if len(out) > maxActivities {
		return nil, ErrRefusal{fmt.Sprintf("activities: at most %d", maxActivities)}
	}
	if len(out) == 0 {
		out = slices.Clone(DefaultActivities)
	}
	return out, nil
}

// SetLabelSettings applies a change (owner only: the caller is the admin route). Turning it on needs a backend.
func (s *Store) SetLabelSettings(ctx context.Context, p LabelSettingsPatch) (LabelSettings, error) {
	var out LabelSettings
	err := s.adminWrite(ctx, "labels.settings", "labels", nil, func(tx *sql.Tx) (string, error) {
		l := loadLabelSettings(tx)
		if p.Enabled != nil {
			l.Enabled = *p.Enabled
		}
		if p.Backend != nil {
			if *p.Backend != "" && !backendRe.MatchString(*p.Backend) {
				return "", ErrRefusal{"backend: apple-fm or foundry:<model>"}
			}
			l.Backend = *p.Backend
		}
		if len(p.Threshold) > 0 {
			var f float64
			if json.Unmarshal(p.Threshold, &f) != nil {
				var str string
				if json.Unmarshal(p.Threshold, &str) != nil {
					return "", ErrRefusal{"threshold: 1.0 or 0.67"}
				}
				f, _ = strconv.ParseFloat(str, 64)
			}
			switch f {
			case 1:
				l.Threshold = "1.0"
			case 0.67:
				l.Threshold = "0.67"
			default:
				return "", ErrRefusal{"threshold: 1.0 (all three samples agree) or 0.67 (two of three)"}
			}
		}
		if len(p.MaxPerHour) > 0 {
			n, ok := wireInt(p.MaxPerHour, 1, 1000)
			if !ok {
				return "", ErrRefusal{"maxPerHour: 1 to 1000"}
			}
			l.MaxPerHour = n
		}
		if p.Shadow != nil {
			l.Shadow = *p.Shadow
		}
		if len(p.ShadowRate) > 0 {
			n, ok := wireInt(p.ShadowRate, 1, 100)
			if !ok {
				return "", ErrRefusal{"shadowRate: 1 to 100"}
			}
			l.ShadowRate = n
		}
		if len(p.Activities) > 0 {
			a, err := ParseActivities(p.Activities)
			if err != nil {
				return "", err
			}
			l.Activities = a
		}
		if l.Enabled && l.Backend == "" {
			return "", ErrRefusal{"choose a backend before turning model labels on (apple-fm or foundry:<model>)"}
		}
		if p.Backend != nil || p.Enabled != nil {
			l.LastError = "" // a new choice starts clean
		}
		l.Revision++
		b, _ := json.Marshal(l)
		out = l.finish()
		return "", setMeta(tx, labelSettingsKey, string(b))
	})
	return out, err
}

// SetLabelError records why the classification worker idles ("" clears it). Called by the gateway's worker; text is capped
// and never holds prompt text (the caller passes an error class).
func (s *Store) SetLabelError(ctx context.Context, msg string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		l := loadLabelSettings(tx)
		l.LastError = capStr(msg, 200)
		b, _ := json.Marshal(l)
		return setMeta(tx, labelSettingsKey, string(b))
	})
}

// SetModelLabels stores the labels a model chose for the root event id, and for its children where their own label is
// still empty. It never replaces a label that has a source (given, branch, model, internal): the UPDATE only matches an
// empty one, so an explicit label always wins. Only a phase from PhaseWords and an activity from the current list are
// stored (anything else is dropped; "" = unknown). A score is per mille of agreement (1 to 1000). Returns how many events
// got at least one label. The event must exist and be a root (ErrNotFound otherwise); an event of the gateway's own (any source "internal") is never touched. It grants nothing: a label is data.
func (s *Store) SetModelLabels(ctx context.Context, id, phase, activity string, phaseScore, activityScore int, revision ...uint64) (n int64, err error) {
	phase, activity = SafeWordLabel(phase), SafeWordLabel(activity)
	err = s.write(ctx, func(tx *sql.Tx) error {
		settings := loadLabelSettings(tx)
		if !acceptLabelRevision(settings, revision) {
			return nil
		}
		var parent sql.NullString
		if e := tx.QueryRow(`SELECT parent_id FROM usage_events WHERE id=?`, id).Scan(&parent); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return ErrNotFound
			}
			return e
		}
		if parent.Valid {
			return ErrNotFound // only a root event is classified; its children follow it
		}
		if !slices.Contains(PhaseWords, phase) || phaseScore < 1 || phaseScore > 1000 {
			phase = ""
		}
		if !slices.Contains(loadLabelSettings(tx).Activities, activity) || activityScore < 1 || activityScore > 1000 {
			activity = ""
		}
		for _, l := range []struct {
			col, val string
			score    int
		}{{"phase", phase, phaseScore}, {"activity", activity, activityScore}} {
			if l.val == "" {
				continue
			}
			r, e := tx.Exec(`UPDATE usage_events SET `+l.col+`=?, `+l.col+`_source='model', `+l.col+`_score=? WHERE (id=? OR parent_id=?) AND `+l.col+`='' AND `+l.col+`_source='' AND 'internal' NOT IN(phase_source,activity_source,task_source)`, l.val, l.score, id, id)
			if e != nil {
				return e
			}
			c, _ := r.RowsAffected()
			n += c
		}
		return nil
	})
	return
}

// RecordAgreement counts one shadow comparison: the kind (phase or activity), the score bucket of the model's choice ("667",
// "1000" or "unknown" when it did not decide) and whether it matched the explicit label. Numbers only; kept 400 days.
func (s *Store) RecordAgreement(ctx context.Context, kind, bucket string, agree bool, revision ...uint64) error {
	if (kind != "phase" && kind != "activity") || (bucket != "667" && bucket != "1000" && bucket != "unknown") {
		return ErrInvalid{"agreement"}
	}
	a := 0
	if agree {
		a = 1
	}
	now := s.now().UTC()
	return s.write(ctx, func(tx *sql.Tx) error {
		if !acceptLabelRevision(loadLabelSettings(tx), revision) {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO label_agreement(day,kind,bucket,agree,n) VALUES(?,?,?,?,1) ON CONFLICT(day,kind,bucket,agree) DO UPDATE SET n=n+1`, now.Format("2006-01-02"), kind, bucket, a); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM label_agreement WHERE day<?`, now.AddDate(0, 0, -400).Format("2006-01-02"))
		return err
	})
}

// AgreementRow: shadow comparisons of one kind and score bucket over the days asked.
type AgreementRow struct {
	Kind   string `json:"kind"`
	Bucket string `json:"bucket"`
	Agree  int64  `json:"agree"`
	N      int64  `json:"n"`
}

// Agreement sums the counters of the last days (1 to 400, default 30), per kind and bucket.
func (s *Store) Agreement(ctx context.Context, days int) (rows []AgreementRow, err error) {
	if days == 0 {
		days = 30
	}
	if days < 1 || days > 400 {
		return nil, ErrInvalid{"days"}
	}
	from := s.now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows = []AgreementRow{}
	err = s.read(ctx, func(tx *sql.Tx) error {
		rs, e := tx.Query(`SELECT kind,bucket,COALESCE(sum(CASE WHEN agree=1 THEN n END),0),sum(n) FROM label_agreement WHERE day>=? GROUP BY kind,bucket ORDER BY kind,bucket`, from)
		if e != nil {
			return e
		}
		defer rs.Close()
		for rs.Next() {
			var r AgreementRow
			if e := rs.Scan(&r.Kind, &r.Bucket, &r.Agree, &r.N); e != nil {
				return e
			}
			rows = append(rows, r)
		}
		return rs.Err()
	})
	return
}

// Worker acceptance belongs in the same transaction as its labels or counters.
// Calls without a revision are trusted storage operations used by import/tests.
func acceptLabelRevision(settings LabelSettings, revision []uint64) bool {
	return len(revision) == 0 || (settings.Enabled && settings.Revision == revision[0])
}
